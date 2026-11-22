# JWT and OIDC without NGINX Plus

In NGINX Ingress Controller, the `jwt` and `oidc` Policies need NGINX Plus: they are built on Plus's
`auth_jwt` and on its njs-based OpenID Connect with a key-value store. wxingress implements both in its
data plane module, so they work with open-source NGINX.

| Policy | NGINX Ingress Controller | wxingress |
|---|---|---|
| `jwt` | NGINX Plus only | with `-dynamic-upstreams` |
| `oidc` | NGINX Plus only | with `-dynamic-upstreams` and `-enable-oidc` |
| `oidcNative` | NGINX Plus only | not supported |

`-dynamic-upstreams` loads the wxingress module. The chart sets it with `controller.dynamicUpstreams` or
`controller.ingressNginxCompat`. Without it, the controller refuses `jwt` and `oidc` Policies, as
NGINX Ingress Controller does without Plus.

Both Policies apply to VirtualServers and VirtualServerRoutes, and to routes and Gateways of the Gateway
API through the annotation `wxingress.wizardtales.com/policies` (see [Gateway API](gateway-api.md)).

## JWT

```yaml
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata:
  name: jwt
spec:
  jwt:
    realm: api
    jwksURI: https://idp.example.com/realms/main/protocol/openid-connect/certs
    keyCache: 1h
---
apiVersion: k8s.nginx.org/v1
kind: VirtualServer
metadata:
  name: api
spec:
  host: api.example.com
  policies:
    - name: jwt
  upstreams:
    - name: api
      service: api
      port: 80
  routes:
    - path: /
      action:
        pass: api
```

The module checks every request in NGINX's access phase:

- the signature, with a key of the set: RS256/384/512, PS256/384/512, ES256/384/512, EdDSA (Ed25519,
  Ed448) and HS256/384/512. `none` is never accepted. The cryptography is OpenSSL's;
- `exp` and `nbf`;
- a failure answers `401` with `WWW-Authenticate: Bearer realm="<realm>"`, as `auth_jwt` does.

The token is taken from `Authorization: Bearer` unless `token` names another place (`$cookie_auth`,
`$arg_token`, `$http_x_token`).

**Keys.** `secret` names a Secret with a JWK set under the key `jwk`. The module reads the file again
when it changes, so a rotated Secret takes effect without a reload. `jwksURI` fetches the set over HTTPS;
it is kept for `keyCache` and fetched again after that, or earlier for a token whose `kid` the set does
not have, at most once every ten seconds. `sslVerify`, `trustedCertSecret`, `sniEnabled` and `sniName`
apply to that fetch.

**Upstream.** The claims of a valid token are `$meta_jwt_claim_<name>`, the fields of its header
`$meta_jwt_header_<name>`; with NGINX Plus they are `$jwt_claim_<name>` and `$jwt_header_<name>`. A string
claim is passed as it is, a number or boolean as JSON writes it, an array joined with commas (arrays
nested more than eight levels deep are left empty). A token with duplicate names or with `\u0000` inside
a string is refused, since either would let a claim compare differently than it reads.

## OIDC

```yaml
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata:
  name: login
spec:
  oidc:
    issuer: https://idp.example.com/realms/main
    clientID: app
    clientSecret: app-oidc          # Secret with the key client-secret
    scope: openid+profile+email
    pkceEnable: true
    accessTokenEnable: true
```

`authEndpoint`, `tokenEndpoint`, `jwksURI` and `endSessionEndpoint` may be left out when `issuer` is set:
the module reads them from the provider's discovery document
(`<issuer>/.well-known/openid-configuration`) and refuses a document that names another issuer.

Install with OIDC enabled:

```sh
helm upgrade --install wxingress oci://ghcr.io/wx-one/charts/wxingress \
  --namespace wxingress \
  --set controller.ingressNginxCompat=true \
  --set controller.enableOIDC=true
```

### The flow

The Policy runs the authorization code flow, with PKCE when `pkceEnable` is set:

1. A request without a session is redirected to the authorization endpoint with a new state, nonce and
   PKCE verifier. They travel in a sealed cookie of their own (`wxoidc_state`).
2. The provider sends the browser back to `redirectURI` (default `/_codexch`). The state must match the
   cookie. The code is exchanged at the token endpoint, with `client_secret_basic`, or with the client ID
   alone for a public client with PKCE.
3. The ID token must be signed by a key of the provider's JWK set, name the client in `aud`, carry the
   nonce, name the issuer in `iss` and not be expired.
4. When the ID token has expired and there is a refresh token, the module refreshes the tokens and sends
   the request to itself again with `307`, which keeps the method and the body. Without a refresh token,
   or when the refresh fails, the user logs in again.

### Sessions without a key-value store

NGINX Plus keeps OIDC sessions in key-value zones and shares them between replicas with `zone_sync`.
wxingress keeps nothing on the server: the session is a cookie with the ID, access and refresh token,
sealed with AES-256-GCM. It is split into `wxoidc_s0` to `wxoidc_s3` when it does not fit one cookie.
The cookies are `HttpOnly`, `SameSite=Lax` and, over HTTPS, `Secure`.

The key is derived from a Secret the chart creates with `controller.enableOIDC` (`<release>-oidc-session`,
kept across upgrades) and mounts into every pod. Every worker and every replica therefore opens every
session, and scaling or restarting the controller logs nobody out. A session is bound to its Policy,
issuer, client and server; it cannot be replayed under another.

Because there are no zones, the ConfigMap keys `oidc-*-timeout` and `oidc-*-zone-size` and the Policy
field `zoneSyncLeeway` have no effect with the wxingress module.

### Logout

| Location | What it does |
|---|---|
| `/logout` | Ends the session and redirects to the provider's end session endpoint with `id_token_hint` and `post_logout_redirect_uri`, or straight to `postLogoutRedirectURI` when the provider has none. |
| `postLogoutRedirectURI` (default `/_logout`) | Where the browser lands after logout. |
| `/_backchannel_logout` | OpenID Connect Back-Channel Logout: the provider POSTs a logout token. |
| `/front_channel_logout` | OpenID Connect Front-Channel Logout. |

Since a session lives in the browser, a logout is a revocation: the module remembers which sessions
ended. With the chart, the controller keeps the revocations in a ConfigMap
(`-oidc-revocations-configmap`, `<release>-oidc-revocations`), one per line under the key `revocations`:

```text
all <unix>            every session created before that time
sub <sub> <unix>      the sessions of one user
sid <sid> <unix>      the sessions of one provider session
ses <id> <unix>       one session (what /logout ends)
```

Each replica writes the logouts it handled into the ConfigMap and gives its module the whole list every
few seconds, without a reload, so a logout reaches all replicas. An administrator can end sessions by
adding a line, for example `sub alice 1795000000`. Lines older than a month are removed.

### Upstream

With the Policy on a route, the upstream receives the `sub` claim in the header `username` and, with
`accessTokenEnable`, the access token as `Authorization: Bearer`. In snippets, the claims of the ID
token are `$meta_oidc_claim_<name>`, the tokens `$meta_oidc_access_token` and `$meta_oidc_id_token`.

### Differences from NGINX Plus

- No key-value zones and no `zone_sync`; see above.
- `sslVerifyDepth` is not applied, neither for `oidc` nor for `jwt`.
- `oidcNative` (Plus's built-in OIDC module) is not supported.
