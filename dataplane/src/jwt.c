/**
 * JSON Web Tokens, validated in nginx's access phase: what NGINX Plus's
 * `auth_jwt` does for NGINX Ingress Controller's jwt Policy, without Plus.
 *
 * The controller writes, where a jwt Policy applies - a server or a
 * location -
 *
 *   set $wxingress_jwt_realm "<realm>";        the Policy's realm; "" for none
 *   set $wxingress_jwt_keys  "<file>";         the JWK set of its Secret, or
 *   set $wxingress_jwt_jwks  "<url>";          the jwksURI, fetched
 *   set $wxingress_jwt_cache "<time>";         and kept that long (keyCache)
 *   set $wxingress_jwt_ca    "<file>";         the CA its server chains to
 *   set $wxingress_jwt_token_from <name>;      the variable the token is in,
 *                                              without its $ (cookie_auth,
 *                                              arg_token, http_x_token);
 *                                              Authorization: Bearer without
 *
 * and an access step here checks every request whose realm is not empty:
 * the token's signature with a key of the set, its `exp` and `nbf` (with
 * `$wxingress_jwt_leeway` seconds of leeway, 0 unless set), and answers 401
 * with `WWW-Authenticate: Bearer realm="<realm>"` when anything is wrong -
 * the answer of `auth_jwt`.
 *
 * Algorithms: RS256/384/512, PS256/384/512, ES256/384/512, EdDSA (Ed25519,
 * Ed448) and HS256/384/512 - the key types RSA, EC, OKP and oct. Never
 * `none`. The verification is OpenSSL's (EVP), nothing of it is written here;
 * this file decodes, picks the key and compares times.
 *
 * A key set from a file is read again when the file changes, which is how a
 * rotated Secret arrives - the controller rewrites the file and reloads
 * nothing. One from a URL is kept for `$wxingress_jwt_cache` and fetched
 * again after it, or sooner for a token whose `kid` the set does not have -
 * at most once in ten seconds, so a stream of made-up kids does not become a
 * stream of fetches. Each worker keeps its own sets.
 *
 * The claims of a token that passed are `$meta_jwt_claim_<name>` - a string
 * as it is, a number or a boolean as JSON writes it, an array of those
 * joined with commas - and the fields of its header `$meta_jwt_header_<name>`,
 * as `$jwt_claim_<name>` and `$jwt_header_<name>` are NGINX Plus's.
 */
#include <meta_fetch.h>

#include <openssl/bn.h>
#include <openssl/core_names.h>
#include <openssl/crypto.h>
#include <openssl/ec.h>
#include <openssl/evp.h>
#include <openssl/param_build.h>
#include <openssl/rsa.h>

#include <ctype.h>
#include <strings.h>
#include <sys/stat.h>

/** The longest token taken; a longer one is a 401. */
#define JWT_MOST 16384

/** Keys in one set, and sets one worker keeps. */
#define JWT_KEYS 32
#define JWT_SETS 32

/** How soon a set from a URL may be fetched again for an unknown kid. */
#define JWT_REFETCH_SECONDS 10

typedef struct {
  char kid[160];
  char kty[8];
  char alg[16];
  /** RSA, EC and OKP keys */
  EVP_PKEY *key;
  /** an oct key's secret */
  unsigned char secret[512];
  size_t secretLength;
} jwtKey_t;

typedef struct {
  /** the file or the URL */
  char source[1024];
  int fromURL;
  /** the file's modification time and size when it was read */
  struct timespec modified;
  off_t size;
  /** when a URL's set was fetched, and when it is due again */
  time_t fetched;
  time_t due;
  /** when the file was last looked at: at most once a second */
  time_t looked;
  /** new with every set of keys, so the tokens verified with the last one go */
  unsigned long generation;
  int count;
  jwtKey_t keys[JWT_KEYS];
} jwtSet_t;

static unsigned long jwtGenerations;

static jwtSet_t jwtSets[JWT_SETS];

/* ------------------------------------------------------------- decoding */

