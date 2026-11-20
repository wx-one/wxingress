package collectors

import (
	"bufio"
	"bytes"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The bins the wxingress module counts the upstream connect times of
// ingress_upstream_latency_seconds in (dataplane/src/metrics.c): one a
// millisecond below latencyExact, then 16 to an octave for latencyOctaves
// octaves, then one for the rest.
const (
	latencyExact   = 64
	latencyOctaves = 12
	latencyBins    = latencyExact + latencyOctaves*16 + 1
)

// The window of ingress-nginx's summary: client_golang's DefMaxAge of ten
// minutes in DefAgeBuckets of five, and its objectives (defObjectives of
// its socket collector).
const (
	latencyMaxAge      = 10 * time.Minute
	latencyAgeBuckets  = 5
	latencyBucketWidth = latencyMaxAge / latencyAgeBuckets
)

var latencyQuantiles = []float64{0.5, 0.9, 0.99}

// latencyBinValue is the time of a bin in seconds: its millisecond, or the middle of a wider bin.
func latencyBinValue(bin int) float64 {
	if bin < latencyExact {
		return float64(bin) / 1000
	}
	if bin >= latencyBins-1 {
		return math.Inf(1)
	}
	octave, sub := (bin-latencyExact)/16, (bin-latencyExact)%16
	shift := octave + 2 // top bit - 4, top = octave + 6
	lower := float64(uint64(16+sub) << shift)
	width := float64(uint64(1) << shift)
	return (lower + (width-1)/2) / 1000
}

// latencyBinOf is the module's bin of a time in milliseconds (latencyBin in metrics.c), for the tests.
func latencyBinOf(ms uint64) int {
	if ms < latencyExact {
		return int(ms)
	}
	top := 63
	for ms>>uint(top) == 0 {
		top--
	}
	octave := top - 6
	if octave >= latencyOctaves {
		return latencyBins - 1
	}
	return latencyExact + octave*16 + int((ms>>uint(top-4))&15)
}

// latencyReading is what the module counted for a series up to a moment.
type latencyReading struct {
	count uint64
	sum   uint64 // microseconds
	bins  [latencyBins]uint32
}

// latencySnapshot is a reading of every series at a time, a base of the window.
type latencySnapshot struct {
	at     time.Time
	series map[string]*latencyReading
}

// upstreamLatency computes ingress-nginx's ingress_upstream_latency_seconds
// from the module's cumulative bins: the count and the sum since the start,
// the quantiles of what came in the last 8 to 10 minutes - the difference
// of the reading now and one taken some ten minutes before - as the age
// buckets of client_golang's summary make them. A reading is kept every two
// minutes of the scrapes, five of them.
type upstreamLatency struct {
	mu        sync.Mutex
	snapshots []latencySnapshot
	now       func() time.Time
}

// parseLatency reads the module's lines: namespace, Ingress, Service and
// canary, the count, the sum and the bins that counted as <bin>:<count>.
func parseLatency(text []byte) map[string]*latencyReading {
	readings := make(map[string]*latencyReading)
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 7 {
			continue
		}
		r := &latencyReading{}
		var err error
		if r.count, err = strconv.ParseUint(fields[4], 10, 64); err != nil {
			continue
		}
		if r.sum, err = strconv.ParseUint(fields[5], 10, 64); err != nil {
			continue
		}
		for _, pair := range strings.Split(fields[6], ",") {
			b, n, ok := strings.Cut(pair, ":")
			bin, err1 := strconv.Atoi(b)
			count, err2 := strconv.ParseUint(n, 10, 32)
			if ok && err1 == nil && err2 == nil && bin >= 0 && bin < latencyBins {
				r.bins[bin] = uint32(count)
			}
		}
		readings[strings.Join(fields[:4], "\t")] = r
	}
	return readings
}

// observe takes the reading of now and returns, per series, what came since the base of the window.
func (u *upstreamLatency) observe(readings map[string]*latencyReading) map[string]*[latencyBins]uint32 {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	if u.now != nil {
		now = u.now()
	}
	// the base: the oldest reading not older than the window, else the newest one
	var base *latencySnapshot
	for i := range u.snapshots {
		if now.Sub(u.snapshots[i].at) <= latencyMaxAge {
			base = &u.snapshots[i]
			break
		}
	}
	if base == nil && len(u.snapshots) > 0 {
		base = &u.snapshots[len(u.snapshots)-1]
	}
	window := make(map[string]*[latencyBins]uint32, len(readings))
	for key, r := range readings {
		w := r.bins
		if base != nil {
			if old, ok := base.series[key]; ok {
				for b := range w {
					w[b] -= old.bins[b] // the bins wrap at 32 bits, their difference does not
				}
			}
		}
		window[key] = &w
	}
	if len(u.snapshots) == 0 || now.Sub(u.snapshots[len(u.snapshots)-1].at) >= latencyBucketWidth {
		u.snapshots = append(u.snapshots, latencySnapshot{at: now, series: readings})
		if len(u.snapshots) > latencyAgeBuckets+1 {
			u.snapshots = u.snapshots[len(u.snapshots)-latencyAgeBuckets-1:]
		}
	}
	return window
}

// latencyQuantile is the value of the q-quantile of the bins: the bin of the ceil(q*n)-th time, NaN without one.
func latencyQuantile(bins *[latencyBins]uint32, q float64) float64 {
	var n uint64
	for _, c := range bins {
		n += uint64(c)
	}
	if n == 0 {
		return math.NaN()
	}
	rank := uint64(math.Ceil(q * float64(n)))
	if rank == 0 {
		rank = 1
	}
	var seen uint64
	for b, c := range bins {
		seen += uint64(c)
		if seen >= rank {
			return latencyBinValue(b)
		}
	}
	return latencyBinValue(latencyBins - 1)
}

// collectLatency exports ingress_upstream_latency_seconds from the module's lines.
func (c *IngressNginxCollector) collectLatency(ch chan<- prometheus.Metric, text []byte) {
	readings := parseLatency(text)
	window := c.latency.observe(readings)
	for key, r := range readings {
		labels := strings.Split(key, "\t") // namespace, Ingress, Service, canary
		quantiles := make(map[float64]float64, len(latencyQuantiles))
		for _, q := range latencyQuantiles {
			quantiles[q] = latencyQuantile(window[key], q)
		}
		ch <- prometheus.MustNewConstSummary(c.upstreamLatency, r.count, float64(r.sum)/1e6, quantiles,
			labels[1], labels[0], labels[2], labels[3])
	}
}
