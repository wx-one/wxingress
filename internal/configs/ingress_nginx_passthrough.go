package configs

import (
	"strings"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IngressNginxSSLPassthroughAnnotation is ingress-nginx's ssl-passthrough.
const IngressNginxSSLPassthroughAnnotation = IngressNginxAnnotationPrefix + "ssl-passthrough"

func init() {
	ingressNginxRead[IngressNginxSSLPassthroughAnnotation] = true
}

// ingressNginxPassthroughs are the TransportServers of the TLS passthrough
// listener that ingress-nginx's ssl-passthrough makes of an Ingress: per host,
// the TLS connections of that name go to the Service of its path / (of its
// first path), untouched, as ingress-nginx sends them.
func ingressNginxPassthroughs(ingEx *IngressEx) ([]*TransportServerEx, Warnings) {
	warnings := newWarnings()
	ing := ingEx.Ingress
	if strings.TrimSpace(ing.Annotations[IngressNginxSSLPassthroughAnnotation]) != "true" {
		return nil, warnings
	}
	var out []*TransportServerEx
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" || rule.HTTP == nil || len(rule.HTTP.Paths) == 0 || !ingEx.ValidHosts[rule.Host] {
			continue
		}
		backend := rule.HTTP.Paths[0].Backend
		for _, p := range rule.HTTP.Paths {
			if pathOrDefault(p.Path) == "/" {
				backend = p.Backend
				break
			}
		}
		if backend.Service == nil || backend.Service.Port.Number == 0 {
			warnings.AddWarningf(ing, "%s for host %q needs a Service backend with a port number", IngressNginxSSLPassthroughAnnotation, rule.Host)
			continue
		}
		port := int(backend.Service.Port.Number)
		ts := &conf_v1.TransportServer{
			ObjectMeta: meta_v1.ObjectMeta{Namespace: ing.Namespace, Name: "ingress-" + ing.Name + "-" + strings.ReplaceAll(rule.Host, "*", "wildcard"), UID: ing.UID},
			Spec: conf_v1.TransportServerSpec{
				Listener:  conf_v1.TransportServerListener{Name: conf_v1.TLSPassthroughListenerName, Protocol: conf_v1.TLSPassthroughListenerProtocol},
				Host:      rule.Host,
				Upstreams: []conf_v1.TransportServerUpstream{{Name: "backend", Service: backend.Service.Name, Port: port}},
				Action:    &conf_v1.TransportServerAction{Pass: "backend"},
			},
		}
		out = append(out, &TransportServerEx{
			TransportServer: ts,
			Endpoints: map[string][]string{
				GenerateEndpointsKey(ing.Namespace, backend.Service.Name, nil, uint16(port)): ingEx.Endpoints[backend.Service.Name+GetBackendPortAsString(backend.Service.Port)], //nolint:gosec // a port
			},
		})
	}
	return out, warnings
}
