/**
 * ------------------------------------------------------------------ metrics
 *
 * ingress-nginx's request metrics (its socket collector, fed by monitor.lua
 * from every request's log phase): a counter and seven histograms per
 * status, method, path, namespace, Ingress, Service, canary and host -
 *
 *   requests                     every request
 *   request_duration_seconds     $request_time
 *   response_duration_seconds    $upstream_response_time
 *   connect_duration_seconds     $upstream_connect_time
 *   header_duration_seconds      $upstream_header_time
 *   request_size                 $request_length
 *   response_size, bytes_sent    $bytes_sent
 *
 * with its buckets. The controller reads them here and exports them under
 * ingress-nginx's names, with its constant labels:
 *
 *   GET /api/wxingress/1/metrics           one line a series, tab-separated
 *   GET /api/wxingress/1/metrics/latency   the same for ingress_upstream_latency_seconds
 *
 * ingress_upstream_latency_seconds is the summary ingress-nginx exported up
 * to 1.11 (its socket collector's upstreamLatency, removed in 1.12): the
 * $upstream_connect_time of a request that tried one upstream, per
 * namespace, Ingress, Service and canary, with the quantiles 0.5, 0.9 and
 * 0.99 of the last ten minutes. Its quantiles are the controller's to
 * compute - over a window of time, which the log phase knows nothing of -
 * so the module counts each series' times in bins: $upstream_connect_time
 * has milliseconds, and a bin is one millisecond up to 63 ms, then 16 to an
 * octave up to 131 s (a sixteenth of the value at most), then one for the
 * rest. The controller takes the difference of two readings ten minutes
 * apart. A bin is 32 bits, which a difference over ten minutes does not
 * wrap; the count and the sum are 64.
 *
 * A location counts where the controller's config set `$wxingress_metrics`
 * - to its labels: "c" or "-" for a canary or none, then its primary
 * upstream, namespace, Ingress, Service and path, separated by tabs - with
 * http.logIf, so a request elsewhere costs a load of the variable's slot,
 * and the step reads what it needs where nginx keeps it: no request is
 * built, nothing is allocated.
 *
 * The series are in shared memory the master maps before it forks, so they
 * outlive reloads as counters must: a new worker adds to what the old ones
 * counted. Their counts are kept per shard - a worker takes one when it
 * starts - so that workers do not fight over the cache lines of a busy
 * series; the controller sums them. A shard is shared by an old worker that
 * is still finishing and the new one that took its number, so every count is
 * an atomic add, which costs nothing much on a line only one of them writes.
 *
 * The table is fixed: METRIC_SERIES series, found by a hash of their labels
 * with open addressing. A series once made stays (ingress-nginx's do as
 * well, until a restart). Where the table is full, a request is counted in
 * the series without labels.
 */

#include <stdint.h>

#define METRIC_SERIES 8192
#define METRIC_SHARDS 16
#define METRIC_PROBES 64
#define METRIC_LABELS 640

/**
 * The buckets of ingress-nginx - prometheus.DefBuckets in seconds,
 * LinearBuckets(10, 10, 10) and ExponentialBuckets(10, 10, 7) in bytes - as
 * integers: the times in microseconds.
 */
static const uint64_t timeBuckets[] = {5000, 10000, 25000, 50000, 100000, 250000, 500000, 1000000, 2500000, 5000000, 10000000};
static const uint64_t lengthBuckets[] = {10, 20, 30, 40, 50, 60, 70, 80, 90, 100};
static const uint64_t sizeBuckets[] = {10, 100, 1000, 10000, 100000, 1000000, 10000000};

#define TIME_BUCKETS 11
#define LENGTH_BUCKETS 10
#define SIZE_BUCKETS 7

/** Counts per bucket (not cumulative; the last is +Inf) and the sum - microseconds or bytes. The count is theirs. */
typedef struct {
  uint64_t bucket[TIME_BUCKETS + 1];
  uint64_t sum;
} histogram_t;

enum { H_DURATION, H_RESPONSE, H_CONNECT, H_HEADER, H_REQUEST_SIZE, H_RESPONSE_SIZE, H_BYTES_SENT, HISTOGRAMS };

/** The requests of a series are the count of its request_duration_seconds. */
typedef struct {
  histogram_t h[HISTOGRAMS];
} counts_t;

/**
 * A series: its labels, packed - status, then namespace, Ingress, Service,
 * path, host, method and canary, each ended by a 0 - and its hash, 0 for a
 * free slot. `ready` is set once the labels are written: a worker that finds
 * the hash of a slot another one is still filling waits for it.
 */
