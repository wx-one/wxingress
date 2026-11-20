package configs

import (
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// nginxOrgConfigMapKeys are the keys of the ConfigMap of wxingress (NGINX Ingress Controller's, docs).
var nginxOrgConfigMapKeys = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`access-log access-log-off add-header add-header-inherit
	app-protect-compressed-requests-action app-protect-cookie-seed app-protect-cpu-thresholds app-protect-dos-arb-fqdn
	app-protect-dos-log-format app-protect-dos-log-format-escaping app-protect-failure-mode-action
	app-protect-physical-memory-util-thresholds app-protect-reconnect-period-seconds client-body-buffer-size
	client-max-body-size default-server-access-log-off default-server-return disable-forwarded-headers
	enforce-initial-report error-log-level external-status-address fail-timeout hsts hsts-behind-proxy
	hsts-include-subdomains hsts-max-age hsts-preload http2 http-redirect-code http-snippets ingress-template keepalive
	keepalive-requests keepalive-timeout lb-method license-token-secret-name location-snippets log-format
	log-format-escaping main-snippets main-template map-hash-bucket-size map-hash-max-size max-fails max-conns
	oidc-access-tokens-timeout oidc-access-tokens-zone-size oidc-id-tokens-timeout oidc-id-tokens-zone-size
	oidc-pkce-timeout oidc-pkce-zone-size oidc-refresh-tokens-timeout oidc-refresh-tokens-zone-size oidc-sids-timeout
	oidc-sids-zone-size otel-exporter-endpoint otel-exporter-header-name otel-exporter-header-value otel-service-name
	otel-trace-context otel-trace-in-http proxy-buffering proxy-buffers proxy-buffer-size proxy-busy-buffers-size
	proxy-connect-timeout proxy-hide-headers proxy-max-temp-file-size proxy-pass-headers proxy-protocol
	proxy-read-timeout proxy-send-timeout real-ip-header real-ip-recursive redirect-to-https resolver-addresses
	resolver-ipv6 resolver-timeout resolver-valid server-names-hash-bucket-size server-names-hash-max-size
	server-snippets server-tokens set-real-ip-from ssl-certificate-secret-name ssl-ciphers ssl-dhparam-file
	ssl-prefer-server-ciphers ssl-protocols ssl-redirect ssl-trusted-certificate-secret-name ssl-verify
	stream-log-format stream-log-format-escaping stream-snippets transportserver-template upstream-zone-size
	global-allowed-response-headers usage-report-endpoint usage-report-interval usage-report-proxy-host use-forwarded-headers variables-hash-bucket-size
	variables-hash-max-size virtualserver-template worker-connections worker-cpu-affinity worker-processes
	worker-rlimit-nofile worker-shutdown-timeout zone-sync zone-sync-port zone-sync-resolver-addresses
	zone-sync-resolver-ipv6 zone-sync-resolver-valid slow-start limit-req-status-code`) {
		nginxOrgConfigMapKeys[k] = true
	}
}

// ingressNginxConfigMapRenames are the ingress-nginx ConfigMap keys that are another key of wxingress.
var ingressNginxConfigMapRenames = map[string]string{
	"proxy-body-size":                "client-max-body-size",
	"keep-alive-requests":            "keepalive-requests",
	"upstream-keepalive-connections": "keepalive",
	"max-worker-connections":         "worker-connections",
	"max-worker-open-files":          "worker-rlimit-nofile",
	"use-proxy-protocol":             "proxy-protocol",
	"server-name-hash-max-size":      "server-names-hash-max-size",
	"server-name-hash-bucket-size":   "server-names-hash-bucket-size",
	"log-format-upstream":            "log-format",
	"log-format-stream":              "stream-log-format",
	"http-snippet":                   "http-snippets",
	"server-snippet":                 "server-snippets",
	"location-snippet":               "location-snippets",
	"main-snippet":                   "main-snippets",
	"stream-snippet":                 "stream-snippets",
	"use-http2":                      "http2",
	"hide-headers":                   "proxy-hide-headers",
}

// ingressNginxConfigMapNoops are ingress-nginx keys that wxingress needs no counterpart for: it behaves so anyway.
var ingressNginxConfigMapNoops = map[string]string{
	"strict-validate-path-type":       "wxingress does not validate path types strictly",
	"allow-snippet-annotations":       "snippets are -enable-snippets",
	"annotations-risk-level":          "snippets are -enable-snippets",
	"enable-annotation-validations":   "annotations are always validated",
	"allow-cross-namespace-resources": "Secrets of other namespaces are not taken",
	"no-tls-redirect-locations":       "ACME challenges are never redirected",
	"compute-full-forwarded-for":      "with -ingress-nginx-defaults as ingress-nginx's default (false)",
	"generate-request-id":             "with -ingress-nginx-defaults as ingress-nginx's default (true)",
	"ssl-session-tickets":             "nginx's default",
}

// ingressNginxConfigMapUnsafe are keys that restrict who gets an answer;
// without a counterpart the Ingresses would be served without them.
var ingressNginxConfigMapUnsafe = regexp.MustCompile(`^(global-auth-.*|enable-modsecurity|enable-owasp-modsecurity-crs|modsecurity-snippet|block-user-agents|block-referers|global-rate-limit-.*)$`)

