package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"

	"github.com/wx-one/wxingress/internal/gateway/extras"
	"github.com/wx-one/wxingress/internal/nginx"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/dataplane"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph"
)

const (
	// NamePrefix starts the name of every VirtualServer made from a Gateway.
	// Kubernetes names are lowercase, so no VirtualServer from the API can
	// have one of these names.
	NamePrefix = "GW-"

	// AnnotationGateway is the Gateway a VirtualServer was made from.
	AnnotationGateway = "wxingress.wizardtales.com/gateway"
	// AnnotationHostname is the hostname of the Gateway it serves.
	AnnotationHostname = "wxingress.wizardtales.com/gateway-hostname"
	// AnnotationPort is the listener port it serves, for a Gateway with an
	// address of its own.
	AnnotationPort = "wxingress.wizardtales.com/gateway-port"

	// catchAllHost is the hostname NGF's dataplane gives the server of a
	// listener without a hostname, and nginx's regex that matches any.
	catchAllHost = "~^"
)

// IsGatewayVirtualServer tells whether vs was made from a Gateway rather
// than read from the API: it has no object to write status or events to.
func IsGatewayVirtualServer(vs *conf_v1.VirtualServer) bool {
	return vs != nil && strings.HasPrefix(vs.Name, NamePrefix)
}

// IsCatchAllHost tells whether host is the hostname of a VirtualServer that
// serves a Gateway listener without a hostname.
func IsCatchAllHost(host string) bool {
	return host == catchAllHost
}

type mapOptions struct {
	// dynamic tells that the wxingress module serves (-dynamic-upstreams)
	dynamic      bool
	snippets     bool
	ingressClass string
	httpPort     int32
	httpsPort    int32
}

// servicePort is a Service port a backend of a route names.
type servicePort struct {
	svc  types.NamespacedName
	port int32
}

// upstreamIndex finds the Service port behind the upstream name the
// dataplane conversion gives a backend.
type upstreamIndex map[string]servicePort

func newUpstreamIndex(g *graph.Graph) upstreamIndex {
	idx := make(upstreamIndex)
	for _, r := range g.L4Routes {
		for _, br := range r.Spec.GetBackendRefs() {
			if ref := br.ServicePortReference(); ref != "" {
				idx[ref] = servicePort{svc: br.SvcNsName, port: br.ServicePort.Port}
			}
		}
	}
	for _, r := range g.Routes {
		for _, rule := range r.Spec.Rules {
			for _, br := range rule.BackendRefs {
				if ref := br.ServicePortReference(); ref != "" {
					idx[ref] = servicePort{svc: br.SvcNsName, port: br.ServicePort.Port}
				}
			}
		}
	}
	return idx
}

func sortedGateways(g *graph.Graph) []*graph.Gateway {
	keys := slices.SortedFunc(maps.Keys(g.Gateways), func(a, b types.NamespacedName) int {
		return strings.Compare(a.String(), b.String())
	})
	gws := make([]*graph.Gateway, 0, len(keys))
	for _, k := range keys {
		gws = append(gws, g.Gateways[k])
	}
	return gws
}

// hostServers are the servers of one hostname of a Gateway.
type hostServers struct {
	http, https *dataplane.VirtualServer
}

// mapped is what one Gateway maps onto.
type mapped struct {
	virtualServers []*conf_v1.VirtualServer
	// listeners are the nginx listeners of a Gateway with an address of its
	// own, one per port
	listeners []conf_v1.Listener
	skipped   []string
}

var http2On = true

// ListenerName names the nginx listener of a port of a Gateway. Like the
// names of the VirtualServers it is no name a GlobalConfiguration can have.
func ListenerName(gw types.NamespacedName, port int32) string {
	sum := sha256.Sum256([]byte(gw.String()))
	return fmt.Sprintf("%s%s-%d", NamePrefix, hex.EncodeToString(sum[:6]), port)
}

// IsGatewayListener tells whether a listener name is one of a Gateway.
func IsGatewayListener(name string) bool {
	return strings.HasPrefix(name, NamePrefix)
}

