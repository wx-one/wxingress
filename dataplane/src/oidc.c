/**
 * OpenID Connect, the relying party, in nginx's access phase: what NGINX
 * Plus's njs-based OIDC does for NGINX Ingress Controller's oidc Policy,
 * without Plus and without a key-value store.
 *
 * The controller writes, in a server with an oidc Policy,
 *
 *   set $wxingress_oidc_client        "<clientID>";
 *   set $wxingress_oidc_client_secret "<clientSecret>";
 *   set $wxingress_oidc_auth          "<authEndpoint>";
 *   set $wxingress_oidc_token         "<tokenEndpoint>";
 *   set $wxingress_oidc_jwks          "<jwksURI>";
 *   set $wxingress_oidc_end_session   "<endSessionEndpoint>";
 *   set $wxingress_oidc_scope         "<scope>";
 *   set $wxingress_oidc_redirect      "<redirectURI>";          /_codexch
 *   set $wxingress_oidc_post_logout   "<postLogoutRedirectURI>"; /_logout
 *   set $wxingress_oidc_extra         "<authExtraArgs, & joined>";
 *   set $wxingress_oidc_pkce          "1" or "";
 *   set $wxingress_oidc_ca            "<CA file>" or "";
 *   set $wxingress_oidc_leeway        "<seconds>";
 *   set $wxingress_oidc_session_key_file "<file>";  the key sessions are sealed with
 *   set $wxingress_oidc_policy        "<namespace>/<name>";   the Policy
 *   set $wxingress_oidc_verify        "1", or "0" for no TLS verification
 *
 * and `set $wxingress_oidc_on "1";` where the Policy applies. The redirect
 * URI's location, `/logout` and the post-logout location exist in the
 * server; the access step answers them.
 *
 * The flow is the authorization code flow, with PKCE when asked:
 *
 *   - a request without a session is sent to the authorization endpoint
 *     with a fresh state, nonce and code verifier, which go with it in a
 *     cookie of their own (`wxoidc_state`), sealed;
 *   - the IdP sends the browser back to the redirect URI with a code; the
 *     state must be the cookie's, the code is exchanged at the token
 *     endpoint (client_secret_basic, or the client_id alone for a public
 *     client with PKCE), and the ID token must be signed by a
 *     key of the jwksURI (jwt.c's key sets), be for the client (`aud`),
 *     carry the nonce and not be expired;
 *   - the session is a cookie too: the ID token, the access token and the
 *     refresh token, sealed with AES-256-GCM under a key derived
 *     (HMAC-SHA256) from the session key file - a Secret the controller
 *     mounts - or the client secret. Every worker and every replica with the
 *     Secret opens it, and nothing is stored anywhere - no keyval, no
 *     zone_sync. It is split into `wxoidc_s0`..`wxoidc_s3` when it does not
 *     fit one cookie;
 *   - when the ID token has expired and there is a refresh token, the
 *     tokens are refreshed at the token endpoint and the request is sent
 *     to itself again (307, which keeps the method and the body) with the
 *     new session; without one, or when the refresh fails, it is a new
 *     login;
 *   - `/logout` drops the session and sends the browser to the end session
 *     endpoint with id_token_hint and post_logout_redirect_uri, or straight
 *     to the post-logout URI when the IdP has none.
 *
 * The claims of the ID token of a session are `$meta_oidc_claim_<name>`,
 * its access token `$meta_oidc_access_token` and the ID token
 * `$meta_oidc_id_token` - what the controller sends upstream as NGINX Plus
 * sends `$jwt_claim_sub` and `$access_token`.
 */

#include <openssl/rand.h>

/** Cookies the session is split into, and how long each may be. */
#define OIDC_CHUNKS 4
#define OIDC_CHUNK 3800

/** How long a login may take, from the redirect to the IdP to its answer. */
#define OIDC_LOGIN_SECONDS 600

static char oidcSealed[OIDC_CHUNKS * OIDC_CHUNK + 64];

static int oidcRevoked(const char *iss, const char *aud, const char *sub, const char *sid, const char *ses,
                       long long created);
static http_response_t oidcBackChannel(http_request_t *req);
static http_response_t oidcFrontChannel(http_request_t *req);
static int oidcRevokeLocally(const char *kind, const char *scope, const char *value);
static void oidcScopeOf(const char *iss, const char *aud, char *into, size_t room);
static void urlDecoded(const char *text, char *into, size_t room);

/** Why a login or a refresh did not work, for the error log (stderr, which nginx's is in the controller). */
static void oidcNote(const char *what, const char *detail) {
  fprintf(stderr, "wxingress oidc: %s%s%s\n", what, detail[0] != 0 ? ": " : "", detail);
}

/* ------------------------------------------------------------- sealing */

/**
 * What a session is sealed for, as the text its key is derived with: the
 * Policy (namespace/name), the issuer, the client and the server. The key
 * file is one Secret for every Policy of the controller, so a key of the
 * client alone was the key of anybody's Policy with the same client ID -
 * which is no secret: a tenant with a Policy of its own, its own IdP behind
 * it and another tenant's client ID had its sessions opened by that tenant's
 * servers, as whoever its IdP said. And a session of one server is not a
 * session of another server of the same Policy.
 */
static int oidcKeyInfo(http_request_t *req, char *into, size_t room) {

  int length = snprintf(into, room, "wxingress oidc session 2|%s|%s|%s|%s",
                        req.variable("wxingress_oidc_policy"), req.variable("wxingress_oidc_issuer"),
                        req.variable("wxingress_oidc_client"), req.variable("server_name"));

  return length > 0 && (size_t)length < room;
}

/**
 * The session key: HMAC-SHA256 of oidcKeyInfo under the session key file
 * (`$wxingress_oidc_session_key_file`, the controller's Secret every replica
 * mounts), or under the client secret when there is no such file - a public
 * client with PKCE has no secret.
 */
static int oidcKey(http_request_t *req, unsigned char key[32]) {

  static char fileKey[256];
  static char fileRead[512];
  static struct timespec fileModified;
  const char *file = req.variable("wxingress_oidc_session_key_file");
  const char *secret = req.variable("wxingress_oidc_client_secret");
  char info[2048];
  size_t length = 32;
  struct stat seen;

  if (file[0] != 0 && stat(file, &seen) == 0 && seen.st_size > 0) {
    if (strcmp(fileRead, file) != 0 || seen.st_mtim.tv_sec != fileModified.tv_sec ||
        seen.st_mtim.tv_nsec != fileModified.tv_nsec) {
      FILE *in = fopen(file, "r");
      size_t got = in != NULL ? fread(fileKey, 1, sizeof fileKey - 1, in) : 0;
      if (in != NULL)
        fclose(in);
      fileKey[got] = 0;
      snprintf(fileRead, sizeof fileRead, "%s", file);
      fileModified = seen.st_mtim;
    }
    if (fileKey[0] != 0)
      secret = fileKey;
  }

  if (secret[0] == 0 || !oidcKeyInfo(req, info, sizeof info))
    return 0;

  return EVP_Q_mac(NULL, "HMAC", NULL, "SHA256", NULL, (const unsigned char *)secret, strlen(secret),
                   (const unsigned char *)info, strlen(info), key, 32, &length) != NULL && length == 32;
}

static size_t base64urlWrite(const unsigned char *bytes, size_t length, char *into, size_t room) {

  int written;

  if ((length + 2) / 3 * 4 + 1 > room)
    return 0;

  written = EVP_EncodeBlock((unsigned char *)into, bytes, (int)length);

  while (written > 0 && into[written - 1] == '=')
    --written;

  for (int at = 0; at < written; ++at) {
    if (into[at] == '+')
      into[at] = '-';
    else if (into[at] == '/')
      into[at] = '_';
  }

  into[written] = 0;

  return (size_t)written;
}

/**
 * `text` sealed under the key for a cookie of `name` (the name is the
 * associated data, so a state cookie cannot pass for a session): base64url
 * of nonce || ciphertext || tag. 0 when it does not fit.
 */
