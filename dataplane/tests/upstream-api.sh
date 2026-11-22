#!/bin/sh
#
# The upstream API, asked the way the controller asks it.
#
# Starts the nginx build.sh made with a configuration in the shape the
# controller's templates write in -dynamic-upstreams mode - the module
# loaded, the API on the socket NGINX Plus has its own on, the
# config-version map, and an upstream as the OSS ingress template writes it,
# placeholder server included - and runs tests/upstreamapi, which drives it
# with nginx-plus-go-client and proxies through it in between.
#
#   NGINX=build/nginx/sbin/nginx MODULE=build/ngx_http_wxingress_module.so \
#       tests/upstream-api.sh
#
#   PORT=8299     the first of the nine ports it uses
#   KEEP=1        leave the prefix behind to look at

set -u

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository=$(CDPATH= cd -- "$here/../.." && pwd)
nginx=${NGINX:-$here/../build/nginx/sbin/nginx}
# NGINX in the environment is how nginx hands listening sockets to a new
# binary: an nginx that finds it takes itself for one, does not daemonize and
# logs an emerg about a socket number - the test would hang on it forever.
unset NGINX
module=${MODULE:-$here/../build/ngx_http_wxingress_module.so}
port=${PORT:-8299}

say() { printf '%s\n' "$*"; }

[ -x "$nginx" ] || { say "no nginx at $nginx - run build.sh first"; exit 2; }
[ -f "$module" ] || { say "no module at $module - run build.sh first"; exit 2; }

work=$(mktemp -d)
# nginx's workers run as nobody when the tests run as root (CI), and read
# files under it
chmod 755 "$work"
prefix="$work/run"
socket="$work/nginx-plus-api.sock"
front=$port
first=$((port + 1))
second=$((port + 2))
canary=$((port + 3))
third=$((port + 4))
slow=$((port + 5))
# nothing listens here: a try that goes to it fails at once
dead=$((port + 6))
balancing=$((port + 7))
# the TCP proxy of the stream block, in front of a TransportServer's upstream
tcp=$((port + 8))
upstream="default-cafe-ingress-cafe.example.com-coffee-svc-80"

stop() {
  if [ -f "$prefix/logs/nginx.pid" ]; then
    kill "$(cat "$prefix/logs/nginx.pid" 2>/dev/null)" 2>/dev/null
    sleep 1
  fi

  [ -n "${KEEP:-}" ] || rm -rf "$work"
}

trap stop EXIT

mkdir -p "$prefix/conf" "$prefix/logs" "$prefix/modules" "$prefix/html"
# what the slow server sends, slowly: a line that says who, and 1.5k after it
{ echo "slow"; head -c 1500 /dev/zero | tr '\0' ' '; echo; } > "$prefix/html/slow"
cp "$module" "$prefix/modules/ngx_http_wxingress_module.so"

# the controller's token and its check, as internal/nginx/apitoken.go writes them
token=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
printf 'if ($http_x_wxingress_api_token != "%s") {\n    return 403;\n}\n' "$token" > "$prefix/conf/api-token.conf"

# internal/nginx/verify.go writes this; version 1 is the first one
cat > "$prefix/conf/config-version.conf" <<CONF
server {
    listen unix:$work/nginx-config-version.sock;
    access_log off;

    location /configVersion {
        return 200 1;
    }
}
map \$http_x_expected_config_version \$config_version_mismatch {
    "1" "";
    default "mismatch";
}
CONF

# the parts of version1/nginx.tmpl, nginx.ingress.tmpl and
# version2/nginx.transportserver.tmpl this mode adds or uses
cat > "$prefix/conf/nginx.conf" <<CONF
load_module modules/ngx_http_wxingress_module.so;

daemon on;
worker_processes 2;
error_log logs/error.log info;
pid logs/nginx.pid;

events { worker_connections 128; }

