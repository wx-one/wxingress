package collectors

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ingressNginxNamespace is the namespace of ingress-nginx's metrics.
const ingressNginxNamespace = "nginx_ingress_controller"

// The buckets of ingress-nginx's request histograms, as the wxingress module
// counts them (dataplane/src/metrics.c).
var (
	ingressNginxTimeBuckets   = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	ingressNginxLengthBuckets = []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	ingressNginxSizeBuckets   = []float64{10, 100, 1000, 10000, 100000, 1000000, 10000000}
)

// ingressNginxHistograms are the histograms of a series in the order the
// module lists them, with their buckets and the unit of their sums.
var ingressNginxHistograms = []struct {
	name, help string
	buckets    []float64
	unit       float64
}{
	{"request_duration_seconds", "The request processing time in milliseconds", ingressNginxTimeBuckets, 1e6},
	{"response_duration_seconds", "The time spent on receiving the response from the upstream server", ingressNginxTimeBuckets, 1e6},
	{"connect_duration_seconds", "The time spent on establishing a connection with the upstream server", ingressNginxTimeBuckets, 1e6},
	{"header_duration_seconds", "The time spent on receiving first header from the upstream server", ingressNginxTimeBuckets, 1e6},
	{"request_size", "The request length (including request line, header, and request body)", ingressNginxLengthBuckets, 1},
	{"response_size", "The response length (including request line, header, and request body)", ingressNginxLengthBuckets, 1},
	{"bytes_sent", "DEPRECATED The number of bytes sent to a client", ingressNginxSizeBuckets, 1},
}

// ingressNginxRequestLabels are the labels of ingress-nginx's request metrics, in the module's order.
var ingressNginxRequestLabels = []string{"status", "namespace", "ingress", "service", "path", "host", "method", "canary"}

// StubStatus is what nginx's stub_status says.
type StubStatus struct {
	Active, Accepted, Handled, Reading, Writing, Waiting, Requests int64
}

// CertificateInfo is a certificate of a host, for ssl_certificate_info and ssl_expire_time_seconds.
type CertificateInfo struct {
	Host, Namespace, SecretName, Identifier            string
	IssuerOrganization, IssuerCommonName, SerialNumber string
	PublicKeyAlgorithm                                 string
	NotAfter                                           time.Time
}

// IngressNginxSources is where IngressNginxCollector gets what it exports.
type IngressNginxSources struct {
	// Requests is the module's list of request series (GET /api/wxingress/1/metrics).
	Requests func() ([]byte, error)
	// Latency is the module's list of the series of ingress_upstream_latency_seconds (GET /api/wxingress/1/metrics/latency).
	Latency func() ([]byte, error)
	// Stub is nginx's stub_status.
	Stub func() (*StubStatus, error)
	// Certificates are those of the hosts served.
	Certificates func() []CertificateInfo
	// Procs is where the nginx processes are found ("/proc").
	Procs string
}

// IngressNginxCollector exports ingress-nginx's metrics - their names,
// labels and buckets - so that its dashboards (Grafana 9614) and alerts work
// unchanged: the request metrics the wxingress module counts, the reloads,
// the certificates and the nginx processes. Its constant labels are
// ingress-nginx's: controller_namespace, controller_class, controller_pod.
type IngressNginxCollector struct {
	sources     IngressNginxSources
	constLabels prometheus.Labels
	class       string
	namespace   string

	requests   *prometheus.Desc
	histograms []*prometheus.Desc

	// upstreamLatency is ingress-nginx's summary ingress_upstream_latency_seconds, which it had up to 1.11.
	upstreamLatency *prometheus.Desc
	latency         upstreamLatency

	configHash, configSuccess, configSuccessTime, success, errors *prometheus.Desc
	sslExpire, sslInfo                                            *prometheus.Desc
	procConnections, procConnectionsTotal, procRequests           *prometheus.Desc
	procCPU, procRSS, procVSZ, procNum, procOldest, procRead      *prometheus.Desc
	procWrite                                                     *prometheus.Desc
	checkSuccess, checkErrors                                     *prometheus.Desc

	// checks counts the admission webhook's checks per namespace and Ingress
	checks map[[2]string]*ingressChecks
	// ingressExists tells if an Ingress exists, so that the counts of those that do not are dropped (pruneChecks)
	ingressExists func(namespace, name string) bool

	mu          sync.Mutex
	reloads     uint64
	reloadFails uint64
	lastOK      bool
	lastOKAt    time.Time
}

