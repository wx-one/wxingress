package version2

import (
	"strings"
	"testing"
)

// The PROXY of ingress-nginx's tcp-services: the first takes the PROXY protocol of the clients, the second sends it on.
func TestExecuteTemplateForTransportServerWithProxyProtocol(t *testing.T) {
	t.Parallel()
	for _, executor := range []*TemplateExecutor{newTmplExecutorNGINX(t), newTmplExecutorNGINXPlus(t)} {
		cfg := transportServerCfg
		cfg.Server.SSL = &StreamSSL{}
		cfg.Server.ProxyProtocol = true
		cfg.Server.UpstreamProxyProtocol = true
		data, err := executor.ExecuteTransportServerTemplate(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		conf := string(data)
		for _, want := range []string{"listen 1234 proxy_protocol udp;", "listen [::]:1234 proxy_protocol udp;", "proxy_protocol on;"} {
			if !strings.Contains(conf, want) {
				t.Errorf("no %q in\n%s", want, conf)
			}
		}
		cfg.Server.ProxyProtocol, cfg.Server.UpstreamProxyProtocol = false, false
		data, _ = executor.ExecuteTransportServerTemplate(&cfg)
		if strings.Contains(string(data), "proxy_protocol") {
			t.Errorf("proxy_protocol without PROXY:\n%s", data)
		}
	}
}
