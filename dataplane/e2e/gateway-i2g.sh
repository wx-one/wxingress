#!/bin/sh
#
# ingress2gateway as the oracle of the Gateway API: Ingresses as one would
# have them for ingress-nginx, served by wxingress as Ingresses, are
# converted by ingress2gateway (provider ingress-nginx) into a Gateway and
# HTTPRoutes, which wxingress serves as well - on the Gateway's own address.
# Every request must get the same answer from both: status, redirect
# target, the backend (v1 or v2) and the path and Host header the backend
# saw.
#
# Runs where kubectl, the kind node's docker and ingress2gateway are,
# against a release of charts/wxingress installed as for gateway.sh:
#
#   (cd ingress2gateway && go build -o /tmp/ingress2gateway .)   # v1.2.0
#   I2G=/tmp/ingress2gateway RELEASE_NS=wxgw CLASS=wxgw dataplane/e2e/gateway-i2g.sh
#
# with the release installed with --set controller.ingressNginxDefaults=true.
#
# The Ingresses compared request by request are what ingress2gateway
# converts and both sides serve: hosts, Prefix and Exact paths, Ingresses
# that share a host, canary by header, permanent-redirect, TLS with its
# redirect from HTTP (308, as ingress-nginx, hence -ingress-nginx-defaults).
# Two more Ingresses are the known differences, checked as such so that a
# change in either shows (docs/developer/upstream.md): rewrite-target and
# upstream-vhost, which ingress2gateway converts into a case-insensitive regex
# path and a Host header modifier that NGF's validators reject. It makes the
# namespace gw-i2g and removes it.
#
#   KUBECTL     how to call kubectl ("sudo kubectl")
#   DOCKER      how to call docker ("sudo docker")
#   NODE        the kind node (wxingress-control-plane)
#   RELEASE_NS  the namespace of the release (wxgw)
#   CLASS       its ingress class, and the GatewayClass of the same name (wxgw)
#   I2G         the ingress2gateway binary (ingress2gateway)
#   KEEP=1      leave the namespace gw-i2g behind

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxgw}
class=${CLASS:-wxgw}
i2g=${I2G:-ingress2gateway}
N=gw-i2g

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

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

$K delete ns $N --wait=true >/dev/null 2>&1
$K create ns $N >/dev/null

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=secure.example.com" \
  -addext "subjectAltName=DNS:secure.example.com" -keyout "$work/tls.key" -out "$work/tls.crt" 2>/dev/null
$K -n $N create secret tls secure --cert "$work/tls.crt" --key "$work/tls.key" >/dev/null

backend() {
  cat <<YAML
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $1, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: $1}}
  template:
    metadata: {labels: {app: $1}}
    spec:
      containers:
      - name: echo
        image: registry.k8s.io/gateway-api/echo-basic:v1.5.1
        env:
        - {name: POD_NAME, valueFrom: {fieldRef: {fieldPath: metadata.name}}}
        - {name: NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}
        readinessProbe: {httpGet: {path: /, port: 3000}, periodSeconds: 1}
---
apiVersion: v1
kind: Service
metadata: {name: $1, namespace: $N}
spec: {selector: {app: $1}, ports: [{port: 80, targetPort: 3000}]}
YAML
}
{ backend v1; backend v2; } | $K apply -f - >/dev/null