/**
 * base64url without padding, as JOSE writes it, into `into`: the number of
 * bytes, or -1 for what is not base64url or does not fit.
 */
static long base64url(const char *text, size_t length, unsigned char *into, size_t room) {

  static char padded[JWT_MOST + 4];
  size_t at;
  int decoded;
  size_t pad;

  if (length + 4 > sizeof padded)
    return -1;

  for (at = 0; at < length; ++at) {

    char c = text[at];

    if (c == '-')
      c = '+';
    else if (c == '_')
      c = '/';
    else if (!isalnum((unsigned char)c))
      return -1;

    padded[at] = c;
  }

  pad = (4 - length % 4) % 4;

  if (pad == 3)
    return -1;

  for (size_t p = 0; p < pad; ++p)
    padded[at++] = '=';

  padded[at] = 0;

  if (at / 4 * 3 > room)
    return -1;

  decoded = EVP_DecodeBlock(into, (const unsigned char *)padded, (int)at);

  if (decoded < 0)
    return -1;

  return (long)(decoded - (long)pad);
}

/** A base64url member of a JWK, decoded into `into`; -1 when it is missing or wrong. */
static long jwkBytes(json_t key, const char *name, unsigned char *into, size_t room) {

  const char *text = key.get(name).text();

  if (text == NULL || text[0] == 0)
    return -1;

  return base64url(text, strlen(text), into, room);
}

/* ------------------------------------------------------------------ keys */

static EVP_PKEY *rsaKey(json_t key) {

  static unsigned char n[1024], e[16];
  long nLength = jwkBytes(key, "n", n, sizeof n);
  long eLength = jwkBytes(key, "e", e, sizeof e);
  EVP_PKEY *made = NULL;
  OSSL_PARAM_BLD *build;
  OSSL_PARAM *params = NULL;
  EVP_PKEY_CTX *ctx;
  BIGNUM *bn = NULL, *be = NULL;

  if (nLength <= 0 || eLength <= 0)
    return NULL;

  bn = BN_bin2bn(n, (int)nLength, NULL);
  be = BN_bin2bn(e, (int)eLength, NULL);
  build = OSSL_PARAM_BLD_new();

  if (bn != NULL && be != NULL && build != NULL &&
      OSSL_PARAM_BLD_push_BN(build, OSSL_PKEY_PARAM_RSA_N, bn) &&
      OSSL_PARAM_BLD_push_BN(build, OSSL_PKEY_PARAM_RSA_E, be))
    params = OSSL_PARAM_BLD_to_param(build);

  ctx = EVP_PKEY_CTX_new_from_name(NULL, "RSA", NULL);

  if (params != NULL && ctx != NULL && EVP_PKEY_fromdata_init(ctx) == 1)
    EVP_PKEY_fromdata(ctx, &made, EVP_PKEY_PUBLIC_KEY, params);

  EVP_PKEY_CTX_free(ctx);
  OSSL_PARAM_free(params);
  OSSL_PARAM_BLD_free(build);
  BN_free(bn);
  BN_free(be);

  return made;
}

static EVP_PKEY *ecKey(json_t key) {

  static unsigned char x[80], y[80], point[1 + 2 * 80];
  const char *crv = key.get("crv").text();
  const char *group;
  size_t size;
  long xLength, yLength;
  EVP_PKEY *made = NULL;
  OSSL_PARAM params[3];
  EVP_PKEY_CTX *ctx;

  if (strcmp(crv, "P-256") == 0) {
    group = "prime256v1";
    size = 32;
  } else if (strcmp(crv, "P-384") == 0) {
    group = "secp384r1";
    size = 48;
  } else if (strcmp(crv, "P-521") == 0) {
    group = "secp521r1";
    size = 66;
  } else {
    return NULL;
  }

  xLength = jwkBytes(key, "x", x, sizeof x);
  yLength = jwkBytes(key, "y", y, sizeof y);

  if (xLength != (long)size || yLength != (long)size)
    return NULL;

  /* the uncompressed point: 04 || x || y */
  point[0] = 4;
  memcpy(point + 1, x, size);
  memcpy(point + 1 + size, y, size);

  params[0] = OSSL_PARAM_construct_utf8_string(OSSL_PKEY_PARAM_GROUP_NAME, (char *)group, 0);
  params[1] = OSSL_PARAM_construct_octet_string(OSSL_PKEY_PARAM_PUB_KEY, point, 1 + 2 * size);
  params[2] = OSSL_PARAM_construct_end();

  ctx = EVP_PKEY_CTX_new_from_name(NULL, "EC", NULL);

  if (ctx != NULL && EVP_PKEY_fromdata_init(ctx) == 1)
    EVP_PKEY_fromdata(ctx, &made, EVP_PKEY_PUBLIC_KEY, params);

  EVP_PKEY_CTX_free(ctx);

  return made;
}