static size_t oidcSeal(http_request_t *req, const char *name, const char *text, char *into, size_t room) {

  static unsigned char out[OIDC_CHUNKS * OIDC_CHUNK];
  unsigned char key[32];
  EVP_CIPHER_CTX *ctx;
  int length = 0, last = 0;
  size_t textLength = strlen(text);
  size_t sealed = 0;

  if (!oidcKey(req, key) || 12 + textLength + 16 > sizeof out)
    return 0;

  if (RAND_bytes(out, 12) != 1)
    return 0;

  ctx = EVP_CIPHER_CTX_new();

  if (ctx != NULL && EVP_EncryptInit_ex(ctx, EVP_aes_256_gcm(), NULL, key, out) == 1 &&
      EVP_EncryptUpdate(ctx, NULL, &length, (const unsigned char *)name, (int)strlen(name)) == 1 &&
      EVP_EncryptUpdate(ctx, out + 12, &length, (const unsigned char *)text, (int)textLength) == 1 &&
      EVP_EncryptFinal_ex(ctx, out + 12 + length, &last) == 1 &&
      EVP_CIPHER_CTX_ctrl(ctx, EVP_CTRL_GCM_GET_TAG, 16, out + 12 + length + last) == 1)
    sealed = base64urlWrite(out, 12 + (size_t)(length + last) + 16, into, room);

  EVP_CIPHER_CTX_free(ctx);
  OPENSSL_cleanse(key, sizeof key);

  return sealed;
}

/** And opened: the text into `into`, or 0 when it is not one this key sealed. */
static int oidcOpen(http_request_t *req, const char *name, const char *sealed, char *into, size_t room) {

  static unsigned char raw[OIDC_CHUNKS * OIDC_CHUNK];
  unsigned char key[32];
  EVP_CIPHER_CTX *ctx;
  long rawLength = base64url(sealed, strlen(sealed), raw, sizeof raw);
  int length = 0, last = 0, good = 0;

  if (rawLength < 12 + 16 + 1 || (size_t)rawLength - 28 >= room || !oidcKey(req, key))
    return 0;

  ctx = EVP_CIPHER_CTX_new();

  if (ctx != NULL && EVP_DecryptInit_ex(ctx, EVP_aes_256_gcm(), NULL, key, raw) == 1 &&
      EVP_DecryptUpdate(ctx, NULL, &length, (const unsigned char *)name, (int)strlen(name)) == 1 &&
      EVP_DecryptUpdate(ctx, (unsigned char *)into, &length, raw + 12, (int)(rawLength - 28)) == 1 &&
      EVP_CIPHER_CTX_ctrl(ctx, EVP_CTRL_GCM_SET_TAG, 16, raw + rawLength - 16) == 1 &&
      EVP_DecryptFinal_ex(ctx, (unsigned char *)into + length, &last) == 1) {
    into[length + last] = 0;
    good = 1;
  }

  EVP_CIPHER_CTX_free(ctx);
  OPENSSL_cleanse(key, sizeof key);

  return good;
}

/* ------------------------------------------------------------ cookies */

/** The attributes of the cookies: Secure only over https. */
static const char *oidcCookieAttributes(http_request_t *req) {

  return strcmp(req.variable("scheme"), "https") == 0 ? "; Path=/; HttpOnly; Secure; SameSite=Lax"
                                                      : "; Path=/; HttpOnly; SameSite=Lax";
}

/**
 * The Set-Cookie values of the answer being built: the request's own, on its
 * task's stack. They were a list of the worker's once, shared by every
 * request it had, and a request that waited - for the token endpoint, a key
 * set, discovery - came back to the cookies of whichever ran meanwhile.
 */
typedef struct {
  char cookies[OIDC_CHUNKS + 2][OIDC_CHUNK + 128];
  int count;
} oidcJar_t;

/**
 * Adds a cookie; 0 when it does not fit, which is refused rather than cut:
 * what a cut takes off is the end, and the end is HttpOnly and Secure.
 */
static int oidcCookie(http_request_t *req, oidcJar_t *jar, const char *name, const char *value, int remove) {

  int length;

  if (jar->count >= OIDC_CHUNKS + 2)
    return 0;

  /* a cookie that goes: Max-Age and Expires both, as not every client reads Max-Age=0 */
  length = snprintf(jar->cookies[jar->count], sizeof jar->cookies[0], "%s=%s%s%s", name, value,
                    oidcCookieAttributes(req), remove ? "; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT" : "");

  if (length < 0 || (size_t)length >= sizeof jar->cookies[0]) {
    jar->cookies[jar->count][0] = 0;
    return 0;
  }

  ++jar->count;

  return 1;
}

/** The session's cookies for `plain`, sealed and split; the unused ones removed. 0 when it is too big. */
static int oidcSessionCookies(http_request_t *req, oidcJar_t *jar, const char *plain) {

  size_t length = oidcSeal(req, "wxoidc_s", plain, oidcSealed, sizeof oidcSealed);
  size_t at = 0;
  char name[16];
  static char chunk[OIDC_CHUNK + 1];

  if (length == 0)
    return 0;

  for (int part = 0; part < OIDC_CHUNKS; ++part) {
    snprintf(name, sizeof name, "wxoidc_s%d", part);
    if (at < length) {
      size_t n = length - at > OIDC_CHUNK ? OIDC_CHUNK : length - at;
      memcpy(chunk, oidcSealed + at, n);
      chunk[n] = 0;
      at += n;
      if (!oidcCookie(req, jar, name, chunk, 0))
        return 0;
    } else if (!oidcCookie(req, jar, name, "", 1)) {
      return 0;
    }
  }

  return at == length;
}

/**
 * What the session key is made from, as a stamp: the key file's modification
 * time (looked at once a second) and the client secret. A session opened
 * under another stamp is opened again.
 */
static long long oidcKeyStamp(http_request_t *req) {

  static time_t looked;
  static long long stamp;
  static char file[512];
  const char *path = req.variable("wxingress_oidc_session_key_file");
  time_t now = time(NULL);

  if (now != looked || strcmp(file, path) != 0) {
    struct stat seen;
    looked = now;
    snprintf(file, sizeof file, "%s", path);
    stamp = path[0] != 0 && stat(path, &seen) == 0 ? (long long)seen.st_mtim.tv_sec * 1000000000LL + seen.st_mtim.tv_nsec : 0;
  }

  return stamp;
}

/**
 * What the last opened session says, parsed once: its expiry, its tokens and
 * the claims of its ID token - what the access step and the `$meta_oidc_*`
 * variables of a request ask for, again and again for the same browser.
 */
static double oidcOpenedExpires;
static char oidcOpenedAccess[OIDC_CHUNK * 2];
static char oidcOpenedId[OIDC_CHUNK * 2];
static json_t oidcOpenedClaims;
static int oidcOpenedHasClaims;
static char oidcOpenedSub[256], oidcOpenedSid[256], oidcOpenedSes[64];
static char oidcOpenedIss[512], oidcOpenedAud[256], oidcOpenedPolicy[512], oidcOpenedServer[256];
static long long oidcOpenedCreated;

/** The claims of the session before, which the next one replaces. */
static void oidcOpenedForget(void) {

  if (oidcOpenedHasClaims)
    oidcOpenedClaims.release();

  oidcOpenedHasClaims = 0;
}

static void oidcOpenedParse(const char *plain) {

  json_t doc = meta_toJSON(plain);
  jwtParts_t parts;

  oidcOpenedForget();

  oidcOpenedExpires = doc.get("expires").real();
  oidcOpenedCreated = (long long)doc.get("created").real();
  snprintf(oidcOpenedSub, sizeof oidcOpenedSub, "%s", doc.get("sub").text());
  snprintf(oidcOpenedSid, sizeof oidcOpenedSid, "%s", doc.get("sid").text());
  snprintf(oidcOpenedSes, sizeof oidcOpenedSes, "%s", doc.get("ses").text());
  snprintf(oidcOpenedIss, sizeof oidcOpenedIss, "%s", doc.get("iss").text());
  snprintf(oidcOpenedAud, sizeof oidcOpenedAud, "%s", doc.get("aud").text());
  snprintf(oidcOpenedPolicy, sizeof oidcOpenedPolicy, "%s", doc.get("pol").text());
  snprintf(oidcOpenedServer, sizeof oidcOpenedServer, "%s", doc.get("srv").text());
  snprintf(oidcOpenedAccess, sizeof oidcOpenedAccess, "%s", doc.get("access").text());
  snprintf(oidcOpenedId, sizeof oidcOpenedId, "%s", doc.get("id").text());
  doc.release();

  if (jwtSplit(oidcOpenedId, &parts)) {
    oidcOpenedClaims = jwtPart(parts.text + parts.headerLength + 1, parts.payloadLength);
    oidcOpenedHasClaims = 1;
  }
}

