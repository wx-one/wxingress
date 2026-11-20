package configs

import (
	"context"
	"maps"
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
		"server-snippets":        "deny 10.1.0.0/16;\nallow 10.0.0.0/8;\nallow 192.0.2.0/24;\ndeny all;",
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
