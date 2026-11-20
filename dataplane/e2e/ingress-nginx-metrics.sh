#!/bin/sh
#
# ingress-nginx's metrics: the request metrics the wxingress module counts,
# with ingress-nginx's names, labels and buckets, kept over a reload; the
# reload, certificate and process metrics; and every metric the Grafana
# dashboard 9614 of ingress-nginx asks for.
#
# Runs where kubectl and the kind node's docker are, against a release of
# charts/wxingress installed as for gateway.sh, with -dynamic-upstreams and
# prometheus.create (the metrics on 9113, the chart's default port):
#
#   RELEASE_NS=wxgw CLASS=wxgw dataplane/e2e/ingress-nginx-metrics.sh
#
# It makes the namespace metrics-e2e and removes it.

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxgw}
class=${CLASS:-wxgw}
port=${METRICS_PORT:-9113}
N=metrics-e2e

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
w=0
while [ "$w" -lt 120 ] && [ "$($K -n "$ns" get pod -l app.kubernetes.io/instance="$ns" -o name | wc -l)" -gt 1 ]; do sleep 1; w=$((w + 1)); done
pod=$($K -n "$ns" get pod -l app.kubernetes.io/instance="$ns" -o name | head -1)
podip=$($K -n "$ns" get "$pod" -o jsonpath='{.status.podIP}')
ctrl=$($K -n "$ns" get svc -l app.kubernetes.io/instance="$ns" -o jsonpath='{.items[0].spec.clusterIP}')
metrics() { X curl -s --max-time 10 "http://$podip:$port/metrics"; }
# value <metric{labels...}> : the value of the one series whose line starts so
value() { metrics | grep -F "$1" | head -1 | awk '{print $NF}'; }

$K delete ns $N --wait=true >/dev/null 2>&1
$K create ns $N >/dev/null
tmp=$(mktemp -d)
openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj /CN=tls.metrics.test -addext "subjectAltName=DNS:tls.metrics.test" \
  -keyout "$tmp/key" -out "$tmp/crt" >/dev/null 2>&1
$K -n $N create secret tls tls --cert="$tmp/crt" --key="$tmp/key" >/dev/null
expires=$(date -d "$(openssl x509 -in "$tmp/crt" -noout -enddate | cut -d= -f2)" +%s)
rm -rf "$tmp"
cat <<YAML | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: app}}
  template:
    metadata: {labels: {app: app}}
    spec:
      terminationGracePeriodSeconds: 2
      containers:
      - name: echo
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: [netexec, --http-port=8080]
        readinessProbe: {httpGet: {path: /healthz, port: 8080}, periodSeconds: 1}
---
apiVersion: v1
kind: Service
metadata: {name: app, namespace: $N}
spec: {selector: {app: app}, ports: [{port: 80, targetPort: 8080}]}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: shop, namespace: $N}
spec:
  ingressClassName: $class
  tls: [{hosts: [tls.metrics.test], secretName: tls}]
  rules:
  - host: shop.metrics.test
    http: {paths: [{path: /api, pathType: Prefix, backend: {service: {name: app, port: {number: 80}}}}]}
  - host: tls.metrics.test
    http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: app, port: {number: 80}}}}]}
YAML
$K -n $N rollout status deploy/app --timeout=120s >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(X curl -s -o /dev/null -w '%{http_code}' --max-time 3 -H 'Host: shop.metrics.test' "http://$ctrl/api")" = "404" ]; do sleep 1; waited=$((waited + 1)); done

L='canary="",controller_class='
series='nginx_ingress_controller_requests{canary="",controller_class="'
latency_before=$(metrics | grep -F 'nginx_ingress_controller_ingress_upstream_latency_seconds_count{' | grep -F 'ingress="shop"' | grep -F 'canary=""' | awk '{s+=$NF} END {print s+0}')
before=$(metrics | grep -F 'nginx_ingress_controller_requests{' | grep -F 'ingress="shop"' | grep -F 'status="200"' | grep -F 'method="GET"' | awk '{s+=$NF} END {print s+0}')
X sh -c "for i in \$(seq 1 25); do curl -s -o /dev/null --max-time 3 -H 'Host: shop.metrics.test' http://$ctrl/api/missing; done"
X sh -c "for i in \$(seq 1 5); do curl -s -o /dev/null --max-time 3 -X POST -d hello -H 'Host: shop.metrics.test' http://$ctrl/api/missing; done"
sleep 1
count() { metrics | grep -F "nginx_ingress_controller_$1{" | grep -F 'ingress="shop"' | grep -F "status=\"$2\"" | grep -F "method=\"$3\"" | awk '{s+=$NF} END {print s+0}'; }
echo "--- the request metrics"
check "requests: a counter per status and method, the Ingress's namespace, name, Service, path and host" \
  "$(($(count requests 200 GET) - before))" "25"
check "and the POSTs apart" "$(count requests 200 POST)" "5"
line=$(metrics | grep -F 'nginx_ingress_controller_requests{' | grep -F 'ingress="shop"' | grep -F 'method="POST"' | head -1)
for label in 'namespace="metrics-e2e"' 'service="app"' 'path="/api"' 'host="shop.metrics.test"' 'canary=""' 'controller_pod=' 'controller_namespace="'"$ns"'"' 'controller_class='; do
  check "  with $label" "$(echo "$line" | grep -cF "$label")" "1"
