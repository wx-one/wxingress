# Changelog

wxingress versions are `<NGINX Ingress Controller version>-wx<n>`: `5.7.1-wx5` is the fifth wxingress
release based on NGINX Ingress Controller 5.7.1. The changes of NGINX Ingress Controller itself are on its
[releases page](https://github.com/nginx/kubernetes-ingress/releases).

## 5.7.1-wx5

The first public release.

- An endpoint of an EndpointSlice without a `ready` condition is served, as the Kubernetes API defines.
  Hand-written EndpointSlices for Services without a selector often have no conditions.
- Licence: EUPL-1.2 (the code from NGINX Ingress Controller and NGINX Gateway Fabric stays Apache-2.0).
- Go module `github.com/wx-one/wxingress`; images on `ghcr.io/wx-one/wxingress`, the chart on
  `oci://ghcr.io/wx-one/charts/wxingress`.
- The chart defaults to the wxingress image.

## 5.7.1-wx4

- Behind the PROXY protocol (`use-proxy-protocol` in an ingress-nginx ConfigMap), the client's address is
  the PROXY header's (`real_ip_header proxy_protocol`), as in ingress-nginx. Before, it was taken from
  `X-Forwarded-For`, so the load balancer's address appeared as every client's.
- A `real-ip-header` or `set-real-ip-from` set in the ConfigMap wins over the translation of
  ingress-nginx's keys.

## 5.7.1-wx3

- A path that a host of an Ingress lists twice (for example a host listed twice with `/`) is served from
  the first rule, as in ingress-nginx. Before, it produced two `location` blocks and NGINX refused the whole
  configuration.
- A pod is not Ready while NGINX refuses the configuration of the resources at startup.

## 5.7.1-wx2

- With `-ingress-nginx-defaults`, an Ingress backend whose port number or name no Service port has is
  matched against the Service ports' `targetPort`, as in ingress-nginx (port 80 to 5000, Ingress says 5000).

## 5.7.1-wx1

- NGINX 1.31.9 with the fixes of the NGINX security advisories of November 2026.
- The leader gives up its Lease when it stops, so the next pod leads within seconds instead of after the
  Lease's 30 s.
- The chart renders the leader election Lease, the Gateway API CRDs and the safe-upgrades
  ValidatingAdmissionPolicy only where none exists or where it is the release's own, and keeps the Lease
  on uninstall.
