package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/gateway/extras"
	ngfhttp "github.com/wx-one/wxingress/internal/third_party/ngf/controller/nginx/config/http"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/dataplane"
)

// The filters of the Gateway API go beyond what a VirtualServer can say:
// adding to a request header, removing one, a path rewrite whose result
// depends on the matched prefix, a redirect with a 303, mirrors. They go to
// the config as extras (internal/gateway/extras): nginx text generated here,
// applied by the config generation to the location of the action they belong
// to. What a VirtualServer can say - the upstream, the match, the split -
// stays in the VirtualServer.

// extrasBuilder collects the extras of one VirtualServer.
type extrasBuilder struct {
	// prefix starts the nginx variables of the VirtualServer, which must
	// be unique among all configs
	prefix string
	e      extras.Extras
	n      int
	// http are the http texts by the variable they define, to define each
	// once; server the server texts by their key
	http   map[string]bool
	server map[string]bool
}

func newExtrasBuilder(vsName string) *extrasBuilder {
	return &extrasBuilder{
		prefix: "wxgw_" + strings.ToLower(strings.NewReplacer("-", "_").Replace(strings.TrimPrefix(vsName, NamePrefix))),
		http:   make(map[string]bool),
		server: make(map[string]bool),
	}
}

// add records a, returns its key.
func (b *extrasBuilder) add(a *extras.Action) string {
	b.n++
	key := "a" + strconv.Itoa(b.n)
	if b.e.Actions == nil {
		b.e.Actions = make(map[string]*extras.Action)
	}
	b.e.Actions[key] = a
	return key
}

// define adds http text that defines the variable name once.
func (b *extrasBuilder) define(name, text string) {
	if b.http[name] {
		return
	}
	b.http[name] = true
	b.e.HTTP = append(b.e.HTTP, text)
}

// serverText adds server text once by its key.
func (b *extrasBuilder) serverText(key, text string) {
	if b.server[key] {
		return
	}
	b.server[key] = true
	b.e.Server = append(b.e.Server, text)
}

// variable is a variable of the VirtualServer.
func (b *extrasBuilder) variable(kind, name string) string {
	return b.prefix + "_" + kind + "_" + safeVar(name)
}

func safeVar(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '_'
	}, s)
}

// nginxString quotes s for nginx, as a value that may hold variables.
func nginxString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// listenerInfo is what a redirect needs of the listener it is on.
type listenerInfo struct {
	// port is the port of the listener, 0 for the default Gateway, whose
	// VirtualServers serve HTTP on 80 and HTTPS on 443 at once
	port int32
	// scheme is its scheme, "" for the default Gateway
	scheme string
	// dynamic tells that the wxingress module serves (-dynamic-upstreams)
	dynamic bool
	// snippets tells that snippets are allowed (-enable-snippets)
	snippets bool
}

// isMirrorPath tells whether a path rule is one NGF made for the target of
// a mirror filter, which only mirrored requests may reach.
func isMirrorPath(path string) bool {
	return strings.HasPrefix(path, ngfhttp.InternalMirrorRoutePathPrefix)
}

// proxyExtras are the extras of a match rule that proxies. Every one has at
// least the Host header: the Gateway API passes the client's Host header as
// it came, with its port, where a VirtualServer passes nginx's $host.
func (m *routeMapper) proxyExtras(rule *dataplane.PathRule, mr *dataplane.MatchRule) *extras.Action {
	a := &extras.Action{SetHeaders: []extras.Header{{Name: "Host", Value: "${" + m.hostVariable() + "}"}}}
	f := &mr.Filters
	if h := f.RequestHeaderModifiers; h != nil {
		for _, s := range h.Set {
			a.SetHeaders = append(a.SetHeaders, extras.Header{Name: s.Name, Value: s.Value})
		}
		for _, s := range h.Add {
			a.SetHeaders = append(a.SetHeaders, extras.Header{Name: s.Name, Value: "${" + m.appendVariable(s.Name) + "}" + s.Value})
		}
		for _, name := range h.Remove {
			a.SetHeaders = append(a.SetHeaders, extras.Header{Name: name, Value: ""})
		}
	}
	if rw := f.RequestURLRewrite; rw != nil {
		if rw.Hostname != nil {
			a.SetHeaders[0].Value = *rw.Hostname
		}
		if rw.Path != nil && !isMirrorPath(rule.Path) {
			if v := m.pathVariable("rewrite", rw.Path, rule); v != "" {
				// the rewritten path, with the arguments nginx appends
				a.Rewrites = append(a.Rewrites, "^ $"+v+" break")
			}
		}
	}
	if h := f.ResponseHeaderModifiers; h != nil {
		for _, s := range h.Set {
			a.HideHeaders = append(a.HideHeaders, s.Name)
			a.AddHeaders = append(a.AddHeaders, extras.Header{Name: s.Name, Value: s.Value})
		}
		for _, s := range h.Add {
			a.AddHeaders = append(a.AddHeaders, extras.Header{Name: s.Name, Value: s.Value})
		}
		a.HideHeaders = append(a.HideHeaders, h.Remove...)
	}
	if h2c(mr.BackendGroup.Backends) {
		a.HTTPVersion = "2"
	}
	if c := f.CORSFilter; c != nil {
		m.cors(a, c)
	}
	for _, mirror := range f.RequestMirrors {
		if mirror.Target != nil {
			a.Directives = append(a.Directives, "mirror "+nginxString(*mirror.Target)+";")
		}
	}
	if isMirrorPath(rule.Path) {
		// the target of mirrored requests: internal, the original URI to the
		// mirror's backend, and only as many as the mirror's percentage asks
		a.Directives = append(a.Directives, "internal;")
		if v, ok := m.mirrorSampled[rule.Path]; ok {
			a.Directives = append(a.Directives, "if ($"+v+` = "") { return 204; }`)
		}
		a.Rewrites = append(a.Rewrites, "^ $request_uri_no_args break")
	} else {
		m.routeExtras(a, mr.Source)
	}
	return a
}