done
check "request_duration_seconds: a histogram with ingress-nginx's buckets" \
  "$(metrics | grep -F 'nginx_ingress_controller_request_duration_seconds_bucket{' | grep -F 'ingress="shop"' | grep -F 'method="POST"' | sed 's/.*le="\([^"]*\)".*/\1/' | tr '\n' ' ')" \
  "0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10 +Inf "
check "and its count is the requests" "$(count request_duration_seconds_count 200 POST)" "5"
check "response_duration_seconds of the upstream" "$(count response_duration_seconds_count 200 POST)" "5"
check "connect_duration_seconds" "$(count connect_duration_seconds_count 200 POST)" "5"
check "header_duration_seconds" "$(count header_duration_seconds_count 200 POST)" "5"
check "request_size: the length of the requests, with their bodies" \
  "$([ "$(count request_size_sum 200 POST)" -gt 500 ] && echo more)" "more"
check "response_size and bytes_sent" "$(count response_size_count 200 POST)$(count bytes_sent_count 200 POST)" "55"
latency() { metrics | grep -F "nginx_ingress_controller_ingress_upstream_latency_seconds$1{" | grep -F 'ingress="shop"' | grep -F 'namespace="metrics-e2e"' | grep -F 'service="app"' | grep -F 'canary=""'; }
check "ingress_upstream_latency_seconds: ingress-nginx's summary up to 1.11 per Ingress, namespace, Service and canary, its count every request that connected" \
  "$(($(latency _count | awk '{s+=$NF} END {print s+0}') - latency_before))" "30"
check "  with the quantiles 0.5, 0.9 and 0.99 of the connect times" \
  "$(latency "" | sed -n 's/.*quantile="\([^"]*\)".*/\1/p' | tr '\n' ' ')" "0.5 0.9 0.99 "
check "  each a time (not NaN: requests came in the last ten minutes), the sum in seconds" \
  "$(latency "" | awk '$NF == "NaN" || $NF < 0 || $NF > 10 {bad++} END {print bad+0}')$(latency _sum | awk '$NF >= 0 && $NF < 30 {print "ok"}')" "0ok"

echo "--- over a reload"
reloads=$(value 'nginx_ingress_controller_success{')
$K -n $N annotate ingress shop nginx.org/proxy-read-timeout=33s --overwrite >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(value 'nginx_ingress_controller_success{')" = "$reloads" ]; do sleep 1; waited=$((waited + 1)); done
check "a reload counts in success (${waited}s)" "$([ "$(value 'nginx_ingress_controller_success{')" -gt "$reloads" ] && echo yes)" "yes"
X sh -c "curl -s -o /dev/null --max-time 3 -X POST -d hello -H 'Host: shop.metrics.test' http://$ctrl/api/missing"
sleep 1
check "the counts are kept over it: the new workers add to them" "$(count requests 200 POST)" "6"
check "config_last_reload_successful" "$(value 'nginx_ingress_controller_config_last_reload_successful{')" "1"
check "config_last_reload_successful_timestamp_seconds is recent" \
  "$([ $(( $(date +%s) - $(value 'nginx_ingress_controller_config_last_reload_successful_timestamp_seconds{' | awk '{printf "%d", $1}') )) -lt 300 ] && echo yes)" "yes"

echo "--- certificates and processes"
waited=0
while [ "$waited" -lt 30 ] && ! metrics | grep -q 'nginx_ingress_controller_ssl_expire_time_seconds{.*host="tls.metrics.test"'; do sleep 2; waited=$((waited + 2)); done
check "ssl_expire_time_seconds of the host, its Secret and namespace (${waited}s)" \
  "$(metrics | grep 'nginx_ingress_controller_ssl_expire_time_seconds{.*host="tls.metrics.test"' | grep -F 'namespace="metrics-e2e"' | grep -F 'secret_name="tls"' | awk '{printf "%d", $NF}')" "$expires"
check "ssl_certificate_info" "$(metrics | grep -c 'nginx_ingress_controller_ssl_certificate_info{.*host="tls.metrics.test"')" "1"
check "nginx_process_num_procs: the master and the workers" \
  "$([ "$(value 'nginx_ingress_controller_nginx_process_num_procs{' | awk '{printf "%d", $1}')" -ge 2 ] && echo yes)" "yes"
check "nginx_process_connections of stub_status" "$(metrics | grep -c '^nginx_ingress_controller_nginx_process_connections{.*state=')" "4"

echo "--- what the dashboard 9614 asks for"
for m in requests request_duration_seconds_bucket request_size_sum response_size_sum config_hash config_last_reload_successful \
  config_last_reload_successful_timestamp_seconds success ssl_expire_time_seconds nginx_process_connections \
  nginx_process_cpu_seconds_total nginx_process_resident_memory_bytes; do
  check "nginx_ingress_controller_$m with controller_pod, controller_class, controller_namespace (but the certificates')" \
    "$(metrics | grep "^nginx_ingress_controller_$m{" | head -1 | grep -c 'controller_class=\|class=')" "1"
done

[ -n "${KEEP:-}" ] || $K delete ns $N --wait=false >/dev/null 2>&1

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
