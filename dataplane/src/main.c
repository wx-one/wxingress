/**
 * wxingress's data plane: what runs inside nginx beside the controller.
 *
 * The controller writes nginx.conf and reloads it, so this program is hosted
 * - it writes nothing, and answers where the controller's file says
 * `meta_pass;`. Today that is one place: the upstream API on the socket
 * NGINX Plus has its own on, /var/lib/nginx/nginx-plus-api.sock.
 *
 * ---------------------------------------------------------- the upstream API
 *
 * The controller changes the servers of an upstream with the client NGINX
 * Plus users have (nginx-plus-go-client), and with `-dynamic-upstreams` it
 * does that against open-source nginx too. This answers the requests that
 * client makes, in the shape it reads:
 *
 *   GET    /api                                         [9]
 *   GET    /api/9/http/upstreams/:name/servers          the servers
 *   GET    /api/9/http/upstreams/:name/servers/:id      one of them
 *   POST   /api/9/http/upstreams/:name/servers          201, the new one
 *   PATCH  /api/9/http/upstreams/:name/servers/:id      200, as it is now
 *   DELETE /api/9/http/upstreams/:name/servers/:id      200, those left
 *
 * and the same five under /api/9/stream/upstreams/ for the upstreams of the
 * `stream` block, which a TransportServer's endpoints go to.
 *
 * The client adds a trailing slash to some of these; the router drops it.
 * Anything this cannot do is an error in NGINX Plus's own form, and the
 * controller answers every error the same way: it reloads, which is slower
 * and never wrong.
 *
 * The servers themselves are nginx's: an upstream with a `zone` keeps them in
 * shared memory, and `http.peers` and the calls beside it change that list
 * the way nginx's own resolver does - `http.streamPeers` and its three the
 * list of a stream upstream, which is another list under the same name.
 * Every worker balances over the one list.
 *
 * --------------------------------------------------------------- canaries
 *
 * ingress-nginx's canary, decided per request as its balancer decided it
 * (balancer.lua, route_to_alternative_balancer): a location proxies to
 * `http://$meta_canary_<primary>`, and the variable names the primary
 * upstream or its canary. The rule for each primary is in shared memory and
 * the controller sets it here, so a weight, a header or a cookie changes
 * with no reload - which is how ingress-nginx changed them too:
 *
 *   GET    /api/wxingress/1/canaries              every rule
 *   GET    /api/wxingress/1/canaries/:primary     one
 *   PUT    /api/wxingress/1/canaries/:primary     set it
 *   DELETE /api/wxingress/1/canaries/:primary     and take it away
 *
 * A rule is {"canary", "weight", "weightTotal", "header", "headerValue",
 * "headerPattern", "cookie"}, the trafficShapingPolicy of ingress-nginx under
 * the names its annotations have, and "affinityCookie", the name of the
 * `sticky cookie` both upstreams pin sessions with. A primary without a rule
 * goes to itself.
 *
 * --------------------------------------------------------------- balancers
 *
 * ingress-nginx's balancer, per upstream and per try (balancer.lua): every
 * upstream of an Ingress says `meta_balancer;`, and a rule the controller
 * sets here - round_robin, ewma, chash, chashsubset, sticky_balanced or
 * sticky_persistent, with the hash key and the affinity cookie - chooses
 * its servers, so load-balance, upstream-hash-by and affinity change with
 * no reload. See "balancers" below for the API and the algorithms.
 *
 * ----------------------------------------------------------- certificates
 *
 * ingress-nginx's certificates, chosen per handshake by the name the client
 * sent (certificate.lua) rather than written into the configuration: the
 * controller sets the PEM of each name here, in shared memory, and every
 * worker answers the next handshake with it - no reload for a certificate
 * that comes, changes or goes:
 *
 *   GET    /api/wxingress/1/certificates          every name, with its generation
 *   GET    /api/wxingress/1/certificates/:name    one, its certificate without the key
 *   PUT    /api/wxingress/1/certificates/:name    set it: {"certificate", "key"}
 *   DELETE /api/wxingress/1/certificates/:name    and take it away
 *
 * A name is a host, a wildcard `*.example.com`, or `_`, the default. A
 * handshake is answered as ingress-nginx answers it: the name the client sent
 * - in lower case and without a trailing dot - then the wildcard of one level
 * above it, then `_`; with none of the three, or with no name at all and no
 * `_`, the certificate the configuration names for the server, which is the
 * default server's.
 *
 * A name may be bound to the port nginx took the connection on, `name:port`:
 * a Gateway has listeners on ports of its own, two Gateways may have the same
 * hostname with other certificates, and a listener without a hostname has a
 * certificate for every name on its port alone. A handshake on a port tries
 * the three names bound to it first - `name:port`, the wildcard's, `_:port` -
 * and then the three above.
 */
#include <meta_http.h>

#include <errno.h>
#include <pthread.h>
#include <regex.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/random.h>
#include <time.h>
#include <unistd.h>

/** How many servers one upstream may be listed with. */
#define MOST_SERVERS 1024

/**
 * Where an answer is built.
 *
 * Static, for the reason meta's own examples give: the module copies the
 * answer into the request's pool once the handler has returned, and no
 * handler here waits between filling it and returning, so one per worker is
 * enough. 160 bytes a server is more than the longest one written.
 */
static char answer[MOST_SERVERS * 160 + 256];
static http_peer_t listed[MOST_SERVERS];

/** A server added to or removed from an upstream, for ewma (with the balancers). */
static void ewmaServers(const char *upstream, const char *address, int added);

/** A counter for `request_id`, which the client prints and nothing reads. */
static unsigned long requests;

/**
 * An error, in the form NGINX Plus writes one:
 *
 *   {"error":{"status":404,"text":"upstream not found","code":"UpstreamNotFound"},
 *    "request_id":"...","href":"https://nginx.org/en/docs/http/ngx_http_api_module.html"}
 *
 * The text is a literal of this file, so it needs no escaping.
 */
static http_response_t failed(http_request_t *req, int status, const char *text,
                              const char *code) {

  snprintf(answer, sizeof answer,
           "{\"error\":{\"status\":%d,\"text\":\"%s\",\"code\":\"%s\"},"
           "\"request_id\":\"%016lx\","
           "\"href\":\"https://nginx.org/en/docs/http/ngx_http_api_module.html\"}",
           status, text, code, ++requests);

  return req.reply(status).json(answer);
}

/** A refusal of meta's peer calls, as the error NGINX Plus gives for it. */
static http_response_t refused(http_request_t *req, int why) {

  switch (why) {
  case HTTP_PEER_NO_UPSTREAM:
    return failed(req, 404, "upstream not found", "UpstreamNotFound");
  case HTTP_PEER_NO_ZONE:
    /* NGINX Plus does not see an upstream without a zone at all */
    return failed(req, 404, "upstream not found", "UpstreamNotFound");
  case HTTP_PEER_NOT_FOUND:
    return failed(req, 404, "server not found", "UpstreamServerNotFound");
  case HTTP_PEER_BAD_ADDRESS:
    return failed(req, 400, "invalid \\\"server\\\" argument", "UpstreamBadAddress");
  case HTTP_PEER_EXISTS:
    return failed(req, 409, "server already exists", "UpstreamServerExists");
  case HTTP_PEER_NO_MEMORY:
    return failed(req, 500, "upstream zone is full", "UpstreamOutOfMemory");
  default:
    return failed(req, 500, "the upstream could not be changed", "NginxError");
  }
}

/**
 * Whether a request is for the upstreams of the `stream` block, whose
 * servers are another list than those of the http upstream of the same name.
 */
static int streamed(http_request_t *req) {
  return strncmp(req->path, "/api/9/stream/", 14) == 0;
}

/** meta's four calls on peers, for the http or the stream upstream of a name. */
static int peersOf(int stream, const char *upstream, http_peer_t *into, int room) {
  return stream ? http.streamPeers(upstream, into, room)
                : http.peers(upstream, into, room);
}

static int peerAdd(int stream, const char *upstream, http_peer_t peer) {
  return stream ? http.addStreamPeer(upstream, peer) : http.addPeer(upstream, peer);
}

static int peerChange(int stream, const char *upstream, int id, http_peer_t peer,
                      unsigned what) {
  return stream ? http.changeStreamPeer(upstream, id, peer, what)
                : http.changePeer(upstream, id, peer, what);
}

static int peerRemove(int stream, const char *upstream, int id) {
  return stream ? http.removeStreamPeer(upstream, id) : http.removePeer(upstream, id);
}

/**
 * One server as JSON, at `into`; how many bytes it took.
 *
 * Every field the client compares, in its defaults where nginx has no
 * other: an open-source nginx has no slow start and no routes, so those are
 * "0s" and "" always - and a stream server has no route at all, as in NGINX
 * Plus. `server` is the address as nginx writes it, which is what the
 * controller sends - `10.42.0.7:8080`, `[fd00::7]:8080`.
 */
static size_t written(char *into, size_t room, const http_peer_t *peer,
                      int stream) {

  int length = snprintf(into, room,
                        "{\"id\":%d,\"server\":\"%s\",\"weight\":%d,"
                        "\"max_conns\":%d,\"max_fails\":%d,\"fail_timeout\":\"%ds\","
                        "\"slow_start\":\"0s\",%s\"backup\":%s,"
                        "\"down\":%s}",
                        peer->id, peer->server, peer->weight, peer->maxConns,
                        peer->maxFails, peer->failTimeout,
                        stream ? "" : "\"route\":\"\",",
                        peer->backup ? "true" : "false",
                        peer->down ? "true" : "false");

  if (length < 0)
    return 0;

  return (size_t)length < room ? (size_t)length : room;
}

/** The servers of an upstream as a JSON array in `answer`; 0, or a refusal. */
static int listing(int stream, const char *upstream) {

  int count = peersOf(stream, upstream, listed, MOST_SERVERS);
  size_t used = 0;

  if (count < 0)
    return count;

  if (count > MOST_SERVERS)
    count = MOST_SERVERS;

  answer[used++] = '[';

  for (int at = 0; at < count; ++at) {

    if (at > 0)
      answer[used++] = ',';

    used += written(answer + used, sizeof answer - used - 2, &listed[at], stream);
  }

  answer[used++] = ']';
  answer[used] = 0;

  return 0;
}

/** The server with that id, from a fresh listing; NULL when there is none. */
static http_peer_t *server(int stream, const char *upstream, int id, int *why) {

  int count = peersOf(stream, upstream, listed, MOST_SERVERS);

  *why = count < 0 ? count : HTTP_PEER_NOT_FOUND;

  for (int at = 0; at < count && at < MOST_SERVERS; ++at)
    if (listed[at].id == id)
      return &listed[at];

  return NULL;
}

/**
 * A duration as nginx writes one - `10s`, `1m30s`, `500ms`, `30` - in whole
 * seconds, rounded up so that half a second is not none. Negative when it is
 * not one.
 */
static int seconds(const char *text) {

  long total = 0;
  const char *at = text;

  if (*at == 0)
    return -1;

  while (*at != 0) {

    char *after;
    long value = strtol(at, &after, 10);
    long scale;

    if (after == at || value < 0)
      return -1;

    at = after;

    if (strncmp(at, "ms", 2) == 0) {
      total += (value + 999) / 1000;
      at += 2;
      continue;
    }

    switch (*at) {
    case 0:
    case 's': scale = 1; break;
    case 'm': scale = 60; break;
    case 'h': scale = 3600; break;
    case 'd': scale = 86400; break;
    default: return -1;
    }

    if (*at != 0)
      ++at;

    total += value * scale;

    if (total > 365L * 86400)
      return -1;
  }

  return (int)total;
}

/**
 * What a request body says about a server, into `peer` and `what`; NULL when
 * it holds, else why not.
 *
 * Only what open-source nginx has: an address, a weight, the connection and
 * failure limits, `down`, `backup`. Slow start, routes, services and draining
 * are NGINX Plus's, and a body asking for one is refused rather than taken
 * without it - except at the value that means "none", which is what the
 * client sends when nobody asked.
 */
