package gateway

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wx-one/wxingress/internal/configs"
	"github.com/wx-one/wxingress/internal/gateway/extras"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

// What the Ingress path has beyond the Gateway API, the Gateway path takes
// the way the Ingress path does, through the same code:
//
//   - the ingress-nginx annotations of an HTTPRoute or GRPCRoute - timeouts,
//     sizes, buffering, next upstream, proxy-http-version, service-upstream,
//     load-balance, upstream-hash-by, affinity and session-cookie-* - are read
//     by the parsers of the Ingress path (configs.ParseGatewaySettings) and
//     go onto the VirtualServer upstreams of its backends, and with
//     -dynamic-upstreams the balancer rule onto the wxingress module, as an
//     Ingress's do;
//   - NGINX Ingress Controller's Policies - rate limits, basic auth, external
//     auth, access control, JWT, OIDC, mTLS, CORS, caching, HSTS, API keys,
//     WAF - attach to a route or a Gateway with the annotation
//     wxingress.wizardtales.com/policies, a list of Policies as a
//     VirtualServer names them (name or namespace/name), and take the
//     VirtualServer's path to nginx.

// AnnotationPolicies lists the Policies of a route or a Gateway.
const AnnotationPolicies = "wxingress.wizardtales.com/policies"

// ingressNginxPrefix starts the annotations of ingress-nginx.
const ingressNginxPrefix = configs.IngressNginxAnnotationPrefix

// policiesOf reads the Policies an object names.
func policiesOf(meta *metav1.ObjectMeta) []conf_v1.PolicyReference {
	if meta == nil {
		return nil
	}
	var out []conf_v1.PolicyReference
	for _, ref := range strings.Split(meta.Annotations[AnnotationPolicies], ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		ns, name, found := strings.Cut(ref, "/")
		if !found {
			ns, name = meta.Namespace, ref
		}
		out = append(out, conf_v1.PolicyReference{Name: name, Namespace: ns})
	}
	return out
}

// addPolicies adds refs to those of a route, once each.
func addPolicies(have []conf_v1.PolicyReference, refs []conf_v1.PolicyReference) []conf_v1.PolicyReference {
	for _, r := range refs {
		if !slices.Contains(have, r) {
			have = append(have, r)
		}
	}
	return have
}

// routeSettings are the settings of the backends of one route.
type routeSettings struct {
	key      string
	settings configs.GatewaySettings
	// warned tells that the restrictions it cannot apply were warned about
	warned bool
}

// settingsOf reads the ingress-nginx annotations of the route of a match
// rule, nil if it has none.
func (m *routeMapper) settingsOf(src *metav1.ObjectMeta) *routeSettings {
	if src == nil {
		return nil
	}
	annotations := make(map[string]string)
	for k, v := range src.Annotations {
		if strings.HasPrefix(k, ingressNginxPrefix) || k == serverSnippetsAnnotation || k == locationSnippetsAnnotation {
			annotations[k] = v
		}
	}
	if len(annotations) == 0 {
		return nil
	}
	key := src.Namespace + "/" + src.Name
	if s, ok := m.settings[key]; ok {
		return s
	}
	s := &routeSettings{key: key, settings: configs.ParseGatewaySettings(src.Namespace, src.Name, annotations, m.listener.snippets)}
	for _, w := range s.settings.Warnings {
		m.skipped = append(m.skipped, fmt.Sprintf("route %s: %s", key, w))
	}
	if s.settings.Empty() {
		s = nil
	}
	m.settings[key] = s
	return s
}

// The snippets of NGINX Ingress Controller a route takes, as an Ingress.
const (
	serverSnippetsAnnotation   = "nginx.org/server-snippets"
	locationSnippetsAnnotation = "nginx.org/location-snippets"
)

// routeExtras gives the action of a match rule what the annotations of its
// route put into its locations: the rate limit of limit-rps, limit-rpm,
// limit-connections, limit-rate and limit-rate-after, as an Ingress renders
// it - its zones named after the VirtualServer and the route, since each
// server of a Gateway is a config of its own - and the location snippets.
func (m *routeMapper) routeExtras(a *extras.Action, src *metav1.ObjectMeta) {
	s := m.settingsOf(src)
	if s == nil {
		return
	}
	if s.settings.Limits() {
		scope := m.ext.variable("limit", s.key)
		http, directives := s.settings.RateLimit(scope)
		if http != "" {
			m.ext.define(scope, http)
		}
		a.Directives = append(a.Directives, directives...)
	}
	a.Directives = append(a.Directives, s.settings.LocationSnippets...)
}

// serverSnippets puts the server snippets of the route of a match rule into
// the server, once.
func (m *routeMapper) serverSnippets(src *metav1.ObjectMeta) {
	if s := m.settingsOf(src); s != nil && len(s.settings.ServerSnippets) > 0 {
		m.ext.serverText("snippets "+s.key, strings.Join(s.settings.ServerSnippets, "\n"))
	}
}

// withSettings gives an upstream the fields the settings set.
func withSettings(u conf_v1.Upstream, s configs.GatewaySettings) conf_v1.Upstream {
	set := reflect.ValueOf(s.Upstream)
	out := reflect.ValueOf(&u).Elem()
	for i := 0; i < set.NumField(); i++ {
		if !set.Field(i).IsZero() {
			out.Field(i).Set(set.Field(i))
		}
	}
	return u
}

// balancers are the module's rules for the upstreams whose routes ask for
// one, as JSON for the extras.
func (m *routeMapper) balancers() map[string]json.RawMessage {
	out := make(map[string]json.RawMessage)
	for _, name := range slices.Sorted(maps.Keys(m.balanced)) {
		b := m.balanced[name]
		rule, ok := b.settings.settings.Balancer(b.backend, slices.Compact(slices.Sorted(slices.Values(b.paths))))
		if !ok {
			continue
		}
		raw, err := json.Marshal(rule)
		if err == nil {
			out[name] = raw
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// balancedUpstream is an upstream with a balancer rule.
type balancedUpstream struct {
	settings *routeSettings
	backend  string
	paths    []string
}

// ingressNginxBackend is ingress-nginx's name of a backend:
// <namespace>-<service>-<port>.
func ingressNginxBackend(sp servicePort) string {
	return sp.svc.Namespace + "-" + sp.svc.Name + "-" + strconv.Itoa(int(sp.port))
}
