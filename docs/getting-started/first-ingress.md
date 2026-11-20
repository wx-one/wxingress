# Your first Ingress

This page assumes wxingress is [installed](installation.md) with `controller.ingressNginxCompat=true` and
the IngressClass `nginx`.

## A backend

```sh
kubectl create deployment echo --image=registry.k8s.io/e2e-test-images/agnhost:2.53 -- /agnhost netexec --http-port=8080
kubectl expose deployment echo --port=80 --target-port=8080
```

## The Ingress

```yaml title="echo-ingress.yaml"
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: 16m
    nginx.ingress.kubernetes.io/proxy-read-timeout: "120"
spec:
  ingressClassName: nginx
  rules:
    - host: echo.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  number: 80
```

```sh
kubectl apply -f echo-ingress.yaml
```

The annotations are ingress-nginx's, and wxingress reads them as they are.

## Try it

```sh
ADDRESS=$(kubectl -n wxingress get service wxingress-controller \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl --resolve echo.example.com:80:$ADDRESS http://echo.example.com/hostname
```

## When something does not work

```sh
kubectl describe ingress echo        # Warning events: unsupported annotations, invalid values
kubectl -n wxingress logs deploy/wxingress-controller | grep -E '^[WE][0-9]'
```

An Ingress that wxingress cannot serve as written always gets a Warning event that says why. An Ingress
with an error is refused by the admission webhook before it is stored.

## TLS

Add a `tls` section with a Secret of type `kubernetes.io/tls`, or let cert-manager create it:

```yaml
metadata:
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
spec:
  tls:
    - hosts: [echo.example.com]
      secretName: echo-tls
```

With the compatibility profile, plain HTTP requests to a host with TLS are redirected to HTTPS with a 308,
as in ingress-nginx. HTTP-01 challenges of cert-manager are never redirected.