typedef struct {
  uint64_t hash;
  int ready;
  int length;
  char labels[METRIC_LABELS];
} seriesKey_t;

/** The series of ingress_upstream_latency_seconds, by namespace, Ingress, Service and canary. */
#define LATENCY_SERIES 2048
#define LATENCY_EXACT 64
#define LATENCY_OCTAVES 12
#define LATENCY_BINS (LATENCY_EXACT + LATENCY_OCTAVES * 16 + 1)

typedef struct {
  uint64_t count;
  /* microseconds */
  uint64_t sum;
  uint32_t bin[LATENCY_BINS];
} latency_t;

typedef struct {
  uint64_t claims;
  /** METRIC_SERIES series and the one without labels, at the end. */
  seriesKey_t keys[METRIC_SERIES + 1];
  counts_t counts[METRIC_SHARDS][METRIC_SERIES + 1];
  /** LATENCY_SERIES series and the one without labels, at the end. */
  seriesKey_t latencyKeys[LATENCY_SERIES + 1];
  latency_t latency[METRIC_SHARDS][LATENCY_SERIES + 1];
} metrics_t;

static metrics_t *metrics;

/** The shard of this worker. */
static int metricShard;

static int metricsOpen(void) {

  void *mapped;

  if (metrics != NULL)
    return 0;

  /* MAP_NORESERVE: a page is only taken once a series of a shard is counted */
  mapped = mmap(NULL, sizeof(metrics_t), PROT_READ | PROT_WRITE,
                MAP_SHARED | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);

  if (mapped == MAP_FAILED) {
    fprintf(stderr, "wxingress: no shared memory for metrics: %s\n", strerror(errno));
    return -1;
  }

  metrics = mapped;
  metrics->keys[METRIC_SERIES].ready = 1;
  metrics->latencyKeys[LATENCY_SERIES].ready = 1;
  return 0;
}

static int metricsWorker(void) {

  if (metrics != NULL)
    metricShard = (int)(__atomic_fetch_add(&metrics->claims, 1, __ATOMIC_RELAXED) % METRIC_SHARDS);
  return 0;
}

/** Appends a label, at most `most` bytes of it, without tabs or line ends, and its 0. */
static int metricLabel(char *into, int used, http_value_t v, int most) {

  int n = v.length < most ? v.length : most;

  for (int at = 0; at < n; ++at) {
    char c = v.at[at];
    into[used++] = c == '\t' || c == '\n' || c == '\r' || c == 0 ? ' ' : c;
  }

  into[used++] = 0;
  return used;
}

static uint64_t metricHash(const char *at, int length) {

  uint64_t h = 1469598103934665603ULL;

  for (int i = 0; i < length; ++i) {
    h ^= (unsigned char)at[i];
    h *= 1099511628211ULL;
  }

  return h == 0 ? 1 : h;
}

/** The slot of the series with these labels in a table of `size` (and the one without labels at `size`), made if it is not there yet. */
static int seriesIn(seriesKey_t *keys, int size, const char *labels, int length) {

  uint64_t hash = metricHash(labels, length);
  int start = (int)(hash % (uint64_t)size);

  for (int probe = 0; probe < METRIC_PROBES; ++probe) {

    int at = (start + probe) % size;
    seriesKey_t *key = &keys[at];
    uint64_t have = __atomic_load_n(&key->hash, __ATOMIC_ACQUIRE);

    if (have == 0) {

      uint64_t empty = 0;

      if (__atomic_compare_exchange_n(&key->hash, &empty, hash, 0, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE)) {
        memcpy(key->labels, labels, (size_t)length);
        key->length = length;
        __atomic_store_n(&key->ready, 1, __ATOMIC_RELEASE);
        return at;
      }

      have = empty;
    }

    if (have != hash)
      continue;

    /* made by another worker this moment: its labels are being written */
    for (int spin = 0; spin < 100000 && !__atomic_load_n(&key->ready, __ATOMIC_ACQUIRE); ++spin)
      ;

    if (key->length == length && memcmp(key->labels, labels, (size_t)length) == 0)
      return at;
  }

  return size;
}

static int metricSeries(const char *labels, int length) {
  return seriesIn(metrics->keys, METRIC_SERIES, labels, length);
}

/** The bin of a time in milliseconds: the millisecond below LATENCY_EXACT, then 16 to an octave. */
static inline int latencyBin(uint64_t ms) {

  int top, octave;

  if (ms < LATENCY_EXACT)
    return (int)ms;

  top = 63 - __builtin_clzll(ms);
  octave = top - 6;

  if (octave >= LATENCY_OCTAVES)
    return LATENCY_BINS - 1;

  return LATENCY_EXACT + octave * 16 + (int)((ms >> (top - 4)) & 15);
}