// toVirtualServers maps the dataplane configuration of a Gateway onto
// VirtualServers. The default Gateway, on nginx's default ports, gets one per
// hostname; every other Gateway one per hostname and port, each on the nginx
// listener of that port. It returns what it could not map as well.
func toVirtualServers(
	gw *graph.Gateway,
	cfg dataplane.Configuration,
	idx upstreamIndex,
	opts mapOptions,
	addr gatewayAddress,
) mapped {
	if addr.shared {
		return toSharedVirtualServers(gw, cfg, idx, opts)
	}
	var out mapped
	gwName := types.NamespacedName{Namespace: gw.Source.Namespace, Name: gw.Source.Name}

	listeners := make(map[int32]conf_v1.Listener)
	add := func(s *dataplane.VirtualServer, ssl bool) {
		if s.IsDefault {
			return
		}
		nginxPort, ok := addr.nginxPorts[listenerPort{port: s.Port, protocol: corev1.ProtocolTCP}]
		if !ok {
			out.skipped = append(out.skipped, fmt.Sprintf("Gateway %s: port %d of %s has no nginx port yet", gwName, s.Port, s.Hostname))
			return
		}
		name := ListenerName(gwName, s.Port)
		listeners[s.Port] = conf_v1.Listener{Name: name, Protocol: conf_v1.HTTPProtocol, Port: int(nginxPort), Ssl: ssl}

		vs := newVirtualServer(gw, virtualServerName(gwName, s.Hostname, s.Port), s.Hostname, opts)
		vs.Annotations[AnnotationPort] = strconv.Itoa(int(s.Port))
		// HTTP/2 as well: with TLS by ALPN, without it a client may speak
		// h2c with prior knowledge (gRPC does), and HTTP/1 still works
		vs.Spec.HTTP2 = &http2On
		if ssl {
			vs.Spec.Listener = &conf_v1.VirtualServerListener{HTTPS: name}
			if !out.setTLS(vs, gwName, s) {
				return
			}
		} else {
			vs.Spec.Listener = &conf_v1.VirtualServerListener{HTTP: name}
		}
		scheme := "http"
		if ssl {
			scheme = "https"
		}
		switch {
		case out.addRoutes(vs, gwName, s.PathRules, idx, listenerInfo{port: s.Port, scheme: scheme, dynamic: opts.dynamic, snippets: opts.snippets}):
			out.virtualServers = append(out.virtualServers, vs)
		case ssl && vs.Spec.TLS != nil:
			// a listener without routes still has its certificate on its
			// port - for a listener without a hostname for every name - and
			// answers 404
			vs.Spec.Routes = []conf_v1.Route{{Path: "/", Action: returnAction(404, "Not Found")}}
			out.virtualServers = append(out.virtualServers, vs)
		}
	}
	for i := range cfg.HTTPServers {
		add(&cfg.HTTPServers[i], false)
	}
	for i := range cfg.SSLServers {
		add(&cfg.SSLServers[i], true)
	}
	for _, port := range slices.Sorted(maps.Keys(listeners)) {
		out.listeners = append(out.listeners, listeners[port])
	}
	return out
}

// toSharedVirtualServers maps the default Gateway onto one VirtualServer per
// hostname, on nginx's default ports.
func toSharedVirtualServers(
	gw *graph.Gateway,
	cfg dataplane.Configuration,
	idx upstreamIndex,
	opts mapOptions,
) mapped {
	var out mapped
	gwName := types.NamespacedName{Namespace: gw.Source.Namespace, Name: gw.Source.Name}

	hosts := make(map[string]*hostServers)
	add := func(s *dataplane.VirtualServer, https bool, port int32) {
		if s.IsDefault {
			return
		}
		if s.Port != port {
			out.skipped = append(out.skipped, fmt.Sprintf("Gateway %s: port %d of %s is not served, only %d and %d",
				gwName, s.Port, s.Hostname, opts.httpPort, opts.httpsPort))
			return
		}
		h := hosts[s.Hostname]
		if h == nil {
			h = &hostServers{}
			hosts[s.Hostname] = h
		}
		if https {
			h.https = s
		} else {
			h.http = s
		}
	}
	for i := range cfg.HTTPServers {
		add(&cfg.HTTPServers[i], false, 80)
	}
	for i := range cfg.SSLServers {
		add(&cfg.SSLServers[i], true, 443)
	}

	for _, host := range slices.Sorted(maps.Keys(hosts)) {
		h := hosts[host]
		// A VirtualServer with TLS serves HTTP and HTTPS. Where the routes of
		// the two listeners differ, or there is only HTTPS, every match
		// says which scheme it is for.
		var sets []ruleSet
		switch {
		case h.https == nil:
			sets = []ruleSet{{rules: h.http.PathRules}}
		case h.http == nil:
			sets = []ruleSet{{rules: h.https.PathRules, scheme: "https"}}
		case reflect.DeepEqual(h.http.PathRules, h.https.PathRules):
			sets = []ruleSet{{rules: h.https.PathRules}}
		default:
			sets = []ruleSet{{rules: h.https.PathRules, scheme: "https"}, {rules: h.http.PathRules, scheme: "http"}}
		}

		vs := newVirtualServer(gw, virtualServerName(gwName, host, 0), host, opts)
		if h.https != nil && !out.setTLS(vs, gwName, h.https) {
			continue
		}
		if out.addRouteSets(vs, gwName, sets, idx, listenerInfo{dynamic: opts.dynamic, snippets: opts.snippets}) {
			out.virtualServers = append(out.virtualServers, vs)
		}
	}
	return out
}

