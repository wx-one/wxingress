#!/bin/sh
#
# OpenID Connect, the relying party of the data plane in nginx's access phase.
#
# Starts the nginx build.sh made with a server in the shape the controller
# writes for an oidc Policy without NGINX Plus, and runs tests/oidc, which is
# the identity provider and the browser.
#
#   PORT=18420    the first of the eight ports it uses (below the ephemeral range,
#                 where an outgoing connection of an earlier test can hold one)
#   KEEP=1        leave the prefix behind to look at

set -u

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository=$(CDPATH= cd -- "$here/../.." && pwd)
nginx=${NGINX:-$here/../build/nginx/sbin/nginx}
unset NGINX
module=${MODULE:-$here/../build/ngx_http_wxingress_module.so}
port=${PORT:-18420}

say() { printf '%s\n' "$*"; }

[ -x "$nginx" ] || { say "no nginx at $nginx - run build.sh first"; exit 2; }
[ -f "$module" ] || { say "no module at $module - run build.sh first"; exit 2; }

work=$(mktemp -d)
# nginx's workers run as nobody when the tests run as root (CI), and read
# files under it
chmod 755 "$work"
prefix="$work/run"
front=$port
backend=$((port + 1))
idp=$((port + 2))
public=$((port + 3))
idp2=$((port + 4))
discovered=$((port + 5))
idp3=$((port + 6))
other=$((port + 7))
api="$work/api.sock"

stop() {
  if [ -f "$prefix/logs/nginx.pid" ]; then
    kill "$(cat "$prefix/logs/nginx.pid" 2>/dev/null)" 2>/dev/null
    sleep 1
  fi
  [ -n "${KEEP:-}" ] || rm -rf "$work"
}

trap stop EXIT

mkdir -p "$prefix/conf" "$prefix/logs" "$prefix/modules"
cp "$module" "$prefix/modules/ngx_http_wxingress_module.so"

head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$prefix/session.key"

# four workers: the session is a cookie, any of them opens it
cat > "$prefix/conf/nginx.conf" <<CONF
load_module modules/ngx_http_wxingress_module.so;

daemon on;
worker_processes 4;
error_log logs/error.log info;
pid logs/nginx.pid;

events { worker_connections 128; }