/** Three adds: the bin, the count and the sum. */
static inline void latencyObserve(latency_t *l, uint64_t us) {
  __atomic_fetch_add(&l->bin[latencyBin((us + 500) / 1000)], 1, __ATOMIC_RELAXED);
  __atomic_fetch_add(&l->count, 1, __ATOMIC_RELAXED);
  __atomic_fetch_add(&l->sum, us, __ATOMIC_RELAXED);
}

/** Two adds: the bucket of the value (the first whose bound it does not exceed, or +Inf) and the sum. */
static inline void metricObserve(histogram_t *h, uint64_t value, const uint64_t *buckets, int count) {

  int at = 0;

  while (at < count && value > buckets[at])
    ++at;

  __atomic_fetch_add(&h->bucket[at], 1, __ATOMIC_RELAXED);
  __atomic_fetch_add(&h->sum, value, __ATOMIC_RELAXED);
}

/** Seconds as nginx keeps them, to the millisecond, in microseconds. */
static inline uint64_t micros(double seconds) {
  return (uint64_t)(seconds * 1e6 + 0.5);
}

static int sameValue(http_value_t a, http_value_t b) {
  return a.length == b.length && memcmp(a.at, b.at, (size_t)a.length) == 0;
}

/**
 * Which series a location's requests of a status, a method and a host are,
 * per worker: the labels of a location are its `$wxingress_metrics`, one
 * constant string of its configuration, so where its value lies tells the
 * location. A request that finds its series here builds no labels and
 * hashes nothing. Not for a location with a canary, whose requests are of
 * two series.
 */
#define METRIC_CACHE 512

typedef struct {
  const char *labels;
  int status;
  int series;
  /* of ingress_upstream_latency_seconds, -1 until a request of the location needs it */
  int latency;
  int methodLength;
  int hostLength;
  char method[16];
  char host[64];
} metricCache_t;

static metricCache_t metricCache[METRIC_CACHE];

static unsigned metricCacheSlot(const char *labels, int status, http_value_t host) {

  uintptr_t h = (uintptr_t)labels;

  h ^= h >> 17;
  h += (uintptr_t)status * 2654435761u;
  h += (uintptr_t)host.length * 40503u;
  if (host.length > 0)
    h += (unsigned char)host.at[0] * 31u + (unsigned char)host.at[host.length - 1];

  return (unsigned)(h % METRIC_CACHE);
}

/** Appends the status as text and its 0. */
static int metricStatus(char *into, int status) {

  char digits[12];
  int n = 0, used = 0;

  if (status <= 0)
    digits[n++] = '0';

  for (; status > 0 && n < 11; status /= 10)
    digits[n++] = (char)('0' + status % 10);

  while (n > 0)
    into[used++] = digits[--n];

  into[used++] = 0;
  return used;
}