func newVirtualServer(gw *graph.Gateway, name, host string, opts mapOptions) *conf_v1.VirtualServer {
	vs := &conf_v1.VirtualServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         gw.Source.Namespace,
			Name:              name,
			CreationTimestamp: gw.Source.CreationTimestamp,
			Annotations: map[string]string{
				AnnotationGateway:  gw.Source.Namespace + "/" + gw.Source.Name,
				AnnotationHostname: host,
			},
		},
		Spec: conf_v1.VirtualServerSpec{
			IngressClass: opts.ingressClass,
			Host:         host,
			// the Policies of the Gateway are those of all its servers
			Policies: policiesOf(&gw.Source.ObjectMeta),
		},
	}
	return vs
}

// setTLS gives vs the certificate of an HTTPS server. It tells whether vs
// can be served.
func (out *mapped) setTLS(vs *conf_v1.VirtualServer, gwName types.NamespacedName, s *dataplane.VirtualServer) bool {
	if s.SSL == nil || len(s.SSL.KeyPairIDs) == 0 {
		return true
	}
	secret, ok := secretOfKeyPair(s.SSL.KeyPairIDs[0])
	switch {
	case !ok:
		out.skipped = append(out.skipped, fmt.Sprintf("Gateway %s: %s: unknown certificate %s", gwName, s.Hostname, s.SSL.KeyPairIDs[0]))
	default:
		// a VirtualServer names a Secret of its own namespace: one with the
		// certificate of another namespace, which a ReferenceGrant allows
		// (NGF's graph checks it), is in that namespace - it exists only in
		// memory, and names the Services of others with their namespace
		vs.Namespace = secret.Namespace
		vs.Spec.TLS = &conf_v1.TLS{Secret: secret.Name}
	}
	if len(s.SSL.KeyPairIDs) > 1 {
		out.skipped = append(out.skipped, fmt.Sprintf("Gateway %s: %s: only the first of %d certificates is served",
			gwName, s.Hostname, len(s.SSL.KeyPairIDs)))
	}
	return true
}

// addRoutes maps path rules onto the routes and upstreams of vs, and what
// their filters need beyond that onto its extras. It tells whether vs has
// routes.
func (out *mapped) addRoutes(
	vs *conf_v1.VirtualServer,
	gwName types.NamespacedName,
	rules []dataplane.PathRule,
	idx upstreamIndex,
	listener listenerInfo,
) bool {
	return out.addRouteSets(vs, gwName, []ruleSet{{rules: rules}}, idx, listener)
}

// ruleSet are path rules, for one scheme or, without one, for both.
type ruleSet struct {
	rules  []dataplane.PathRule
	scheme string
}

// addRouteSets maps sets of path rules onto the routes of vs.
func (out *mapped) addRouteSets(
	vs *conf_v1.VirtualServer,
	gwName types.NamespacedName,
	sets []ruleSet,
	idx upstreamIndex,
	listener listenerInfo,
) bool {
	var all []dataplane.PathRule
	for _, set := range sets {
		all = append(all, set.rules...)
	}
	if len(all) == 0 {
		return false
	}
	m := &routeMapper{
		vsNamespace: vs.Namespace,
		idx:         idx,
		upstreams:   make(map[string]conf_v1.Upstream),
		ext:         newExtrasBuilder(vs.Name),
		listener:    listener,
		keys:        make(map[*extras.Action]string),
		settings:    make(map[string]*routeSettings),
		dynamic:     listener.dynamic,
		balanced:    make(map[string]*balancedUpstream),
	}
	m.mirrorSamples(all)
	vs.Spec.Routes = m.routes(sets)
	out.skipped = append(out.skipped, m.skipped...)
	for _, name := range slices.Sorted(maps.Keys(m.upstreams)) {
		vs.Spec.Upstreams = append(vs.Spec.Upstreams, m.upstreams[name])
	}
	m.ext.e.Balancers = m.balancers()
	if !m.ext.e.Empty() {
		vs.Annotations[extras.Annotation] = m.ext.e.Encode()
	}
	if m.grpc {
		// gRPC is HTTP/2; on the default Gateway's ports, without TLS, h2c
		// also needs the http2 key of the ConfigMap
		vs.Spec.HTTP2 = &http2On
	}
	return len(vs.Spec.Routes) > 0
}