// NewIngressNginxCollector makes the collector for the controller pod, its namespace and its class.
func NewIngressNginxCollector(pod, namespace, class string, sources IngressNginxSources) *IngressNginxCollector {
	cl := prometheus.Labels{"controller_namespace": namespace, "controller_class": class, "controller_pod": pod}
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(ingressNginxNamespace+"_"+name, help, labels, cl)
	}
	c := &IngressNginxCollector{
		sources: sources, constLabels: cl, class: class, namespace: namespace,
		requests:          desc("requests", "The total number of client requests", ingressNginxRequestLabels...),
		upstreamLatency:   desc("ingress_upstream_latency_seconds", "DEPRECATED Upstream service latency per Ingress", "ingress", "namespace", "service", "canary"),
		configHash:        desc("config_hash", "Running configuration hash actually running"),
		configSuccess:     desc("config_last_reload_successful", "Whether the last configuration reload attempt was successful"),
		configSuccessTime: desc("config_last_reload_successful_timestamp_seconds", "Timestamp of the last successful configuration reload."),
		success:           desc("success", "Cumulative number of Ingress controller reload operations"),
		errors:            desc("errors", "Cumulative number of Ingress controller errors during reload operations"),
		// as ingress-nginx: the certificate metrics have the labels namespace and class, not the constant ones
		sslExpire: prometheus.NewDesc(ingressNginxNamespace+"_ssl_expire_time_seconds",
			"Number of seconds since 1970 to the SSL Certificate expire.",
			[]string{"namespace", "class", "host", "secret_name", "identifier"}, nil),
		sslInfo: prometheus.NewDesc(ingressNginxNamespace+"_ssl_certificate_info", "Hold all labels associated to a certificate",
			[]string{"namespace", "class", "host", "secret_name", "identifier", "issuer_organization", "issuer_common_name", "serial_number", "public_key_algorithm"}, nil),
		procConnections:      desc("nginx_process_connections", "current number of client connections with state {active, reading, writing, waiting}", "state"),
		procConnectionsTotal: desc("nginx_process_connections_total", "total number of connections with state {accepted, handled}", "state"),
		procRequests:         desc("nginx_process_requests_total", "total number of client requests"),
		procCPU:              desc("nginx_process_cpu_seconds_total", "Cpu usage in seconds"),
		procRSS:              desc("nginx_process_resident_memory_bytes", "number of bytes of memory in use"),
		procVSZ:              desc("nginx_process_virtual_memory_bytes", "number of bytes of memory in use"),
		procNum:              desc("nginx_process_num_procs", "number of processes"),
		procOldest:           desc("nginx_process_oldest_start_time_seconds", "start time in seconds since 1970/01/01"),
		procRead:             desc("nginx_process_read_bytes_total", "number of bytes read"),
		procWrite:            desc("nginx_process_write_bytes_total", "number of bytes written"),
		checkSuccess:         desc("check_success", "Cumulative number of Ingress controller syntax check operations", "namespace", "ingress"),
		checkErrors:          desc("check_errors", "Cumulative number of Ingress controller errors during syntax check operations", "namespace", "ingress"),
		checks:               make(map[[2]string]*ingressChecks),
	}
	for _, h := range ingressNginxHistograms {
		c.histograms = append(c.histograms, desc(h.name, h.help, ingressNginxRequestLabels...))
	}
	return c
}

// SetCertificates sets where the certificates come from, once the configurator is there.
func (c *IngressNginxCollector) SetCertificates(certificates func() []CertificateInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sources.Certificates = certificates
}

// Reloaded records the outcome of a reload, as ingress-nginx's ConfigSuccess does.
func (c *IngressNginxCollector) Reloaded(ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastOK = ok
	if ok {
		c.reloads++
		c.lastOKAt = time.Now()
	} else {
		c.reloadFails++
	}
}

// ingressChecks are the counts of the checks of an Ingress, and when it was last checked.
type ingressChecks struct {
	passed, failed uint64
	last           time.Time
}

// checksRetention is how long the counts of the checks of an Ingress that does not exist are kept after its last
// check: those of an Ingress refused, never created, or deleted.
const checksRetention = 10 * time.Minute

// maxIngressChecks is the most Ingresses with counts of checks kept: beyond, the counts of those that do not exist
// go at once, whenever they were checked, so that names checked by the million do not fill the memory.
const maxIngressChecks = 10000