static EVP_PKEY *okpKey(json_t key) {

  static unsigned char x[64];
  const char *crv = key.get("crv").text();
  long length = jwkBytes(key, "x", x, sizeof x);

  if (strcmp(crv, "Ed25519") == 0 && length == 32)
    return EVP_PKEY_new_raw_public_key(EVP_PKEY_ED25519, NULL, x, 32);

  if (strcmp(crv, "Ed448") == 0 && length == 57)
    return EVP_PKEY_new_raw_public_key(EVP_PKEY_ED448, NULL, x, 57);

  return NULL;
}

static void jwtSetEmpty(jwtSet_t *set) {

  for (int at = 0; at < set->count; ++at)
    EVP_PKEY_free(set->keys[at].key);

  set->count = 0;
}

/** The keys of a JWK set - or of one JWK - into `set`; how many were taken. */
static int jwtSetRead(jwtSet_t *set, const char *text, size_t length) {

  json_t doc = meta_toJSONn(text, length);
  json_t keys;
  int count;

  jwtSetEmpty(set);

  if (doc._refused != NULL) {
    doc.release();
    return 0;
  }

  keys = doc.get("keys");
  count = keys.isNothing() ? 1 : keys.count();

  for (int at = 0; at < count && set->count < JWT_KEYS; ++at) {

    json_t key = keys.isNothing() ? doc : keys.at(at);
    jwtKey_t *into = &set->keys[set->count];
    const char *kty = key.get("kty").text();

    memset(into, 0, sizeof *into);
    snprintf(into->kid, sizeof into->kid, "%s", key.get("kid").text());
    snprintf(into->kty, sizeof into->kty, "%s", kty);
    snprintf(into->alg, sizeof into->alg, "%s", key.get("alg").text());

    if (strcmp(key.get("use").text(), "enc") == 0)
      continue;

    if (strcmp(kty, "RSA") == 0)
      into->key = rsaKey(key);
    else if (strcmp(kty, "EC") == 0)
      into->key = ecKey(key);
    else if (strcmp(kty, "OKP") == 0)
      into->key = okpKey(key);
    else if (strcmp(kty, "oct") == 0) {
      long got = jwkBytes(key, "k", into->secret, sizeof into->secret);
      if (got > 0)
        into->secretLength = (size_t)got;
    }

    if (into->key != NULL || into->secretLength > 0)
      ++set->count;
  }

  doc.release();

  return set->count;
}

/** nginx's time - 30s, 10m, 1h, 1d or seconds - in seconds; 0 for none. */
static long jwtSeconds(const char *text) {

  char *end;
  long n = strtol(text, &end, 10);

  if (n <= 0)
    return 0;

  switch (*end) {
  case 'm':
    return n * 60;
  case 'h':
    return n * 3600;
  case 'd':
    return n * 86400;
  default:
    return n;
  }
}

/**
 * The set of a source, read or fetched when it has to be. A fetch waits,
 * which an access step may. NULL when there is none to be had.
 */
