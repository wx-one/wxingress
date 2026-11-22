package configs

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestTranslateIngressNginxConfigMap(t *testing.T) {
	t.Parallel()
	out, warnings, unsafe := TranslateIngressNginxConfigMap(map[string]string{
		"proxy-body-size": "20m", "keep-alive": "30", "use-proxy-protocol": "true", "proxy-read-timeout": "120",
		"enable-real-ip": "true", "forwarded-for-header": "X-Real-IP", "proxy-real-ip-cidr": "10.0.0.0/8",
		"log-format-escape-json": "true", "otlp-collector-host": "otel", "otlp-collector-port": "4318",
		"whitelist-source-range": "10.0.0.0/8, 192.0.2.0/24", "block-cidrs": "10.1.0.0/16",
		"strict-validate-path-type": "false", "global-auth-url": "http://auth", "enable-brotli": "true",
		"client-max-body-size": "5m",
	})
	want := map[string]string{
		"client-max-body-size": "5m", "keepalive-timeout": "30s", "proxy-protocol": "true", "proxy-read-timeout": "120",
		"real-ip-header": "proxy_protocol", "set-real-ip-from": "10.0.0.0/8", "log-format-escaping": "json",
		"otel-exporter-endpoint": "otel:4318",
		"otel-trace-context":     "propagate",
		"denylist-source-range":  "10.1.0.0/16",
		"allowlist-source-range": "10.0.0.0/8,192.0.2.0/24",
	}
	if !maps.Equal(out, want) {
		t.Errorf("got  %v\nwant %v", out, want)
	}
	all := strings.Join(warnings, "\n")
	for _, w := range []string{"proxy-body-size is ignored: client-max-body-size is set", "global-auth-url is not supported by wxingress: the Ingresses are served WITHOUT it", "enable-brotli is not supported"} {
		if !strings.Contains(all, w) {
			t.Errorf("no %q in\n%s", w, all)
		}
	}
	if strings.Contains(all, "strict-validate-path-type") {
		t.Error("a key wxingress needs nothing for is warned about")
	}
	if len(unsafe) != 1 || unsafe[0] != "global-auth-url" {
		t.Errorf("unsafe %v", unsafe)
	}
}

func TestParseConfigMapTakesIngressNginxKeys(t *testing.T) {
	t.Parallel()
	cm := &v1.ConfigMap{Data: map[string]string{"proxy-body-size": "3m", "keep-alive": "20"}}
	with, _ := ParseConfigMap(context.Background(), cm, false, false, false, false, false, false, true, nil)
	if with.ClientMaxBodySize != "3m" || with.MainKeepaliveTimeout != "20s" {
		t.Errorf("with -ingress-nginx-defaults: %q %q", with.ClientMaxBodySize, with.MainKeepaliveTimeout)
	}
	without, _ := ParseConfigMap(context.Background(), cm, false, false, false, false, false, false, false, nil)
	if without.ClientMaxBodySize == "3m" {
		t.Error("ingress-nginx's keys are taken without -ingress-nginx-defaults")
	}
}

// The client's address behind the PROXY protocol is the header's, as in
// ingress-nginx, whatever forwarded-for-header says; without it that header;
// a real-ip-header or set-real-ip-from of wxingress's own wins.
func TestTranslateIngressNginxRealIP(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		data         map[string]string
		header, from string
	}{
		{map[string]string{"use-proxy-protocol": "true", "proxy-real-ip-cidr": "192.0.2.4/32"}, "proxy_protocol", "192.0.2.4/32"},
		{map[string]string{"enable-real-ip": "true", "forwarded-for-header": "X-Real-IP"}, "X-Real-IP", "0.0.0.0/0"},
		{map[string]string{"enable-real-ip": "true"}, "X-Forwarded-For", "0.0.0.0/0"},
		{map[string]string{"use-proxy-protocol": "true", "proxy-real-ip-cidr": "10.0.0.0/8", "real-ip-header": "X-Real-IP", "set-real-ip-from": "192.0.2.1"}, "X-Real-IP", "192.0.2.1"},
	} {
		out, _, _ := TranslateIngressNginxConfigMap(test.data)
		if out["real-ip-header"] != test.header || out["set-real-ip-from"] != test.from {
			t.Errorf("%v: real-ip-header %q set-real-ip-from %q, want %q %q", test.data, out["real-ip-header"], out["set-real-ip-from"], test.header, test.from)
		}
	}
}

// The global source ranges are rules of every server of their own, not server
// snippets: an Ingress or VirtualServer with snippets of its own replaced
// those, and the rules with them.
func TestParseConfigMapGlobalAccessRules(t *testing.T) {
	t.Parallel()
	parse := func(data map[string]string, defaults bool) []string {
		t.Helper()
		cfg, _ := ParseConfigMap(context.Background(), &v1.ConfigMap{Data: data}, false, false, false, false, false, false, defaults, nil)
		return cfg.GlobalAccessRules
	}
	for _, tc := range []struct {
		name string
		data map[string]string
		want []string
	}{
		{"whitelist and block-cidrs", map[string]string{"whitelist-source-range": "10.0.0.0/8, 192.0.2.1", "block-cidrs": "10.1.0.0/16"}, []string{"deny 10.1.0.0/16", "allow 10.0.0.0/8", "allow 192.0.2.1", "deny all"}},
		{"allowlist wins over whitelist", map[string]string{"allowlist-source-range": "10.0.0.0/8", "whitelist-source-range": "192.0.2.0/24"}, []string{"allow 10.0.0.0/8", "deny all"}},
		{"denylist alone", map[string]string{"denylist-source-range": "198.51.100.0/24"}, []string{"deny 198.51.100.0/24"}},
		{"not a list of addresses", map[string]string{"allowlist-source-range": "intranet"}, []string{"deny 0.0.0.0/0", "deny ::/0"}},
		{"an invalid CIDR", map[string]string{"allowlist-source-range": "10.0.0.0/99"}, []string{"deny all"}},
		{"with server snippets", map[string]string{"allowlist-source-range": "10.0.0.0/8", "server-snippets": "# mine"}, []string{"allow 10.0.0.0/8", "deny all"}},
	} {
		if got := parse(tc.data, true); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := parse(map[string]string{"allowlist-source-range": "10.0.0.0/8"}, false); got != nil {
		t.Errorf("without -ingress-nginx-defaults: %q", got)
	}
	cfg, _ := ParseConfigMap(context.Background(), &v1.ConfigMap{Data: map[string]string{"allowlist-source-range": "10.0.0.0/8", "server-snippets": "# mine"}}, false, false, false, false, false, true, true, nil)
	if !slices.Equal(cfg.ServerSnippets, []string{"# mine"}) {
		t.Errorf("the server snippets are %q", cfg.ServerSnippets)
	}
}
