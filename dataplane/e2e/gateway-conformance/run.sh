#!/bin/sh
#
# The Gateway API conformance suite against a release of charts/wxingress on
# kind, with the Gateway profiles. The suite runs inside the kind node,
# where the Gateways' addresses - the cluster IPs of their Services -
# are reachable:
#
#   dataplane/e2e/gateway-conformance/run.sh build    # here: the test binary
#   CLASS=wxgw dataplane/e2e/gateway-conformance/run.sh run   # where docker is
#
# The suite's Gateways must have addresses of their own: none of them may be
# the release's -default-gateway, which shares the controller's address with
# Ingresses - a deliberate deviation from the Gateway API
# (docs/developer/upstream.md).
#
# run leaves the report in conformance-report.yaml and the log in
# conformance.log next to the binary. The suite makes its namespaces
# gateway-conformance-* and removes them again.
#
#   DOCKER    how to call docker ("sudo docker")
#   NODE      the kind node (wxingress-control-plane)
#   CLASS     the GatewayClass of the release (wxgw)
#   PROFILES  the profiles to run (all the Gateway profiles: GATEWAY-HTTP,
#             GATEWAY-GRPC, GATEWAY-TLS, GATEWAY-TCP, GATEWAY-UDP)
#   FEATURES  the features to test (default: those internal/gateway/features.go
#             claims, which the GatewayClass reports)
#   ARGS      more arguments for the suite, e.g. "-run-test=HTTPRouteSimpleSameNamespace"

set -u
here=$(cd "$(dirname "$0")" && pwd)
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
class=${CLASS:-wxgw}
claimed=$(grep -o 'features\.Support[A-Za-z0-9]*' "$here/../../../internal/gateway/features.go" | sed 's/features\.Support//' | paste -sd, -)
features=${FEATURES:-$claimed}

case "${1:-}" in
build)
  cd "$here" && CGO_ENABLED=0 go test -c -o conformance.test .
  ;;
run)
  # the node's /tmp is a tmpfs, which docker cp does not reach
  $D cp "$here/conformance.test" "$node:/root/conformance.test" >/dev/null
  # shellcheck disable=SC2086
  $D exec -e KUBECONFIG=/etc/kubernetes/admin.conf "$node" /root/conformance.test \
    -test.v -test.timeout=60m \
    -gateway-class="$class" \
    -conformance-profiles="${PROFILES:-GATEWAY-HTTP,GATEWAY-GRPC,GATEWAY-TLS,GATEWAY-TCP,GATEWAY-UDP}" \
    -supported-features="$features" \
    -organization=wxone -project=wxingress -url=https://github.com/wx-one/wxingress \
    -version="$(git -C "$here" describe --always 2>/dev/null || echo local)" -contact=https://github.com/wx-one/wxingress \
    -report-output=/root/conformance-report.yaml \
    ${ARGS:-} >"$here/conformance.log" 2>&1
  status=$?
  $D cp "$node:/root/conformance-report.yaml" "$here/conformance-report.yaml" >/dev/null 2>&1
  $D exec "$node" rm -f /root/conformance.test /root/conformance-report.yaml
  grep -E '^    --- (PASS|FAIL)' "$here/conformance.log" | sed 's/ (.*//' | sort
  # the tests that passed before must pass (passing.txt, docs/developer/upstream.md)
  grep -E '^    --- PASS' "$here/conformance.log" | sed 's/ (.*//; s/.*TestConformance\///' | sort >"$here/passed.txt"
  lost=$(comm -23 "$here/passing.txt" "$here/passed.txt")
  if [ -n "$lost" ]; then
    echo "these passed before and do not now:"
    echo "$lost"
    status=1
  fi
  exit $status
  ;;
*)
  echo "usage: $0 build|run" >&2
  exit 2
  ;;
esac