// virtualServerName names the VirtualServer of a hostname and port of a
// Gateway (port 0 for the default Gateway, which has one per hostname): the
// prefix that marks it, then a hash of all. The name ends up in nginx
// variable names, which take no dots - Gateway names and hostnames have
// them; the annotations say which Gateway and hostname it is.
func virtualServerName(gw types.NamespacedName, host string, port int32) string {
	key := gw.String() + "\n" + host
	if port != 0 {
		key += "\n" + strconv.Itoa(int(port))
	}
	sum := sha256.Sum256([]byte(key))
	return NamePrefix + hex.EncodeToString(sum[:8])
}

// secretOfKeyPair parses the ID NGF's dataplane gives a certificate,
// ssl_keypair_<namespace>_<name>. Neither can contain an underscore.
func secretOfKeyPair(id dataplane.SSLKeyPairID) (types.NamespacedName, bool) {
	rest, ok := strings.CutPrefix(string(id), "ssl_keypair_")
	if !ok {
		return types.NamespacedName{}, false
	}
	ns, name, ok := strings.Cut(rest, "_")
	if !ok || ns == "" || name == "" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: ns, Name: name}, true
}

type routeMapper struct {
	// vsNamespace is the namespace of the VirtualServer, which Services in
	// others are named with
	vsNamespace string
	idx         upstreamIndex
	upstreams   map[string]conf_v1.Upstream
	skipped     []string

	// ext collects the extras of the VirtualServer, keys the key each
	// action's extras have there
	ext  *extrasBuilder
	keys map[*extras.Action]string
	// listener is what a redirect needs to know of the listener
	listener listenerInfo
	// mirrorSampled are the variables of mirror targets that get only a
	// share of the requests
	mirrorSampled map[string]string
	// grpc tells that a route is gRPC, which needs HTTP/2
	grpc bool
	// dynamic tells that the wxingress module serves (-dynamic-upstreams)
	dynamic bool
	// settings are those of the routes, by namespace and name; balanced the
	// upstreams with a balancer rule
	settings map[string]*routeSettings
	balanced map[string]*balancedUpstream
}

// location is an nginx location and the path rules that end up in it, in
// the order they are tried.
type location struct {
	path  string
	rules []contribution
}

type contribution struct {
	rule  *dataplane.PathRule
	exact bool
	// scheme is the one scheme the rule is for, "" for both
	scheme string
}

// routes maps path rules onto VirtualServer routes. A Gateway API prefix
// /foo matches /foo and /foo/..., not /foobar, so it becomes the locations
// /foo/ and = /foo, as NGF renders it. Path rules that end up in the same
// location are tried exact first, then by length of their path, longest
// first - the precedence of the Gateway API.
func (m *routeMapper) routes(sets []ruleSet) []conf_v1.Route {
	locs := make(map[string]*location)
	var scheme string
	add := func(path string, r *dataplane.PathRule, exact bool) {
		l := locs[path]
		if l == nil {
			l = &location{path: path}
			locs[path] = l
		}
		l.rules = append(l.rules, contribution{rule: r, exact: exact, scheme: scheme})
	}
	for _, set := range sets {
		scheme = set.scheme
		m.addLocations(set.rules, add)
	}
	return m.locationRoutes(locs)
}

// addLocations adds the locations of path rules.
func (m *routeMapper) addLocations(rules []dataplane.PathRule, add func(path string, r *dataplane.PathRule, exact bool)) {
	for i := range rules {
		r := &rules[i]
		switch r.PathType {
		case dataplane.PathTypeExact:
			add("="+r.Path, r, true)
		case dataplane.PathTypePrefix:
			if strings.HasSuffix(r.Path, "/") {
				add(r.Path, r, false)
			} else {
				add(r.Path+"/", r, false)
				add("="+r.Path, r, false)
			}
		case dataplane.PathTypeRegularExpression:
			add("~"+r.Path, r, false)
		}
	}
}