/** The session of the request, opened into `into`; 0 for none. */
static int oidcSessionOf(http_request_t *req, char *into, size_t room) {

  size_t used = 0;
  /* the Cookie header once, split here: nginx scans the whole header for every cookie_ variable */
  const char *cookies = req.variable("http_cookie");

  for (int part = 0; part < OIDC_CHUNKS; ++part) {
    char name[16];
    const char *at = cookies;
    const char *value = NULL;
    size_t length = 0, nameLength;

    snprintf(name, sizeof name, "wxoidc_s%d=", part);
    nameLength = strlen(name);

    while (at != NULL && *at != 0) {
      while (*at == ' ' || *at == ';' || *at == ',')
        ++at;
      if (strncmp(at, name, nameLength) == 0) {
        value = at + nameLength;
        length = strcspn(value, "; ,");
        break;
      }
      at = strchr(at, ';');
    }

    if (value == NULL || length == 0)
      break;
    if (used + length + 1 > sizeof oidcSealed)
      return 0;
    memcpy(oidcSealed + used, value, length);
    used += length;
  }

  oidcSealed[used] = 0;

  if (used == 0)
    return 0;

  /**
   * The last session this worker opened, by its sealed text and what its key
   * is derived from (oidcKeyInfo): the access step and each `$meta_oidc_*` of
   * a request open the same cookie, and so do the next requests of the same
   * browser. Equal text under the same key is the same session, so it is not
   * decrypted again - and under another Policy, issuer, client or server it
   * is not the same key, so not the same session either.
   */
  {
    static char lastSealed[sizeof oidcSealed], lastClient[2048], lastSecret[512], lastPlain[OIDC_CHUNKS * OIDC_CHUNK];
    static long long lastStamp;
    char client[2048];
    const char *secret = req.variable("wxingress_oidc_client_secret");
    long long stamp = oidcKeyStamp(req);

    if (!oidcKeyInfo(req, client, sizeof client))
      return 0;

    if (lastSealed[0] != 0 && strcmp(lastSealed, oidcSealed) == 0 && strcmp(lastClient, client) == 0 &&
        strcmp(lastSecret, secret) == 0 && lastStamp == stamp && (into == NULL || strlen(lastPlain) < room)) {
      if (into != NULL)
        strcpy(into, lastPlain);
      return 1;
    }

    {
      static char opened[OIDC_CHUNKS * OIDC_CHUNK];

      if (!oidcOpen(req, "wxoidc_s", oidcSealed, opened, sizeof opened))
        return 0;

      if (into != NULL) {
        if (strlen(opened) >= room)
          return 0;
        strcpy(into, opened);
      }
      into = opened;
    }

    oidcOpenedParse(into);

    if (strlen(into) < sizeof lastPlain && strlen(client) < sizeof lastClient && strlen(secret) < sizeof lastSecret) {
      strcpy(lastSealed, oidcSealed);
      strcpy(lastClient, client);
      strcpy(lastSecret, secret);
      lastStamp = stamp;
      strcpy(lastPlain, into);
    }

    return 1;
  }
}

/* --------------------------------------------------------------- answers */

static http_response_t oidcAnswer(http_request_t *req, oidcJar_t *jar, int code, const char *location,
                                  const char *text) {

  http_response_t answer = req.reply(code);

  for (int at = 0; jar != NULL && at < jar->count; ++at)
    answer = answer.header("Set-Cookie", jar->cookies[at]);

  if (location != NULL)
    answer = answer.header("Location", location);

  return answer.text(text);
}

/** The text with what a URL must not have percent-encoded, into `into`. */
static void urlEncoded(const char *text, char *into, size_t room) {

  static const char hex[] = "0123456789ABCDEF";
  size_t used = 0;

  for (; *text != 0 && used + 4 < room; ++text) {
    unsigned char c = (unsigned char)*text;
    if (isalnum(c) || c == '-' || c == '.' || c == '_' || c == '~') {
      into[used++] = (char)c;
    } else {
      into[used++] = '%';
      into[used++] = hex[c >> 4];
      into[used++] = hex[c & 15];
    }
  }

  into[used] = 0;
}

/**
 * Where to send the browser back to on this host: a path of it, or `/`.
 * `//host/` and `/\host/` are other hosts to a browser, and so is anything
 * that is not a path.
 */
static void oidcLocalPath(const char *was, char *into, size_t room) {

  int local = was[0] == '/' && was[1] != '/' && was[1] != '\\' && strpbrk(was, "\r\n") == NULL;

  snprintf(into, room, "%s", local && strlen(was) < room ? was : "/");
}

/**
 * The scope for the query: NGINX Ingress Controller writes it as
 * `openid+profile+email`, which is already a query's spelling of spaces, so
 * a `+` stays and a space becomes one; the rest is encoded.
 */
static void oidcScope(const char *scope, char *into, size_t room) {

  static char one[2], encoded[8];
  size_t used = 0;

  into[0] = 0;

  for (; *scope != 0; ++scope) {
    one[0] = *scope == ' ' ? '+' : *scope;
    if (one[0] == '+')
      snprintf(encoded, sizeof encoded, "+");
    else
      urlEncoded(one, encoded, sizeof encoded);
    if (used + strlen(encoded) + 1 > room)
      break;
    strcpy(into + used, encoded);
    used += strlen(encoded);
  }
}

/* ------------------------------------------------------------ discovery */

/**
 * The provider's metadata (`<issuer>/.well-known/openid-configuration`), per
 * worker and issuer, fetched when an endpoint the Policy does not name is
 * asked for and kept for an hour - ten minutes after a fetch that failed.
 */
#define OIDC_PROVIDERS 8

typedef struct {
  char issuer[512];
  /** how it is fetched (jwtTlsKey): verified or not, the CA - another way is another provider */
  char tls[1200];
  int fetching;
  time_t due;
  char said[512];
  char auth[1024], token[1024], jwks[1024], endSession[1024];
} oidcProvider_t;

static oidcProvider_t oidcProviders[OIDC_PROVIDERS];