http {
    access_log off;

    # the upstream API of the wxingress module, on the socket NGINX Plus has its own on
    server {
        listen unix:$socket;
        access_log off;

        # only the controller: a request without its token is refused here
        include $prefix/conf/api-token.conf;

        location /configVersionCheck {
            if (\$config_version_mismatch) {
                return 503;
            }
            return 200;
        }

        location /api {
            meta_pass;
        }
    }

    include $prefix/conf/config-version.conf;

    # configuration for default/cafe-ingress
    upstream $upstream {
        zone $upstream 256k;
        random two least_conn;
        server 127.0.0.1:8181 max_fails=1 fail_timeout=10s max_conns=0;
    }

    server {
        listen 127.0.0.1:$front;
        location / {
            proxy_pass http://$upstream;
            proxy_next_upstream off;
        }
    }

    # a primary and its canary, as the controller writes them for a canary Ingress
    upstream default-echo-80        { zone default-echo-80 64k;        server 127.0.0.1:$first; }
    upstream default-echo-canary-80 { zone default-echo-canary-80 64k; server 127.0.0.1:$second; }
    upstream default-empty-80       { zone default-empty-80 64k;       server 127.0.0.1:$first; }
    upstream default-gone-80        { zone default-gone-80 64k;        server 127.0.0.1:$second; }
    upstream default-many-80        { zone default-many-80 4m;         server 127.0.0.1:$first; }

    # a Gateway route's upstreams have capitals, which nginx lowers in a variable's name
    upstream vs_ns_GW-1_be-2 { zone vs_ns_GW-1_be-2 64k; server 127.0.0.1:$first; }
    upstream vs_ns_GW-1_be-3 { zone vs_ns_GW-1_be-3 64k; server 127.0.0.1:$second; }

    # sticky on both sides, as ingress-nginx copies the primary's affinity into its canary
    upstream default-pin-80        { zone default-pin-80 64k;        server 127.0.0.1:$first;  sticky cookie pin; }
    upstream default-pin-canary-80 { zone default-pin-canary-80 64k; server 127.0.0.1:$second; sticky cookie pin; }

    server {
        listen 127.0.0.1:$canary;
        location /echo  { proxy_pass http://\$meta_canary_default_decho_d80; }
        location /empty { proxy_pass http://\$meta_canary_default_dempty_d80; }
        location /pin   { proxy_pass http://\$meta_canary_default_dpin_d80; }
        location /gw    { proxy_pass http://\$meta_canary_vs__ns___ug_uw_d1__be_d2; }
    }

    # ingress-nginx's balancers: every upstream of an Ingress says meta_balancer,
    # after its method and before its keepalive, as nginx.ingress.tmpl writes it
    upstream default-lb-80 {
        zone default-lb-80 256k;
        random two least_conn;
        server 127.0.0.1:$first max_fails=1 fail_timeout=10s max_conns=0;
        server 127.0.0.1:$second max_fails=1 fail_timeout=10s max_conns=0;
        server 127.0.0.1:$third max_fails=1 fail_timeout=10s max_conns=0;
        meta_balancer;
        keepalive 8;
    }
    upstream default-fail-80 {
        zone default-fail-80 64k;
        server 127.0.0.1:$dead max_fails=0;
        server 127.0.0.1:$first max_fails=0;
        meta_balancer;
    }
    upstream default-slow-80 {
        zone default-slow-80 64k;
        server 127.0.0.1:$first;
        server 127.0.0.1:$second;
        server 127.0.0.1:$slow;
        meta_balancer;
    }
    upstream default-stick-80        { zone default-stick-80 64k;        server 127.0.0.1:$first;  meta_balancer; }
    upstream default-stick-canary-80 { zone default-stick-canary-80 64k; server 127.0.0.1:$second; meta_balancer; }

    server {
        listen 127.0.0.1:$balancing;
        location /lb {
            proxy_pass http://default-lb-80;
            proxy_http_version 1.1;
            proxy_set_header Connection "";
        }
        location /fail  { proxy_pass http://default-fail-80; proxy_next_upstream error timeout; proxy_next_upstream_tries 3; }
        location /slow  { proxy_pass http://default-slow-80; }
        location /stick { proxy_pass http://\$meta_canary_default_dstick_d80; }
    }

    server { listen 127.0.0.1:$first;  location / { return 200 "first\n"; } }
    server { listen 127.0.0.1:$second; location / { return 200 "second\n"; } }
    server { listen 127.0.0.1:$third;  location / { return 200 "third\n"; } }
    server {
        listen 127.0.0.1:$slow;
        location / { root $prefix/html; try_files /slow =404; default_type text/plain; limit_rate 1k; limit_rate_after 0; }
    }

    # an http upstream named as the stream one below, which is another list
    upstream ts_default_tcp-app_app { zone http_ts_default_tcp-app_app 64k; server 127.0.0.1:$third; }
}

stream {
    # configuration for default/tcp-app, as nginx.transportserver.tmpl writes a
    # TransportServer upstream without endpoints
    upstream ts_default_tcp-app_app {
        zone ts_default_tcp-app_app 512k;

        server unix:/var/lib/nginx/non-existent.sock max_fails=3 fail_timeout=30s max_conns=0;
    }

    server {
        listen 127.0.0.1:$tcp;
        proxy_pass ts_default_tcp-app_app;
        proxy_connect_timeout 2s;
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

( cd "$repository" && go run ./dataplane/tests/upstreamapi \
    -socket "$socket" -token "$token" -front "http://127.0.0.1:$front/" -upstream "$upstream" \
    -first "127.0.0.1:$first" -second "127.0.0.1:$second" \
    -stream-front "http://127.0.0.1:$tcp/" -stream-upstream ts_default_tcp-app_app )
result=$?

# ----------------------------------------------------------------- canaries

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

api() { curl -s --max-time 5 -H "X-Wxingress-Api-Token: $token" --unix-socket "$socket" "$@"; }
code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "X-Wxingress-Api-Token: $token" --unix-socket "$socket" "$@"; }
bare() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$socket" "$@"; }

# ------------------------------------------------------------ the API token

check "without the controller's token the API refuses" \
      "$(bare http://nginx-plus-api/api/wxingress/1/canaries)" "403"
check "and with another token" \
      "$(bare -H "X-Wxingress-Api-Token: ${token}0" http://nginx-plus-api/api/wxingress/1/canaries)" "403"
check "and a change without it" \
      "$(bare -X PUT --data '{"canary":"x","weight":1}' http://nginx-plus-api/api/wxingress/1/canaries/default-echo-80)" "403"
check "with it the API answers" \
      "$(code http://nginx-plus-api/api/wxingress/1/canaries)" "200"
rule() { code -X PUT --data "$1" "http://nginx-plus-api/api/wxingress/1/canaries/default-echo-80"; }

# who answers `count` requests, as "first:N second:M"
tally() {
  count=$1
  shift
  for _ in $(seq 1 "$count"); do
    curl -s --max-time 5 "$@" "http://127.0.0.1:$canary/echo"
  done | sort | uniq -c | awk '{printf "%s%s:%s", (NR > 1 ? " " : ""), $2, $1}'
}

check "no rule, everything goes to the primary" "$(tally 10)" "first:10"

check "a rule is set" "$(rule '{"canary":"default-echo-canary-80","header":"X-Canary","cookie":"wxwaf_canary"}')" "200"
check "and listed" "$(api http://nginx-plus-api/api/wxingress/1/canaries | grep -c default-echo-canary-80)" "1"
check "the header says always" "$(tally 5 -H 'X-Canary: always')" "second:5"
check "the header says never" "$(tally 5 -H 'X-Canary: never' -b 'wxwaf_canary=always')" "first:5"
check "never stops the cookie from being asked" "$(tally 3 -H 'X-Canary: never' -b 'wxwaf_canary=always')" "first:3"
check "another header value falls through to the cookie" "$(tally 5 -H 'X-Canary: maybe' -b 'wxwaf_canary=always')" "second:5"
check "the cookie says always" "$(tally 5 -b 'a=b; wxwaf_canary=always')" "second:5"
check "the cookie says never" "$(tally 5 -b 'wxwaf_canary=never')" "first:5"
check "without either, weight 0 is the primary" "$(tally 10)" "first:10"

rule '{"canary":"default-echo-canary-80","weight":100,"cookie":"wxwaf_canary"}' >/dev/null
check "weight 100 is the canary, at once and without a reload" "$(tally 10)" "second:10"
check "and the cookie still says never" "$(tally 5 -b 'wxwaf_canary=never')" "first:5"

rule '{"canary":"default-echo-canary-80","weight":50}' >/dev/null
split=$(tally 200)
first_n=$(echo "$split" | sed -n 's/.*first:\([0-9]*\).*/\1/p')
check "weight 50 splits about evenly" "$([ "${first_n:-0}" -gt 60 ] && [ "${first_n:-0}" -lt 140 ] && echo yes || echo "$split")" "yes"

rule '{"canary":"default-echo-canary-80","weight":30,"weightTotal":30}' >/dev/null
check "weightTotal below 100 counts as 100, as in ingress-nginx" "$([ "$(tally 100 | sed -n 's/.*first:\([0-9]*\).*/\1/p')" -gt 40 ] && echo yes || echo no)" "yes"

rule '{"canary":"default-echo-canary-80","header":"X-Canary","headerValue":"wxwaf"}' >/dev/null
check "a header value is the canary" "$(tally 3 -H 'X-Canary: wxwaf')" "second:3"
check "and always means nothing then" "$(tally 3 -H 'X-Canary: always')" "first:3"

rule '{"canary":"default-echo-canary-80","header":"X-Canary","headerPattern":"^(beta|test)[0-9]+$"}' >/dev/null
check "a header pattern is the canary" "$(tally 3 -H 'X-Canary: beta7')" "second:3"
check "and what it does not match is not" "$(tally 3 -H 'X-Canary: gamma7')" "first:3"
check "a pattern that is no pattern is refused" \
      "$(rule '{"canary":"default-echo-canary-80","header":"X","headerPattern":"(unclosed"}')" "400"

check "a rule for an upstream that is its own canary is refused" \
      "$(rule '{"canary":"default-echo-80"}')" "400"
check "a rule without a canary is refused" "$(rule '{"weight":10}')" "400"
check "an unknown parameter is refused" "$(rule '{"canary":"default-echo-canary-80","sticky":true}')" "400"

# a canary with no servers is not chosen, as one without endpoints is not in ingress-nginx
code -X PUT --data '{"canary":"default-gone-80","weight":100}' \
    "http://nginx-plus-api/api/wxingress/1/canaries/default-empty-80" >/dev/null
gone=$(api http://nginx-plus-api/api/9/http/upstreams/default-gone-80/servers | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
code -X DELETE "http://nginx-plus-api/api/9/http/upstreams/default-gone-80/servers/$gone" >/dev/null
check "a canary with no servers is passed over" \
      "$(curl -s --max-time 5 "http://127.0.0.1:$canary/empty")" "first"

# a session pinned to one side stays there, before header, cookie and weight
code -X PUT --data '{"canary":"default-pin-canary-80","header":"X-Canary","weight":50,"affinityCookie":"pin"}' \
    "http://nginx-plus-api/api/wxingress/1/canaries/default-pin-80" >/dev/null
sid() { api "http://nginx-plus-api/api/9/http/upstreams/$1/servers" >/dev/null; \
        curl -s -D - -o /dev/null --max-time 5 -H "X-Canary: $2" "http://127.0.0.1:$canary/pin" |
        sed -n 's/^[Ss]et-[Cc]ookie: pin=\([0-9a-f]*\).*/\1/p'; }
on_primary=$(sid default-pin-80 never)
on_canary=$(sid default-pin-canary-80 always)
pinned_tally() {
  for _ in $(seq 1 "$1"); do curl -s --max-time 5 -b "pin=$2" -H "X-Canary: $3" "http://127.0.0.1:$canary/pin"; done |
    sort | uniq -c | awk '{printf "%s%s:%s", (NR > 1 ? " " : ""), $2, $1}'
}
check "pinned to the primary, always is not enough to move it" "$(pinned_tally 5 "$on_primary" always)" "first:5"
check "pinned to the canary, never is not enough to move it" "$(pinned_tally 5 "$on_canary" never)" "second:5"
check "a cookie that pins nothing is the rules as usual" "$(pinned_tally 3 0000 always)" "second:3"

check "the rule is taken away" "$(code -X DELETE http://nginx-plus-api/api/wxingress/1/canaries/default-echo-80)" "200"
check "and everything goes to the primary again" "$(tally 10 -H 'X-Canary: always')" "first:10"
check "a rule that is gone is not found" "$(code -X DELETE http://nginx-plus-api/api/wxingress/1/canaries/default-echo-80)" "404"

code -X PUT --data '{"canary":"vs_ns_GW-1_be-3","weight":100}' \
    "http://nginx-plus-api/api/wxingress/1/canaries/vs_ns_GW-1_be-2" >/dev/null
check "a primary with capitals, _u in the variable, has its canary" \
      "$(curl -s --max-time 5 "http://127.0.0.1:$canary/gw")" "second"

# ---------------------------------------------------------------- balancers

lb() { code -X PUT --data "$2" "http://nginx-plus-api/api/wxingress/1/balancers/$1"; }
reference() { ( cd "$repository" && go run ./dataplane/tests/restychash "$@" ); }
servers="127.0.0.1:$first 127.0.0.1:$second 127.0.0.1:$third"
sorted=$(printf '%s\n' $servers | sort | tr '\n' ' ')

# who answers `count` requests to a path of the balancing server, as "first:N second:M"
btally() {
  path=$1
  count=$2
  shift 2
  for _ in $(seq 1 "$count"); do
    curl -s --max-time 5 "$@" "http://127.0.0.1:$balancing$path" | head -1
  done | sort | uniq -c | awk '{printf "%s%s:%s", (NR > 1 ? " " : ""), $2, $1}'
}
# the name of the backend that answers an address
named() { case "$1" in *:$first) echo first ;; *:$second) echo second ;; *:$third) echo third ;; *:$dead) echo dead ;; esac; }
between() { [ "${1:-0}" -ge "$2" ] && [ "${1:-0}" -le "$3" ] && echo yes || echo "no: $1"; }
count_of() { echo "$1" | tr ' ' '\n' | sed -n "s/^$2:\([0-9]*\)$/\1/p"; }

first_spread=$(btally /lb 30)
check "without a rule the block's method balances ($first_spread)" \
      "$(echo "$first_spread" | tr ' ' '\n' | wc -l | tr -d ' ')" "3"

check "round_robin is set" "$(lb default-lb-80 '{"algorithm":"round_robin"}')" "200"
spread=$(btally /lb 30)
for who in first second third; do
  check "round_robin gives $who a third" "$(between "$(count_of "$spread" $who)" 8 12)" "yes"
done

check "chash is set, without a reload" "$(lb default-lb-80 '{"algorithm":"chash","hashBy":"$arg_k"}')" "200"
for key in alpha beta gamma delta epsilon; do
  wanted=$(named "$(reference find "$key" $sorted)")
  check "chash sends $key where resty.chash does" "$(btally "/lb?k=$key" 4)" "$wanted:4"
done

check "chashsubset is set" \
      "$(lb default-lb-80 '{"algorithm":"chashsubset","hashBy":"$arg_k","subsetSize":2}')" "200"
for key in alpha beta gamma; do
  set=$(reference subset 2 "$key" $sorted | while read -r a; do named "$a"; done | sort -u | tr '\n' ' ' | sed 's/ $//')
  got=$(btally "/lb?k=$key" 20 | tr ' ' '\n' | sed 's/:.*//' | sort -u | tr '\n' ' ' | sed 's/ $//')
  check "chashsubset keeps $key in its set of two" "$got" "$set"
done

# sticky_balanced: a new session gets ingress-nginx's cookie, and stays
check "sticky_balanced is set" "$(lb default-lb-80 '{"algorithm":"sticky_balanced","backend":"default-cafe-80","cookieName":"route","cookiePath":"/lb","cookieMaxAge":"60","cookieExpires":"60","cookieSameSite":"Lax","changeOnFailure":true}')" "200"
answer=$(curl -s -D - --max-time 5 "http://127.0.0.1:$balancing/lb" | tr -d '\r')
cookie=$(echo "$answer" | sed -n 's/^[Ss]et-[Cc]ookie: //p')
key=$(echo "$cookie" | sed -n 's/^route=\([^|]*\)|\([0-9a-f]*\);.*/\1/p')
backend=$(echo "$cookie" | sed -n 's/^route=\([^|]*\)|\([0-9a-f]*\);.*/\2/p')
check "the cookie names the backend as sticky.lua does" "$backend" "$(reference backendkey default-cafe-80)"
check "and has resty.cookie's attributes in its order" \
      "$(echo "$cookie" | sed 's/^route=[^;]*//; s/Expires=[^;]*GMT/Expires=T/')" \
      "; Expires=T; Max-Age=60; Path=/lb; HttpOnly; SameSite=Lax"
pinned=$(echo "$answer" | tail -1)
check "the session is on the server its key finds on the ring" \
      "$pinned" "$(named "$(reference find "$key" $sorted)")"
check "and stays there" "$(btally /lb 10 -b "route=$key|$backend")" "$pinned:10"
check "with no new cookie" \
      "$(curl -s -D - -o /dev/null --max-time 5 -b "route=$key|$backend" "http://127.0.0.1:$balancing/lb" | grep -ci '^set-cookie')" "0"
check "a TLS request or session-cookie-secure makes it Secure" \
      "$(lb default-lb-80 '{"algorithm":"sticky_balanced","backend":"default-cafe-80","cookieName":"route","cookieSecure":true}' >/dev/null;
         curl -s -D - -o /dev/null --max-time 5 "http://127.0.0.1:$balancing/lb" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: route=[^;]*; //p')" \
      "Path=/; Secure; HttpOnly"

# sticky_persistent: the key is md5(backend .. address)
check "sticky_persistent is set" "$(lb default-lb-80 '{"algorithm":"sticky_persistent","backend":"default-cafe-80","cookieName":"route"}')" "200"
answer=$(curl -s -D - --max-time 5 "http://127.0.0.1:$balancing/lb" | tr -d '\r')
key=$(echo "$answer" | sed -n 's/^[Ss]et-[Cc]ookie: route=\([^|]*\)|.*/\1/p')
pinned=$(echo "$answer" | tail -1)
case $pinned in first) address=127.0.0.1:$first ;; second) address=127.0.0.1:$second ;; *) address=127.0.0.1:$third ;; esac
check "the persistent key is nodemap.lua's" "$key" "$(reference persistent default-cafe-80 "$address")"
check "and the session stays" "$(btally /lb 10 -b "route=$key|x")" "$pinned:10"
for who in first second third; do
  case $who in first) a=$first ;; second) a=$second ;; third) a=$third ;; esac
  k=$(reference persistent default-cafe-80 "127.0.0.1:$a")
  check "a cookie ingress-nginx wrote for $who goes to $who" "$(btally /lb 3 -b "route=$k|whatever")" "$who:3"