static const char *reading(json_t said, http_peer_t *peer, unsigned *what,
                           int adding) {

  int count = said.count();

  if (strcmp(said.kind(), "object") != 0)
    return "the body is a JSON object";

  for (int at = 0; at < count; ++at) {

    const char *key = said.keyAt(at);
    json_t value = said.get(key);

    if (strcmp(key, "server") == 0) {

      const char *address = value.text();

      if (strcmp(value.kind(), "string") != 0 || strlen(address) == 0 ||
          strlen(address) >= sizeof peer->server)
        return "\\\"server\\\" is an address";

      strcpy(peer->server, address);

    } else if (strcmp(key, "weight") == 0) {

      if (value.number() < 1)
        return "\\\"weight\\\" is at least 1";

      peer->weight = (int)value.number();
      *what |= HTTP_PEER_WEIGHT;

    } else if (strcmp(key, "max_conns") == 0) {

      if (value.number() < 0)
        return "\\\"max_conns\\\" is 0 or more";

      peer->maxConns = (int)value.number();
      *what |= HTTP_PEER_MAX_CONNS;

    } else if (strcmp(key, "max_fails") == 0) {

      if (value.number() < 0)
        return "\\\"max_fails\\\" is 0 or more";

      /* 0 is nginx's "never counted", which meta reads as the default 1 */
      peer->maxFails = value.number() == 0 ? 1 : (int)value.number();
      *what |= HTTP_PEER_MAX_FAILS;

    } else if (strcmp(key, "fail_timeout") == 0) {

      int given = strcmp(value.kind(), "string") == 0 ? seconds(value.text())
                                                      : (int)value.number();

      if (given < 0)
        return "\\\"fail_timeout\\\" is a duration";

      peer->failTimeout = given;
      *what |= HTTP_PEER_FAIL_TIMEOUT;

    } else if (strcmp(key, "down") == 0) {

      peer->down = value.truth();
      *what |= HTTP_PEER_DOWN;

    } else if (strcmp(key, "backup") == 0) {

      if (!adding && value.truth())
        return "a server cannot become a backup";

      peer->backup = value.truth();

    } else if (strcmp(key, "slow_start") == 0) {

      if (seconds(value.text()) != 0)
        return "\\\"slow_start\\\" is for NGINX Plus";

    } else if (strcmp(key, "route") == 0 || strcmp(key, "service") == 0) {

      if (value.text()[0] != 0)
        return "routes and services are for NGINX Plus";

    } else if (strcmp(key, "drain") == 0) {

      if (value.truth())
        return "draining is for NGINX Plus";

    } else if (strcmp(key, "id") == 0) {

      return "\\\"id\\\" is the server's, not the request's";

    } else {

      return "unknown parameter";
    }
  }

  return NULL;
}

/** The id in the path, or -1 for one that is not a number. */
static int idOf(http_request_t *req) {

  const char *text = req.param("id");
  char *after;
  long id = strtol(text, &after, 10);

  if (*text == 0 || *after != 0 || id < 0 || id > 0x7fffffffL)
    return -1;

  return (int)id;
}

/* ------------------------------------------------------------------ routes */

static http_response_t versions(http_request_t *req) {
  return req.reply(200).json("[9]");
}

static http_response_t listServers(http_request_t *req) {

  int why = listing(streamed(req), req.param("name"));

  if (why < 0)
    return refused(req, why);

  return req.reply(200).json(answer);
}

static http_response_t showServer(http_request_t *req) {

  int why, stream = streamed(req);
  http_peer_t *found = server(stream, req.param("name"), idOf(req), &why);

  if (found == NULL)
    return refused(req, why);

  written(answer, sizeof answer, found, stream);

  return req.reply(200).json(answer);
}

static http_response_t addServer(http_request_t *req) {

  json_t said = req.readJson();
  http_peer_t peer = {0};
  unsigned what = 0;
  const char *wrong;
  int id, why, stream = streamed(req);
  http_peer_t *added;

  if (said._refused != NULL) {
    said.release();
    return failed(req, 415, "the body is not JSON", "UpstreamConfFormatError");
  }

  wrong = reading(said, &peer, &what, 1);
  said.release();

  if (wrong != NULL)
    return failed(req, 400, wrong, "UpstreamConfFormatError");

  if (peer.server[0] == 0)
    return failed(req, 400, "\\\"server\\\" is required", "UpstreamConfFormatError");

  id = peerAdd(stream, req.param("name"), peer);

  if (id < 0)
    return refused(req, id);

  /* ewma is a balancer of http upstreams */
  if (!stream)
    ewmaServers(req.param("name"), peer.server, 1);

  added = server(stream, req.param("name"), id, &why);

  if (added == NULL)
    return refused(req, why);

  written(answer, sizeof answer, added, stream);

  return req.reply(201).json(answer);
}

static http_response_t changeServer(http_request_t *req) {

  json_t said = req.readJson();
  http_peer_t peer = {0};
  unsigned what = 0;
  const char *wrong;
  int id = idOf(req), why, done, stream = streamed(req);
  http_peer_t *found;

  if (said._refused != NULL) {
    said.release();
    return failed(req, 415, "the body is not JSON", "UpstreamConfFormatError");
  }

  wrong = reading(said, &peer, &what, 0);
  said.release();

  if (wrong != NULL)
    return failed(req, 400, wrong, "UpstreamConfFormatError");

  found = server(stream, req.param("name"), id, &why);

  if (found == NULL)
    return refused(req, why);

  /**
   * The client sends the address with every change, and it is the one the
   * server has. A different one would be a different server, which is a
   * removal and an addition, not a change.
   */
  if (peer.server[0] != 0 && strcmp(peer.server, found->server) != 0)
    return failed(req, 400, "the address of a server does not change",
                  "UpstreamConfFormatError");

  done = peerChange(stream, req.param("name"), id, peer, what);

  if (done < 0)
    return refused(req, done);

  found = server(stream, req.param("name"), id, &why);

  if (found == NULL)
    return refused(req, why);

  written(answer, sizeof answer, found, stream);

  return req.reply(200).json(answer);
}

static http_response_t removeServer(http_request_t *req) {

  int why, stream = streamed(req);
  http_peer_t *going = server(stream, req.param("name"), idOf(req), &why);
  char address[sizeof going->server];
  int done;

  address[0] = 0;

  if (going != NULL)
    strcpy(address, going->server);

  done = peerRemove(stream, req.param("name"), idOf(req));

  if (done < 0)
    return refused(req, done);

  if (address[0] != 0 && !stream)
    ewmaServers(req.param("name"), address, 0);

  done = listing(stream, req.param("name"));

  if (done < 0)
    return refused(req, done);

  return req.reply(200).json(answer);
}

/* ---------------------------------------------------------------- canaries */

/** How many primaries can have a canary at once. */
#define CANARIES 4096

/** One primary's rule. `seq` is odd while it is being written. */
typedef struct {
  unsigned seq;
  int used;
  unsigned generation;
  char primary[128];
  char canary[128];
  int weight;
  int weightTotal;
  char header[64];
  char headerValue[256];
  char headerPattern[256];
  char cookie[64];
  char affinityCookie[64];
} canaryRule_t;

typedef struct {
  pthread_mutex_t lock;
  unsigned generation;
  canaryRule_t rules[CANARIES];
} canaries_t;

/**
 * Made by the master before it forks, so every worker - and every worker of
 * every later configuration - has the same table at the same address. A
 * reload is the controller's and keeps it: the rules outlive the file.
 */
static canaries_t *canaries;

static int canariesOpen(void) {

  pthread_mutexattr_t how;
  void *mapped;

  if (canaries != NULL)
    return 0;

  mapped = mmap(NULL, sizeof(canaries_t), PROT_READ | PROT_WRITE,
                MAP_SHARED | MAP_ANONYMOUS, -1, 0);

  if (mapped == MAP_FAILED) {
    fprintf(stderr, "wxingress: no shared memory for canaries: %s\n",
            strerror(errno));
    return -1;
  }

  canaries = mapped;

  if (pthread_mutexattr_init(&how) != 0 ||
      pthread_mutexattr_setpshared(&how, PTHREAD_PROCESS_SHARED) != 0 ||
      pthread_mutexattr_setrobust(&how, PTHREAD_MUTEX_ROBUST) != 0 ||
      pthread_mutex_init(&canaries->lock, &how) != 0) {
    fprintf(stderr, "wxingress: no lock for the canaries\n");
    return -1;
  }

  return 0;
}

static void canariesLock(void) {

  /* a worker that died holding it left the table as it was: take it over */
  if (pthread_mutex_lock(&canaries->lock) == EOWNERDEAD)
    pthread_mutex_consistent(&canaries->lock);
}

static unsigned slotOf(const char *primary) {

  unsigned hash = 2166136261u;

  for (const char *at = primary; *at != 0; ++at)
    hash = (hash ^ (unsigned char)*at) * 16777619u;

  return hash % CANARIES;
}

/**
 * A copy of the rule for `primary`, read without a lock: the writer makes
 * `seq` odd, writes, and makes it even again, and a reader that saw it move
 * reads again. Zero when there is a rule.
 */
static int canaryRead(const char *primary, canaryRule_t *into) {

  unsigned slot = slotOf(primary);

  for (unsigned probe = 0; probe < CANARIES; ++probe) {

    canaryRule_t *rule = &canaries->rules[(slot + probe) % CANARIES];
    unsigned before;

    do {
      before = __atomic_load_n(&rule->seq, __ATOMIC_ACQUIRE);

      while (before & 1)
        before = __atomic_load_n(&rule->seq, __ATOMIC_ACQUIRE);

      memcpy(into, rule, sizeof *into);
      __atomic_thread_fence(__ATOMIC_ACQUIRE);
    } while (__atomic_load_n(&rule->seq, __ATOMIC_RELAXED) != before);

    if (!into->used && into->primary[0] == 0)
      return -1; /* an empty slot ends the probe */

    if (into->used && strcmp(into->primary, primary) == 0)
      return 0;
  }

  return -1;
}

/**
 * Writes `rule` for its primary, or takes the primary's rule away with
 * `rule->used` 0. Under the lock, so two writers never share a slot. Zero, or
 * -1 when the table is full.
 */
static int canaryWrite(const canaryRule_t *given) {

  unsigned slot = slotOf(given->primary);
  canaryRule_t *found = NULL;
  canaryRule_t *free = NULL;

  canariesLock();

  for (unsigned probe = 0; probe < CANARIES; ++probe) {

    canaryRule_t *rule = &canaries->rules[(slot + probe) % CANARIES];

    if (rule->used && strcmp(rule->primary, given->primary) == 0) {
      found = rule;
      break;
    }

    /* a removed rule keeps its primary, so a later one is still found past it */
    if (!rule->used && free == NULL)
      free = rule;

    if (!rule->used && rule->primary[0] == 0)
      break;
  }

  if (found == NULL && given->used)
    found = free;

  if (found == NULL) {
    pthread_mutex_unlock(&canaries->lock);
    return given->used ? -1 : 0;
  }

  __atomic_add_fetch(&found->seq, 1, __ATOMIC_RELEASE);

  if (given->used) {
    unsigned seq = found->seq;
    memcpy(found, given, sizeof *found);
    found->seq = seq;
    found->generation = ++canaries->generation;
  } else {
    found->used = 0;
  }

  __atomic_add_fetch(&found->seq, 1, __ATOMIC_RELEASE);

  pthread_mutex_unlock(&canaries->lock);

  return 0;
}

/**
 * The header pattern of each rule, compiled once per worker and again when
 * the rule changes. POSIX extended expressions, unanchored, as ingress-nginx
 * matched its PCRE: what differs is PCRE's own syntax - \\d, lookarounds -
 * which a rule written for POSIX does not need.
 */
static struct {
  unsigned generation;
  int ok;
  regex_t compiled;
} patterns[CANARIES];

static int patternMatches(const canaryRule_t *rule, const char *text, int *broken) {

  unsigned slot = slotOf(rule->primary);
  unsigned probe;

  /* the slot the rule is in, which the hash only starts at */
  for (probe = 0; probe < CANARIES; ++probe)
    if (strcmp(canaries->rules[(slot + probe) % CANARIES].primary, rule->primary) == 0)
      break;

  slot = (slot + probe) % CANARIES;

  if (patterns[slot].generation != rule->generation) {

    if (patterns[slot].ok)
      regfree(&patterns[slot].compiled);

    patterns[slot].ok = regcomp(&patterns[slot].compiled, rule->headerPattern,
                                REG_EXTENDED | REG_NOSUB) == 0;
    patterns[slot].generation = rule->generation;
  }

  if (!patterns[slot].ok) {
    *broken = 1;
    return 0;
  }

  return regexec(&patterns[slot].compiled, text, 0, NULL, 0) == 0;
}

/** The value of cookie `name` in the request, or NULL. */
static const char *cookieOf(http_request_t *req, const char *name, char *into,
                            size_t room) {

  const char *at = req.header("cookie");
  size_t length = strlen(name);

  while (*at != 0) {

    const char *end;

    while (*at == ' ' || *at == ';')
      ++at;

    end = strchr(at, ';');

    if (end == NULL)
      end = at + strlen(at);

    if ((size_t)(end - at) > length && strncmp(at, name, length) == 0 &&
        at[length] == '=') {

      size_t value = (size_t)(end - at) - length - 1;

      if (value >= room)
        value = room - 1;

      memcpy(into, at + length + 1, value);
      into[value] = 0;
      return into;
    }

    at = end;
  }

  return NULL;
}

/** A worker's own random numbers, for the weight. */
static unsigned long long randomState;

static unsigned randomUpTo(unsigned total) {

  if (randomState == 0 &&
      getrandom(&randomState, sizeof randomState, 0) != sizeof randomState)
    randomState = (unsigned long long)time(NULL) ^ (unsigned long long)getpid() << 32;

  randomState ^= randomState << 13;
  randomState ^= randomState >> 7;
  randomState ^= randomState << 17;

  return (unsigned)(randomState % total) + 1;
}

/**
 * Whether a request goes to the canary - ingress-nginx's order, rule for
 * rule (route_to_alternative_balancer, steps 7 to 9): the header, then the
 * cookie, then the weight, and the first that decides wins.
 *
 *   header      with a headerValue, that value means the canary and anything
 *               else falls through; with a headerPattern, a match does; with
 *               neither, "always" is the canary and "never" is the primary -
 *               and stops, cookie and weight unasked.
 *   cookie      "always" and "never", the same way.
 *   weight      out of weightTotal, at least 100: a random 1..total at or
 *               under the weight.
 *
 * Before all of it, as in ingress-nginx (steps 1 and 5), a session already
 * pinned stays where it is pinned: a sticky cookie naming a server of the
 * primary keeps the request on the primary whatever the header says, and
 * one naming a server of the canary keeps it on the canary.
 */
