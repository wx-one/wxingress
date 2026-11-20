#!/bin/sh
#
# ingress-nginx annotations on Ingresses, as ingress-nginx would serve them:
# the source ranges, and what wxingress does with an annotation it does not
# support - a Warning event, and 503 where serving without it would serve
# what ingress-nginx restricts (docs/user-guide/ingress-nginx-compatibility.md).
#
# Runs where kubectl, openssl and the kind node's docker are, against a
# release of charts/wxingress installed as for gateway.sh, with
# controller.ingressNginxDefaults=true:
#
#   RELEASE_NS=wxgw CLASS=wxgw dataplane/e2e/ingress-nginx-annotations.sh
#
# It makes the namespace inx-e2e and removes it.
#
#   KUBECTL     how to call kubectl ("sudo kubectl")
#   DOCKER      how to call docker ("sudo docker")
#   NODE        the kind node (wxingress-control-plane)
#   RELEASE_NS  the namespace of the release (wxgw)
#   CLASS       the IngressClass of the release (wxgw)

set -u

K=${KUBECTL:-sudo kubectl}
D=${DOCKER:-sudo docker}
node=${NODE:-wxingress-control-plane}
ns=${RELEASE_NS:-wxgw}
class=${CLASS:-wxgw}
N=inx-e2e
P=nginx.ingress.kubernetes.io

failed=0
check() {
  if [ "$2" = "$3" ]; then
    echo "ok       $1"
  else
    echo "FAIL     $1"
    echo "  wanted: $3"
    echo "  got:    $2"
    failed=$((failed + 1))
  fi
}
X() { $D exec "$node" "$@"; }
# the pod of the release, not one a rollout is still taking down
w=0
while [ "$w" -lt 120 ] && [ "$($K -n "$ns" get pod -l app.kubernetes.io/instance="$ns" -o name | wc -l)" -gt 1 ]; do sleep 1; w=$((w + 1)); done
ctrl=$($K -n "$ns" get svc -l app.kubernetes.io/instance="$ns" -o jsonpath='{.items[0].spec.clusterIP}')
# code <host>: the status of /hostname
code() { X curl -s -o /dev/null --max-time 3 -w '%{http_code}' -H "Host: $1" "http://$ctrl/hostname"; }
# warned <ingress> <text>: whether a Warning event of the Ingress has the text
# (events are sent asynchronously: up to 15 s)
warned() {
  w=0
  while [ "$w" -lt 15 ] && ! $K -n $N get events --field-selector "involvedObject.name=$1,type=Warning" -o jsonpath='{.items[*].message}' | grep -q "$2"; do sleep 1; w=$((w + 1)); done
  $K -n $N get events --field-selector "involvedObject.name=$1,type=Warning" -o jsonpath='{.items[*].message}' | grep -c "$2"
}

$K delete ns $N --wait=true >/dev/null 2>&1
$K create ns $N >/dev/null
tmp=$(mktemp -d)
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=inx.test -addext "subjectAltName=DNS:tls.inx.test,DNS:plainhttp.inx.test,DNS:cert.inx.test,DNS:certcn.inx.test" \
  -keyout "$tmp/key" -out "$tmp/crt" >/dev/null 2>&1
$K -n $N create secret tls tls --cert="$tmp/crt" --key="$tmp/key" >/dev/null
# a CA and a client certificate of it for auth-tls
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=inx-ca -keyout "$tmp/ca.key" -out "$tmp/ca.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj /CN=alice -keyout "$tmp/client.key" -out "$tmp/client.csr" >/dev/null 2>&1
openssl x509 -req -in "$tmp/client.csr" -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAcreateserial -days 2 -out "$tmp/client.crt" >/dev/null 2>&1
$K -n $N create secret generic ca --from-file=ca.crt="$tmp/ca.crt" >/dev/null
cat "$tmp/client.crt" | $D exec -i "$node" sh -c 'cat > /var/tmp/inx-client.crt'
cat "$tmp/client.key" | $D exec -i "$node" sh -c 'cat > /var/tmp/inx-client.key'
$K -n $N create secret generic basic --from-literal=auth="user:$(openssl passwd -apr1 secret)" >/dev/null
rm -rf "$tmp"