// locationRoutes are the routes of the locations, in the order of their paths.
func (m *routeMapper) locationRoutes(locs map[string]*location) []conf_v1.Route {
	var routes []conf_v1.Route
	for _, path := range slices.Sorted(maps.Keys(locs)) {
		l := locs[path]
		sort.SliceStable(l.rules, func(i, j int) bool {
			a, b := l.rules[i], l.rules[j]
			if a.exact != b.exact {
				return a.exact
			}
			return len(a.rule.Path) > len(b.rule.Path)
		})
		routes = append(routes, m.route(l))
	}
	return routes
}

// route maps the match rules of a location onto a route: every rule with
// conditions becomes a match, tried in order; the first rule without
// conditions is the route's own action, and what follows it can never
// match. Without one the route answers 404, as a request no rule matches.
func (m *routeMapper) route(l *location) conf_v1.Route {
	route := conf_v1.Route{Path: l.path}
	// the Policies of the routes of the location, of all of them
	for _, c := range l.rules {
		for i := range c.rule.MatchRules {
			route.Policies = addPolicies(route.Policies, policiesOf(c.rule.MatchRules[i].Source))
		}
	}
	for _, c := range l.rules {
		for i := range c.rule.MatchRules {
			mr := &c.rule.MatchRules[i]
			action, splits := m.action(c.rule, mr)
			conds := matchConditions(mr.Match)
			if c.scheme != "" {
				conds = append([]conf_v1.Condition{{Variable: "$scheme", Value: c.scheme}}, conds...)
			}
			if len(conds) == 0 {
				route.Action, route.Splits = action, splits
				return route
			}
			route.Matches = append(route.Matches, conf_v1.Match{Conditions: conds, Action: action, Splits: splits})
		}
	}
	route.Action = returnAction(404, "Not Found")
	return route
}

// matchConditions maps a match onto VirtualServer conditions, which all have to
// hold. Exact values are compared as they are; regular expressions go into
// the map as nginx regexes.
func matchConditions(match dataplane.Match) []conf_v1.Condition {
	var conds []conf_v1.Condition
	if match.Method != nil {
		conds = append(conds, conf_v1.Condition{Variable: "$request_method", Value: *match.Method})
	}
	for _, h := range match.Headers {
		conds = append(conds, conf_v1.Condition{Header: h.Name, Value: matchValue(h.Type, h.Value)})
	}
	for _, q := range match.QueryParams {
		conds = append(conds, conf_v1.Condition{Variable: "$args", Value: "~" + queryParamRegex(q)})
	}
	return conds
}

// queryParamRegex matches the arguments of a request whose first parameter
// of the name has the value. nginx's $arg_<name> would find the name in any
// case; the Gateway API's names are case-sensitive, and it is the first
// occurrence that counts.
func queryParamRegex(q dataplane.HTTPQueryParamMatch) string {
	value := regexp.QuoteMeta(q.Value)
	if q.Type == dataplane.MatchTypeRegularExpression {
		value = "[^&]*(?:" + q.Value + ")[^&]*"
	}
	name := regexp.QuoteMeta(q.Name)
	// the parameters before it, none of them of the name, then it
	return "^(?:(?!" + name + "=)[^&]*&)*" + name + "=" + value + "(?:&|$)"
}

func matchValue(t dataplane.MatchType, v string) string {
	if t == dataplane.MatchTypeRegularExpression {
		return "~" + v
	}
	return v
}

