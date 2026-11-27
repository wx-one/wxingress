# Lua to njs

ingress-nginx is built on OpenResty and lets users run Lua in snippets: `access_by_lua_block`,
`rewrite_by_lua_block`, `content_by_lua_block`, `header_filter_by_lua_block`, `body_filter_by_lua_block`,
`log_by_lua_block` and the `*_by_lua_file` variants. **wxingress has no Lua.** Its NGINX is the open-source
NGINX from nginx.org. Its dynamic upstreams and metrics come from a data plane module, not from Lua.

What NGINX offers instead is **[njs](https://nginx.org/en/docs/njs/)**, a JavaScript interpreter that runs
inside NGINX. The wxingress image ships njs (`ngx_http_js_module`). Most Lua snippets in Ingresses are
small: they read a header, decode a token, compare values, and refuse or redirect a request. All of that
translates to a few lines of njs.

!!! tip "Only Lua needs this"
    Snippets with plain NGINX directives, such as `proxy_set_header` or `add_header`, are not Lua. They
    work in wxingress once snippets are enabled (`controller.enableSnippets=true`). Directives of modules
    that ingress-nginx bundles but nginx.org's NGINX does not, such as headers-more's `more_set_headers`,
    need an equivalent in plain NGINX (`add_header ... always`, `proxy_hide_header`).

## How njs is wired in

njs needs three things in the NGINX configuration:

1. **The module**, loaded at the top of `nginx.conf`.
2. **The script**, imported in the `http` block with `js_import`.
3. **A directive that calls the script** where the request is handled: `js_set` (computes a variable),
   `js_content` (produces the response), `js_header_filter` / `js_body_filter` (change the response), or
   `auth_request` to a location with `js_content`.

In wxingress, the first two go into the ConfigMap. ConfigMap snippets work even with snippet annotations
disabled. The script file comes from a ConfigMap mounted into the controller pods:

```yaml title="values.yaml"
controller:
  config:
    entries:
      # 1. the module (the data plane module replaces njs by default, so load it here)
      main-snippets: load_module /usr/lib/nginx/modules/ngx_http_js_module.so;
      # 2. the script, and the variables it computes
      http-snippets: |
        js_import checks from /etc/nginx/njs/checks.js;
        js_set $token_ip_mismatch checks.tokenIPMismatch;
      # 3. where it acts: here, every server
      server-snippets: |
        if ($token_ip_mismatch = 1) {
          return 403;
        }
  volumes:
    - name: njs
      configMap:
        name: wxingress-njs
  volumeMounts:
    - name: njs
      mountPath: /etc/nginx/njs
      readOnly: true
```

```sh
kubectl -n wxingress create configmap wxingress-njs --from-file=checks.js
```

Instead of `server-snippets` in the ConfigMap, which apply to every server, you can attach step 3 to
single Ingresses with `nginx.org/location-snippets` or `nginx.org/server-snippets`. That requires
`controller.enableSnippets=true`.

!!! tip "Switching without a gap"
    `nginx.org/location-snippets` wins over `nginx.ingress.kubernetes.io/configuration-snippet` in
    wxingress, and ingress-nginx ignores `nginx.org/*` annotations. An Ingress can carry both during the
    switch: the Lua version for ingress-nginx and the njs version for wxingress.

## Lua to njs, call by call

| Lua (OpenResty) | njs |
|---|---|
| `ngx.var.remote_addr`, `ngx.var.host` | `r.variables.remote_addr`, `r.variables.host` |
| `ngx.var.http_x_my_header` | `r.headersIn['X-My-Header']` |
| `ngx.req.get_headers()` | `r.headersIn` |
| `ngx.req.get_uri_args()` | `r.args` |
| `ngx.req.get_method()` | `r.method` |
| `ngx.req.read_body()` + `ngx.req.get_body_data()` | `r.requestText` (in `js_content`) |
| `ngx.exit(ngx.HTTP_FORBIDDEN)` | return a flag from `js_set` and `return 403` in NGINX, or `r.return(403)` in `js_content` |
| `ngx.redirect(url)` | `r.return(302, url)` in `js_content`, or `return 302 $var;` with a `js_set` variable |
| `ngx.header['X-Foo'] = 'bar'` | `r.headersOut['X-Foo'] = 'bar'` (in `js_content` or `js_header_filter`) |
| `ngx.say(body)` | `r.return(200, body)` |
| `ngx.log(ngx.ERR, msg)` | `r.error(msg)` (also `r.warn`, `r.log`) |
| `ngx.decode_base64(s)` | `Buffer.from(s, 'base64').toString()` |
| `ngx.encode_base64(s)` | `Buffer.from(s).toString('base64')` |
| `require "cjson"`, `cjson.decode(s)` | `JSON.parse(s)` |
| `cjson.encode(t)` | `JSON.stringify(o)` |
| `ngx.md5`, `ngx.sha1_bin`, `ngx.hmac_sha1` | `require('crypto')`: `crypto.createHash('md5')`, `crypto.createHmac('sha1', key)` |
| `ngx.re.match(s, re)` | `s.match(/re/)` |
| `ngx.shared.DICT` | `js_shared_dict_zone` and `ngx.shared.<zone>` |
| `ngx.location.capture(uri)` | `r.subrequest(uri)` (async, in `js_content`) |
| `ngx.now()` | `Date.now() / 1000` |
| `string.find`, `string.gsub` | `String.prototype.indexOf`, `replace` |

### Phases

Lua has a phase for everything. njs does not, and choosing the right njs hook is most of the work:

| Lua | njs equivalent |
|---|---|
| `rewrite_by_lua_block`, `access_by_lua_block` that refuses or redirects | `js_set` computes a variable, and an NGINX `if` / `return` acts on it. This runs in the rewrite phase, before `auth_request`. |
| `access_by_lua_block` that must run **after** `auth-url` (external auth) | `auth_request` to an internal location with `js_content`, or a check on what auth returned with `auth_request_set` plus a `js_set` |
| `access_by_lua_block` that calls another service | `auth_request` to a location with `js_content` that uses `r.subrequest()` or `ngx.fetch()` |
| `content_by_lua_block` | `js_content` |
| `header_filter_by_lua_block` | `js_header_filter` |
| `body_filter_by_lua_block` | `js_body_filter` |
| `log_by_lua_block` | usually not needed: use a `log_format` with variables computed by `js_set` |
| `balancer_by_lua_block` | not needed: wxingress's data plane module does the balancing (`load-balance`, `upstream-hash-by`, affinity) |

## A complete example: a token bound to the client's address

An Ingress protected by oauth2-proxy (`auth-url`, `auth-signin`) had this Lua snippet. It refuses a
request whose access token carries a `client_ip` claim that is not the client's address, so a token stolen
from one machine cannot be used from another:

```yaml title="before: ingress-nginx"
metadata:
  annotations:
    nginx.ingress.kubernetes.io/auth-url: https://$host/oauth2/auth
    nginx.ingress.kubernetes.io/auth-signin: https://$host/oauth2/start?rd=$escaped_request_uri
    nginx.ingress.kubernetes.io/configuration-snippet: |
      access_by_lua_block {
        local cjson = require "cjson"
        local auth_header = ngx.var.http_x_auth_request_access_token
        if not auth_header or auth_header == "" then
          return
        end
        local _, _, payload_b64 = string.find(auth_header, "%.([^%.]+)%.")
        if not payload_b64 then return end
        local payload_raw = payload_b64:gsub("-", "+"):gsub("_", "/")
        local padding = #payload_raw % 4
        if padding > 0 then payload_raw = payload_raw .. string.rep("=", 4 - padding) end
        local decoded = ngx.decode_base64(payload_raw)
        if decoded then
          local data = cjson.decode(decoded)
          local token_ip = data.client_ip
          if token_ip and token_ip ~= ngx.var.remote_addr then
            ngx.log(ngx.ERR, "IP Mismatch! Token IP: " .. token_ip .. " | Client IP: " .. ngx.var.remote_addr)
            ngx.exit(ngx.HTTP_FORBIDDEN)
          end
        end
      }
```

The njs version computes "does this request have to be refused?" as a variable, and the NGINX
configuration refuses it:

```js title="checks.js"
// Hosts the check applies to. With the check in ConfigMap snippets, it runs
// for every server, so it has to know where it is meant to act.
var HOSTS = { "app.example.com": true };

function payload(token) {
    var parts = token.split(".");
    if (parts.length < 3 || !parts[1]) {
        return null;
    }
    // base64url to base64, with padding
    var b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    while (b64.length % 4) {
        b64 += "=";
    }
    return JSON.parse(Buffer.from(b64, "base64").toString());
}

// js_set $token_ip_mismatch checks.tokenIPMismatch: "1" refuses the request
function tokenIPMismatch(r) {
    if (!HOSTS[r.variables.host]) {
        return "0";
    }
    var token = r.headersIn["X-Auth-Request-Access-Token"];
    if (!token) {
        return "0";
    }
    var data;
    try {
        data = payload(token);
    } catch (e) {
        // Lua failed with a 500 here; refusing is the safe equivalent
        r.error("the access token cannot be read: " + e);
        return "1";
    }
    if (!data) {
        return "0";
    }
    var tokenIP = data.client_ip;
    var clientIP = r.variables.remote_addr;
    if (tokenIP && tokenIP !== clientIP) {
        r.error("IP Mismatch! Token IP: " + tokenIP + " | Client IP: " + clientIP);
        return "1";
    }
    return "0";
}

export default { tokenIPMismatch };
```

With the values from [How njs is wired in](#how-njs-is-wired-in), this gives:

| Request | Answer |
|---|---|
| no token | as before: the login redirect of `auth-signin` |
| a token with another address in `client_ip` | 403, and `IP Mismatch! …` in the error log |
| a token with the client's address | as before: on to oauth2-proxy and the backend |
| a token whose payload is not JSON | 403 |
| any other host | unaffected |

One difference in order: the Lua block ran in the access phase, after `auth_request`. The `js_set` check
runs in the rewrite phase, before it. A request with a mismatching token is refused before oauth2-proxy
sees it; for every other request nothing changes.

!!! warning "The client's address"
    `remote_addr` is the client's address only if NGINX knows it. Behind a load balancer that speaks the
    PROXY protocol, set `use-proxy-protocol: "true"` and `proxy-real-ip-cidr` (or `set-real-ip-from`) in
    the ConfigMap. Behind an HTTP proxy, set `enable-real-ip`, `forwarded-for-header` and
    `proxy-real-ip-cidr`. Otherwise every request seems to come from the load balancer.

Finally, remove the `configuration-snippet` from the Ingress. While snippets are disabled, wxingress
ignores it with a Warning event, but its admission webhook refuses any update to an Ingress that still
carries it.

## Testing njs

Test the script before it reaches the cluster, with the `njs` command line or in a container of NGINX's
official image, which has the njs module (the wxingress image has no shell to run this in):

```sh
docker run --rm -v "$PWD:/njs" nginx:1.31.10 sh -c '
  cat > /tmp/nginx.conf <<EOF
load_module /usr/lib/nginx/modules/ngx_http_js_module.so;
events {}
http {
  js_import checks from /njs/checks.js;
  js_set \$token_ip_mismatch checks.tokenIPMismatch;
  server { listen 8080; location / { if (\$token_ip_mismatch = 1) { return 403; } return 200 ok; } }
}
EOF
  nginx -t -c /tmp/nginx.conf'
```

`nginx -t` loads the script, so syntax errors show up there. For behaviour, run the same configuration
with `nginx -g "daemon off;"` and send requests with `curl -H "Host: app.example.com" -H
"X-Auth-Request-Access-Token: …"`.

## Need help?

If you have Lua that does not map well, open a
[discussion](https://github.com/wx-one/wxingress/discussions). WizardTales also offers
[commercial support](../about/support.md) for migrations.