ingress() { # name host annotations...
  name=$1 host=$2
  shift 2
  echo "---"
  echo "apiVersion: networking.k8s.io/v1"
  echo "kind: Ingress"
  echo "metadata:"
  echo "  name: $name"
  echo "  namespace: $N"
  echo "  annotations:"
  echo "    inx-e2e: \"1\""
  for a in "$@"; do echo "    $a"; done
  cat <<YAML
spec:
  ingressClassName: $class
  rules:
  - host: $host
    http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: app, port: {number: 80}}}}]}
YAML
}

{
  cat <<YAML
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: app}}
  template:
    metadata: {labels: {app: app}}
    spec:
      terminationGracePeriodSeconds: 2
      containers:
      - name: echo
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: [netexec, --http-port=8080]
        readinessProbe: {httpGet: {path: /healthz, port: 8080}, periodSeconds: 1}
---
apiVersion: v1
kind: Service
metadata: {name: app, namespace: $N}
spec: {selector: {app: app}, ports: [{port: 80, targetPort: 8080}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: errors, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: errors}}
  template:
    metadata: {labels: {app: errors}}
    spec:
      terminationGracePeriodSeconds: 2
      containers:
      - name: echo
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: [netexec, --http-port=8080]
        readinessProbe: {httpGet: {path: /healthz, port: 8080}, periodSeconds: 1}
---
apiVersion: v1
kind: Service
metadata: {name: errors, namespace: $N}
spec: {selector: {app: errors}, ports: [{port: 8080, targetPort: 8080}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: tlsapp, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: tlsapp}}
  template:
    metadata: {labels: {app: tlsapp}}
    spec:
      terminationGracePeriodSeconds: 2
      volumes: [{name: tls, secret: {secretName: tls}}]
      containers:
      - name: echo
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: [netexec, --http-port=8443, --tls-cert-file=/tls/tls.crt, --tls-private-key-file=/tls/tls.key]
        volumeMounts: [{name: tls, mountPath: /tls}]
