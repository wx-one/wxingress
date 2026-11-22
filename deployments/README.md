# Manifests

wxingress is installed with its Helm chart, `charts/wxingress`
(`helm install wxingress oci://ghcr.io/wx-one/charts/wxingress`); see the
[installation guide](https://wx-one.github.io/wxingress/getting-started/installation/).
The chart sets the least privilege RBAC, the hardened image and the security
context; there are no static manifests for the controller any more.

What is left here:

| Path | What |
| ---- | ---- |
| `rbac/rbac.yaml` | the chart's ClusterRole and Role for a release `nginx-ingress` in the namespace `nginx-ingress`, which the integration tests restore |
| `common/nginx-config.yaml` | the ConfigMap the integration tests reset the controller's configuration to |
| `gateway-api/` | the Gateway API CRDs the chart installs with `installCRDs` |
| `rke2-cutover/` | the cut-over from RKE2's ingress-nginx |