static http_peer_t pinned[MOST_SERVERS];

/** Whether `sid` names one of the servers of `upstream`. */
static int pins(const char *upstream, const char *sid) {

  int count = http.peers(upstream, pinned, MOST_SERVERS);

  for (int at = 0; at < count && at < MOST_SERVERS; ++at)
    if (pinned[at].sid[0] != 0 && strcmp(pinned[at].sid, sid) == 0)
      return 1;

  return 0;
}

/** Whether the affinity cookie of the balancer of `upstream` names it (below). */
static int affinitized(http_request_t *req, const char *upstream);

static int toCanary(http_request_t *req, const canaryRule_t *rule) {

  char cookie[256];
  unsigned total;

  /* the cookie of ingress-nginx's affinity names the backend it pinned */
  if (affinitized(req, rule->primary))
    return 0;

  if (affinitized(req, rule->canary))
    return 1;

  if (rule->affinityCookie[0] != 0 &&
      cookieOf(req, rule->affinityCookie, cookie, sizeof cookie) != NULL &&
      cookie[0] != 0) {

    if (pins(rule->primary, cookie))
      return 0;

    if (pins(rule->canary, cookie))
      return 1;
  }

  if (rule->header[0] != 0) {

    const char *said = req.header(rule->header);

    if (said[0] != 0) {

      if (rule->headerValue[0] != 0) {
        if (strcmp(said, rule->headerValue) == 0)
          return 1;
      } else if (rule->headerPattern[0] != 0) {
        int broken = 0;

        if (patternMatches(rule, said, &broken))
          return 1;

        if (broken)
          return 0;
      } else if (strcmp(said, "always") == 0) {
        return 1;
      } else if (strcmp(said, "never") == 0) {
        return 0;
      }
    }
  }

  if (rule->cookie[0] != 0 && cookieOf(req, rule->cookie, cookie, sizeof cookie) != NULL) {

    if (strcmp(cookie, "always") == 0)
      return 1;

    if (strcmp(cookie, "never") == 0)
      return 0;
  }

  if (rule->weight <= 0)
    return 0;

  total = rule->weightTotal > 100 ? (unsigned)rule->weightTotal : 100;

  return randomUpTo(total) <= (unsigned)rule->weight;
}

/**
 * The upstream name a variable's argument stands for.
 *
 * Upstream names have `-` and `.` in them and a variable's name may not, so
 * the controller writes `_d` for `-`, `_p` for `.` and `__` for `_`, and this
 * reads them back. `default-cafe.example.com-tea-80` is
 * `$meta_canary_default_dcafe_pexample_pcom_dtea_d80`. nginx lowers the
 * names of variables, so a capital is `_u` and the letter:
 * `vs_ns_GW-1_be-2`, an upstream of a Gateway route, is
 * `$meta_canary_vs__ns___ug_uw_d1__be_d2`.
 */
static const char *upstreamOf(const char *argument, char *into, size_t room) {

  size_t used = 0;

  for (const char *at = argument; *at != 0 && used + 1 < room; ++at) {

    if (*at != '_') {
      into[used++] = *at;
      continue;
    }

    ++at;

    if (*at == 'd')
      into[used++] = '-';
    else if (*at == 'p')
      into[used++] = '.';
    else if (*at == '_')
      into[used++] = '_';
    else if (*at == 'u' && at[1] >= 'a' && at[1] <= 'z')
      into[used++] = (char)(*++at - 'a' + 'A');
    else
      return NULL;
  }

  into[used] = 0;

  return into;
}

/**
 * `$meta_canary_<primary>`: the primary, or its canary for this request.
 *
 * A canary with no servers is not chosen, as in ingress-nginx, where a
 * canary without endpoints has no balancer to route to.
 */
static const char *chooseUpstream(http_request_t *req, const char *argument) {

  static char primary[128];
  static canaryRule_t rule;

  if (upstreamOf(argument, primary, sizeof primary) == NULL)
    return NULL;

  if (canaries == NULL || canaryRead(primary, &rule) != 0)
    return primary;

  if (!toCanary(req, &rule) || http.peers(rule.canary, NULL, 0) <= 0)
    return primary;

  return rule.canary;
}

/** The rule as JSON at `into`. Every text in a rule passed `textual`. */
static size_t canaryWritten(char *into, size_t room, const canaryRule_t *rule) {

  int length = snprintf(into, room,
                        "{\"primary\":\"%s\",\"canary\":\"%s\",\"weight\":%d,"
                        "\"weightTotal\":%d,\"header\":\"%s\",\"headerValue\":\"%s\","
                        "\"headerPattern\":\"%s\",\"cookie\":\"%s\","
                        "\"affinityCookie\":\"%s\"}",
                        rule->primary, rule->canary, rule->weight,
                        rule->weightTotal, rule->header, rule->headerValue,
                        rule->headerPattern, rule->cookie, rule->affinityCookie);

  if (length < 0)
    return 0;

  return (size_t)length < room ? (size_t)length : room;
}

/**
 * Whether text can go into a rule and back out into JSON as it is: no quote,
 * no backslash, nothing below a space, and room for it. A header pattern
 * has backslashes in it often enough, so it is the one field that may - and
 * it is written back escaped.
 */
static int textual(const char *text, size_t room, int backslashes) {

  if (strlen(text) >= room)
    return 0;

  for (const char *at = text; *at != 0; ++at)
    if (*at == '"' || (unsigned char)*at < 0x20 || (*at == '\\' && !backslashes))
      return 0;

  return 1;
}

static const char *ruleRead(json_t said, canaryRule_t *rule) {

  int count = said.count();

  if (strcmp(said.kind(), "object") != 0)
    return "the body is a JSON object";

  for (int at = 0; at < count; ++at) {

    const char *key = said.keyAt(at);
    json_t value = said.get(key);
    const char *text = value.text();

    if (strcmp(key, "canary") == 0) {
      if (!textual(text, sizeof rule->canary, 0) || text[0] == 0)
        return "\\\"canary\\\" is an upstream";
      strcpy(rule->canary, text);
    } else if (strcmp(key, "weight") == 0) {
      if (value.number() < 0)
        return "\\\"weight\\\" is 0 or more";
      rule->weight = (int)value.number();
    } else if (strcmp(key, "weightTotal") == 0) {
      if (value.number() < 0)
        return "\\\"weightTotal\\\" is 0 or more";
      rule->weightTotal = (int)value.number();
    } else if (strcmp(key, "header") == 0) {
      if (!textual(text, sizeof rule->header, 0))
        return "\\\"header\\\" is a header's name";
      strcpy(rule->header, text);
    } else if (strcmp(key, "headerValue") == 0) {
      if (!textual(text, sizeof rule->headerValue, 0))
        return "\\\"headerValue\\\" is too long or has a quote";
      strcpy(rule->headerValue, text);
    } else if (strcmp(key, "headerPattern") == 0) {
      regex_t trial;

      if (!textual(text, sizeof rule->headerPattern, 1))
        return "\\\"headerPattern\\\" is too long or has a quote";

      if (text[0] != 0) {
        if (regcomp(&trial, text, REG_EXTENDED | REG_NOSUB) != 0)
          return "\\\"headerPattern\\\" is not an extended regular expression";
        regfree(&trial);
      }

      strcpy(rule->headerPattern, text);
    } else if (strcmp(key, "cookie") == 0) {
      if (!textual(text, sizeof rule->cookie, 0))
        return "\\\"cookie\\\" is a cookie's name";
      strcpy(rule->cookie, text);
    } else if (strcmp(key, "affinityCookie") == 0) {
      if (!textual(text, sizeof rule->affinityCookie, 0))
        return "\\\"affinityCookie\\\" is a cookie's name";
      strcpy(rule->affinityCookie, text);
    } else if (strcmp(key, "primary") != 0) {
      return "unknown parameter";
    }
  }

  if (rule->canary[0] == 0)
    return "\\\"canary\\\" is required";

  return NULL;
}

/** A backslash in a pattern, written back as JSON wants it. */
static size_t escaped(char *into, size_t room, const char *text) {

  size_t used = 0;

  for (const char *at = text; *at != 0 && used + 2 < room; ++at) {
    if (*at == '\\')
      into[used++] = '\\';
    into[used++] = *at;
  }

  into[used] = 0;

  return used;
}

static http_response_t listCanaries(http_request_t *req) {

  size_t used = 0;
  int first = 1;

  answer[used++] = '[';

  for (int at = 0; at < CANARIES && used + 2048 < sizeof answer; ++at) {

    canaryRule_t rule;

    if (!canaries->rules[at].used || canaryRead(canaries->rules[at].primary, &rule) != 0)
      continue;

    if (!first)
      answer[used++] = ',';

    first = 0;
    {
      char pattern[sizeof rule.headerPattern];

      escaped(pattern, sizeof pattern, rule.headerPattern);
      strcpy(rule.headerPattern, pattern);
    }
    used += canaryWritten(answer + used, sizeof answer - used - 2, &rule);
  }

  answer[used++] = ']';
  answer[used] = 0;

  return req.reply(200).json(answer);
}

static http_response_t showCanary(http_request_t *req) {

  canaryRule_t rule;
  char pattern[sizeof rule.headerPattern];

  if (canaryRead(req.param("primary"), &rule) != 0)
    return failed(req, 404, "no canary for that upstream", "CanaryNotFound");

  escaped(pattern, sizeof pattern, rule.headerPattern);
  strcpy(rule.headerPattern, pattern);
  canaryWritten(answer, sizeof answer, &rule);

  return req.reply(200).json(answer);
}

static http_response_t setCanary(http_request_t *req) {

  json_t said = req.readJson();
  canaryRule_t rule;
  const char *wrong;
  const char *primary = req.param("primary");

  memset(&rule, 0, sizeof rule);

  if (said._refused != NULL) {
    said.release();
    return failed(req, 415, "the body is not JSON", "CanaryFormatError");
  }

  if (!textual(primary, sizeof rule.primary, 0) || primary[0] == 0) {
    said.release();
    return failed(req, 400, "not an upstream's name", "CanaryFormatError");
  }

  strcpy(rule.primary, primary);
  rule.used = 1;

  wrong = ruleRead(said, &rule);
  said.release();

  if (wrong != NULL)
    return failed(req, 400, wrong, "CanaryFormatError");

  if (strcmp(rule.canary, rule.primary) == 0)
    return failed(req, 400, "an upstream is not its own canary", "CanaryFormatError");

  if (canaryWrite(&rule) != 0)
    return failed(req, 507, "the canary table is full", "CanaryTableFull");

  return showCanary(req);
}

static http_response_t removeCanary(http_request_t *req) {

  canaryRule_t rule;

  if (canaryRead(req.param("primary"), &rule) != 0)
    return failed(req, 404, "no canary for that upstream", "CanaryNotFound");

  rule.used = 0;
  canaryWrite(&rule);

  return req.reply(200).json("{}");
}

/* --------------------------------------------------------------- balancers */

/**
 * ingress-nginx's balancer (balancer.lua and balancer/*.lua) for the
 * upstreams whose block says `meta_balancer;`, which in -dynamic-upstreams
 * mode is all of an Ingress's. The algorithm of each upstream, its hash key
 * and its cookie are a rule in shared memory that the controller sets here,
 * so that changing load-balance, upstream-hash-by or the affinity of an
 * Ingress reloads nothing:
 *
 *   GET    /api/wxingress/1/balancers              every rule
 *   GET    /api/wxingress/1/balancers/:upstream    one
 *   PUT    /api/wxingress/1/balancers/:upstream    set it
 *   DELETE /api/wxingress/1/balancers/:upstream    and take it away
 *
 * A rule is {"algorithm", "hashBy", "subsetSize", "backend", "cookieName",
 * "cookiePath", "cookieDomain", "cookieExpires", "cookieMaxAge",
 * "cookieSecure", "cookieSameSite", "cookieConditionalSameSiteNone",
 * "changeOnFailure"}. The algorithm is the name ingress-nginx's
 * get_implementation gives the backend - round_robin, ewma, chash,
 * chashsubset, sticky_balanced, sticky_persistent - and `backend` is
 * ingress-nginx's name of it, <namespace>-<service>-<port>, which salts the
 * persistent cookie and names the backend in every cookie as ingress-nginx
 * does, so a session pinned by ingress-nginx stays pinned here. An upstream
 * without a rule is its block's method's, as before.
 *
 * Each algorithm is the Lua one's, step for step, over the servers nginx
 * has for the upstream:
 *
 *   round_robin        each worker in turn from a random start, as
 *                      resty.roundrobin with weight 1; a server this
 *                      request already tried is passed over.
 *   ewma               two servers at random of those not yet tried, the
 *                      one with the lower decayed average of connect and
 *                      response time (Peak-EWMA, 10 s decay). The averages
 *                      are per address in shared memory, so every worker
 *                      and every later configuration has them; a server
 *                      added starts at the average of the others.
 *   chash              resty.chash's ring - 160 points a server, CRC32 of
 *                      the address and the point before - and the key of
 *                      upstream-hash-by, nginx variables in it expanded.
 *   chashsubset        the ring over sets of subsetSize servers, the last
 *                      filled from the first, and a random server of the set.
 *   sticky_balanced    the cookie's key on the ring of servers; a new
 *                      session, or a failed one with changeOnFailure, a new
 *                      random key, up to 20 tries for a server not tried.
 *   sticky_persistent  the cookie's key is md5(backend + address); a new
 *                      session a random server not tried.
 *
 * The cookie is `<key>|<md5(md5(backend) + backend)>`, with Expires and
 * Max-Age, Domain, Path, Secure (asked for, or the request came over TLS),
 * HttpOnly and SameSite as resty.cookie writes them; with
 * cookieConditionalSameSiteNone, SameSite=None is left out for the browsers
 * ingress-nginx leaves it out for.
 */

