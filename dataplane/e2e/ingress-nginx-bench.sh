#!/bin/bash
#
# What wxingress costs against ingress-nginx, on the same kind node with the
# same backends and the same Ingresses (the corpus of
# ingress-nginx-parity.sh, which must have run its backends phase): ingress-nginx
# 4.15.1 with its default metrics, rke2-ingress-nginx 4.12.600 (v1.12.6, what
# RKE2 runs) and wxingress with deployments/rke2-cutover/values.yaml (the
# ingress-nginx compatibility profile, -dynamic-upstreams, its metrics on).
#
# Each controller in turn as the hostPort DaemonSet, with 4 nginx workers
# pinned to the CPUs of CPUS; the load (wrk, keepalive, 64 connections) runs
# pinned to other CPUs (LOADCPUS) against a backend that costs nothing
# (nginx returning 200), over http and https. Measured:
#
#   req/s, p50 and p99 latency         wrk --latency, 30 s after a 10 s warm-up
#   CPU per request                    utime+stime of the pod's processes (nginx
#                                      and the controller) over the run / requests
#   memory                             the PSS of the pod's processes at rest,
#                                      after the load and after the churn
#   endpoint churn                     the backend scaled 1-3-1 six times under
#                                      load: reloads, controller CPU, errors, p99
#   Ingress churn                      20 Ingresses made, changed and deleted
#                                      under load: reloads, controller CPU,
#                                      errors, p99, and the time from creating an
#                                      Ingress to its first answer
#
# METRICS=off runs each controller with its metrics off instead - ingress-nginx
# and rke2 with controller.metrics.enabled=false and --enable-metrics=false (no
# Lua monitor in the log phase, no socket to the controller, no collector),
# wxingress with prometheus.create=false (-enable-prometheus-metrics=false: no
# location counts in the module, no metrics listener) - and skips the churn,
# whose reloads are read from the metrics. RUNS=n measures http and https n
# times on the same install (median and range in the table); the CPU of the
# controller's own process is recorded beside the pod's, which splits
# ingress-nginx's metrics cost into nginx's (Lua, the socket) and the Go
# collector's.
#
# Writes $WORK/bench-<controller>[-nometrics].json and prints a table.