/**
 * How a key set is fetched over TLS: the CA, the server name (SNI and the
 * certificate's) and whether the server is verified at all - NGINX Ingress
 * Controller's trustedCertSecret, sniName with sniEnabled and sslVerify,
 * whose default is no verification, as nginx's proxy_ssl_verify.
 */
typedef struct {
  const char *ca;
  const char *sni;
  int verify;
} jwtTls_t;

/** The TLS settings of `<prefix>_ca`, `_sni` and `_verify`. */
static jwtTls_t jwtTlsOf(http_request_t *req, const char *prefix) {

  char name[64];
  jwtTls_t tls;

  snprintf(name, sizeof name, "%s_ca", prefix);
  tls.ca = req.variable(name);
  snprintf(name, sizeof name, "%s_sni", prefix);
  tls.sni = req.variable(name);
  snprintf(name, sizeof name, "%s_verify", prefix);
  tls.verify = req.variable(name)[0] == '1';

  return tls;
}

static jwtSet_t *jwtSetOf(const char *source, int fromURL, const char *ttl,
                          const jwtTls_t *tls, int unknownKid) {

  jwtSet_t *set = NULL;
  jwtSet_t *free = NULL;
  time_t now = time(NULL);

  if (source[0] == 0 || strlen(source) >= sizeof jwtSets[0].source)
    return NULL;

  for (int at = 0; at < JWT_SETS; ++at) {
    if (jwtSets[at].source[0] == 0) {
      if (free == NULL)
        free = &jwtSets[at];
      continue;
    }
    if (jwtSets[at].fromURL == fromURL && strcmp(jwtSets[at].source, source) == 0) {
      set = &jwtSets[at];
      break;
    }
  }

  if (set == NULL) {
    if (free == NULL) {
      /* full: the first one goes */
      free = &jwtSets[0];
      jwtSetEmpty(free);
    }
    set = free;
    memset(set, 0, sizeof *set);
    snprintf(set->source, sizeof set->source, "%s", source);
    set->fromURL = fromURL;
  }

  if (!fromURL) {

    struct stat info;
    FILE *file;
    static char text[256 * 1024];
    size_t length;

    /* a rotated Secret is a new file: looked for once a second, not on every request */
    if (set->count > 0 && set->looked == now)
      return set;

    set->looked = now;

    if (stat(source, &info) != 0)
      return set->count > 0 ? set : NULL;

    if (set->count > 0 && info.st_mtim.tv_sec == set->modified.tv_sec &&
        info.st_mtim.tv_nsec == set->modified.tv_nsec && info.st_size == set->size)
      return set;

    file = fopen(source, "r");

    if (file == NULL)
      return set->count > 0 ? set : NULL;

    length = fread(text, 1, sizeof text, file);
    fclose(file);

    if (jwtSetRead(set, text, length) > 0) {
      set->modified = info.st_mtim;
      set->size = info.st_size;
      set->generation = ++jwtGenerations;
    }

    return set->count > 0 ? set : NULL;
  }

  if (set->count > 0 && now < set->due &&
      !(unknownKid && now - set->fetched >= JWT_REFETCH_SECONDS))
    return set;

  if (set->count > 0 && now - set->fetched < JWT_REFETCH_SECONDS)
    return set;

  {
    fetch_call_t call = meta_fetch("GET", source).timeout(5).connectTimeout(3).retry(3);
    fetch_answer_t got;
    jwtSet_t fresh;

    if (tls != NULL) {
      call = call.verifyPeer(tls->verify);
      if (tls->verify && tls->ca[0] != 0)
        call = call.trust(tls->ca);
      if (tls->sni[0] != 0)
        call = call.serverName(tls->sni);
    }

    got = call.send();

    /* the slot may have gone to another source while this one waited */
    if (strcmp(set->source, source) != 0 || set->fromURL != fromURL) {
      got.release();
      return NULL;
    }

    set->fetched = now;

    if (got.ok && got.status == 200) {

      const char *text = got.text();

      memset(&fresh, 0, sizeof fresh);

      if (jwtSetRead(&fresh, text, strlen(text)) > 0) {
        jwtSetEmpty(set);
        memcpy(set->keys, fresh.keys, sizeof fresh.keys);
        set->count = fresh.count;
        set->generation = ++jwtGenerations;
      }
    }

    got.release();

    /* a set that has no keys is fetched again on the next request after the refetch time */
    set->due = now + (jwtSeconds(ttl) > 0 ? jwtSeconds(ttl) : JWT_REFETCH_SECONDS);
  }

  return set->count > 0 ? set : NULL;
}