// cors answers a preflight request itself and adds the CORS headers to
// every response, as NGF renders a CORS filter: the allowed origin and the
// credentials only for an origin the filter allows.
func (m *routeMapper) cors(a *extras.Action, c *dataplane.HTTPCORSFilter) {
	key := shortHash(fmt.Sprintf("%v", *c))
	originMap := func(kind, result string) string {
		v := m.ext.variable(kind, key)
		var b strings.Builder
		fmt.Fprintf(&b, "map $http_origin $%s {\n", v)
		for _, o := range c.AllowOrigins {
			re := "~^" + strings.ReplaceAll(strings.ReplaceAll(o, ".", `\.`), "*", ".*") + "$"
			fmt.Fprintf(&b, "    %s %s;\n", nginxString(re), result)
		}
		b.WriteString("}")
		m.ext.define(v, b.String())
		return v
	}
	if len(c.AllowOrigins) > 0 {
		a.AddHeaders = append(a.AddHeaders, extras.Header{Name: "Access-Control-Allow-Origin", Value: "$" + originMap("cors_origin", "$http_origin")})
	}
	if len(c.AllowMethods) > 0 {
		v := strings.Join(c.AllowMethods, ", ")
		if v == "*" {
			v = "$http_access_control_request_method"
		}
		a.AddHeaders = append(a.AddHeaders, extras.Header{Name: "Access-Control-Allow-Methods", Value: v})
	}
	if len(c.AllowHeaders) > 0 {
		v := strings.Join(c.AllowHeaders, ", ")
		if v == "*" {
			v = "$http_access_control_request_headers"
		}
		a.AddHeaders = append(a.AddHeaders, extras.Header{Name: "Access-Control-Allow-Headers", Value: v})
	}
	if len(c.ExposeHeaders) > 0 {
		a.AddHeaders = append(a.AddHeaders, extras.Header{Name: "Access-Control-Expose-Headers", Value: strings.Join(c.ExposeHeaders, ", ")})
	}
	if c.AllowCredentials {
		a.AddHeaders = append(a.AddHeaders, extras.Header{Name: "Access-Control-Allow-Credentials", Value: "$" + originMap("cors_credentials", "true")})
	}
	if c.MaxAge > 0 {
		a.AddHeaders = append(a.AddHeaders, extras.Header{Name: "Access-Control-Max-Age", Value: strconv.Itoa(int(c.MaxAge))})
	}
	a.Directives = append(a.Directives, "if ($request_method = OPTIONS) { return 200; }")
}

// h2c tells whether every valid backend speaks HTTP/2 without TLS (the
// appProtocol kubernetes.io/h2c of its Service port): the version is one per
// location, so all or none, as NGF decides it.
func h2c(backends []dataplane.Backend) bool {
	found := false
	for _, b := range backends {
		if !b.Valid {
			continue
		}
		if b.AppProtocol != "kubernetes.io/h2c" {
			return false
		}
		found = true
	}
	return found
}

// hostVariable is the variable with the Host header the client sent, or
// nginx's $host for a request without one.
func (m *routeMapper) hostVariable() string {
	v := m.ext.prefix + "_host"
	m.ext.define(v, fmt.Sprintf("map $http_host $%s {\n    \"\" $host;\n    default $http_host;\n}", v))
	return v
}

// unsupportedFilters records the filters of a match rule that are not served.
func (m *routeMapper) unsupportedFilters(rule *dataplane.PathRule, mr *dataplane.MatchRule) {
	f := &mr.Filters
	for _, u := range []struct {
		name string
		set  bool
	}{
		{"ExternalAuth", f.ExternalAuthFilter != nil},
		{"NGF's authentication", f.AuthenticationFilter != nil},
		{"NGF's snippets", len(f.SnippetsFilters) > 0},
	} {
		if u.set {
			m.skipped = append(m.skipped, fmt.Sprintf("path %s: the %s filter is not served", rule.Path, u.name))
		}
	}
}

