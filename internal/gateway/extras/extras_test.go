package extras

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wx-one/wxingress/internal/configs/version2"
)

func TestApply(t *testing.T) {
	t.Parallel()
	e := &Extras{
		HTTP:   []string{"map $http_x $v { default 1; }"},
		Server: []string{"location = /m { internal; proxy_pass http://" + UpstreamRef("be-1") + "$request_uri; }"},
		Actions: map[string]*Action{
			"a1": {
				SetHeaders:  []Header{{Name: "host", Value: "${h}"}, {Name: "X-Set", Value: "v"}, {Name: "X-Gone", Value: ""}},
				HideHeaders: []string{"X-Hidden"},
				AddHeaders:  []Header{{Name: "X-Added", Value: "a"}},
				Rewrites:    []string{"^ $r break"},
				Directives:  []string{"mirror /m;"},
			},
			"a2": {Return: &version2.Return{Code: 303, Text: "https://$host$request_uri"}},
		},
	}
	cfg := &version2.VirtualServerConfig{Server: version2.Server{Locations: []version2.Location{
		{
			Path:            "/",
			ProxyPass:       "http://up",
			ProxySetHeaders: []version2.Header{{Name: MarkerHeader, Value: "a1"}, {Name: "Host", Value: "$host"}},
		},
		{
			Path:            "/internal_location_matches_0_match_0",
			Internal:        true,
			ProxyPass:       "http://up$request_uri",
			ProxySetHeaders: []version2.Header{{Name: "Host", Value: "$host"}, {Name: MarkerHeader, Value: "a1"}},
		},
		{
			Path:                 "/r",
			ProxyInterceptErrors: true,
			InternalProxyPass:    "http://unix:/var/lib/nginx/nginx-418-server.sock",
			ErrorPages:           []version2.ErrorPage{{Name: MarkerURL("a2"), Codes: "418", ResponseCode: 302}},
		},
		{Path: "/other", ProxyPass: "http://up"},
	}}}

	names := map[string]string{"be-1": "vs_ns_GW-x_be-1"}
	if err := Apply(map[string]string{Annotation: e.Encode()}, cfg, func(n string) string { return names[n] }, func(n string) string { return "$c_" + n }); err != nil {
		t.Fatal(err)
	}

	if !cmp.Equal(cfg.HTTPSnippets, e.HTTP) {
		t.Errorf("http %v", cfg.HTTPSnippets)
	}
	if want := "location = /m { internal; proxy_pass http://vs_ns_GW-x_be-1$request_uri; }"; len(cfg.Server.Snippets) != 1 || cfg.Server.Snippets[0] != want {
		t.Errorf("server %v", cfg.Server.Snippets)
	}

	for _, loc := range cfg.Server.Locations[:2] {
		want := []version2.Header{{Name: "Host", Value: "${h}"}, {Name: "X-Set", Value: "v"}, {Name: "X-Gone", Value: ""}}
		if d := cmp.Diff(want, loc.ProxySetHeaders); d != "" {
			t.Errorf("%s: headers (-want +got):\n%s", loc.Path, d)
		}
		if !cmp.Equal(loc.ProxyHideHeaders, []string{"X-Hidden"}) || len(loc.AddHeaders) != 1 || !loc.AddHeaders[0].Always {
			t.Errorf("%s: response headers %v %v", loc.Path, loc.ProxyHideHeaders, loc.AddHeaders)
		}
		if !cmp.Equal(loc.Rewrites, []string{"^ $r break"}) || !cmp.Equal(loc.Snippets, []string{"mirror /m;"}) {
			t.Errorf("%s: rewrites %v snippets %v", loc.Path, loc.Rewrites, loc.Snippets)
		}
	}
	// a rewritten URI goes to the upstream as it is, not the request's
	if got := cfg.Server.Locations[1].ProxyPass; got != "http://up" {
		t.Errorf("internal proxy_pass %q", got)
	}
	if got := cfg.Server.Locations[0].ProxyPass; got != "http://up" {
		t.Errorf("proxy_pass %q", got)
	}

	r := cfg.Server.Locations[2]
	if r.InternalProxyPass != "" || len(r.ErrorPages) != 0 || r.ProxyInterceptErrors {
		t.Errorf("the redirect still has its own action: %+v", r)
	}
	if !cmp.Equal(r.Snippets, []string{`return 303 "https://$host$request_uri";`}) {
		t.Errorf("redirect %v", r.Snippets)
	}
	if o := cfg.Server.Locations[3]; len(o.Snippets) != 0 || len(o.ProxySetHeaders) != 0 {
		t.Errorf("an unmarked location changed: %+v", o)
	}
}

func TestApplyFindsEveryAction(t *testing.T) {
	t.Parallel()
	e := &Extras{Actions: map[string]*Action{"a1": {}}}
	cfg := &version2.VirtualServerConfig{Server: version2.Server{Locations: []version2.Location{{Path: "/"}}}}
	err := Apply(map[string]string{Annotation: e.Encode()}, cfg, func(n string) string { return n }, func(n string) string { return n })
	if err == nil || !strings.Contains(err.Error(), "a1") {
		t.Errorf("error %v, want one about a1", err)
	}

	cfg = &version2.VirtualServerConfig{Server: version2.Server{Locations: []version2.Location{
		{Path: "/", ProxySetHeaders: []version2.Header{{Name: MarkerHeader, Value: "a9"}}},
	}}}
	if err := Apply(map[string]string{Annotation: e.Encode()}, cfg, func(n string) string { return n }, func(n string) string { return n }); err == nil {
		t.Error("a mark without extras is no error")
	}
}

func TestNothingToApply(t *testing.T) {
	t.Parallel()
	cfg := &version2.VirtualServerConfig{}
	if err := Apply(nil, cfg, nil, nil); err != nil {
		t.Error(err)
	}
	if err := Apply(map[string]string{Annotation: "{"}, cfg, nil, nil); err == nil {
		t.Error("a broken annotation is no error")
	}
}

func TestApplyStream(t *testing.T) {
	t.Parallel()
	e := &Extras{
		Stream:        []string{"split_clients x $v { 50% " + UpstreamRef("be-1") + "; * " + UpstreamRef("be-2") + "; }"},
		StreamServer:  []string{"ssl_preread on;"},
		ProxyPass:     "$v",
		DefaultServer: true,
	}
	cfg := &version2.TransportServerConfig{Server: version2.StreamServer{ProxyPass: "ts_ns_GW-x_be-1"}}
	names := map[string]string{"be-1": "ts_ns_GW-x_be-1", "be-2": "ts_ns_GW-x_be-2"}
	if err := ApplyStream(map[string]string{Annotation: e.Encode()}, cfg, func(n string) string { return names[n] }); err != nil {
		t.Fatal(err)
	}
	if want := "split_clients x $v { 50% ts_ns_GW-x_be-1; * ts_ns_GW-x_be-2; }"; len(cfg.StreamSnippets) != 1 || cfg.StreamSnippets[0] != want {
		t.Errorf("stream %v", cfg.StreamSnippets)
	}
	if !cmp.Equal(cfg.Server.ServerSnippets, []string{"ssl_preread on;"}) || cfg.Server.ProxyPass != "$v" || !cfg.Server.DefaultServer {
		t.Errorf("server %+v", cfg.Server)
	}
}
