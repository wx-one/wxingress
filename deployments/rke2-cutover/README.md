# rke2-cutover

Example values for replacing RKE2's bundled `rke2-ingress-nginx` with wxingress node by node:

- `values.yaml`: wxingress as a hostPort DaemonSet in ingress-nginx's place
- `rke2-step1-values.yaml`: what step 1 adds to the HelmChartConfig of `rke2-ingress-nginx`

The runbook is in the documentation: [RKE2: node by node](https://wx-one.github.io/wxingress/migration/rke2/)
(source: `docs/migration/rke2.md`).
