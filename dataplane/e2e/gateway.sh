#!/bin/sh
#
# The Gateway API on kind: a Gateway with HTTPRoutes - hostnames, a header
# match, weights, a missing backend - their status, and backends whose
# endpoints change: none of that may reload nginx with -dynamic-upstreams.
#
# Runs where kubectl and the kind node's docker are, against a release of
# charts/wxingress installed with -dynamic-upstreams, Prometheus metrics and
# the Gateway API:
#
#   helm install wxgw charts/wxingress -n wxgw --create-namespace \
#     --set controller.dynamicUpstreams=true \
#     --set controller.ingressClass.name=wxgw \
#     --set controller.service.type=ClusterIP \
#     --set controller.gatewayAPI.enable=true \
#     --set controller.gatewayAPI.installCRDs=true \
#     --set controller.gatewayAPI.gatewayClass.name=wxgw \
#     --set controller.gatewayAPI.service.type=ClusterIP \
#     --set controller.gatewayAPI.defaultGateway.name=shared \
#     --set controller.gatewayAPI.defaultGateway.namespace=gw-e2e
#
#   RELEASE_NS=wxgw CLASS=wxgw dataplane/e2e/gateway.sh
#
# It makes the namespaces gw-e2e and gw-e2e-certs with two Deployments of agnhost's netexec
# (v1, v2), their Services, a Gateway of the class and HTTPRoutes, sends
# requests to the Gateway's address - its own Service - from inside the kind
# node, and scales, restarts and empties v1. The reloads are nginx's own
# count (nginx_ingress_controller_nginx_reloads_total) and the pids of its
# master and workers. Then the default Gateway "shared" on the controller's
# address, and the Service of a Gateway that goes.
#
#   KUBECTL     how to call kubectl ("sudo kubectl")
#   DOCKER      how to call docker ("sudo docker")
#   NODE        the kind node (wxingress-control-plane)
#   RELEASE_NS  the namespace of the release (wxgw)
#   CLASS       its GatewayClass (wxgw)
#   KEEP=1      leave the namespace gw-e2e behind

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxgw}
class=${CLASS:-wxgw}
N=gw-e2e

failed=0
check() {
  if [ "$2" = "$3" ]; then
    echo "ok       $1"
  else
    echo "FAIL     $1"
    echo "  wanted: $3"
    echo "  got:    $2"
    failed=$((failed + 1))
  fi
}
within() {
  if [ "$2" -ge "$3" ] && [ "$2" -le "$4" ]; then
    echo "ok       $1 ($2)"
  else
    echo "FAIL     $1: $2 is not in $3..$4"
    failed=$((failed + 1))
  fi
}

# a rollout that just ended may still have the old pod terminating
waited=0
while [ "$waited" -lt 120 ] && [ "$($K -n "$ns" get pod -o name | wc -l | tr -d ' ')" != "1" ]; do sleep 1; waited=$((waited + 1)); done
pod=$($K -n "$ns" get pod --field-selector=status.phase=Running -o name | head -1)
podip=$($K -n "$ns" get "$pod" -o jsonpath='{.status.podIP}')
[ -n "$podip" ] || { echo "no running controller in $ns"; exit 2; }

X() { $D exec "$node" "$@"; }
reloads() { X curl -s "http://$podip:9113/metrics" | grep '^nginx_ingress_controller_nginx_reloads_total' | awk '{s+=$2} END {print s+0}'; }
nginxes() {
  list=$($K -n "$ns" exec "$pod" -- sh -c 'for p in /proc/[0-9]*; do c=$(tr "\0" " " < $p/cmdline 2>/dev/null); case "$c" in nginx:*) echo "${p#/proc/}:${c#nginx: }";; esac; done' | sort)
  echo "$(echo "$list" | grep -c master) master, $(echo "$list" | grep -c worker) workers, $(echo "$list" | cksum | cut -d' ' -f1)"
}
cond() { $K -n $N get "$1" -o jsonpath="{$2}"; }
# settle waits until nginx has not reloaded for six seconds: what came before
# may still be on its way
settle() {
  last=$(reloads)
  stable=0
  while [ "$stable" -lt 3 ]; do
    sleep 2
    now=$(reloads)
    if [ "$now" = "$last" ]; then stable=$((stable + 1)); else stable=0; last=$now; fi
  done
}
addr() { cond gateway/gw '.status.addresses[0].value'; }
# the answer of one request: the pod's name, or the status code if it is no 200
get() {
  host=$1
  shift
  out=$(X curl -s --max-time 3 -w '\n%{http_code}' -H "Host: $host" "$@" "http://$(addr)/hostname")
  code=$(echo "$out" | tail -1)
  if [ "$code" = "200" ]; then echo "$out" | head -1; else echo "$code"; fi
}
# the pods that answer `count` requests, sorted
through() {
  n=$1
  shift
  for _ in $(seq 1 "$n"); do get "$@"; done | sort -u | tr '\n' ' ' | sed 's/ $//'
}
pods() { $K -n $N get pod -l app="$1" --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' | tr ' ' '\n' | grep . | sort | tr '\n' ' ' | sed 's/ $//'; }
converged() {
  start=$(date +%s)
  while [ $(($(date +%s) - start)) -lt "$1" ]; do
    want=$(pods v1)
    [ -n "$want" ] && [ "$(through 30 gw.example.com)" = "$want" ] && { echo "$(($(date +%s) - start))s"; return; }
    sleep 1
  done
  echo "timeout"
}

$K delete ns $N --wait=true >/dev/null 2>&1
$K create ns $N >/dev/null
deploy() {
  cat <<YAML
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $1, namespace: $N}
spec:
  replicas: $2
  selector: {matchLabels: {app: $1}}
  template:
    metadata: {labels: {app: $1}}
    spec:
      terminationGracePeriodSeconds: 5
      containers:
      - name: echo
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: [netexec, --http-port=8080]
        readinessProbe: {httpGet: {path: /healthz, port: 8080}, periodSeconds: 1}
---
apiVersion: v1
kind: Service
metadata: {name: $1, namespace: $N}
spec: {selector: {app: $1}, ports: [{port: 80, targetPort: 8080}]}
YAML
}
{ deploy v1 2; deploy v2 1; cat <<YAML
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: gw, namespace: $N}
spec:
  gatewayClassName: $class
  listeners:
  - {name: http, port: 80, protocol: HTTP}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: app, namespace: $N}
spec:
  parentRefs: [{name: gw}]
  hostnames: [gw.example.com]
  rules:
  - matches: [{path: {type: PathPrefix, value: /}, headers: [{name: X-Version, value: "2"}]}]
    backendRefs: [{name: v2, port: 80}]
  - backendRefs: [{name: v1, port: 80}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: split, namespace: $N}
spec:
  parentRefs: [{name: gw}]
  hostnames: [split.example.com]
  rules:
  - backendRefs: [{name: v1, port: 80, weight: 80}, {name: v2, port: 80, weight: 20}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: missing, namespace: $N}
spec:
  parentRefs: [{name: gw}]
  hostnames: [missing.example.com]
  rules:
  - backendRefs: [{name: nope, port: 80}]
YAML
} | $K apply -f - >/dev/null
$K -n $N rollout status deploy/v1 --timeout=120s >/dev/null
$K -n $N rollout status deploy/v2 --timeout=120s >/dev/null

echo "--- set up (reloads expected here)"
waited=0
# the status of routes comes with the reload that serves them
while [ "$waited" -lt 60 ] && { [ "$(cond gateway/gw '.status.conditions[?(@.type=="Programmed")].status')" != "True" ] ||
  [ "$(cond gateway/gw '.status.listeners[0].attachedRoutes')" != "3" ] ||
  [ -z "$(cond httproute/missing '.status.parents[0].conditions[?(@.type=="ResolvedRefs")].reason')" ]; }; do
  sleep 1
  waited=$((waited + 1))
