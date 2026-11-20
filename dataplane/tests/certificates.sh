#!/bin/sh
#
# The certificate API, asked the way the controller asks it, and nginx asked
# for certificates by name the way a client asks.
#
# Starts the nginx build.sh made with TLS servers in the shape the
# controller's templates write them with -dynamic-upstreams - every one
# naming the default certificate, none a certificate of its own - and runs
# tests/certificates, which sets, changes and takes away certificates and
# looks at what each handshake is given. Then it checks that none of it
# reloaded nginx, and that a reload keeps the certificates.
#
#   NGINX=build/nginx/sbin/nginx MODULE=build/ngx_http_wxingress_module.so \
#       tests/certificates.sh
#
#   PORT=8399     the port it speaks TLS on, and the next one for the
#                 names bound to a port
#   MANY=1000     how many names the bulk part sets
#   KEEP=1        leave the prefix behind to look at

set -u

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository=$(CDPATH= cd -- "$here/../.." && pwd)
nginx=${NGINX:-$here/../build/nginx/sbin/nginx}
# NGINX in the environment is how nginx hands listening sockets to a new
# binary - see upstream-api.sh
unset NGINX
module=${MODULE:-$here/../build/ngx_http_wxingress_module.so}
port=${PORT:-8399}
bound=$((port + 1))

say() { printf '%s\n' "$*"; }

[ -x "$nginx" ] || { say "no nginx at $nginx - run build.sh first"; exit 2; }
[ -f "$module" ] || { say "no module at $module - run build.sh first"; exit 2; }

work=$(mktemp -d)
# nginx's workers run as nobody when the tests run as root (CI), and read
# files under it
chmod 755 "$work"
prefix="$work/run"
socket="$work/nginx-plus-api.sock"

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

# the default server's certificate, in one file as the controller writes it
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
    -subj "/CN=default" -keyout "$work/default.key" -out "$work/default.crt" \
    >/dev/null 2>&1
cat "$work/default.crt" "$work/default.key" > "$work/default"

cat > "$prefix/conf/nginx.conf" <<CONF
load_module modules/ngx_http_wxingress_module.so;

daemon on;
worker_processes 2;
error_log logs/error.log info;
pid logs/nginx.pid;

events { worker_connections 256; }

http {
    access_log off;

    server {
        listen unix:$socket;
        access_log off;

        location /api {
            meta_pass;
        }
    }

    server {
        listen 127.0.0.1:$port ssl default_server;
        server_name _;
        ssl_certificate $work/default;
        ssl_certificate_key $work/default;
        location / { return 404; }
    }

    server {
        listen 127.0.0.1:$port ssl;
        server_name a.test;
        ssl_certificate $work/default;
        ssl_certificate_key $work/default;
        location / { return 200 "a.test\n"; }
    }

    # the port of a Gateway's listeners: names bound to it come first
    server {
        listen 127.0.0.1:$bound ssl default_server;
        server_name _;
        ssl_certificate $work/default;
        ssl_certificate_key $work/default;
        location / { return 404; }
    }
}
CONF

"$nginx" -p "$prefix" -c "$prefix/conf/nginx.conf" >"$work/nginx.out" 2>&1 || {
  say "FAIL     nginx did not start"
  tail -5 "$work/nginx.out" "$prefix/logs/error.log" 2>/dev/null
  exit 1
}

waited=0
while [ "$waited" -lt 100 ] && [ ! -S "$socket" ]; do
  sleep 0.1
  waited=$((waited + 1))
done

failed=0

check() {
  if [ "$2" = "$3" ]; then
    say "ok       $1"
  else
    say "FAIL     $1"
    say "  wanted: $3"
    say "  got:    $2"
    failed=$((failed + 1))
  fi
}

master=$(cat "$prefix/logs/nginx.pid")
workers() { pgrep -P "$master" | sort | tr '\n' ' '; }
before=$(workers)

( cd "$repository" && go run ./dataplane/tests/certificates \
    -socket "$socket" -tls "127.0.0.1:$port" -many "${MANY:-1000}" )
result=$?

check "none of it was a reload: the same master and the same workers" \
      "$(cat "$prefix/logs/nginx.pid") $(workers)" "$master $before"

subject() {
  echo | openssl s_client -connect "127.0.0.1:$port" -servername "$1" \
      2>/dev/null </dev/null | openssl x509 -noout -subject 2>/dev/null |
      sed -n 's/.*CN *= *\([^,]*\).*/\1/p'
}

check "a name with a trailing dot is the name" "$(subject a.test.)" "a.test"

kill -HUP "$master"
sleep 1.5
check "a reload starts new workers" "$([ "$(workers)" != "$before" ] && echo yes)" "yes"
check "and they have the certificates of the old ones" "$(subject n5.many.test)" "n5.many.test"
check "wildcards included" "$(subject q.w.test)" "*.w.test"

# names bound to the port of a Gateway's listeners
put() {
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
    -subj "/CN=$2" -keyout "$work/put.key" -out "$work/put.crt" 2>/dev/null
  body=$(printf '{"certificate":"%s","key":"%s"}' "$(awk '{printf "%s\\n", $0}' "$work/put.crt")" \
    "$(awk '{printf "%s\\n", $0}' "$work/put.key")")
  curl -s -o /dev/null -w '%{http_code}' --unix-socket "$socket" -X PUT \
    -H 'Content-Type: application/json' --data "$body" "http://x/api/wxingress/1/certificates/$1"
}
on() {
  echo | openssl s_client -connect "127.0.0.1:$1" -servername "$2" \
      2>/dev/null </dev/null | openssl x509 -noout -subject 2>/dev/null |
      sed -n 's/.*CN *= *\([^,]*\).*/\1/p'
}
check "a name bound to a port is taken" "$(put "a.test:$bound" a-on-bound)" "200"
check "and the catch-all of the port" "$(put "_:$bound" all-on-bound)" "200"
check "and a wildcard bound to it" "$(put "*.w.test:$bound" w-on-bound)" "200"
check "a name with a bad port is not" "$(put "a.test:123456" bad)" "400"
check "the name on its port gets its own" "$(on "$bound" a.test)" "a-on-bound"
check "the wildcard on its port" "$(on "$bound" q.w.test)" "w-on-bound"
check "another name on the port gets the port's" "$(on "$bound" elsewhere.test)" "all-on-bound"
check "the name on the other port keeps the one it had" "$(on "$port" a.test)" "a.test"
check "the wildcard there too" "$(on "$port" q.w.test)" "*.w.test"

check "nothing went wrong in the log" \
      "$(grep -c -E '\[(alert|emerg|crit)\]' "$prefix/logs/error.log")" "0"

if [ "$result" -ne 0 ] || [ "$failed" -gt 0 ]; then
  say "failed"
  exit 1
fi

say "all passed"