/** How many upstreams can have a rule at once. */
#define BALANCERS 4096

enum {
  BY_NGINX,
  ROUND_ROBIN,
  EWMA,
  CHASH,
  CHASH_SUBSET,
  STICKY_BALANCED,
  STICKY_PERSISTENT
};

static const char *algorithms[] = {"", "round_robin", "ewma", "chash",
                                   "chashsubset", "sticky_balanced",
                                   "sticky_persistent"};

/** One upstream's rule. `seq` is odd while it is written, as for canaries. */
typedef struct {
  unsigned seq;
  int used;
  unsigned generation;
  char upstream[128];
  int algorithm;
  char hashBy[256];
  int subsetSize;
  char backend[256];
  char backendKey[33];
  char cookieName[64];
  char cookiePath[256];
  char cookieDomain[256];
  char cookieExpires[16];
  char cookieMaxAge[16];
  char cookieSameSite[8];
  int cookieSecure;
  int cookieConditionalSameSiteNone;
  int changeOnFailure;
} balancerRule_t;

typedef struct {
  pthread_mutex_t lock;
  unsigned generation;
  balancerRule_t rules[BALANCERS];
} balancers_t;

/**
 * ingress-nginx's two shared dictionaries, balancer_ewma and
 * balancer_ewma_last_touched_at, in one table: an address's average and
 * when it was last touched.
 */
#define EWMA_ADDRESSES 16384

typedef struct {
  unsigned seq;
  int used;
  char address[64];
  double ewma;
  double touched;
} ewmaStat_t;

typedef struct {
  pthread_mutex_t lock;
  ewmaStat_t stats[EWMA_ADDRESSES];
} ewmas_t;

/** Made by the master, like the canaries: the rules and the averages outlive reloads. */
static balancers_t *balancers;
static ewmas_t *ewmas;

static int sharedLock(pthread_mutex_t *lock) {

  pthread_mutexattr_t how;

  return pthread_mutexattr_init(&how) != 0 ||
                 pthread_mutexattr_setpshared(&how, PTHREAD_PROCESS_SHARED) != 0 ||
                 pthread_mutexattr_setrobust(&how, PTHREAD_MUTEX_ROBUST) != 0 ||
                 pthread_mutex_init(lock, &how) != 0
             ? -1
             : 0;
}

static int balancersOpen(void) {

  void *rules, *stats;

  if (balancers != NULL)
    return 0;

  rules = mmap(NULL, sizeof(balancers_t), PROT_READ | PROT_WRITE,
               MAP_SHARED | MAP_ANONYMOUS, -1, 0);
  stats = mmap(NULL, sizeof(ewmas_t), PROT_READ | PROT_WRITE,
               MAP_SHARED | MAP_ANONYMOUS, -1, 0);

  if (rules == MAP_FAILED || stats == MAP_FAILED) {
    fprintf(stderr, "wxingress: no shared memory for balancers: %s\n",
            strerror(errno));
    return -1;
  }

  balancers = rules;
  ewmas = stats;

  if (sharedLock(&balancers->lock) != 0 || sharedLock(&ewmas->lock) != 0) {
    fprintf(stderr, "wxingress: no lock for the balancers\n");
    return -1;
  }

  return 0;
}

static void robustLock(pthread_mutex_t *lock) {

  if (pthread_mutex_lock(lock) == EOWNERDEAD)
    pthread_mutex_consistent(lock);
}

/** A copy of `size` bytes whose first member is the `seq` of a seqlock. */
static void seqCopy(void *into, const void *from, size_t size) {

  const unsigned *seq = from;
  unsigned before;

  do {
    before = __atomic_load_n(seq, __ATOMIC_ACQUIRE);

    while (before & 1)
      before = __atomic_load_n(seq, __ATOMIC_ACQUIRE);

    memcpy(into, from, size);
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
  } while (__atomic_load_n(seq, __ATOMIC_RELAXED) != before);
}

static unsigned fnv(const char *text) {

  unsigned hash = 2166136261u;

  for (const char *at = text; *at != 0; ++at)
    hash = (hash ^ (unsigned char)*at) * 16777619u;

  return hash;
}

/** A copy of the rule of `upstream`; zero when there is one. */
static int balancerRead(const char *upstream, balancerRule_t *into) {

  unsigned slot = fnv(upstream) % BALANCERS;

  if (balancers == NULL)
    return -1;

  for (unsigned probe = 0; probe < BALANCERS; ++probe) {

    balancerRule_t *rule = &balancers->rules[(slot + probe) % BALANCERS];

    /* the name first, without copying the rest: most upstreams have no rule */
    if (!__atomic_load_n(&rule->used, __ATOMIC_ACQUIRE) && rule->upstream[0] == 0)
      return -1;

    if (strncmp(rule->upstream, upstream, sizeof rule->upstream) != 0)
      continue;

    seqCopy(into, rule, sizeof *into);

    if (into->used && strcmp(into->upstream, upstream) == 0)
      return 0;
  }

  return -1;
}

/** Writes the rule, or takes it away with `used` 0; -1 when the table is full. */
static int balancerWrite(const balancerRule_t *given) {

  unsigned slot = fnv(given->upstream) % BALANCERS;
  balancerRule_t *found = NULL, *free = NULL;

  robustLock(&balancers->lock);

  for (unsigned probe = 0; probe < BALANCERS; ++probe) {

    balancerRule_t *rule = &balancers->rules[(slot + probe) % BALANCERS];

    if (rule->used && strcmp(rule->upstream, given->upstream) == 0) {
      found = rule;
      break;
    }

    /* a removed rule keeps its name, so a later one is still found past it */
    if (!rule->used && free == NULL)
      free = rule;

    if (!rule->used && rule->upstream[0] == 0)
      break;
  }

  if (found == NULL && given->used)
    found = free;

  if (found == NULL) {
    pthread_mutex_unlock(&balancers->lock);
    return given->used ? -1 : 0;
  }

  __atomic_add_fetch(&found->seq, 1, __ATOMIC_RELEASE);

  if (given->used) {
    unsigned seq = found->seq;
    memcpy(found, given, sizeof *found);
    found->seq = seq;
    found->generation = ++balancers->generation;
  } else {
    found->used = 0;
  }

  __atomic_add_fetch(&found->seq, 1, __ATOMIC_RELEASE);

  pthread_mutex_unlock(&balancers->lock);

  return 0;
}

/* ------------------------------------------------------- md5, crc32, time */

/** MD5 in lower-case hex, as ngx.md5 gives it. */
static void md5Hex(const char *text, char hex[33]) {

  static const unsigned shifts[64] = {
      7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
      5, 9,  14, 20, 5, 9,  14, 20, 5, 9,  14, 20, 5, 9,  14, 20,
      4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
      6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21};
  static const unsigned sines[64] = {
      0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee,
      0xf57c0faf, 0x4787c62a, 0xa8304613, 0xfd469501,
      0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be,
      0x6b901122, 0xfd987193, 0xa679438e, 0x49b40821,
      0xf61e2562, 0xc040b340, 0x265e5a51, 0xe9b6c7aa,
      0xd62f105d, 0x02441453, 0xd8a1e681, 0xe7d3fbc8,
      0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed,
      0xa9e3e905, 0xfcefa3f8, 0x676f02d9, 0x8d2a4c8a,
      0xfffa3942, 0x8771f681, 0x6d9d6122, 0xfde5380c,
      0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70,
      0x289b7ec6, 0xeaa127fa, 0xd4ef3085, 0x04881d05,
      0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665,
      0xf4292244, 0x432aff97, 0xab9423a7, 0xfc93a039,
      0x655b59c3, 0x8f0ccc92, 0xffeff47d, 0x85845dd1,
      0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1,
      0xf7537e82, 0xbd3af235, 0x2ad7d2bb, 0xeb86d391,
  };
  unsigned state[4] = {0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476};
  size_t length = strlen(text), total = ((length + 8) / 64 + 1) * 64;
  unsigned char block[64];

  for (size_t offset = 0; offset < total; offset += 64) {

    unsigned words[16], a = state[0], b = state[1], c = state[2], d = state[3];

    for (int at = 0; at < 64; ++at) {
      size_t index = offset + (size_t)at;
      block[at] = index < length ? (unsigned char)text[index]
                  : index == length ? 0x80 : 0;
    }

    if (offset + 64 == total) {
      unsigned long long bits = (unsigned long long)length * 8;
      for (int at = 0; at < 8; ++at)
        block[56 + at] = (unsigned char)(bits >> (8 * at));
    }

    for (int at = 0; at < 16; ++at)
      words[at] = (unsigned)block[at * 4] | (unsigned)block[at * 4 + 1] << 8 |
                  (unsigned)block[at * 4 + 2] << 16 | (unsigned)block[at * 4 + 3] << 24;

    for (int at = 0; at < 64; ++at) {

      unsigned f, g, rotated;

      if (at < 16) {
        f = (b & c) | (~b & d);
        g = (unsigned)at;
      } else if (at < 32) {
        f = (d & b) | (~d & c);
        g = (unsigned)(5 * at + 1) % 16;
      } else if (at < 48) {
        f = b ^ c ^ d;
        g = (unsigned)(3 * at + 5) % 16;
      } else {
        f = c ^ (b | ~d);
        g = (unsigned)(7 * at) % 16;
      }

      f += a + sines[at] + words[g];
      a = d;
      d = c;
      c = b;
      rotated = f << shifts[at] | f >> (32 - shifts[at]);
      b += rotated;
    }

    state[0] += a;
    state[1] += b;
    state[2] += c;
    state[3] += d;
  }

  for (int at = 0; at < 16; ++at)
    snprintf(hex + at * 2, 3, "%02x", (state[at / 4] >> (8 * (at % 4))) & 0xff);

  hex[32] = 0;
}

static unsigned crcTable[256];

/** CRC-32 as ngx.crc32_short has it, carried on from `crc` before its final xor. */
static unsigned crcGo(unsigned crc, const unsigned char *bytes, size_t length) {

  if (crcTable[1] == 0)
    for (unsigned at = 0; at < 256; ++at) {
      unsigned c = at;
      for (int bit = 0; bit < 8; ++bit)
        c = c & 1 ? 0xedb88320u ^ (c >> 1) : c >> 1;
      crcTable[at] = c;
    }

  while (length-- > 0)
    crc = crcTable[(crc ^ *bytes++) & 0xff] ^ (crc >> 8);

  return crc;
}

static unsigned crc32(const char *text) {
  return crcGo(0xffffffffu, (const unsigned char *)text, strlen(text)) ^ 0xffffffffu;
}

/** ngx.now(): seconds, with the milliseconds. */
static double now(void) {

  struct timespec at;

  clock_gettime(CLOCK_REALTIME, &at);

  return (double)at.tv_sec + (double)(at.tv_nsec / 1000000) / 1000.0;
}

/* ------------------------------------------------------------------- ewma */

/** The slot of `address`, with a copy of it; -1 when it has none. */
static int ewmaRead(const char *address, ewmaStat_t *into) {

  unsigned slot = fnv(address) % EWMA_ADDRESSES;

  for (unsigned probe = 0; probe < EWMA_ADDRESSES; ++probe) {

    unsigned at = (slot + probe) % EWMA_ADDRESSES;

    seqCopy(into, &ewmas->stats[at], sizeof *into);

    if (!into->used && into->address[0] == 0)
      return -1;

    if (into->used && strcmp(into->address, address) == 0)
      return (int)at;
  }

  return -1;
}

/** Sets, or with `used` 0 deletes, an address's average; under the lock. */
static void ewmaWrite(const char *address, int used, double ewma, double touched) {

  unsigned slot = fnv(address) % EWMA_ADDRESSES;
  ewmaStat_t *found = NULL, *free = NULL;

  for (unsigned probe = 0; probe < EWMA_ADDRESSES; ++probe) {

    ewmaStat_t *stat = &ewmas->stats[(slot + probe) % EWMA_ADDRESSES];

    if (stat->used && strcmp(stat->address, address) == 0) {
      found = stat;
      break;
    }

    if (!stat->used && free == NULL)
      free = stat;

    if (!stat->used && stat->address[0] == 0)
      break;
  }

  if (found == NULL && used)
    found = free;

  /* full: ingress-nginx's dictionary would drop the oldest; this drops the new one */
  if (found == NULL || strlen(address) >= sizeof found->address)
    return;

  __atomic_add_fetch(&found->seq, 1, __ATOMIC_RELEASE);

  if (used) {
    if (strcmp(found->address, address) != 0) {
      memset(found->address, 0, sizeof found->address);
      strcpy(found->address, address);
    }
    found->ewma = ewma;
    found->touched = touched;
  }

  found->used = used;

  __atomic_add_fetch(&found->seq, 1, __ATOMIC_RELEASE);
}