/* -------------------------------------------------------------- checking */

/**
 * The digests, fetched once per worker: OpenSSL 3 looks EVP_sha256() and its
 * kin up in a locked table every time one is used to initialise a context,
 * which is most of what a cached token would cost otherwise.
 */
static EVP_MD *jwtSha(int bits) {

  static EVP_MD *fetched[3];
  int at = bits == 256 ? 0 : bits == 384 ? 1 : 2;

  if (fetched[at] == NULL)
    fetched[at] = EVP_MD_fetch(NULL, bits == 256 ? "SHA256" : bits == 384 ? "SHA384" : "SHA512", NULL);

  return fetched[at];
}

static const EVP_MD *jwtDigest(const char *alg) {

  const char *bits = alg + 2;

  if (strcmp(bits, "256") == 0)
    return jwtSha(256);
  if (strcmp(bits, "384") == 0)
    return jwtSha(384);
  if (strcmp(bits, "512") == 0)
    return jwtSha(512);

  return NULL;
}

/** Whether the signature of `input` is the key's, by the algorithm. */
static int jwtVerified(const jwtKey_t *key, const char *alg, const char *input, size_t inputLength,
                       const unsigned char *signature, size_t signatureLength) {

  int good = 0;
  EVP_MD_CTX *ctx;
  EVP_PKEY_CTX *pctx = NULL;
  const EVP_MD *md;

  if (key->alg[0] != 0 && strcmp(key->alg, alg) != 0)
    return 0;

  if (strncmp(alg, "HS", 2) == 0) {

    unsigned char mac[EVP_MAX_MD_SIZE];
    size_t macLength = sizeof mac;

    md = jwtDigest(alg);

    if (md == NULL || strcmp(key->kty, "oct") != 0 || key->secretLength == 0)
      return 0;

    if (EVP_Q_mac(NULL, "HMAC", NULL, EVP_MD_get0_name(md), NULL, key->secret, key->secretLength,
                  (const unsigned char *)input, inputLength, mac, sizeof mac, &macLength) == NULL)
      return 0;

    return macLength == signatureLength && CRYPTO_memcmp(mac, signature, macLength) == 0;
  }

  if (key->key == NULL)
    return 0;

  /* one context for the worker, reset after each use: verifying does not wait */
  {
    static EVP_MD_CTX *kept;
    if (kept == NULL)
      kept = EVP_MD_CTX_new();
    ctx = kept;
  }

  if (ctx == NULL)
    return 0;

  if (strcmp(alg, "EdDSA") == 0) {

    if (strcmp(key->kty, "OKP") == 0 && EVP_DigestVerifyInit(ctx, NULL, NULL, NULL, key->key) == 1)
      good = EVP_DigestVerify(ctx, signature, signatureLength, (const unsigned char *)input, inputLength) == 1;

  } else if (strncmp(alg, "RS", 2) == 0 || strncmp(alg, "PS", 2) == 0) {

    md = jwtDigest(alg);

    if (md != NULL && strcmp(key->kty, "RSA") == 0 &&
        EVP_DigestVerifyInit(ctx, &pctx, md, NULL, key->key) == 1) {

      if (alg[0] == 'P') {
        EVP_PKEY_CTX_set_rsa_padding(pctx, RSA_PKCS1_PSS_PADDING);
        EVP_PKEY_CTX_set_rsa_pss_saltlen(pctx, RSA_PSS_SALTLEN_DIGEST);
      }

      good = EVP_DigestVerify(ctx, signature, signatureLength, (const unsigned char *)input, inputLength) == 1;
    }

  } else if (strncmp(alg, "ES", 2) == 0) {

    /* JOSE writes r || s; OpenSSL verifies the DER of ECDSA-Sig-Value */
    size_t half = signatureLength / 2;
    ECDSA_SIG *sig = ECDSA_SIG_new();
    unsigned char *der = NULL;
    int derLength = 0;

    md = jwtDigest(alg);

    if (sig != NULL && md != NULL && strcmp(key->kty, "EC") == 0 && signatureLength % 2 == 0 &&
        (half == 32 || half == 48 || half == 66)) {

      BIGNUM *r = BN_bin2bn(signature, (int)half, NULL);
      BIGNUM *s = BN_bin2bn(signature + half, (int)half, NULL);

      if (r != NULL && s != NULL && ECDSA_SIG_set0(sig, r, s) == 1) {
        derLength = i2d_ECDSA_SIG(sig, &der);
      } else {
        BN_free(r);
        BN_free(s);
      }

      if (derLength > 0 && EVP_DigestVerifyInit(ctx, NULL, md, NULL, key->key) == 1)
        good = EVP_DigestVerify(ctx, der, (size_t)derLength, (const unsigned char *)input, inputLength) == 1;
    }

    OPENSSL_free(der);
    ECDSA_SIG_free(sig);
  }

  EVP_MD_CTX_reset(ctx);

  return good;
}