static oidcProvider_t *oidcDiscovered(http_request_t *req) {

  const char *issuer = req.variable("wxingress_oidc_issuer");
  oidcProvider_t *p = NULL;
  time_t now = time(NULL);
  char url[1200];
  jwtTls_t tls = jwtTlsOf(req, "wxingress_oidc");
  char tlsKey[sizeof oidcProviders[0].tls];

  if (issuer[0] == 0 || strlen(issuer) >= sizeof oidcProviders[0].issuer)
    return NULL;

  jwtTlsKey(&tls, tlsKey, sizeof tlsKey);

  for (int at = 0; at < OIDC_PROVIDERS && p == NULL; ++at)
    if (strcmp(oidcProviders[at].issuer, issuer) == 0 && strcmp(oidcProviders[at].tls, tlsKey) == 0)
      p = &oidcProviders[at];

  if (p == NULL) {
    for (int at = 0; at < OIDC_PROVIDERS && p == NULL; ++at)
      if (oidcProviders[at].issuer[0] == 0)
        p = &oidcProviders[at];
    for (int at = 0; at < OIDC_PROVIDERS && p == NULL; ++at)
      if (!oidcProviders[at].fetching)
        p = &oidcProviders[at];
    if (p == NULL)
      return NULL;
    memset(p, 0, sizeof *p);
    snprintf(p->issuer, sizeof p->issuer, "%s", issuer);
    snprintf(p->tls, sizeof p->tls, "%s", tlsKey);
  }

  /* one fetch of the metadata at a time; the others wait for it, as for a key set */
  if (p->fetching) {
    for (int waited = 0; p->fetching && waited < JWT_FETCH_WAIT_MS; waited += 50)
      meta_sleepMs(50);
    if (strcmp(p->issuer, issuer) != 0 || strcmp(p->tls, tlsKey) != 0 || p->fetching)
      return NULL;
    return p;
  }

  if (now < p->due)
    return p;

  /* the issuer without a trailing slash, then the well-known path (OpenID Connect Discovery 4) */
  snprintf(url, sizeof url, "%.*s/.well-known/openid-configuration",
           (int)(strlen(issuer) - (issuer[strlen(issuer) - 1] == '/')), issuer);

  {
    fetch_call_t call = meta_fetch("GET", url).timeout(10).connectTimeout(5).retry(3).verifyPeer(tls.verify);
    fetch_answer_t got;

    if (tls.verify && tls.ca[0] != 0)
      call = call.trust(tls.ca);

    p->fetching = 1;
    got = call.send();
    p->fetching = 0;

    /* the slot may have gone to another issuer while this one waited */
    if (strcmp(p->issuer, issuer) != 0 || strcmp(p->tls, tlsKey) != 0) {
      got.release();
      return NULL;
    }

    if (got.ok && got.status == 200) {
      json_t doc = meta_toJSON(got.text());
      if (doc._refused == NULL) {
        snprintf(p->said, sizeof p->said, "%s", doc.get("issuer").text());
        snprintf(p->auth, sizeof p->auth, "%s", doc.get("authorization_endpoint").text());
        snprintf(p->token, sizeof p->token, "%s", doc.get("token_endpoint").text());
        snprintf(p->jwks, sizeof p->jwks, "%s", doc.get("jwks_uri").text());
        snprintf(p->endSession, sizeof p->endSession, "%s", doc.get("end_session_endpoint").text());
        /* metadata for another issuer is no metadata (Discovery 4.3) */
        if (strcmp(p->said, issuer) != 0) {
          oidcNote("the discovery document names another issuer", p->said);
          p->auth[0] = p->token[0] = p->jwks[0] = p->endSession[0] = 0;
        }
      }
      doc.release();
      p->due = now + 3600;
    } else {
      oidcNote("the discovery document could not be read", url);
      p->due = now + 600;
    }

    got.release();
  }

  return p;
}

/**
 * An endpoint: the Policy's, or the provider's metadata's when the Policy
 * leaves it out and names an issuer. `which` is auth, token, jwks or
 * end_session.
 */
static const char *oidcEndpoint(http_request_t *req, const char *which) {

  char name[64];
  const char *configured;
  oidcProvider_t *p;

  snprintf(name, sizeof name, "wxingress_oidc_%s", which);
  configured = req.variable(name);

  if (configured[0] != 0 || (p = oidcDiscovered(req)) == NULL)
    return configured;

  if (strcmp(which, "auth") == 0)
    return p->auth;
  if (strcmp(which, "token") == 0)
    return p->token;
  if (strcmp(which, "jwks") == 0)
    return p->jwks;
  return p->endSession;
}

/**
 * The issuer the ID token must name in `iss`: the Policy's, which the
 * provider's metadata must agree with (OpenID Connect Discovery 4.3); ""
 * when the Policy names none, and then `iss` is not checked.
 */
static const char *oidcIssuer(http_request_t *req) {

  const char *issuer = req.variable("wxingress_oidc_issuer");

  return issuer;
}

/** A URI of the configuration as an absolute URL: a path gets this request's scheme and host. */
static void oidcAbsolute(http_request_t *req, const char *uri, char *into, size_t room) {

  const char *host = req.variable("http_host");

  if (uri[0] != '/') {
    snprintf(into, room, "%s", uri);
    return;
  }

  if (host[0] == 0)
    host = req.variable("host");

  snprintf(into, room, "%s://%s%s", req.variable("scheme"), host, uri);
}

/** The redirect URI of the configuration, /_codexch without one, as NGINX Plus's. */
static const char *oidcRedirectURI(http_request_t *req) {

  const char *redirect = req.variable("wxingress_oidc_redirect");

  return redirect[0] != 0 ? redirect : "/_codexch";
}

static void oidcRandom(char *into, size_t room) {

  unsigned char bytes[32];

  if (RAND_bytes(bytes, sizeof bytes) != 1)
    memset(bytes, 0, sizeof bytes);

  base64urlWrite(bytes, sizeof bytes, into, room);
}

/** To the IdP, with a fresh state, nonce and verifier in the state cookie. */
static http_response_t oidcLogin(http_request_t *req, oidcJar_t *jar) {

  char state[64], nonce[64], verifier[64], challenge[64];
  char plain[8192], sealed[12000], location[32768];
  char redirect[2048], encodedRedirect[6144], scope[1024], back[4096];
  unsigned char digest[32];
  unsigned int digestLength = 0;
  const char *pkce = req.variable("wxingress_oidc_pkce");
  const char *extra = req.variable("wxingress_oidc_extra");
  json_t doc = meta_toJSON("{}");

  /* the authorization endpoint first: discovery may wait */
  const char *auth = oidcEndpoint(req, "auth");

  oidcRandom(state, sizeof state);
  oidcRandom(nonce, sizeof nonce);
  oidcRandom(verifier, sizeof verifier);

  /* the state cookie: what the callback checks, and where the browser was going */
  doc.state = state;
  doc.nonce = nonce;
  doc.verifier = pkce[0] != 0 ? verifier : "";
  doc.back = req.variable("request_uri");
  doc.until = (long)(time(NULL) + OIDC_LOGIN_SECONDS);
  doc.into(plain, sizeof plain);
  doc.release();

  if (oidcSeal(req, "wxoidc_state", plain, sealed, sizeof sealed) == 0 ||
      !oidcCookie(req, jar, "wxoidc_state", sealed, 0))
    return req.reply(500).text("the login state does not fit a cookie\n");

  oidcAbsolute(req, oidcRedirectURI(req), redirect, sizeof redirect);
  urlEncoded(redirect, encodedRedirect, sizeof encodedRedirect);
  oidcScope(req.variable("wxingress_oidc_scope")[0] != 0 ? req.variable("wxingress_oidc_scope") : "openid",
            scope, sizeof scope);
  urlEncoded(req.variable("wxingress_oidc_client"), back, sizeof back);

  if (pkce[0] != 0) {
    EVP_Digest(verifier, strlen(verifier), digest, &digestLength, EVP_sha256(), NULL);
    base64urlWrite(digest, digestLength, challenge, sizeof challenge);
  }

  snprintf(location, sizeof location,
           "%s%sresponse_type=code&client_id=%s&redirect_uri=%s&scope=%s&state=%s&nonce=%s%s%s%s%s",
           auth, strchr(auth, '?') ? "&" : "?",
           back, encodedRedirect, scope, state, nonce,
           pkce[0] != 0 ? "&code_challenge_method=S256&code_challenge=" : "", pkce[0] != 0 ? challenge : "",
           extra[0] != 0 ? "&" : "", extra);

  return oidcAnswer(req, jar, 302, location, "");
}

/**
 * A token request at the token endpoint: the form, with the client's
 * credentials in Basic. The JSON of the answer, or nothing.
 */
static json_t oidcTokenRequest(http_request_t *req, const char *form) {

  char credentials[2048], basic[3000], authorization[3100], encodedClient[1024], encodedSecret[1024];
  /* where the form of a public client is written: used by the call below, so not in a block of its own */
  char publicForm[40000];
  jwtTls_t tls = jwtTlsOf(req, "wxingress_oidc");
  fetch_call_t call;
  fetch_answer_t got;
  json_t doc;

  urlEncoded(req.variable("wxingress_oidc_client"), encodedClient, sizeof encodedClient);
  urlEncoded(req.variable("wxingress_oidc_client_secret"), encodedSecret, sizeof encodedSecret);
  snprintf(credentials, sizeof credentials, "%s:%s", encodedClient, encodedSecret);
  EVP_EncodeBlock((unsigned char *)basic, (const unsigned char *)credentials, (int)strlen(credentials));
  snprintf(authorization, sizeof authorization, "Basic %s", basic);

  /* a public client (PKCE, no secret) names itself in the form */
  if (req.variable("wxingress_oidc_client_secret")[0] == 0) {
    int length = snprintf(publicForm, sizeof publicForm, "%s&client_id=%s", form, encodedClient);
    if (length < 0 || (size_t)length >= sizeof publicForm)
      return meta_toJSON("");
    form = publicForm;
  }

  call = meta_fetch("POST", oidcEndpoint(req, "token"))
             .header("content-type", "application/x-www-form-urlencoded")
             .header("accept", "application/json")
             .text(form)
             .timeout(10)
             .connectTimeout(5)
             /* a connection kept from the last exchange may have been closed by now */
             .retry(3);

  if (req.variable("wxingress_oidc_client_secret")[0] != 0)
    call = call.header("authorization", authorization);

  call = call.verifyPeer(tls.verify);

  if (tls.verify && tls.ca[0] != 0)
    call = call.trust(tls.ca);

  got = call.send();

  if (!got.ok || got.status != 200) {
    char status[512];
    if (got.failed)
      snprintf(status, sizeof status, "no answer: %s", got.why);
    else
      snprintf(status, sizeof status, "status %d: %.400s", got.status, got.text());
    oidcNote("the token endpoint refused", status);
    got.release();
    return meta_toJSON("");
  }

  doc = meta_toJSON(got.text());
  got.release();

  return doc;
}