#define EWMA_DECAY 10.0

/**
 * e to the x for x at or below 0, without libm: nginx does not link it, and
 * a module that needs it would not load. Halved until small, a short series,
 * squared back - well inside what a weight needs.
 */
static double decay(double x) {

  int halvings = 0;
  double term = 1, sum = 1;

  if (x < -700)
    return 0;

  while (x < -0.5) {
    x /= 2;
    ++halvings;
  }

  for (int n = 1; n < 16; ++n) {
    term *= x / n;
    sum += term;
  }

  while (halvings-- > 0)
    sum *= sum;

  return sum;
}

static double decayed(double ewma, double touched, double rtt, double at) {

  double gone = at - touched;
  double weight = decay(-(gone > 0 ? gone : 0) / EWMA_DECAY);

  return ewma * weight + rtt * (1.0 - weight);
}

/** score(): the average of an address now, as if a response of no time came in. */
static double ewmaScore(const char *address) {

  ewmaStat_t stat;

  if (ewmaRead(address, &stat) < 0)
    return decayed(0, 0, 0, now());

  return decayed(stat.ewma, stat.touched, 0, now());
}

/**
 * after_balance(): a response's time into the address's average. Skipped,
 * as resty.lock with no timeout skips it, when another worker is writing.
 */
static void ewmaUpdate(const char *address, double rtt) {

  ewmaStat_t stat;
  double at = now(), ewma = 0, touched = 0;
  int locked = pthread_mutex_trylock(&ewmas->lock);

  if (locked == EOWNERDEAD)
    pthread_mutex_consistent(&ewmas->lock);
  else if (locked != 0)
    return;

  if (ewmaRead(address, &stat) >= 0) {
    ewma = stat.ewma;
    touched = stat.touched;
  }

  ewmaWrite(address, 1, decayed(ewma, touched, rtt, at), at);

  pthread_mutex_unlock(&ewmas->lock);
}

/* ------------------------------------------------- each worker's own pools */

/** A point of resty.chash's ring: its hash and the node it stands for. */
typedef struct {
  unsigned hash;
  int node;
} point_t;

#define POINTS_A_NODE 160

/**
 * What a worker derived from an upstream's servers and its rule, kept while
 * neither changes: the servers that are not down and where nginx has them,
 * the ring, the subsets, the persistent keys, the round robin's turn.
 */
typedef struct {
  char upstream[128];
  unsigned long generation;
  unsigned ruleGeneration;
  int count;
  /** For each server of the pool, its position in nginx's list, and its address. */
  int *position;
  char (*address)[64];
  point_t *ring;
  int points;
  int setSize;
  int sets;
  /** sets * setSize servers of the pool, the set n at n * setSize. */
  int *members;
  char (*keys)[33];
  unsigned turn;
} pool_t;

#define POOLS 4096

static pool_t *pools[POOLS];
static http_peer_t balanced[MOST_SERVERS];

static int pointOrder(const void *one, const void *two) {

  const point_t *a = one, *b = two;

  if (a->hash != b->hash)
    return a->hash < b->hash ? -1 : 1;

  return a->node - b->node;
}

/** resty.chash's points for the nodes named, as chash_point_init makes them. */
static point_t *ringOf(const char (*names)[64], int count, int *points) {

  point_t *ring = malloc(sizeof(point_t) * (size_t)(count > 0 ? count : 1) *
                         POINTS_A_NODE);

  *points = 0;

  if (ring == NULL)
    return NULL;

  for (int node = 0; node < count; ++node) {

    unsigned base = crc32(names[node]) ^ 0xffffffffu;
    unsigned char previous[4] = {0, 0, 0, 0};

    for (int at = 0; at < POINTS_A_NODE; ++at) {

      unsigned hash = crcGo(base, previous, 4) ^ 0xffffffffu;

      ring[*points].hash = hash;
      ring[*points].node = node;
      ++*points;

      previous[0] = (unsigned char)hash;
      previous[1] = (unsigned char)(hash >> 8);
      previous[2] = (unsigned char)(hash >> 16);
      previous[3] = (unsigned char)(hash >> 24);
    }
  }

  qsort(ring, (size_t)*points, sizeof *ring, pointOrder);

  return ring;
}

/** resty.chash's find: the node of the first point at or after the key's hash. */
static int ringFind(const pool_t *pool, const char *key) {

  unsigned hash = crc32(key);
  int low = 0, high = pool->points;

  while (low < high) {
    int middle = (low + high) / 2;
    if (pool->ring[middle].hash < hash)
      low = middle + 1;
    else
      high = middle;
  }

  return pool->ring[low < pool->points ? low : 0].node;
}

static int addressOrder(const void *one, const void *two) {
  return strcmp(((const char *)one) + sizeof(int), ((const char *)two) + sizeof(int));
}

static void poolEmpty(pool_t *pool) {

  free(pool->position);
  free(pool->address);
  free(pool->ring);
  free(pool->members);
  free(pool->keys);

  pool->position = NULL;
  pool->address = NULL;
  pool->ring = NULL;
  pool->members = NULL;
  pool->keys = NULL;
  pool->count = pool->points = pool->sets = 0;
}

/**
 * The servers of the upstream, built again for its list and rule. Sorted by
 * address, so every worker - and every replica of the controller - makes
 * the same subsets of the same list, whatever order nginx has it in.
 */
static int poolBuild(pool_t *pool, http_balance_t *ask, const balancerRule_t *rule) {

  struct {
    int position;
    char address[64];
  } *sorted;
  int listed = ask.peers(balanced, MOST_SERVERS), count = 0;

  poolEmpty(pool);

  if (listed > MOST_SERVERS)
    listed = MOST_SERVERS;

  sorted = malloc(sizeof *sorted * (size_t)(listed > 0 ? listed : 1));
  pool->position = malloc(sizeof(int) * (size_t)(listed > 0 ? listed : 1));
  pool->address = malloc(64 * (size_t)(listed > 0 ? listed : 1));

  if (sorted == NULL || pool->position == NULL || pool->address == NULL) {
    free(sorted);
    poolEmpty(pool);
    return -1;
  }

  for (int at = 0; at < listed; ++at) {

    if (balanced[at].down || strlen(balanced[at].server) >= 64)
      continue;

    sorted[count].position = at;
    strcpy(sorted[count].address, balanced[at].server);
    ++count;
  }

  qsort(sorted, (size_t)count, sizeof *sorted, addressOrder);

  for (int at = 0; at < count; ++at) {
    pool->position[at] = sorted[at].position;
    strcpy(pool->address[at], sorted[at].address);
  }

  free(sorted);
  pool->count = count;

  switch (rule->algorithm) {

  case CHASH:
  case STICKY_BALANCED:
    pool->ring = ringOf((const char (*)[64])pool->address, count, &pool->points);
    break;

  case CHASH_SUBSET: {
    char (*names)[64];

    pool->setSize = rule->subsetSize > 0 ? rule->subsetSize : 3;
    pool->sets = (count + pool->setSize - 1) / pool->setSize;
    pool->members = malloc(sizeof(int) * (size_t)(pool->sets > 0 ? pool->sets : 1) *
                           (size_t)pool->setSize);
    names = malloc(64 * (size_t)(pool->sets > 0 ? pool->sets : 1));

    if (pool->members == NULL || names == NULL) {
      free(names);
      poolEmpty(pool);
      return -1;
    }

    /* build_subset_map: the last set filled up from the first servers */
    for (int at = 0; at < pool->sets * pool->setSize; ++at)
      pool->members[at] = at % count;

    for (int set = 0; set < pool->sets; ++set)
      snprintf(names[set], 64, "set%d", set + 1);

    pool->ring = ringOf((const char (*)[64])names, pool->sets, &pool->points);
    free(names);
    break;
  }

  case STICKY_PERSISTENT:
    pool->keys = malloc(33 * (size_t)(count > 0 ? count : 1));

    if (pool->keys == NULL) {
      poolEmpty(pool);
      return -1;
    }

    /* util/nodemap.lua: md5(salt .. endpoint), the salt the backend's name */
    for (int at = 0; at < count; ++at) {
      char salted[sizeof rule->backend + 64];
      snprintf(salted, sizeof salted, "%s%s", rule->backend, pool->address[at]);
      md5Hex(salted, pool->keys[at]);
    }
    break;
  }

  if ((rule->algorithm == CHASH || rule->algorithm == STICKY_BALANCED ||
       rule->algorithm == CHASH_SUBSET) && pool->ring == NULL && count > 0) {
    poolEmpty(pool);
    return -1;
  }

  pool->turn = count > 0 ? randomUpTo((unsigned)count) - 1 : 0;

  return 0;
}

/** The worker's pool of the upstream, built again if its list or rule changed. */
static pool_t *poolOf(http_balance_t *ask, const balancerRule_t *rule) {

  unsigned slot = fnv(ask->upstream) % POOLS;
  pool_t *pool = NULL;

  for (unsigned probe = 0; probe < POOLS; ++probe) {

    pool_t **at = &pools[(slot + probe) % POOLS];

    if (*at == NULL) {
      *at = calloc(1, sizeof(pool_t));
      if (*at == NULL || strlen(ask->upstream) >= sizeof(*at)->upstream)
        return NULL;
      strcpy((*at)->upstream, ask->upstream);
      (*at)->ruleGeneration = rule->generation + 1;
    }

    if (strcmp((*at)->upstream, ask->upstream) == 0) {
      pool = *at;
      break;
    }
  }

  if (pool == NULL)
    return NULL;

  if (pool->generation != ask->generation || pool->ruleGeneration != rule->generation) {

    if (poolBuild(pool, ask, rule) != 0)
      return NULL;

    pool->generation = ask->generation;
    pool->ruleGeneration = rule->generation;
  }

  return pool;
}

/* ------------------------------------------------------------- algorithms */

static int roundRobin(http_balance_t *ask, pool_t *pool) {

  for (int step = 0; step < pool->count; ++step) {

    int at = (int)((pool->turn + (unsigned)step) % (unsigned)pool->count);

    if (!ask.tried(pool->position[at])) {
      pool->turn = (unsigned)at + 1;
      return pool->position[at];
    }
  }

  return pool->position[pool->turn++ % (unsigned)pool->count];
}

/**
 * ewma.lua's balance: of the servers this request has not tried (all of
 * them when it has tried every one), two at random, and the lower score.
 */
static int peakEwma(http_balance_t *ask, pool_t *pool) {

  static int left[MOST_SERVERS];
  int count = 0, pick = 2;
  double best;
  int chosen;

  if (pool->count == 1)
    return pool->position[0];

  for (int at = 0; at < pool->count; ++at)
    if (!ask.tried(pool->position[at]))
      left[count++] = at;

  if (count == 0)
    for (int at = 0; at < pool->count; ++at)
      left[count++] = at;

  if (count == 1)
    return pool->position[left[0]];

  /* shuffle_peers: the first two of a Fisher-Yates shuffle */
  for (int at = 0; at < pick; ++at) {
    int other = at + (int)randomUpTo((unsigned)(count - at)) - 1;
    int kept = left[at];
    left[at] = left[other];
    left[other] = kept;
  }

  chosen = left[0];
  best = ewmaScore(pool->address[chosen]);

  for (int at = 1; at < pick; ++at) {
    double score = ewmaScore(pool->address[left[at]]);
    if (score < best) {
      best = score;
      chosen = left[at];
    }
  }

  return pool->position[chosen];
}

/**
 * util.lua's parse_complex_value and generate_var_value: `$name` and
 * `${name}` are nginx variables, `\$name` and anything else is itself.
 */
static const char *hashKey(http_request_t *req, const char *pattern, char *into,
                           size_t room) {

  size_t used = 0;

  for (const char *at = pattern; *at != 0 && used + 1 < room;) {

    const char *value = NULL;
    char name[128];
    size_t length = 0;

    if (at[0] == '$' && at[1] == '{') {

      const char *end = strchr(at + 2, '}');

      if (end != NULL && end > at + 2 && (size_t)(end - at - 2) < sizeof name) {
        length = (size_t)(end - at - 2);
        memcpy(name, at + 2, length);
        name[length] = 0;
        value = req.variable(name);
        at = end + 1;
      }

    } else if (at[0] == '$') {

      while (length + 1 < sizeof name &&
             ((at[1 + length] >= 'a' && at[1 + length] <= 'z') ||
              (at[1 + length] >= 'A' && at[1 + length] <= 'Z') ||
              (at[1 + length] >= '0' && at[1 + length] <= '9') || at[1 + length] == '_'))
        ++length;

      if (length > 0) {
        memcpy(name, at + 1, length);
        name[length] = 0;
        value = req.variable(name);
        at += 1 + length;
      }
    }

    if (value != NULL) {
      while (*value != 0 && used + 1 < room)
        into[used++] = *value++;
      continue;
    }

    into[used++] = *at++;
  }

  into[used] = 0;

  return into;
}

static int consistentHash(http_balance_t *ask, pool_t *pool, const balancerRule_t *rule) {

  char key[1024];

  hashKey(ask->req, rule->hashBy, key, sizeof key);

  if (rule->algorithm == CHASH)
    return pool->position[ringFind(pool, key)];

  /* chashsubset: the set of the key, and a server of it at random */
  {
    int set = ringFind(pool, key);
    int member = pool->members[set * pool->setSize +
                               (int)randomUpTo((unsigned)pool->setSize) - 1];
    return pool->position[member];
  }
}

