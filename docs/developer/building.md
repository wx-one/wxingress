# Building

wxingress has three parts:

| Part | Language | Built with |
|---|---|---|
| The controller (`cmd/nginx-ingress`, `internal/`, `pkg/`) | Go | `go build`, `make` |
| The data plane module (`dataplane/`) | meta | the meta compiler, in the image `docker.io/wxone/meta` |
| The image | Dockerfile | `build/Dockerfile.wxingress`, from the two above and NGINX packages from nginx.org |

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
ingress-nginx metrics and what NGINX Ingress Controller does in njs. It is written in meta and compiled to C
by the meta compiler, then built as a dynamic NGINX module against the exact NGINX version of the image
(`NGINX_VERSION` in `dataplane/build.sh`).

```sh
docker run --rm -v "$PWD:/src" -w /src -e META=/opt/meta/meta \
  docker.io/wxone/meta:0a3067f ./dataplane/build.sh
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
`dataplane/build/ngx_http_wxingress_module.so` in the build context.

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
module in the meta image, and lints the chart, on every push and pull request.
`.github/workflows/release.yml` runs on tags `v*`, builds the image and pushes it to
`docker.io/wxone/wxingress`, pushes the chart to `oci://ghcr.io/wx-one/charts`, and creates a GitHub
release.