/**
 * Whether an ID token is good for the client: signed by a key of the
 * jwksURI, for the client, not expired, and with the nonce when one is
 * expected.
 */
static int oidcIdTokenGood(http_request_t *req, const char *token, const char *nonce, long *expires) {

  jwtParts_t parts;
  json_t header, payload, aud;
  char alg[16], kid[160];
  unsigned char signature[1024];
  long signatureLength;
  const char *client = req.variable("wxingress_oidc_client");
  long leeway = strtol(req.variable("wxingress_oidc_leeway"), NULL, 10);
  int good = 0, forClient = 0, kidKnown = 0;
  jwtSet_t *set;
  time_t now = time(NULL);

  if (!jwtSplit(token, &parts))
    return 0;

  header = jwtPart(parts.text, parts.headerLength);
  snprintf(alg, sizeof alg, "%s", header.get("alg").text());
  snprintf(kid, sizeof kid, "%s", header.get("kid").text());
  header.release();

  if (alg[0] == 0 || strcmp(alg, "none") == 0)
    return 0;

  signatureLength = base64url(parts.text + parts.headerLength + parts.payloadLength + 2, parts.signatureLength,
                              signature, sizeof signature);

  if (signatureLength <= 0)
    return 0;

  for (int pass = 0; pass < 2 && !good; ++pass) {
    jwtTls_t tls = jwtTlsOf(req, "wxingress_oidc");
    set = jwtSetOf(oidcEndpoint(req, "jwks"), 1, "1h", &tls, pass == 1);
    if (set == NULL)
      return 0;
    for (int at = 0; at < set->count && !good; ++at) {
      if (kid[0] != 0 && set->keys[at].kid[0] != 0 && strcmp(kid, set->keys[at].kid) != 0)
        continue;
      if (kid[0] != 0 && strcmp(kid, set->keys[at].kid) == 0)
        kidKnown = 1;
      good = jwtVerified(&set->keys[at], alg, parts.text, parts.headerLength + 1 + parts.payloadLength, signature,
                         (size_t)signatureLength);
    }
    if (good || kid[0] == 0 || kidKnown)
      break;
  }

  if (!good)
    return 0;

  payload = jwtPart(parts.text + parts.headerLength + 1, parts.payloadLength);
  aud = payload.get("aud");

  if (strcmp(aud.kind(), "array") == 0) {
    for (int at = 0; at < aud.count(); ++at)
      if (strcmp(aud.at(at).text(), client) == 0)
        forClient = 1;
  } else {
    forClient = strcmp(aud.text(), client) == 0;
  }

  if (oidcIssuer(req)[0] != 0 && strcmp(payload.get("iss").text(), oidcIssuer(req)) != 0) {
    oidcNote("the ID token is from another issuer", payload.get("iss").text());
    forClient = 0;
  }

  good = forClient && !payload.get("exp").isNothing() &&
         (double)now <= payload.get("exp").real() + (double)leeway &&
         (nonce == NULL || strcmp(payload.get("nonce").text(), nonce) == 0);

  if (expires != NULL)
    *expires = (long)payload.get("exp").real();

  payload.release();

  return good;
}

/** The session of a token answer, as the text the cookies seal; 0 when its ID token is not good. */
static void oidcRandom(char *into, size_t room);

/**
 * `keepSes` is the session id a refresh keeps, read from the session before
 * the token request waited - never from the worker's last opened session,
 * which by then is any browser's.
 */
static int oidcSessionText(http_request_t *req, json_t tokens, const char *nonce, const char *refreshBefore,
                           const char *keepSes, char *into, size_t room) {

  long expires = 0;
  const char *idToken = tokens.get("id_token").text();
  const char *refresh = tokens.get("refresh_token").text();
  json_t doc;
  size_t written;

  if (!oidcIdTokenGood(req, idToken, nonce, &expires)) {
    oidcNote("the ID token is not good", idToken[0] == 0 ? "there is none" : "signature, audience, nonce or expiry");
    return 0;
  }

  doc = meta_toJSON("{}");
  doc.id = idToken;
  doc.access = tokens.get("access_token").text();
  /* who and which IdP session, and when: what a revocation names */
  {
    jwtParts_t parts;
    if (jwtSplit(idToken, &parts)) {
      json_t claims = jwtPart(parts.text + parts.headerLength + 1, parts.payloadLength);
      doc.sub = claims.get("sub").text();
      doc.sid = claims.get("sid").text();
      doc.iss = claims.get("iss").text();
      claims.release();
    }
  }
  /* what the session is for, which every request checks: the client, the Policy and the server */
  doc.aud = req.variable("wxingress_oidc_client");
  doc.pol = req.variable("wxingress_oidc_policy");
  doc.srv = req.variable("server_name");
  doc.created = (long)time(NULL);
  /* the session's own id, which /logout revokes: kept by a refresh, new for a login */
  {
    char ses[64];
    if (refreshBefore[0] != 0 && keepSes != NULL && keepSes[0] != 0)
      snprintf(ses, sizeof ses, "%s", keepSes);
    else
      oidcRandom(ses, sizeof ses);
    doc.ses = ses;
  }
  /* a refresh answer without a refresh token keeps the one it was asked with */
  doc.refresh = refresh[0] != 0 ? refresh : refreshBefore;
  doc.expires = expires;
  written = doc.into(into, room);
  doc.release();

  return written > 0 && written < room;
}

/** The redirect URI: the code for tokens, and the session. */
static http_response_t oidcCallback(http_request_t *req, oidcJar_t *jar) {

  char state[8192], form[32768], redirect[2048], encodedRedirect[6144], encodedCode[4096];
  char back[8192], session[OIDC_CHUNKS * OIDC_CHUNK];
  char code[4096], givenState[512];
  json_t saved, tokens;
  int good;

  urlDecoded(req.query("code"), code, sizeof code);
  urlDecoded(req.query("state"), givenState, sizeof givenState);

  if (!oidcOpen(req, "wxoidc_state", req.variable("cookie_wxoidc_state"), state, sizeof state))
    return req.reply(400).text("no login was started here\n");

  saved = meta_toJSON(state);

  if (strcmp(saved.get("state").text(), givenState) != 0 || saved.get("until").number() < time(NULL) ||
      code[0] == 0) {
    saved.release();
    return req.reply(400).text("the login does not match the one started here\n");
  }

  oidcAbsolute(req, oidcRedirectURI(req), redirect, sizeof redirect);
  urlEncoded(redirect, encodedRedirect, sizeof encodedRedirect);
  urlEncoded(code, encodedCode, sizeof encodedCode);
  snprintf(form, sizeof form, "grant_type=authorization_code&code=%s&redirect_uri=%s%s%s", encodedCode,
           encodedRedirect, saved.get("verifier").text()[0] != 0 ? "&code_verifier=" : "",
           saved.get("verifier").text());
  /* back to a path of this host only */
  oidcLocalPath(saved.get("back").text(), back, sizeof back);

  tokens = oidcTokenRequest(req, form);
  good = tokens._refused == NULL &&
         oidcSessionText(req, tokens, saved.get("nonce").text(), "", NULL, session, sizeof session);
  tokens.release();
  saved.release();

  if (!good)
    return req.reply(401).text("the identity provider's answer is not good\n");

  if (!oidcCookie(req, jar, "wxoidc_state", "", 1) || !oidcSessionCookies(req, jar, session))
    return req.reply(500).text("the session does not fit its cookies\n");

  return oidcAnswer(req, jar, 302, back, "");
}

