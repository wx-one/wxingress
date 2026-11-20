#!/bin/sh
#
# Dynamic certificates on kind: nothing of it may reload nginx, and every
# name must be given its own certificate.
#
# Runs where kubectl and the kind node's docker are, against a release of
# charts/wxingress installed with -dynamic-upstreams and no default server
# Secret, so that a name without a certificate gets ingress-nginx's fake one
# (see transportserver.sh for such an install). dataplane/e2e/certs is the
# client, run inside the node; build it where Go is and name it in HELPER:
#
#   CGO_ENABLED=0 go build -o certs-e2e ./dataplane/e2e/certs
#   HELPER=./certs-e2e RELEASE_NS=wxts CLASS=wxts dataplane/e2e/certificates.sh
#
# It makes the namespace certs-e2e with an echo Deployment, Ingresses and TLS
# Secrets, changes, deletes and rotates them, MANY of them in the end, and
# checks after each change which certificate a handshake with each name
# gets. A name that gets another certificate than its own is a failure, and
# any failure makes the run exit 1.
#
#   KUBECTL     how to call kubectl ("sudo kubectl")
#   DOCKER      how to call docker ("sudo docker")
#   NODE        the kind node (wxingress-control-plane)
#   RELEASE_NS  the namespace of the release (wxts)
#   CLASS       its ingress class (wxts)
#   HELPER      the certs binary to copy into the node as wxingress-certs;
#               without it the one copied there before is used
#   MANY        how many Secrets and hosts the bulk part has (1000)
#   KEEP=1      leave the namespace certs-e2e behind

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxts}
class=${CLASS:-wxts}
N=certs-e2e
FAKE="Kubernetes Ingress Controller Fake Certificate/Acme Co"

if [ -n "${HELPER:-}" ]; then
  $D cp "$HELPER" "$node:/usr/local/bin/wxingress-certs" || exit 2
fi

pod=$($K -n "$ns" get pod --field-selector=status.phase=Running -o name | head -1)
podip=$($K -n "$ns" get "$pod" -o jsonpath='{.status.podIP}')
[ -n "$podip" ] || { echo "no running controller in $ns"; exit 2; }
addr=$podip:443

T() { $D exec "$node" wxingress-certs "$@"; }
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
# every name of the bulk part has its own certificate: each "wrong:" line of
# certs bulk is a name that did not, and counts as a failure
bulk() {
  what=$1
  shift
  out=$(T bulk "$@")
  status=$?
  echo "$out" | sed -n '/^wrong: /p' | head -10 | sed 's/^/         /'
  echo "$out" | grep -v '^wrong: ' | sed 's/^/         /'
  wrong=$(echo "$out" | grep -c '^wrong: ')
  check "$what: names with another certificate than their own" "$wrong" "0"
  check "$what: and certs bulk says so" "$status" "0"
}

# nginx master and workers, from /proc of the controller's pod
nginxes() {
  list=$($K -n "$ns" exec "$pod" -- sh -c 'for p in /proc/[0-9]*; do c=$(tr "\0" " " < $p/cmdline 2>/dev/null); case "$c" in nginx:*) echo "${p#/proc/}:${c#nginx: }";; esac; done' | sort)
  echo "$(echo "$list" | grep -c master) master, $(echo "$list" | grep -c worker) workers, $(echo "$list" | cksum | cut -d' ' -f1)"
}
reloads() { $D exec "$node" curl -s "http://$podip:9113/metrics" | grep '^nginx_ingress_controller_nginx_reloads_total' | awk '{s+=$2} END {print s+0}'; }
# until the reloads the set up asked for are done: the count the same for 5 s
settle() {
  last=$(reloads)
  same=0
  waited=0
  while [ "$same" -lt 5 ] && [ "$waited" -lt 60 ]; do
    sleep 1
    now=$(reloads)
    if [ "$now" = "$last" ]; then same=$((same + 1)); else same=0; last=$now; fi
    waited=$((waited + 1))
  done
}
apply() { $K apply -f - >/dev/null; }

$K delete ns $N --wait=true >/dev/null 2>&1
$K create ns $N >/dev/null
cat <<YAML | apply
apiVersion: apps/v1
kind: Deployment
metadata: {name: echo, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: echo}}
  template:
    metadata: {labels: {app: echo}}
    spec:
      containers:
      - {name: echo, image: registry.k8s.io/e2e-test-images/agnhost:2.53, args: [netexec, --http-port=8080], ports: [{containerPort: 8080}]}
---
apiVersion: v1
kind: Service
metadata: {name: echo, namespace: $N}
spec: {selector: {app: echo}, ports: [{port: 80, targetPort: 8080}]}
YAML
$K -n $N rollout status deploy/echo --timeout=120s >/dev/null

