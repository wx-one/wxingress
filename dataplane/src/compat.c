/*
 * The variables the configuration otherwise asked njs for - so that an nginx
 * with this module loads no njs at all, its library, its libxml2 and the
 * compiled scripts of every process.
 *
 *   $meta_ingress_nginx_escaped_request_uri
 *       $request_uri escaped as ingress-nginx's set_escape_uri does: every
 *       byte but A-Z a-z 0-9 - . _ ~ as %XX (the auth-signin redirect's rd)
 *   $meta_ingress_nginx_apikey_hash
 *       the SHA-256 of $header_query_value in lower-case hex, which the map
 *       of an APIKey Policy's clients is keyed by
 *   $meta_ingress_nginx_apikey_status
 *       what the APIKey check answers: 401 without a key, 403 with one that
 *       names no client of the map $apikey_auth_local_map names, 204 else
 *
 * Each is asked for once per request, where the configuration names it.
 */

#include <openssl/evp.h>

/** Room for a $request_uri of nginx's largest header buffer, every byte escaped. */
static char escapedURI[3 * 65536 + 1];

static const char *escapeRequestURI(const char *uri) {

  static const char hex[] = "0123456789ABCDEF";
  size_t at = 0;

  for (const unsigned char *c = (const unsigned char *)uri; *c != 0; ++c) {

    if (at + 4 > sizeof escapedURI)
      return "";

    if ((*c >= 'A' && *c <= 'Z') || (*c >= 'a' && *c <= 'z') || (*c >= '0' && *c <= '9') ||
        *c == '-' || *c == '.' || *c == '_' || *c == '~') {
      escapedURI[at++] = (char)*c;
    } else {
      escapedURI[at++] = '%';
      escapedURI[at++] = hex[*c >> 4];
      escapedURI[at++] = hex[*c & 15];
    }
  }

  escapedURI[at] = 0;
  return escapedURI;
}

static const char *apikeyHash(const char *key) {

  static char text[2 * EVP_MAX_MD_SIZE + 1];
  unsigned char digest[EVP_MAX_MD_SIZE];
  unsigned int length = 0;

  if (EVP_Digest(key, strlen(key), digest, &length, EVP_sha256(), NULL) != 1)
    return "";

  for (unsigned int i = 0; i < length; ++i)
    snprintf(text + 2 * i, 3, "%02x", digest[i]);

  return text;
}

static const char *ingressNginxVariable(http_request_t *req, const char *argument) {

  if (strcmp(argument, "escaped_request_uri") == 0)
    return escapeRequestURI(req.variable("request_uri"));

  if (strcmp(argument, "apikey_hash") == 0)
    return apikeyHash(req.variable("header_query_value"));

  if (strcmp(argument, "apikey_status") == 0) {

    const char *map = req.variable("apikey_auth_local_map");

    if (req.variable("header_query_value")[0] == 0)
      return "401";
    if (map[0] == 0 || req.variable(map)[0] == 0)
      return "403";
    return "204";
  }

  return "";
}