/**
 * The token of the request: in the variable the configuration names, else
 * Authorization: Bearer - one or the other, as auth_jwt's token= is.
 */
static const char *jwtTokenOf(http_request_t *req) {

  const char *from = req.variable("wxingress_jwt_token_from");
  const char *authorization;

  if (from[0] != 0)
    return req.variable(from);

  authorization = req.header("Authorization");

  if (strncasecmp(authorization, "Bearer ", 7) != 0)
    return "";

  authorization += 7;

  while (*authorization == ' ')
    ++authorization;

  return authorization;
}

/** The three parts of a compact JWS: where the dots are. */
typedef struct {
  const char *text;
  size_t headerLength, payloadLength, signatureLength;
} jwtParts_t;

static int jwtSplit(const char *token, jwtParts_t *parts) {

  const char *first = strchr(token, '.');
  const char *second = first != NULL ? strchr(first + 1, '.') : NULL;

  if (second == NULL || strchr(second + 1, '.') != NULL || strlen(token) > JWT_MOST)
    return 0;

  parts->text = token;
  parts->headerLength = (size_t)(first - token);
  parts->payloadLength = (size_t)(second - first - 1);
  parts->signatureLength = strlen(second + 1);

  return parts->headerLength > 0 && parts->payloadLength > 0;
}

/** The JSON of one part, decoded; the document is the caller's to release. */
static json_t jwtPart(const char *text, size_t length) {

  static unsigned char decoded[JWT_MOST];
  long got = base64url(text, length, decoded, sizeof decoded);

  return meta_toJSONn((const char *)decoded, got > 0 ? (size_t)got : 0);
}

/* ---------------------------------------------------- verified tokens */

/**
 * The tokens a worker verified, by SHA-256 of the key set's source and the
 * token: a token that comes again - every request of a client does - is a
 * digest and a lookup, not a signature check (RS256 is ~250 000 instructions,
 * EdDSA over a million). An entry holds until the token's exp, or a minute,
 * whichever is first, and only for the generation of the key set it was
 * verified with: a new set of keys - a rotated Secret, a fetched jwksURI -
 * verifies again. Direct-mapped and bounded: a new token takes the slot of
 * whatever was there.
 */
#define JWT_VERIFIED 4096
#define JWT_VERIFIED_SECONDS 60

typedef struct {
  unsigned char digest[32];
  unsigned long generation;
  time_t until;
} jwtVerified_t;

static jwtVerified_t jwtVerifiedTokens[JWT_VERIFIED];