done

# a server removed: balanced sessions move by the ring, persistent ones only if theirs went
ids=$(api "http://nginx-plus-api/api/9/http/upstreams/default-lb-80/servers")
third_id=$(echo "$ids" | tr '{' '\n' | grep "127.0.0.1:$third" | sed -n 's/^"id":\([0-9]*\).*/\1/p')
k=$(reference persistent default-cafe-80 "127.0.0.1:$first")
code -X DELETE "http://nginx-plus-api/api/9/http/upstreams/default-lb-80/servers/$third_id" >/dev/null
check "a session on a server that stays keeps it when another goes" "$(btally /lb 5 -b "route=$k|x")" "first:5"
k=$(reference persistent default-cafe-80 "127.0.0.1:$third")
check "one whose server went is given another, and a cookie for it" \
      "$(curl -s -D - -o /dev/null --max-time 5 -b "route=$k|x" "http://127.0.0.1:$balancing/lb" | grep -ci '^set-cookie: route=')" "1"
code -X POST --data "{\"server\":\"127.0.0.1:$third\"}" "http://nginx-plus-api/api/9/http/upstreams/default-lb-80/servers" >/dev/null

# change-on-failure: a session pinned to a server that fails
dead_key=$(reference persistent default-fail-80 "127.0.0.1:$dead")
lb default-fail-80 '{"algorithm":"sticky_persistent","backend":"default-fail-80","cookieName":"route","changeOnFailure":true}' >/dev/null
answer=$(curl -s -D - --max-time 5 -b "route=$dead_key|x" "http://127.0.0.1:$balancing/fail" | tr -d '\r')
check "with change-on-failure the retry goes to another server" "$(echo "$answer" | tail -1)" "first"
check "and the cookie moves the session there" \
      "$(echo "$answer" | sed -n 's/^[Ss]et-[Cc]ookie: route=\([^|]*\)|.*/\1/p')" \
      "$(reference persistent default-fail-80 "127.0.0.1:$first")"