// action maps the backends of a match rule onto an action or, for more
// than one, weighted splits. Invalid backends keep their share of the
// traffic and answer it with 500, as the Gateway API asks. A redirect
// answers instead of the backends. The filters go to the extras of the
// VirtualServer, under the key the action is marked with.
func (m *routeMapper) action(rule *dataplane.PathRule, mr *dataplane.MatchRule) (*conf_v1.Action, []conf_v1.Split) {
	if mr.Filters.InvalidFilter != nil {
		return returnAction(500, "Internal Server Error"), nil
	}
	m.serverSnippets(mr.Source)
	if rd := mr.Filters.RequestRedirect; rd != nil {
		a := m.redirect(rule, rd)
		if a == nil {
			m.skipped = append(m.skipped, fmt.Sprintf("path %s: a redirect that cannot apply to it", rule.Path))
			return returnAction(500, "Internal Server Error"), nil
		}
		return &conf_v1.Action{Redirect: &conf_v1.ActionRedirect{URL: extras.MarkerURL(m.key(a)), Code: 302}}, nil
	}
	extra := m.proxyExtras(rule, mr)
	m.unsupportedFilters(rule, mr)

	type share struct {
		action *conf_v1.Action
		weight int64
	}
	// the upstream of each backend, "" for an invalid one
	type backend struct {
		upstream string
		weight   int64
	}
	var backends []backend
	var total int64
	// a rule with two backends whose route has canary-by annotations is a
	// canary even with the weight 0, which only the header or cookie reaches
	var by *nginx.Canary
	if s := m.settingsOf(mr.Source); s != nil && s.settings.CanaryBy != nil && len(mr.BackendGroup.Backends) == 2 {
		by = s.settings.CanaryBy
	}
	for _, b := range mr.BackendGroup.Backends {
		if b.Weight <= 0 && (by == nil || b.Weight < 0) {
			continue
		}
		total += int64(b.Weight)
		name := ""
		if b.Valid {
			if n, ok := m.upstream(b.UpstreamName, rule.GRPC, rule, mr.Source); ok {
				name = n
			}
		}
		backends = append(backends, backend{upstream: name, weight: int64(b.Weight)})
	}
	if total == 0 {
		return returnAction(500, "Internal Server Error"), nil
	}
	if len(backends) == 2 && backends[0].upstream != "" && backends[1].upstream != "" {
		if a := m.canary(rule, mr, extra, by, backends[0].upstream, backends[0].weight, backends[1].upstream, backends[1].weight); a != nil {
			return a, nil
		}
	}
	var shares []share
	for _, b := range backends {
		action := returnAction(500, "Internal Server Error")
		if b.upstream != "" {
			action = m.proxy(b.upstream, extra)
		}
		shares = append(shares, share{action: action, weight: b.weight})
	}
	if len(shares) == 1 {
		return shares[0].action, nil
	}

	weights := make([]int64, len(shares))
	for i, s := range shares {
		weights[i] = s.weight
	}
	var splits []conf_v1.Split
	for i, pct := range percentages(weights) {
		if pct > 0 {
			splits = append(splits, conf_v1.Split{Weight: pct, Action: shares[i].action})
		}
	}
	if len(splits) == 1 {
		return splits[0].Action, nil
	}
	return nil, splits
}

// canary maps a rule with two weighted backends onto a canary of the
// wxingress module, as for an Ingress with a canary-weight: the first is the
// primary, the second its canary, the weights the rule's - exact, where
// splits would round to percents - and a change of them is a new rule for
// the module, not a reload. Only with -dynamic-upstreams, for HTTP.
func (m *routeMapper) canary(
	rule *dataplane.PathRule,
	mr *dataplane.MatchRule,
	extra *extras.Action,
	by *nginx.Canary,
	primary string,
	primaryWeight int64,
	canary string,
	canaryWeight int64,
) *conf_v1.Action {
	if !m.dynamic || rule.GRPC || primary == canary || len(mr.Filters.RequestMirrors) > 0 {
		if by != nil {
			m.skipped = append(m.skipped, fmt.Sprintf("path %s: the canary-by annotations need -dynamic-upstreams, an HTTPRoute and no mirror", rule.Path))
		}
		return nil
	}
	// the module counts a total below 100 as 100, as ingress-nginx does with
	// canary-weight-total; the Gateway API's weights are relative, so both
	// are scaled up to keep their ratio
	total := primaryWeight + canaryWeight
	if total > 0 && total < 100 {
		scale := (100 + total - 1) / total
		canaryWeight *= scale
		total *= scale
	}
	a := *extra
	a.Canary = &extras.Canary{
		Primary:     primary,
		Upstream:    canary,
		Weight:      int(canaryWeight),
		WeightTotal: int(total),
	}
	if by != nil {
		a.Canary.Header, a.Canary.HeaderValue, a.Canary.HeaderPattern, a.Canary.Cookie = by.Header, by.HeaderValue, by.HeaderPattern, by.Cookie
	}
	return m.proxy(primary, &a)
}