/**
 * Whether the session just opened is one of this place: sealed for this
 * Policy, issuer, client and server - which its key already says, and which
 * the session says again in what it holds.
 */
static int oidcSessionHere(http_request_t *req) {

  const char *issuer = oidcIssuer(req);

  return strcmp(oidcOpenedAud, req.variable("wxingress_oidc_client")) == 0 &&
         strcmp(oidcOpenedPolicy, req.variable("wxingress_oidc_policy")) == 0 &&
         strcmp(oidcOpenedServer, req.variable("server_name")) == 0 &&
         (issuer[0] == 0 || strcmp(oidcOpenedIss, issuer) == 0);
}

/** /logout: the session goes, and the browser to the IdP's end of it. */
static http_response_t oidcLogout(http_request_t *req, oidcJar_t *jar) {

  char session[OIDC_CHUNKS * OIDC_CHUNK], location[49152], after[2048], encodedAfter[6144],
      encodedHint[8192];
  const char *end = oidcEndpoint(req, "end_session");
  const char *post = req.variable("wxingress_oidc_post_logout");
  char name[16];

  oidcAbsolute(req, post[0] != 0 ? post : "/_logout", after, sizeof after);
  encodedHint[0] = 0;

  if (oidcSessionOf(req, session, sizeof session) && oidcSessionHere(req)) {
    json_t doc = meta_toJSON(session);
    urlEncoded(doc.get("id").text(), encodedHint, sizeof encodedHint);
    /* a copy of the cookie elsewhere ends too, on every replica */
    if (!oidcRevokeLocally("ses", "", doc.get("ses").text()))
      oidcNote("the session could not be revoked", "the revocation table is full");
    doc.release();
  }

  for (int part = 0; part < OIDC_CHUNKS; ++part) {
    snprintf(name, sizeof name, "wxoidc_s%d", part);
    oidcCookie(req, jar, name, "", 1);
  }

  if (end[0] == 0)
    return oidcAnswer(req, jar, 302, after, "");

  urlEncoded(after, encodedAfter, sizeof encodedAfter);

  snprintf(location, sizeof location, "%s%spost_logout_redirect_uri=%s&client_id=%s%s%s", end,
           strchr(end, '?') ? "&" : "?", encodedAfter, req.variable("wxingress_oidc_client"),
           encodedHint[0] != 0 ? "&id_token_hint=" : "", encodedHint);

  return oidcAnswer(req, jar, 302, location, "");
}

/** The access step. */
static http_response_t oidcGuard(http_request_t *req) {

  char session[OIDC_CHUNKS * OIDC_CHUNK], form[16384], encodedRefresh[6144], fresh[OIDC_CHUNKS * OIDC_CHUNK];
  char ses[64], back[8192];
  const char *client = req.variable("wxingress_oidc_client");
  const char *path;
  const char *post;
  json_t doc;
  long leeway;
  oidcJar_t jar;

  if (client[0] == 0)
    return req.reply(0);

  jar.count = 0;
  path = req.variable("uri");
  post = req.variable("wxingress_oidc_post_logout");

  if (strcmp(path, oidcRedirectURI(req)) == 0)
    return oidcCallback(req, &jar);

  if (strcmp(path, "/logout") == 0)
    return oidcLogout(req, &jar);

  if (strcmp(path, "/_backchannel_logout") == 0)
    return oidcBackChannel(req);

  if (strcmp(path, "/front_channel_logout") == 0)
    return oidcFrontChannel(req);

  if (strcmp(path, post[0] != 0 ? post : "/_logout") == 0)
    return req.reply(200).text("You have been logged out.\n");

  if (req.variable("wxingress_oidc_on")[0] == 0)
    return req.reply(0);

  /**
   * The session, and everything asked of it before anything waits: what was
   * opened is the worker's, and after a wait it is whichever browser's came
   * meanwhile.
   */
  if (!oidcSessionOf(req, session, sizeof session) || !oidcSessionHere(req))
    return oidcLogin(req, &jar);

  /* a session the IdP or an administrator ended: a new login */
  if (oidcRevoked(oidcOpenedIss, oidcOpenedAud, oidcOpenedSub, oidcOpenedSid, oidcOpenedSes, oidcOpenedCreated))
    return oidcLogin(req, &jar);

  leeway = strtol(req.variable("wxingress_oidc_leeway"), NULL, 10);

  /* a session that has not expired: decrypted and parsed once, nothing verified again */
  if ((double)time(NULL) <= oidcOpenedExpires + (double)leeway)
    return req.reply(0);

  snprintf(ses, sizeof ses, "%s", oidcOpenedSes);
  doc = meta_toJSON(session);

  /* expired: refreshed, or a new login */
  if (doc.get("refresh").text()[0] != 0) {

    json_t tokens;
    int good;

    urlEncoded(doc.get("refresh").text(), encodedRefresh, sizeof encodedRefresh);
    snprintf(form, sizeof form, "grant_type=refresh_token&refresh_token=%s", encodedRefresh);
    tokens = oidcTokenRequest(req, form);
    good = tokens._refused == NULL &&
           oidcSessionText(req, tokens, NULL, doc.get("refresh").text(), ses, fresh, sizeof fresh);
    tokens.release();
    doc.release();

    /* to itself again, with the new session - a path of this host, never `//other.example` */
    if (good && oidcSessionCookies(req, &jar, fresh)) {
      oidcLocalPath(req.variable("request_uri"), back, sizeof back);
      return oidcAnswer(req, &jar, 307, back, "");
    }

    if (good)
      oidcNote("the refreshed session does not fit its cookies", "");

    jar.count = 0;
    return oidcLogin(req, &jar);
  }

  doc.release();

  return oidcLogin(req, &jar);
}

/* --------------------------------------------------------- the variables */

/** `$meta_oidc_claim_<name>`, `$meta_oidc_access_token`, `$meta_oidc_id_token` of the session. */
static const char *oidcVariable(http_request_t *req, const char *argument) {

  static char value[OIDC_CHUNK * 2];

  if (req.variable("wxingress_oidc_on")[0] == 0 || !oidcSessionOf(req, NULL, 0) || !oidcSessionHere(req))
    return "";

  if (strcmp(argument, "access_token") == 0)
    return oidcOpenedAccess;

  if (strcmp(argument, "id_token") == 0)
    return oidcOpenedId;

  value[0] = 0;

  if (strncmp(argument, "claim_", 6) == 0 && oidcOpenedHasClaims)
    jwtValue(jwtMember(oidcOpenedClaims, argument + 6), value, sizeof value);

  return value;
}

/* ---------------------------------------------------------- revocations */

/**
 * Sessions that end before their ID token does, without a store: a list of
 * "sessions of this sub, or of this sid, made before this time" - and "all
 * sessions made before this time" - in shared memory, which every worker
 * reads for every request with a session.
 *
 * The controller sets the list from its ConfigMap (`PUT .../oidc/revocations`,
 * no reload): an administrator revokes a user by adding a line there. A
 * back-channel logout (the IdP's POST of a logout token) or a front-channel
 * logout (its iframe) adds an entry here directly and marks it local; the
 * controller of each replica reads the local ones (`GET ...?local=1`) and
 * writes them into the ConfigMap, from which every replica gets them. So a
 * logout at the IdP reaches every replica within seconds, and the cluster's
 * API is the only store.
 *
 *   PUT /api/wxingress/1/oidc/revocations   {"all": <unix>, "entries": [{"key": "sub:alice", "before": <unix>}]}
 *   GET /api/wxingress/1/oidc/revocations   the same, with "local" on the ones a logout added
 *
 * What a logout of an IdP adds is scoped to that IdP and client:
 * `sid:<scope>|<sid>` and `sub:<scope>|<sub>`, the scope being the first 16
 * hex digits of SHA-256 over "<iss>|<client>" (oidcScopeOf). An IdP vouches
 * for its own sessions only - the logout token of a tenant's own IdP, for
 * another tenant's sub, ended that tenant's sessions everywhere. What an
 * administrator writes without a scope - `sub:alice` - still ends that sub's
 * sessions of every IdP.
 */
