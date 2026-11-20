#!/bin/bash
# CycloneDX 1.6 SBOMs for wxingress, written from what the build knows rather
# than scanned for: the controller is a static Go binary (its modules are in
# its build info), the data plane module links libcurl statically and the
# meta runtime, and the image installs packages whose list dpkg keeps.
#
#   ci/sbom.sh release <version> <nginx-ingress> <module.so> > release.cdx.json
#       the controller binary and its Go modules, the data plane module with
#       the libcurl and meta runtime inside it, and the nginx it is built for
#       (scope "excluded": the image provides it)
#
#   ci/sbom.sh image <release.cdx.json> <image-ref> <image-id> > image.cdx.json
#       the image: the release, every Debian and nginx.org package installed
#       in it (read from the image's dpkg database), and the base image by
#       digest
#
# Needs jq, go (for `go version -m`) and, for image, docker. Reads from
# dataplane/build.sh: NGINX_VERSION, CURL_VERSION, CURL_SHA256, CURL_URL; and
# META-IMAGE beside the module (which meta built it).
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
build_var() { sed -n "s/^$1=//p" "$here/dataplane/build.sh" | head -1; }

component() { # <name> <version> <purl> <sha256> <licence> <scope> [description] [url]
  jq -n --arg n "$1" --arg v "$2" --arg p "$3" --arg h "$4" --arg l "$5" --arg s "$6" \
        --arg d "${7:-}" --arg u "${8:-}" '
    {type: "library", "bom-ref": $p, name: $n, version: $v, purl: $p, scope: $s}
    + (if $l != "" then {licenses: [ (if ($l | test(" WITH | AND | OR ")) then {expression: $l} else {license: {id: $l}} end) ]} else {} end)
    + (if $h != "" then {hashes: [{alg: "SHA-256", content: $h}]} else {} end)
    + (if $u != "" then {externalReferences: [{type: "distribution", url: $u}]} else {} end)
    + (if $d != "" then {description: $d} else {} end)'
}

document() { # <metadata-component-json> <components-json-array>
  jq -n --argjson app "$1" --argjson comps "$2" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        --arg serial "urn:uuid:$(cat /proc/sys/kernel/random/uuid)" '
    {bomFormat: "CycloneDX", specVersion: "1.6", serialNumber: $serial, version: 1,
     metadata: {timestamp: $ts,
                tools: {components: [{type: "application", name: "wxingress ci/sbom.sh"}]},
                component: $app},
     components: $comps,
     dependencies: [{ref: $app["bom-ref"], dependsOn: [$comps[]["bom-ref"]]}]}'
}