---
apiVersion: v1
kind: Service
metadata: {name: tlsapp, namespace: $N}
spec: {selector: {app: tlsapp}, ports: [{port: 443, targetPort: 8443}]}
---
apiVersion: v1
kind: Service
metadata: {name: empty, namespace: $N}
spec: {selector: {app: nobody}, ports: [{port: 80, targetPort: 8080}]}
YAML
  ingress plain plain.inx.test
  ingress allowed allowed.inx.test "$P/allowlist-source-range: 0.0.0.0/0"
  ingress elsewhere elsewhere.inx.test "$P/allowlist-source-range: 192.0.2.0/24,2001:db8::/32"
  ingress whitelisted whitelisted.inx.test "$P/whitelist-source-range: 192.0.2.0/24"
  ingress denied denied.inx.test "$P/denylist-source-range: 0.0.0.0/0"
  ingress badrange badrange.inx.test "$P/allowlist-source-range: 10.0.0.0/8,nonsense"
  ingress mtls mtls.inx.test "$P/auth-tls-secret: $N/ca" "$P/auth-tls-verify-client: \"on\""
  ingress cert cert.inx.test "$P/auth-tls-secret: $N/ca" "$P/auth-tls-pass-certificate-to-upstream: \"true\""
  echo "  tls: [{hosts: [cert.inx.test], secretName: tls}]"
  ingress certcn certcn.inx.test "$P/auth-tls-secret: ca" "$P/auth-tls-match-cn: \"CN=bob\""
  echo "  tls: [{hosts: [certcn.inx.test], secretName: tls}]"
  ingress waf waf.inx.test "$P/enable-modsecurity: \"true\""
  ingress wafoff wafoff.inx.test "$P/enable-modsecurity: \"false\""
  ingress unknown unknown.inx.test "$P/http2-push-preload: \"true\""
  ingress tls tls.inx.test
  echo "  tls: [{hosts: [tls.inx.test], secretName: tls}]"
  ingress plainhttp plainhttp.inx.test "$P/ssl-redirect: \"false\""
  echo "  tls: [{hosts: [plainhttp.inx.test], secretName: tls}]"
  ingress force force.inx.test "$P/force-ssl-redirect: \"true\""
  ingress vhost vhost.inx.test "$P/upstream-vhost: backend.internal"
  ingress approot approot.inx.test "$P/app-root: /app1"
  ingress alias alias.inx.test "$P/server-alias: other-name.inx.test"
  ingress nolog nolog.inx.test "$P/enable-access-log: \"false\""
  ingress https https.inx.test "$P/backend-protocol: HTTPS"
  ingress cors cors.inx.test "$P/enable-cors: \"true\"" "$P/cors-allow-origin: \"https://app.inx.test, https://*.apps.inx.test\"" "$P/cors-expose-headers: X-Total"
  ingress corsany corsany.inx.test "$P/enable-cors: \"true\""
  ingress wwwsite wwwsite.inx.test "$P/from-to-www-redirect: \"true\""
  ingress passthrough pt.inx.test "$P/ssl-passthrough: \"true\"" | sed 's/name: app, port: {number: 80}/name: tlsapp, port: {number: 443}/'
  ingress satisfy satisfy.inx.test "$P/auth-type: basic" "$P/auth-secret: basic" "$P/allowlist-source-range: 0.0.0.0/0" "$P/satisfy: any" "$P/relative-redirects: \"true\"" "$P/proxy-cookie-path: / /app"
  ingress satisfyall satisfyall.inx.test "$P/auth-type: basic" "$P/auth-secret: basic" "$P/allowlist-source-range: 0.0.0.0/0"
  ingress mirror mirror.inx.test "$P/mirror-target: http://errors.$N.svc.cluster.local:8080/echo?msg=mirror\$request_uri"
  ingress proxyssl proxyssl.inx.test "$P/backend-protocol: HTTPS" "$P/proxy-ssl-secret: $N/ca" "$P/proxy-ssl-verify: \"on\"" "$P/proxy-ssl-name: app.internal" "$P/proxy-ssl-server-name: \"on\"" "$P/proxy-ssl-protocols: TLSv1.3"
  ingress errs errs.inx.test "$P/default-backend: errors" "$P/custom-http-errors: \"302,404\""
  ingress fallback fallback.inx.test "$P/default-backend: errors" | sed 's/name: app, port: {number: 80}/name: empty, port: {number: 80}/'
  ingress conn conn.inx.test "$P/connection-proxy-header: keep-alive"
  ingress custom custom.inx.test "$P/custom-headers: $N/custom"
  ingress customnone customnone.inx.test "$P/custom-headers: custom-missing"
  ingress customforeign customforeign.inx.test "$P/custom-headers: kube-system/kube-root-ca.crt"
  ingress keepauth keepauth.inx.test "$P/auth-url: http://authsvc.$N.svc.cluster.local/auth" "$P/auth-keepalive: \"8\"" \
    "$P/auth-proxy-set-headers: authheaders" "$P/auth-response-headers: X-Auth-Seen"
  ingress traced traced.inx.test "$P/enable-opentelemetry: \"true\"" "$P/opentelemetry-operation-name: inx-traced"
  ingress streamed streamed.inx.test "$P/stream-snippet: |" "      server { listen 9431; return stream-snippet; }"
  ingress fcgi fcgi.inx.test "$P/backend-protocol: FCGI" "$P/fastcgi-index: index.php" "$P/fastcgi-params-configmap: fcgiparams" \
    | sed 's/name: app, port: {number: 80}/name: php, port: {number: 9000}/'
  cat <<YAML
---
apiVersion: v1
kind: ConfigMap
metadata: {name: fcgiparams, namespace: $N}
data: {SCRIPT_FILENAME: "/srv/index.php", INX_PARAM: "from-the-configmap"}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: phpapp, namespace: $N}
data:
  index.php: |
    <?php echo "fcgi ", getenv("INX_PARAM"), " ", \$_SERVER["REQUEST_URI"];
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: php, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: php}}
  template:
    metadata: {labels: {app: php}}
    spec:
      terminationGracePeriodSeconds: 2
      volumes: [{name: app, configMap: {name: phpapp}}]
      containers:
      - name: fpm
        image: php:8.3-fpm-alpine
        volumeMounts: [{name: app, mountPath: /srv}]
