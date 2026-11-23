#!/bin/sh
#
# ListenerSets on the default Gateway: a namespace with no access to the
# Gateway's adds an HTTPS listener for its hostname with its own
# certificate, and its route is served from the controller's address with
# that certificate. A namespace the Gateway does not allow is not served,
# and a listener named like one of the Gateway's does not take the
# Gateway's routes off.
#
# Runs where kubectl and the kind node's docker are, against a release of
# charts/wxingress installed as for gateway.sh (default Gateway "shared" in
# gw-e2e):
#
#   RELEASE_NS=wxgw CLASS=wxgw dataplane/e2e/gateway-listenerset.sh
#
# It makes the namespaces gw-e2e (the default Gateway), gw-ls-team
# (labelled) and gw-ls-other (not), and removes them again.
#
#   KUBECTL     how to call kubectl ("sudo kubectl")
#   DOCKER      how to call docker ("sudo docker")
#   NODE        the kind node (wxingress-control-plane)
#   RELEASE_NS  the namespace of the release (wxgw)
#   CLASS       its GatewayClass (wxgw)
#   KEEP=1      leave the namespaces behind

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxgw}
class=${CLASS:-wxgw}
label=wxingress-e2e/listenersets

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
X() { $D exec "$node" "$@"; }
cleanup() {
  [ "${KEEP:-}" = 1 ] && return
  $K delete namespace gw-e2e gw-ls-team gw-ls-other --ignore-not-found --wait=false >/dev/null
}
trap cleanup EXIT

svc=$($K -n "$ns" get svc -l app.kubernetes.io/name=wxingress -o jsonpath='{range .items[*]}{.metadata.name} {.spec.ports[*].port}{"\n"}{end}' | awk '/ 80 443/ {print $1; exit}')
addr=$($K -n "$ns" get svc "$svc" -o jsonpath='{.spec.clusterIP}')
[ -n "$addr" ] || { echo "no Service of the release in $ns"; exit 1; }

tmp=$(mktemp -d)
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=app-ls.example.com \
  -addext subjectAltName=DNS:app-ls.example.com -keyout "$tmp/key" -out "$tmp/crt" 2>/dev/null

$K create namespace gw-e2e >/dev/null 2>&1
$K create namespace gw-ls-team >/dev/null 2>&1
$K create namespace gw-ls-other >/dev/null 2>&1
$K label namespace gw-ls-team "$label=allowed" --overwrite >/dev/null
for n in gw-ls-team gw-ls-other; do
  $K -n "$n" create secret tls app-tls --cert="$tmp/crt" --key="$tmp/key" >/dev/null
done
rm -rf "$tmp"

$K apply -f - >/dev/null <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: shared, namespace: gw-e2e}
spec:
  gatewayClassName: $class
  allowedListeners:
    namespaces:
      from: Selector
      selector: {matchLabels: {$label: allowed}}
  listeners:
  - name: http
    protocol: HTTP
    port: 80
    allowedRoutes: {namespaces: {from: All}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: gw-ls-team}
spec:
  selector: {matchLabels: {app: app}}
  template:
    metadata: {labels: {app: app}}
    spec:
      containers:
      - name: app
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: [netexec, --http-port=8080]
---
apiVersion: v1
kind: Service
metadata: {name: app, namespace: gw-ls-team}
spec:
  selector: {app: app}
  ports: [{port: 80, targetPort: 8080}]
---
# its listener has the name of the Gateway's listener
apiVersion: gateway.networking.k8s.io/v1
kind: ListenerSet
metadata: {name: app, namespace: gw-ls-team}
spec:
  parentRef: {name: shared, namespace: gw-e2e}
  listeners:
  - name: http
    hostname: app-ls.example.com
    protocol: HTTPS
    port: 443
    tls: {mode: Terminate, certificateRefs: [{name: app-tls}]}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: app, namespace: gw-ls-team}
spec:
  parentRefs: [{group: gateway.networking.k8s.io, kind: ListenerSet, name: app, sectionName: http}]
  hostnames: [app-ls.example.com]
  rules: [{backendRefs: [{name: app, port: 80}]}]
---
# a route on the Gateway itself, with no sectionName
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: plain, namespace: gw-ls-team}
spec:
  parentRefs: [{name: shared, namespace: gw-e2e}]
  hostnames: [plain-ls.example.com]
  rules: [{backendRefs: [{name: app, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: ListenerSet
metadata: {name: other, namespace: gw-ls-other}
spec:
  parentRef: {name: shared, namespace: gw-e2e}
  listeners:
  - name: https
    hostname: other-ls.example.com
    protocol: HTTPS
    port: 443
    tls: {mode: Terminate, certificateRefs: [{name: app-tls}]}
EOF
$K -n gw-ls-team rollout status deploy/app --timeout=120s >/dev/null

cond() { $K -n "$1" get listenerset "$2" -o jsonpath="{.status.conditions[?(@.type==\"$3\")].reason}"; }
https() { X curl -sk --max-time 3 -o /dev/null -w '%{http_code}' --resolve "$1:443:$addr" "https://$1/hostname"; }
plain() { X curl -s --max-time 3 -o /dev/null -w '%{http_code}' -H "Host: $1" "http://$addr/hostname"; }
routeAccepted() { $K -n gw-ls-team get httproute "$1" -o jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}'; }
# waitfor <want> <command...>: until the command prints want, at most 60s
waitfor() {
  want=$1
  shift
  waited=0
  while [ "$waited" -lt 60 ] && [ "$("$@")" != "$want" ]; do sleep 1; waited=$((waited + 1)); done
}
waitfor 200 https app-ls.example.com
waitfor 200 plain plain-ls.example.com
waitfor True routeAccepted app
waitfor NotAllowed cond gw-ls-other other Accepted

check "the ListenerSet of an allowed namespace is accepted" "$(cond gw-ls-team app Accepted)" "Accepted"
check "and programmed" "$(cond gw-ls-team app Programmed)" "Programmed"
check "its route is accepted" "$(routeAccepted app)" "True"
check "the Gateway counts it" "$($K -n gw-e2e get gateway shared -o jsonpath='{.status.attachedListenerSets}')" "1"
check "the Gateway does not list its listener" \
  "$($K -n gw-e2e get gateway shared -o jsonpath='{.status.listeners[*].name}')" "http"
check "its hostname answers over HTTPS on the controller's address" "$(https app-ls.example.com)" "200"
check "with its certificate" \
  "$(X sh -c "echo | openssl s_client -connect $addr:443 -servername app-ls.example.com 2>/dev/null | openssl x509 -noout -subject" | sed 's/.*CN *= *//')" \
  "app-ls.example.com"
check "a route of the Gateway stays served next to a listener with its name" "$(plain plain-ls.example.com)" "200"
check "and accepted" "$(routeAccepted plain)" "True"
check "the ListenerSet of a namespace not allowed is not accepted" "$(cond gw-ls-other other Accepted)" "NotAllowed"
check "and its hostname not served" "$(https other-ls.example.com)" "404"

echo
[ "$failed" -eq 0 ] && echo "all passed" || echo "$failed failed"
[ "$failed" -eq 0 ]