done
check "the GatewayClass is accepted" "$($K get gatewayclass "$class" -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}')" "True"
check "the Gateway is accepted" "$(cond gateway/gw '.status.conditions[?(@.type=="Accepted")].status')" "True"
check "and programmed" "$(cond gateway/gw '.status.conditions[?(@.type=="Programmed")].status')" "True"
ctrl=$($K -n "$ns" get svc -l app.kubernetes.io/instance="$ns" -o jsonpath='{.items[0].spec.clusterIP}')
gwsvc() { $K -n $N get svc -l "gateway.networking.k8s.io/gateway-name=$1" -o jsonpath="{.items[*]$2}"; }
check "its address is its own Service's" "$(addr)" "$(gwsvc gw .spec.clusterIP)"
check "which is not the controller's" "$([ "$(addr)" != "$ctrl" ] && echo other)" "other"
check "the Service is in the Gateway's namespace, owned by it" "$(gwsvc gw '.metadata.ownerReferences[0].kind')/$(gwsvc gw '.metadata.ownerReferences[0].name')" "Gateway/gw"
check "without a selector, its EndpointSlice has the controller's pods" "$(gwsvc gw '.spec.selector')/$($K -n $N get endpointslice -l "kubernetes.io/service-name=$(gwsvc gw .metadata.name)" -o jsonpath='{.items[0].endpoints[0].addresses[0]}')" "/$podip"
check "and maps port 80 into the listener ports" "$(gwsvc gw '.spec.ports[0].port') $([ "$(gwsvc gw '.spec.ports[0].targetPort')" -ge 20000 ] && echo ok)" "80 ok"
check "its listener has the three routes" "$(cond gateway/gw '.status.listeners[0].attachedRoutes')" "3"
check "the route is accepted" "$(cond httproute/app '.status.parents[0].conditions[?(@.type=="Accepted")].status')" "True"
check "its references are resolved" "$(cond httproute/app '.status.parents[0].conditions[?(@.type=="ResolvedRefs")].status')" "True"
check "the route to a missing Service is not resolved" "$(cond httproute/missing '.status.parents[0].conditions[?(@.type=="ResolvedRefs")].reason')" "BackendNotFound"

t=$(converged 60)
check "the hostname reaches v1, both pods ($t)" "$(through 30 gw.example.com)" "$(pods v1)"
check "X-Version: 2 reaches v2" "$(through 10 gw.example.com -H 'X-Version: 2')" "$(pods v2)"
check "another hostname is not found" "$(get other.example.com)" "404"
check "a missing backend is 500" "$(get missing.example.com)" "500"
v2=$(for _ in $(seq 1 200); do get split.example.com; done | grep -c "^v2-")
within "weights 80/20: requests of 200 to v2" "$v2" 20 60

r0=$(reloads)
p0=$(nginxes)
echo "         reloads so far: $r0; nginx: $p0"

echo "--- endpoints without reloads"
$K -n $N scale deploy/v1 --replicas=4 >/dev/null
$K -n $N rollout status deploy/v1 --timeout=120s >/dev/null
t=$(converged 60)
check "scaled to 4, all four answer ($t)" "$(through 40 gw.example.com | wc -w | tr -d ' ')" "4"

$K -n $N scale deploy/v1 --replicas=1 >/dev/null
sleep 8
t=$(converged 60)
check "scaled to 1, only it answers ($t)" "$(through 20 gw.example.com)" "$(pods v1)"

$K -n $N rollout restart deploy/v1 >/dev/null
$K -n $N rollout status deploy/v1 --timeout=120s >/dev/null
sleep 8
t=$(converged 60)
check "restarted, the new pod answers ($t)" "$(through 20 gw.example.com)" "$(pods v1)"

$K -n $N scale deploy/v1 --replicas=0 >/dev/null
sleep 8
check "no pods: no pod answers" "$(through 3 gw.example.com | grep -c v1-)" "0"
$K -n $N scale deploy/v1 --replicas=2 >/dev/null
$K -n $N rollout status deploy/v1 --timeout=120s >/dev/null
check "and two pods again ($(converged 60))" "$(through 30 gw.example.com)" "$(pods v1)"

sleep 2
check "none of it reloaded nginx: the count" "$(reloads)" "$r0"
check "and the same master and workers" "$(nginxes)" "$p0"

echo "--- a change of a route reloads"
$K -n $N patch httproute app --type json -p '[{"op":"replace","path":"/spec/rules/0/matches/0/headers/0/value","value":"3"}]' >/dev/null
sleep 5
check "the changed header match is one reload" "$(reloads)" "$((r0 + 1))"
check "X-Version: 3 reaches v2" "$(through 10 gw.example.com -H 'X-Version: 3')" "$(pods v2)"
check "X-Version: 2 reaches v1 now" "$(through 10 gw.example.com -H 'X-Version: 2')" "$(pods v1)"

echo "--- what reloads, and how changes batch (docs/developer/upstream.md)"
# reloadsfor <what> <command...>: the reloads a change makes, once nginx has settled
reloadsfor() {
  what=$1
  shift
  settle
  before=$(reloads)
  "$@" >/dev/null
  settle
  n=$(($(reloads) - before))
  echo "         $what: $n reload(s)"
  echo "$n"
}
patchrule() { $K -n $N patch httproute app --type json -p "[{\"op\":\"replace\",\"path\":\"/spec/rules/0/matches/0/headers/0/value\",\"value\":\"$1\"}]"; }
burst() { for v in 11 12 13 14 15 16 17 18 19 20; do patchrule $v; done; }
weights() { $K -n $N patch httproute split --type json -p '[{"op":"replace","path":"/spec/rules/0/backendRefs/0/weight","value":20}]'; }
listener() { $K -n $N patch gateway gw --type json -p '[{"op":"add","path":"/spec/listeners/-","value":{"name":"alt","port":8080,"protocol":"HTTP"}}]'; }
n=$(reloadsfor "one change of a route's match" patchrule 21 | tail -1)
check "one change of a match is one reload ($n)" "$n" "1"
n=$(reloadsfor "ten changes of a match in a row" burst | tail -1)
within "ten changes of a match in a row are batched into few reloads" "$n" 1 3
n=$(reloadsfor "a change of the weights of a route" weights | tail -1)
check "a change of the weights of two backends is no reload: the module's canary ($n)" "$n" "0"
v2=$(for _ in $(seq 1 200); do get split.example.com; done | grep -c "^v2-")
within "and 20/20 at once: requests of 200 to v2" "$v2" 60 140
reloadsfor "a new listener port of a Gateway" listener | head -1
patchrule 2 >/dev/null

echo "--- what an Ingress has: Policies and ingress-nginx annotations on routes"
cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: v1
kind: Secret
metadata: {name: users, namespace: $N}
type: nginx.org/htpasswd
stringData: {htpasswd: "user:{PLAIN}secret"}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: auth, namespace: $N}
spec: {basicAuth: {secret: users, realm: e2e}}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: deny, namespace: $N}
spec: {accessControl: {deny: [0.0.0.0/0]}}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: rate, namespace: $N}
spec: {rateLimit: {rate: 1r/s, key: "\${binary_remote_addr}", zoneSize: 10M}}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: tuned
  namespace: $N
  annotations:
    wxingress.wizardtales.com/policies: auth
    nginx.ingress.kubernetes.io/affinity: cookie
    nginx.ingress.kubernetes.io/session-cookie-name: WXGW
    nginx.ingress.kubernetes.io/proxy-body-size: 1k
spec:
  parentRefs: [{name: gw}]
  hostnames: [tuned.example.com]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: denied
  namespace: $N
  annotations: {wxingress.wizardtales.com/policies: deny}
spec:
  parentRefs: [{name: gw}]
  hostnames: [denied.example.com]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: limited
  namespace: $N
  annotations: {wxingress.wizardtales.com/policies: rate}
spec:
  parentRefs: [{name: gw}]
  hostnames: [limited.example.com]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