ingress() { # name host secret
cat <<YAML | apply
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: $1, namespace: $N}
spec:
  ingressClassName: $class
  tls: [{hosts: ["$2"], secretName: $3}]
  rules:
  - host: "$2"
    http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: echo, port: {number: 80}}}}]}
YAML
}

echo "--- set up (reloads expected here)"
T secret $N a-tls a.certs.test one | apply
ingress a a.certs.test a-tls
ingress b b.certs.test b-tls
T secret $N w-tls '*.w.certs.test' wild | apply
ingress w '*.w.certs.test' w-tls
T secret $N other-tls a.certs.test other | apply
t=$(T wait "$addr" a.certs.test a.certs.test/one 60)
T wait "$addr" x.w.certs.test '*.w.certs.test/wild' 60 >/dev/null
settle
check "a.certs.test has its certificate ($t)" "$(T probe "$addr" a.certs.test)" "a.certs.test/one"
check "and serves the Ingress" "$($D exec "$node" curl -sk -o /dev/null -w '%{http_code}' --resolve "a.certs.test:443:$podip" https://a.certs.test/hostname)" "200"
check "an unknown name has the fake certificate" "$(T probe "$addr" nobody.certs.test)" "$FAKE"
check "so does no name" "$(T probe "$addr" '')" "$FAKE"
check "a host whose Secret is missing has the fake one, not a refused handshake" "$(T probe "$addr" b.certs.test)" "$FAKE"
check "a wildcard host's certificate one level below" "$(T probe "$addr" x.w.certs.test)" "*.w.certs.test/wild"
check "not two levels below" "$(T probe "$addr" y.x.w.certs.test)" "$FAKE"

p0=$(nginxes)
r0=$(reloads)
echo "         reloads so far: $r0; nginx: $p0"

echo "--- changes without reloads"
T secret $N a-tls a.certs.test two | apply
t=$(T wait "$addr" a.certs.test a.certs.test/two 30)
check "a changed Secret ($t)" "$(T probe "$addr" a.certs.test)" "a.certs.test/two"
$K -n $N delete secret a-tls >/dev/null
t=$(T wait "$addr" a.certs.test "$FAKE" 30)
check "a deleted Secret: the fake certificate ($t)" "$(T probe "$addr" a.certs.test)" "$FAKE"
T secret $N a-tls a.certs.test three | apply
t=$(T wait "$addr" a.certs.test a.certs.test/three 30)
check "a Secret created again ($t)" "$(T probe "$addr" a.certs.test)" "a.certs.test/three"
T secret $N b-tls b.certs.test bee | apply
t=$(T wait "$addr" b.certs.test b.certs.test/bee 30)
check "the missing Secret of an Ingress created ($t)" "$(T probe "$addr" b.certs.test)" "b.certs.test/bee"
T secret $N w-tls '*.w.certs.test' wild2 | apply
t=$(T wait "$addr" z.w.certs.test '*.w.certs.test/wild2' 30)
check "a changed wildcard Secret ($t)" "$(T probe "$addr" z.w.certs.test)" "*.w.certs.test/wild2"
$K -n $N patch ingress a --type merge -p '{"spec":{"tls":[{"hosts":["a.certs.test"],"secretName":"other-tls"}]}}' >/dev/null
t=$(T wait "$addr" a.certs.test a.certs.test/other 30)
check "an Ingress that names another Secret ($t)" "$(T probe "$addr" a.certs.test)" "a.certs.test/other"
sleep 2
check "none of it reloaded nginx: the count" "$(reloads)" "$r0"
check "and the same master and workers" "$(nginxes)" "$p0"

n=${MANY:-1000}
echo "--- $n Secrets and an Ingress with $n hosts"
T many $N "$n" many ingress "$class" | $K apply -f - >/dev/null
t=$(T wait "$addr" "m$((n - 1)).many.certs.test" "m$((n - 1)).many.certs.test/many" 300)
echo "         the last one served after $t"
bulk "$n hosts" "$addr" "$n" many 2

settle
p1=$(nginxes)
r1=$(reloads)
echo "--- rotate all $n Secrets"
T many $N "$n" many2 | $K apply -f - >/dev/null
t=$(T wait "$addr" "m$((n - 1)).many.certs.test" "m$((n - 1)).many.certs.test/many2" 300)
echo "         all rotated, the last one served after $t"
bulk "rotated" "$addr" "$n" many2 0
check "rotating $n Secrets reloaded nothing: the count" "$(reloads)" "$r1"
check "and the same master and workers" "$(nginxes)" "$p1"

[ -n "${KEEP:-}" ] || $K delete ns $N --wait=false >/dev/null

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