// SetIngressExists sets how the collector learns if an Ingress exists: the counts of the checks of one that does
// not are dropped checksRetention after its last check. Without it, every count is kept.
func (c *IngressNginxCollector) SetIngressExists(exists func(namespace, name string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ingressExists = exists
}

// CheckDone counts a check of the admission webhook, as ingress-nginx's check_success and check_errors.
func (c *IngressNginxCollector) CheckDone(namespace, name string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	key := [2]string{namespace, name}
	n := c.checks[key]
	if n == nil {
		c.pruneChecks(now)
		n = &ingressChecks{}
		c.checks[key] = n
	}
	n.last = now
	if ok {
		n.passed++
	} else {
		n.failed++
	}
}

// pruneChecks drops the counts of the Ingresses that do not exist and were last checked checksRetention ago, or,
// with maxIngressChecks Ingresses counted, whenever they were last checked. c.mu is held.
func (c *IngressNginxCollector) pruneChecks(now time.Time) {
	if c.ingressExists == nil {
		return
	}
	full := len(c.checks) >= maxIngressChecks
	for key, n := range c.checks {
		if (full || now.Sub(n.last) > checksRetention) && !c.ingressExists(key[0], key[1]) {
			delete(c.checks, key)
		}
	}
}

// Register registers the collector.
func (c *IngressNginxCollector) Register(registry *prometheus.Registry) error {
	return registry.Register(c)
}

// Describe sends nothing: the collector is unchecked, as its series come and go with the requests.
func (c *IngressNginxCollector) Describe(chan<- *prometheus.Desc) {}

// Collect implements prometheus.Collector.
func (c *IngressNginxCollector) Collect(ch chan<- prometheus.Metric) {
	c.collectReloads(ch)
	if c.sources.Requests != nil {
		if text, err := c.sources.Requests(); err == nil {
			c.collectRequests(ch, text)
		}
	}
	if c.sources.Latency != nil {
		if text, err := c.sources.Latency(); err == nil {
			c.collectLatency(ch, text)
		}
	}
	c.mu.Lock()
	certificates := c.sources.Certificates
	c.mu.Unlock()
	if certificates != nil {
		for _, cert := range certificates() {
			ch <- prometheus.MustNewConstMetric(c.sslExpire, prometheus.GaugeValue, float64(cert.NotAfter.Unix()),
				cert.Namespace, c.class, cert.Host, cert.SecretName, cert.Identifier)
			ch <- prometheus.MustNewConstMetric(c.sslInfo, prometheus.GaugeValue, 1,
				cert.Namespace, c.class, cert.Host, cert.SecretName, cert.Identifier, cert.IssuerOrganization,
				cert.IssuerCommonName, cert.SerialNumber, cert.PublicKeyAlgorithm)
		}
	}
	if c.sources.Stub != nil {
		if s, err := c.sources.Stub(); err == nil {
			for state, v := range map[string]int64{"active": s.Active, "reading": s.Reading, "writing": s.Writing, "waiting": s.Waiting} {
				ch <- prometheus.MustNewConstMetric(c.procConnections, prometheus.GaugeValue, float64(v), state)
			}
			ch <- prometheus.MustNewConstMetric(c.procConnectionsTotal, prometheus.CounterValue, float64(s.Accepted), "accepted")
			ch <- prometheus.MustNewConstMetric(c.procConnectionsTotal, prometheus.CounterValue, float64(s.Handled), "handled")
			ch <- prometheus.MustNewConstMetric(c.procRequests, prometheus.CounterValue, float64(s.Requests))
		}
	}
	c.collectProcesses(ch)
}

func (c *IngressNginxCollector) collectReloads(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	reloads, fails, ok, at := c.reloads, c.reloadFails, c.lastOK, c.lastOKAt
	c.mu.Unlock()
	success, hash := 0.0, 0.0
	if ok {
		success = 1
		// ingress-nginx's is a hash of the configuration; this one changes with every configuration loaded
		hash = float64(reloads)
	}
	ch <- prometheus.MustNewConstMetric(c.configHash, prometheus.GaugeValue, hash)
	ch <- prometheus.MustNewConstMetric(c.configSuccess, prometheus.GaugeValue, success)
	timestamp := 0.0
	if !at.IsZero() {
		timestamp = float64(at.Unix())
	}
	ch <- prometheus.MustNewConstMetric(c.configSuccessTime, prometheus.GaugeValue, timestamp)
	ch <- prometheus.MustNewConstMetric(c.success, prometheus.CounterValue, float64(reloads))
	ch <- prometheus.MustNewConstMetric(c.errors, prometheus.CounterValue, float64(fails))

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneChecks(time.Now())
	for key, n := range c.checks {
		// as ingress-nginx's counter vectors: a series once there is a count of it
		if n.passed > 0 {
			ch <- prometheus.MustNewConstMetric(c.checkSuccess, prometheus.CounterValue, float64(n.passed), key[0], key[1])
		}
		if n.failed > 0 {
			ch <- prometheus.MustNewConstMetric(c.checkErrors, prometheus.CounterValue, float64(n.failed), key[0], key[1])
		}
	}
}

// collectRequests turns the module's lines - the eight labels, the requests,
// and per histogram its count, sum and the counts of its buckets with +Inf -
// into ingress-nginx's metrics.
func (c *IngressNginxCollector) collectRequests(ch chan<- prometheus.Metric, text []byte) {
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		labels := len(ingressNginxRequestLabels)
		want := labels + 1
		for _, h := range ingressNginxHistograms {
			want += 2 + len(h.buckets) + 1
		}
		if len(fields) != want {
			continue
		}
		values := fields[:labels]
		at := labels
		number := func() uint64 {
			v, _ := strconv.ParseUint(fields[at], 10, 64)
			at++
			return v
		}
		ch <- prometheus.MustNewConstMetric(c.requests, prometheus.CounterValue, float64(number()), values...)
		for i, h := range ingressNginxHistograms {
			count := number()
			sum := float64(number()) / h.unit
			buckets := make(map[float64]uint64, len(h.buckets))
			var cumulative uint64
			for _, upper := range h.buckets {
				cumulative += number()
				buckets[upper] = cumulative
			}
			number() // +Inf: the count
			ch <- prometheus.MustNewConstHistogram(c.histograms[i], count, sum, buckets, values...)
		}
	}
}

