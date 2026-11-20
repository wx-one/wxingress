// Package extras carries what a VirtualServer made from a Gateway needs of
// nginx beyond what a VirtualServer can say - the filters of the Gateway API:
// adding to request headers, removing them, rewriting paths and hosts,
// redirects with every status, mirrors - from the Gateway layer, which knows
// the semantics, to the config generation, which applies it to the
// VirtualServer's config after generating it.
//
// The Gateway layer puts it, encoded, into an annotation of the
// VirtualServer, and marks every action it belongs to: a proxy action with
// a request header (MarkerHeader) that is dropped again, a redirect with a
// URL (MarkerURL). The generated location of the action carries the mark,
// wherever the VirtualServer's routes, matches and splits put it.
//
// What goes in here is nginx config text the Gateway layer generates from
// values NGF's validators accepted; it is applied only to VirtualServers
// made from Gateways, which no object in the API can be.
package extras

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wx-one/wxingress/internal/configs/version2"
)

const (
	// Annotation holds the encoded Extras of a VirtualServer.
	Annotation = "wxingress.wizardtales.com/gateway-extras"
	// MarkerHeader marks the proxy action an Action belongs to; its value is
	// the Action's key.
	MarkerHeader = "X-Wxingress-Gateway-Action"
	// markerHost is the host of the URL that marks a redirect action.
	markerHost = ".action.wxingress.invalid"
	// upstreamRef starts a reference to a VirtualServer upstream in Server
	// text, ended by "}}": the config generation puts the name of the nginx
	// upstream there.
	upstreamRef = "{{upstream:"
)

// MarkerURL is the URL of the redirect action an Action with key belongs to.
func MarkerURL(key string) string {
	return "http://" + key + markerHost
}

// UpstreamRef refers to the nginx upstream of a VirtualServer upstream.
func UpstreamRef(name string) string {
	return upstreamRef + name + "}}"
}

// Header is a header name and value.
type Header struct {
	Name  string `json:"n"`
	Value string `json:"v"`
}

// Action is what one action of the VirtualServer gets.
type Action struct {
	// SetHeaders are set on the request to the upstream, in place of what
	// the location sets for the same name; an empty value removes it.
	SetHeaders []Header `json:"set,omitempty"`
	// HideHeaders are removed from the response.
	HideHeaders []string `json:"hide,omitempty"`
	// AddHeaders are added to every response.
	AddHeaders []Header `json:"add,omitempty"`
	// Rewrites are rewrite directives, without the word rewrite, before the
	// location's own.
	Rewrites []string `json:"rewrite,omitempty"`
	// Directives are more location directives, each ending with ';'.
	Directives []string `json:"dir,omitempty"`
	// Return replaces the action: the location returns this (code and
	// text) instead of what the action does. For redirects.
	Return *version2.Return `json:"return,omitempty"`
	// HTTPVersion is the HTTP version of the requests to the upstream.
	HTTPVersion string `json:"httpv,omitempty"`
	// Canary makes the action's upstream the primary of a canary of the
	// wxingress module: each request goes to it or to the canary, by the
	// rule the controller gives the module - weights that change without a
	// reload, as an Ingress canary's.
	Canary *Canary `json:"canary,omitempty"`
}

// Canary is the canary of the upstream of an action.
type Canary struct {
	// Primary and Upstream are VirtualServer upstreams.
	Primary     string `json:"primary"`
	Upstream    string `json:"upstream"`
	Weight      int    `json:"weight"`
	WeightTotal int    `json:"weightTotal"`
	// Header, HeaderValue, HeaderPattern and Cookie are the rule's parts of
	// ingress-nginx's canary-by-header, -header-value, -header-pattern and
	// canary-by-cookie, which come before the weights.
	Header        string `json:"header,omitempty"`
	HeaderValue   string `json:"headerValue,omitempty"`
	HeaderPattern string `json:"headerPattern,omitempty"`
	Cookie        string `json:"cookie,omitempty"`
}

// Extras are all of a VirtualServer's.
type Extras struct {
	// HTTP is config text for the http context: maps and split_clients.
	HTTP []string `json:"http,omitempty"`
	// Server is config text for the server context: the locations mirrors
	// go to.
	Server []string `json:"server,omitempty"`
	// Actions by their keys.
	Actions map[string]*Action `json:"actions,omitempty"`

	// Stream is config text for the stream context of a TransportServer:
	// split_clients.
	Stream []string `json:"stream,omitempty"`
	// StreamServer is config text for its stream server.
	StreamServer []string `json:"streamServer,omitempty"`
	// ProxyPass replaces where its stream server proxies to.
	ProxyPass string `json:"proxyPass,omitempty"`
	// DefaultServer makes its stream server the default of its port.
	DefaultServer bool `json:"defaultServer,omitempty"`

	// Balancers are the rules of the wxingress module's balancer for
	// upstreams of the VirtualServer, by the upstream's name in it: the
	// JSON of nginx.Balancer. Their upstreams say meta_balancer.
	Balancers map[string]json.RawMessage `json:"balancers,omitempty"`
}

// Empty tells whether there is nothing to apply.
func (e *Extras) Empty() bool {
	return e == nil || (len(e.HTTP) == 0 && len(e.Server) == 0 && len(e.Actions) == 0 &&
		len(e.Stream) == 0 && len(e.StreamServer) == 0 && e.ProxyPass == "" && !e.DefaultServer && len(e.Balancers) == 0)
}

// Encode is the value of the annotation.
func (e *Extras) Encode() string {
	b, err := json.Marshal(e)
	if err != nil {
		// nothing in Extras can fail to marshal
		panic(err)
	}
	return string(b)
}

