#!/bin/sh
#
# JSON Web Tokens, checked by the data plane in nginx's access phase.
#
# Starts the nginx build.sh made with locations in the shape the controller
# writes for a jwt Policy without NGINX Plus - the realm, the key set from a
# file or a URL, where the token is - and runs tests/jwt, which makes the
# keys, serves a key set over HTTP, signs tokens with every algorithm and
# asks.
#
#   NGINX=build/nginx/sbin/nginx MODULE=build/ngx_http_wxingress_module.so \
#       tests/jwt.sh
#
#   PORT=18400    the first of the four ports it uses (below the ephemeral range,
#                 where an outgoing connection of an earlier test can hold one)
#   KEEP=1        leave the prefix behind to look at

set -u

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository=$(CDPATH= cd -- "$here/../.." && pwd)
nginx=${NGINX:-$here/../build/nginx/sbin/nginx}
unset NGINX
module=${MODULE:-$here/../build/ngx_http_wxingress_module.so}
port=${PORT:-18400}

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
jwks=$((port + 2))
tls=$((port + 3))

stop() {
  if [ -f "$prefix/logs/nginx.pid" ]; then
    kill "$(cat "$prefix/logs/nginx.pid" 2>/dev/null)" 2>/dev/null
    sleep 1
  fi
  [ -n "${KEEP:-}" ] || rm -rf "$work"
}

trap stop EXIT

mkdir -p "$prefix/conf" "$prefix/logs" "$prefix/modules" "$prefix/keys"
cp "$module" "$prefix/modules/ngx_http_wxingress_module.so"
echo '{"keys":[]}' > "$prefix/keys/keys.json"

# one worker: each worker keeps its own key sets, and the test counts fetches
cat > "$prefix/conf/nginx.conf" <<CONF
load_module modules/ngx_http_wxingress_module.so;

daemon on;
worker_processes 1;
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
    map "" \$wxingress_jwt_verify { default ""; }
    map "" \$wxingress_jwt_sni { default ""; }
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

    server {
        listen 127.0.0.1:$backend;
        location / { return 200 "ok\n"; }
    }

    server {
        listen 127.0.0.1:$front;

        # as the controller writes a jwt Policy of a route
        set \$wxingress_jwt_realm "file";
        set \$wxingress_jwt_keys "$prefix/keys/keys.json";

        add_header X-Sub \$meta_jwt_claim_sub;
        add_header X-Groups \$meta_jwt_claim_groups;
        add_header X-Alg \$meta_jwt_header_alg;

        location /file { proxy_pass http://127.0.0.1:$backend; }

        location /leeway {
            set \$wxingress_jwt_leeway 60;
            proxy_pass http://127.0.0.1:$backend;
        }

        location /url {
            set \$wxingress_jwt_realm "url";
            set \$wxingress_jwt_keys "";
            set \$wxingress_jwt_jwks "http://127.0.0.1:$jwks/jwks";
            set \$wxingress_jwt_cache "1h";
            proxy_pass http://127.0.0.1:$backend;
        }

        # a key set that is slow to come, and one that never does
        location /slow-url {
            set \$wxingress_jwt_realm "slow";
            set \$wxingress_jwt_keys "";
            set \$wxingress_jwt_jwks "http://127.0.0.1:$jwks/slow";
            proxy_pass http://127.0.0.1:$backend;
        }

        location /broken-url {
            set \$wxingress_jwt_realm "broken";
            set \$wxingress_jwt_keys "";
            set \$wxingress_jwt_jwks "http://127.0.0.1:$jwks/broken";
            proxy_pass http://127.0.0.1:$backend;
        }

        # one key set over TLS with a certificate of its own: verified unless
        # the Policy says 0, against the Policy's CA when it names one
        location /tls-off {
            set \$wxingress_jwt_realm "tls";
            set \$wxingress_jwt_keys "";
            set \$wxingress_jwt_jwks "https://127.0.0.1:$tls/jwks";
            set \$wxingress_jwt_verify "0";
            proxy_pass http://127.0.0.1:$backend;
        }

        location /tls-default {
            set \$wxingress_jwt_realm "tls";
            set \$wxingress_jwt_keys "";
            set \$wxingress_jwt_jwks "https://127.0.0.1:$tls/jwks";
            proxy_pass http://127.0.0.1:$backend;
        }

        location /tls-ca {
            set \$wxingress_jwt_realm "tls";
            set \$wxingress_jwt_keys "";
            set \$wxingress_jwt_jwks "https://127.0.0.1:$tls/jwks";
            set \$wxingress_jwt_verify "1";
            set \$wxingress_jwt_ca "$prefix/keys/ca.pem";
            proxy_pass http://127.0.0.1:$backend;
        }

        location /cookie {
            set \$wxingress_jwt_token_from cookie_auth;
            proxy_pass http://127.0.0.1:$backend;
        }

        location /arg {
            set \$wxingress_jwt_token_from arg_token;
            proxy_pass http://127.0.0.1:$backend;
        }

        location /open {
            set \$wxingress_jwt_realm "";
            proxy_pass http://127.0.0.1:$backend;
        }

        # requests nginx carries elsewhere inside itself, each of which sets
        # r->internal: the realm of where they land is asked all the same
        location /rewrite-break {
            rewrite ^/rewrite-break(.*)\$ /file\$1 break;
            proxy_pass http://127.0.0.1:$backend;
        }

        # the shape of a VirtualServer route with matches or splits: the
        # route's own location opens no realm, the internal one it rewrites
        # to does
        location /split {
            set \$wxingress_jwt_realm "";
            rewrite ^ /internal_split last;
        }

        location /internal_split {
            internal;
            set \$wxingress_jwt_realm "split";
            proxy_pass http://127.0.0.1:$backend;
        }

        location /tried {
            set \$wxingress_jwt_realm "";
            try_files /nowhere @protected;
        }

        location /tried-uri {
            set \$wxingress_jwt_realm "";
            try_files /nowhere /internal_split;
        }

        location /errored {
            set \$wxingress_jwt_realm "";
            error_page 418 = @protected;
            return 418;
        }

        location @protected {
            set \$wxingress_jwt_realm "named";
            proxy_pass http://127.0.0.1:$backend;
        }
    }
}
CONF

"$nginx" -p "$prefix" -c conf/nginx.conf || { say "nginx did not start"; exit 1; }
sleep 1

result=0
( cd "$repository" && go run ./dataplane/tests/jwt -nginx "127.0.0.1:$front" -jwks "127.0.0.1:$jwks" -tls "127.0.0.1:$tls" -keys "$prefix/keys" ) || result=1

alarms=$(grep -cE '\[(alert|emerg|crit)\]' "$prefix/logs/error.log")
if [ "$alarms" != "0" ]; then
  say "FAIL     the log has alerts"
  grep -E '\[(alert|emerg|crit)\]' "$prefix/logs/error.log" | head -5
  result=1
fi

[ "$result" = 0 ] && say "all passed"
exit "$result"
