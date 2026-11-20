#!/bin/sh
#
# ingress-nginx's tcp and udp services and its default backend: the
# ConfigMaps of -tcp-services-configmap and -udp-services-configmap (with the
# PROXY protocol both ways), -default-backend-service for unknown hosts, paths
# no Ingress has and custom-http-errors without a default-backend annotation,
# and endpoints of all of them changing without a reload.
#
# Runs where kubectl and the kind node's docker are, against a release of
# charts/wxingress installed as for gateway.sh with -dynamic-upstreams and
#
#   --set controller.tcpServicesConfigMap=svc-e2e/tcp
#   --set controller.udpServicesConfigMap=svc-e2e/udp
#   --set controller.defaultBackendService=svc-e2e/fallback
#
# It makes the namespace svc-e2e (whose ConfigMaps and Services those name)
# and removes it.
#
#   KUBECTL     how to call kubectl ("sudo kubectl")
#   DOCKER      how to call docker ("sudo docker")
#   NODE        the kind node (wxingress-control-plane)
#   RELEASE_NS  the namespace of the release (wxgw)
#   CLASS       the IngressClass of the release (wxgw)

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxgw}
class=${CLASS:-wxgw}
N=svc-e2e

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
# the pod of the release, not one a rollout is still taking down
w=0
while [ "$w" -lt 120 ] && [ "$($K -n "$ns" get pod -l app.kubernetes.io/instance="$ns" -o name | wc -l)" -gt 1 ]; do sleep 1; w=$((w + 1)); done
pod=$($K -n "$ns" get pod -l app.kubernetes.io/instance="$ns" -o name | head -1)
podip=$($K -n "$ns" get "$pod" -o jsonpath='{.status.podIP}')
ctrl=$($K -n "$ns" get svc -l app.kubernetes.io/instance="$ns" -o jsonpath='{.items[0].spec.clusterIP}')
reloads() { X curl -s "http://$podip:9113/metrics" | grep '^nginx_ingress_controller_nginx_reloads_total' | awk '{s+=$2} END {print s+0}'; }
# settle waits until nginx has not reloaded for six seconds
settle() {
  last=$(reloads) quiet=0 w=0
  while [ "$quiet" -lt 6 ] && [ "$w" -lt 90 ]; do
    sleep 1; w=$((w + 1)); now=$(reloads)
    if [ "$now" = "$last" ]; then quiet=$((quiet + 1)); else quiet=0 last=$now; fi
  done
}
body() { X curl -s --max-time 3 -H "Host: $1" "http://$ctrl$2"; }
# udp <port> <message>: the answer of the UDP service on the controller's pod
udp() { $K -n $N exec client -- sh -c "echo $2 | nc -u -w 2 $podip $1" 2>/dev/null; }

$K delete ns $N --wait=true >/dev/null 2>&1
$K create ns $N >/dev/null
{
  for app in echo fallback; do
    cat <<YAML
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $app, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: $app}}
  template:
    metadata: {labels: {app: $app}}
    spec:
      terminationGracePeriodSeconds: 2
      containers:
      - name: echo
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: [netexec, --http-port=8080, --udp-port=8081]
        readinessProbe: {httpGet: {path: /healthz, port: 8080}, periodSeconds: 1}
---
apiVersion: v1
kind: Service
metadata: {name: $app, namespace: $N}
spec:
  selector: {app: $app}
  ports:
  - {name: http, port: 80, targetPort: 8080}
  - {name: dns, port: 53, targetPort: 8081, protocol: UDP}
YAML
  done
  cat <<YAML
---
apiVersion: v1
kind: ConfigMap
metadata: {name: proxyconf, namespace: $N}
data:
  default.conf: |
    server {
      listen 8080 proxy_protocol;
      location / { return 200 "from \$proxy_protocol_addr\n"; }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: proxied, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: proxied}}
  template:
    metadata: {labels: {app: proxied}}
    spec:
      terminationGracePeriodSeconds: 2
      volumes: [{name: conf, configMap: {name: proxyconf}}]
      containers:
      - name: nginx
        image: nginx:1.27-alpine
        volumeMounts: [{name: conf, mountPath: /etc/nginx/conf.d}]
---
apiVersion: v1
kind: Service
metadata: {name: proxied, namespace: $N}
spec: {selector: {app: proxied}, ports: [{port: 80, targetPort: 8080}]}
---
apiVersion: v1
kind: Pod
metadata: {name: client, namespace: $N}
spec:
  terminationGracePeriodSeconds: 1
  containers: [{name: c, image: busybox:1.36, command: [sleep, "3600"]}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: tcp, namespace: $N}
data:
  "9401": "$N/echo:80"
  "9402": "$N/echo:http:PROXY"
  "9403": "$N/proxied:80::PROXY"
  "9404": "$N/nonexistent:80"
