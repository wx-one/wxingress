#!/usr/bin/env bash
# Copy the Gateway API graph of NGINX Gateway Fabric (NGF) into wxingress.
#
#   hack/sync-ngf.sh <tag>          e.g. hack/sync-ngf.sh v2.8.0
#   NGF_SRC=/path/to/checkout hack/sync-ngf.sh <tag>   (no clone; HEAD must be <tag>)
#
# What it does, in order:
#   1. clones nginx/nginx-gateway-fabric at <tag> into a temporary directory
#      (or uses NGF_SRC),
#   2. computes the Go package closure of the roots below with `go list`
#      inside the NGF checkout - the closure, not a fixed list, so a new NGF
#      version brings what its graph needs,
#   3. refuses to continue if the closure contains anything on the deny
#      list (telemetry, usage reporting, licensing, the agent, the
#      provisioner, NGF's own nginx config generation),
#   4. replaces internal/third_party/ngf/ with the closure: every package's
#      .go files (tests included), import paths rewritten from
#      github.com/nginx/nginx-gateway-fabric/v2/{internal/,} to
#      github.com/wx-one/wxingress/internal/third_party/ngf/,
#      plus NGF's LICENSE and a VERSION file (tag, commit, package list),
#   5. applies hack/ngf-patches/*.patch in name order (listed with their
#      reasons in docs/developer/upstream.md),
#   6. raises go.mod to at least NGF's versions of the modules the closure
#      shares with it (gateway-api pinned to exactly NGF's version, see 2.3),
#      runs go mod tidy,
#   7. writes the standard channel CRDs of that gateway-api version to
#      deployments/gateway-api/standard-install.yaml, and builds and tests
#      the copied packages.
#
# Nothing under internal/third_party/ngf/ is edited by hand. Re-running with
# the same tag leaves the tree unchanged.
set -euo pipefail

