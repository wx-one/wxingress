package collectors

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// a line of the module: the labels, the requests, and per histogram its count, sum and buckets with +Inf
func moduleLine() string {
	f := []string{"200", "shop", "web", "web-svc", "/api", "shop.example", "GET", "", "3"}
	add := func(count, sum int, buckets ...int) {
		f = append(f, strconv.Itoa(count), strconv.Itoa(sum))
		for _, b := range buckets {
			f = append(f, strconv.Itoa(b))
		}
	}
	add(3, 150000, 0, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0) // request_duration: 0.15 s in all
	for i := 0; i < 3; i++ {
		add(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	}
	add(3, 600, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3) // request_size: all above 100
	add(3, 900, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3)
	add(3, 900, 0, 0, 3, 0, 0, 0, 0, 0)
	return strings.Join(f, "\t") + "\n"
}

func TestIngressNginxCollector(t *testing.T) {
	t.Parallel()
	c := NewIngressNginxCollector("pod-1", "ingress", "k8s.io/ingress-nginx", IngressNginxSources{
		Requests: func() ([]byte, error) { return []byte(moduleLine() + "broken\tline\n"), nil },
		Stub: func() (*StubStatus, error) {
			return &StubStatus{Active: 2, Accepted: 10, Handled: 10, Requests: 30, Reading: 0, Writing: 1, Waiting: 1}, nil
		},
		Certificates: func() []CertificateInfo {
			return []CertificateInfo{{Host: "shop.example", Namespace: "shop", SecretName: "tls", Identifier: "1-ab", SerialNumber: "1", NotAfter: time.Unix(2000000000, 0)}}
		},
	})
	c.Reloaded(true)
	c.Reloaded(false)
	c.Reloaded(true)
	registry := prometheus.NewPedanticRegistry()
	if err := c.Register(registry); err != nil {
		t.Fatal(err)
	}
	want := `
# HELP nginx_ingress_controller_requests The total number of client requests
# TYPE nginx_ingress_controller_requests counter
nginx_ingress_controller_requests{canary="",controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",host="shop.example",ingress="web",method="GET",namespace="shop",path="/api",service="web-svc",status="200"} 3
# HELP nginx_ingress_controller_success Cumulative number of Ingress controller reload operations
# TYPE nginx_ingress_controller_success counter
nginx_ingress_controller_success{controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1"} 2
# HELP nginx_ingress_controller_errors Cumulative number of Ingress controller errors during reload operations
# TYPE nginx_ingress_controller_errors counter
nginx_ingress_controller_errors{controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1"} 1
# HELP nginx_ingress_controller_config_last_reload_successful Whether the last configuration reload attempt was successful
# TYPE nginx_ingress_controller_config_last_reload_successful gauge
nginx_ingress_controller_config_last_reload_successful{controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1"} 1
# HELP nginx_ingress_controller_ssl_expire_time_seconds Number of seconds since 1970 to the SSL Certificate expire.
# TYPE nginx_ingress_controller_ssl_expire_time_seconds gauge
nginx_ingress_controller_ssl_expire_time_seconds{class="k8s.io/ingress-nginx",host="shop.example",identifier="1-ab",namespace="shop",secret_name="tls"} 2e+09
# HELP nginx_ingress_controller_nginx_process_connections current number of client connections with state {active, reading, writing, waiting}
# TYPE nginx_ingress_controller_nginx_process_connections gauge
nginx_ingress_controller_nginx_process_connections{controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",state="active"} 2
nginx_ingress_controller_nginx_process_connections{controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",state="reading"} 0
nginx_ingress_controller_nginx_process_connections{controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",state="waiting"} 1
nginx_ingress_controller_nginx_process_connections{controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",state="writing"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want),
		"nginx_ingress_controller_requests", "nginx_ingress_controller_success", "nginx_ingress_controller_errors",
		"nginx_ingress_controller_config_last_reload_successful", "nginx_ingress_controller_ssl_expire_time_seconds",
		"nginx_ingress_controller_nginx_process_connections"); err != nil {
		t.Error(err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "nginx_ingress_controller_request_duration_seconds" {
			continue
		}
		h := f.GetMetric()[0].GetHistogram()
		if h.GetSampleCount() != 3 || h.GetSampleSum() != 0.15 {
			t.Errorf("request_duration_seconds count %d sum %v", h.GetSampleCount(), h.GetSampleSum())
		}
		// cumulative: 0 at 0.005, 1 at 0.01, 2 at 0.025, 3 from 0.05 on
		if b := h.GetBucket(); b[0].GetCumulativeCount() != 0 || b[1].GetCumulativeCount() != 1 || b[3].GetCumulativeCount() != 3 || b[10].GetCumulativeCount() != 3 {
			t.Errorf("buckets %v", b)
		}
		return
	}
	t.Error("no request_duration_seconds")
}

func TestIngressNginxCollectorChecks(t *testing.T) {
	t.Parallel()
	c := NewIngressNginxCollector("pod-1", "ingress", "k8s.io/ingress-nginx", IngressNginxSources{})
	c.CheckDone("shop", "web", true)
	c.CheckDone("shop", "web", true)
	c.CheckDone("shop", "bad", false)
	registry := prometheus.NewPedanticRegistry()
	if err := c.Register(registry); err != nil {
		t.Fatal(err)
	}
	want := `
# HELP nginx_ingress_controller_check_errors Cumulative number of Ingress controller errors during syntax check operations
# TYPE nginx_ingress_controller_check_errors counter
nginx_ingress_controller_check_errors{controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",ingress="bad",namespace="shop"} 1
# HELP nginx_ingress_controller_check_success Cumulative number of Ingress controller syntax check operations
# TYPE nginx_ingress_controller_check_success counter
nginx_ingress_controller_check_success{controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",ingress="web",namespace="shop"} 2
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), "nginx_ingress_controller_check_success", "nginx_ingress_controller_check_errors"); err != nil {
		t.Fatal(err)
	}
}