static jwtVerified_t *jwtVerifiedSlot(const char *source, const char *token, unsigned char digest[32]) {

  static EVP_MD_CTX *ctx;
  unsigned int length = 0;
  unsigned int slot;

  if (ctx == NULL)
    ctx = EVP_MD_CTX_new();

  if (ctx == NULL || jwtSha(256) == NULL)
    return NULL;

  EVP_DigestInit_ex(ctx, jwtSha(256), NULL);
  EVP_DigestUpdate(ctx, source, strlen(source) + 1);
  EVP_DigestUpdate(ctx, token, strlen(token));
  EVP_DigestFinal_ex(ctx, digest, &length);

  memcpy(&slot, digest, sizeof slot);

  return &jwtVerifiedTokens[slot % JWT_VERIFIED];
}

/** Why the token is not good, or NULL when it is. */
static const char *jwtCheck(http_request_t *req, const char *token) {

  jwtParts_t parts;
  json_t header, payload;
  char alg[16], kid[160];
  unsigned char signature[1024];
  long signatureLength;
  const char *keys = req.variable("wxingress_jwt_keys");
  const char *jwks = req.variable("wxingress_jwt_jwks");
  int fromURL = keys[0] == 0;
  jwtSet_t *set;
  int good = 0, kidKnown = 0;
  long leeway = strtol(req.variable("wxingress_jwt_leeway"), NULL, 10);
  time_t now = time(NULL);

  unsigned char digest[32];
  jwtVerified_t *slot;
  double expires = 0;

  if (token[0] == 0)
    return "no token";

  /* a token this worker verified with this set of keys, still good */
  slot = jwtVerifiedSlot(fromURL ? jwks : keys, token, digest);
  jwtTls_t tls = jwtTlsOf(req, "wxingress_jwt");

  set = jwtSetOf(fromURL ? jwks : keys, fromURL, req.variable("wxingress_jwt_cache"), &tls, 0);

  if (slot != NULL && set != NULL && memcmp(slot->digest, digest, sizeof digest) == 0 &&
      slot->generation == set->generation && now < slot->until)
    return NULL;

  if (!jwtSplit(token, &parts))
    return "not a JWS";

  header = jwtPart(parts.text, parts.headerLength);

  if (header._refused != NULL) {
    header.release();
    return "the header is not JSON";
  }

  snprintf(alg, sizeof alg, "%s", header.get("alg").text());
  snprintf(kid, sizeof kid, "%s", header.get("kid").text());
  header.release();

  if (alg[0] == 0 || strcmp(alg, "none") == 0)
    return "no algorithm";

  signatureLength = base64url(parts.text + parts.headerLength + parts.payloadLength + 2,
                              parts.signatureLength, signature, sizeof signature);

  if (signatureLength <= 0)
    return "no signature";

  for (int pass = 0; pass < 2 && !good; ++pass) {

    set = jwtSetOf(fromURL ? jwks : keys, fromURL, req.variable("wxingress_jwt_cache"), &tls, pass == 1);

    if (set == NULL)
      return "no keys";

    for (int at = 0; at < set->count && !good; ++at) {

      if (kid[0] != 0 && set->keys[at].kid[0] != 0 && strcmp(kid, set->keys[at].kid) != 0)
        continue;

      if (kid[0] != 0 && strcmp(kid, set->keys[at].kid) == 0)
        kidKnown = 1;

      good = jwtVerified(&set->keys[at], alg, parts.text, parts.headerLength + 1 + parts.payloadLength,
                         signature, (size_t)signatureLength);
    }

    /* a kid the set from a URL does not have: fetched once more */
    if (good || !fromURL || kid[0] == 0 || kidKnown)
      break;
  }

  if (!good)
    return "the signature is not good";

  payload = jwtPart(parts.text + parts.headerLength + 1, parts.payloadLength);

  if (payload._refused != NULL) {
    payload.release();
    return "the payload is not JSON";
  }

  if (!payload.get("exp").isNothing() && (double)now > payload.get("exp").real() + (double)leeway) {
    payload.release();
    return "expired";
  }

  if (!payload.get("nbf").isNothing() && (double)now + (double)leeway < payload.get("nbf").real()) {
    payload.release();
    return "not yet valid";
  }

  if (!payload.get("exp").isNothing())
    expires = payload.get("exp").real() + (double)leeway;

  payload.release();

  /* remembered until it expires, or a minute, whichever is first */
  if (slot != NULL) {
    time_t until = now + JWT_VERIFIED_SECONDS;
    if (expires > 0 && expires < (double)until)
      until = (time_t)expires;
    memcpy(slot->digest, digest, sizeof digest);
    slot->generation = set->generation;
    slot->until = until;
  }

  return NULL;
}

