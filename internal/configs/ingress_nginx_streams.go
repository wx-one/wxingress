package configs

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/nginx"
)

// IngressNginxStreamService is an entry of ingress-nginx's tcp-services or
// udp-services ConfigMap: "<port>": "<namespace>/<service>:<port>[:PROXY][:PROXY]".
type IngressNginxStreamService struct {
	Port      int
	UDP       bool
	Namespace string
	Service   string
	// ServicePort is the port of the Service, a number or a name.
	ServicePort string
	// Decode takes the PROXY protocol of the client (the first PROXY), Encode sends it to the Service (the second).
	Decode bool
	Encode bool
}

// Name is the name of the listener and the TransportServer of the entry.
func (s IngressNginxStreamService) Name() string {
	proto := "tcp"
	if s.UDP {
		proto = "udp"
	}
	return fmt.Sprintf("ingress-nginx-%s-%d", proto, s.Port)
}

// ParseIngressNginxStreamServices reads a tcp-services (udp false) or
// udp-services ConfigMap as ingress-nginx does, sorted by port. An entry it
// cannot take is left out, with a warning.
func ParseIngressNginxStreamServices(data map[string]string, udp bool) ([]IngressNginxStreamService, []string) {
	var out []IngressNginxStreamService
	var warnings []string
	keys := slices.Collect(maps.Keys(data))
	sort.Strings(keys)
	for _, key := range keys {
		value := strings.TrimSpace(data[key])
		port, err := strconv.Atoi(strings.TrimSpace(key))
		if err != nil || port < 1 || port > 65535 {
			warnings = append(warnings, fmt.Sprintf("%q is not a port", key))
			continue
		}
		parts := strings.Split(value, ":")
		if len(parts) < 2 || len(parts) > 4 {
			warnings = append(warnings, fmt.Sprintf("port %d: %q is not <namespace>/<service>:<port>[:PROXY][:PROXY]", port, value))
			continue
		}
		ns, svc, found := strings.Cut(parts[0], "/")
		if !found || ns == "" || svc == "" || strings.Contains(svc, "/") || parts[1] == "" {
			warnings = append(warnings, fmt.Sprintf("port %d: %q is not <namespace>/<service>:<port>", port, value))
			continue
		}
		s := IngressNginxStreamService{Port: port, UDP: udp, Namespace: ns, Service: svc, ServicePort: parts[1]}
		bad := false
		// as ingress-nginx: PROXY (any case) or nothing, "<port>::PROXY" for sending it only
		for i, p := range parts[2:] {
			switch strings.ToUpper(p) {
			case "":
				continue
			case "PROXY":
			default:
				warnings = append(warnings, fmt.Sprintf("port %d: %q is not PROXY", port, p))
				bad = true
			}
			if bad {
				break
			}
			if i == 0 {
				s.Decode = true
			} else {
				s.Encode = true
			}
		}
		if bad {
			continue
		}
		if udp && (s.Decode || s.Encode) {
			// as ingress-nginx: no PROXY protocol for UDP
			s.Decode, s.Encode = false, false
			warnings = append(warnings, fmt.Sprintf("port %d: the PROXY protocol is for TCP only and is ignored", port))
		}
		out = append(out, s)
	}
	return out, warnings
}

// ReplaceIngressNginxStreams makes tsExes - the TransportServers of
// ingress-nginx's tcp-services and udp-services - the stream servers of
// those ConfigMaps in place of those it had. A change of their endpoints
// only goes to the wxingress module with -dynamic-upstreams.
func (cnf *Configurator) ReplaceIngressNginxStreams(tsExes []*TransportServerEx) error {
	if cnf.ingressNginxStreams == nil {
		cnf.ingressNginxStreams = map[string]bool{}
	}
	changed := false
	keep := map[string]bool{}
	var unchanged []*TransportServerEx
	for _, tsEx := range tsExes {
		key := tsEx.TransportServer.Namespace + "/" + tsEx.TransportServer.Name
		keep[key] = true
		c, _, err := cnf.addOrUpdateTransportServer(tsEx)
		if err != nil {
			return fmt.Errorf("error adding the stream service %v: %w", key, err)
		}
		if c {
			changed = true
		} else {
			unchanged = append(unchanged, tsEx)
		}
	}
	for key := range cnf.ingressNginxStreams {
		if !keep[key] {
			if err := cnf.deleteTransportServer(key); err != nil {
				return err
			}
			changed = true
		}
	}
	cnf.ingressNginxStreams = keep
	if changed {
		return cnf.Reload(nginx.ReloadForOtherUpdate)
	}
	if cnf.dynamicMode() && len(unchanged) > 0 {
		return cnf.applyDynamically(nil, nil, nil, unchanged)
	}
	return nil
}

