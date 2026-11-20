# wxingress Helm chart

Installs wxingress from this source tree:

```console
helm install my-release charts/wxingress --set controller.dynamicUpstreams=true
```

The values are those of NGINX Ingress Controller, which wxingress is forked
from, so their documentation applies; `values.yaml` describes each of them,
and `controller.dynamicUpstreams` turns on the wxingress module (see
`docs/user-guide/ingress-nginx-compatibility.md`). The chart's name gives the default names and the
`app.kubernetes.io/name` label of its resources: a release `my-release` has
the Deployment `my-release-wxingress-controller`.

A release installed from the chart of upstream (`nginx-ingress`) cannot be
upgraded to this one in place, since the selector of its Deployment changes
with the name; install anew, or keep the old names with
`--set nameOverride=nginx-ingress`.
