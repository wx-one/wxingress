# Choosing an ingress-nginx replacement

ingress-nginx was retired in March 2026. Every cluster that ran it needs a successor, and there are several
good ones. This page helps you decide, and it is honest about when wxingress is not the best fit.

## The real question: rewrite or not?

What a switch costs is mostly not the controller. It is the Ingresses: every
`nginx.ingress.kubernetes.io/*` annotation, every default your applications silently depend on (timeouts,
buffering, retries, body size limits, HSTS), every Helm chart of a vendor that writes those Ingresses for
you. The successors differ in how much of that you have to rewrite and test again.

| Path | What you rewrite | Fits when |
|---|---|---|
| **wxingress** | nothing, except Lua in snippets (to [njs](lua-to-njs.md)) | you want the switch to be invisible to the applications, now |
| **Another Ingress controller** (Traefik, HAProxy, F5 NGINX Ingress Controller, cloud provider controllers) | the annotations, into that controller's own, and possibly the defaults | you already use that controller elsewhere, or need its specific features |
| **Gateway API** (Envoy Gateway, Istio, Cilium, NGINX Gateway Fabric, …) | every Ingress, into Gateways and routes (tools such as `ingress2gateway` help) | you plan the move to the Gateway API anyway and can afford to touch every route |

Some controllers read a subset of ingress-nginx's annotations. Check which ones your Ingresses use (the
[migration guide](ingress-nginx.md#1-check-what-your-ingresses-use) has the command) against what the
controller you consider supports, and test the defaults, not just the annotations.

## What makes wxingress different

- **The compatibility is measured.** 97 Ingresses from production clusters and popular Helm charts,
  probed 2562 ways against ingress-nginx and rke2-ingress-nginx: 2511 identical answers, and each of the
  51 differences has a documented reason. The scripts are in the repository; run them against your own
  Ingresses. See [Testing and benchmarks](testing.md).
- **ingress-nginx's defaults, not just its annotations.** An Ingress without annotations behaves as it did
  under ingress-nginx: the 5 s connect timeout, buffering off, three retries, HSTS, the 308 redirect, the
  WebSocket headers.
- **The operational surface stays.** The IngressClass `nginx`, the admission webhook's checks, the
  Prometheus metric names your dashboards and alerts use, the TCP/UDP services ConfigMap, the default
  backend.
- **A switch you can undo.** wxingress and ingress-nginx serve the same Ingresses at the same time, so
  traffic moves node by node or address by address, and back. There is a tested
  [runbook for RKE2](rke2.md).
- **It is faster.** No Lua in the request path: about 4 times the HTTP requests per second of
  ingress-nginx 1.15 on the same node, half the CPU per HTTPS request, half the memory under load.
- **The Gateway API is included.** When you are ready to move routes to the Gateway API, the same
  controller serves Gateways and HTTPRoutes next to your Ingresses, and the ingress-nginx annotations even
  work on routes. You can migrate to the Gateway API later, one route at a time, instead of all at once. (The Gateway
  API side is tested against the conformance suite but, unlike the Ingress side, not yet in our production.)

## When wxingress is not the best choice

- **You depend heavily on Lua.** Small Lua snippets translate to njs well. If your platform runs large
  amounts of custom Lua (OpenResty plugins, custom balancers), a rewrite is real work, and an
  OpenResty-based gateway may suit you better.
- **You want a service mesh anyway.** If you are introducing Istio or Cilium for other reasons, their
  Gateway API implementations may be the more consistent choice.
- **You need a vendor's commercial product.** wxingress is community-driven open source with
  [commercial support](../about/support.md) from WizardTales; if your organisation requires a specific
  vendor, follow that requirement.

## Try it without risk

Install wxingress next to ingress-nginx, without taking over the IngressClass or any traffic, and compare
the answers of both for every host of your cluster. The [migration guide](ingress-nginx.md) shows how;
nothing changes for your users until you move traffic.