set -u
export KUBECONFIG=${KUBECONFIG:-$HOME/inxparity/kubeconfig}
here=$(cd "$(dirname "$0")" && pwd)
K="${KUBECTL:-kubectl}"
H="${HELM:-helm}"
D="${DOCKER:-sudo docker}"
NODE=${NODE:-inxparity-control-plane}
WX=${WXINGRESS:-wxingress:parity}
CHARTS=${CHARTS:-$HOME/inxparity/charts}
WORK=${WORK:-$HOME/inxparity/run}
CPUS=${CPUS:-8-11}
LOADCPUS=${LOADCPUS:-64-79}
WORKERS=4
DURATION=${DURATION:-30s}
CONTROLLERS=${CONTROLLERS:-ingress-nginx-4.15.1 rke2-ingress-nginx-1.12.6 wxingress}
METRICS=${METRICS:-on}
RUNS=${RUNS:-1}
case "$METRICS" in on) metrics=true suffix= ;; off) metrics=false suffix=-nometrics ;; *) echo "METRICS is on or off" >&2; exit 2 ;; esac
CHURN=${CHURN:-$([ "$METRICS" = on ] && echo yes || echo no)}
B=parity-backends
mkdir -p "$WORK"
say() { echo "$(date +%H:%M:%S) $*" >&2; }
nodeip=$($K get node "$NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
mask=$(python3 -c "
lo, hi = map(int, '$CPUS'.split('-'))
print(' '.join(format(1 << c, 'b') for c in range(lo, hi + 1)))")

# the load generator: wrk in a container on kind's network, on CPUs of its own
$D image inspect parity-wrk >/dev/null 2>&1 || printf 'FROM alpine:3.20\nRUN apk add --no-cache wrk\n' | $D build -q -t parity-wrk - >/dev/null
wrk() { # wrk <host> <scheme> [duration]: the summary wrk prints, with latency percentiles
  $D run --rm --network kind --cpuset-cpus "$LOADCPUS" parity-wrk wrk -t8 -c64 -d"${3:-$DURATION}" --latency \
    -H "Host: $1" "$2://$nodeip/" 2>&1
}
parse() { # parse < wrk output: req/s p50 p99 requests errors
  python3 -c '
import re, sys
t = sys.stdin.read()
def ms(v):
    n, u = float(re.match(r"[0-9.]+", v).group()), re.sub(r"[0-9.]", "", v)
    return n * {"us": 0.001, "ms": 1, "s": 1000, "m": 60000}[u]
rps = float(re.search(r"Requests/sec:\s+([0-9.]+)", t).group(1))
p50 = ms(re.search(r"^\s+50%\s+(\S+)", t, re.M).group(1)); p99 = ms(re.search(r"^\s+99%\s+(\S+)", t, re.M).group(1))
n = int(re.search(r"(\d+) requests in", t).group(1))
err = sum(int(x) for x in re.findall(r"(?:connect|read|write|timeout) (\d+)", t))
non2xx = re.search(r"Non-2xx or 3xx responses: (\d+)", t)
print(f"{rps:.0f} {p50:.3f} {p99:.3f} {n} {err + (int(non2xx.group(1)) if non2xx else 0)}")'
}
# the controller's pod that runs: not one an earlier install left terminating
livepod() { # livepod <namespace> <selector> <uid|ip>
  $K -n "$1" get pod -l "$2" -o json | python3 -c '
import json, sys
pods = [p for p in json.load(sys.stdin)["items"] if p["status"].get("phase") == "Running" and not p["metadata"].get("deletionTimestamp")]
print(pods[0]["metadata"]["uid"] if sys.argv[1] == "uid" else pods[0]["status"]["podIP"]) if pods else None' "$3"
}
# the processes of the controller pod in the node, by the pod's cgroup
podprocs() { # podprocs <namespace> <selector>: "cpu-ticks pss-kB" of nginx and the controller, summed
  uid=$(livepod "$1" "$2" uid | tr - _)
  $D exec "$NODE" sh -c "t=0; m=0; for p in /proc/[0-9]*; do grep -q '$uid' \$p/cgroup 2>/dev/null || continue; set -- \$(sed 's/.*) //' \$p/stat); t=\$((t + \${12} + \${13})); k=\$(awk '/^Pss:/ {print \$2}' \$p/smaps_rollup 2>/dev/null); m=\$((m + \${k:-0})); done; echo \$t \$m"
}
ctlcpu() { # ctlcpu <namespace> <selector> <process name>: cpu ticks of the controller process alone
  uid=$(livepod "$1" "$2" uid | tr - _)
  $D exec "$NODE" sh -c "t=0; for p in /proc/[0-9]*; do grep -q '$uid' \$p/cgroup 2>/dev/null || continue; grep -q '$3' \$p/cmdline 2>/dev/null || continue; set -- \$(sed 's/.*) //' \$p/stat); t=\$((t + \${12} + \${13})); done; echo \$t"
}

# the backend that costs nothing, and the Ingresses of the benchmark
cat <<YAML | $K apply -f - >/dev/null
apiVersion: v1
kind: ConfigMap
metadata: {name: fastconf, namespace: $B}
data:
  default.conf: |
    server { listen 8080 reuseport; access_log off; keepalive_requests 100000; location / { return 200 "ok\\n"; } }
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: fast, namespace: $B}
spec:
  replicas: 1
  selector: {matchLabels: {app: fast}}
  template:
    metadata: {labels: {app: fast}}
    spec:
      terminationGracePeriodSeconds: 5
      volumes: [{name: conf, configMap: {name: fastconf}}]
      containers:
      - name: nginx
        image: nginx:1.27-alpine@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10
        volumeMounts: [{name: conf, mountPath: /etc/nginx/conf.d}]
        readinessProbe: {httpGet: {path: /, port: 8080}, periodSeconds: 1}
---
apiVersion: v1
kind: Service
metadata: {name: fast, namespace: $B}
spec: {selector: {app: fast}, ports: [{port: 80, targetPort: 8080}]}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: bench, namespace: $B}
spec:
  ingressClassName: nginx
  tls: [{hosts: [bench.parity.test], secretName: bench-tls}]
  rules:
  - host: bench.parity.test
    http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: fast, port: {number: 80}}}}]}
YAML
$K -n $B create secret tls bench-tls --cert="$WORK/tls.crt" --key="$WORK/tls.key" --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n $B rollout status deploy/fast --timeout=120s >/dev/null