// ingressNginxDefaultBackendUpstream is the upstream of -default-backend-service.
const ingressNginxDefaultBackendUpstream = "ingress-nginx-default-backend"

// DefaultBackendConfigName is the file of the upstream of -default-backend-service.
const DefaultBackendConfigName = "_default-backend"

// SetIngressNginxDefaultBackend gives the controller's default backend
// (-default-backend-service) its endpoints: the upstream every server
// without a path for a request passes it to. The first call writes its
// configuration; later ones change only the servers of the upstream, which
// go to the wxingress module with -dynamic-upstreams.
func (cnf *Configurator) SetIngressNginxDefaultBackend(endpoints []string) error {
	upstream := version1.Upstream{
		Name:             ingressNginxDefaultBackendUpstream,
		LBMethod:         cnf.CfgParams.LBMethod,
		UpstreamZoneSize: upstreamZoneSize(cnf.CfgParams.UpstreamZoneSize, cnf.staticCfgParams.UpstreamAPI),
		MetaBalancer:     cnf.staticCfgParams.UpstreamAPI,
	}
	if cnf.CfgParams.Keepalive > 0 {
		upstream.Keepalive = strconv.Itoa(cnf.CfgParams.Keepalive)
	}
	for _, e := range endpoints {
		upstream.UpstreamServers = append(upstream.UpstreamServers, version1.UpstreamServer{
			Address: e, MaxFails: cnf.CfgParams.MaxFails, MaxConns: cnf.CfgParams.MaxConns, FailTimeout: cnf.CfgParams.FailTimeout,
		})
	}
	if len(upstream.UpstreamServers) == 0 {
		// as for a Service without endpoints: nginx answers 502
		upstream.UpstreamServers = []version1.UpstreamServer{{Address: nginx502Server, MaxFails: 1, FailTimeout: "10s"}}
	}
	first := !cnf.hasDefaultBackend
	cnf.hasDefaultBackend = true
	content, err := cnf.templateExecutor.ExecuteIngressConfigTemplate(&version1.IngressNginxConfig{
		Upstreams: []version1.Upstream{upstream},
		Ingress:   version1.Ingress{Name: "default-backend", Namespace: "-"},
	})
	if err != nil {
		return fmt.Errorf("error generating the default backend: %w", err)
	}
	fileChanged, err := cnf.nginxManager.CreateConfig(DefaultBackendConfigName, content)
	if err != nil {
		return fmt.Errorf("error writing the default backend: %w", err)
	}
	changed := cnf.configChanged(DefaultBackendConfigName, content, fileChanged)
	cnf.setRenderedServers(DefaultBackendConfigName, renderedServersOfIngress([]version1.Upstream{upstream}))
	if first {
		// the servers without a path for a request pass it there now
		if err := cnf.syncDefaultServerConfig(); err != nil {
			return err
		}
		return cnf.Reload(nginx.ReloadForOtherUpdate)
	}
	if changed || !cnf.dynamicMode() {
		if fileChanged {
			return cnf.Reload(nginx.ReloadForEndpointsUpdate)
		}
		return nil
	}
	servers := make([]string, 0, len(endpoints))
	servers = append(servers, endpoints...)
	if err := cnf.updateServers(ingressNginxDefaultBackendUpstream, servers, nginx.ServerConfig{
		MaxFails: cnf.CfgParams.MaxFails, MaxConns: cnf.CfgParams.MaxConns, FailTimeout: cnf.CfgParams.FailTimeout,
	}); err != nil {
		return cnf.Reload(nginx.ReloadForEndpointsUpdate)
	}
	return nil
}

// IngressNginxDefaultBackend is the upstream of -default-backend-service, if the controller has one.
func (cnf *Configurator) ingressNginxDefaultBackend() string {
	if cnf.hasDefaultBackend {
		return ingressNginxDefaultBackendUpstream
	}
	return ""
}
