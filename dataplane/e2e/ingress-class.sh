#!/bin/sh
#
# Which Ingresses wxingress serves, as ingress-nginx would in its place: those
# of every IngressClass whose controller is in -ingress-class-controllers -
# here k8s.io/ingress-nginx besides wxingress's own - by the field or the
# kubernetes.io/ingress.class annotation, those without a class
# (-watch-ingress-without-class), not those of another controller's class; an
# Ingress with only spec.defaultBackend is a catch-all, and one with the same
# host twice is served with the paths of both rules.
#
# Runs where kubectl and the kind node's docker are, against a release of
# charts/wxingress installed as for gateway.sh and with
#
#   --set controller.ingressClass.controllers={k8s.io/ingress-nginx}
#   --set controller.watchIngressWithoutClass=true
#   --set controller.allowEmptyIngressHost=true
#
#   RELEASE_NS=wxgw dataplane/e2e/ingress-class.sh
#
# It makes the namespace ic-e2e and the IngressClasses ic-legacy
# (k8s.io/ingress-nginx) and ic-foreign (example.com/other), and removes them.
#
#   KUBECTL     how to call kubectl ("sudo kubectl")
#   DOCKER      how to call docker ("sudo docker")
#   NODE        the kind node (wxingress-control-plane)
#   RELEASE_NS  the namespace of the release (wxgw)

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxgw}
N=ic-e2e

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
ctrl=$($K -n "$ns" get svc -l app.kubernetes.io/instance="$ns" -o jsonpath='{.items[0].spec.clusterIP}')
# get <host> [path]: the pod that answers, or the status
get() {
  out=$(X curl -s --max-time 3 -w '\n%{http_code}' -H "Host: $1" "http://$ctrl${2:-/hostname}")
  code=$(echo "$out" | tail -1)
  if [ "$code" = "200" ]; then echo "$out" | head -1 | sed 's/-[^-]*-[^-]*$//'; else echo "$code"; fi
}

$K delete ns $N --wait=true >/dev/null 2>&1
$K delete ingressclass ic-legacy ic-foreign >/dev/null 2>&1
$K create ns $N >/dev/null

cat <<YAML | $K apply -f - >/dev/null
---
apiVersion: networking.k8s.io/v1
kind: IngressClass
metadata: {name: ic-legacy}
spec: {controller: k8s.io/ingress-nginx}
---
apiVersion: networking.k8s.io/v1
kind: IngressClass
metadata: {name: ic-foreign}
spec: {controller: example.com/other}
---
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
YAML

ingress() { # name class-field class-annotation host
  cat <<YAML
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: $1
  namespace: $N
  annotations: {${3:+kubernetes.io/ingress.class: $3}}
spec:
  ${2:+ingressClassName: $2}
  rules:
  - host: $4
    http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: app, port: {number: 80}}}}]}
YAML
}
{
  ingress noclass "" "" noclass.ic.test
  ingress legacy ic-legacy "" legacy.ic.test
  ingress annotated "" ic-legacy annotated.ic.test
  ingress foreign ic-foreign "" foreign.ic.test
  cat <<YAML
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: twice, namespace: $N}
spec:
  rules:
  - host: twice.ic.test
    http: {paths: [{path: /hostname, pathType: Exact, backend: {service: {name: app, port: {number: 80}}}}]}
  - host: twice.ic.test
    http: {paths: [{path: /echo, pathType: Prefix, backend: {service: {name: app, port: {number: 80}}}}]}
YAML
} | $K apply -f - >/dev/null
$K -n $N rollout status deploy/app --timeout=120s >/dev/null
waited=0
served() { echo "$(get noclass.ic.test) $(get legacy.ic.test) $(get annotated.ic.test) $(get twice.ic.test)"; }
while [ "$waited" -lt 60 ] && [ "$(served)" != "app app app app" ]; do sleep 1; waited=$((waited + 1)); done
echo "         served after ${waited}s"

check "an Ingress without a class (-watch-ingress-without-class)" "$(get noclass.ic.test)" "app"
check "one of an IngressClass of k8s.io/ingress-nginx, by the field" "$(get legacy.ic.test)" "app"
check "and by the kubernetes.io/ingress.class annotation" "$(get annotated.ic.test)" "app"
check "one of another controller's class is not served" "$(get foreign.ic.test)" "404"
check "the same host twice: the first rule's path" "$(get twice.ic.test)" "app"
check "and the second's" "$(get twice.ic.test '/echo?msg=second')" "second"

# an Ingress with only spec.defaultBackend: every host no other Ingress has (also the foreign one's), with no
# redirect to https although the default server it becomes has the default certificate
check "without it, a host no Ingress has is not served" "$(get anything.else.test)" "404"
$K apply -f - >/dev/null <<YAML
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: catchall, namespace: $N}
spec:
  defaultBackend: {service: {name: app, port: {number: 80}}}
YAML
waited=0
while [ "$waited" -lt 60 ] && [ "$(get anything.else.test)" != "app" ]; do sleep 1; waited=$((waited + 1)); done
check "an Ingress with only spec.defaultBackend is the catch-all, over http without a redirect (${waited}s)" "$(get anything.else.test)" "app"
$K -n $N delete ingress catchall >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(get anything.else.test)" != "404" ]; do sleep 1; waited=$((waited + 1)); done

# the class goes over to another controller: its Ingresses are not served any more, without a restart
$K delete ingressclass ic-legacy >/dev/null
$K create -f - >/dev/null <<YAML
apiVersion: networking.k8s.io/v1
kind: IngressClass
metadata: {name: ic-legacy}
spec: {controller: example.com/other}
YAML
waited=0
# every Ingress of the cluster is looked at again, behind the sync rate limit
while [ "$waited" -lt 90 ] && [ "$(get legacy.ic.test)" != "404" ]; do sleep 1; waited=$((waited + 1)); done
check "the IngressClass now another controller's: its Ingress is not served (${waited}s)" "$(get legacy.ic.test)" "404"

[ -n "${KEEP:-}" ] || {
  $K delete ns $N --wait=false >/dev/null 2>&1
  $K delete ingressclass ic-legacy ic-foreign >/dev/null 2>&1
}

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
