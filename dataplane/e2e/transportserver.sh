#!/bin/sh
#
# A TransportServer on kind whose endpoints change: none of it may reload
# nginx with -dynamic-upstreams.
#
# Runs where kubectl and the kind node's docker are, against a release of
# charts/wxingress installed with -dynamic-upstreams, Prometheus metrics and
# a GlobalConfiguration with a TCP listener:
#
#   helm install wxts charts/wxingress -n wxts --create-namespace \
#     --set controller.dynamicUpstreams=true \
#     --set controller.ingressClass.name=wxts \
#     --set controller.globalConfiguration.create=true \
#     --set 'controller.globalConfiguration.spec.listeners[0].name=tcp-e2e' \
#     --set 'controller.globalConfiguration.spec.listeners[0].port=5000' \
#     --set 'controller.globalConfiguration.spec.listeners[0].protocol=TCP'
#
#   RELEASE_NS=wxts CLASS=wxts dataplane/e2e/transportserver.sh
#
# It makes the namespace ts-e2e with a Deployment of agnhost's netexec, its
# Service and a TransportServer for it, proxies TCP to it through the
# listener from inside the kind node - a connection of its own for every
# request - and scales, restarts and changes it. The reloads are nginx's own
# count (nginx_ingress_controller_nginx_reloads_total) and the pids of its
# master and workers.
#
#   KUBECTL     how to call kubectl ("sudo kubectl")
#   DOCKER      how to call docker ("sudo docker")
#   NODE        the kind node (wxingress-control-plane)
#   RELEASE_NS  the namespace of the release (wxts)
#   CLASS       its ingress class (wxts)
#   LISTENER    the TCP listener of its GlobalConfiguration (tcp-e2e)
#   PORT        and its port (5000)
#   KEEP=1      leave the namespace ts-e2e behind

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxts}
class=${CLASS:-wxts}
listener=${LISTENER:-tcp-e2e}
port=${PORT:-5000}
N=ts-e2e
upstream=ts_${N}_tcp_app

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

pod=$($K -n "$ns" get pod --field-selector=status.phase=Running -o name | head -1)
podip=$($K -n "$ns" get "$pod" -o jsonpath='{.status.podIP}')
[ -n "$podip" ] || { echo "no running controller in $ns"; exit 2; }

X() { $D exec "$node" "$@"; }
reloads() { X curl -s "http://$podip:9113/metrics" | grep '^nginx_ingress_controller_nginx_reloads_total' | awk '{s+=$2} END {print s+0}'; }
# the pids of nginx's master and workers, as a count of each and a checksum of all
nginxes() {
  list=$($K -n "$ns" exec "$pod" -- sh -c 'for p in /proc/[0-9]*; do c=$(tr "\0" " " < $p/cmdline 2>/dev/null); case "$c" in nginx:*) echo "${p#/proc/}:${c#nginx: }";; esac; done' | sort)
  echo "$(echo "$list" | grep -c master) master, $(echo "$list" | grep -c worker) workers, $(echo "$list" | cksum | cut -d' ' -f1)"
}
# the names of the pods that answer `count` connections, sorted
through() {
  for _ in $(seq 1 "$1"); do
    X curl -s --max-time 3 "http://$podip:$port/hostname"
    echo
  done | grep . | sort -u | tr '\n' ' ' | sed 's/ $//'
}
pods() { $K -n $N get pod -l app=tcp --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' | tr ' ' '\n' | grep . | sort | tr '\n' ' ' | sed 's/ $//'; }
# until the connections reach exactly the running pods, at most $1 seconds; how long it took
converged() {
  start=$(date +%s)
  while [ $(($(date +%s) - start)) -lt "$1" ]; do
    want=$(pods)
    [ -n "$want" ] && [ "$(through 30)" = "$want" ] && { echo "$(($(date +%s) - start))s"; return; }
    sleep 1
  done
  echo "timeout"
}
# the image has no curl; perl-base is Debian's and speaks to the socket, with
# the controller's token from the file the API's server includes
api() {
  $K -n "$ns" exec "$pod" -- perl -MIO::Socket::UNIX -e '
    my $t = "";
    if (open(my $f, "<", "/etc/nginx/secrets/_wxingress-api-token.conf")) { local $/; ($t) = <$f> =~ /"([0-9a-f]+)"/; }
    my $s = IO::Socket::UNIX->new(Peer => "/var/lib/nginx/nginx-plus-api.sock") or die "$!\n";
    print $s "GET $ARGV[0] HTTP/1.0\r\nHost: api\r\nX-Wxingress-Api-Token: $t\r\n\r\n";
    my $r = join("", <$s>); $r =~ s/^.*?\r\n\r\n//s; print $r' "$1"
}
servers() { api "/api/9/stream/upstreams/$upstream/servers" | tr '{' '\n' | sed -n 's/.*"server":"\([^"]*\)".*"max_fails":\([0-9]*\),"fail_timeout":"\([^"]*\)".*/\1 \2 \3/p' | sort; }
endpoints() { $K -n $N get endpointslice -l kubernetes.io/service-name=tcp -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.addresses[0]}{":8080 2 20s\n"}{end}' | sort; }

$K delete ns $N --wait=true >/dev/null 2>&1
$K create ns $N >/dev/null
cat <<YAML | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: tcp, namespace: $N}
spec:
  replicas: 2
  selector: {matchLabels: {app: tcp}}
  template:
    metadata: {labels: {app: tcp}}
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
metadata: {name: tcp, namespace: $N}
spec: {selector: {app: tcp}, ports: [{port: 8080, targetPort: 8080}]}
---
apiVersion: k8s.nginx.org/v1
kind: TransportServer
metadata: {name: tcp, namespace: $N}
spec:
  ingressClassName: $class
  listener: {name: $listener, protocol: TCP}
  upstreams:
  - {name: app, service: tcp, port: 8080, maxFails: 2, failTimeout: 20s}
  action: {pass: app}