// appendVariable is the variable that holds what a header has, followed by
// a comma, or nothing: what the Gateway API's add appends to.
func (m *routeMapper) appendVariable(header string) string {
	v := m.ext.variable("add", header)
	src := "$http_" + safeVar(strings.ReplaceAll(header, "-", "_"))
	m.ext.define(v, fmt.Sprintf("map %s $%s {\n    \"\" \"\";\n    default %s;\n}", src, v, nginxString(src+",")))
	return v
}

// pathVariable is a variable that holds the path a rewrite or redirect makes
// of the request's path - "" if the modifier cannot apply to the rule. It is
// computed from the request's own URI, so that it is the same in the
// internal locations of matches and splits, where nginx's $uri is another.
func (m *routeMapper) pathVariable(kind string, pm *dataplane.HTTPPathModifier, rule *dataplane.PathRule) string {
	var regex, replacement string
	switch pm.Type {
	case dataplane.ReplaceFullPath:
		regex, replacement = "^", pm.Replacement
	case dataplane.ReplacePrefixMatch:
		if rule.PathType != dataplane.PathTypePrefix {
			return ""
		}
		// as NGF renders it (servers.go createMainRewriteForFilters)
		prefix := pm.Replacement
		if prefix == "" {
			prefix = "/"
		}
		escaped := strings.ReplaceAll(rule.Path, "$", `\$`)
		regex = "^" + escaped + "([^?]*)?"
		replacement = prefix + "$1"
		if strings.HasSuffix(prefix, "/") && !strings.HasSuffix(rule.Path, "/") {
			regex = "^" + escaped + "(?:/([^?]*))?"
		}
		if strings.HasSuffix(rule.Path, "/") && !strings.HasSuffix(prefix, "/") {
			replacement = prefix + "/$1"
		}
	default:
		return ""
	}
	key := string(pm.Type) + "\n" + pm.Replacement + "\n" + rule.Path
	v := m.ext.variable(kind, shortHash(key))
	m.ext.define(v, fmt.Sprintf("map $request_uri_no_args $%s {\n    %s %s;\n    default $request_uri_no_args;\n}",
		v, nginxString("~"+regex), nginxString(replacement)))
	return v
}

// redirect maps a RequestRedirect filter onto a return of the location,
// as NGF renders it.
func (m *routeMapper) redirect(rule *dataplane.PathRule, f *dataplane.HTTPRequestRedirectFilter) *extras.Action {
	hostname := "$host"
	if f.Hostname != nil {
		hostname = *f.Hostname
	}
	code := 302
	if f.StatusCode != nil {
		code = *f.StatusCode
	}
	scheme := "$scheme"
	if f.Scheme != nil {
		scheme = *f.Scheme
	}

	// the port: the filter's, else the scheme's if the filter has one, else
	// the listener's; left out where it is the scheme's well-known one
	hostPort := hostname
	switch {
	case f.Port != nil:
		if !wellKnownPort(scheme, *f.Port) {
			hostPort = hostname + ":" + strconv.Itoa(int(*f.Port))
		}
	case f.Scheme != nil:
	case m.listener.port != 0 && !wellKnownPort(m.listener.scheme, m.listener.port):
		hostPort = hostname + ":" + strconv.Itoa(int(m.listener.port))
	}

	path := "$request_uri"
	if f.Path != nil {
		v := m.pathVariable("redirect", f.Path, rule)
		if v == "" {
			return nil
		}
		path = "$" + v + "$is_args$args"
	}
	return &extras.Action{Return: &version2.Return{Code: code, Text: scheme + "://" + hostPort + path}}
}

func wellKnownPort(scheme string, port int32) bool {
	return (scheme == "http" && port == 80) || (scheme == "https" && port == 443)
}

// mirrorSamples defines, for every mirror target that is to get only a
// percentage of the requests, a variable that is not empty for that share.
func (m *routeMapper) mirrorSamples(rules []dataplane.PathRule) {
	m.mirrorSampled = make(map[string]string)
	percents := make(map[string]float64)
	for i := range rules {
		for j := range rules[i].MatchRules {
			for _, mirror := range rules[i].MatchRules[j].Filters.RequestMirrors {
				if mirror.Target == nil || mirror.Percent == nil || *mirror.Percent >= 100 {
					continue
				}
				percents[*mirror.Target] = *mirror.Percent
			}
		}
	}
	targets := make([]string, 0, len(percents))
	for t := range percents {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		v := m.ext.variable("mirror", shortHash(t))
		m.mirrorSampled[t] = v
		m.ext.define(v, fmt.Sprintf("split_clients $request_id $%s {\n    %.2f%% \"1\";\n    * \"\";\n}", v, percents[t]))
	}
}

// shortHash is a short hex hash of s, for names.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:5])
}