YAML
$K -n $N scale deploy/v1 --replicas=3 >/dev/null
$K -n $N rollout status deploy/v1 --timeout=120s >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(get tuned.example.com -u user:secret | grep -c '^v1-')" != "1" ]; do sleep 1; waited=$((waited + 1)); done
check "basic auth of a Policy: without credentials" "$(get tuned.example.com)" "401"
check "and with them" "$(get tuned.example.com -u user:secret | sed 's/-[^-]*-[^-]*$//')" "v1"
check "access control of a Policy denies" "$(get denied.example.com)" "403"
limited=$(for _ in $(seq 1 20); do get limited.example.com; done | grep -c '^503$')
within "the rate limit of a Policy rejects most of 20 requests in a row" "$limited" 10 20
check "proxy-body-size on the route: a larger body" "$(X curl -s -o /dev/null -w '%{http_code}' --max-time 3 -u user:secret -H 'Host: tuned.example.com' --data-binary @/etc/os-release --data-binary @/etc/os-release --data-binary @/etc/os-release --data-binary @/etc/os-release "http://$(addr)/echo")" "413"
cookie=$(X curl -s -o /dev/null -D - --max-time 3 -u user:secret -H 'Host: tuned.example.com' "http://$(addr)/hostname" | sed -n 's/^[Ss]et-[Cc]ookie: \(WXGW=[^;]*\).*/\1/p' | tr -d '\r')
check "affinity on the route: the module sets the cookie" "$(echo "$cookie" | grep -c '^WXGW=')" "1"
check "and every request with it goes to one pod" "$(through 20 tuned.example.com -u user:secret -H "Cookie: $cookie" | wc -w | tr -d ' ')" "1"
check "without it to all three" "$(through 40 tuned.example.com -u user:secret | wc -w | tr -d ' ')" "3"
$K -n $N annotate httproute tuned nginx.ingress.kubernetes.io/affinity- nginx.ingress.kubernetes.io/session-cookie-name- nginx.ingress.kubernetes.io/load-balance=ewma >/dev/null
settle
r3=$(reloads)
$K -n $N annotate httproute tuned nginx.ingress.kubernetes.io/load-balance=round_robin --overwrite >/dev/null
settle
check "switching the module's balancer from ewma to round_robin reloads nothing" "$(($(reloads) - r3))" "0"
check "and round_robin reaches all three" "$(through 40 tuned.example.com -u user:secret | wc -w | tr -d ' ')" "3"

# more of what an Ingress has: canary-by-*, limit-*, snippets, service-upstream
# and the Policies for external auth, API keys and caching
{ deploy authz 1; cat <<YAML
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: extauth, namespace: $N}
spec: {externalAuth: {authURI: /hostname, authServiceName: authz}}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: extauth-broken, namespace: $N}
spec: {externalAuth: {authURI: /hostname, authServiceName: authz-down}}
---
apiVersion: v1
kind: Service
metadata: {name: authz-down, namespace: $N}
spec: {selector: {app: nobody}, ports: [{port: 80, targetPort: 8080}]}
---
apiVersion: v1
kind: Secret
metadata: {name: apikeys, namespace: $N}
type: nginx.org/apikey
stringData: {client1: s3cret}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: apikey, namespace: $N}
spec: {apiKey: {suppliedIn: {header: [X-API-Key]}, clientSecret: apikeys}}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: cache, namespace: $N}
spec: {cache: {cacheZoneName: e2e, cacheZoneSize: 10m, allowedCodes: [200], time: 1m}}
YAML
route() {
  cat <<YAML
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: $1
  namespace: $N
  annotations: {$2}
spec:
  parentRefs: [{name: gw}]
  hostnames: [$1.example.com]
  rules: [{backendRefs: [${3:-{name: v1, port: 80\}}]}]
YAML
}
route byheader 'nginx.ingress.kubernetes.io/canary-by-header: X-Canary, nginx.ingress.kubernetes.io/canary-by-cookie: beta' \
  '{name: v1, port: 80, weight: 100}, {name: v2, port: 80, weight: 0}'
# limit-rate and caching need proxy-buffering, which ingress-nginx's defaults turn off - on an Ingress as here
route slow 'nginx.ingress.kubernetes.io/limit-rate: "1", nginx.ingress.kubernetes.io/limit-connections: "1", nginx.ingress.kubernetes.io/proxy-buffering: "on"'
route rps 'nginx.ingress.kubernetes.io/limit-rps: "1", nginx.ingress.kubernetes.io/limit-burst-multiplier: "1"'
route snip 'nginx.ingress.kubernetes.io/server-snippet: "location = /server-snippet { return 200 \"server-snippet\\n\"; }", nginx.ingress.kubernetes.io/configuration-snippet: "add_header X-Snippet on always;"'
route clusterip 'nginx.ingress.kubernetes.io/service-upstream: "true"'
route extauth 'wxingress.wizardtales.com/policies: extauth'
route extauth-broken 'wxingress.wizardtales.com/policies: extauth-broken'
route keyed 'wxingress.wizardtales.com/policies: apikey'
route cached 'wxingress.wizardtales.com/policies: cache, nginx.ingress.kubernetes.io/proxy-buffering: "on"'
} | $K apply -f - >/dev/null
$K -n $N rollout status deploy/authz --timeout=120s >/dev/null
waited=0
while [ "$waited" -lt 90 ] && { [ "$(get cached.example.com | grep -c '^v1-')" != "1" ] || [ "$(get byheader.example.com | grep -c '^v1-')" != "1" ]; }; do sleep 1; waited=$((waited + 1)); done
settle
v2of() { n=0; for _ in $(seq 1 10); do get byheader.example.com "$@" | grep -q '^v2-' && n=$((n + 1)); done; echo "$n"; }
check "canary-by-header on a route, the canary's weight 0: without it no request to v2" "$(v2of)" "0"
check "X-Canary: always, all ten" "$(v2of -H 'X-Canary: always')" "10"
check "canary-by-cookie: beta=always, all ten" "$(v2of -H 'Cookie: beta=always')" "10"
check "X-Canary: never wins over the cookie" "$(v2of -H 'X-Canary: never' -H 'Cookie: beta=always')" "0"
r4=$(reloads)
$K -n $N annotate httproute byheader nginx.ingress.kubernetes.io/canary-by-header-value=yes >/dev/null
settle
check "canary-by-header-value added: no reload, a rule of the module" "$(($(reloads) - r4))" "0"
check "X-Canary: yes, all ten" "$(v2of -H 'X-Canary: yes')" "10"
check "X-Canary: always now means nothing" "$(v2of -H 'X-Canary: always')" "0"

long=$(printf '%3000s' | tr ' ' x)
t=$(X curl -s -o /dev/null -w '%{time_total}' --max-time 10 -H 'Host: slow.example.com' "http://$(addr)/echo?msg=$long")
check "limit-rate 1 on the route: 3000 bytes take more than a second ($t s)" "$(echo "$t" | awk '{print ($1 >= 1) ? "yes" : "no"}')" "yes"
check "limit-connections 1: a second connection while one is open is 503" \
  "$(X sh -c "curl -s -o /dev/null --max-time 10 -H 'Host: slow.example.com' 'http://$(addr)/echo?msg=$long' & sleep 0.5; curl -s -o /dev/null -w '%{http_code}' --max-time 3 -H 'Host: slow.example.com' 'http://$(addr)/hostname'; wait")" "503"
limited=$(for _ in $(seq 1 20); do get rps.example.com; done | grep -c '^503$')
within "limit-rps 1 with burst multiplier 1 on the route rejects most of 20 requests in a row" "$limited" 10 20

if $K -n "$ns" get deploy -o jsonpath='{..args}' | grep -q 'enable-snippets=true'; then
  check "server-snippet on the route, with -enable-snippets: its location" "$(X curl -s --max-time 3 -H 'Host: snip.example.com' "http://$(addr)/server-snippet")" "server-snippet"
  check "configuration-snippet: its header" "$(X curl -s -o /dev/null -D - --max-time 3 -H 'Host: snip.example.com' "http://$(addr)/hostname" | grep -ci '^x-snippet: on')" "1"
else
  check "without -enable-snippets the route has no snippets" "$(X curl -s -o /dev/null -D - --max-time 3 -H 'Host: snip.example.com' "http://$(addr)/hostname" | grep -ci '^x-snippet:')" "0"
