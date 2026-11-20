#!/bin/sh
#
# The admission webhook (docs/user-guide/ingress-nginx-compatibility.md, "The admission webhook"):
# the ValidatingWebhookConfiguration of the chart with the CA of its patch
# job, an Ingress that is valid admitted, and refused one that is invalid,
# one whose host and path another Ingress has (ingress-nginx's checkOverlap),
# one with a snippet while snippets are off, and one whose configuration
# passes the validation but not nginx -t; a dry run checks without creating,
# a canary may share its primary's path, an Ingress of another class is let
# through, and check_success/check_errors count.
#
# Runs where kubectl and the kind node's docker are, against a release of
# charts/wxingress installed as for gateway.sh, with
#
#   --set controller.admissionWebhooks.enabled=true
#
#   RELEASE_NS=wxgw CLASS=wxgw dataplane/e2e/admission-webhook.sh
#
# It makes the namespace aw-e2e and removes it.
#
#   KUBECTL       how to call kubectl ("sudo kubectl")
#   DOCKER        how to call docker ("sudo docker")
#   NODE          the kind node (wxingress-control-plane)
#   RELEASE_NS    the namespace of the release (wxgw)
#   CLASS         the IngressClass of the release (wxgw)
#   METRICS_PORT  the port of its metrics (9113), for check_success and check_errors

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxgw}
class=${CLASS:-wxgw}
port=${METRICS_PORT:-9113}
N=aw-e2e

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
webhook=$($K get validatingwebhookconfiguration -l app.kubernetes.io/instance="$ns" -o name | head -1)

ingress() { # name host path [annotation=value ...]: an Ingress of the class to the Service app
  name=$1 host=$2 path=$3
  shift 3
  echo "apiVersion: networking.k8s.io/v1"
  echo "kind: Ingress"
  echo "metadata:"
  echo "  name: $name"
  echo "  namespace: $N"
  echo "  annotations:"
  echo "    wxingress-e2e: \"admission\""
  for a in "$@"; do
    echo "    ${a%%=*}: '${a#*=}'"
  done
  echo "spec:"
  echo "  ingressClassName: $class"
  echo "  rules:"
  echo "  - host: $host"
  echo "    http: {paths: [{path: '$path', pathType: ImplementationSpecific, backend: {service: {name: app, port: {number: 80}}}}]}"
}
# apply <args>: kubectl apply of stdin, its output and errors on one line
apply() { $K apply "$@" -f - 2>&1 | tr '\n' ' '; }

$K delete ns $N --wait=true >/dev/null 2>&1
$K create ns $N >/dev/null
cat <<YAML | $K apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata: {name: app, namespace: $N}
spec: {selector: {app: app}, ports: [{port: 80, targetPort: 8080}]}
YAML

echo "--- the webhook"
check "a ValidatingWebhookConfiguration of the release" "$(echo "$webhook" | grep -c validatingwebhookconfiguration)" "1"
check "  failurePolicy Fail, as ingress-nginx's" "$($K get "$webhook" -o jsonpath='{.webhooks[0].failurePolicy}')" "Fail"
check "  with the CA the patch job put in" \
  "$($K get "$webhook" -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | base64 -d 2>/dev/null | grep -c 'BEGIN CERTIFICATE')" "1"
check "  for Ingresses created and updated" "$($K get "$webhook" -o jsonpath='{.webhooks[0].rules[0].operations}')" '["CREATE","UPDATE"]'

echo "--- admitted"
out=$(ingress shop shop.aw.test / | apply)
check "a valid Ingress is admitted" "$(echo "$out" | grep -c 'created')" "1"
out=$(ingress shop shop.aw.test / nginx.org/proxy-read-timeout=30s | apply)
check "and an update of it" "$(echo "$out" | grep -c 'configured')" "1"
out=$(ingress shop-canary shop.aw.test / nginx.ingress.kubernetes.io/canary=true nginx.ingress.kubernetes.io/canary-weight=10 | apply)
check "a canary with the host and path of its primary" "$(echo "$out" | grep -c 'created')" "1"
out=$(ingress other shop.aw.test /other | apply)
check "another path of the host, merged into its server" "$(echo "$out" | grep -c 'created')" "1"
out=$(ingress dry dry.aw.test / | apply --dry-run=server)
check "a server-side dry run is checked and creates nothing" \
  "$(echo "$out" | grep -c 'created (server dry run)')$($K -n $N get ingress dry -o name 2>/dev/null | wc -l)" "10"
out=$(ingress foreign shop.aw.test / | sed "s/ingressClassName: $class/ingressClassName: not-$class/" | apply)
check "an Ingress of another class is let through, whatever it has" "$(echo "$out" | grep -c 'created')" "1"

echo "--- refused"
out=$(ingress bad bad.aw.test / nginx.org/proxy-connect-timeout=soon | apply)
check "an invalid one, with what is invalid" \
  "$(echo "$out" | grep -c 'validate.wxingress.wizardtales.com.*denied.*nginx.org/proxy-connect-timeout')" "1"
out=$(ingress dup shop.aw.test / | apply)
check "one with the host and path of another, as ingress-nginx says it" \
  "$(echo "$out" | grep -c "and path \"/\" is already defined in ingress $N/shop")" "1"
out=$(ingress snippet snippet.aw.test / nginx.ingress.kubernetes.io/configuration-snippet='return 200;' | apply)
check "one with a snippet while snippets are disabled" \
  "$(echo "$out" | grep -c 'configuration-snippet annotation cannot be used')" "1"
out=$(ingress regex regex.aw.test '/(?<n>a)(?<n>b)' nginx.ingress.kubernetes.io/use-regex=true | apply)
check "one that passes the validation but whose configuration fails nginx -t" \
  "$(echo "$out" | grep -c 'fails nginx -t.*two named subpatterns have the same name')" "1"
check "  none of them is there" "$($K -n $N get ingress bad dup snippet regex -o name 2>/dev/null | wc -l)" "0"
out=$(ingress shop shop.aw.test / nginx.org/proxy-read-timeout=never | apply)
check "an update that would make it invalid" "$(echo "$out" | grep -c 'denied')" "1"
check "  leaves it as it was" "$($K -n $N get ingress shop -o jsonpath='{.metadata.annotations.nginx\.org/proxy-read-timeout}')" "30s"

echo "--- counted"
metrics() { X curl -s --max-time 10 "http://$podip:$port/metrics"; }
check "check_success of the admitted, per namespace and Ingress" \
  "$(metrics | grep "^nginx_ingress_controller_check_success{" | grep -F "namespace=\"$N\"" | grep -cF 'ingress="shop"')" "1"
check "check_errors of the refused" \
  "$(metrics | grep "^nginx_ingress_controller_check_errors{" | grep -F "namespace=\"$N\"" | grep -cF 'ingress="regex"')" "1"

[ -n "${KEEP:-}" ] || $K delete ns $N --wait=false >/dev/null 2>&1

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
