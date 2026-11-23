# Upstream

wxingress follows two upstream projects. Both are licensed under the Apache License 2.0.

| Upstream | What wxingress takes | Where |
|---|---|---|
| [NGINX Ingress Controller](https://github.com/nginx/kubernetes-ingress) | the controller: Ingress, VirtualServer, TransportServer, Policy, templates, chart | the whole repository is a fork |
| [NGINX Gateway Fabric](https://github.com/nginx/nginx-gateway-fabric) | the Gateway API graph: attachment, conflicts, status | `internal/third_party/ngf/`, a copy |

## NGINX Ingress Controller

wxingress is a fork of NGINX Ingress Controller 5.7.1 and merges new upstream releases. What wxingress
removes or replaces:

- **Telemetry** is removed (`internal/telemetry` must not exist; a test checks it).
- **NGINX Plus, App Protect and App Protect DoS** code is inherited but neither built nor tested by
  wxingress. Its flags and CRDs are still there so that merges stay small.
- **njs in the data path** is replaced by the data plane module, which computes the same things in
  `compat.c`.
- **The release infrastructure** (F5's workflows and registries) is replaced by wxingress's own CI.

When you merge an upstream release, keep conflicts in templates and `internal/configs` small: the
ingress-nginx compatibility lives mostly in files of its own (`internal/configs/ingress_nginx*.go`,
`internal/k8s/ingress_shape.go` and others).

## NGINX Gateway Fabric

`internal/third_party/ngf/` is a copy of NGF's Gateway API graph, made by `hack/sync-ngf.sh`. Nothing in
it is edited by hand: a test (`TestNGFCopyIsUnmodified`) recomputes the checksum the script wrote into
`internal/third_party/ngf/VERSION`. To change something, add a patch to `hack/ngf-patches/` or change the
code in `internal/gateway/` that uses the copy, then sync again.

```sh
hack/sync-ngf.sh v2.9.0
```

The script clones NGF at the tag, copies the package closure of the graph, rewrites the import paths,
applies the patches, raises `go.mod` to NGF's versions where needed (the Gateway API module exactly to
NGF's), writes the matching standard channel CRDs to `deployments/gateway-api/standard-install.yaml`, and
builds and tests the copy. Re-running it with the same tag changes nothing.

## The ingress-nginx corpus

`dataplane/e2e/corpus/` holds the Ingresses the parity tests use. To add the Ingresses of a cluster
without their names, use the anonymiser:

```sh
kubectl get ingress -A -o json > /tmp/ing.json
python3 -I dataplane/e2e/corpus/anonymise.py /tmp/ing.json > dataplane/e2e/corpus/<name>/ingresses.json
```

It replaces namespaces, names, hosts, Secrets, issuers and Helm releases with placeholders and drops
labels, status and Rancher's annotations.