install() { # install <controller>: it alone on the node, ready; prints "<namespace> <selector> <controller process>"
  common=(--set-string "controller.config.worker-processes=$WORKERS" --set-string "controller.config.worker-cpu-affinity=$mask")
  case "$1" in
    ingress-nginx-4.15.1)
      $H upgrade --install ingress-nginx "$CHARTS/ingress-nginx-4.15.1.tgz" -n ingress-nginx --create-namespace -f "$WORK/a15-values.yaml" \
        --set controller.kind=DaemonSet --set controller.hostPort.enabled=true --set controller.service.enabled=false \
        --set controller.watchIngressWithoutClass=true --set controller.admissionWebhooks.enabled=false --set controller.metrics.enabled=$metrics \
        --set-string controller.extraArgs.enable-metrics=$metrics \
        --set controller.image.digest=sha256:594ceea76b01c592858f803f9ff4d2cb40542cae2060410b2c95f75907d659e1 "${common[@]}" --wait --timeout 10m >/dev/null
      echo "ingress-nginx app.kubernetes.io/name=ingress-nginx nginx-ingress-controller" ;;
    rke2-ingress-nginx-1.12.6)
      $H upgrade --install rke2-ingress-nginx "$CHARTS/rke2-ingress-nginx-4.12.600.tgz" -n kube-system -f "$WORK/rke2-values.yaml" \
        --set controller.admissionWebhooks.enabled=false --set controller.metrics.enabled=$metrics \
        --set-string controller.extraArgs.enable-metrics=$metrics \
        --set controller.image.digest=sha256:73db650d9b44b6abc208c6a45d07ef1ab59dae139a86d484c01dcd831bbbd614 "${common[@]}" --wait --timeout 10m >/dev/null
      echo "kube-system app.kubernetes.io/name=rke2-ingress-nginx nginx-ingress-controller" ;;
    wxingress)
      $K label node "$NODE" wx1.eu/ingress=wxingress --overwrite >/dev/null
      $H upgrade --install wxingress "$CHARTS/wxingress-chart.tgz" -n wxingress --create-namespace -f "$here/../../deployments/rke2-cutover/values.yaml" \
        --set controller.image.repository="${WX%:*}" --set controller.image.tag="${WX#*:}" --set controller.image.pullPolicy=Never \
        --set prometheus.serviceMonitor.create=false --set prometheus.create=$metrics --set prometheus.service.create=$metrics \
        --set-json 'controller.tolerations=[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists"}]' \
        --set-string controller.config.entries.worker-processes=$WORKERS --set-string "controller.config.entries.worker-cpu-affinity=$mask" \
        --set-string controller.config.entries.global-allowed-response-headers=X-Inx-Custom --wait --timeout 10m >/dev/null
      # OnDelete (the cutover example values): no rollout status, the pod is new with the install
      $K -n wxingress wait --for=condition=Ready pod -l app.kubernetes.io/instance=wxingress --timeout=300s >/dev/null
      echo "wxingress app.kubernetes.io/instance=wxingress nginx-ingress /nginx-ingress" ;;
  esac
}
uninstall() {
  case "$1" in
    ingress-nginx-4.15.1) $H uninstall ingress-nginx -n ingress-nginx --wait >/dev/null ;;
    rke2-ingress-nginx-1.12.6) $H uninstall rke2-ingress-nginx -n kube-system --wait >/dev/null ;;
    wxingress) $H uninstall wxingress -n wxingress --wait >/dev/null; $K label node "$NODE" wx1.eu/ingress- >/dev/null ;;
  esac
  sleep 15
}
reloads() { # the reloads of nginx so far, from the controller's metrics
  case "$1" in
    wxingress) $D exec "$NODE" curl -s "http://$2:10254/metrics" | awk '/^nginx_ingress_controller_nginx_reloads_total/ {s += $2} END {print s + 0}' ;;
    *) $D exec "$NODE" curl -s "http://$2:10254/metrics" | awk '/^nginx_ingress_controller_success/ {s += $2} END {print s + 0}' ;;
  esac
}

