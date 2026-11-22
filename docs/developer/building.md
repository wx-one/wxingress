# Building

wxingress has three parts:

| Part | Language | Built with |
|---|---|---|
| The controller (`cmd/nginx-ingress`, `internal/`, `pkg/`) | Go | `go build`, `make` |
| The data plane module (`dataplane/`) | metalang | the metalang compiler, in the image `docker.io/wxone/meta` |
| The image | Dockerfile | `build/Dockerfile.wxingress`, from the two above and NGINX packages from nginx.org, on the hardened Debian 13 runtime seed of container.gov.de |

## The controller

Requirements: Go (the version in `go.mod`), `make`, and Helm for the chart tests.

```sh
make secrets          # test certificates and keys the tests read (generated, not committed)
make test             # go test -tags=aws,helmunit -shuffle=on ./...
make build            # the nginx-ingress binary
```

After a change to `pkg/apis/configuration/v1/types.go`, regenerate the code and the CRDs and commit
the result:

```sh
make update-codegen
make update-crds
```

After a change to a template (`internal/configs/version*/*.tmpl`) or to what feeds it, update the
snapshots, and check that a fixture actually renders the new directive:

```sh
make test-update-snaps
```

## The data plane module

The module `ngx_http_wxingress_module.so` provides the dynamic upstreams, the balancers, the
ingress-nginx metrics and what NGINX Ingress Controller does in njs. It is written in metalang and compiled
to C by the metalang compiler, then built as a dynamic NGINX module against the exact NGINX version of the
image (`NGINX_VERSION` in `dataplane/build.sh`).

!!! note "About metalang"
    metalang is the programming language WizardTales writes all of its software in. It is not open source
    yet: it is still developed rapidly, and we keep the freedom to change it until others depend on it. It
    is planned to be open-sourced in Q2-Q3 2027. Until then the compiler and its runtime are published as
    the container image `docker.io/wxone/meta`, which is all you need to build the module.

```sh
docker run --rm -v "$PWD:/src" -w /src -e META=/opt/meta/meta \
  docker.io/wxone/meta:5129ecb6 ./dataplane/build.sh
# the module:      dataplane/build/ngx_http_wxingress_module.so
# a test nginx:    dataplane/build/nginx/sbin/nginx

./dataplane/build.sh test   # the data plane tests against that nginx
```

!!! note "NGINX version"
    NGINX loads a dynamic module only if it was built against exactly the same version. When you raise
    the NGINX version, change it in `dataplane/build.sh`, `build/Dockerfile`,
    `build/Dockerfile.wxingress`, the `Makefile` and `.github/workflows/ci.yml` together.

## The image

```sh
CGO_ENABLED=0 go build -trimpath -o nginx-ingress ./cmd/nginx-ingress
docker build -f build/Dockerfile.wxingress -t wxingress:dev \
  --build-arg IC_VERSION=dev .
```

`build/Dockerfile.wxingress` expects the controller binary `nginx-ingress` and
`dataplane/build/ngx_http_wxingress_module.so` in the build context. It has two stages:

- **assemble**, on the Debian snapshot the seed's own builder uses (by digest): installs NGINX, njs and
  the OpenTelemetry module from nginx.org - the signing key checked by its SHA-256 (`ADD --checksum`)
  and against nginx.org's published fingerprints - loads all three modules with `nginx -t`, and puts
  into `/rootfs` what the image carries: NGINX and `nginx-debug`, the modules, `/etc/nginx`, the
  templates, the njs scripts, the controller, the licences, every shared library `ldd` proves for NGINX
  and the modules (and `libgcc_s`), and `/usr/share/doc/wxingress/PACKAGES`, the Debian and nginx.org
  packages those files come from, for the SBOM. Binaries, templates and libraries are root's and
  read-only; what the controller and NGINX write belongs to 101:0. NGINX, `nginx-debug` and
  `/nginx-ingress` get the file capability `cap_net_bind_service` (it survives `COPY --from`).
- the final stage: the runtime seed
  (`registry.opencode.de/oci-community/images/auswaertiges-amt/debian:13.7`, by digest) with `/rootfs`
  on top, `USER 101`.

There is no shell in the image. The chart runs the two things it needs besides the controller as
subcommands of the controller binary: `/nginx-ingress sleep <seconds>` (the preStop hook before
Kubernetes 1.30) and `/nginx-ingress copy <source> <dest>` (the init container with a read-only root
file system). To look inside, use `docker cp` or run `nginx` as entrypoint:

```sh
docker run --rm --entrypoint /usr/sbin/nginx wxingress:dev -V
```

The SBOMs of a build:

```sh
ci/sbom.sh release dev nginx-ingress dataplane/build/ngx_http_wxingress_module.so > release.cdx.json
ci/sbom.sh image release.cdx.json wxingress:dev wxingress:dev > image.cdx.json
```

When container.gov.de publishes a new seed, update `SEED` and the assemble stage's snapshot together.

## End-to-end tests

The scripts in `dataplane/e2e/` run against a kind cluster and need Docker, kind, kubectl and Helm:

| Script | What it tests |
|---|---|
| `ingress-nginx-parity.sh` | the same answers as ingress-nginx and rke2-ingress-nginx for the 97-Ingress corpus, and a switch under load |
| `ingress-nginx-bench.sh` | throughput, latency, CPU and memory against ingress-nginx |
| `ingress-nginx-annotations.sh` | each supported ingress-nginx annotation |
| `admission-webhook.sh` | what the admission webhook admits and refuses |
| `gateway.sh`, `gateway-namespaces.sh` | the Gateway API |
| `gateway-conformance/run.sh` | the Gateway API conformance suite |
| `transportserver.sh`, `certificates.sh` | TransportServers and certificate handling |

## CI

`.github/workflows/ci.yml` runs the Go tests, builds the controller, builds and tests the data plane
module with the metalang compiler, and lints the chart, on every push and pull request.
`.github/workflows/release.yml` runs on tags `v<major>.<minor>.<patch>[-wx<n>]` whose commit is on
`main`. In the GitHub Environment `release` (its protection rules gate every push) it builds the image,
pushes it to `ghcr.io/wx-one/wxingress` (`:latest` only for the highest version), signs it with cosign
(keyless), attaches build provenance and SBOM attestations, pushes the chart with the image's digest to
`oci://ghcr.io/wx-one/charts`, and creates a GitHub release. Releases always build the data plane with
the metalang compiler of `ci/meta-image`; the `META_IMAGE` override (by digest) applies to runs of
`ci.yml` alone. All actions are pinned by commit.