fi
clusterip=$($K -n $N get svc v1 -o jsonpath='{.spec.clusterIP}')
check "service-upstream on the route: its upstream is the ClusterIP" "$($K -n "$ns" exec "$pod" -- sh -c 'grep -l clusterip.example.com /etc/nginx/conf.d/*.conf | xargs cat' | grep -q "server $clusterip:80" && echo yes)" "yes"
check "and it answers" "$(get clusterip.example.com | grep -c '^v1-')" "1"
check "external auth of a Policy: the auth service says 2xx, the request goes on" "$(get extauth.example.com | grep -c '^v1-')" "1"
check "and when it has no endpoints, it does not" "$(get extauth-broken.example.com)" "500"
settle
r5=$(reloads)
$K -n $N scale deploy/authz --replicas=2 >/dev/null
$K -n $N rollout status deploy/authz --timeout=120s >/dev/null
sleep 3
settle
check "scaling the auth service of a Policy reloads nothing" "$(($(reloads) - r5))" "0"
check "an API key Policy: without the key" "$(get keyed.example.com)" "401"
check "with it" "$(get keyed.example.com -H 'X-API-Key: s3cret' | grep -c '^v1-')" "1"
check "a cache Policy: three pods, one cached answer" "$(through 20 cached.example.com | wc -w | tr -d ' ')" "1"
$K -n $N scale deploy/v1 --replicas=2 >/dev/null

echo "--- the default Gateway shares the controller's address"
cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: shared, namespace: $N}
spec:
  gatewayClassName: $class
  listeners:
  - {name: http, port: 80, protocol: HTTP}
  - {name: alt, port: 8080, protocol: HTTP}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: shared, namespace: $N}
spec:
  parentRefs: [{name: shared, sectionName: http}]
  hostnames: [shared.example.com]
  rules:
  - backendRefs: [{name: v2, port: 80}]
YAML
waited=0
while [ "$waited" -lt 60 ] && [ "$(cond gateway/shared '.status.listeners[0].attachedRoutes')" != "1" ]; do sleep 1; waited=$((waited + 1)); done
sleep 3
check "its address is the controller's" "$(cond gateway/shared '.status.addresses[0].value')" "$ctrl"
check "it has no Service of its own" "$(gwsvc shared .metadata.name)" ""
check "its listener on 8080 is not accepted" "$(cond gateway/shared '.status.listeners[?(@.name=="alt")].conditions[?(@.type=="Accepted")].reason')" "PortUnavailable"
shared() { X curl -s --max-time 3 -o /dev/null -w '%{http_code}' -H "Host: $1" "http://$2/hostname"; }
check "its hostname answers on the controller's address" "$(shared shared.example.com "$ctrl")" "200"
check "but not on the address of the other Gateway" "$(shared shared.example.com "$(addr)")" "404"
check "whose hostname does not answer on the controller's" "$(shared gw.example.com "$ctrl")" "404"

echo "--- TLS: a certificate from another namespace, HTTP and HTTPS apart"
C=$N-certs
$K delete ns $C --wait=true >/dev/null 2>&1
$K create ns $C >/dev/null
tmp=$(mktemp -d)
# secret <namespace> <name> <host> <organization>: a TLS Secret with a new
# certificate, created or replaced
secret() {
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/O=$4/CN=$3" \
    -addext "subjectAltName=DNS:$3" -keyout "$tmp/key" -out "$tmp/crt" 2>/dev/null
  $K -n "$1" create secret tls "$2" --cert "$tmp/crt" --key "$tmp/key" --dry-run=client -o yaml | $K apply -f - >/dev/null
}
for host in tls.example.com shared.example.com; do
  secret $C $host $host first
done
cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata: {name: gateways, namespace: $C}
spec:
  from: [{group: gateway.networking.k8s.io, kind: Gateway, namespace: $N}]
  to: [{group: "", kind: Secret}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: tls, namespace: $N}
spec:
  gatewayClassName: $class
  listeners:
  - {name: http, port: 80, protocol: HTTP, hostname: tls.example.com}
  - name: https
    port: 443
    protocol: HTTPS
    hostname: tls.example.com
    tls: {certificateRefs: [{name: tls.example.com, namespace: $C}]}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: tls-http, namespace: $N}
spec:
  parentRefs: [{name: tls, sectionName: http}]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: tls-https, namespace: $N}
spec:
  parentRefs: [{name: tls, sectionName: https}]
  rules: [{backendRefs: [{name: v2, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: shared, namespace: $N}
spec:
  gatewayClassName: $class
  listeners:
  - {name: http, port: 80, protocol: HTTP}
  - {name: alt, port: 8080, protocol: HTTP}
  - name: https
    port: 443
    protocol: HTTPS
    hostname: shared.example.com
    tls: {certificateRefs: [{name: shared.example.com, namespace: $C}]}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: shared-https, namespace: $N}
spec:
  parentRefs: [{name: shared, sectionName: https}]
  hostnames: [shared.example.com]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
YAML
waited=0
while [ "$waited" -lt 60 ] && { [ "$(cond gateway/tls '.status.conditions[?(@.type=="Programmed")].status')" != "True" ] ||
  [ "$(cond httproute/tls-https '.status.parents[0].conditions[?(@.type=="Accepted")].status')" != "True" ] ||
  [ "$(cond httproute/shared-https '.status.parents[0].conditions[?(@.type=="Accepted")].status')" != "True" ]; }; do
  sleep 1
  waited=$((waited + 1))
done
sleep 3
tls=$(cond gateway/tls '.status.addresses[0].value')
app() { X curl -s --max-time 3 "$@" | sed 's/-[^-]*-[^-]*$//'; }
subject() { X curl -sk --max-time 3 -o /dev/null -v --resolve "$1:${3:-443}:$2" "https://$1:${3:-443}/" 2>&1 | sed -n 's/.*subject: *//p' | head -1; }
check "the HTTP listener has its route" "$(app -H 'Host: tls.example.com' "http://$tls/hostname")" "v1"
check "the HTTPS listener of the same hostname its own" "$(app -k --resolve "tls.example.com:443:$tls" https://tls.example.com/hostname)" "v2"
check "with the certificate from the other namespace" "$(subject tls.example.com "$tls")" "O=first; CN=tls.example.com"
check "the default Gateway's hostname keeps HTTP" "$(app -H 'Host: shared.example.com' "http://$ctrl/hostname")" "v2"
check "and its HTTPS listener has another route" "$(app -k --resolve "shared.example.com:443:$ctrl" https://shared.example.com/hostname)" "v1"
check "with its certificate" "$(subject shared.example.com "$ctrl")" "O=first; CN=shared.example.com"

echo "--- certificates rotate without reloads"
secret $N term term.example.com first
cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: term, namespace: $N}
spec:
  gatewayClassName: $class
  listeners:
  - name: tls
    port: 8443
    protocol: TLS
    hostname: term.example.com
    tls: {mode: Terminate, certificateRefs: [{name: term}]}
---
apiVersion: gateway.networking.k8s.io/v1
kind: TLSRoute
metadata: {name: term, namespace: $N}
spec:
  parentRefs: [{name: term}]
  hostnames: [term.example.com]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
YAML
waited=0
while [ "$waited" -lt 60 ] && [ "$(cond tlsroute/term '.status.parents[0].conditions[?(@.type=="Accepted")].status')" != "True" ]; do sleep 1; waited=$((waited + 1)); done
term=$(cond gateway/term '.status.addresses[0].value')
waited=0
while [ "$waited" -lt 30 ] && [ "$(subject term.example.com "$term" 8443)" != "O=first; CN=term.example.com" ]; do sleep 1; waited=$((waited + 1)); done
check "a TLSRoute terminates with the listener's certificate" "$(subject term.example.com "$term" 8443)" "O=first; CN=term.example.com"
check "and passes on to its backend" "$(app -k --resolve "term.example.com:8443:$term" https://term.example.com:8443/hostname)" "v1"
settle
r2=$(reloads)
p2=$(nginxes)
secret $C tls.example.com tls.example.com rotated
secret $C shared.example.com shared.example.com rotated
secret $N term term.example.com rotated
rotated() {
  waited=0
  while [ "$waited" -lt 30 ] && [ "$(subject "$@")" != "O=rotated; CN=$1" ]; do sleep 1; waited=$((waited + 1)); done
  echo "$(subject "$@") (${waited}s)"
}
check "the HTTPS listener serves the rotated certificate" "$(rotated tls.example.com "$tls" | cut -d' ' -f1-2)" "O=rotated; CN=tls.example.com"
check "so does the default Gateway's" "$(subject shared.example.com "$ctrl")" "O=rotated; CN=shared.example.com"
check "and the TLSRoute's" "$(rotated term.example.com "$term" 8443 | cut -d' ' -f1-2)" "O=rotated; CN=term.example.com"
sleep 2
check "none of the rotations reloaded nginx: the count" "$(reloads)" "$r2"
check "and the same master and workers" "$(nginxes)" "$p2"

echo "--- Policies of a Gateway: client certificates (ingressMTLS) and HSTS"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=e2e-ca" -keyout "$tmp/ca.key" -out "$tmp/ca.crt" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=client" -keyout "$tmp/client.key" -out "$tmp/client.csr" 2>/dev/null
openssl x509 -req -in "$tmp/client.csr" -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAcreateserial -days 1 -out "$tmp/client.crt" 2>/dev/null
$D cp "$tmp/client.crt" "$node:/var/tmp/wxgw-e2e-client.crt" && $D cp "$tmp/client.key" "$node:/var/tmp/wxgw-e2e-client.key"
$K -n $N create secret generic mtls-ca --type=nginx.org/ca --from-file=ca.crt="$tmp/ca.crt" >/dev/null
secret $N mtls mtls.example.com first
cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: mtls, namespace: $N}
spec: {ingressMTLS: {clientCertSecret: mtls-ca}}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: hsts, namespace: $N}
spec: {hsts: {maxAge: 600}}
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: mtls
  namespace: $N
  annotations: {wxingress.wizardtales.com/policies: "mtls, hsts"}