/** same_site.lua: the browsers that reject SameSite=None. */
static int sameSiteNoneCompatible(const char *agent) {

  if (agent[0] == 0)
    return 1;

  if (strstr(agent, "Chrome/4") || strstr(agent, "Chrome/5") ||
      strstr(agent, "Chrome/6") || strstr(agent, "CPU iPhone OS 12") ||
      strstr(agent, "iPad; CPU OS 12"))
    return 0;

  if (strstr(agent, "Macintosh") && strstr(agent, "Intel Mac OS X 10_14") &&
      strstr(agent, "Safari") && !strstr(agent, "Chrome"))
    return 0;

  return 1;
}

/** ngx.cookie_time: a two-digit year until 2037, as nginx writes it. */
static void cookieTime(time_t at, char *into, size_t room) {

  static const char *days[] = {"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"};
  static const char *months[] = {"Jan", "Feb", "Mar", "Apr", "May", "Jun",
                                 "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"};
  struct tm tm;
  int year;

  gmtime_r(&at, &tm);
  year = tm.tm_year + 1900;

  snprintf(into, room, year > 2037 ? "%s, %02d-%s-%d %02d:%02d:%02d GMT"
                                   : "%s, %02d-%s-%02d %02d:%02d:%02d GMT",
           days[tm.tm_wday], tm.tm_mday, months[tm.tm_mon],
           year > 2037 ? year : year % 100, tm.tm_hour, tm.tm_min, tm.tm_sec);
}

/** sticky.lua's set_cookie, written as resty.cookie's bake writes it. */
static void stickyCookie(http_balance_t *ask, const balancerRule_t *rule,
                         const char *key) {

  char cookie[1024], expires[64];
  const char *sameSite = rule->cookieSameSite;
  size_t used;

  if (sameSite[0] != 0 && rule->cookieConditionalSameSiteNone &&
      strcmp(sameSite, "None") == 0 &&
      !sameSiteNoneCompatible(ask->req.header("user-agent")))
    sameSite = "";

  used = (size_t)snprintf(cookie, sizeof cookie, "%s=%s|%s", rule->cookieName, key,
                          rule->backendKey);

  if (rule->cookieExpires[0] != 0) {
    cookieTime(time(NULL) + atol(rule->cookieExpires), expires, sizeof expires);
    used += (size_t)snprintf(cookie + used, sizeof cookie - used, "; Expires=%s", expires);
  }

  if (rule->cookieMaxAge[0] != 0)
    used += (size_t)snprintf(cookie + used, sizeof cookie - used, "; Max-Age=%s",
                             rule->cookieMaxAge);

  if (rule->cookieDomain[0] != 0)
    used += (size_t)snprintf(cookie + used, sizeof cookie - used, "; Domain=%s",
                             rule->cookieDomain);

  used += (size_t)snprintf(cookie + used, sizeof cookie - used, "; Path=%s",
                           rule->cookiePath[0] != 0 ? rule->cookiePath : "/");

  /* session-cookie-secure, or `ngx.var.https == "on"` where it is not set */
  if (rule->cookieSecure || ask->req->secure)
    used += (size_t)snprintf(cookie + used, sizeof cookie - used, "; Secure");

  used += (size_t)snprintf(cookie + used, sizeof cookie - used, "; HttpOnly");

  if (strcmp(sameSite, "Strict") == 0 || strcmp(sameSite, "Lax") == 0 ||
      strcmp(sameSite, "None") == 0)
    snprintf(cookie + used, sizeof cookie - used, "; SameSite=%s", sameSite);

  ask.header("Set-Cookie", cookie);
}

/** The first part of the affinity cookie, the key, into `into`; NULL without one. */
static const char *stickyKey(http_request_t *req, const balancerRule_t *rule,
                             char *into, size_t room, const char **backend) {

  char *bar;

  *backend = "";

  if (cookieOf(req, rule->cookieName, into, room) == NULL)
    return NULL;

  bar = strchr(into, '|');

  if (bar != NULL) {
    *bar = 0;
    *backend = bar + 1;
  }

  /* split_string of "" or "|..." has no first part */
  return into[0] != 0 ? into : NULL;
}

/**
 * sticky.lua's balance, for both modes: the server the cookie names, unless
 * there is none or - with changeOnFailure - this is a retry; then a new one
 * the request has not tried, and a cookie for it.
 */
static int sticky(http_balance_t *ask, pool_t *pool, const balancerRule_t *rule) {

  char cookie[256], key[64];
  const char *backend;
  const char *given = stickyKey(ask->req, rule, cookie, sizeof cookie, &backend);
  int fromCookie = -1, member = -1;

  if (given != NULL) {
    if (rule->algorithm == STICKY_BALANCED) {
      fromCookie = ringFind(pool, given);
    } else {
      for (int at = 0; at < pool->count; ++at)
        if (strcmp(pool->keys[at], given) == 0)
          fromCookie = at;
    }
  }

  if (!((ask->attempt > 1 && rule->changeOnFailure) || fromCookie < 0))
    return pool->position[fromCookie];

  if (rule->algorithm == STICKY_BALANCED) {

    /* sticky_balanced.lua: a random key, twenty times at most */
    for (int at = 1; at <= 20 && member < 0; ++at) {
      int found;

      snprintf(key, sizeof key, "%.14g.%d.%u", now() + at, (int)getpid(),
               randomUpTo(999999));
      found = ringFind(pool, key);

      if (!ask.tried(pool->position[found]))
        member = found;
    }

  } else {

    /* nodemap.lua's random_except: any server not tried */
    static int left[MOST_SERVERS];
    int count = 0;

    for (int at = 0; at < pool->count; ++at)
      if (!ask.tried(pool->position[at]))
        left[count++] = at;

    if (count > 0) {
      member = left[randomUpTo((unsigned)count) - 1];
      snprintf(key, sizeof key, "%s", pool->keys[member]);
    }
  }

  /* nothing left to try: the method's choice, where ingress-nginx sets no peer */
  if (member < 0)
    return HTTP_BALANCE_NGINX;

  stickyCookie(ask, rule, key);

  return pool->position[member];
}

/** Whether an upstream has a rule of its own: the others are nginx's, at no cost. */
static int balancesFor(const char *upstream) {

  balancerRule_t rule;

  return balancerRead(upstream, &rule) == 0 && rule.algorithm != BY_NGINX;
}

static int balance(http_balance_t *ask) {

  static balancerRule_t rule;
  pool_t *pool;

  if (balancerRead(ask->upstream, &rule) != 0 || rule.algorithm == BY_NGINX)
    return HTTP_BALANCE_NGINX;

  pool = poolOf(ask, &rule);

  if (pool == NULL || pool->count == 0)
    return HTTP_BALANCE_NGINX;

  switch (rule.algorithm) {
  case ROUND_ROBIN:
    return roundRobin(ask, pool);
  case EWMA:
    return peakEwma(ask, pool);
  case CHASH:
  case CHASH_SUBSET:
    return consistentHash(ask, pool, &rule);
  case STICKY_BALANCED:
  case STICKY_PERSISTENT:
    return sticky(ask, pool, &rule);
  default:
    return HTTP_BALANCE_NGINX;
  }
}

/**
 * ewma.lua's after_balance: connect and response time of a try into the
 * average of its address. Only for a try that did not fail: a refused
 * connection takes no time, and would make a dead server the fastest.
 */
static void balancedTry(http_balanced_t *done) {

  static balancerRule_t rule;
  double rtt = 0;

  if (done->failed || balancerRead(done->upstream, &rule) != 0 ||
      rule.algorithm != EWMA)
    return;

  if (done->connectMs > 0)
    rtt += (double)done->connectMs / 1000.0;

  if (done->responseMs > 0)
    rtt += (double)done->responseMs / 1000.0;

  ewmaUpdate(done->server, rtt);
}

/**
 * Whether the request's affinity cookie names the backend of `upstream`
 * (sticky.lua's is_affinitized), for the canary.
 */
static int affinitized(http_request_t *req, const char *upstream) {

  static balancerRule_t rule;
  char cookie[256];
  const char *backend;

  if (balancerRead(upstream, &rule) != 0 ||
      (rule.algorithm != STICKY_BALANCED && rule.algorithm != STICKY_PERSISTENT))
    return 0;

  stickyKey(req, &rule, cookie, sizeof cookie, &backend);

  return strcmp(backend, rule.backendKey) == 0;
}

/**
 * sync() of ewma.lua for a server added or removed through the upstream API:
 * a removed address loses its average, an added one starts at the average
 * of the upstream's others, so it is not flooded for having none.
 */
static void ewmaServers(const char *upstream, const char *address, int added) {

  static balancerRule_t rule;
  ewmaStat_t stat;
  double total = 0;
  int known = 0, count;

  if (balancers == NULL || balancerRead(upstream, &rule) != 0 || rule.algorithm != EWMA)
    return;

  robustLock(&ewmas->lock);

  if (!added) {
    ewmaWrite(address, 0, 0, 0);
    pthread_mutex_unlock(&ewmas->lock);
    return;
  }

  count = http.peers(upstream, balanced, MOST_SERVERS);

  for (int at = 0; at < count && at < MOST_SERVERS; ++at)
    if (ewmaRead(balanced[at].server, &stat) >= 0) {
      total += stat.ewma;
      ++known;
    }

  if (known > 0)
    ewmaWrite(address, 1, total / known, now());

  pthread_mutex_unlock(&ewmas->lock);
}

/* ------------------------------------------------------------- the API */

static size_t balancerWritten(char *into, size_t room, const balancerRule_t *rule) {

  int length = snprintf(
      into, room,
      "{\"upstream\":\"%s\",\"algorithm\":\"%s\",\"hashBy\":\"%s\",\"subsetSize\":%d,"
      "\"backend\":\"%s\",\"cookieName\":\"%s\",\"cookiePath\":\"%s\","
      "\"cookieDomain\":\"%s\",\"cookieExpires\":\"%s\",\"cookieMaxAge\":\"%s\","
      "\"cookieSecure\":%s,\"cookieSameSite\":\"%s\","
      "\"cookieConditionalSameSiteNone\":%s,\"changeOnFailure\":%s}",
      rule->upstream, algorithms[rule->algorithm], rule->hashBy, rule->subsetSize,
      rule->backend, rule->cookieName, rule->cookiePath, rule->cookieDomain,
      rule->cookieExpires, rule->cookieMaxAge, rule->cookieSecure ? "true" : "false",
      rule->cookieSameSite, rule->cookieConditionalSameSiteNone ? "true" : "false",
      rule->changeOnFailure ? "true" : "false");

  if (length < 0)
    return 0;

  return (size_t)length < room ? (size_t)length : room;
}

/** Whether text is a number of seconds, or nothing. */
static int secondsText(const char *text) {

  for (const char *at = text; *at != 0; ++at)
    if (*at < '0' || *at > '9')
      return 0;

  return strlen(text) < 12;
}

static const char *balancerReadJson(json_t said, balancerRule_t *rule) {

  int count = said.count();

  if (strcmp(said.kind(), "object") != 0)
    return "the body is a JSON object";

  for (int at = 0; at < count; ++at) {

    const char *key = said.keyAt(at);
    json_t value = said.get(key);
    const char *text = value.text();

    struct {
      const char *name;
      char *into;
      size_t room;
    } texts[] = {
        {"hashBy", rule->hashBy, sizeof rule->hashBy},
        {"backend", rule->backend, sizeof rule->backend},
        {"cookieName", rule->cookieName, sizeof rule->cookieName},
        {"cookiePath", rule->cookiePath, sizeof rule->cookiePath},
        {"cookieDomain", rule->cookieDomain, sizeof rule->cookieDomain},
        {"cookieExpires", rule->cookieExpires, sizeof rule->cookieExpires},
        {"cookieMaxAge", rule->cookieMaxAge, sizeof rule->cookieMaxAge},
        {"cookieSameSite", rule->cookieSameSite, sizeof rule->cookieSameSite},
    };
    int matched = 0;

    for (size_t field = 0; field < sizeof texts / sizeof texts[0]; ++field) {

      if (strcmp(key, texts[field].name) != 0)
        continue;

      if (!textual(text, texts[field].room, 0) || strchr(text, ';') != NULL)
        return "a text of the rule is too long or has a quote or a semicolon";

      strcpy(texts[field].into, text);
      matched = 1;
    }

    if (matched)
      continue;

    if (strcmp(key, "algorithm") == 0) {

      rule->algorithm = -1;

      for (int known = 1; known <= STICKY_PERSISTENT; ++known)
        if (strcmp(text, algorithms[known]) == 0)
          rule->algorithm = known;

      if (rule->algorithm < 0)
        return "\\\"algorithm\\\" is round_robin, ewma, chash, chashsubset, "
               "sticky_balanced or sticky_persistent";

    } else if (strcmp(key, "subsetSize") == 0) {
      if (value.number() < 0 || value.number() > 1024)
        return "\\\"subsetSize\\\" is 0 to 1024";
      rule->subsetSize = (int)value.number();
    } else if (strcmp(key, "cookieSecure") == 0) {
      rule->cookieSecure = value.truth();
    } else if (strcmp(key, "cookieConditionalSameSiteNone") == 0) {
      rule->cookieConditionalSameSiteNone = value.truth();
    } else if (strcmp(key, "changeOnFailure") == 0) {
      rule->changeOnFailure = value.truth();
    } else if (strcmp(key, "upstream") != 0) {
      return "unknown parameter";
    }
  }

  if (rule->algorithm <= BY_NGINX)
    return "\\\"algorithm\\\" is required";

  if ((rule->algorithm == CHASH || rule->algorithm == CHASH_SUBSET) && rule->hashBy[0] == 0)
    return "\\\"hashBy\\\" is required for chash and chashsubset";

  if ((rule->algorithm == STICKY_BALANCED || rule->algorithm == STICKY_PERSISTENT) &&
      (rule->cookieName[0] == 0 || strchr(rule->cookieName, '=') != NULL ||
       strchr(rule->cookieName, ' ') != NULL))
    return "\\\"cookieName\\\" is the name of a cookie";

  if (!secondsText(rule->cookieExpires) || !secondsText(rule->cookieMaxAge))
    return "\\\"cookieExpires\\\" and \\\"cookieMaxAge\\\" are seconds";

  if (strchr(rule->cookiePath, ' ') != NULL || strchr(rule->cookieDomain, ' ') != NULL)
    return "a cookie's path and domain have no spaces";

  /* sticky.lua's backend_key: ngx.md5(ngx.md5(backend.name) .. backend.name) */
  {
    char inner[33], salted[sizeof rule->backend + 33];
    const char *name = rule->backend[0] != 0 ? rule->backend : rule->upstream;

    md5Hex(name, inner);
    snprintf(salted, sizeof salted, "%s%s", inner, name);
    md5Hex(salted, rule->backendKey);

    if (rule->backend[0] == 0)
      strcpy(rule->backend, rule->upstream);
  }

  return NULL;
}