---
apiVersion: v1
kind: Service
metadata: {name: php, namespace: $N}
spec: {selector: {app: php}, ports: [{port: 9000, targetPort: 9000}]}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: otelconf, namespace: $N}
data:
  config.yaml: |
    receivers: {otlp: {protocols: {grpc: {endpoint: "0.0.0.0:4317"}}}}
    exporters: {debug: {verbosity: detailed}}
    service: {pipelines: {traces: {receivers: [otlp], exporters: [debug]}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: otel, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: otel}}
  template:
    metadata: {labels: {app: otel}}
    spec:
      terminationGracePeriodSeconds: 2
      volumes: [{name: conf, configMap: {name: otelconf}}]
      containers:
      - name: collector
        image: otel/opentelemetry-collector:0.113.0
        args: [--config=/conf/config.yaml]
        volumeMounts: [{name: conf, mountPath: /conf}]
---
apiVersion: v1
kind: Service
metadata: {name: otel, namespace: $N}
spec: {selector: {app: otel}, ports: [{port: 4317, targetPort: 4317}]}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: custom, namespace: $N}
data: {X-Inx-Custom: "forty \$two"}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: authheaders, namespace: $N}
data: {X-Inx-Tenant: "tenant-\$host"}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: authconf, namespace: $N}
data:
  default.conf: |
    log_format conn '\$connection \$connection_requests \$http_x_inx_tenant';
    server {
      listen 8080;
      access_log /dev/stdout conn;
      location /auth { add_header X-Auth-Seen yes; return 200; }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: authsvc, namespace: $N}
spec:
  replicas: 1
  selector: {matchLabels: {app: authsvc}}
  template:
    metadata: {labels: {app: authsvc}}
    spec:
      terminationGracePeriodSeconds: 2
      volumes: [{name: conf, configMap: {name: authconf}}]
      containers:
      - name: nginx
        image: nginx:1.27-alpine
        volumeMounts: [{name: conf, mountPath: /etc/nginx/conf.d}]
---
apiVersion: v1
kind: Service
metadata: {name: authsvc, namespace: $N}
spec: {selector: {app: authsvc}, ports: [{port: 80, targetPort: 8080}]}
YAML
} | $K apply -f - >/dev/null
$K -n $N rollout status deploy/app --timeout=120s >/dev/null
$K -n $N rollout status deploy/errors --timeout=120s >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(code plain.inx.test)$(code unknown.inx.test)$(code badrange.inx.test)" != "200200503" ]; do sleep 1; waited=$((waited + 1)); done

check "an Ingress without ingress-nginx annotations" "$(code plain.inx.test)" "200"
echo "--- source ranges"
check "allowlist-source-range 0.0.0.0/0" "$(code allowed.inx.test)" "200"
check "allowlist-source-range without the client's address: 403" "$(code elsewhere.inx.test)" "403"
check "whitelist-source-range, the older name" "$(code whitelisted.inx.test)" "403"
check "denylist-source-range 0.0.0.0/0" "$(code denied.inx.test)" "403"
check "an invalid range: 503, as ingress-nginx denies the location" "$(code badrange.inx.test)" "503"
echo "--- what wxingress does not support"
check "enable-modsecurity: true: 503" "$(code waf.inx.test)" "503"
check "enable-modsecurity: false restricts nothing: served" "$(code wafoff.inx.test)" "200"
check "an unsupported annotation that restricts nothing: served" "$(code unknown.inx.test)" "200"
check "with a Warning event" "$(warned unknown "$P/http2-push-preload is not supported by wxingress and is ignored")" "1"
check "an Ingress with only supported ones has no Warning" "$($K -n $N get events --field-selector involvedObject.name=allowed,type=Warning -o name | wc -l | tr -d ' ')" "0"

