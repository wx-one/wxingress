#!/bin/sh
#
# The Gateway API in the watched namespaces only: with -watch-namespace-label
# a Gateway and its HTTPRoute in a namespace without the label are not
# served - no status, no Service - and are once the namespace gets the label,
# and are no longer when it loses it again.
#
# Runs where kubectl and the kind node's docker are, against a release of
# charts/wxingress installed as for gateway.sh and with
#
#   --set controller.watchNamespaceLabel=wxingress-e2e=watched
#
#   RELEASE_NS=wxgw CLASS=wxgw dataplane/e2e/gateway-namespaces.sh
#
# It makes the namespaces gw-ns-on (labelled) and gw-ns-off (not), each
# with an agnhost Deployment, its Service, a Gateway and an HTTPRoute, and
# removes them again.
#
#   KUBECTL     how to call kubectl ("sudo kubectl")
#   DOCKER      how to call docker ("sudo docker")
#   NODE        the kind node (wxingress-control-plane)
#   RELEASE_NS  the namespace of the release (wxgw)
#   CLASS       its GatewayClass (wxgw)
#   LABEL       the label the release watches (wxingress-e2e=watched)

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxgw}
class=${CLASS:-wxgw}
label=${LABEL:-wxingress-e2e=watched}

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
accepted() { $K -n "$1" get gateway gw -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}'; }
# the Gateway's Service is in its own namespace (since c602a60bb), not the release's
service() { $K -n "$1" get svc -l "gateway.networking.k8s.io/gateway-name=gw" -o jsonpath='{.items[*].metadata.name}'; }
slices() { $K -n "$1" get endpointslices -l "endpointslice.kubernetes.io/managed-by=gateway.wxingress.wizardtales.com" -o jsonpath='{.items[*].metadata.name}'; }
answer() {
  a=$($K -n "$1" get gateway gw -o jsonpath='{.status.addresses[0].value}')
  [ -n "$a" ] || { echo none; return; }
  X curl -s --max-time 3 -o /dev/null -w '%{http_code}' -H "Host: app.example.com" "http://$a/hostname"
}
waitfor() {
  waited=0
  while [ "$waited" -lt 60 ] && [ "$($1 "$2")" != "$3" ]; do sleep 1; waited=$((waited + 1)); done
}

for n in gw-ns-on gw-ns-off; do
  $K delete ns $n --wait=true >/dev/null 2>&1
  $K create ns $n >/dev/null
  cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: $n}
spec:
  replicas: 1
  selector: {matchLabels: {app: app}}
  template:
    metadata: {labels: {app: app}}
    spec:
      containers:
      - name: echo
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: [netexec, --http-port=8080]
        readinessProbe: {httpGet: {path: /healthz, port: 8080}, periodSeconds: 1}
---
apiVersion: v1
kind: Service
metadata: {name: app, namespace: $n}
spec: {selector: {app: app}, ports: [{port: 80, targetPort: 8080}]}
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: gw, namespace: $n}
spec:
  gatewayClassName: $class
  listeners: [{name: http, port: 80, protocol: HTTP}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: app, namespace: $n}
spec:
  parentRefs: [{name: gw}]
  hostnames: [app.example.com]
  rules: [{backendRefs: [{name: app, port: 80}]}]
YAML
done
$K label ns gw-ns-on "$label" >/dev/null
$K -n gw-ns-on rollout status deploy/app --timeout=120s >/dev/null
$K -n gw-ns-off rollout status deploy/app --timeout=120s >/dev/null

waitfor answer gw-ns-on 200
check "the Gateway of the watched namespace is accepted" "$(accepted gw-ns-on)" "True"
check "and has its Service" "$([ -n "$(service gw-ns-on)" ] && echo yes)" "yes"
check "and serves its route" "$(answer gw-ns-on)" "200"
sleep 5
check "the Gateway of the other namespace is untouched (the API server's Unknown)" "$(accepted gw-ns-off)" "Unknown"
check "and no Service" "$(service gw-ns-off)" ""

$K label ns gw-ns-off "$label" >/dev/null
waitfor answer gw-ns-off 200
check "once it has the label, it is accepted" "$(accepted gw-ns-off)" "True"
check "and serves its route" "$(answer gw-ns-off)" "200"

$K label ns gw-ns-off "${label%%=*}-" >/dev/null
waitfor service gw-ns-off ""
check "without the label again its Service goes" "$(service gw-ns-off)" ""
waitfor slices gw-ns-off ""
check "and its EndpointSlices with it" "$(slices gw-ns-off)" ""
check "and the other Gateway still serves" "$(answer gw-ns-on)" "200"

$K delete ns gw-ns-on gw-ns-off --wait=false >/dev/null

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