http {
    access_log off;

    # as nginx.tmpl writes them
    map "" \$wxingress_jwt_realm { default ""; }
    map "" \$wxingress_jwt_keys { default ""; }
    map "" \$wxingress_jwt_jwks { default ""; }
    map "" \$wxingress_jwt_cache { default ""; }
    map "" \$wxingress_jwt_ca { default ""; }
    map "" \$wxingress_jwt_token_from { default ""; }
    map "" \$wxingress_jwt_leeway { default ""; }
    map "" \$wxingress_oidc_client { default ""; }
    map "" \$wxingress_oidc_client_secret { default ""; }
    map "" \$wxingress_oidc_session_key_file { default ""; }
    map "" \$wxingress_oidc_auth { default ""; }
    map "" \$wxingress_oidc_token { default ""; }
    map "" \$wxingress_oidc_jwks { default ""; }
    map "" \$wxingress_oidc_end_session { default ""; }
    map "" \$wxingress_oidc_scope { default ""; }
    map "" \$wxingress_oidc_redirect { default ""; }
    map "" \$wxingress_oidc_post_logout { default ""; }
    map "" \$wxingress_oidc_extra { default ""; }
    map "" \$wxingress_oidc_pkce { default ""; }
    map "" \$wxingress_oidc_ca { default ""; }
    map "" \$wxingress_oidc_leeway { default ""; }
    map "" \$wxingress_oidc_on { default ""; }
    map "" \$wxingress_oidc_issuer { default ""; }
    map "" \$wxingress_oidc_verify { default ""; }
    map "" \$wxingress_oidc_sni { default ""; }
    map "" \$wxingress_oidc_policy { default ""; }

    server {
        listen 127.0.0.1:$backend;
        location / { return 200 "ok \$request_uri\n"; }
    }

    server {
        listen 127.0.0.1:$front;

        # as the controller writes an oidc Policy
        set \$wxingress_oidc_client "e2e-client";
        set \$wxingress_oidc_client_secret "e2e secret: with & and =";
        set \$wxingress_oidc_auth "http://127.0.0.1:$idp/authorize";
        set \$wxingress_oidc_token "http://127.0.0.1:$idp/token";
        set \$wxingress_oidc_jwks "http://127.0.0.1:$idp/jwks";
        set \$wxingress_oidc_end_session "http://127.0.0.1:$idp/end_session";
        set \$wxingress_oidc_scope "openid profile email";
        set \$wxingress_oidc_redirect "/_codexch";
        set \$wxingress_oidc_post_logout "/_logout";
        set \$wxingress_oidc_extra "prompt=login";
        set \$wxingress_oidc_pkce "1";

        location /app {
            set \$wxingress_oidc_on "1";
            add_header X-User \$meta_oidc_claim_sub;
            add_header X-Token \$meta_oidc_access_token;
            proxy_pass http://127.0.0.1:$backend;
        }

        location /open { proxy_pass http://127.0.0.1:$backend; }

        location = /_codexch { try_files /.wxingress-oidc =404; }
        location = /logout { try_files /.wxingress-oidc =404; }
        location = /_logout { try_files /.wxingress-oidc =404; }
    }

    # the module's API, where the controller sets the revocations
    server {
        listen unix:$api;
        location / { meta_pass; }
    }

    # only the issuer: the endpoints from its discovery document, iss checked
    server {
        listen 127.0.0.1:$discovered;

        set \$wxingress_oidc_client "e2e-client";
        set \$wxingress_oidc_client_secret "e2e secret: with & and =";
        set \$wxingress_oidc_issuer "http://127.0.0.1:$idp3";
        set \$wxingress_oidc_extra "prompt=login";
        set \$wxingress_oidc_pkce "1";

        location /app {
            set \$wxingress_oidc_on "1";
            add_header X-User \$meta_oidc_claim_sub;
            add_header X-Token \$meta_oidc_access_token;
            proxy_pass http://127.0.0.1:$backend;
        }

        location /open { proxy_pass http://127.0.0.1:$backend; }

        location = /_codexch { try_files /.wxingress-oidc =404; }
        location = /logout { try_files /.wxingress-oidc =404; }
        location = /_logout { try_files /.wxingress-oidc =404; }
        location = /_backchannel_logout { try_files /.wxingress-oidc =404; }
        location = /front_channel_logout { try_files /.wxingress-oidc =404; }
    }

    # a public client: PKCE, no secret, the sessions sealed with the key file
    server {
        listen 127.0.0.1:$public;

        set \$wxingress_oidc_policy "tenant-a/oidc";
        set \$wxingress_oidc_client "e2e-public";
        set \$wxingress_oidc_client_secret "";
        set \$wxingress_oidc_session_key_file "$prefix/session.key";
        set \$wxingress_oidc_auth "http://127.0.0.1:$idp2/authorize";
        set \$wxingress_oidc_token "http://127.0.0.1:$idp2/token";
        set \$wxingress_oidc_jwks "http://127.0.0.1:$idp2/jwks";
        set \$wxingress_oidc_end_session "http://127.0.0.1:$idp2/end_session";
        set \$wxingress_oidc_extra "prompt=login";
        set \$wxingress_oidc_pkce "1";

        location /app {
            set \$wxingress_oidc_on "1";
            add_header X-User \$meta_oidc_claim_sub;
            add_header X-Token \$meta_oidc_access_token;
            proxy_pass http://127.0.0.1:$backend;
        }

        location /open { proxy_pass http://127.0.0.1:$backend; }

        location = /_codexch { try_files /.wxingress-oidc =404; }
        location = /logout { try_files /.wxingress-oidc =404; }
        location = /_logout { try_files /.wxingress-oidc =404; }
    }

    # another tenant's Policy with the same client ID - which is no secret -
    # and the controller's one key file: its sessions are its own
    server {
        listen 127.0.0.1:$other;

        set \$wxingress_oidc_policy "tenant-b/oidc";
        set \$wxingress_oidc_client "e2e-public";
        set \$wxingress_oidc_client_secret "";
        set \$wxingress_oidc_session_key_file "$prefix/session.key";
        set \$wxingress_oidc_auth "http://127.0.0.1:$idp2/authorize";
        set \$wxingress_oidc_token "http://127.0.0.1:$idp2/token";
        set \$wxingress_oidc_jwks "http://127.0.0.1:$idp2/jwks";
        set \$wxingress_oidc_extra "prompt=login";
        set \$wxingress_oidc_pkce "1";

        location /app {
            set \$wxingress_oidc_on "1";
            add_header X-User \$meta_oidc_claim_sub;
            proxy_pass http://127.0.0.1:$backend;
        }

        location = /_codexch { try_files /.wxingress-oidc =404; }
    }
}
CONF

"$nginx" -p "$prefix" -c conf/nginx.conf || { say "nginx did not start"; exit 1; }
sleep 1

result=0
say "--- a confidential client"
( cd "$repository" && go run ./dataplane/tests/oidc -nginx "127.0.0.1:$front" -idp "127.0.0.1:$idp" ) || result=1
say "--- only an issuer: discovery, iss, logouts of the IdP, revocations"
( cd "$repository" && go run ./dataplane/tests/oidc -discovery -api "$api" -nginx "127.0.0.1:$discovered" -idp "127.0.0.1:$idp3" ) || result=1
say "--- a public client with PKCE"
( cd "$repository" && go run ./dataplane/tests/oidc -public -nginx "127.0.0.1:$public" -idp "127.0.0.1:$idp2" -other "127.0.0.1:$other" ) || result=1

alarms=$(grep -cE '\[(alert|emerg|crit)\]' "$prefix/logs/error.log")
if [ "$alarms" != "0" ]; then
  say "FAIL     the log has alerts"
  grep -E '\[(alert|emerg|crit)\]' "$prefix/logs/error.log" | head -5
  result=1
fi

[ "$result" = 0 ] && say "all passed"
exit "$result"