// collectProcesses sums the nginx processes - master and workers, old ones too - as ingress-nginx's process collector does.
func (c *IngressNginxCollector) collectProcesses(ch chan<- prometheus.Metric) {
	procs := c.sources.Procs
	if procs == "" {
		return
	}
	entries, err := os.ReadDir(procs)
	if err != nil {
		return
	}
	ticks := 100.0 // USER_HZ
	pageSize := float64(os.Getpagesize())
	boot := bootTime(procs)
	var num, cpu, rss, vsz, read, write float64
	oldest := 0.0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		dir := filepath.Join(procs, e.Name())
		cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || !bytes.HasPrefix(cmdline, []byte("nginx:")) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		// after the command in parentheses: state is field 3, utime 14, stime 15, starttime 22, vsize 23, rss 24
		s := string(stat)
		end := strings.LastIndexByte(s, ')')
		if end < 0 {
			continue
		}
		f := strings.Fields(s[end+1:])
		if len(f) < 22 {
			continue
		}
		num++
		utime, _ := strconv.ParseFloat(f[11], 64)
		stime, _ := strconv.ParseFloat(f[12], 64)
		start, _ := strconv.ParseFloat(f[19], 64)
		vsize, _ := strconv.ParseFloat(f[20], 64)
		pages, _ := strconv.ParseFloat(f[21], 64)
		cpu += (utime + stime) / ticks
		vsz += vsize
		rss += pages * pageSize
		if started := boot + start/ticks; oldest == 0 || started < oldest {
			oldest = started
		}
		if io, err := os.ReadFile(filepath.Join(dir, "io")); err == nil {
			for _, line := range strings.Split(string(io), "\n") {
				k, v, _ := strings.Cut(line, ": ")
				n, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
				switch k {
				case "rchar":
					read += n
				case "wchar":
					write += n
				}
			}
		}
	}
	if num == 0 {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.procNum, prometheus.GaugeValue, num)
	ch <- prometheus.MustNewConstMetric(c.procCPU, prometheus.CounterValue, cpu)
	ch <- prometheus.MustNewConstMetric(c.procRSS, prometheus.GaugeValue, rss)
	ch <- prometheus.MustNewConstMetric(c.procVSZ, prometheus.GaugeValue, vsz)
	ch <- prometheus.MustNewConstMetric(c.procOldest, prometheus.GaugeValue, oldest)
	ch <- prometheus.MustNewConstMetric(c.procRead, prometheus.CounterValue, read)
	ch <- prometheus.MustNewConstMetric(c.procWrite, prometheus.CounterValue, write)
}

func bootTime(procs string) float64 {
	stat, err := os.ReadFile(filepath.Join(procs, "stat"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(stat), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			n, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return n
		}
	}
	return 0
}

// ingressNginxManager is a ManagerCollector that also tells the reloads to an IngressNginxCollector.
type ingressNginxManager struct {
	ManagerCollector
	inx *IngressNginxCollector
}

// WithIngressNginxReloads returns mc, telling inx about every reload as well.
func WithIngressNginxReloads(mc ManagerCollector, inx *IngressNginxCollector) ManagerCollector {
	return &ingressNginxManager{ManagerCollector: mc, inx: inx}
}

// IncNginxReloadCount implements ManagerCollector.
func (m *ingressNginxManager) IncNginxReloadCount(isEndPointUpdate bool) {
	m.ManagerCollector.IncNginxReloadCount(isEndPointUpdate)
	m.inx.Reloaded(true)
}

// IncNginxReloadErrors implements ManagerCollector.
func (m *ingressNginxManager) IncNginxReloadErrors() {
	m.ManagerCollector.IncNginxReloadErrors()
	m.inx.Reloaded(false)
}