ingress() {
  # name host path pathType service [annotations as "k: v" lines]
  name=$1 host=$2 path=$3 type=$4 svc=$5
  shift 5
  echo "---"
  echo "apiVersion: networking.k8s.io/v1"
  echo "kind: Ingress"
  echo "metadata:"
  echo "  name: $name"
  echo "  namespace: $N"
  if [ $# -gt 0 ]; then
    echo "  annotations:"
    for a in "$@"; do echo "    $a"; done
  fi
  echo "spec:"
  echo "  ingressClassName: $class"
  echo "  rules:"
  echo "  - host: $host"
  echo "    http:"
  echo "      paths:"
  echo "      - {path: \"$path\", pathType: $type, backend: {service: {name: $svc, port: {number: 80}}}}"
}
{
  ingress app app.example.com / Prefix v1
  ingress api app.example.com /api Prefix v2
  ingress exact app.example.com /exact Exact v2
  ingress canary-main canary.example.com / Prefix v1
  ingress canary canary.example.com / Prefix v2 \
    'nginx.ingress.kubernetes.io/canary: "true"' \
    'nginx.ingress.kubernetes.io/canary-by-header: X-Canary' \
    'nginx.ingress.kubernetes.io/canary-by-header-value: always'
  ingress rewrite rw.example.com /old Exact v1 'nginx.ingress.kubernetes.io/rewrite-target: /new'
  ingress redirect redir.example.com / Prefix v1 'nginx.ingress.kubernetes.io/permanent-redirect: https://example.org/target'
  ingress vhost vhost.example.com / Prefix v2 'nginx.ingress.kubernetes.io/upstream-vhost: internal.example'
  ingress secure secure.example.com / Prefix v1
  cat <<YAML
  tls:
  - {hosts: [secure.example.com], secretName: secure}
YAML
} > "$work/ingresses.yaml"
$K apply -f "$work/ingresses.yaml" >/dev/null

# the oracle: the same Ingresses as Gateway API resources
if ! "$i2g" print --providers=ingress-nginx --ingress-nginx-ingress-class="$class" -n $N \
  --input-file "$work/ingresses.yaml" --no-color >"$work/gateway.yaml" 2>"$work/i2g.log"; then
  echo "FAIL     ingress2gateway"
  cat "$work/i2g.log"
  exit 1
fi
$K apply -f "$work/gateway.yaml" >/dev/null || { echo "FAIL     the converted resources do not apply"; cat "$work/gateway.yaml"; exit 1; }
gw=$($K -n $N get gateway -o jsonpath='{.items[0].metadata.name}')
routes=$($K -n $N get httproute -o name | wc -l | tr -d ' ')
echo "         ingress2gateway made Gateway $gw and $routes HTTPRoutes"

$K -n $N rollout status deploy/v1 --timeout=120s >/dev/null
$K -n $N rollout status deploy/v2 --timeout=120s >/dev/null
waited=0
while [ "$waited" -lt 90 ] && [ "$($K -n $N get gateway "$gw" -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}')" != "True" ]; do
  sleep 1
  waited=$((waited + 1))
done
sleep 5
check "the Gateway is programmed" "$($K -n $N get gateway "$gw" -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}')" "True"
for r in $($K -n $N get httproute -o name | grep -v -e rewrite-rw -e vhost-vhost); do
  check "$r is accepted" "$($K -n $N get "$r" -o jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}')" "True"
done

ingressAddr=$($K -n "$ns" get svc -l app.kubernetes.io/instance="$ns" -o jsonpath='{.items[0].spec.clusterIP}')
gatewayAddr=$($K -n $N get gateway "$gw" -o jsonpath='{.status.addresses[0].value}')

# answer <address> <scheme> <host> <path> [curl args]: status, Location,
# backend, the path and host the backend saw
answer() {
  addr=$1 scheme=$2 host=$3 path=$4
  shift 4
  port=80
  [ "$scheme" = https ] && port=443
  out=$(X curl -sk --max-time 3 -o - -w '\n%{http_code} %{redirect_url}' --resolve "$host:$port:$addr" "$@" "$scheme://$host$path")
  status=$(echo "$out" | tail -1)
  body=$(echo "$out" | sed '$d' | tr -d ' \n')
  pod=$(echo "$body" | sed -n 's/.*"pod":"\([^"-]*\)-.*/\1/p')
  seen=$(echo "$body" | sed -n 's/.*"path":"\([^"]*\)".*/\1/p')
  vhost=$(echo "$body" | sed -n 's/.*"host":"\([^"]*\)".*/\1/p')
  # a redirect to the scheme's own port names it or not: the same URL
  echo "$status ${pod:--} ${seen:--} ${vhost:--}" | sed 's#^\([0-9]* https://[^/:]*\):443/#\1/#; s#^\([0-9]* http://[^/:]*\):80/#\1/#' | tr -s ' '

}
same() {
  name=$1
  shift
  viaIngress=$(answer "$ingressAddr" "$@")
  viaGateway=$(answer "$gatewayAddr" "$@")
  check "$name: as an Ingress ($viaIngress)" "$viaGateway" "$viaIngress"
}

same "app /" http app.example.com /
same "app /api/x" http app.example.com /api/x
same "app /exact" http app.example.com /exact
same "app /exact/sub" http app.example.com /exact/sub
same "app /apix" http app.example.com /apix
same "canary without the header" http canary.example.com /
same "canary with the header" http canary.example.com / -H 'X-Canary: always'
same "TLS over HTTPS" https secure.example.com /x
same "TLS over HTTP" http secure.example.com /x
same "an unknown host" http unknown.example.com /
same "permanent-redirect" http redir.example.com /x

echo "--- the known differences"
rejected() { $K -n $N get httproute "$1" -o jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].reason}'; }
check "rewrite-target: the case-insensitive regex path is not accepted by NGF's validator" "$(rejected rewrite-rw-example-com)" "UnsupportedValue"
check "upstream-vhost: the Host header modifier is not accepted by NGF's validator" "$(rejected vhost-vhost-example-com)" "UnsupportedValue"

[ -n "${KEEP:-}" ] || $K delete ns $N --wait=false >/dev/null

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