YAML
$K -n $N rollout status deploy/tcp --timeout=120s >/dev/null

echo "--- set up (reloads expected here)"
state() { $K -n $N get ts tcp -o jsonpath='{.status.state}'; }
waited=0
while [ "$waited" -lt 30 ] && [ -z "$(state)" ]; do sleep 1; waited=$((waited + 1)); done
check "the TransportServer is valid" "$(state)" "Valid"
check "both pods answer through the listener ($(converged 60))" "$(through 30)" "$(pods)"
check "the module has the endpoints, with the upstream's parameters" "$(servers)" "$(endpoints)"

r0=$(reloads)
p0=$(nginxes)
echo "         reloads so far: $r0; nginx: $p0"

echo "--- endpoints without reloads"
$K -n $N scale deploy/tcp --replicas=4 >/dev/null
$K -n $N rollout status deploy/tcp --timeout=120s >/dev/null
t=$(converged 60)
check "scaled to 4, all four answer ($t)" "$(through 40 | wc -w | tr -d ' ')" "4"
check "and the module has them" "$(servers)" "$(endpoints)"

$K -n $N scale deploy/tcp --replicas=1 >/dev/null
sleep 8
t=$(converged 60)
check "scaled to 1, only it answers ($t)" "$(through 20)" "$(pods)"

$K -n $N rollout restart deploy/tcp >/dev/null
$K -n $N rollout status deploy/tcp --timeout=120s >/dev/null
sleep 8
t=$(converged 60)
check "restarted, the new pod answers ($t)" "$(through 20)" "$(pods)"
check "and the module has its address" "$(servers)" "$(endpoints)"

$K -n $N scale deploy/tcp --replicas=0 >/dev/null
sleep 8
check "no pods: the module has no servers" "$(servers)" ""
check "and a connection gets nothing" "$(through 3)" ""
$K -n $N scale deploy/tcp --replicas=2 >/dev/null
$K -n $N rollout status deploy/tcp --timeout=120s >/dev/null
check "and two pods again ($(converged 60))" "$(through 30)" "$(pods)"

# a parameter of the servers is in their server lines: the module changes it
$K -n $N patch ts tcp --type json -p '[{"op":"replace","path":"/spec/upstreams/0/failTimeout","value":"30s"}]' >/dev/null
sleep 3
check "a changed fail_timeout goes to the module" "$(servers | awk '{print $3}' | sort -u)" "30s"

sleep 2
check "none of it reloaded nginx: the count" "$(reloads)" "$r0"
check "and the same master and workers" "$(nginxes)" "$p0"

echo "--- a change of the config reloads"
$K -n $N patch ts tcp --type json -p '[{"op":"add","path":"/spec/upstreams/0/loadBalancingMethod","value":"least_conn"}]' >/dev/null
sleep 5
check "least_conn is one reload" "$(reloads)" "$((r0 + 1))"
check "and the pods still answer" "$(through 30)" "$(pods)"

[ -n "${KEEP:-}" ] || $K delete ns $N --wait=false >/dev/null

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
