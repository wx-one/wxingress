package configs

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestNewDefaultConfigParamsForIngressNginx(t *testing.T) {
	t.Parallel()
	cfg := NewDefaultConfigParamsFor(context.Background(), false, true)

	checks := map[string][2]string{
		"ProxyConnectTimeout":              {cfg.ProxyConnectTimeout, "5s"},
		"ProxyReadTimeout":                 {cfg.ProxyReadTimeout, "60s"},
		"ProxySendTimeout":                 {cfg.ProxySendTimeout, "60s"},
		"ClientMaxBodySize":                {cfg.ClientMaxBodySize, "1m"},
		"ClientBodyBufferSize":             {cfg.ClientBodyBufferSize, "8k"},
		"ProxyBufferSize":                  {cfg.ProxyBufferSize, "4k"},
		"ProxyBuffers":                     {cfg.ProxyBuffers, "4 4k"},
		"ProxyRequestBuffering":            {cfg.ProxyRequestBuffering, "on"},
		"ProxyNextUpstream":                {cfg.ProxyNextUpstream, "error timeout"},
		"ProxyNextUpstreamTimeout":         {cfg.ProxyNextUpstreamTimeout, "0"},
		"ProxyRedirectFrom":                {cfg.ProxyRedirectFrom, "off"},
		"ServerTokens":                     {cfg.ServerTokens, "off"},
		"MainServerSSLProtocols":           {cfg.MainServerSSLProtocols, "TLSv1.2 TLSv1.3"},
		"MainServerSSLCiphers":             {cfg.MainServerSSLCiphers, ingressNginxDefaultSSLCiphers},
		"MainWorkerConnections":            {cfg.MainWorkerConnections, "16384"},
		"MainWorkerShutdownTimeout":        {cfg.MainWorkerShutdownTimeout, "240s"},
		"MainKeepaliveTimeout (unchanged)": {cfg.MainKeepaliveTimeout, "75s"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if cfg.ProxyBuffering {
		t.Error("ProxyBuffering is on")
	}
	if cfg.ProxyNextUpstreamTries == nil || *cfg.ProxyNextUpstreamTries != 3 {
		t.Errorf("ProxyNextUpstreamTries = %v, want 3", cfg.ProxyNextUpstreamTries)
	}
	if cfg.Keepalive != 64 || cfg.HTTPRedirectCode != 308 || cfg.LimitReqRejectCode != 503 || cfg.VariablesHashMaxSize != 2048 {
		t.Errorf("keepalive %d, redirect code %d, limit_req_status %d, variables_hash_max_size %d", cfg.Keepalive, cfg.HTTPRedirectCode, cfg.LimitReqRejectCode, cfg.VariablesHashMaxSize)
	}
	if !cfg.HSTS || !cfg.HSTSIncludeSubdomains || cfg.HSTSPreload || cfg.HSTSMaxAge != 31536000 {
		t.Errorf("HSTS %v, includeSubDomains %v, preload %v, max-age %d", cfg.HSTS, cfg.HSTSIncludeSubdomains, cfg.HSTSPreload, cfg.HSTSMaxAge)
	}
	if !cfg.HTTP2 || !cfg.MainServerSSLPreferServerCiphers || !cfg.IngressNginxDefaults {
		t.Errorf("HTTP2 %v, ssl_prefer_server_ciphers %v, IngressNginxDefaults %v", cfg.HTTP2, cfg.MainServerSSLPreferServerCiphers, cfg.IngressNginxDefaults)
	}
}

func TestNewDefaultConfigParamsForKeepsTheControllersDefaults(t *testing.T) {
	t.Parallel()
	cfg := NewDefaultConfigParamsFor(context.Background(), false, false)
	if cfg.ProxyConnectTimeout != "60s" || !cfg.ProxyBuffering || cfg.HSTS || !cfg.HSTSPreload || cfg.IngressNginxDefaults || cfg.HTTPRedirectCode != 301 {
		t.Errorf("the defaults of the controller changed: %+v", cfg)
	}
}

func TestParseConfigMapWithIngressNginxDefaults(t *testing.T) {
	t.Parallel()
	parse := func(data map[string]string) *ConfigParams {
		t.Helper()
		cfg, ok := ParseConfigMap(context.Background(), &v1.ConfigMap{Data: data}, false, false, false, false, false, false, true, makeEventLogger())
		if !ok {
			t.Fatalf("ConfigMap %v is invalid", data)
		}
		return cfg
	}

	cfg := parse(nil)
	if cfg.ProxyConnectTimeout != "5s" || cfg.ProxyBuffering || !cfg.IngressNginxDefaults {
		t.Errorf("an empty ConfigMap does not keep the defaults of ingress-nginx: connect timeout %q, buffering %v", cfg.ProxyConnectTimeout, cfg.ProxyBuffering)
	}

	// the keys of the ConfigMap win
	cfg = parse(map[string]string{
		"proxy-connect-timeout": "30s",
		"proxy-buffering":       "true",
		"server-tokens":         "true",
		"http-redirect-code":    "301",
		"keepalive":             "0",
		"worker-connections":    "2048",
	})
	if cfg.ProxyConnectTimeout != "30s" || !cfg.ProxyBuffering || cfg.ServerTokens != "on" || cfg.HTTPRedirectCode != 301 || cfg.Keepalive != 0 || cfg.MainWorkerConnections != "2048" {
		t.Errorf("the ConfigMap does not win: %+v", cfg)
	}

	// a proxy-buffer-size alone sizes the four buffers of ingress-nginx too
	cfg = parse(map[string]string{"proxy-buffer-size": "16k"})
	if cfg.ProxyBufferSize != "16k" || cfg.ProxyBuffers != "4 16k" {
		t.Errorf("proxy_buffer_size %q, proxy_buffers %q, want 16k and 4 16k", cfg.ProxyBufferSize, cfg.ProxyBuffers)
	}
	cfg = parse(map[string]string{"proxy-buffer-size": "16k", "proxy-buffers": "8 8k"})
	if cfg.ProxyBuffers != "8 8k" {
		t.Errorf("proxy_buffers %q, want the 8 8k of the ConfigMap", cfg.ProxyBuffers)
	}

	// the options of HSTS apply without the hsts key, as HSTS is on
	cfg = parse(map[string]string{"hsts-max-age": "600", "hsts-preload": "true"})
	if !cfg.HSTS || cfg.HSTSMaxAge != 600 || !cfg.HSTSPreload || !cfg.HSTSIncludeSubdomains {
		t.Errorf("HSTS %v, max-age %d, preload %v, includeSubDomains %v", cfg.HSTS, cfg.HSTSMaxAge, cfg.HSTSPreload, cfg.HSTSIncludeSubdomains)
	}
	cfg = parse(map[string]string{"hsts": "false"})
	if cfg.HSTS {
		t.Error("hsts false does not turn HSTS off")
	}
}

func TestParseConfigMapWithoutIngressNginxDefaultsIgnoresHSTSOptionsAlone(t *testing.T) {
	t.Parallel()
	cfg, _ := ParseConfigMap(context.Background(), &v1.ConfigMap{Data: map[string]string{"hsts-max-age": "600", "proxy-buffer-size": "16k"}},
		false, false, false, false, false, false, false, makeEventLogger())
	if cfg.HSTS || cfg.HSTSMaxAge != 2592000 || cfg.ProxyBuffers != "" {
		t.Errorf("HSTS %v, max-age %d, proxy_buffers %q", cfg.HSTS, cfg.HSTSMaxAge, cfg.ProxyBuffers)
	}
}

func TestGenerateNginxCfgWithIngressNginxDefaults(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Annotations[IngressNginxProxyConnectTimeoutAnnotation] = "15"
	ingEx.Ingress.Annotations[IngressNginxProxyBufferSizeAnnotation] = "16k"
	ingEx.Ingress.Annotations["nginx.org/proxy-buffering"] = "true"

	cfg, _ := generateNginxCfg(NginxCfgParams{
		ingEx:         &ingEx,
		BaseCfgParams: NewDefaultConfigParamsFor(context.Background(), false, true),
		staticParams:  &StaticConfigParams{},
	})

	tea := findLocation(cfg, "/tea")
	if tea == nil {
		t.Fatal("the config lacks /tea")
	}
	// the annotations win over the defaults
	if tea.ProxyConnectTimeout != "15s" || tea.ProxyBufferSize != "16k" || tea.ProxyBuffers != "4 16k" || !tea.ProxyBuffering {
		t.Errorf("/tea: connect timeout %q, proxy_buffer_size %q, proxy_buffers %q, buffering %v", tea.ProxyConnectTimeout, tea.ProxyBufferSize, tea.ProxyBuffers, tea.ProxyBuffering)
	}
	// the rest is that of ingress-nginx
	if tea.ProxyReadTimeout != "60s" || tea.ClientBodyBufferSize != "8k" || tea.ProxyNextUpstreamTries == nil || *tea.ProxyNextUpstreamTries != 3 || !tea.IngressNginxProxyHeaders {
		t.Errorf("/tea: read timeout %q, client_body_buffer_size %q, tries %v, ingress-nginx headers %v", tea.ProxyReadTimeout, tea.ClientBodyBufferSize, tea.ProxyNextUpstreamTries, tea.IngressNginxProxyHeaders)
	}
	if tea.Upstream.Keepalive != "64" {
		t.Errorf("keepalive %q, want 64", tea.Upstream.Keepalive)
	}
	server := cfg.Servers[0]
	if !server.HSTS || server.HSTSPreload || server.HSTSMaxAge != 31536000 || server.HTTPRedirectCode != 308 || server.ServerTokens != "off" || server.ProxyRedirectFrom != "off" || !server.HTTP2 {
		t.Errorf("server: %+v", server)
	}
}