echo "--- server and redirect annotations"
# redir <host> <path> [curl args]: the status and Location of the answer
redir() { X curl -s -o /dev/null --max-time 3 -w '%{http_code} %{redirect_url}' -H "Host: $1" "http://$ctrl$2"; }
check "TLS on the host: http goes to https as ingress-nginx sends it (no port, no trailing slash)" "$(redir tls.inx.test /some/path/)" "308 https://tls.inx.test/some/path"
check "the port of the Host header is not taken along" "$(redir tls.inx.test:8080 /x)" "308 https://tls.inx.test/x"
check "ssl-redirect: false keeps plain http" "$(code plainhttp.inx.test)" "200"
check "force-ssl-redirect without TLS on the host" "$(redir force.inx.test /hostname)" "308 https://force.inx.test/hostname"
check "app-root: / goes to it" "$(redir approot.inx.test /)" "302 http://approot.inx.test/app1"
check "server-alias: the other name is served" "$(code other-name.inx.test)" "200"
pod=$($K -n "$ns" get pod -l app.kubernetes.io/instance="$ns" -o name | head -1)
check "enable-access-log: false: no access log in its locations" "$($K -n "$ns" exec "$pod" -- grep -c 'access_log off' /etc/nginx/conf.d/$N-nolog.conf)" "1"
check "upstream-vhost: the Host the backend gets" "$($K -n "$ns" exec "$pod" -- grep -c 'proxy_set_header Host backend.internal;' /etc/nginx/conf.d/$N-vhost.conf)" "1"
check "backend-protocol: HTTPS: TLS to the backend" "$($K -n "$ns" exec "$pod" -- grep -c 'proxy_pass https://' /etc/nginx/conf.d/$N-https.conf)" "1"

echo "--- from-to-www-redirect"
check "www. of the host goes to the host, as ingress-nginx sends it" "$(redir www.wwwsite.inx.test /a/)" "308 http://wwwsite.inx.test:80/a"
ingress wwwtaker www.wwwsite.inx.test | $K apply -f - >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(code www.wwwsite.inx.test)" != "200" ]; do sleep 1; waited=$((waited + 1)); done
check "an Ingress that takes the www. host gets it (${waited}s)" "$(code www.wwwsite.inx.test)" "200"
check "and the host is still served" "$(code wwwsite.inx.test)" "200"

echo "--- custom-http-errors and default-backend"
body() { X curl -s --max-time 3 -H "Host: $1" "http://$ctrl$2"; }
check "a 302 of the backend (in the list) goes to the default-backend's /" "$(body errs.inx.test "/redirect?location=/elsewhere" | grep -c '^NOW:')" "1"
check "an answer not in the list stays the backend's" "$(body errs.inx.test /hostname | sed 's/-[^-]*-[^-]*$//')" "app"
check "no endpoints: the default-backend answers, with the request's URI" "$(body fallback.inx.test /hostname | sed 's/-[^-]*-[^-]*$//')" "errors"

echo "--- satisfy and one-directive annotations"
check "basic auth alone: 401 without credentials" "$(code satisfyall.inx.test)" "401"
check "satisfy: any with an allowed address: served without credentials" "$(code satisfy.inx.test)" "200"
check "relative-redirects, proxy-cookie-path in its locations" "$($K -n "$ns" exec "$pod" -- grep -cE 'absolute_redirect off|proxy_cookie_path / /app' /etc/nginx/conf.d/$N-satisfy.conf)" "2"

echo "--- mirror and proxy-ssl"
code mirror.inx.test >/dev/null
X curl -s -o /dev/null --max-time 3 -H "Host: mirror.inx.test" "http://$ctrl/mirrored-path-$$"
sleep 2
check "mirror-target: the request also reaches the mirror" "$($K -n $N logs deploy/errors | grep -c "msg=mirror/mirrored-path-$$")" "1"
check "proxy-ssl-*: verified TLS to the backend with the CA, the name and the protocol" \
  "$($K -n "$ns" exec "$pod" -- sh -c "grep -cE 'proxy_ssl_verify on|proxy_ssl_trusted_certificate|proxy_ssl_name \"app.internal\"|proxy_ssl_server_name on|proxy_ssl_protocols TLSv1.3' /etc/nginx/conf.d/$N-proxyssl.conf")" "5"

