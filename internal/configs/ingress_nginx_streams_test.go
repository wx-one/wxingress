package configs

import (
	"slices"
	"strings"
	"testing"
)

func TestParseIngressNginxStreamServices(t *testing.T) {
	t.Parallel()
	got, warnings := ParseIngressNginxStreamServices(map[string]string{
		"22":    "git/gitlab-shell:22",
		"2222":  "git/gitlab-shell:ssh:PROXY",
		"2223":  "git/gitlab-shell:22:PROXY:PROXY",
		"2224":  "git/gitlab-shell:22::proxy",
		"5432":  "db/postgres",
		"x":     "db/postgres:5432",
		"6000":  "postgres:5432",
		"7000":  "db/postgres:5432:HAPROXY",
		"99999": "db/postgres:5432",
	}, false)
	want := []IngressNginxStreamService{
		{Port: 22, Namespace: "git", Service: "gitlab-shell", ServicePort: "22"},
		{Port: 2222, Namespace: "git", Service: "gitlab-shell", ServicePort: "ssh", Decode: true},
		{Port: 2223, Namespace: "git", Service: "gitlab-shell", ServicePort: "22", Decode: true, Encode: true},
		{Port: 2224, Namespace: "git", Service: "gitlab-shell", ServicePort: "22", Encode: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if len(warnings) != 5 {
		t.Errorf("warnings %q", warnings)
	}
	if got[0].Name() != "ingress-nginx-tcp-22" {
		t.Errorf("name %s", got[0].Name())
	}

	got, warnings = ParseIngressNginxStreamServices(map[string]string{"53": "kube-system/kube-dns:53:PROXY"}, true)
	if len(got) != 1 || !got[0].UDP || got[0].Decode || got[0].Name() != "ingress-nginx-udp-53" || len(warnings) != 1 || !strings.Contains(warnings[0], "TCP only") {
		t.Errorf("udp: %+v %q", got, warnings)
	}
}