spec:
  gatewayClassName: $class
  listeners:
  - name: https
    port: 443
    protocol: HTTPS
    hostname: mtls.example.com
    tls: {certificateRefs: [{name: mtls}]}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: mtls, namespace: $N}
spec:
  parentRefs: [{name: mtls}]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
YAML
waited=0
while [ "$waited" -lt 60 ] && [ "$(cond httproute/mtls '.status.parents[0].conditions[?(@.type=="Accepted")].status')" != "True" ]; do sleep 1; waited=$((waited + 1)); done
mtls=$(cond gateway/mtls '.status.addresses[0].value')
mget() { X curl -sk --max-time 3 --resolve "mtls.example.com:443:$mtls" "$@" https://mtls.example.com/hostname; }
waited=0
while [ "$waited" -lt 60 ] && [ "$(mget --cert /var/tmp/wxgw-e2e-client.crt --key /var/tmp/wxgw-e2e-client.key | sed 's/-[^-]*-[^-]*$//')" != "v1" ]; do sleep 1; waited=$((waited + 1)); done
check "ingressMTLS of the Gateway: without a client certificate" "$(mget -o /dev/null -w '%{http_code}')" "400"
check "with one the CA signed" "$(mget --cert /var/tmp/wxgw-e2e-client.crt --key /var/tmp/wxgw-e2e-client.key | sed 's/-[^-]*-[^-]*$//')" "v1"
check "HSTS of the Gateway" "$(mget -D - -o /dev/null --cert /var/tmp/wxgw-e2e-client.crt --key /var/tmp/wxgw-e2e-client.key | tr -d '\r' | sed -n 's/^[Ss]trict-[Tt]ransport-[Ss]ecurity: //p')" "max-age=600"
X rm -f /var/tmp/wxgw-e2e-client.crt /var/tmp/wxgw-e2e-client.key

echo "--- egressMTLS: TLS to a backend that verifies client certificates, a route and a VirtualServer"
# one CA for the backend's certificate and the clients'; the backend, nginx,
# answers with the subject of the client certificate it verified
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=e2e-backend-ca" -keyout "$tmp/bca.key" -out "$tmp/bca.crt" 2>/dev/null
signed() {
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$1" -addext "subjectAltName=DNS:$1" -keyout "$tmp/$1.key" -out "$tmp/$1.csr" 2>/dev/null
  printf 'subjectAltName=DNS:%s\n' "$1" > "$tmp/$1.ext"
  openssl x509 -req -in "$tmp/$1.csr" -CA "$tmp/bca.crt" -CAkey "$tmp/bca.key" -CAcreateserial -days 1 -extfile "$tmp/$1.ext" -out "$tmp/$1.crt" 2>/dev/null
}
signed backend.example.com
signed client1
signed client2
tlssecret() { $K -n $N create secret tls "$1" --cert "$tmp/$2.crt" --key "$tmp/$2.key" --dry-run=client -o yaml | $K apply -f - >/dev/null; }
tlssecret mtls-backend backend.example.com
tlssecret egress-client client1
$K -n $N create secret generic egress-ca --type=nginx.org/ca --from-file=ca.crt="$tmp/bca.crt" >/dev/null
$K -n $N create secret generic mtls-backend-ca --from-file=ca.crt="$tmp/bca.crt" >/dev/null
{ cat <<'CONF' | sed 's/^/    /'
server {
    listen 8443 ssl;
    ssl_certificate /tls/tls.crt;
    ssl_certificate_key /tls/tls.key;
    ssl_client_certificate /ca/ca.crt;
    ssl_verify_client on;
    keepalive_timeout 2s;
    location / { return 200 "$ssl_client_s_dn\n"; }
}
CONF
} > "$tmp/backend.conf"
eroute() {
  cat <<YAML
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: $1
  namespace: $N
  annotations: {nginx.ingress.kubernetes.io/backend-protocol: HTTPS$2}
spec:
  parentRefs: [{name: gw}]
  hostnames: [$1.example.com]
  rules: [{backendRefs: [{name: mtls-backend, port: 443}]}]
YAML
}
{ cat <<YAML
---
apiVersion: v1
kind: ConfigMap
metadata: {name: mtls-backend, namespace: $N}
data:
  default.conf: |
$(cat "$tmp/backend.conf")
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: mtls-backend, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: mtls-backend}}
  template:
    metadata: {labels: {app: mtls-backend}}
    spec:
      terminationGracePeriodSeconds: 5
      containers:
      - name: nginx
        image: docker.io/library/nginx:1.27-alpine
        readinessProbe: {tcpSocket: {port: 8443}, periodSeconds: 1}
        volumeMounts:
        - {name: conf, mountPath: /etc/nginx/conf.d}
        - {name: tls, mountPath: /tls}
        - {name: ca, mountPath: /ca}
      volumes:
      - {name: conf, configMap: {name: mtls-backend}}
      - {name: tls, secret: {secretName: mtls-backend}}
      - {name: ca, secret: {secretName: mtls-backend-ca}}
---
apiVersion: v1
kind: Service
metadata: {name: mtls-backend, namespace: $N}
spec: {selector: {app: mtls-backend}, ports: [{port: 443, targetPort: 8443}]}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: egress, namespace: $N}
spec: {egressMTLS: {tlsSecret: egress-client, trustedCertSecret: egress-ca, verifyServer: true, serverName: true, sslName: backend.example.com, sessionReuse: false}}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: egress-othername, namespace: $N}
spec: {egressMTLS: {tlsSecret: egress-client, trustedCertSecret: egress-ca, verifyServer: true, serverName: true, sslName: other.example.com}}
---
apiVersion: k8s.nginx.org/v1
kind: VirtualServer
metadata: {name: vs-etls, namespace: $N}
spec:
  ingressClassName: $class
  host: vs-etls.example.com
  upstreams: [{name: backend, service: mtls-backend, port: 443, tls: {enable: true}}]
  routes: [{path: /, policies: [{name: egress}], action: {pass: backend}}]