echo "--- client certificates (auth-tls)"
# tls <host> [curl args]: the status over https
tlsget() { h=$1; shift; X curl -sk -o /dev/null --max-time 3 -w '%{http_code}' --resolve "$h:443:$ctrl" "$@" "https://$h/hostname"; }
cc="--cert /var/tmp/inx-client.crt --key /var/tmp/inx-client.key"
check "without a client certificate: 400" "$(tlsget cert.inx.test)" "400"
check "with one of the CA: 200" "$(tlsget cert.inx.test $cc)" "200"
# a host without TLS has https with the default certificate, as in ingress-nginx
# (-ingress-nginx-defaults): auth-tls applies there; plain http, which such a
# host is not redirected from, has no client certificate to check - served
# without the check by ingress-nginx, 503 here
check "auth-tls on a host without TLS, https without a client certificate: 400" "$(tlsget mtls.inx.test)" "400"
check "and with one of the CA: 200" "$(tlsget mtls.inx.test $cc)" "200"
check "and plain http: 503 rather than served without the check" "$(code mtls.inx.test)" "503"
check "the backend gets ssl-client-verify" "$(X curl -sk --max-time 3 --resolve "cert.inx.test:443:$ctrl" $cc "https://cert.inx.test/header?key=ssl-client-verify")" "SUCCESS"
check "and the certificate (pass-certificate-to-upstream)" "$(X curl -sk --max-time 3 --resolve "cert.inx.test:443:$ctrl" $cc "https://cert.inx.test/header?key=ssl-client-cert" | grep -c BEGIN)" "1"
check "auth-tls-match-cn CN=bob: alice's certificate is 403" "$(tlsget certcn.inx.test $cc)" "403"

if $K -n "$ns" get deploy -o jsonpath='{..args}' | grep -q 'enable-tls-passthrough=true'; then
  echo "--- ssl-passthrough"
  waited=0
  while [ "$waited" -lt 60 ] && [ "$(X curl -sk --max-time 3 --resolve "pt.inx.test:443:$ctrl" https://pt.inx.test/hostname | sed 's/-[^-]*-[^-]*$//')" != "tlsapp" ]; do sleep 1; waited=$((waited + 1)); done
  check "ssl-passthrough: TLS of the name goes untouched to the backend, which terminates it (${waited}s)" "$(X curl -sk --max-time 3 --resolve "pt.inx.test:443:$ctrl" https://pt.inx.test/hostname | sed 's/-[^-]*-[^-]*$//')" "tlsapp"
  check "other names are still terminated by nginx" "$(tlsget cert.inx.test $cc)" "200"
fi

echo "--- CORS"
# hdr <host> <header> [curl args]: the value of a response header
hdr() { h=$1 name=$2; shift 2; X curl -s -o /dev/null -D - --max-time 3 -H "Host: $h" "$@" "http://$ctrl/hostname" | tr -d '\r' | grep -i "^$name:" | sed 's/^[^:]*: //'; }
check "a preflight of an allowed origin: 204" "$(X curl -s -o /dev/null --max-time 3 -w '%{http_code}' -X OPTIONS -H 'Host: cors.inx.test' -H 'Origin: https://app.inx.test' "http://$ctrl/hostname")" "204"
check "with its origin allowed" "$(hdr cors.inx.test Access-Control-Allow-Origin -X OPTIONS -H 'Origin: https://app.inx.test')" "https://app.inx.test"
check "and ingress-nginx's methods" "$(hdr cors.inx.test Access-Control-Allow-Methods -X OPTIONS -H 'Origin: https://app.inx.test')" "GET, PUT, POST, DELETE, PATCH, OPTIONS"
check "a request of a wildcard origin gets the headers" "$(hdr cors.inx.test Access-Control-Expose-Headers -H 'Origin: https://x1.apps.inx.test')" "X-Total"
check "another origin gets none" "$(hdr cors.inx.test Access-Control-Allow-Origin -H 'Origin: https://evil.test')" ""
check "and its preflight goes to the backend" "$(X curl -s -o /dev/null --max-time 3 -w '%{http_code}' -X OPTIONS -H 'Host: cors.inx.test' -H 'Origin: https://evil.test' "http://$ctrl/hostname")" "200"
check "without cors-allow-origin every origin: *" "$(hdr corsany.inx.test Access-Control-Allow-Origin -H 'Origin: https://any.test')" "*"