// percentages divides 100 among weights in proportion, by largest
// remainder, so that the parts add up to exactly 100 as VirtualServer
// splits must.
func percentages(weights []int64) []int {
	var total int64
	for _, w := range weights {
		total += w
	}
	out := make([]int, len(weights))
	type rem struct {
		i int
		r int64
	}
	rems := make([]rem, len(weights))
	sum := 0
	for i, w := range weights {
		out[i] = int(w * 100 / total)
		sum += out[i]
		rems[i] = rem{i: i, r: w * 100 % total}
	}
	sort.SliceStable(rems, func(a, b int) bool { return rems[a].r > rems[b].r })
	for k := 0; sum < 100; k++ {
		out[rems[k%len(rems)].i]++
		sum++
	}
	return out
}

// upstream names the VirtualServer upstream of a backend's Service port,
// adding it on first use. A gRPC route's is an upstream of its own, of type
// grpc.
func (m *routeMapper) upstream(upstreamName string, grpc bool, rule *dataplane.PathRule, src *metav1.ObjectMeta) (string, bool) {
	sp, ok := m.idx[upstreamName]
	if !ok {
		m.skipped = append(m.skipped, fmt.Sprintf("backend %s has no Service port", upstreamName))
		return "", false
	}
	key := upstreamName
	if grpc {
		key += "\ngrpc"
	}
	// a route with settings of its own has upstreams of its own
	settings := m.settingsOf(src)
	if settings != nil {
		key += "\nroute " + settings.key
	}
	sum := sha256.Sum256([]byte(key))
	name := "be-" + hex.EncodeToString(sum[:5])
	if grpc {
		m.grpc = true
	}
	if _, exists := m.upstreams[name]; !exists {
		svc := sp.svc.Name
		if sp.svc.Namespace != m.vsNamespace {
			svc = sp.svc.Namespace + "/" + sp.svc.Name
		}
		u := conf_v1.Upstream{Name: name, Service: svc, Port: uint16(sp.port)} //nolint:gosec // a Service port fits
		if settings != nil {
			u = withSettings(u, settings.settings)
		}
		if grpc {
			u.Type = "grpc"
		}
		m.upstreams[name] = u
	}
	if settings != nil && settings.settings.Balancing {
		b := m.balanced[name]
		if b == nil {
			b = &balancedUpstream{settings: settings, backend: ingressNginxBackend(sp)}
			m.balanced[name] = b
		}
		b.paths = append(b.paths, rule.Path)
	}
	return name, true
}

// key is the key of the extras of an action, the same for the same extras.
func (m *routeMapper) key(a *extras.Action) string {
	if k, ok := m.keys[a]; ok {
		return k
	}
	k := m.ext.add(a)
	m.keys[a] = k
	return k
}

// proxy is the action that proxies to an upstream with the extras of a
// match rule: marked with their key, which the config generation finds in
// the location of the action.
func (m *routeMapper) proxy(upstream string, extra *extras.Action) *conf_v1.Action {
	return &conf_v1.Action{Proxy: &conf_v1.ActionProxy{
		Upstream: upstream,
		RequestHeaders: &conf_v1.ProxyRequestHeaders{
			Set: []conf_v1.Header{{Name: extras.MarkerHeader, Value: m.key(extra)}},
		},
	}}
}

func returnAction(code int, body string) *conf_v1.Action {
	return &conf_v1.Action{Return: &conf_v1.ActionReturn{Code: code, Type: "text/plain", Body: body}}
}

// equalVirtualServers tells whether two VirtualServers made from a Gateway
// make the same config.
func equalVirtualServers(a, b *conf_v1.VirtualServer) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name &&
		a.CreationTimestamp.Equal(&b.CreationTimestamp) &&
		reflect.DeepEqual(a.Annotations, b.Annotations) &&
		reflect.DeepEqual(a.Spec, b.Spec)
}

// catchAllPlaceholder stands in for the catch-all hostname when a
// VirtualServer made from a Gateway is validated: the validator wants a DNS
// name, which the regex nginx gets is not.
const catchAllPlaceholder = "catch-all.gateway.invalid"

// ForValidation returns vs as the VirtualServer validator is to see it.
func ForValidation(vs *conf_v1.VirtualServer) *conf_v1.VirtualServer {
	if !IsCatchAllHost(vs.Spec.Host) {
		return vs
	}
	cp := vs.DeepCopy()
	cp.Spec.Host = catchAllPlaceholder
	return cp
}