YAML
eroute etls ', wxingress.wizardtales.com/policies: egress'
eroute etls-nocert ''
eroute etls-othername ', wxingress.wizardtales.com/policies: egress-othername'
} | $K apply -f - >/dev/null
$K -n $N rollout status deploy/mtls-backend --timeout=120s >/dev/null
eget() { X curl -s --max-time 3 -H "Host: $1" "http://${2:-$(addr)}/"; }
ecode() { X curl -s -o /dev/null -w '%{http_code}' --max-time 3 -H "Host: $1" "http://${2:-$(addr)}/"; }
waited=0
while [ "$waited" -lt 90 ] && { [ "$(eget etls.example.com)" != "CN=client1" ] || [ "$(eget vs-etls.example.com "$ctrl")" != "CN=client1" ]; }; do sleep 1; waited=$((waited + 1)); done
check "egressMTLS on a route: the backend verified the client certificate" "$(eget etls.example.com)" "CN=client1"
check "and on a VirtualServer" "$(eget vs-etls.example.com "$ctrl")" "CN=client1"
check "TLS to the backend without the Policy: no client certificate, the backend says 400" "$(ecode etls-nocert.example.com)" "400"
check "verifyServer with an sslName the backend's certificate is not for: 502" "$(ecode etls-othername.example.com)" "502"
settle
r6=$(reloads)
tlssecret egress-client client2
waited=0
while [ "$waited" -lt 60 ] && { [ "$(eget etls.example.com)" != "CN=client2" ] || [ "$(eget vs-etls.example.com "$ctrl")" != "CN=client2" ]; }; do sleep 3; waited=$((waited + 3)); done
# a client certificate goes with a full handshake: connections the backend
# keeps alive and resumed sessions (sessionReuse, on by default) present the
# old one until they end - the backend here closes idle ones after 2s, the
# Policy turns sessionReuse off
check "the rotated client certificate is sent on new connections, route and VirtualServer (${waited}s)" "$(eget etls.example.com) $(eget vs-etls.example.com "$ctrl")" "CN=client2 CN=client2"
settle
n=$(($(reloads) - r6))
echo "         rotating the client certificate Secret of egressMTLS: $n reload(s)"
check "rotating the client certificate is no reload, as an Ingress's certificate" "$n" "0"
r7=$(reloads)
$K -n $N scale deploy/mtls-backend --replicas=2 >/dev/null
$K -n $N rollout status deploy/mtls-backend --timeout=120s >/dev/null
sleep 3
settle
check "a second backend pod: the route still answers through TLS" "$(for _ in $(seq 1 4); do eget etls.example.com; sleep 3; done | sort -u)" "CN=client2"
check "and the endpoints of the TLS upstream changed without a reload" "$(($(reloads) - r7))" "0"
rm -rf "$tmp"
settle
$K delete ns $C --wait=true >/dev/null

echo "--- jwt Policies without NGINX Plus: the module checks the tokens, a route and a VirtualServer"
jt=$(mktemp -d)
b64url() { base64 -w0 | tr '+/' '-_' | tr -d '='; }
# rsakey <name>: a key and its JWK with kid <name>
rsakey() {
  openssl genrsa -out "$jt/$1.pem" 2048 2>/dev/null
  n=$(openssl rsa -in "$jt/$1.pem" -noout -modulus 2>/dev/null | sed 's/^Modulus=//' | xxd -r -p | b64url)
  printf '{"kty":"RSA","kid":"%s","alg":"RS256","n":"%s","e":"AQAB"}' "$1" "$n" > "$jt/$1.jwk"
}
# token <key> <sub> <exp offset>: an RS256 token
token() {
  h=$(printf '{"alg":"RS256","typ":"JWT","kid":"%s"}' "$1" | b64url)
  p=$(printf '{"sub":"%s","exp":%s}' "$2" "$(($(date +%s) + $3))" | b64url)
  printf '%s.%s.%s' "$h" "$p" "$(printf '%s.%s' "$h" "$p" | openssl dgst -sha256 -sign "$jt/$1.pem" | b64url)"
}
rsakey one
rsakey two
jwks() { printf '{"keys":[%s]}' "$(for f in "$@"; do cat "$f"; echo; done | paste -sd, -)"; }
$K -n $N create secret generic jwks --type=nginx.org/jwk --from-literal=jwk="$(jwks "$jt/one.jwk")" >/dev/null
{ cat <<YAML
---
apiVersion: v1
kind: ConfigMap
metadata: {name: jwks, namespace: $N}
data:
  jwks.json: '$(jwks "$jt/one.jwk")'
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: jwks, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: jwks}}
  template:
    metadata: {labels: {app: jwks}}
    spec:
      terminationGracePeriodSeconds: 2
      containers:
      - name: nginx
        image: docker.io/library/nginx:1.27-alpine
        readinessProbe: {tcpSocket: {port: 80}, periodSeconds: 1}
        volumeMounts: [{name: jwks, mountPath: /usr/share/nginx/html}]
      volumes: [{name: jwks, configMap: {name: jwks}}]
---
apiVersion: v1
kind: Service
metadata: {name: jwks, namespace: $N}
spec: {selector: {app: jwks}, ports: [{port: 80, targetPort: 80}]}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: jwt, namespace: $N}
spec: {jwt: {realm: e2e, secret: jwks}}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: jwt-uri, namespace: $N}
spec: {jwt: {realm: e2e-uri, jwksURI: "http://jwks.$N.svc.cluster.local/jwks.json", keyCache: 1h, token: \$cookie_auth}}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: jwt
  namespace: $N
  annotations:
    wxingress.wizardtales.com/policies: jwt
    nginx.ingress.kubernetes.io/configuration-snippet: "proxy_set_header X-Sub \$meta_jwt_claim_sub;"
spec:
  parentRefs: [{name: gw}]
  hostnames: [jwt.example.com]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
---
apiVersion: k8s.nginx.org/v1
kind: VirtualServer
metadata: {name: vs-jwt, namespace: $N}
spec:
  ingressClassName: $class
  host: vs-jwt.example.com
  upstreams: [{name: v1, service: v1, port: 80}]
  routes: [{path: /, policies: [{name: jwt-uri}], action: {pass: v1}}]
YAML
} | $K apply -f - >/dev/null
$K -n $N rollout status deploy/jwks --timeout=120s >/dev/null
jget() { h=$1; shift; X curl -s -o /dev/null -w '%{http_code}' --max-time 3 -H "Host: $h" "$@" "http://${JA:-$(addr)}/hostname"; }
good=$(token one alice 3600)
waited=0
while [ "$waited" -lt 90 ] && { [ "$(jget jwt.example.com -H "Authorization: Bearer $good")" != "200" ] ||
  [ "$(JA=$ctrl jget vs-jwt.example.com -H "Cookie: auth=$good")" != "200" ]; }; do sleep 1; waited=$((waited + 1)); done
check "a jwt Policy on a route: no token, 401" "$(jget jwt.example.com)" "401"
check "with the realm" "$(X curl -s -o /dev/null -D - --max-time 3 -H 'Host: jwt.example.com' "http://$(addr)/hostname" | tr -d '\r' | sed -n 's/^[Ww][Ww][Ww]-[Aa]uthenticate: //p')" 'Bearer realm="e2e"'
check "a good token, 200" "$(jget jwt.example.com -H "Authorization: Bearer $good")" "200"
check "and the claim goes to the backend" "$(X curl -s --max-time 3 -H 'Host: jwt.example.com' -H "Authorization: Bearer $good" "http://$(addr)/header?key=X-Sub")" "alice"
check "an expired one, 401" "$(jget jwt.example.com -H "Authorization: Bearer $(token one alice -60)")" "401"
check "one of a key the Secret does not have, 401" "$(jget jwt.example.com -H "Authorization: Bearer $(token two alice 3600)")" "401"
check "a jwt Policy with a jwksURI on a VirtualServer, the token in a cookie: 200" "$(JA=$ctrl jget vs-jwt.example.com -H "Cookie: auth=$good")" "200"
check "and in the header instead, 401" "$(JA=$ctrl jget vs-jwt.example.com -H "Authorization: Bearer $good")" "401"
settle
r9=$(reloads)
$K -n $N create secret generic jwks --type=nginx.org/jwk --from-literal=jwk="$(jwks "$jt/one.jwk" "$jt/two.jwk")" --dry-run=client -o yaml | $K apply -f - >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(jget jwt.example.com -H "Authorization: Bearer $(token two bob 3600)")" != "200" ]; do sleep 1; waited=$((waited + 1)); done
check "a key added to the Secret is taken (${waited}s)" "$(jget jwt.example.com -H "Authorization: Bearer $(token two bob 3600)")" "200"
settle
n=$(($(reloads) - r9))
echo "         a new key in the jwk Secret: $n reload(s)"
check "a new key in the jwk Secret is no reload: the module reads the file again" "$n" "0"
rm -rf "$jt"