#define REVOCATIONS 8192

typedef struct {
  /** FNV-1a of the key ("sub:<sub>" or "sid:<sid>"); 0 for a free slot */
  unsigned long long hash;
  long long before;
  int local;
  char key[244];
} revocation_t;

typedef struct {
  pthread_mutex_t lock;
  long long all;
  revocation_t entries[REVOCATIONS];
} revocations_t;

static revocations_t *revocations;

static int revocationsOpen(void) {

  pthread_mutexattr_t how;
  void *mapped;

  if (revocations != NULL)
    return 0;

  mapped = mmap(NULL, sizeof(revocations_t), PROT_READ | PROT_WRITE, MAP_SHARED | MAP_ANONYMOUS, -1, 0);

  if (mapped == MAP_FAILED) {
    fprintf(stderr, "wxingress: no shared memory for revocations: %s\n", strerror(errno));
    return -1;
  }

  revocations = mapped;

  if (pthread_mutexattr_init(&how) != 0 || pthread_mutexattr_setpshared(&how, PTHREAD_PROCESS_SHARED) != 0 ||
      pthread_mutexattr_setrobust(&how, PTHREAD_MUTEX_ROBUST) != 0 ||
      pthread_mutex_init(&revocations->lock, &how) != 0) {
    fprintf(stderr, "wxingress: no lock for the revocations\n");
    return -1;
  }

  return 0;
}

static void revocationsLock(void) {
  if (pthread_mutex_lock(&revocations->lock) == EOWNERDEAD)
    pthread_mutex_consistent(&revocations->lock);
}

static unsigned long long revocationHash(const char *key) {

  unsigned long long hash = 14695981039346656037ull;

  for (; *key != 0; ++key)
    hash = (hash ^ (unsigned char)*key) * 1099511628211ull;

  return hash != 0 ? hash : 1;
}

/**
 * How long a local entry is kept when the table is full and the controller
 * has not taken it over yet: the controller keeps a line for a month, and a
 * session is refreshed - made anew - long before.
 */
#define REVOCATION_KEPT_SECONDS (30LL * 86400)

/**
 * Adds or moves an entry; the caller holds the lock. 0 when the table is
 * full - which the caller says, rather than the logout being dropped without
 * a word.
 *
 * An entry older than REVOCATION_KEPT_SECONDS gives its slot to a new one
 * on the new one's probe chain. A slot is never emptied - a reader without
 * the lock walks the chain until an empty one - only taken over, so every
 * other chain through it stays whole.
 */
static int revocationPut(const char *key, long long before, int local) {

  unsigned long long hash = revocationHash(key);
  long long expired = (long long)time(NULL) - REVOCATION_KEPT_SECONDS;
  revocation_t *stale = NULL;

  for (unsigned probe = 0; probe < REVOCATIONS; ++probe) {
    revocation_t *e = &revocations->entries[(hash + probe) % REVOCATIONS];
    if (e->hash == hash && strcmp(e->key, key) == 0) {
      if (before > e->before)
        __atomic_store_n(&e->before, before, __ATOMIC_RELEASE);
      e->local = e->local && local;
      return 1;
    }
    if (e->hash == 0) {
      if (stale != NULL)
        e = stale;
      snprintf(e->key, sizeof e->key, "%s", key);
      e->local = local;
      __atomic_store_n(&e->before, before, __ATOMIC_RELEASE);
      __atomic_store_n(&e->hash, hash, __ATOMIC_RELEASE);
      return 1;
    }
    if (stale == NULL && e->before < expired)
      stale = e;
  }

  if (stale == NULL)
    return 0;

  snprintf(stale->key, sizeof stale->key, "%s", key);
  stale->local = local;
  __atomic_store_n(&stale->before, before, __ATOMIC_RELEASE);
  __atomic_store_n(&stale->hash, hash, __ATOMIC_RELEASE);

  return 1;
}

/** When sessions of a key made before it end; 0 for none. No lock: slots are only added or taken over. */
static long long revocationOf(const char *key) {

  unsigned long long hash = revocationHash(key);

  if (revocations == NULL)
    return 0;

  for (unsigned probe = 0; probe < REVOCATIONS; ++probe) {
    revocation_t *e = &revocations->entries[(hash + probe) % REVOCATIONS];
    unsigned long long seen = __atomic_load_n(&e->hash, __ATOMIC_ACQUIRE);
    if (seen == 0)
      return 0;
    if (seen == hash && strcmp(e->key, key) == 0)
      return __atomic_load_n(&e->before, __ATOMIC_ACQUIRE);
  }

  return 0;
}

/** The scope of the entries a logout of an IdP for a client adds: see above. */
static void oidcScopeOf(const char *iss, const char *aud, char *into, size_t room) {

  static const char hex[] = "0123456789abcdef";
  unsigned char digest[32];
  unsigned int length = 0;
  char text[1024];

  snprintf(text, sizeof text, "%s|%s", iss, aud);
  into[0] = 0;

  if (room < 17 || EVP_Digest(text, strlen(text), digest, &length, EVP_sha256(), NULL) != 1)
    return;

  for (int at = 0; at < 8; ++at) {
    into[2 * at] = hex[digest[at] >> 4];
    into[2 * at + 1] = hex[digest[at] & 15];
  }

  into[16] = 0;
}

/** Whether `created` is before the entry of kind:value - or kind:scope|value with a scope. */
static int oidcRevokedBy(const char *kind, const char *scope, const char *value, long long created) {

  char key[300];

  if (value[0] == 0)
    return 0;

  if (scope[0] != 0)
    snprintf(key, sizeof key, "%s:%s|%s", kind, scope, value);
  else
    snprintf(key, sizeof key, "%s:%s", kind, value);

  return created < revocationOf(key);
}

/**
 * Whether a session of an issuer and client, its sub and sid, made at
 * `created`, is revoked: by everything before a time, by the
 * administrator's entries of its sub or sid, by its IdP's logouts, or by its
 * own id (/logout).
 */
static int oidcRevoked(const char *iss, const char *aud, const char *sub, const char *sid, const char *ses,
                       long long created) {

  char scope[32];
  char key[260];

  if (revocations == NULL)
    return 0;

  if (created < __atomic_load_n(&revocations->all, __ATOMIC_ACQUIRE))
    return 1;

  oidcScopeOf(iss, aud, scope, sizeof scope);

  if (oidcRevokedBy("sub", "", sub, created) || oidcRevokedBy("sid", "", sid, created) ||
      (scope[0] != 0 && (oidcRevokedBy("sub", scope, sub, created) || oidcRevokedBy("sid", scope, sid, created))))
    return 1;

  snprintf(key, sizeof key, "ses:%s", ses);
  if (ses[0] != 0 && revocationOf(key) != 0)
    return 1;

  return 0;
}

/**
 * The list from the controller, as a whole: what it has replaces what was
 * here, except the local entries it does not have yet. Clearing and refilling
 * under the lock leaves a reader a moment without an entry it will find again.
 */
static http_response_t setRevocations(http_request_t *req) {

  json_t said = req.readJson();
  json_t entries;
  static revocation_t kept[256];
  int keptCount = 0, missing = 0;

  if (said._refused != NULL) {
    said.release();
    return failed(req, 415, "the body is not JSON", "RevocationFormatError");
  }

  entries = said.get("entries");
  revocationsLock();

  /* the local ones the list does not have are kept for the controller to write down */
  for (unsigned at = 0; at < REVOCATIONS && keptCount < 256; ++at) {
    revocation_t *e = &revocations->entries[at];
    int listed = 0;
    if (e->hash == 0 || !e->local)
      continue;
    for (int i = 0; i < entries.count() && !listed; ++i)
      listed = strcmp(entries.at(i).get("key").text(), e->key) == 0;
    if (!listed)
      kept[keptCount++] = *e;
  }

  memset(revocations->entries, 0, sizeof revocations->entries);
  __atomic_store_n(&revocations->all, (long long)said.get("all").real(), __ATOMIC_RELEASE);

  for (int i = 0; i < entries.count(); ++i) {
    const char *key = entries.at(i).get("key").text();
    if ((strncmp(key, "sub:", 4) == 0 || strncmp(key, "sid:", 4) == 0 || strncmp(key, "ses:", 4) == 0) &&
        strlen(key) < sizeof kept[0].key &&
        !revocationPut(key, (long long)entries.at(i).get("before").real(), 0))
      ++missing;
  }

  for (int i = 0; i < keptCount; ++i)
    revocationPut(kept[i].key, kept[i].before, 1);

  pthread_mutex_unlock(&revocations->lock);
  said.release();

  if (missing > 0)
    return failed(req, 507, "the revocation table is full", "RevocationTableFull");

  return req.reply(200).json("{}");
}