---
apiVersion: v1
kind: ConfigMap
metadata: {name: udp, namespace: $N}
data:
  "9405": "$N/echo:dns"
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: some, namespace: $N, annotations: {nginx.ingress.kubernetes.io/custom-http-errors: "404"}}
spec:
  ingressClassName: $class
  rules:
  - host: some.svc.test
    http: {paths: [{path: /hostname, pathType: Prefix, backend: {service: {name: echo, port: {number: 80}}}}]}
YAML
} | $K apply -f - >/dev/null
for d in echo fallback proxied; do $K -n $N rollout status deploy/$d --timeout=120s >/dev/null; done
$K -n $N wait pod/client --for=condition=Ready --timeout=120s >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(X curl -s --max-time 2 "http://$podip:9401/hostname" | sed 's/-[^-]*-[^-]*$//')" != "echo" ]; do sleep 1; waited=$((waited + 1)); done

echo "--- tcp-services"
check "a TCP service: the port passes to the Service (${waited}s)" "$(X curl -s --max-time 2 "http://$podip:9401/hostname" | sed 's/-[^-]*-[^-]*$//')" "echo"
check "a port of the Service by name, PROXY from the client: taken, not passed on" \
  "$(X curl -s --max-time 2 --haproxy-protocol "http://$podip:9402/hostname" | sed 's/-[^-]*-[^-]*$//')" "echo"
check "PROXY to the Service: the backend gets the client's address" \
  "$(X curl -s --max-time 2 "http://$podip:9403/" | sed 's/[0-9.]*$/ADDR/')" "from ADDR"
check "a Service that does not exist: a Warning event on the ConfigMap" \
  "$($K -n $N get events --field-selector involvedObject.name=tcp,type=Warning -o jsonpath='{.items[*].message}' | grep -c 'nonexistent')" "1"
echo "--- udp-services"
waited=0
while [ "$waited" -lt 30 ] && [ "$(udp 9405 hostname | sed 's/-[^-]*-[^-]*$//')" != "echo" ]; do waited=$((waited + 3)); done
check "a UDP service: the datagrams go to the Service and back (${waited}s)" "$(udp 9405 hostname | sed 's/-[^-]*-[^-]*$//')" "echo"

echo "--- endpoints without reloads"
settle
before=$(reloads)
$K -n $N scale deploy/echo --replicas=2 >/dev/null
$K -n $N scale deploy/fallback --replicas=2 >/dev/null
$K -n $N rollout status deploy/echo --timeout=120s >/dev/null
$K -n $N rollout status deploy/fallback --timeout=120s >/dev/null
sleep 3
seen=""
for i in $(seq 1 30); do seen="$seen $(X curl -s --max-time 2 "http://$podip:9401/hostname")"; done
check "the second endpoint of the TCP service gets connections" "$(echo $seen | tr ' ' '\n' | sort -u | grep -c .)" "2"
check "two endpoints more: $(($(reloads) - before)) reload(s)" "$(($(reloads) - before))" "0"

echo "--- the default backend"
check "an unknown host: the default backend" "$(body unknown.svc.test /x | grep -c '^NOW:')" "1"
check "a path no Ingress of the host has: the default backend" "$(body some.svc.test /other | grep -c '^NOW:')" "1"
check "a path it has: its backend" "$(body some.svc.test /hostname | sed 's/-[^-]*-[^-]*$//')" "echo"
check "custom-http-errors without default-backend: the codes (a 404 of the backend) to the default backend" "$(body some.svc.test /hostname/nonexistent | grep -c '^NOW:')" "1"
settle
before=$(reloads)
$K -n $N scale deploy/fallback --replicas=1 >/dev/null
sleep 5
check "fewer endpoints of the default backend: $(($(reloads) - before)) reload(s)" "$(($(reloads) - before))" "0"
check "and it still answers" "$(body unknown.svc.test /x | grep -c '^NOW:')" "1"

echo "--- a change of the ConfigMap"
$K -n $N patch cm tcp --type merge -p '{"data":{"9406":"'$N'/echo:80"}}' >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(X curl -s --max-time 2 "http://$podip:9406/hostname" | sed 's/-[^-]*-[^-]*$//')" != "echo" ]; do sleep 1; waited=$((waited + 1)); done
check "a new port is served (${waited}s)" "$(X curl -s --max-time 2 "http://$podip:9406/hostname" | sed 's/-[^-]*-[^-]*$//')" "echo"
$K -n $N patch cm tcp --type json -p '[{"op":"remove","path":"/data/9401"}]' >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ -n "$(X curl -s --max-time 1 "http://$podip:9401/hostname")" ]; do sleep 1; waited=$((waited + 1)); done
check "a port taken out is closed (${waited}s)" "$(X curl -s --max-time 1 "http://$podip:9401/hostname")" ""

[ -n "${KEEP:-}" ] || $K delete ns $N --wait=false >/dev/null 2>&1

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