ticks=$($D exec "$NODE" getconf CLK_TCK)
# wrk's summary, once docker has written all of it
settled() { w=0; while [ "$w" -lt 20 ] && ! grep -q "Requests/sec" "$1"; do sleep 0.5; w=$((w + 1)); done; }
for c in $CONTROLLERS; do
  say "$c"
  # every controller of an earlier run must be gone, this one too: each starts fresh
  for other in ingress-nginx-4.15.1 rke2-ingress-nginx-1.12.6 wxingress; do uninstall "$other" >/dev/null 2>&1; done
  set -- $(install "$c"); ns=$1 sel=$2 proc=$3
  w=0; while [ "$w" -lt 120 ] && [ "$($D exec "$NODE" curl -s -o /dev/null -w '%{http_code}' -H 'Host: bench.parity.test' http://localhost/)" != 200 ]; do sleep 1; w=$((w + 1)); done
  sleep 20
  # only one pod is left now, the one that answered
  podip=$(livepod "$ns" "$sel" ip)
  set -- $(podprocs "$ns" "$sel"); rest_pss=$2
  wrk bench.parity.test http 10s >/dev/null
  out="{\"controller\": \"$c\", \"metrics\": \"$METRICS\", \"rest_pss_kb\": $rest_pss"
  runs=""
  for run in $(seq 1 "$RUNS"); do
    for scheme in http https; do
      # a run whose wrk printed no summary (it could not reach the node) is repeated
      for try in 1 2 3; do
        set -- $(podprocs "$ns" "$sel"); t0=$1; c0=$(ctlcpu "$ns" "$sel" "$proc")
        wrk bench.parity.test $scheme > "$WORK/wrk-$c$suffix-$scheme-$run.txt"
        set -- $(podprocs "$ns" "$sel"); t1=$1 pss=$2; c1=$(ctlcpu "$ns" "$sel" "$proc")
        r=$(parse < "$WORK/wrk-$c$suffix-$scheme-$run.txt" 2>/dev/null) && [ -n "$r" ] && break
        say "$c $scheme: no summary from wrk, again: $(tail -2 "$WORK/wrk-$c$suffix-$scheme-$run.txt" | tr '\n' ' ')"
        sleep 10
      done
      set -- $r
      runs="$runs${runs:+, }{\"scheme\": \"$scheme\", \"rps\": $1, \"p50_ms\": $2, \"p99_ms\": $3, \"errors\": $5, \"cpu_us_per_request\": $(python3 -c "print(round(($t1 - $t0) / $ticks / $4 * 1e6, 2))"), \"controller_cpu_us_per_request\": $(python3 -c "print(round(($c1 - $c0) / $ticks / $4 * 1e6, 2))")}"
      say "$c $scheme run $run: $r"
    done
  done
  # each value the median of the runs, the runs themselves beside it
  out="$out, $(python3 -c '
import json, statistics, sys
runs = json.loads(sys.argv[1])
o = {}
for s in ("http", "https"):
    for k in ("rps", "p50_ms", "p99_ms", "errors", "cpu_us_per_request", "controller_cpu_us_per_request"):
        m = statistics.median(r[k] for r in runs if r["scheme"] == s)
        o[f"{s}_{k}"] = round(m) if k in ("rps", "errors") else round(m, 3 if k.endswith("_ms") else 2)