static http_response_t listBalancers(http_request_t *req) {

  size_t used = 0;
  int first = 1;

  answer[used++] = '[';

  for (int at = 0; at < BALANCERS && used + 2048 < sizeof answer; ++at) {

    balancerRule_t rule;

    if (!balancers->rules[at].used ||
        balancerRead(balancers->rules[at].upstream, &rule) != 0)
      continue;

    if (!first)
      answer[used++] = ',';

    first = 0;
    used += balancerWritten(answer + used, sizeof answer - used - 2, &rule);
  }

  answer[used++] = ']';
  answer[used] = 0;

  return req.reply(200).json(answer);
}

static http_response_t showBalancer(http_request_t *req) {

  balancerRule_t rule;

  if (balancerRead(req.param("upstream"), &rule) != 0)
    return failed(req, 404, "no balancer for that upstream", "BalancerNotFound");

  balancerWritten(answer, sizeof answer, &rule);

  return req.reply(200).json(answer);
}

static http_response_t setBalancer(http_request_t *req) {

  json_t said = req.readJson();
  balancerRule_t rule;
  const char *wrong;
  const char *upstream = req.param("upstream");

  memset(&rule, 0, sizeof rule);

  if (said._refused != NULL) {
    said.release();
    return failed(req, 415, "the body is not JSON", "BalancerFormatError");
  }

  if (!textual(upstream, sizeof rule.upstream, 0) || upstream[0] == 0) {
    said.release();
    return failed(req, 400, "not an upstream's name", "BalancerFormatError");
  }

  strcpy(rule.upstream, upstream);
  rule.used = 1;

  wrong = balancerReadJson(said, &rule);
  said.release();

  if (wrong != NULL)
    return failed(req, 400, wrong, "BalancerFormatError");

  if (balancerWrite(&rule) != 0)
    return failed(req, 507, "the balancer table is full", "BalancerTableFull");

  return showBalancer(req);
}

static http_response_t removeBalancer(http_request_t *req) {

  balancerRule_t rule;

  if (balancerRead(req.param("upstream"), &rule) != 0)
    return failed(req, 404, "no balancer for that upstream", "BalancerNotFound");

  rule.used = 0;
  balancerWrite(&rule);

  return req.reply(200).json("{}");
}

/** The average of each address, for looking at: GET /api/wxingress/1/ewma. */
static http_response_t listEwma(http_request_t *req) {

  size_t used = 0;
  int first = 1;

  answer[used++] = '[';

  for (int at = 0; at < EWMA_ADDRESSES && used + 256 < sizeof answer; ++at) {

    ewmaStat_t stat;

    seqCopy(&stat, &ewmas->stats[at], sizeof stat);

    if (!stat.used)
      continue;

    used += (size_t)snprintf(answer + used, sizeof answer - used,
                             "%s{\"address\":\"%s\",\"ewma\":%.6f,\"touched\":%.3f,"
                             "\"score\":%.6f}",
                             first ? "" : ",", stat.address, stat.ewma, stat.touched,
                             decayed(stat.ewma, stat.touched, 0, now()));
    first = 0;
  }

  answer[used++] = ']';
  answer[used] = 0;

  return req.reply(200).json(answer);
}

/* ------------------------------------------------------------ certificates */

/** How many names can have a certificate at once. */
#define CERTIFICATE_NAMES 32768

/**
 * Room for the PEM of all of them. Mapped, not used: a page is only taken
 * when a certificate is written to it, so this is a ceiling - ingress-nginx's
 * certificate_data dictionary is 20 MB.
 */
#define CERTIFICATE_HEAP ((size_t)128 << 20)

/** The most one name's certificate, chain and key may be together. */
#define CERTIFICATE_MOST (64 << 10)

/**
 * One name. `seq` is odd while it is written, as for canaries. `generation`
 * is new for every certificate set, across all names, so a worker that
 * parsed generation 7 for a slot knows it still has the right one.
 */
typedef struct {
  unsigned seq;
  int used;
  unsigned long long generation;
  char name[256];
  /** Where its PEM is in the heap: the certificate, a 0, the key, a 0. */
  size_t at;
  unsigned certificateLength;
  unsigned keyLength;
} certificate_t;

/** In front of each PEM in the heap, so the heap can be walked. */
typedef struct {
  unsigned slot;
  unsigned size;
} certificateBlock_t;

typedef struct {
  pthread_mutex_t lock;
  unsigned long long generation;
  /** The first free byte of the heap. */
  size_t top;
  /** Bytes below `top` that no name has any more. */
  size_t garbage;
  certificate_t names[CERTIFICATE_NAMES];
} certificates_t;

/** Made by the master, like the canaries, and outliving every reload. */
static certificates_t *certificates;
static char *certificateHeap;

static int certificatesOpen(void) {

  pthread_mutexattr_t how;
  void *mapped;

  if (certificates != NULL)
    return 0;

  mapped = mmap(NULL, sizeof(certificates_t) + CERTIFICATE_HEAP,
                PROT_READ | PROT_WRITE, MAP_SHARED | MAP_ANONYMOUS | MAP_NORESERVE,
                -1, 0);

  if (mapped == MAP_FAILED) {
    fprintf(stderr, "wxingress: no shared memory for certificates: %s\n",
            strerror(errno));
    return -1;
  }

  certificates = mapped;
  certificateHeap = (char *)mapped + sizeof(certificates_t);

  if (pthread_mutexattr_init(&how) != 0 ||
      pthread_mutexattr_setpshared(&how, PTHREAD_PROCESS_SHARED) != 0 ||
      pthread_mutexattr_setrobust(&how, PTHREAD_MUTEX_ROBUST) != 0 ||
      pthread_mutex_init(&certificates->lock, &how) != 0) {
    fprintf(stderr, "wxingress: no lock for the certificates\n");
    return -1;
  }

  return 0;
}

static void certificatesLock(void) {

  if (pthread_mutex_lock(&certificates->lock) == EOWNERDEAD)
    pthread_mutex_consistent(&certificates->lock);
}

static unsigned certificateSlotOf(const char *name) {

  unsigned hash = 2166136261u;

  for (const char *at = name; *at != 0; ++at)
    hash = (hash ^ (unsigned char)*at) * 16777619u;

  return hash % CERTIFICATE_NAMES;
}

/** A copy of one slot, read without the lock. */
static void certificateCopy(unsigned slot, certificate_t *into) {

  certificate_t *entry = &certificates->names[slot];
  unsigned before;

  do {
    before = __atomic_load_n(&entry->seq, __ATOMIC_ACQUIRE);

    while (before & 1)
      before = __atomic_load_n(&entry->seq, __ATOMIC_ACQUIRE);

    memcpy(into, entry, sizeof *into);
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
  } while (__atomic_load_n(&entry->seq, __ATOMIC_RELAXED) != before);
}

/** The slot of `name`, with a copy of it in `into`; -1 when it has none. */
static int certificateRead(const char *name, certificate_t *into) {

  unsigned slot = certificateSlotOf(name);

  for (unsigned probe = 0; probe < CERTIFICATE_NAMES; ++probe) {

    unsigned at = (slot + probe) % CERTIFICATE_NAMES;

    certificateCopy(at, into);

    if (!into->used && into->name[0] == 0)
      return -1; /* an empty slot ends the probe */

    if (into->used && strcmp(into->name, name) == 0)
      return (int)at;
  }

  return -1;
}

static size_t certificateBlockSize(unsigned certificateLength, unsigned keyLength) {

  size_t size = sizeof(certificateBlock_t) + certificateLength + 1 + keyLength + 1;

  return (size + 7) & ~(size_t)7;
}

static void certificateWriting(certificate_t *entry) {
  __atomic_add_fetch(&entry->seq, 1, __ATOMIC_RELEASE);
}

/**
 * Every PEM a name still has moved down over what none has, under the lock.
 * Readers of a slot without the lock read its generation, never its PEM, so
 * nothing they read moves under them.
 */
static void certificatesCompact(void) {

  size_t read = 0, written = 0;

  while (read < certificates->top) {

    certificateBlock_t block;
    certificate_t *entry;

    memcpy(&block, certificateHeap + read, sizeof block);
    entry = &certificates->names[block.slot];

    if (entry->used && entry->at == read + sizeof block) {

      if (written != read) {
        memmove(certificateHeap + written, certificateHeap + read, block.size);
        certificateWriting(entry);
        entry->at = written + sizeof block;
        certificateWriting(entry);
      }

      written += block.size;
    }

    read += block.size;
  }

  certificates->top = written;
  certificates->garbage = 0;
}

/**
 * Sets the certificate of `name`, or takes it away with `certificate` NULL.
 * 0; -1 when there is no room for the name, -2 when there is none for its
 * PEM, -3 when there was nothing to take away.
 */
static int certificateWrite(const char *name, const char *certificate,
                            const char *key) {

  unsigned slot = certificateSlotOf(name);
  certificate_t *found = NULL;
  certificate_t *free = NULL;
  unsigned certificateLength = 0, keyLength = 0;
  size_t size = 0;

  certificatesLock();

  for (unsigned probe = 0; probe < CERTIFICATE_NAMES; ++probe) {

    certificate_t *entry = &certificates->names[(slot + probe) % CERTIFICATE_NAMES];

    if (entry->used && strcmp(entry->name, name) == 0) {
      found = entry;
      break;
    }

    /* a removed name keeps its text, so a later one is still found past it */
    if (!entry->used && free == NULL)
      free = entry;

    if (!entry->used && entry->name[0] == 0)
      break;
  }

  if (certificate == NULL) {

    if (found == NULL) {
      pthread_mutex_unlock(&certificates->lock);
      return -3;
    }

    certificateWriting(found);
    found->used = 0;
    certificateWriting(found);

    certificates->garbage +=
        certificateBlockSize(found->certificateLength, found->keyLength);

    pthread_mutex_unlock(&certificates->lock);
    return 0;
  }

  if (found == NULL)
    found = free;

  if (found == NULL) {
    pthread_mutex_unlock(&certificates->lock);
    return -1;
  }

  certificateLength = (unsigned)strlen(certificate);
  keyLength = (unsigned)strlen(key);
  size = certificateBlockSize(certificateLength, keyLength);

  if (certificates->top + size > CERTIFICATE_HEAP && certificates->garbage > 0)
    certificatesCompact();

  if (certificates->top + size > CERTIFICATE_HEAP) {
    pthread_mutex_unlock(&certificates->lock);
    return -2;
  }

  {
    certificateBlock_t block = {(unsigned)(found - certificates->names), (unsigned)size};
    char *to = certificateHeap + certificates->top;

    memcpy(to, &block, sizeof block);
    memcpy(to + sizeof block, certificate, certificateLength + 1);
    memcpy(to + sizeof block + certificateLength + 1, key, keyLength + 1);
  }

  if (found->used)
    certificates->garbage +=
        certificateBlockSize(found->certificateLength, found->keyLength);

  certificateWriting(found);
  if (strcmp(found->name, name) != 0) {
    memset(found->name, 0, sizeof found->name);
    strcpy(found->name, name);
  }
  found->used = 1;
  found->at = certificates->top + sizeof(certificateBlock_t);
  found->certificateLength = certificateLength;
  found->keyLength = keyLength;
  found->generation = ++certificates->generation;
  certificateWriting(found);

  certificates->top += size;

  pthread_mutex_unlock(&certificates->lock);

  return 0;
}

/**
 * The PEM of a slot, copied out under the lock - certificate at `into`, key
 * after it - if the slot still has `generation`. 0, or -1 when it changed.
 */