/** The access step: a request whose realm is set needs a good token. */
static http_response_t jwtGuard(http_request_t *req) {

  static char challenge[512];
  const char *realm = req.variable("wxingress_jwt_realm");
  const char *token;
  const char *wrong;

  if (realm[0] == 0)
    return req.reply(0);

  token = jwtTokenOf(req);
  wrong = jwtCheck(req, token);

  if (wrong == NULL)
    return req.reply(0);

  snprintf(challenge, sizeof challenge, "Bearer realm=\"%s\"%s", realm,
           token[0] != 0 ? ", error=\"invalid_token\"" : "");

  return req.reply(401)
      .header("WWW-Authenticate", challenge)
      .text("401 Authorization Required\n");
}

/* ------------------------------------------------------------ the claims */

/** A value of a claim or a header field as nginx text into `into`. */
static void jwtValue(json_t value, char *into, size_t room) {

  const char *kind = value.kind();

  into[0] = 0;

  if (strcmp(kind, "string") == 0) {
    snprintf(into, room, "%s", value.text());
  } else if (strcmp(kind, "number") == 0) {
    double real = value.real();
    if (real == (double)(long long)real)
      snprintf(into, room, "%lld", (long long)real);
    else
      snprintf(into, room, "%g", real);
  } else if (strcmp(kind, "bool") == 0) {
    snprintf(into, room, "%s", value.truth() ? "true" : "false");
  } else if (strcmp(kind, "array") == 0) {
    size_t used = 0;
    for (int at = 0; at < value.count() && used + 1 < room; ++at) {
      char one[1024];
      jwtValue(value.at(at), one, sizeof one);
      used += (size_t)snprintf(into + used, room - used, "%s%s", at > 0 ? "," : "", one);
    }
  }
}

/**
 * A member of a part of the token by a name nginx lowered: the exact name
 * first, then one that differs in case only.
 */
static json_t jwtMember(json_t object, const char *name) {

  json_t exact = object.get(name);

  if (!exact.isNothing())
    return exact;

  for (int at = 0; at < object.count(); ++at)
    if (strcasecmp(object.keyAt(at), name) == 0)
      return object.get(object.keyAt(at));

  return exact;
}

/** `$meta_jwt_claim_<name>` and `$meta_jwt_header_<name>`, of a token that passed. */
static const char *jwtVariable(http_request_t *req, const char *argument) {

  static char value[4096];
  jwtParts_t parts;
  json_t part;
  int claim;
  const char *name;

  if (strncmp(argument, "claim_", 6) == 0) {
    claim = 1;
    name = argument + 6;
  } else if (strncmp(argument, "header_", 7) == 0) {
    claim = 0;
    name = argument + 7;
  } else {
    return "";
  }

  /* only where the access step checked the token */
  if (req.variable("wxingress_jwt_realm")[0] == 0 || !jwtSplit(jwtTokenOf(req), &parts))
    return "";

  part = claim ? jwtPart(parts.text + parts.headerLength + 1, parts.payloadLength)
               : jwtPart(parts.text, parts.headerLength);

  if (part._refused != NULL) {
    part.release();
    return "";
  }

  jwtValue(jwtMember(part, name), value, sizeof value);
  part.release();

  return value;
}
