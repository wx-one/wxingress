#!/bin/bash
#
# wxingress in ingress-nginx's place, as on an RKE2 cluster: the same
# Ingresses answered the same by both, and the switch from one to the other
# node by node with no failed request.
#
# On a kind cluster whose nodes all run the controller as a hostPort
# DaemonSet (the RKE2 way), it
#
#   1. makes the backends - an echo server that answers with what it got
#      (HTTP and HTTPS), a WebSocket echo, grpcbin, an auth service - and
#      the corpus: dataplane/e2e/corpus/ (the anonymised Ingresses of a production RKE2 cluster, the
#      Ingresses of real charts, those of ingress-nginx's own e2e suite)
#      with their Services and Secrets (parity/corpus.py);
#   2. installs ingress-nginx 4.15.1 and probes every node (A15), takes it
#      away;
#   3. installs rke2-ingress-nginx 4.12.600 (rancher/nginx-ingress-controller
#      v1.12.6-hardened1) as RKE2 does - release rke2-ingress-nginx in
#      kube-system, its admission webhook with failurePolicy Fail - and
#      probes every node (A);
#   4. follows docs/migration/rke2.md under a load that goes round the
#      nodes like an outside balancer (parity/probe.py load): the affinity of
#      rke2-ingress-nginx, wxingress with values like the cutover example.s, each node out
#      of the balancer, switched by its label, its probes compared with A and
#      back (the first node with several: back to rke2-ingress-nginx and over
#      again, the runbook's way back for one node), wxingress's webhook
#      (Ignore from its install) made Fail and rke2-ingress-nginx's off
#      before the last one, then the status, the end of rke2-ingress-nginx
#      and the IngressClass taken over (BACK=1: the whole way back instead);
#   5. probes every node again (B) and compares B with A and with A15
#      (parity/diff.py), with the differences dataplane/e2e/ingress-nginx-parity.allow
#      allows - each citing the doc that says why.
#
# Fails if the load saw a failed request, or a probe differs where the
# allow-list does not say it may.
#
#   KUBECONFIG    of the kind cluster (~/inxparity/kubeconfig)
#   KIND          the kind binary, and NAME the cluster (inxparity)
#   WXINGRESS     the wxingress image (wxingress:parity), loaded into the nodes
#   CHARTS        a directory with rke2-ingress-nginx-4.12.600.tgz,
#                 ingress-nginx-4.15.1.tgz and wxingress-chart.tgz
#   WORK          where the results go (~/inxparity/run)
#   PHASES        which of backends a15 rke2 cutover after diff to run (all)
#   LB            haproxy: an outside balancer in front of the nodes as well -
#                 a haproxy container on kind's network (TCP, ports 80 and
#                 443, round robin, no health checks) whose servers the
#                 cutover drains and readies through its runtime API with the
#                 probe pool, and wrk through it over http and https with
#                 keepalive, per phase (rolling, steps, each node, the way
#                 back for one node): the requests and errors of each go to
#                 $WORK/phases.txt
#   DRAIN         seconds to wait for a drained node's connections to end
#                 before it is switched (30)
#   BACK          1: instead of step 7, the runbook's way back, all of it -
#                 every node back to rke2-ingress-nginx, its webhook again,
#                 wxingress uninstalled; the probes after (B) are then
#                 rke2-ingress-nginx's again
#   DOCKER        the docker command (docker), for LB and loading the image

set -u

export KUBECONFIG=${KUBECONFIG:-$HOME/inxparity/kubeconfig}
here=$(cd "$(dirname "$0")" && pwd)
K="${KUBECTL:-kubectl}"
H="${HELM:-helm}"
KIND=${KIND:-kind}
NAME=${NAME:-inxparity}
WX=${WXINGRESS:-wxingress:parity}
CHARTS=${CHARTS:-$HOME/inxparity/charts}
WORK=${WORK:-$HOME/inxparity/run}
PHASES=${PHASES:-backends a15 rke2 cutover after diff}
LB=${LB:-}
DRAIN=${DRAIN:-30}
BACK=${BACK:-}
D=${DOCKER:-docker}
B=parity-backends