static int certificatePem(unsigned slot, unsigned long long generation,
                          char *into, size_t room, const char **key) {

  certificate_t *entry = &certificates->names[slot];
  int done = -1;

  certificatesLock();

  if (entry->used && entry->generation == generation &&
      (size_t)entry->certificateLength + entry->keyLength + 2 <= room) {
    memcpy(into, certificateHeap + entry->at,
           (size_t)entry->certificateLength + entry->keyLength + 2);
    *key = into + entry->certificateLength + 1;
    done = 0;
  }

  pthread_mutex_unlock(&certificates->lock);

  return done;
}

/**
 * The pair each worker parsed for each slot, and the generation it was
 * parsed for. A handshake whose name's slot still has that generation is
 * answered with the pair - no PEM read, hashed or parsed.
 */
static struct {
  unsigned long long generation;
  http_tlsPair_t pair;
} parsed[CERTIFICATE_NAMES];

static char pem[CERTIFICATE_MOST];

/**
 * The slot whose certificate the name has, as certificate.lua's
 * get_pem_cert_uid finds it: the name, then the wildcard one level above it
 * (the first label replaced by `*`, where there is a first label and a dot
 * after it). -1 for neither.
 */
static int certificateFor(const char *name, certificate_t *found) {

  const char *dot = strchr(name, '.');
  char wildcard[256];
  int slot = certificateRead(name, found);

  if (slot >= 0 || dot == NULL || dot == name ||
      strlen(dot) + 2 > sizeof wildcard)
    return slot;

  wildcard[0] = '*';
  strcpy(wildcard + 1, dot);

  return certificateRead(wildcard, found);
}

/**
 * The certificate of a handshake (certificate.lua's call): the client's name,
 * or `_` when it sent none; its certificate or its wildcard's; else `_`'s;
 * else what the configuration has - for ingress-nginx the fake certificate,
 * here the default server's. Never waits, so it needs no patched nginx.
 */
static http_tlsAnswer_t certificateOf(http_tlsHello_t *hello) {

  char name[256];
  size_t length = strlen(hello->serverName);
  certificate_t found;
  const char *key;
  http_tlsPair_t pair;
  int slot;

  if (length >= sizeof name)
    length = sizeof name - 1;

  memcpy(name, hello->serverName, length);
  name[length] = 0;

  /* the name in lower case already; a trailing dot is not part of it */
  if (length > 0 && name[length - 1] == '.')
    name[--length] = 0;

  if (length == 0)
    strcpy(name, "_");

  slot = -1;

  /* the names bound to the port first: the name's, its wildcard's, the port's */
  if (hello->localPort > 0) {
    char bound[sizeof name + 16];

    snprintf(bound, sizeof bound, "%s:%d", name, hello->localPort);
    slot = certificateFor(bound, &found);

    if (slot < 0) {
      snprintf(bound, sizeof bound, "_:%d", hello->localPort);
      slot = certificateRead(bound, &found);
    }
  }

  if (slot < 0)
    slot = certificateFor(name, &found);

  if (slot < 0 && strcmp(name, "_") != 0)
    slot = certificateRead("_", &found);

  if (slot < 0)
    return hello.keep();

  if (parsed[slot].generation == found.generation &&
      http.tlsHeld(parsed[slot].pair))
    return hello.usePair(parsed[slot].pair);

  if (certificatePem((unsigned)slot, found.generation, pem, sizeof pem, &key) != 0)
    return hello.keep(); /* it changed this instant; the next handshake has it */

  pair = http.tlsLoad(pem, key);

  if (pair.generation == 0) {
    fprintf(stderr, "wxingress: the certificate of \"%s\" could not be read; "
                    "the default one is used\n", found.name);
    return hello.keep();
  }

  parsed[slot].generation = found.generation;
  parsed[slot].pair = pair;

  return hello.usePair(pair);
}

/**
 * A name the API takes: a host in lower case - letters, digits, `-`, `.` -
 * optionally `*.` in front of one, or `_`. Upper case is lowered, as
 * certificate.lua lowers the client's name.
 */
static int certificateName(const char *escaped, char *into, size_t room) {

  char given[256];
  size_t length = 0;

  /* the name is a segment of the path, and a client may escape the `*` */
  for (const char *at = escaped; *at != 0; ++at) {

    unsigned value;

    if (length + 1 >= sizeof given)
      return 0;

    if (*at == '%' && sscanf(at + 1, "%2x", &value) == 1 && value >= 0x20 &&
        value < 0x7f) {
      given[length++] = (char)value;
      at += 2;
    } else {
      given[length++] = *at;
    }
  }

  given[length] = 0;

  if (length == 0 || length >= room)
    return 0;

  /* a port it is bound to: `:` and one to five digits at the end */
  {
    char *colon = strrchr(given, ':');

    if (colon != NULL) {
      size_t digits = strlen(colon + 1);

      if (digits == 0 || digits > 5 || strspn(colon + 1, "0123456789") != digits)
        return 0;
    }

    if (strcmp(given, "_") == 0 ||
        (colon != NULL && colon == given + 1 && given[0] == '_')) {
      strcpy(into, given);
      return 1;
    }

    if (colon != NULL)
      length = (size_t)(colon - given);
  }

  for (size_t at = 0; at < length; ++at) {

    char c = given[at];

    if (c >= 'A' && c <= 'Z')
      c = (char)(c - 'A' + 'a');

    if (!((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' ||
          c == '.' || (c == '*' && at == 0 && given[1] == '.')))
      return 0;

    into[at] = c;
  }

  into[length] = 0;

  if (into[length - 1] == '.' || strstr(into, "..") != NULL)
    return 0;

  /* and the port again */
  strcat(into, given + length);
  return 1;
}

/** Text as a JSON string's inside: PEM has newlines, nothing else to escape. */
static size_t pemInJson(char *into, size_t room, const char *text) {

  size_t used = 0;

  for (const char *at = text; *at != 0 && used + 3 < room; ++at) {
    if (*at == '\n') {
      into[used++] = '\\';
      into[used++] = 'n';
    } else if (*at == '\r') {
      into[used++] = '\\';
      into[used++] = 'r';
    } else if (*at == '"' || *at == '\\') {
      into[used++] = '\\';
      into[used++] = *at;
    } else if ((unsigned char)*at >= 0x20) {
      into[used++] = *at;
    }
  }

  into[used] = 0;

  return used;
}

/** Room for every name of the list, which the answer buffer is not. */
static char named[CERTIFICATE_NAMES * 300 + 16];

static http_response_t listCertificates(http_request_t *req) {

  size_t used = 0;
  int first = 1;

  named[used++] = '[';

  for (unsigned at = 0; at < CERTIFICATE_NAMES; ++at) {

    certificate_t entry;

    certificateCopy(at, &entry);

    if (!entry.used)
      continue;

    used += (size_t)snprintf(named + used, sizeof named - used,
                             "%s{\"name\":\"%s\",\"generation\":%llu}",
                             first ? "" : ",", entry.name, entry.generation);
    first = 0;
  }

  named[used++] = ']';
  named[used] = 0;

  return req.reply(200).json(named);
}

static http_response_t showCertificate(http_request_t *req) {

  char name[256];
  certificate_t entry;
  const char *key;
  int slot;
  size_t used;

  if (!certificateName(req.param("name"), name, sizeof name) ||
      (slot = certificateRead(name, &entry)) < 0 ||
      certificatePem((unsigned)slot, entry.generation, pem, sizeof pem, &key) != 0)
    return failed(req, 404, "no certificate for that name", "CertificateNotFound");

  used = (size_t)snprintf(answer, sizeof answer,
                          "{\"name\":\"%s\",\"generation\":%llu,\"certificate\":\"",
                          entry.name, entry.generation);
  used += pemInJson(answer + used, sizeof answer - used - 3, pem);
  strcpy(answer + used, "\"}");

  return req.reply(200).json(answer);
}

static http_response_t setCertificate(http_request_t *req) {

  json_t said = req.readJson();
  char name[256];
  const char *certificate = NULL;
  const char *key = NULL;
  http_tlsAnswer_t trial;
  int count, done;

  if (said._refused != NULL) {
    said.release();
    return failed(req, 415, "the body is not JSON", "CertificateFormatError");
  }

  if (!certificateName(req.param("name"), name, sizeof name)) {
    said.release();
    return failed(req, 400, "not a host, a wildcard or _", "CertificateFormatError");
  }

  if (strcmp(said.kind(), "object") != 0) {
    said.release();
    return failed(req, 400, "the body is a JSON object", "CertificateFormatError");
  }

  count = said.count();

  for (int at = 0; at < count; ++at) {

    const char *field = said.keyAt(at);
    json_t value = said.get(field);

    if (strcmp(field, "certificate") == 0 && strcmp(value.kind(), "string") == 0)
      certificate = value.text();
    else if (strcmp(field, "key") == 0 && strcmp(value.kind(), "string") == 0)
      key = value.text();
    else if (strcmp(field, "name") != 0) {
      said.release();
      return failed(req, 400, "unknown parameter", "CertificateFormatError");
    }
  }

  if (certificate == NULL || key == NULL) {
    said.release();
    return failed(req, 400, "\\\"certificate\\\" and \\\"key\\\" are PEM",
                  "CertificateFormatError");
  }

  if (strlen(certificate) + strlen(key) + 2 > CERTIFICATE_MOST) {
    said.release();
    return failed(req, 413, "the certificate and key are too long",
                  "CertificateFormatError");
  }

  /* parsed here once, so that what no worker could use is never stored */
  trial = http.tlsUse(certificate, key);

  if (trial.kind != HTTP_TLS_USE) {
    said.release();
    return failed(req, 400, trial.failed != NULL ? trial.failed : "not a certificate and its key",
                  "CertificateFormatError");
  }

  done = certificateWrite(name, certificate, key);
  said.release();

  if (done == -1)
    return failed(req, 507, "the certificate table is full", "CertificateTableFull");

  if (done == -2)
    return failed(req, 507, "no room for the certificate", "CertificateTableFull");

  return showCertificate(req);
}

static http_response_t removeCertificate(http_request_t *req) {

  char name[256];

  if (!certificateName(req.param("name"), name, sizeof name) ||
      certificateWrite(name, NULL, NULL) != 0)
    return failed(req, 404, "no certificate for that name", "CertificateNotFound");

  return req.reply(200).json("{}");
}

#include "jwt.c"
#include "oidc.c"
#include "metrics.c"
#include "compat.c"

int main(void) {

  http.hosted();

  /* only where the controller's config opted in: a request elsewhere costs nothing */
  http.accessIf("wxingress_jwt_realm", jwtGuard);
  http.variable("jwt", jwtVariable);
  http.accessIf("wxingress_oidc_client", oidcGuard);
  http.variable("oidc", oidcVariable);

  /* ingress-nginx's request metrics, where the config set $wxingress_metrics */
  http.once(metricsOpen);
  http.eachWorker(metricsWorker);
  http.logIf("wxingress_metrics", countRequest);
  http.get("/api/wxingress/1/metrics", listMetrics);
  http.get("/api/wxingress/1/metrics/latency", listLatency);

  http.once(canariesOpen);
  http.variable("canary", chooseUpstream);

  /* what njs computed for the configuration before: compat.c */
  http.variable("ingress_nginx", ingressNginxVariable);

  http.once(balancersOpen);
  http.balancer(balance);
  http.balancerFor(balancesFor);
  http.balanced(balancedTry);

  http.once(revocationsOpen);
  http.get("/api/wxingress/1/oidc/revocations", listRevocations);
  http.put("/api/wxingress/1/oidc/revocations", setRevocations);

  http.once(certificatesOpen);
  http.certificate(certificateOf);
  http.tlsPairs(CERTIFICATE_NAMES);

  http.get("/api/wxingress/1/certificates", listCertificates);
  http.get("/api/wxingress/1/certificates/:name", showCertificate);
  http.put("/api/wxingress/1/certificates/:name", setCertificate);
  http.remove("/api/wxingress/1/certificates/:name", removeCertificate);

  http.get("/api/wxingress/1/balancers", listBalancers);
  http.get("/api/wxingress/1/balancers/:upstream", showBalancer);
  http.put("/api/wxingress/1/balancers/:upstream", setBalancer);
  http.remove("/api/wxingress/1/balancers/:upstream", removeBalancer);
  http.get("/api/wxingress/1/ewma", listEwma);

  http.get("/api/wxingress/1/canaries", listCanaries);
  http.get("/api/wxingress/1/canaries/:primary", showCanary);
  http.put("/api/wxingress/1/canaries/:primary", setCanary);
  http.remove("/api/wxingress/1/canaries/:primary", removeCanary);

  http.get("/api", versions);

  http.get("/api/9/http/upstreams/:name/servers", listServers);
  http.get("/api/9/http/upstreams/:name/servers/:id", showServer);
  http.post("/api/9/http/upstreams/:name/servers", addServer);
  http.patch("/api/9/http/upstreams/:name/servers/:id", changeServer);
  http.remove("/api/9/http/upstreams/:name/servers/:id", removeServer);

  http.get("/api/9/stream/upstreams/:name/servers", listServers);
  http.get("/api/9/stream/upstreams/:name/servers/:id", showServer);
  http.post("/api/9/stream/upstreams/:name/servers", addServer);
  http.patch("/api/9/stream/upstreams/:name/servers/:id", changeServer);
  http.remove("/api/9/stream/upstreams/:name/servers/:id", removeServer);

  return 0;
}