echo "--- connection-proxy-header, custom-headers, auth-proxy-set-headers, auth-keepalive, opentelemetry, stream-snippet"
$K -n $N rollout status deploy/authsvc --timeout=120s >/dev/null
$K -n $N rollout status deploy/otel --timeout=180s >/dev/null
$K -n $N rollout status deploy/php --timeout=180s >/dev/null
check "backend-protocol FCGI: php-fpm answers through FastCGI with the parameters of the ConfigMap" "$(body fcgi.inx.test /some/where)" "fcgi from-the-configmap /some/where"
check "connection-proxy-header: the backend gets its Connection" "$(X curl -s --max-time 3 -H 'Host: conn.inx.test' "http://$ctrl/header?key=Connection")" "keep-alive"
check "and only that one" "$($K -n "$ns" exec "$pod" -- grep -c 'proxy_set_header Connection' /etc/nginx/conf.d/$N-conn.conf)" "1"
cm=$($K -n "$ns" get cm -l app.kubernetes.io/instance="$ns" -o name | grep -v -e leader -e oidc | head -1)
allowed=$($K -n "$ns" get "$cm" -o jsonpath='{.data.global-allowed-response-headers}')
check "custom-headers of a header not in global-allowed-response-headers: 503, as ingress-nginx" "$(code custom.inx.test)" "503"
$K -n "$ns" patch "$cm" --type merge -p '{"data":{"global-allowed-response-headers":"X-Inx-Custom"}}' >/dev/null
waited=0
while [ "$waited" -lt 60 ] && [ "$(code custom.inx.test)" != "200" ]; do sleep 1; waited=$((waited + 1)); done
check "with the header allowed: the header of every answer, a \$ as it is (${waited}s)" "$(hdr custom.inx.test X-Inx-Custom)" 'forty $two'
check "also of an answer nginx makes itself (413)" "$(X curl -s -o /dev/null -D - --max-time 3 -H 'Host: custom.inx.test' -H 'Content-Length: 99999999' -X POST "http://$ctrl/hostname" | tr -d '\r' | grep -ci '^X-Inx-Custom:')" "1"
$K -n $N patch cm custom --type merge -p '{"data":{"X-Inx-Custom":"changed"}}' >/dev/null
waited=0
while [ "$waited" -lt 30 ] && [ "$(hdr custom.inx.test X-Inx-Custom)" != "changed" ]; do sleep 1; waited=$((waited + 1)); done
check "a change of the ConfigMap is served (${waited}s)" "$(hdr custom.inx.test X-Inx-Custom)" "changed"
check "custom-headers of a ConfigMap that does not exist: 503" "$(code customnone.inx.test)" "503"
check "of another namespace: 503 (no resources of other namespaces)" "$(code customforeign.inx.test)" "503"
$K -n $N create configmap custom-missing --from-literal=X-Inx-Custom=late >/dev/null
waited=0
while [ "$waited" -lt 30 ] && [ "$(code customnone.inx.test)" != "200" ]; do sleep 1; waited=$((waited + 1)); done
check "the ConfigMap created later: served with it (${waited}s)" "$(hdr customnone.inx.test X-Inx-Custom)" "late"
$K -n $N delete configmap custom-missing >/dev/null
waited=0
while [ "$waited" -lt 30 ] && [ "$(code customnone.inx.test)" != "503" ]; do sleep 1; waited=$((waited + 1)); done
check "and deleted again: 503 (${waited}s)" "$(code customnone.inx.test)" "503"

check "auth-proxy-set-headers: the auth service gets the headers of the ConfigMap" \
  "$(code keepauth.inx.test; sleep 1; $K -n $N logs deploy/authsvc --tail=1 | awk '{print $3}')" "200tenant-keepauth.inx.test"