var ingressNginxCIDRListRegexp = regexp.MustCompile(`^[0-9a-fA-F.:/, ]+$`)

// TranslateIngressNginxConfigMap turns the keys of an ingress-nginx ConfigMap
// into those of wxingress: the same key as it is, a renamed one under its
// name (where wxingress has the key itself, that one wins), the global
// allow, deny and block lists into the servers of the resources. It returns, sorted, a
// warning for each key it cannot take; unsafe tells which of them restrict
// access, which the Ingresses are then served without.
func TranslateIngressNginxConfigMap(data map[string]string) (out map[string]string, warnings []string, unsafe []string) {
	out = map[string]string{}
	var denies, allows []string
	denyAll := false
	keys := sortedKeys(data)
	for _, k := range keys {
		v := data[k]
		switch {
		case nginxOrgConfigMapKeys[k]:
			if _, set := out[k]; !set {
				out[k] = v
			}
		case ingressNginxConfigMapRenames[k] != "":
			to := ingressNginxConfigMapRenames[k]
			if _, own := data[to]; own {
				warnings = append(warnings, fmt.Sprintf("%s is ignored: %s is set", k, to))
				continue
			}
			out[to] = v
			if k == "log-format-upstream" || k == "log-format-stream" {
				warnings = append(warnings, fmt.Sprintf("%s is taken as %s; ingress-nginx's variables ($proxy_upstream_name, $req_id, ...) do not exist here", k, to))
			}
		case k == "keep-alive":
			if _, err := strconv.Atoi(v); err == nil {
				v += "s"
			}
			out["keepalive-timeout"] = v
		case k == "log-format-escape-json" && v == "true":
			out["log-format-escaping"] = "json"
		case k == "log-format-escape-none" && v == "true":
			out["log-format-escaping"] = "none"
		case k == "otlp-collector-host":
			port := data["otlp-collector-port"]
			if port == "" {
				port = "4317"
			}
			out["otel-exporter-endpoint"] = v + ":" + port
			if _, set := data["opentelemetry-trust-incoming-span"]; !set {
				// ingress-nginx's default: spans of the requests are the parents
				out["otel-trace-context"] = "propagate"
			}
		case k == "otlp-collector-port":
		case k == "enable-opentelemetry":
			out["otel-trace-in-http"] = v
		case k == "opentelemetry-trust-incoming-span":
			if v == "false" {
				out["otel-trace-context"] = "inject"
			} else {
				out["otel-trace-context"] = "propagate"
			}
		case k == "enable-real-ip", k == "forwarded-for-header", k == "proxy-real-ip-cidr":
			if data["enable-real-ip"] == "true" || data["use-proxy-protocol"] == "true" {
				// as ingress-nginx: behind the PROXY protocol the client is the
				// header's address, else that of forwarded-for-header
				header := data["forwarded-for-header"]
				if data["use-proxy-protocol"] == "true" {
					header = "proxy_protocol"
				} else if header == "" {
					header = "X-Forwarded-For"
				}
				from := data["proxy-real-ip-cidr"]
				if from == "" {
					from = "0.0.0.0/0"
				}
				// wxingress's own keys win
				if _, own := data["real-ip-header"]; !own {
					out["real-ip-header"] = header
				}
				if _, own := data["set-real-ip-from"]; !own {
					out["set-real-ip-from"] = from
				}
			}
		case k == "whitelist-source-range", k == "allowlist-source-range", k == "denylist-source-range", k == "block-cidrs":
			if !ingressNginxCIDRListRegexp.MatchString(v) {
				warnings = append(warnings, fmt.Sprintf("%s %q is not a list of addresses: every request is denied", k, v))
				unsafe = append(unsafe, k)
				denyAll = true
				continue
			}
			for _, r := range strings.Split(v, ",") {
				if r = strings.TrimSpace(r); r == "" {
					continue
				} else if k == "denylist-source-range" || k == "block-cidrs" {
					denies = append(denies, "deny "+r+";")
				} else if k == "whitelist-source-range" && data["allowlist-source-range"] != "" {
					// allowlist-source-range wins
				} else {
					allows = append(allows, "allow "+r+";")
				}
			}
		case ingressNginxConfigMapNoops[k] != "":
		case ingressNginxConfigMapUnsafe.MatchString(k):
			warnings = append(warnings, fmt.Sprintf("%s is not supported by wxingress: the Ingresses are served WITHOUT it", k))
			unsafe = append(unsafe, k)
		default:
			warnings = append(warnings, fmt.Sprintf("%s is not supported by wxingress and is ignored", k))
		}
	}
	// the denied first, as nginx takes the first rule that matches; the
	// allowed with deny all; Ingresses with source ranges of their own replace them
	httpAccess := denies
	if denyAll {
		httpAccess = append(httpAccess, "deny all;")
	} else if len(allows) > 0 {
		httpAccess = append(append(httpAccess, allows...), "deny all;")
	}
	if len(httpAccess) > 0 {
		// in the servers of the resources, not the http block: nginx's own servers (status, the module's API) stay open
		if s := out["server-snippets"]; s != "" {
			httpAccess = append([]string{s}, httpAccess...)
		}
		out["server-snippets"] = strings.Join(httpAccess, "\n")
	}
	return out, warnings, unsafe
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range maps.Keys(m) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