// Decode reads the annotation's value.
func Decode(v string) (*Extras, error) {
	var e Extras
	if err := json.Unmarshal([]byte(v), &e); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", Annotation, err)
	}
	return &e, nil
}

// Apply applies the extras of annotations to the config of a VirtualServer.
// upstreamName names the nginx upstream of a VirtualServer upstream.
// canaryVariable names the variable of the module that names the primary
// upstream or its canary.
func Apply(annotations map[string]string, cfg *version2.VirtualServerConfig, upstreamName func(string) string, canaryVariable func(string) string) error {
	v, ok := annotations[Annotation]
	if !ok {
		return nil
	}
	e, err := Decode(v)
	if err != nil {
		return err
	}
	cfg.HTTPSnippets = append(cfg.HTTPSnippets, e.HTTP...)
	for _, s := range e.Server {
		cfg.Server.Snippets = append(cfg.Server.Snippets, resolveUpstreams(s, upstreamName))
	}

	for name := range e.Balancers {
		for i := range cfg.Upstreams {
			if cfg.Upstreams[i].Name == upstreamName(name) {
				cfg.Upstreams[i].MetaBalancer = true
			}
		}
	}

	applied := make(map[string]bool)
	for i := range cfg.Server.Locations {
		loc := &cfg.Server.Locations[i]
		key, ok := markOf(loc)
		if !ok {
			continue
		}
		a := e.Actions[key]
		if a == nil {
			return fmt.Errorf("location %s has the mark of action %s, which has no extras", loc.Path, key)
		}
		applyAction(loc, a)
		if a.Canary != nil {
			primary := upstreamName(a.Canary.Primary)
			scheme, rest, _ := strings.Cut(loc.ProxyPass, "://")
			if strings.HasPrefix(rest, primary) {
				loc.ProxyPass = scheme + "://" + canaryVariable(primary) + strings.TrimPrefix(rest, primary)
			}
		}
		applied[key] = true
	}
	for key := range e.Actions {
		if !applied[key] {
			return fmt.Errorf("no location has the mark of action %s", key)
		}
	}
	return nil
}

// markOf finds the mark of an action in a location and removes it.
func markOf(loc *version2.Location) (string, bool) {
	for i, h := range loc.ProxySetHeaders {
		if strings.EqualFold(h.Name, MarkerHeader) {
			loc.ProxySetHeaders = append(loc.ProxySetHeaders[:i:i], loc.ProxySetHeaders[i+1:]...)
			return h.Value, true
		}
	}
	for _, p := range loc.ErrorPages {
		if key, ok := strings.CutPrefix(p.Name, "http://"); ok {
			if key, ok = strings.CutSuffix(key, markerHost); ok {
				return key, true
			}
		}
	}
	return "", false
}

func applyAction(loc *version2.Location, a *Action) {
	if a.Return != nil {
		// a redirect: what the location did goes, the rewrites and the
		// return come instead
		loc.ErrorPages = nil
		loc.InternalProxyPass = ""
		loc.ProxyInterceptErrors = false
		for _, r := range a.Rewrites {
			loc.Snippets = append(loc.Snippets, "rewrite "+r+";")
		}
		loc.Snippets = append(loc.Snippets, a.Directives...)
		loc.Snippets = append(loc.Snippets, fmt.Sprintf("return %d %q;", a.Return.Code, a.Return.Text))
		return
	}
	for _, h := range a.SetHeaders {
		replaced := false
		for i := range loc.ProxySetHeaders {
			if strings.EqualFold(loc.ProxySetHeaders[i].Name, h.Name) {
				loc.ProxySetHeaders[i].Value = h.Value
				replaced = true
			}
		}
		if !replaced {
			loc.ProxySetHeaders = append(loc.ProxySetHeaders, version2.Header{Name: h.Name, Value: h.Value})
		}
	}
	loc.ProxyHideHeaders = append(loc.ProxyHideHeaders, a.HideHeaders...)
	for _, h := range a.AddHeaders {
		loc.AddHeaders = append(loc.AddHeaders, version2.AddHeader{Header: version2.Header{Name: h.Name, Value: h.Value}, Always: true})
	}
	loc.Rewrites = append(append([]string(nil), a.Rewrites...), loc.Rewrites...)
	if len(a.Rewrites) > 0 {
		// the location of a match or split passes the request's own URI
		// on; a rewritten one has to go instead
		loc.ProxyPass = strings.TrimSuffix(loc.ProxyPass, "$request_uri")
	}
	loc.Snippets = append(loc.Snippets, a.Directives...)
	if a.HTTPVersion != "" {
		loc.ProxyHTTPVersion = a.HTTPVersion
	}
}

func resolveUpstreams(s string, upstreamName func(string) string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, upstreamRef)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(upstreamName(s[i+len(upstreamRef) : i+j]))
		s = s[i+j+2:]
	}
}

// ApplyStream applies the extras of annotations to the config of a
// TransportServer. upstreamName names the nginx upstream of a
// TransportServer upstream.
func ApplyStream(annotations map[string]string, cfg *version2.TransportServerConfig, upstreamName func(string) string) error {
	v, ok := annotations[Annotation]
	if !ok {
		return nil
	}
	e, err := Decode(v)
	if err != nil {
		return err
	}
	for _, s := range e.Stream {
		cfg.StreamSnippets = append(cfg.StreamSnippets, resolveUpstreams(s, upstreamName))
	}
	cfg.Server.ServerSnippets = append(cfg.Server.ServerSnippets, e.StreamServer...)
	if e.ProxyPass != "" {
		cfg.Server.ProxyPass = resolveUpstreams(e.ProxyPass, upstreamName)
	}
	if e.DefaultServer {
		cfg.Server.DefaultServer = true
	}
	return nil
}