echo "--- oidc Policies without NGINX Plus: dex, login, session, refresh, logout - a route and a VirtualServer"

dexhost=dex.$N.svc.cluster.local
cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: v1
kind: ConfigMap
metadata: {name: dex, namespace: $N}
data:
  config.yaml: |
    issuer: http://$dexhost:5556/dex
    storage: {type: memory}
    web: {http: 0.0.0.0:5556}
    expiry: {idTokens: 20s}
    oauth2: {skipApprovalScreen: true}
    enablePasswordDB: true
    staticPasswords:
    - email: alice@example.com
      # "password"
      hash: "\$2a\$10\$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W"
      username: alice
      userID: 08a8684b-db88-4b73-90a9-3cd1661f5466
    staticClients:
    - id: e2e
      secret: e2e-secret
      name: e2e
      redirectURIs: [http://oidc.example.com/_codexch, http://vs-oidc.example.com/_codexch]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: dex, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: dex}}
  template:
    metadata: {labels: {app: dex}}
    spec:
      containers:
      - name: dex
        image: ghcr.io/dexidp/dex@sha256:bc7cfce7c17f52864e2bb2a4dc1d2f86a41e3019f6d42e81d92a301fad0c8a1d
        args: [dex, serve, /etc/dex/config.yaml]
        readinessProbe: {httpGet: {path: /dex/healthz, port: 5556}, periodSeconds: 1}
        volumeMounts: [{name: config, mountPath: /etc/dex}]
      volumes: [{name: config, configMap: {name: dex}}]
---
apiVersion: v1
kind: Service
metadata: {name: dex, namespace: $N}
spec: {selector: {app: dex}, ports: [{port: 5556, targetPort: 5556}]}
---
apiVersion: v1
kind: Secret
metadata: {name: dex-client, namespace: $N}
type: nginx.org/oidc
stringData: {client-secret: e2e-secret}
---
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: oidc, namespace: $N}
spec:
  oidc:
    clientID: e2e
    clientSecret: dex-client
    authEndpoint: http://$dexhost:5556/dex/auth
    tokenEndpoint: http://$dexhost:5556/dex/token
    jwksURI: http://$dexhost:5556/dex/keys
    scope: openid+email+offline_access
    accessTokenEnable: true
---
# only the issuer: the endpoints from dex's discovery document, its iss checked
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata: {name: oidc-discovered, namespace: $N}
spec:
  oidc:
    issuer: http://$dexhost:5556/dex
    clientID: e2e
    clientSecret: dex-client
    scope: openid+email+offline_access
    accessTokenEnable: true
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: oidc
  namespace: $N
  annotations: {wxingress.wizardtales.com/policies: oidc}
spec:
  parentRefs: [{name: gw}]
  hostnames: [oidc.example.com]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
---
apiVersion: k8s.nginx.org/v1
kind: VirtualServer
metadata: {name: vs-oidc, namespace: $N}
spec:
  ingressClassName: $class
  host: vs-oidc.example.com
  policies: [{name: oidc-discovered}]
  upstreams: [{name: v1, service: v1, port: 80}]
  routes: [{path: /, action: {pass: v1}}]
YAML
$K -n $N rollout status deploy/dex --timeout=180s >/dev/null
dexip=$($K -n $N get svc dex -o jsonpath='{.spec.clusterIP}')
jar=/var/tmp/wxgw-e2e-oidc
# as a browser: curl in the node, the app's host on its Gateway's address and dex's on its Service
browse() {
  host=$1 at=$2
  shift 2
  X curl -s --max-time 10 -c "$jar.$host" -b "$jar.$host" --resolve "$dexhost:5556:$dexip" --resolve "$host:80:$at" "$@"
}
# login <host> <address>: from /header?key=username through dex's password form back; what the backend got
login() {
  X rm -f "$jar.$1"
  page=$(browse "$1" "$2" -L -o /dev/null -w '%{url_effective}' "http://$1/header?key=username")
  browse "$1" "$2" -L --data-urlencode login=alice@example.com --data-urlencode password=password "$page"
}
waited=0
# dex has signing keys a moment after it is ready
while [ "$waited" -lt 90 ] && { [ "$(browse oidc.example.com "$(addr)" -o /dev/null -w '%{http_code}' http://oidc.example.com/hostname)" != "302" ] ||
  ! X curl -s --max-time 3 --resolve "$dexhost:5556:$dexip" "http://$dexhost:5556/dex/keys" | grep -q '"kid"'; }; do sleep 1; waited=$((waited + 1)); done
check "an oidc Policy on a route: without a session, to dex" "$(browse oidc.example.com "$(addr)" -o /dev/null -w '%{http_code} %{redirect_url}' http://oidc.example.com/hostname | cut -d'?' -f1)" "302 http://$dexhost:5556/dex/auth"
# dex's subject of alice: its user id and connector, encoded
sub=CiQwOGE4Njg0Yi1kYjg4LTRiNzMtOTBhOS0zY2QxNjYxZjU0NjYSBWxvY2Fs
# a new Service is not reachable from every pod at the same moment: until the first login works
waited=0
while [ "$waited" -lt 60 ] && [ "$(login oidc.example.com "$(addr)")" != "$sub" ]; do sleep 2; waited=$((waited + 2)); done
user=$(login oidc.example.com "$(addr)")
check "logged in with dex's password form, the backend gets the user (sub)" "$user" "$sub"
check "and the session serves the next request" "$(browse oidc.example.com "$(addr)" -o /dev/null -w '%{http_code}' http://oidc.example.com/hostname)" "200"
check "with the access token upstream" "$(browse oidc.example.com "$(addr)" http://oidc.example.com/header?key=Authorization | cut -c1-7)" "Bearer "
user=$(login vs-oidc.example.com "$ctrl")
check "an oidc Policy with only the issuer on a VirtualServer: logged in through discovery" "$user" "$sub"
settle
r10=$(reloads)
sleep 22
# dex keeps one refresh token for a user and a client: the VirtualServer's login took it over
check "the ID token expired (20s): refreshed, the request sent to itself (307)" "$(browse vs-oidc.example.com "$ctrl" -o /dev/null -w '%{http_code}' http://vs-oidc.example.com/hostname)" "307"
check "and served with the new session" "$(browse vs-oidc.example.com "$ctrl" -o /dev/null -w '%{http_code}' http://vs-oidc.example.com/hostname)" "200"
# an administrator ends alice's sessions: a line in the ConfigMap, on every replica, no reload
r11=$(reloads)
login vs-oidc.example.com "$ctrl" >/dev/null
check "a fresh session is served" "$(browse vs-oidc.example.com "$ctrl" -o /dev/null -w '%{http_code}' http://vs-oidc.example.com/hostname)" "200"
$K -n "$ns" create configmap $ns-wxingress-controller-oidc-revocations --from-literal=revocations="sub $sub $(($(date +%s) + 1))" --dry-run=client -o yaml | $K apply -f - >/dev/null 2>&1
waited=0
while [ "$waited" -lt 30 ] && [ "$(browse vs-oidc.example.com "$ctrl" -o /dev/null -w '%{http_code}' http://vs-oidc.example.com/hostname)" != "302" ]; do sleep 1; waited=$((waited + 1)); done
check "the revocation in the ConfigMap ends it (${waited}s)" "$(browse vs-oidc.example.com "$ctrl" -o /dev/null -w '%{http_code}' http://vs-oidc.example.com/hostname)" "302"
check "without a reload" "$(($(reloads) - r11))" "0"
sleep 2
user=$(login vs-oidc.example.com "$ctrl")
check "and a login after it is served" "$user" "$sub"
login vs-oidc.example.com "$ctrl" >/dev/null
copy=/var/tmp/wxgw-e2e-oidc-copy
X cp "$jar.vs-oidc.example.com" "$copy"
browse vs-oidc.example.com "$ctrl" -o /dev/null http://vs-oidc.example.com/logout
waited=0
while [ "$waited" -lt 30 ] && [ "$(X curl -s -o /dev/null -w '%{http_code}' --max-time 10 -b "$copy" --resolve "vs-oidc.example.com:80:$ctrl" http://vs-oidc.example.com/hostname)" != "302" ]; do sleep 1; waited=$((waited + 1)); done
check "a copy of a session cookie ends with /logout, on every replica (${waited}s)" "$(X curl -s -o /dev/null -w '%{http_code}' --max-time 10 -b "$copy" --resolve "vs-oidc.example.com:80:$ctrl" http://vs-oidc.example.com/hostname)" "302"
sessions() { $K -n "$ns" get configmap $ns-wxingress-controller-oidc-revocations -o jsonpath='{.data.revocations}' | grep -c '^ses '; }
waited=0
while [ "$waited" -lt 20 ] && [ "$(sessions)" != "1" ]; do sleep 1; waited=$((waited + 1)); done
check "the logout is written into the ConfigMap (${waited}s)" "$(sessions)" "1"
X rm -f "$copy"
login vs-oidc.example.com "$ctrl" >/dev/null