lb default-fail-80 '{"algorithm":"sticky_persistent","backend":"default-fail-80","cookieName":"route","changeOnFailure":false}' >/dev/null
check "without it every try goes to the pinned server, and fails" \
      "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -b "route=$dead_key|x" "http://127.0.0.1:$balancing/fail")" "502"

# ewma: a slow server is passed over, and its average is shared and kept
check "ewma is set" "$(lb default-slow-80 '{"algorithm":"ewma"}')" "200"
spread=$(btally /slow 40)
check "ewma sends little to the slow server ($spread)" "$(between "$(count_of "$spread" slow)" 1 4)" "yes"
ewma_of() { api http://nginx-plus-api/api/wxingress/1/ewma | tr '{' '\n' | grep "\"$1\"" | sed -n 's/.*"ewma":\([0-9.]*\),"touched":\([0-9.]*\).*/\1 \2/p'; }
slow_ewma=$(ewma_of "127.0.0.1:$slow")
check "the slow server's average is that of a slow server" \
      "$(echo "$slow_ewma" | awk '{print ($1 > 0.2) ? "slow" : $1}')" "slow"

# a server added to an ewma upstream starts at the average of the others
code -X POST --data "{\"server\":\"127.0.0.1:$canary\"}" "http://nginx-plus-api/api/9/http/upstreams/default-slow-80/servers" >/dev/null
check "a server added starts at the others' average, not at nothing" \
      "$(ewma_of "127.0.0.1:$canary" | awk '{print ($1 > 0) ? "averaged" : "none"}')" "averaged"
added=$(api "http://nginx-plus-api/api/9/http/upstreams/default-slow-80/servers" | tr '{' '\n' | grep "127.0.0.1:$canary" | sed -n 's/^"id":\([0-9]*\).*/\1/p')
code -X DELETE "http://nginx-plus-api/api/9/http/upstreams/default-slow-80/servers/$added" >/dev/null
check "and one removed loses its average" "$(ewma_of "127.0.0.1:$canary")" ""

# the canary: a session pinned to either side by the balancer's cookie stays there
lb default-stick-80 '{"algorithm":"sticky_persistent","backend":"default-stick-80","cookieName":"route"}' >/dev/null
lb default-stick-canary-80 '{"algorithm":"sticky_persistent","backend":"default-stick-canary-80","cookieName":"route"}' >/dev/null
code -X PUT --data '{"canary":"default-stick-canary-80","header":"X-Canary","weight":50}' \
    "http://nginx-plus-api/api/wxingress/1/canaries/default-stick-80" >/dev/null
on_canary=$(curl -s -D - -o /dev/null --max-time 5 -H 'X-Canary: always' "http://127.0.0.1:$balancing/stick" |
            tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: route=\([^;]*\);.*/\1/p')
on_primary=$(curl -s -D - -o /dev/null --max-time 5 -H 'X-Canary: never' "http://127.0.0.1:$balancing/stick" |
             tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: route=\([^;]*\);.*/\1/p')
check "pinned to the canary by its backend, never does not move it" \
      "$(btally /stick 5 -b "route=$on_canary" -H 'X-Canary: never')" "second:5"
check "pinned to the primary, always does not move it" \
      "$(btally /stick 5 -b "route=$on_primary" -H 'X-Canary: always')" "first:5"

check "a rule is listed" "$(api http://nginx-plus-api/api/wxingress/1/balancers | grep -c '"algorithm":"ewma"')" "1"
check "an unknown algorithm is refused" "$(lb default-lb-80 '{"algorithm":"least_conn"}')" "400"
check "chash without a key is refused" "$(lb default-lb-80 '{"algorithm":"chash"}')" "400"
check "the rule is taken away" "$(code -X DELETE http://nginx-plus-api/api/wxingress/1/balancers/default-lb-80)" "200"
last_spread=$(btally /lb 30 -b "route=$on_primary")
check "and the block's method balances again ($last_spread)" \
      "$(echo "$last_spread" | tr ' ' '\n' | wc -l | tr -d ' ')" "3"

# ------------------------------------------------- the longest listing
#
# 1024 servers - MOST_SERVERS - at the longest address and numbers the API
# writes, some 230 bytes each: the answer's buffer held 160 a server, and the
# listing ran past its end into whatever lay behind it.

added=0
for i in $(seq 1 1023); do
  h=$(printf '%04x' "$i")
  c=$(code -X POST --data "{\"server\":\"[fd00:1234:5678:9abc:def0:1234:5678:$h]:65535\",\"weight\":1000000,\"max_conns\":2000000000,\"max_fails\":2000000000,\"fail_timeout\":\"3600s\"}" \
      http://nginx-plus-api/api/9/http/upstreams/default-many-80/servers)
  [ "$c" = 201 ] && added=$((added + 1))
done
check "1023 servers more, at the longest" "$added" "1023"
listing=$(api http://nginx-plus-api/api/9/http/upstreams/default-many-80/servers)
check "all 1024 are listed" "$(printf '%s' "$listing" | grep -o '"id":' | wc -l | tr -d ' ')" "1024"
check "and the listing is whole" "$(printf '%s' "$listing" | tail -c 2)" "}]"
check "the API still answers" "$(code http://nginx-plus-api/api/9/http/upstreams/default-many-80/servers)" "200"

# a reload is the controller's, and the rules outlive it
rule '{"canary":"default-echo-canary-80","weight":100}' >/dev/null
kill -HUP "$(cat "$prefix/logs/nginx.pid")"
sleep 2
check "a rule outlives a reload" "$(tally 5)" "second:5"
check "the ewma averages outlive it too" "$(ewma_of "127.0.0.1:$slow")" "$slow_ewma"
check "and a balancer rule" "$(btally /slow 10 | grep -c slow)" "0"

[ "$failed" -gt 0 ] && result=1

# a connection to the socket that stands for no endpoints is a crit of nginx's
# own, which the stream part asks for on purpose
alarms=$(grep -E '\[(alert|emerg|crit)\]' "$prefix/logs/error.log" | grep -vc 'non-existent.sock')

if [ "$alarms" != "0" ]; then
  say "FAIL     the log has alerts"
  grep -E '\[(alert|emerg|crit)\]' "$prefix/logs/error.log" | grep -v 'non-existent.sock' | head -5
  result=1
fi

[ "$result" = 0 ] && say "all passed"

exit "$result"