/** The log step of a location that counts: `$wxingress_metrics` is its labels (ingressNginxMetricsLabels). */
static void countRequest(http_done_t *done) {

  char labels[METRIC_LABELS];
  int used, series = -1, canaried, timed, latency;
  http_value_t canary = {"", 0};
  counts_t *c;
  /* the value of $wxingress_metrics, which opened the gate */
  http_value_t gate = done->gate;
  http_value_t parts[5];
  metricCache_t *cached = NULL;

  if (metrics == NULL || gate.length < 2)
    return;

  canaried = gate.at[0] == 'c';

  if (!canaried && done->host.length <= (int)sizeof cached->host && done->method.length <= (int)sizeof cached->method) {

    cached = &metricCache[metricCacheSlot(gate.at, done->status, done->host)];

    if (cached->labels == gate.at && cached->status == done->status &&
        cached->hostLength == done->host.length && cached->methodLength == done->method.length &&
        memcmp(cached->host, done->host.at, (size_t)done->host.length) == 0 &&
        memcmp(cached->method, done->method.at, (size_t)done->method.length) == 0)
      series = cached->series;
  }

  /* as ingress-nginx: the times of a request that tried one upstream, a list it cannot read */
  timed = done->upstreamTries == 1;
  latency = timed && done->upstreamConnectTime >= 0 ? (cached != NULL && series >= 0 ? cached->latency : -1) : -2;

  if (series < 0 || latency == -1) {

    /* upstream, namespace, Ingress, Service, path */
    const char *at = gate.at + 1, *end = gate.at + gate.length;

    for (int p = 0; p < 5; ++p) {
      const char *tab = p < 4 ? memchr(at, '\t', (size_t)(end - at)) : NULL;
      const char *stop = tab != NULL ? tab : end;
      parts[p].at = at;
      parts[p].length = (int)(stop - at);
      at = tab != NULL ? tab + 1 : end;
    }

    if (canaried) {
      /* ingress-nginx's canary label: the upstream a canary sent the request to instead */
      http_value_t proxied = done.var("proxy_host");
      if (proxied.length > 0 && !sameValue(proxied, parts[0]))
        canary = proxied;
    }
  }

  if (latency == -1) {
    used = metricLabel(labels, 0, parts[1], 63);
    used = metricLabel(labels, used, parts[2], 127);
    used = metricLabel(labels, used, parts[3], 127);
    used = metricLabel(labels, used, canary, 127);
    latency = seriesIn(metrics->latencyKeys, LATENCY_SERIES, labels, used);
    if (cached != NULL && series >= 0)
      cached->latency = latency;
  }

  if (series < 0) {

    used = metricStatus(labels, done->status);
    used = metricLabel(labels, used, parts[1], 63);
    used = metricLabel(labels, used, parts[2], 127);
    used = metricLabel(labels, used, parts[3], 127);
    used = metricLabel(labels, used, parts[4], 127);
    used = metricLabel(labels, used, done->host, 63);
    used = metricLabel(labels, used, done->method, 15);

    used = metricLabel(labels, used, canary, 127);

    series = metricSeries(labels, used);

    if (cached != NULL) {
      cached->labels = gate.at;
      cached->status = done->status;
      cached->series = series;
      cached->latency = latency >= 0 ? latency : -1;
      cached->hostLength = done->host.length;
      cached->methodLength = done->method.length;
      memcpy(cached->host, done->host.at, (size_t)done->host.length);
      memcpy(cached->method, done->method.at, (size_t)done->method.length);
    }
  }

  c = &metrics->counts[metricShard][series];

  metricObserve(&c->h[H_DURATION], micros(done->requestTime), timeBuckets, TIME_BUCKETS);
  metricObserve(&c->h[H_REQUEST_SIZE], (uint64_t)done->requestLength, lengthBuckets, LENGTH_BUCKETS);
  metricObserve(&c->h[H_RESPONSE_SIZE], (uint64_t)done->bytesSent, lengthBuckets, LENGTH_BUCKETS);
  metricObserve(&c->h[H_BYTES_SENT], (uint64_t)done->bytesSent, sizeBuckets, SIZE_BUCKETS);

  if (latency >= 0)
    latencyObserve(&metrics->latency[metricShard][latency], micros(done->upstreamConnectTime));

  if (timed) {
    if (done->upstreamResponseTime >= 0)
      metricObserve(&c->h[H_RESPONSE], micros(done->upstreamResponseTime), timeBuckets, TIME_BUCKETS);
    if (done->upstreamConnectTime >= 0)
      metricObserve(&c->h[H_CONNECT], micros(done->upstreamConnectTime), timeBuckets, TIME_BUCKETS);
    if (done->upstreamHeaderTime >= 0)
      metricObserve(&c->h[H_HEADER], micros(done->upstreamHeaderTime), timeBuckets, TIME_BUCKETS);
  }
}

/** Where the answer of the metrics is built: a line a series, up to the size of all of them. */
static char metricsAnswer[(size_t)(METRIC_SERIES + 1) * 1600];

static const int bucketsOf[HISTOGRAMS] = {TIME_BUCKETS, TIME_BUCKETS, TIME_BUCKETS, TIME_BUCKETS,
                                         LENGTH_BUCKETS, LENGTH_BUCKETS, SIZE_BUCKETS};

/**
 * Every series that counted, summed over the shards: its eight labels, the
 * requests, and per histogram its count, its sum and the counts of its
 * buckets with +Inf, tab-separated - the order of the histograms is that of
 * the comment at the top.
 */