check "auth-keepalive with HTTP/2 on: ignored with a warning, as ingress-nginx" "$(warned keepauth 'ignored with HTTP/2')" "1"
check "enable-opentelemetry without otel-exporter-endpoint: ignored with a warning" "$(warned traced 'no otel-exporter-endpoint')" "1"
# a second configuration: HTTP/2 off, which auth-keepalive needs, and an OTLP collector for the traces
$K -n "$ns" patch "$cm" --type merge -p "{\"data\":{\"http2\":\"false\",\"otel-exporter-endpoint\":\"otel.$N.svc.cluster.local:4317\"}}" >/dev/null
waited=0
while [ "$waited" -lt 60 ] && ! $K -n "$ns" exec "$pod" -- grep -q 'keepalive 8;' /etc/nginx/conf.d/$N-keepauth.conf 2>/dev/null; do sleep 1; waited=$((waited + 1)); done
# the file is written before nginx reloads with every other one; every worker keeps connections
# of its own: 16 requests at a time, so that some meet a worker again, until the reload is done
reused() {
  X sh -c "for i in \$(seq 1 16); do curl -s -o /dev/null --max-time 3 -H 'Host: keepauth.inx.test' http://$ctrl/hostname; done"
  sleep 1
  $K -n $N logs deploy/authsvc --tail=16 | awk '$2 > 1 { n++ } END { print (n > 0) ? "reused" : "not reused" }'
}
while [ "$waited" -lt 60 ] && [ "$(reused)" != "reused" ]; do waited=$((waited + 3)); done
check "auth-keepalive with HTTP/1.1: the auth requests share connections (${waited}s)" "$(reused)" "reused"
check "and are still answered" "$(code keepauth.inx.test)" "200"
check "enable-opentelemetry: the location traces to the collector of the ConfigMap" \
  "$($K -n "$ns" exec "$pod" -- grep -cE 'otel_trace on|otel_trace_context propagate|otel_span_name "inx-traced"' /etc/nginx/conf.d/$N-traced.conf)" "3"
check "and is served" "$(code traced.inx.test)" "200"
waited=0
while [ "$waited" -lt 30 ] && ! $K -n $N logs deploy/otel | grep -q inx-traced; do code traced.inx.test >/dev/null; sleep 1; waited=$((waited + 1)); done
check "its spans reach the collector (${waited}s)" "$($K -n $N logs deploy/otel | grep -c 'Name *: inx-traced' | awk '{print ($1 > 0) ? "yes" : "no"}')" "yes"
podip=$($K -n "$ns" get pod -l app.kubernetes.io/instance="$ns" -o jsonpath='{.items[0].status.podIP}')
check "stream-snippet: its server in the stream block" "$(X curl -s --max-time 2 "telnet://$podip:9431" </dev/null)" "stream-snippet"
$K -n $N delete ingress streamed >/dev/null
waited=0
while [ "$waited" -lt 30 ] && [ -n "$(X curl -s --max-time 1 "telnet://$podip:9431" </dev/null)" ]; do sleep 1; waited=$((waited + 1)); done
check "and gone with its Ingress (${waited}s)" "$(X curl -s --max-time 1 "telnet://$podip:9431" </dev/null)" ""
# the release's configuration as it was
$K -n "$ns" patch "$cm" --type json -p '[{"op":"remove","path":"/data/http2"},{"op":"remove","path":"/data/otel-exporter-endpoint"}]' >/dev/null
if [ -n "$allowed" ]; then
  $K -n "$ns" patch "$cm" --type merge -p "{\"data\":{\"global-allowed-response-headers\":\"$allowed\"}}" >/dev/null
else
  $K -n "$ns" patch "$cm" --type json -p '[{"op":"remove","path":"/data/global-allowed-response-headers"}]' >/dev/null
fi

X rm -f /var/tmp/inx-client.crt /var/tmp/inx-client.key
[ -n "${KEEP:-}" ] || $K delete ns $N --wait=false >/dev/null 2>&1

if [ "$failed" -gt 0 ]; then
  echo "$failed failed"
  exit 1
fi
echo "all passed"