mkdir -p "$WORK"
say() { echo "$(date +%H:%M:%S) $*"; }
failed=0
check() {
  if [ "$2" = "$3" ]; then echo "ok       $1"; else echo "FAIL     $1"; echo "  wanted: $3"; echo "  got:    $2"; failed=$((failed + 1)); fi
}
phase() { case " $PHASES " in *" $1 "*) return 0 ;; esac; return 1; }
nodes() { $K get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'; }
ips() { nodes | awk '{print $2}' | tr '\n' ' '; }
P() { $K -n $B exec probe -- "$@"; }
# pool <ip>...: the nodes that get traffic - the probe load's pool and, with LB, the balancer's servers: the others
# drained, and waited for until their connections end (DRAIN seconds at most)
pool() {
  P sh -c "echo '$*' > /tmp/pool"
  [ -n "$LB" ] || return 0
  # the callers' n, ip and w stay theirs
  local n ip st w drained=""
  while read -r n ip; do
    case " $* " in
      *" $ip "*) st=ready ;;
      *) st=drain; drained="$drained $n" ;;
    esac
    lbcmd "set server http/$n state $st; set server https/$n state $st" >/dev/null
  done < <(nodes)
  [ -n "$drained" ] || return 0
  w=0
  while [ "$w" -lt "$DRAIN" ]; do
    [ "$(lbconns $drained)" = 0 ] && break
    sleep 1; w=$((w + 1))
  done
  say "drained$drained: $(lbconns $drained) connections left after ${w}s"
}
lbcmd() { $D exec "$NAME-lb" sh -c "echo '$*' | socat stdio tcp4:127.0.0.1:9999"; }
lbconns() { # lbconns <node>...: the balancer's open connections to them (both ports)
  lbcmd "show stat" | awk -F, -v ns=" $* " '$1 ~ /^https?$/ && index(ns, " " $2 " ") {c += $5} END {print c + 0}'
}
lb_up() { # the balancer in front of the nodes' hostPorts, every node ready
  $D rm -f "$NAME-lb" >/dev/null 2>&1
  {
    printf 'global\n  stats socket ipv4@127.0.0.1:9999 level admin\n  maxconn 20000\n'
    printf 'defaults\n  mode tcp\n  timeout connect 5s\n  timeout client 5m\n  timeout server 5m\n'
    for port in 80 443; do
      be=http; [ $port = 443 ] && be=https
      printf 'frontend %s\n  bind :%s\n  default_backend %s\nbackend %s\n  balance roundrobin\n' $be $port $be $be
      nodes | while read -r n ip; do printf '  server %s %s:%s\n' "$n" "$ip" $port; done
    done
  } > "$WORK/haproxy.cfg"
  chmod 644 "$WORK/haproxy.cfg"
  $D run -d --name "$NAME-lb" --network kind -v "$WORK/haproxy.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro" \
    haproxy:3.0-alpine@sha256:d2b3567d836f3b7d174edb6d6f611ed5308e7966e53a9cd9a597e0415a298efd >/dev/null
  $D image inspect parity-wrk >/dev/null 2>&1 || printf 'FROM alpine:3.20\nRUN apk add --no-cache wrk\n' | $D build -q -t parity-wrk - >/dev/null
  sleep 2
  lbip=$($D inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$NAME-lb")
}
# wrk_phase <name>: the wrk of the phase before ends - its requests and errors (socket errors and 5xx) into
# $WORK/phases.txt beside the probe load's - and the next one starts (no name: none)
loadmark=""
wrk_phase() {
  if [ -n "$curphase" ]; then
    line="$curphase"
    now=$(loadstat)
    line="$line load $(echo "$loadmark $now" | awk '{print $3 - $1, $4 - $2}')"
    if [ -n "$LB" ]; then
      for s in http https; do
        $D kill -s INT "$NAME-wrk-$s" >/dev/null 2>&1
        $D wait "$NAME-wrk-$s" >/dev/null 2>&1
        $D logs "$NAME-wrk-$s" > "$WORK/wrk-$curphase-$s.txt" 2>&1
        $D rm -f "$NAME-wrk-$s" >/dev/null 2>&1
        line="$line $s $(awk '/requests in/ {r = $1} /Socket errors/ {gsub(/[^0-9 ]/, " "); e += $1 + $2 + $3 + $4}
          /Non-2xx/ {e += $NF} END {print r + 0, e + 0}' "$WORK/wrk-$curphase-$s.txt")"
      done
    fi
    echo "$line" >> "$WORK/phases.txt"
    say "phase $line"
  fi
  curphase=${1:-}
  [ -n "$curphase" ] || return 0
  loadmark=$(loadstat)
  [ -n "$LB" ] || return 0
  for s in http https; do
    $D run -d --name "$NAME-wrk-$s" --network kind --add-host "$wrkhost:$lbip" parity-wrk \
      wrk -t2 -c16 -d24h --timeout 10s -H 'User-Agent: parity-wrk' "$s://$wrkhost$wrkpath" >/dev/null
  done
}
curphase=""
probe_all() { # probe_all <name>: every node, the probes into $WORK/<name>-<node>.json
  for ip in $(ips); do
    P python3 /p/probe.py probe "$ip" /p/probes.json "/tmp/$1-$ip.json" >/dev/null
    $K -n $B cp "probe:/tmp/$1-$ip.json" "$WORK/$1-$ip.json" >/dev/null 2>&1
  done
  say "probed $1 on $(ips)"
}
# restart_drained: the wxingress pod of each node restarted the runbook's way - the node out of the balancer, the pod
# deleted, Ready again, the node back - for a change of the chart that its OnDelete DaemonSet does not roll itself
restart_drained() {
  for n in $(nodes | awk '{print $1}'); do
    ip=$(nodes | awk -v n="$n" '$1 == n {print $2}')
    pool "$(ips | tr ' ' '\n' | grep -vx "$ip" | paste -sd' ' -)"
    sleep 3
    $K -n wxingress delete pod -l app.kubernetes.io/instance=wxingress --field-selector spec.nodeName="$n" --wait=true >/dev/null
    ready wxingress app.kubernetes.io/instance=wxingress "$n"
    sleep 2
    pool "$(ips)"
  done
}
# ready <namespace> <label selector> <node>: the controller pod of the node is Ready
ready() {
  w=0
  while [ "$w" -lt 300 ]; do
    if [ "$($K -n "$1" get pod -l "$2" --field-selector spec.nodeName="$3" -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = "True" ]; then return 0; fi
    sleep 1; w=$((w + 1))
  done
  return 1
}
gone() { # gone <namespace> <label selector> <node>: no pod of it on the node
  w=0
  while [ "$w" -lt 400 ] && [ -n "$($K -n "$1" get pod -l "$2" --field-selector spec.nodeName="$3" -o name 2>/dev/null)" ]; do sleep 1; w=$((w + 1)); done
}

# ------------------------------------------------------------------ backends
if phase backends; then
  say "backends and corpus"
  $K create ns $B --dry-run=client -o yaml | $K apply -f - >/dev/null
  # every node runs the controller, the control plane too
  $K taint nodes --all node-role.kubernetes.io/control-plane- >/dev/null 2>&1
  $K apply -f "$here/parity/backends.yaml" >/dev/null
  $K -n $B create configmap probe --from-file="$here/parity/probe.py" --dry-run=client -o yaml | $K apply -f - >/dev/null
  for d in echo ws grpcbin auth; do $K -n $B rollout status deploy/$d --timeout=300s >/dev/null; done
  $K -n $B wait pod/probe --for=condition=Ready --timeout=300s >/dev/null
  ipsof() { $K -n $B get pod -l app="$1" -o jsonpath='{.items[*].status.podIP}' | tr ' ' '\n' | sed 's/.*/"&"/' | paste -sd, -; }
  echo "{\"echo\": [$(ipsof echo)], \"errors\": [$(ipsof echo)], \"ws\": [$(ipsof ws)], \"grpc\": [$(ipsof grpcbin)], \"auth\": [$(ipsof auth)]}" > "$WORK/backends.json"
  corpus="$here/corpus/rke2-cluster/ingresses.json $here/corpus/charts/*.yaml $here/corpus/annotations/*.yaml"
  rm -f "$WORK/tls.crt" "$WORK/tls.key"
  python3 "$here/parity/corpus.py" "$WORK/backends.json" "$WORK" $corpus >/dev/null
  # one certificate for every host of the corpus
  san=$(sed 's/^/DNS:/' "$WORK/hosts.txt" | paste -sd, -)
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj /CN=parity.test -addext "subjectAltName=$san" \
    -keyout "$WORK/tls.key" -out "$WORK/tls.crt" >/dev/null 2>&1
  python3 "$here/parity/corpus.py" "$WORK/backends.json" "$WORK" $corpus
  $K apply -f "$WORK/manifests.yaml" >/dev/null 2>"$WORK/apply.err" || { head -5 "$WORK/apply.err"; }
  $K -n $B cp "$WORK/probes.json" probe:/p/probes.json >/dev/null 2>&1 || P sh -c 'mkdir -p /p'
  $K -n $B cp "$WORK/probes.json" probe:/p/probes.json >/dev/null
  $K -n $B cp "$here/parity/probe.py" probe:/p/probe.py >/dev/null
  $K -n $B cp "$WORK/tls.crt" probe:/p/tls.crt >/dev/null
fi

ingress_nginx_values() { # the values the cutover example has in its HelmChartConfig, and the hostPort DaemonSet RKE2's chart has
  cat <<YAML
controller:
  tolerations:
    - key: app
      operator: Exists
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
  config:
    strict-validate-path-type: "false"
    global-allowed-response-headers: X-Inx-Custom
YAML
}

# --------------------------------------------- ingress-nginx 4.15.1 (A15)
if phase a15; then
  say "ingress-nginx 4.15.1"
  ingress_nginx_values > "$WORK/a15-values.yaml"
  $H upgrade --install ingress-nginx "$CHARTS/ingress-nginx-4.15.1.tgz" -n ingress-nginx --create-namespace -f "$WORK/a15-values.yaml" \
    --set controller.kind=DaemonSet --set controller.hostPort.enabled=true --set controller.service.enabled=false \
    --set controller.watchIngressWithoutClass=true --set controller.admissionWebhooks.enabled=false \
    --set controller.image.digest=sha256:594ceea76b01c592858f803f9ff4d2cb40542cae2060410b2c95f75907d659e1 --wait --timeout 10m >/dev/null
  for n in $(nodes | awk '{print $1}'); do ready ingress-nginx app.kubernetes.io/name=ingress-nginx "$n"; done
  sleep 10
  probe_all a15
  $H uninstall ingress-nginx -n ingress-nginx --wait >/dev/null
  for n in $(nodes | awk '{print $1}'); do gone ingress-nginx app.kubernetes.io/name=ingress-nginx "$n"; done
fi

# ---------------------------------------------------- rke2-ingress-nginx (A)
rke2_values() { ingress_nginx_values; cat <<YAML
global:
  systemDefaultIngressClass: ingress-nginx
YAML
}
if phase rke2; then
  say "rke2-ingress-nginx 4.12.600 as RKE2 installs it"
  rke2_values > "$WORK/rke2-values.yaml"
  $H upgrade --install rke2-ingress-nginx "$CHARTS/rke2-ingress-nginx-4.12.600.tgz" -n kube-system -f "$WORK/rke2-values.yaml" \
    --set controller.image.digest=sha256:73db650d9b44b6abc208c6a45d07ef1ab59dae139a86d484c01dcd831bbbd614 --wait --timeout 10m >/dev/null
  for n in $(nodes | awk '{print $1}'); do ready kube-system app.kubernetes.io/name=rke2-ingress-nginx "$n"; done
  check "the admission webhook of RKE2, failurePolicy Fail" \
    "$($K get validatingwebhookconfiguration rke2-ingress-nginx-admission -o jsonpath='{.webhooks[0].failurePolicy}')" "Fail"
  sleep 10
  probe_all a
fi

# webhook_fail: step 5 of the runbook for wxingress's webhook - Fail, every Ingress there is passes, an invalid one is refused
webhook_fail() {
  $H upgrade wxingress "$CHARTS/wxingress-chart.tgz" -n wxingress --reuse-values --set controller.admissionWebhooks.failurePolicy=Fail >/dev/null
  check "step 5: wxingress's webhook, Fail" \
    "$($K get validatingwebhookconfiguration wxingress-controller-admission -o jsonpath='{.webhooks[0].failurePolicy}')" "Fail"
  $K get ingress -A -o json | jq "del(.items[].metadata.resourceVersion)" | $K apply --dry-run=server -f - > "$WORK/step5-dry-run.txt" 2>&1
  check "step 5: every Ingress of the corpus passes the webhooks as it is" \
    "$(grep -c 'denied the request\|^Error' "$WORK/step5-dry-run.txt")" "0"
  grep 'denied the request\|^Error' "$WORK/step5-dry-run.txt" | cut -c1-300 | head -5
  check "step 5: wxingress's webhook answers and refuses an invalid Ingress" \
    "$(printf 'apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata: {name: webhook-check, namespace: inx-annotations, annotations: {nginx.org/proxy-connect-timeout: soon}}\nspec:\n  ingressClassName: nginx\n  rules: [{host: webhook-check.parity.test, http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: echo, port: {number: 80}}}}]}}]\n' | $K apply --dry-run=server -f - 2>&1 | grep -c 'validate.wxingress.wizardtales.com" denied the request.*proxy-connect-timeout')" "1"
  webhook_failed=1
}
webhook_failed=0

# ---------------------------------------------------- the runbook, under load
if phase cutover; then
  say "docs/migration/rke2.md under load"
  P sh -c 'rm -f /tmp/stop /tmp/load.json'
  # the load: the probes that ingress-nginx answered without a 5xx
  first=$(ips | awk '{print $1}')
  python3 -c '
import json, sys
probes, seen = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
ok = [p for p in probes if p.get("load") and seen.get(p["id"], {}).get("status", 599) < 500]
json.dump(ok, open(sys.argv[3], "w"))
print(len(ok), "probes in the load")' "$WORK/probes.json" "$WORK/a-$first.json" "$WORK/load-probes.json"
  $K -n $B cp "$WORK/load-probes.json" probe:/p/load-probes.json >/dev/null
  if [ -n "$LB" ]; then
    lb_up
    # wrk's request: a GET without headers or body that rke2-ingress-nginx answered with 200 over http and https
    set -- $(python3 -c '
import json, sys
probes, seen = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
ok = {}
for p in probes:
    if p.get("method", "GET") == "GET" and not p.get("headers") and not p.get("body") and not p.get("sni") \
            and seen.get(p["id"], {}).get("status") == 200:
        ok.setdefault((p["host"], p["path"]), set()).add(p["scheme"])
print(*sorted(k for k, v in ok.items() if v == {"http", "https"})[0])' "$WORK/probes.json" "$WORK/a-$first.json")
    wrkhost=$1 wrkpath=$2
    say "balancer $lbip in front of the nodes; wrk with keepalive to $wrkhost$wrkpath"
  fi
  rm -f "$WORK/phases.txt"
  pool "$(ips)"
  P sh -c 'nohup python3 /p/probe.py load /tmp/pool /p/load-probes.json /tmp/load.json /tmp/stop >/tmp/load.out 2>&1 &'
  sleep 5
  loadstat() { P cat /tmp/load.json 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["sent"], d["failed"])'; }
  lostnow() { loadstat | awk '{print $2}'; }
  say "load: $(loadstat)"

  # what the runbook avoids: a rolling restart of a hostPort DaemonSet, no node out of the balancer
  wrk_phase rolling
  before=$(lostnow)
  $K -n kube-system rollout restart ds/rke2-ingress-nginx-controller >/dev/null
  $K -n kube-system rollout status ds/rke2-ingress-nginx-controller --timeout=20m >/dev/null 2>&1
  sleep 5
  rolled=$(( $(lostnow) - before ))
  say "a RollingUpdate restart of rke2-ingress-nginx under the load: $rolled failed requests (why step 1 sets OnDelete)"
  echo "$rolled" > "$WORK/rolling-restart-failures"
  start=$(lostnow)

  wrk_phase step1
  # step 1: rke2-ingress-nginx keeps off labelled nodes (the HelmChartConfig; here helm, which is what helm-controller runs)
  $H upgrade rke2-ingress-nginx "$CHARTS/rke2-ingress-nginx-4.12.600.tgz" -n kube-system --reuse-values -f "$WORK/rke2-values.yaml" \
    -f "$here/../../deployments/rke2-cutover/rke2-step1-values.yaml" >/dev/null
  $K -n kube-system rollout status ds/rke2-ingress-nginx-controller --timeout=20m >/dev/null 2>&1
  say "step 1 done; load: $(loadstat)"
  check "step 1 rolls no pod: OnDelete" "$($K -n kube-system get ds rke2-ingress-nginx-controller -o jsonpath='{.spec.updateStrategy.type}')" "OnDelete"

  wrk_phase step2
  # step 2: wxingress, no pod yet
  $KIND load docker-image "$WX" --name "$NAME" >/dev/null 2>&1
  $H upgrade --install wxingress "$CHARTS/wxingress-chart.tgz" -n wxingress --create-namespace -f "$here/../../deployments/rke2-cutover/values.yaml" \
    --set controller.image.repository="${WX%:*}" --set controller.image.tag="${WX#*:}" --set controller.image.pullPolicy=Never \
    --set prometheus.serviceMonitor.create=false --set controller.admissionWebhooks.failurePolicy=Ignore \
    --set-json 'controller.tolerations=[{"key":"app","operator":"Exists"},{"key":"customer","operator":"Exists"},{"key":"gitlab","operator":"Exists"},{"key":"node-role.kubernetes.io/control-plane","operator":"Exists"}]' \
    --set-string controller.config.entries.global-allowed-response-headers=X-Inx-Custom >/dev/null
  check "step 2: DESIRED 0" "$($K -n wxingress get ds wxingress-controller -o jsonpath='{.status.desiredNumberScheduled}')" "0"
  check "step 2: wxingress's webhook, Ignore while no pod answers it" \
    "$($K get validatingwebhookconfiguration wxingress-controller-admission -o jsonpath='{.webhooks[0].failurePolicy}')" "Ignore"
  check "step 2: an Ingress can be written while no wxingress pod runs" \
    "$(printf 'apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata: {name: early, namespace: inx-annotations}\nspec:\n  ingressClassName: nginx\n  rules: [{host: early.parity.test, http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: echo, port: {number: 80}}}}]}}]\n' | $K apply --dry-run=server -f - 2>&1 | grep -c created)" "1"

  # steps 3 and 4: node by node - out of the balancer, labelled, checked against its probes before, back
  # switch <node> <wxingress|rke2>: the node to that controller, the runbook's way (to rke2: "The way back for one node")
  switch() {
    ip=$(nodes | awk -v n="$1" '$1 == n {print $2}')
    pool "$(ips | tr ' ' '\n' | grep -vx "$ip" | paste -sd' ' -)"
    sleep 3
    if [ "$2" = wxingress ]; then
      $K label node "$1" wx1.eu/ingress=wxingress --overwrite >/dev/null
      gone kube-system app.kubernetes.io/name=rke2-ingress-nginx "$1"
      if ready wxingress app.kubernetes.io/instance=wxingress "$1"; then w=up; else w=down; fi
    else
      $K label node "$1" wx1.eu/ingress- >/dev/null
      gone wxingress app.kubernetes.io/instance=wxingress "$1"
      if ready kube-system app.kubernetes.io/name=rke2-ingress-nginx "$1"; then w=up; else w=down; fi
    fi
    check "step 3/4: $1 runs $2" "$w" "up"
    sleep 3
    P python3 /p/probe.py probe "$ip" /p/probes.json "/tmp/node-$1-$2.json" >/dev/null
    $K -n $B cp "probe:/tmp/node-$1-$2.json" "$WORK/node-$1-$2.json" >/dev/null 2>&1
    python3 "$here/parity/diff.py" "$here/ingress-nginx-parity.allow" "$WORK/a-$ip.json" "$WORK/node-$1-$2.json" \
      "rke2-ingress-nginx 1.12.6" > "$WORK/diff-node-$1-$2.txt"
    check "step 3/4: $1 with $2 answers its probes as before ($(tail -1 "$WORK/diff-node-$1-$2.txt"))" \
      "$(grep -c '^failing 0$' "$WORK/diff-node-$1-$2.txt")" "1"
    pool "$(ips)"
    say "$1 on $2; load: $(loadstat)"
  }
  all=$(nodes | awk '{print $1}')
  first=$(echo "$all" | head -1)
  last=$(echo "$all" | tail -1)
  for n in $all; do
    if [ "$n" = "$last" ]; then
      wrk_phase step5-webhooks
      # step 5: wxingress's webhook to Fail and checked where a node runs it (on one node: after its switch); then
      # rke2-ingress-nginx's goes before its last pod
      if [ -n "$($K -n wxingress get pod -l app.kubernetes.io/instance=wxingress -o name 2>/dev/null)" ]; then webhook_fail; fi
      $H upgrade rke2-ingress-nginx "$CHARTS/rke2-ingress-nginx-4.12.600.tgz" -n kube-system --reuse-values \
        --set controller.admissionWebhooks.enabled=false >/dev/null
      check "step 5: the webhook is gone" "$($K get validatingwebhookconfiguration rke2-ingress-nginx-admission -o name 2>&1 | grep -c NotFound)" "1"
    fi
    wrk_phase "switch-$n"
    switch "$n" wxingress
    # the way back for one node, while others still run rke2-ingress-nginx, and over again
    if [ "$n" = "$first" ] && [ "$n" != "$last" ]; then
      wrk_phase "back-$n"
      switch "$n" rke2
      wrk_phase "again-$n"
      switch "$n" wxingress
    fi
  done
  wrk_phase written
  [ "$webhook_failed" = 1 ] || webhook_fail
  check "an Ingress can be written through wxingress's webhook alone" \
    "$(printf 'apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata: {name: written-%s, namespace: inx-annotations}\nspec:\n  ingressClassName: nginx\n  rules: [{host: written.parity.test, http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: echo, port: {number: 80}}}}]}}]\n' "$(date +%s)" | $K apply -f - 2>&1 | grep -c created)" "1"

  if [ "$BACK" = 1 ]; then
    # "The way back, all of it", before step 7: rke2-ingress-nginx's webhook back with Ignore (its pods serve it only
    # once restarted, and OnDelete restarts none), wxingress's Ignore; node by node back to rke2-ingress-nginx, whose
    # new pods serve the webhook; then it is Fail again and wxingress goes
    wrk_phase back-webhooks
    $H upgrade rke2-ingress-nginx "$CHARTS/rke2-ingress-nginx-4.12.600.tgz" -n kube-system --reuse-values \
      --set controller.admissionWebhooks.enabled=true --set controller.admissionWebhooks.failurePolicy=Ignore >/dev/null
    $H upgrade wxingress "$CHARTS/wxingress-chart.tgz" -n wxingress --reuse-values --set controller.admissionWebhooks.failurePolicy=Ignore >/dev/null
    check "the way back: rke2-ingress-nginx's webhook, Ignore" \
      "$($K get validatingwebhookconfiguration rke2-ingress-nginx-admission -o jsonpath='{.webhooks[0].failurePolicy}')" "Ignore"
    for n in $all; do
      wrk_phase "back-$n"
      switch "$n" rke2
    done
    wrk_phase back-end
    $H upgrade rke2-ingress-nginx "$CHARTS/rke2-ingress-nginx-4.12.600.tgz" -n kube-system --reuse-values \
      --set controller.admissionWebhooks.failurePolicy=Fail >/dev/null
    check "the way back: rke2-ingress-nginx's webhook, Fail" \
      "$($K get validatingwebhookconfiguration rke2-ingress-nginx-admission -o jsonpath='{.webhooks[0].failurePolicy}')" "Fail"
    check "the way back: every Ingress of the corpus passes the webhooks as it is" \
      "$($K get ingress -A -o json | jq "del(.items[].metadata.resourceVersion)" | $K apply --dry-run=server -f - 2>&1 | grep -c 'denied the request\|^Error')" "0"
    check "the way back: rke2-ingress-nginx's webhook answers and refuses a snippet" \
      "$(printf 'apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata: {name: webhook-check, namespace: inx-annotations, annotations: {nginx.ingress.kubernetes.io/configuration-snippet: "more_set_headers x: y;"}}\nspec:\n  ingressClassName: nginx\n  rules: [{host: webhook-check.parity.test, http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: echo, port: {number: 80}}}}]}}]\n' | $K apply --dry-run=server -f - 2>&1 | grep -c 'validate.nginx.ingress.kubernetes.io" denied the request')" "1"
    $H uninstall wxingress -n wxingress --wait >/dev/null
    check "the way back: wxingress's webhook is gone" "$($K get validatingwebhookconfiguration wxingress-controller-admission -o name 2>&1 | grep -c NotFound)" "1"
    sleep 5
    say "the way back done; load: $(loadstat)"
  else
    # step 7: the status, the end of rke2-ingress-nginx, the IngressClass taken over
    wrk_phase step7-status
    status_ips=$(ips | tr ' ' ',' | sed 's/,$//')
    $H upgrade wxingress "$CHARTS/wxingress-chart.tgz" -n wxingress --reuse-values --set controller.reportIngressStatus.enable=true \
      --set-json "controller.config.entries.external-status-address=\"$status_ips\"" >/dev/null
    restart_drained
    w=0
    while [ "$w" -lt 120 ] && [ "$($K -n inx-annotations get ingress rewrite -o jsonpath='{.status.loadBalancer.ingress[*].ip}' | tr ' ' ',')" != "$status_ips" ]; do sleep 2; w=$((w + 2)); done
    check "step 7: the status is the nodes' addresses" "$($K -n inx-annotations get ingress rewrite -o jsonpath='{.status.loadBalancer.ingress[*].ip}' | tr ' ' ',')" "$status_ips"
    # disable: rke2-ingress-nginx - what RKE2 does then is uninstall the release
    wrk_phase step7-rke2-gone
    $H uninstall rke2-ingress-nginx -n kube-system --wait >/dev/null
    check "the IngressClass nginx went with it" "$($K get ingressclass nginx -o name 2>&1 | grep -c NotFound)" "1"
    sleep 5
    check "the Ingresses of nginx are served without it" \
      "$(P python3 -c "import http.client as h; c=h.HTTPConnection('$(ips | awk '{print $1}')', 80, timeout=5); c.request('GET', '/', headers={'Host': 'approot.annotations.parity.test'}); print(c.getresponse().status)")" "302"
    wrk_phase step7-class
    $H upgrade wxingress "$CHARTS/wxingress-chart.tgz" -n wxingress --reuse-values --set controller.ingressClass.create=true \
      --set controller.ingressClass.setAsDefaultIngress=false >/dev/null
    restart_drained
    w=0
    while [ "$w" -lt 60 ] && ! $K get ingressclass nginx >/dev/null 2>&1; do sleep 2; w=$((w + 2)); done
    check "the chart's IngressClass nginx" "$($K get ingressclass nginx -o jsonpath='{.spec.controller}')" "nginx.wx1.eu/ingress-controller"
    sleep 10
    say "step 7 done; load: $(loadstat)"
  fi
  wrk_phase
  P touch /tmp/stop
  sleep 3
  P cat /tmp/load.json > "$WORK/load.json"
  sent=$(python3 -c 'import json; print(json.load(open("'"$WORK/load.json"'"))["sent"])')
  lost=$(( $(python3 -c 'import json; print(json.load(open("'"$WORK/load.json"'"))["failed"])') - start ))
  check "the load through the runbook: $sent requests, none failed" "$lost" "0"
  [ "$lost" = 0 ] || python3 -c 'import json; [print("  ", f) for f in json.load(open("'"$WORK/load.json"'"))["failures"][:20]]'
  if [ -n "$LB" ]; then
    # phase, the probe load's requests and failures, wrk's over http and over https
    column -t "$WORK/phases.txt"
    # not counted: rolling, what the runbook avoids, and written - the Ingress written reloads nginx on every node, and
    # a reload closes the idle keepalive connections, so a request sent on one as it closes gets a read error; that is
    # nginx's reload, rke2-ingress-nginx's alike (11 reloads under the same wrk on one node: 27 read errors for it, 17
    # for wxingress), not the switch
    check "wrk with keepalive through the balancer and the runbook: $(awk '$1 != "rolling" && $1 != "written" {r += $6 + $9} END {print r}' "$WORK/phases.txt") requests, none failed" \
      "$(awk '$1 != "rolling" && $1 != "written" {e += $7 + $10} END {print e + 0}' "$WORK/phases.txt")" "0"
    $D rm -f "$NAME-lb" >/dev/null 2>&1
  fi
fi

# ------------------------------------------------------------------- after (B)
if phase after; then
  sleep 5
  probe_all b
fi

# ------------------------------------------------------------------- the diff
if phase diff; then
  for ip in $(ips); do
    python3 "$here/parity/diff.py" "$here/ingress-nginx-parity.allow" "$WORK/a-$ip.json" "$WORK/b-$ip.json" "rke2-ingress-nginx 1.12.6" > "$WORK/diff-a-$ip.txt"
    tail -1 "$WORK/diff-a-$ip.txt"
    grep -q '^failing 0$' "$WORK/diff-a-$ip.txt" || { grep '^FAIL' "$WORK/diff-a-$ip.txt" | head -40; failed=$((failed + 1)); }
    if [ -f "$WORK/a15-$ip.json" ]; then
      python3 "$here/parity/diff.py" "$here/ingress-nginx-parity.allow" "$WORK/a15-$ip.json" "$WORK/b-$ip.json" "ingress-nginx 1.15.1" > "$WORK/diff-a15-$ip.txt"
      tail -1 "$WORK/diff-a15-$ip.txt"
    fi
  done
fi

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