static http_response_t listMetrics(http_request_t *req) {

  size_t used = 0;

  if (metrics == NULL)
    return req.reply(200).text("");

  for (int at = 0; at <= METRIC_SERIES; ++at) {

    seriesKey_t *key = &metrics->keys[at];
    counts_t sum;
    uint64_t count[HISTOGRAMS] = {0};
    const char *label;

    if (!__atomic_load_n(&key->ready, __ATOMIC_ACQUIRE))
      continue;

    memset(&sum, 0, sizeof sum);

    for (int shard = 0; shard < METRIC_SHARDS; ++shard) {

      counts_t *c = &metrics->counts[shard][at];

      for (int h = 0; h < HISTOGRAMS; ++h) {
        sum.h[h].sum += __atomic_load_n(&c->h[h].sum, __ATOMIC_RELAXED);
        for (int b = 0; b <= bucketsOf[h]; ++b)
          sum.h[h].bucket[b] += __atomic_load_n(&c->h[h].bucket[b], __ATOMIC_RELAXED);
      }
    }

    for (int h = 0; h < HISTOGRAMS; ++h)
      for (int b = 0; b <= bucketsOf[h]; ++b)
        count[h] += sum.h[h].bucket[b];

    if (count[H_DURATION] == 0 || used + 1600 > sizeof metricsAnswer)
      continue;

    /* the labels: the series without them has none */
    label = key->labels;
    for (int l = 0; l < 8; ++l) {
      const char *text = at == METRIC_SERIES ? "" : label;
      used += (size_t)snprintf(metricsAnswer + used, sizeof metricsAnswer - used, "%s\t", text);
      if (at != METRIC_SERIES)
        label += strlen(label) + 1;
    }

    used += (size_t)snprintf(metricsAnswer + used, sizeof metricsAnswer - used, "%llu", (unsigned long long)count[H_DURATION]);

    for (int h = 0; h < HISTOGRAMS; ++h) {
      used += (size_t)snprintf(metricsAnswer + used, sizeof metricsAnswer - used, "\t%llu\t%llu",
                               (unsigned long long)count[h], (unsigned long long)sum.h[h].sum);
      for (int b = 0; b <= bucketsOf[h]; ++b)
        used += (size_t)snprintf(metricsAnswer + used, sizeof metricsAnswer - used, "\t%llu",
                                 (unsigned long long)sum.h[h].bucket[b]);
    }

    metricsAnswer[used++] = '\n';
  }

  return req.reply(200).bytes("text/tab-separated-values", metricsAnswer, used);
}

/** Where the answer of the latency series is built: their labels and the bins that counted, up to the size of all of them. */
static char latencyAnswer[(size_t)(LATENCY_SERIES + 1) * (512 + LATENCY_BINS * 16)];

/**
 * Every series of ingress_upstream_latency_seconds that counted, summed
 * over the shards: its four labels, the count, the sum in microseconds and
 * the bins that counted as <bin>:<count>, separated by commas - a bin's
 * count wraps at 32 bits, which the controller's differences allow for.
 */
static http_response_t listLatency(http_request_t *req) {

  size_t used = 0;

  if (metrics == NULL)
    return req.reply(200).text("");

  for (int at = 0; at <= LATENCY_SERIES; ++at) {

    seriesKey_t *key = &metrics->latencyKeys[at];
    uint64_t count = 0, sum = 0;
    uint32_t bins[LATENCY_BINS];
    const char *label;
    int first = 1;

    if (!__atomic_load_n(&key->ready, __ATOMIC_ACQUIRE))
      continue;

    memset(bins, 0, sizeof bins);

    for (int shard = 0; shard < METRIC_SHARDS; ++shard) {

      latency_t *l = &metrics->latency[shard][at];

      count += __atomic_load_n(&l->count, __ATOMIC_RELAXED);
      sum += __atomic_load_n(&l->sum, __ATOMIC_RELAXED);
      for (int b = 0; b < LATENCY_BINS; ++b)
        bins[b] += __atomic_load_n(&l->bin[b], __ATOMIC_RELAXED);
    }

    if (count == 0 || used + 512 + LATENCY_BINS * 16 > sizeof latencyAnswer)
      continue;

    label = key->labels;
    for (int l = 0; l < 4; ++l) {
      const char *text = at == LATENCY_SERIES ? "" : label;
      used += (size_t)snprintf(latencyAnswer + used, sizeof latencyAnswer - used, "%s\t", text);
      if (at != LATENCY_SERIES)
        label += strlen(label) + 1;
    }

    used += (size_t)snprintf(latencyAnswer + used, sizeof latencyAnswer - used, "%llu\t%llu\t",
                             (unsigned long long)count, (unsigned long long)sum);

    for (int b = 0; b < LATENCY_BINS; ++b) {
      if (bins[b] == 0)
        continue;
      used += (size_t)snprintf(latencyAnswer + used, sizeof latencyAnswer - used, "%s%d:%u", first ? "" : ",", b, bins[b]);
      first = 0;
    }

    latencyAnswer[used++] = '\n';
  }

  return req.reply(200).bytes("text/tab-separated-values", latencyAnswer, used);
}