tag=${1:?usage: hack/sync-ngf.sh <ngf tag, e.g. v2.8.0>}
repo=$(cd "$(dirname "$0")/.." && pwd)
dest=$repo/internal/third_party/ngf
patches=$repo/hack/ngf-patches
ngf_mod=github.com/nginx/nginx-gateway-fabric/v2
our_mod=github.com/wx-one/wxingress/internal/third_party/ngf
url=${NGF_URL:-https://github.com/nginx/nginx-gateway-fabric.git}

# Packages whose closure is copied:
#   graph       Gateway API semantics: attachment, hostnames, ReferenceGrants,
#               conflicts, the status conditions of every object
#   dataplane   the graph as data-plane-neutral servers, path and match rules
#               in Gateway API precedence, backend groups with weights
#   validation  NGF's checks of route fields against nginx, which the graph
#               takes as an interface (our data plane is nginx as well)
#   status      the Gateway API status of GatewayClasses, Gateways and routes
#               as built from the graph; internal/gateway writes it with
#               client-go (NGF's own writer, controller-runtime, stays unused)
roots=(
	internal/controller/state/graph
	internal/controller/state/dataplane
	internal/controller/nginx/config/validation
	internal/controller/status
)

# Path fragments that must never be part of the copy. A match aborts the sync.
deny_re='(^|/)(telemetry|licensing|usage|productusage|provisioner|agent|nginx/config)$|(^|/)(telemetry|licensing|usage|productusage|provisioner|agent)/'

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if [[ -n ${NGF_SRC:-} ]]; then
	src=$NGF_SRC
else
	src=$work/ngf
	git -c advice.detachedHead=false clone -q --depth 1 --branch "$tag" "$url" "$src"
fi
commit=$(git -C "$src" rev-parse HEAD)
if [[ $(git -C "$src" rev-parse "$tag^{commit}") != "$commit" ]]; then
	echo "sync-ngf: $src is not at $tag" >&2
	exit 1
fi

# 2. closure (production) and the extra packages of its tests.
pkgs=$(cd "$src" && GOFLAGS=-mod=mod go list -deps \
	-f '{{if .Module}}{{if eq .Module.Path "'$ngf_mod'"}}{{.ImportPath}}{{end}}{{end}}' \
	"${roots[@]/#/./}")
test_pkgs=$(cd "$src" && GOFLAGS=-mod=mod go list -test -deps \
	-f '{{if .Module}}{{if eq .Module.Path "'$ngf_mod'"}}{{.ImportPath}}{{end}}{{end}}' \
	$pkgs | sed 's/ \[.*//' | grep -v -e '\.test$' -e '_test$' || true)
pkgs=$(sed "s|^$ngf_mod/||" <<<"$pkgs" | sort -u)
all=$(printf '%s\n%s\n' "$pkgs" "$test_pkgs" | sed "s|^$ngf_mod/||" | sort -u)

# 3. deny list - also checks the external modules the closure links.
if bad=$(grep -E "$deny_re" <<<"$all"); then
	echo "sync-ngf: $tag's closure contains denied packages:" >&2
	echo "$bad" >&2
	exit 1
fi
ext=$(cd "$src" && GOFLAGS=-mod=mod go list -test -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' \
	$(sed "s|^|$ngf_mod/|" <<<"$pkgs") | sort -u)
if bad=$(grep -E 'telemetry-exporter|nginx/agent' <<<"$ext"); then
	echo "sync-ngf: $tag's closure links denied modules:" >&2
	echo "$bad" >&2
	exit 1
fi

# 4. copy with rewritten import paths. Tests only for the packages the graph
# links; the packages only its tests need come without their own tests, so
# the closure stays closed.
rm -rf "$dest"
mkdir -p "$dest"
while read -r p; do
	[[ -n $p ]] || continue
	out=$dest/${p#internal/}
	mkdir -p "$out"
	with_tests=false
	grep -qxF "$p" <<<"$pkgs" && with_tests=true
	for f in "$src/$p"/*.go; do
		[[ -e $f ]] || continue
		[[ $with_tests == true || $f != *_test.go ]] || continue
		sed -e "s|\"$ngf_mod/internal/|\"$our_mod/|g" \
			-e "s|\"$ngf_mod/|\"$our_mod/|g" \
			"$f" >"$out/$(basename "$f")"
	done
done <<<"$all"
gofmt -w "$dest"
cp "$src/LICENSE" "$dest/LICENSE"

# 5. patches.
applied=()
if compgen -G "$patches/*.patch" >/dev/null; then
	for p in "$patches"/*.patch; do
		echo "sync-ngf: applying $(basename "$p")"
		git -C "$repo" apply --whitespace=nowarn "$p"
		applied+=("$(basename "$p")")
	done
fi

# VERSION last: it records the checksum of the tree as copied and patched,
# which internal/gateway's guard test recomputes - an edit by hand fails
# make test.
tree_sha=$(cd "$dest" && find . -type f ! -name VERSION -printf '%P\n' | LC_ALL=C sort |
	while read -r f; do printf '%s  %s\n' "$(sha256sum <"$f" | cut -d' ' -f1)" "$f"; done |
	sha256sum | cut -d' ' -f1)
{
	echo "# Copied by hack/sync-ngf.sh - do not edit anything below this directory."
	echo "repository: $url"
	echo "tag: $tag"
	echo "commit: $commit"
	echo "gateway-api: $(cd "$src" && go list -m -f '{{.Version}}' sigs.k8s.io/gateway-api)"
	echo "tree-sha256: $tree_sha"
	echo "patches:"
	for p in "${applied[@]}"; do echo "  - $p"; done
	echo "packages:"
	sed 's/^/  - /' <<<"$all"
} >"$dest/VERSION"

# 6. module versions.
cd "$repo"
ver_ge() { [[ $(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1) == "$1" ]]; }
for m in sigs.k8s.io/controller-runtime sigs.k8s.io/gateway-api-inference-extension \
	k8s.io/api k8s.io/apimachinery k8s.io/client-go github.com/onsi/gomega github.com/onsi/ginkgo/v2; do
	theirs=$(cd "$src" && go list -m -f '{{.Version}}' "$m")
	ours=$(go list -m -f '{{.Version}}' "$m" 2>/dev/null || echo v0.0.0)
	ver_ge "$ours" "$theirs" || go get "$m@$theirs"
done
gw=$(cd "$src" && go list -m -f '{{.Version}}' sigs.k8s.io/gateway-api)
[[ $(go list -m -f '{{.Version}}' sigs.k8s.io/gateway-api) == "$gw" ]] || go get "sigs.k8s.io/gateway-api@$gw"
go mod tidy

# 7. the standard channel CRDs of that Gateway API version, for kubectl
# (deployments/gateway-api) and the chart (controller.gatewayAPI.installCRDs,
# through the symlink charts/wxingress/gateway-api-crds.yaml).
crd_dir=$(go list -m -f '{{.Dir}}' sigs.k8s.io/gateway-api)/config/crd/standard
mkdir -p deployments/gateway-api
{
	echo "# Gateway API $gw, standard channel: the CRDs and their ValidatingAdmissionPolicy,"
	echo "# copied by hack/sync-ngf.sh from sigs.k8s.io/gateway-api@$gw/config/crd/standard."
	echo "# kubectl apply --server-side -f deployments/gateway-api/standard-install.yaml"
	for f in $(cd "$crd_dir" && ls *.yaml | LC_ALL=C sort); do
		echo "---"
		cat "$crd_dir/$f"
	done
} >deployments/gateway-api/standard-install.yaml

go build ./internal/third_party/ngf/...
go vet ./internal/third_party/ngf/...
go test ./internal/third_party/ngf/...
[[ ! -d internal/gateway ]] || go test ./internal/gateway/...
echo "sync-ngf: internal/third_party/ngf is NGF $tag ($commit)"