static http_response_t listRevocations(http_request_t *req) {

  int localOnly = strcmp(req.query("local"), "1") == 0;
  size_t used;
  int first = 1;

  used = (size_t)snprintf(answer, sizeof answer, "{\"all\":%lld,\"entries\":[",
                          revocations != NULL ? __atomic_load_n(&revocations->all, __ATOMIC_ACQUIRE) : 0LL);

  for (unsigned at = 0; revocations != NULL && at < REVOCATIONS && used + 400 < sizeof answer; ++at) {
    revocation_t *e = &revocations->entries[at];
    if (__atomic_load_n(&e->hash, __ATOMIC_ACQUIRE) == 0 || (localOnly && !e->local) || !textual(e->key, sizeof e->key, 0))
      continue;
    used += (size_t)snprintf(answer + used, sizeof answer - used, "%s{\"key\":\"%s\",\"before\":%lld%s}",
                             first ? "" : ",", e->key, e->before, e->local ? ",\"local\":true" : "");
    first = 0;
  }

  snprintf(answer + used, sizeof answer - used, "]}");

  return req.reply(200).json(answer);
}

/**
 * A logout for a sid, a sub or a session, on this replica until the
 * controller passes it on: scoped when `scope` is given. 1 when it is in the
 * table; 0 when it cannot be - too long, not text the controller can write
 * on one line, or the table full.
 */
static int oidcRevokeLocally(const char *kind, const char *scope, const char *value) {

  char key[300];
  int put;

  if (revocations == NULL || value[0] == 0)
    return 0;

  if (scope[0] != 0)
    snprintf(key, sizeof key, "%s:%s|%s", kind, scope, value);
  else
    snprintf(key, sizeof key, "%s:%s", kind, value);

  /* a line of the ConfigMap is split at white space */
  if (strlen(key) >= sizeof revocations->entries[0].key || !textual(key, sizeof revocations->entries[0].key, 0) ||
      strpbrk(key, " \t") != NULL)
    return 0;

  revocationsLock();
  put = revocationPut(key, (long long)time(NULL) + 1, 1);
  pthread_mutex_unlock(&revocations->lock);

  if (!put)
    fprintf(stderr, "wxingress oidc: the revocation table is full, %s is not revoked\n", kind);

  return put;
}

/**
 * Back-channel logout (OpenID Connect Back-Channel Logout 1.0): the IdP posts
 * `logout_token`, a JWT signed with the keys of the jwksURI, for the client,
 * from the issuer, with the back-channel logout event, a sub or a sid and no
 * nonce. Its sessions end on every replica - those of its issuer and the
 * client, scoped (see the revocations above). A logout that cannot be kept
 * because the table is full is a 503, which the IdP may send again.
 */
static http_response_t oidcBackChannel(http_request_t *req) {

  http_body_t body = req.readBody();
  char token[JWT_MOST];
  const char *at;
  jwtParts_t parts;
  json_t claims, events;
  int good, kept = 0;
  static const char *event = "http://schemas.openid.net/event/backchannel-logout";

  if (strcmp(req->method, "POST") != 0)
    return req.reply(405).text("POST a logout_token\n");

  token[0] = 0;

  if (body.refused == NULL && body.length > 0 && body.length < sizeof token) {
    static char form[JWT_MOST + 64];
    memcpy(form, body.bytes, body.length);
    form[body.length] = 0;
    at = strstr(form, "logout_token=");
    if (at != NULL && (at == form || at[-1] == '&')) {
      size_t length = strcspn(at + 13, "&");
      snprintf(token, sizeof token, "%.*s", (int)length, at + 13);
    }
  }

  /* the token is a JWS: base64url and dots, nothing a form would encode */
  if (token[0] == 0 || !oidcIdTokenGood(req, token, NULL, NULL) || !jwtSplit(token, &parts))
    return req.reply(400).header("Cache-Control", "no-store").text("{\"error\":\"invalid_request\"}\n");

  claims = jwtPart(parts.text + parts.headerLength + 1, parts.payloadLength);
  events = claims.get("events");
  good = !events.get(event).isNothing() && claims.get("nonce").isNothing() &&
         (claims.get("sub").text()[0] != 0 || claims.get("sid").text()[0] != 0);

  if (good) {
    char scope[32];
    oidcScopeOf(claims.get("iss").text(), req.variable("wxingress_oidc_client"), scope, sizeof scope);
    if (scope[0] == 0)
      kept = 0;
    else if (claims.get("sid").text()[0] != 0)
      kept = oidcRevokeLocally("sid", scope, claims.get("sid").text());
    else
      kept = oidcRevokeLocally("sub", scope, claims.get("sub").text());
  }

  claims.release();

  if (!good)
    return req.reply(400).header("Cache-Control", "no-store").text("{\"error\":\"invalid_request\"}\n");

  if (!kept)
    return req.reply(503).header("Cache-Control", "no-store").text("{\"error\":\"temporarily_unavailable\"}\n");

  return req.reply(200).header("Cache-Control", "no-store").text("");
}

/**
 * Front-channel logout (OpenID Connect Front-Channel Logout 1.0): the IdP's
 * page loads this in an iframe with `iss` and `sid`. Nothing vouches for the
 * request - anybody can send a sid - so it ends a session only of the browser
 * that sends it: the session its cookies carry, when that session is of this
 * place, of the issuer and of the sid. The sid of its IdP and client ends on every replica
 * and its cookies go. Without the session cookie along - it is SameSite Lax,
 * and an IdP of another site loads the iframe as a third party - it ends
 * nothing; the back channel is what does it then.
 */
/** A query value percent-decoded, `+` a space. */
static void urlDecoded(const char *text, char *into, size_t room) {

  size_t used = 0;

  for (; *text != 0 && used + 1 < room; ++text) {
    if (*text == '%' && isxdigit((unsigned char)text[1]) && isxdigit((unsigned char)text[2])) {
      char hex[3] = {text[1], text[2], 0};
      into[used++] = (char)strtol(hex, NULL, 16);
      text += 2;
    } else {
      into[used++] = *text == '+' ? ' ' : *text;
    }
  }

  into[used] = 0;
}

static http_response_t oidcFrontChannel(http_request_t *req) {

  char iss[1024], sid[512], session[OIDC_CHUNKS * OIDC_CHUNK], name[16];
  const char *issuer = oidcIssuer(req);
  http_response_t answer;
  oidcJar_t jar;

  jar.count = 0;
  urlDecoded(req.query("iss"), iss, sizeof iss);
  urlDecoded(req.query("sid"), sid, sizeof sid);

  if (sid[0] != 0 && (issuer[0] == 0 || strcmp(iss, issuer) == 0) && oidcSessionOf(req, session, sizeof session) &&
      oidcSessionHere(req) && strcmp(oidcOpenedSid, sid) == 0 && (iss[0] == 0 || strcmp(oidcOpenedIss, iss) == 0)) {
    char scope[32];
    oidcScopeOf(oidcOpenedIss, oidcOpenedAud, scope, sizeof scope);
    if (scope[0] == 0 || !oidcRevokeLocally("sid", scope, sid))
      oidcNote("the session could not be revoked", "the revocation table is full");
    for (int part = 0; part < OIDC_CHUNKS; ++part) {
      snprintf(name, sizeof name, "wxoidc_s%d", part);
      oidcCookie(req, &jar, name, "", 1);
    }
  }

  answer = req.reply(200).header("Cache-Control", "no-store").mime("text/html");

  for (int at = 0; at < jar.count; ++at)
    answer = answer.header("Set-Cookie", jar.cookies[at]);

  return answer.text("<!doctype html><title>logged out</title>\n");
}
