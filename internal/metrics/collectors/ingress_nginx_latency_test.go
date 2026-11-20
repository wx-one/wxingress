package collectors

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestLatencyBins(t *testing.T) {
	t.Parallel()
	for ms := uint64(0); ms < 200000; ms += 1 + ms/50 {
		bin := latencyBinOf(ms)
		v := latencyBinValue(bin) * 1000
		switch {
		case ms < latencyExact && v != float64(ms):
			t.Errorf("%d ms: bin %d is %v ms", ms, bin, v)
		case bin == latencyBins-1:
			if ms < 1<<17 {
				t.Errorf("%d ms in the last bin", ms)
			}
		case math.Abs(v-float64(ms)) > float64(ms)/16:
			t.Errorf("%d ms: bin %d is %v ms, more than a sixteenth off", ms, bin, v)
		}
		if ms > 0 && latencyBinOf(ms-1) > bin {
			t.Errorf("%d ms: bin %d below that of %d ms", ms, bin, ms-1)
		}
	}
}

// latencyLine is a module line of a series: its labels, the count, the sum and the bins of the times in milliseconds.
func latencyLine(labels string, times ...uint64) string {
	bins := map[int]int{}
	var sum uint64
	for _, ms := range times {
		bins[latencyBinOf(ms)]++
		sum += ms * 1000
	}
	var pairs []string
	for b := 0; b < latencyBins; b++ {
		if bins[b] > 0 {
			pairs = append(pairs, fmt.Sprintf("%d:%d", b, bins[b]))
		}
	}
	return fmt.Sprintf("%s\t%d\t%d\t%s\n", labels, len(times), sum, strings.Join(pairs, ","))
}

func TestIngressNginxUpstreamLatency(t *testing.T) {
	t.Parallel()
	var times []uint64
	for i := uint64(1); i <= 100; i++ {
		times = append(times, i) // 1..100 ms
	}
	line := latencyLine("shop\tweb\tweb-svc\t", times...)
	now := time.Unix(1700000000, 0)
	c := NewIngressNginxCollector("pod-1", "ingress", "k8s.io/ingress-nginx", IngressNginxSources{
		Latency: func() ([]byte, error) { return []byte(line + "broken\tline\n"), nil },
	})
	c.latency.now = func() time.Time { return now }
	registry := prometheus.NewPedanticRegistry()
	if err := c.Register(registry); err != nil {
		t.Fatal(err)
	}
	name := "nginx_ingress_controller_ingress_upstream_latency_seconds"
	// the 50th, 90th and 99th of 1..100 ms; 90 and 99 are in bins of 4 ms whose middles are 89.5 and 97.5
	want := `
# HELP nginx_ingress_controller_ingress_upstream_latency_seconds DEPRECATED Upstream service latency per Ingress
# TYPE nginx_ingress_controller_ingress_upstream_latency_seconds summary
nginx_ingress_controller_ingress_upstream_latency_seconds{canary="",controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",ingress="web",namespace="shop",service="web-svc",quantile="0.5"} 0.05
nginx_ingress_controller_ingress_upstream_latency_seconds{canary="",controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",ingress="web",namespace="shop",service="web-svc",quantile="0.9"} 0.0895
nginx_ingress_controller_ingress_upstream_latency_seconds{canary="",controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",ingress="web",namespace="shop",service="web-svc",quantile="0.99"} 0.0975
nginx_ingress_controller_ingress_upstream_latency_seconds_sum{canary="",controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",ingress="web",namespace="shop",service="web-svc"} 5.05
nginx_ingress_controller_ingress_upstream_latency_seconds_count{canary="",controller_class="k8s.io/ingress-nginx",controller_namespace="ingress",controller_pod="pod-1",ingress="web",namespace="shop",service="web-svc"} 100
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), name); err != nil {
		t.Fatal(err)
	}

	// ten minutes and more later, 100 more of 200 ms: the window has only those, the count and the sum all
	for _, step := range []time.Duration{2 * time.Minute, 4 * time.Minute, 6 * time.Minute, 8 * time.Minute, 10*time.Minute + time.Second} {
		now = time.Unix(1700000000, 0).Add(step)
		if _, err := registry.Gather(); err != nil {
			t.Fatal(err)
		}
	}
	more := append([]uint64{}, times...)
	for i := 0; i < 100; i++ {
		more = append(more, 200)
	}
	line = latencyLine("shop\tweb\tweb-svc\t", more...)
	now = now.Add(time.Minute)
	got, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range got {
		if mf.GetName() != name {
			continue
		}
		s := mf.GetMetric()[0].GetSummary()
		if s.GetSampleCount() != 200 {
			t.Errorf("count %d, want 200", s.GetSampleCount())
		}
		for _, q := range s.GetQuantile() {
			if v := q.GetValue(); math.Abs(v-latencyBinValue(latencyBinOf(200))) > 1e-9 {
				t.Errorf("quantile %v is %v, want the bin of 200 ms", q.GetQuantile(), v)
			}
		}
	}
}

func TestLatencyQuantileEmpty(t *testing.T) {
	t.Parallel()
	if v := latencyQuantile(&[latencyBins]uint32{}, 0.5); !math.IsNaN(v) {
		t.Errorf("an empty window has %v, want NaN as client_golang's summary", v)
	}
}

func TestLatencyWindowWraps(t *testing.T) {
	t.Parallel()
	u := upstreamLatency{}
	now := time.Unix(0, 0)
	u.now = func() time.Time { return now }
	old := &latencyReading{count: 1 << 33}
	old.bins[3] = math.MaxUint32 - 1
	u.observe(map[string]*latencyReading{"k": old})
	now = now.Add(3 * time.Minute)
	wrapped := &latencyReading{count: 1<<33 + 5}
	wrapped.bins[3] = 3 // 5 later
	w := u.observe(map[string]*latencyReading{"k": wrapped})
	if w["k"][3] != 5 {
		t.Errorf("the window has %d in the bin, want 5", w["k"][3])
	}
}
