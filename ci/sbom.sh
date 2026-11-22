#!/bin/bash
# CycloneDX 1.6 SBOMs for wxingress, written from what the build knows rather
# than scanned for: the controller is a static Go binary (its modules are in
# its build info), the data plane module links libcurl statically and the
# metalang runtime, and the image carries the list of the Debian and
# nginx.org packages its files were copied from.
#
#   ci/sbom.sh release <version> <nginx-ingress> <module.so> > release.cdx.json
#       the controller binary and its Go modules, the data plane module with
#       the libcurl and metalang runtime inside it, and the nginx it is built for
#       (scope "excluded": the image provides it)
#
#   ci/sbom.sh image <release.cdx.json> <image-ref> <image-id> > image.cdx.json
#       what the image adds on top of its base: the release, nginx and its
#       modules from nginx.org's packages, and the Debian packages the shared
#       libraries were copied from - read from /usr/share/doc/wxingress/PACKAGES
#       in the image, which build/Dockerfile.wxingress writes with dpkg (the
#       image itself has no dpkg) - and the base, the runtime seed of
#       container.gov.de, by digest; it carries a signed SBOM of its own
#
# Needs jq, go (for `go version -m`) and, for image, docker. Reads from
# dataplane/build.sh: NGINX_VERSION, CURL_VERSION, CURL_SHA256, CURL_URL; and
# META-IMAGE beside the module (which metalang compiler built it).
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
     description: "runtime of metalang, the language the data plane module is written in, linked statically",
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

  # the packages the image's files came from, as the build wrote them down;
  # nginx.org's carry "~<codename>". The image has no shell: docker cp.
  packages=$(mktemp)
  container=$(docker create "$id")
  trap 'docker rm "$container" >/dev/null; rm -f "$packages"' EXIT
  docker cp "$container:/usr/share/doc/wxingress/PACKAGES" - | tar -xOf - > "$packages"
  [ -s "$packages" ] || { echo "no /usr/share/doc/wxingress/PACKAGES in $id" >&2; exit 1; }
  while read -r pkg ver src srcver arch; do
    case "$ver" in
      *~trixie*|*~bookworm*) ns=nginx origin="nginx.org's package repository" ;;
      *) ns=debian origin="Debian 13" ;;
    esac
    case "$src" in
      glibc) l=LGPL-2.1-or-later ;;
      gcc-*) l="GPL-3.0-or-later WITH GCC-exception-3.1" ;;
      nginx|nginx-module-njs) l=BSD-2-Clause ;;
      nginx-module-otel) l=Apache-2.0 ;;
      openssl) l=Apache-2.0 ;;
      zlib) l=Zlib ;;
      pcre2) l="BSD-3-Clause WITH PCRE2-exception" ;;
      libxml2|c-ares) l=MIT ;;
      xz-utils) l=0BSD ;;
      libzstd) l="BSD-3-Clause OR GPL-2.0-only" ;;
      libxcrypt) l=LGPL-2.1-or-later ;;
      *) l="" ;;
    esac
    comps+=("$(jq -n --arg p "$pkg" --arg v "$ver" --arg s "${src:-$pkg}" --arg sv "${srcver:-$ver}" \
                     --arg a "$arch" --arg ns "$ns" --arg o "$origin" --arg l "$l" '
      {type: "library", "bom-ref": ("pkg:deb/" + $ns + "/" + $p + "@" + $v), name: $p, version: $v,
       purl: ("pkg:deb/" + $ns + "/" + $p + "@" + $v + "?arch=" + $a + "&distro=debian-13"),
       description: ("copied from " + $o + " (source package " + $s + " " + $sv + ")")}
      + (if $l == "" then {} elif ($l | test(" WITH | OR ")) then {licenses: [{expression: $l}]} else {licenses: [{license: {id: $l}}]} end)')")
  done < "$packages"

  # the base: the SEED argument of the Dockerfile, by digest
  base=$(sed -n 's/^ARG SEED=\(.*\)$/\1/p' "$here/build/Dockerfile.wxingress" | head -1)
  case "$base" in *@sha256:*) ;; *) echo "no SEED by digest in build/Dockerfile.wxingress" >&2; exit 1 ;; esac
  comps+=("$(jq -n --arg s "$base" '
    {type: "container", "bom-ref": "base-image", name: ($s | split("@")[0]), version: ($s | split("@")[1]),
     description: "hardened Debian 13 runtime seed of container.gov.de (Secure Government Container Initiative): a trust store and a nonroot user, no shell, no package manager; its own signed CycloneDX SBOM is attached to it in its registry",
     externalReferences: [{type: "distribution", url: ("https://" + ($s | split("@")[0]))}]}')")

  app=$(jq -n --arg i "$image" --arg v "$(jq -r .metadata.component.version "$release")" '
    {type: "container", "bom-ref": ("image:" + $i), name: $i, version: $v,
     licenses: [{expression: "EUPL-1.2 AND Apache-2.0"}]}')

  document "$app" "$(printf '%s\n' "${comps[@]}" | jq -s .)"
  ;;

*)
  sed -n '2,24p' "$0"; exit 2 ;;
esac