case "${1:-}" in
release)
  version=${2:?version} controller=${3:?nginx-ingress binary} module=${4:?module .so}
  comps=()

  # the controller: the Go toolchain's standard library and every module
  # in the binary's build info, replacements resolved
  gover=$(go version "$controller" | awk '{print $2}' | sed 's/-X:.*//')
  comps+=("$(component stdlib "$gover" "pkg:golang/stdlib@$gover" "" BSD-3-Clause required "the Go standard library, linked statically into nginx-ingress")")
  while IFS=$'\t' read -r kind path ver sum; do
    case "$kind" in dep) ;; *) continue ;; esac
    comps+=("$(component "$path" "$ver" "pkg:golang/$path@$ver" "" "" required "Go module, linked statically into nginx-ingress" \
      | jq --arg h "${sum:-}" 'if $h != "" then . + {properties: [{name: "go:sum", value: $h}]} else . end')")
  done < <(go version -m "$controller" | sed 's/^\t//' | awk -F'\t' '$1=="dep"{print} $1=="=>"{print "dep\t"$2"\t"$3"\t"$4}')

  # the data plane module: libcurl linked statically, and the meta runtime
  curl_v=$(build_var CURL_VERSION)
  curl_url=$(build_var CURL_URL | sed "s/\$CURL_VERSION/$curl_v/")
  comps+=("$(component curl "$curl_v" "pkg:generic/curl@$curl_v" "$(build_var CURL_SHA256)" curl required \
    "built from source by dataplane/build.sh (HTTP and HTTPS only), linked statically into the data plane module" "$curl_url")")
  meta_ref=$(cat "$(dirname "$module")/META-IMAGE" 2>/dev/null || echo unknown)
  comps+=("$(jq -n --arg ref "$meta_ref" '
    {type: "library", "bom-ref": "pkg:generic/meta-runtime", name: "meta-runtime",
     version: ($ref | split("@")[0] | split(":")[1] // "unknown"),
     properties: [{name: "meta:image", value: $ref}], licenses: [{license: {name: "proprietary (wx-one)"}}],
     scope: "required",
     description: "runtime of the meta compiler the data plane module is written in, linked statically",
     externalReferences: [{type: "distribution", url: "https://hub.docker.com/r/wxone/meta"}]}')")

  nginx_v=$(build_var NGINX_VERSION)
  comps+=("$(component nginx "$nginx_v" "pkg:generic/nginx@$nginx_v" "" BSD-2-Clause excluded \
    "the data plane module is built for exactly this nginx; the image installs it from nginx.org's packages" "https://nginx.org/download/nginx-$nginx_v.tar.gz")")

  # the files shipped, by their hashes
  for f in "$controller" "$module"; do
    comps+=("$(jq -n --arg f "$(basename "$f")" --arg h "$(sha256sum "$f" | cut -d' ' -f1)" '
      {type: "file", "bom-ref": ("file:" + $f), name: $f, hashes: [{alg: "SHA-256", content: $h}]}')")
  done

  app=$(jq -n --arg v "$version" --arg c "$(git -C "$here" rev-parse HEAD 2>/dev/null || echo unknown)" '
    {type: "application", "bom-ref": ("pkg:github/wx-one/wxingress@" + $v), name: "wxingress",
     version: $v, purl: ("pkg:github/wx-one/wxingress@" + $v),
     licenses: [{expression: "EUPL-1.2 AND Apache-2.0"}],
     externalReferences: [{type: "vcs", url: "https://github.com/wx-one/wxingress"}],
     properties: [{name: "git:commit", value: $c}]}')

  document "$app" "$(printf '%s\n' "${comps[@]}" | jq -s .)"
  ;;

image)
  release=${2:?release sbom} image=${3:?image ref} id=${4:?local image}
  comps=()

  # the release as one component, with what is inside it nested
  comps+=("$(jq '.metadata.component + {components: [.components[] | select(.scope != "excluded")]}' "$release")")

  # every package installed in the image; nginx.org's carry "~<codename>"
  while read -r pkg ver src srcver arch; do
    case "$ver" in
      *~trixie*|*~bookworm*) ns=nginx origin="nginx.org's package repository" ;;
      *) ns=debian origin="Debian 13" ;;
    esac
    comps+=("$(jq -n --arg p "$pkg" --arg v "$ver" --arg s "${src:-$pkg}" --arg sv "${srcver:-$ver}" \
                     --arg a "$arch" --arg ns "$ns" --arg o "$origin" '
      {type: "library", "bom-ref": ("pkg:deb/" + $ns + "/" + $p + "@" + $v), name: $p, version: $v,
       purl: ("pkg:deb/" + $ns + "/" + $p + "@" + $v + "?arch=" + $a + "&distro=debian-13"),
       description: ("installed from " + $o + " (source package " + $s + " " + $sv + ")")}')")
  done < <(docker run --rm --entrypoint dpkg-query "$id" -W \
             -f '${Package} ${Version} ${source:Package} ${source:Version} ${Architecture}\n')

  base=$(sed -n 's/^FROM \([^ ]*\).*/\1/p' "$here/build/Dockerfile.wxingress" | head -1)
  comps+=("$(jq -n --arg s "$base" '
    {type: "container", "bom-ref": "base-image", name: ($s | split("@")[0]), version: ($s | split("@")[1]),
     description: "the base image, by digest"}')")

  app=$(jq -n --arg i "$image" --arg v "$(jq -r .metadata.component.version "$release")" '
    {type: "container", "bom-ref": ("image:" + $i), name: $i, version: $v,
     licenses: [{expression: "EUPL-1.2 AND Apache-2.0"}]}')

  document "$app" "$(printf '%s\n' "${comps[@]}" | jq -s .)"
  ;;

*)
  sed -n '2,20p' "$0"; exit 2 ;;
esac