o["runs"] = runs
print(json.dumps(o)[1:-1])' "[$runs]")"
  out="$out, \"loaded_pss_kb\": $pss"

  if [ "$CHURN" = yes ]; then
  # endpoint churn under load
  ctl0=$(ctlcpu "$ns" "$sel" "$proc"); r0=$(reloads "$c" "$podip")
  wrk bench.parity.test http 60s > "$WORK/churn-ep-$c.txt" &
  sleep 3
  for i in 1 2 3 4 5 6; do
    $K -n $B scale deploy/fast --replicas=3 >/dev/null; sleep 4
    $K -n $B scale deploy/fast --replicas=1 >/dev/null; sleep 4
  done
  wait
  settled "$WORK/churn-ep-$c.txt"; r=$(parse < "$WORK/churn-ep-$c.txt"); set -- $r
  out="$out, \"endpoint_churn\": {\"reloads\": $(( $(reloads "$c" "$podip") - r0 )), \"controller_cpu_s\": $(python3 -c "print(round(($(ctlcpu "$ns" "$sel" "$proc") - $ctl0) / $ticks, 2))"), \"errors\": $5, \"p99_ms\": $3, \"rps\": $1}"

  # Ingress churn under load
  ctl0=$(ctlcpu "$ns" "$sel" "$proc"); r0=$(reloads "$c" "$podip")
  wrk bench.parity.test http 60s > "$WORK/churn-ing-$c.txt" &
  sleep 3
  first=""
  for i in $(seq 1 20); do
    t=$(date +%s.%N)
    printf 'apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata: {name: churn-%s, namespace: %s}\nspec:\n  ingressClassName: nginx\n  rules: [{host: churn-%s.parity.test, http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: fast, port: {number: 80}}}}]}}]\n' "$i" $B "$i" | $K apply -f - >/dev/null
    while [ "$($D exec "$NODE" curl -s -o /dev/null -w '%{http_code}' -H "Host: churn-$i.parity.test" http://localhost/)" != 200 ]; do sleep 0.05; done
    first="$first $(python3 -c "import time; print(round(time.time() - $t, 3))")"
    $K -n $B annotate ingress "churn-$i" nginx.ingress.kubernetes.io/proxy-read-timeout=120 --overwrite >/dev/null
  done
  for i in $(seq 1 20); do $K -n $B delete ingress "churn-$i" --wait=false >/dev/null; done
  wait
  settled "$WORK/churn-ing-$c.txt"; r=$(parse < "$WORK/churn-ing-$c.txt"); set -- $r
  median=$(echo $first | tr ' ' '\n' | sort -n | awk '{a[NR]=$1} END {print a[int((NR+1)/2)]}')
  out="$out, \"ingress_churn\": {\"reloads\": $(( $(reloads "$c" "$podip") - r0 )), \"controller_cpu_s\": $(python3 -c "print(round(($(ctlcpu "$ns" "$sel" "$proc") - $ctl0) / $ticks, 2))"), \"errors\": $5, \"p99_ms\": $3, \"rps\": $1, \"first_answer_median_s\": $median}"
  # what is left after the reloads, once the old workers are gone
  sleep 30
  set -- $(podprocs "$ns" "$sel")
  out="$out, \"churned_pss_kb\": $2"
  fi
  echo "$out}" | tee "$WORK/bench-$c$suffix.json"
done
python3 - "$WORK" "$suffix" $CONTROLLERS <<'PY'
import json, sys
work, suffix, names = sys.argv[1], sys.argv[2], sys.argv[3:]
rows = [json.load(open(f"{work}/bench-{n}{suffix}.json")) for n in names]
def spread(r, s, k):  # the median, with the range of the runs when there are several
    v = [x[k] for x in r.get("runs", []) if x["scheme"] == s]
    m = r[f"{s}_{k}"]
    return f"{m} ({min(v)}-{max(v)})" if len(v) > 1 else m
def row(label, f):
    print(f"| {label} | " + " | ".join(str(f(r)) for r in rows) + " |")
print("| | " + " | ".join(names) + " |")
print("|---|" + "---|" * len(names))
for s in ("http", "https"):
    row(f"{s} req/s", lambda r: spread(r, s, "rps"))
    row(f"{s} p50 / p99 ms", lambda r: f"{r[s+'_p50_ms']} / {r[s+'_p99_ms']}")
    row(f"{s} CPU µs/request", lambda r: spread(r, s, "cpu_us_per_request"))
    row(f"{s} of it the controller process", lambda r: spread(r, s, "controller_cpu_us_per_request"))
row("memory at rest / loaded / after the churn (PSS MiB)", lambda r: f"{r['rest_pss_kb']//1024} / {r['loaded_pss_kb']//1024} / {r['churned_pss_kb']//1024 if 'churned_pss_kb' in r else '-'}")
if any("endpoint_churn" not in r for r in rows):
    sys.exit()
for k, label in (("endpoint_churn", "endpoint churn"), ("ingress_churn", "Ingress churn")):
    row(f"{label}: reloads", lambda r: r[k]["reloads"])
    row(f"{label}: controller CPU s", lambda r: r[k]["controller_cpu_s"])
    row(f"{label}: errors, p99 ms", lambda r: f"{r[k]['errors']}, {r[k]['p99_ms']}")
row("new Ingress to its first answer (median s)", lambda r: r["ingress_churn"]["first_answer_median_s"])
PY