# the curl of the kind node (7.88) keeps a cookie whose Set-Cookie removes it; browsers do not -
# so the headers of the logout are what is checked, and a browser without the cookies
check "logout: the session cookies are removed" "$(browse vs-oidc.example.com "$ctrl" -D - -o /dev/null http://vs-oidc.example.com/logout | grep -c '^Set-Cookie: wxoidc_s0=; .*Max-Age=0')" "1"
check "and to the post-logout page" "$(browse vs-oidc.example.com "$ctrl" -L http://vs-oidc.example.com/logout)" "You have been logged out."
X rm -f "$jar.vs-oidc.example.com"
check "without the session the next request logs in again" "$(browse vs-oidc.example.com "$ctrl" -o /dev/null -w '%{http_code}' http://vs-oidc.example.com/hostname)" "302"
check "logins, refreshes and logouts reload nothing" "$(($(reloads) - r10))" "0"
X sh -c "rm -f $jar.*"
$K -n "$ns" delete configmap $ns-wxingress-controller-oidc-revocations >/dev/null 2>&1

echo "--- a TCPRoute: the stream server, endpoints without reloads"
cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: tcp, namespace: $N}
spec:
  gatewayClassName: $class
  listeners: [{name: tcp, port: 9000, protocol: TCP}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: TCPRoute
metadata: {name: tcp, namespace: $N}
spec:
  parentRefs: [{name: tcp}]
  rules: [{backendRefs: [{name: v1, port: 80}]}]
YAML
waited=0
while [ "$waited" -lt 60 ] && [ "$(cond tcproute/tcp '.status.parents[0].conditions[?(@.type=="Accepted")].status')" != "True" ]; do sleep 1; waited=$((waited + 1)); done
sleep 3
tcp=$(cond gateway/tcp '.status.addresses[0].value')
overtcp() { for _ in $(seq 1 "$1"); do X curl -s --max-time 3 "http://$tcp:9000/hostname"; echo; done | grep . | sort -u | tr '\n' ' ' | sed 's/ $//'; }
check "the TCPRoute is accepted" "$(cond tcproute/tcp '.status.parents[0].conditions[?(@.type=="Accepted")].status')" "True"
check "its backend answers through the stream server" "$(overtcp 20)" "$(pods v1)"
settle
r1=$(reloads)
$K -n $N scale deploy/v1 --replicas=3 >/dev/null
$K -n $N rollout status deploy/v1 --timeout=120s >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(overtcp 30)" != "$(pods v1)" ]; do sleep 1; waited=$((waited + 1)); done
check "scaled to 3, all three answer (${waited}s)" "$(overtcp 30)" "$(pods v1)"
check "without a reload" "$(reloads)" "$r1"

echo "--- a TCPRoute with backends in two namespaces, allowed by a ReferenceGrant"
F=$N-far
$K delete ns $F --wait=true >/dev/null 2>&1
$K create ns $F >/dev/null
deploy far 1 | sed "s/namespace: $N\$/namespace: $F/; s/namespace: $N}/namespace: $F}/" | $K apply -f - >/dev/null
cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata: {name: tcproutes, namespace: $F}
spec:
  from: [{group: gateway.networking.k8s.io, kind: TCPRoute, namespace: $N}]
  to: [{group: "", kind: Service}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: tcp2, namespace: $N}
spec:
  gatewayClassName: $class
  listeners: [{name: tcp, port: 9001, protocol: TCP}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: TCPRoute
metadata: {name: tcp2, namespace: $N}
spec:
  parentRefs: [{name: tcp2}]
  rules: [{backendRefs: [{name: v1, port: 80, weight: 1}, {name: far, namespace: $F, port: 80, weight: 1}]}]
YAML
$K -n $F rollout status deploy/far --timeout=120s >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(cond tcproute/tcp2 '.status.parents[0].conditions[?(@.type=="ResolvedRefs")].status')" != "True" ]; do sleep 1; waited=$((waited + 1)); done
tcp2=$(cond gateway/tcp2 '.status.addresses[0].value')
overtcp2() { for _ in $(seq 1 "$1"); do X curl -s --max-time 3 "http://$tcp2:9001/hostname"; echo; done | grep . | sed 's/-[^-]*-[^-]*$//' | sort -u | tr '\n' ' ' | sed 's/ $//'; }
waited=0
while [ "$waited" -lt 60 ] && [ "$(overtcp2 20)" != "far v1" ]; do sleep 1; waited=$((waited + 1)); done
check "the TCPRoute's backends in both namespaces answer (${waited}s)" "$(overtcp2 30)" "far v1"
settle
r8=$(reloads)
$K -n $F scale deploy/far --replicas=2 >/dev/null
$K -n $F rollout status deploy/far --timeout=120s >/dev/null
farpods=$($K -n $F get pod -l app=far --field-selector=status.phase=Running -o name | wc -l | tr -d ' ')
waited=0
while [ "$waited" -lt 60 ] && [ "$(for _ in $(seq 1 30); do X curl -s --max-time 3 "http://$tcp2:9001/hostname"; echo; done | grep '^far-' | sort -u | wc -l | tr -d ' ')" != "$farpods" ]; do sleep 1; waited=$((waited + 1)); done
check "the other namespace's backend scaled to 2, both answer (${waited}s)" "$(for _ in $(seq 1 40); do X curl -s --max-time 3 "http://$tcp2:9001/hostname"; echo; done | grep '^far-' | sort -u | wc -l | tr -d ' ')" "2"
check "without a reload" "$(($(reloads) - r8))" "0"
$K -n $N delete referencegrant --all >/dev/null 2>&1
$K -n $F delete referencegrant tcproutes >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(cond tcproute/tcp2 '.status.parents[0].conditions[?(@.type=="ResolvedRefs")].status')" != "False" ]; do sleep 1; waited=$((waited + 1)); done
sleep 5
check "without the ReferenceGrant only the route's own namespace answers" "$(overtcp2 20)" "v1"

echo "--- a Gateway that goes takes its Service along"
$K -n $N delete gateway gw >/dev/null
waited=0
while [ "$waited" -lt 30 ] && [ -n "$(gwsvc gw .metadata.name)" ]; do sleep 1; waited=$((waited + 1)); done
check "the Service is gone (${waited}s)" "$(gwsvc gw .metadata.name)" ""

[ -n "${KEEP:-}" ] || $K delete ns $N $N-certs $N-far --wait=false >/dev/null 2>&1

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
