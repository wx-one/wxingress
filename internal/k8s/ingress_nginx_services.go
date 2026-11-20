package k8s

import (
	"fmt"
	"strconv"

	api_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wx-one/wxingress/internal/configs"
	nl "github.com/wx-one/wxingress/internal/logger"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

// streamServicesUser is the user of the ConfigMaps of -tcp-services-configmap
// and -udp-services-configmap in referencedConfigMaps.
const streamServicesUser = "-stream-services"

// ingressNginxServices are ingress-nginx's tcp/udp services and default
// backend, which the controller keeps in step with their Services.
type ingressNginxServices struct {
	tcp, udp       string
	defaultBackend string
	// streamServices are the Services the stream servers pass to, as namespace/name
	streamServices map[string]bool
}

func (lbc *LoadBalancerController) initIngressNginxServices(input NewLoadBalancerControllerInput) {
	lbc.inxServices = ingressNginxServices{
		tcp:            input.TCPServicesConfigMap,
		udp:            input.UDPServicesConfigMap,
		defaultBackend: input.DefaultBackendService,
	}
	if lbc.configMapUsers == nil {
		lbc.configMapUsers = map[string]func(){}
	}
	if input.TCPServicesConfigMap != "" || input.UDPServicesConfigMap != "" {
		lbc.configMapUsers[streamServicesUser] = lbc.syncIngressNginxStreams
	}
}

// syncIngressNginxServicesOf configures again what of ingress-nginx's
// services the Service svc (namespace/name) is: a backend of a stream server,
// the default backend.
func (lbc *LoadBalancerController) syncIngressNginxServicesOf(svc string) {
	if !lbc.isNginxReady {
		return
	}
	if lbc.inxServices.streamServices[svc] {
		lbc.syncIngressNginxStreams()
	}
	if svc == lbc.inxServices.defaultBackend {
		lbc.syncIngressNginxDefaultBackend()
	}
}

// syncIngressNginxStreams makes the entries of the tcp and udp services
// ConfigMaps the stream servers of the controller.
func (lbc *LoadBalancerController) syncIngressNginxStreams() {
	s := &lbc.inxServices
	var keys []string
	for _, k := range []string{s.tcp, s.udp} {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	cms := lbc.useConfigMaps(streamServicesUser, keys)
	services := map[string]bool{}
	var tsExes []*configs.TransportServerEx
	for _, c := range []struct {
		key string
		udp bool
	}{{s.tcp, false}, {s.udp, true}} {
		if c.key == "" {
			continue
		}
		cm := cms[c.key]
		if cm == nil {
			nl.Warnf(lbc.Logger, "the ConfigMap %s of the %s services does not exist", c.key, protocolOf(c.udp))
			continue
		}
		entries, warnings := configs.ParseIngressNginxStreamServices(cm.Data, c.udp)
		for _, e := range entries {
			services[e.Namespace+"/"+e.Service] = true
			tsEx, err := lbc.ingressNginxStream(e)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("port %d: %v", e.Port, err))
				continue
			}
			tsExes = append(tsExes, tsEx)
		}
		for _, w := range warnings {
			lbc.recorder.Eventf(cm, api_v1.EventTypeWarning, nl.EventReasonRejected, "%s services: %s", protocolOf(c.udp), w)
		}
	}
	s.streamServices = services
	if err := lbc.configurator.ReplaceIngressNginxStreams(tsExes); err != nil {
		nl.Errorf(lbc.Logger, "error configuring the tcp and udp services: %v", err)
	}
}

func protocolOf(udp bool) string {
	if udp {
		return "UDP"
	}
	return "TCP"
}

// ingressNginxStream is the TransportServer of an entry of the tcp or udp
// services, with ingress-nginx's stream settings: 600 s of proxy_timeout,
// next upstream on, 3 tries in 600 s, one response a UDP datagram.
func (lbc *LoadBalancerController) ingressNginxStream(e configs.IngressNginxStreamService) (*configs.TransportServerEx, error) {
	svc, err := lbc.getServiceFromInformer(e.Namespace, e.Service)
	if err != nil {
		return nil, err
	}
	port := 0
	for _, p := range svc.Spec.Ports {
		if p.Name == e.ServicePort || strconv.Itoa(int(p.Port)) == e.ServicePort {
			port = int(p.Port)
			break
		}
	}
	if port == 0 {
		return nil, fmt.Errorf("the Service %s/%s has no port %s", e.Namespace, e.Service, e.ServicePort)
	}
	protocol := protocolOf(e.UDP)
	params := &conf_v1.UpstreamParameters{NextUpstream: true, NextUpstreamTries: 3, NextUpstreamTimeout: "600s"}
	if e.UDP {
		one := 1
		params = &conf_v1.UpstreamParameters{UDPResponses: &one}
	}
	ts := &conf_v1.TransportServer{
		ObjectMeta: meta_v1.ObjectMeta{Namespace: e.Namespace, Name: e.Name()},
		Spec: conf_v1.TransportServerSpec{
			Listener:           conf_v1.TransportServerListener{Name: e.Name(), Protocol: protocol},
			Upstreams:          []conf_v1.TransportServerUpstream{{Name: "backend", Service: e.Service, Port: port}},
			UpstreamParameters: params,
			SessionParameters:  &conf_v1.SessionParameters{Timeout: "600s"},
			Action:             &conf_v1.TransportServerAction{Pass: "backend"},
		},
	}
	endpoints, _, err := lbc.getEndpointsForUpstream(e.Namespace, e.Service, uint16(port)) //nolint:gosec // a port
	if err != nil {
		nl.Debugf(lbc.Logger, "no endpoints of %s/%s:%d: %v", e.Namespace, e.Service, port, err)
	}
	return &configs.TransportServerEx{
		ListenerPort:    e.Port,
		TransportServer: ts,
		Endpoints: map[string][]string{
			configs.GenerateEndpointsKey(e.Namespace, e.Service, nil, uint16(port)): getIPAddressesFromEndpoints(endpoints), //nolint:gosec // a port
		},
		DisableIPV6:           lbc.isIPV6Disabled,
		ProxyProtocol:         e.Decode,
		UpstreamProxyProtocol: e.Encode,
	}, nil
}

// syncIngressNginxDefaultBackend gives the controller's default backend the
// endpoints of the first port of its Service.
func (lbc *LoadBalancerController) syncIngressNginxDefaultBackend() {
	key := lbc.inxServices.defaultBackend
	if key == "" {
		return
	}
	ns, name, _ := ParseNamespaceName(key)
	var ips []string
	svc, err := lbc.getServiceFromInformer(ns, name)
	switch {
	case err != nil:
		nl.Warnf(lbc.Logger, "the default backend %s: %v", key, err)
	case len(svc.Spec.Ports) == 0:
		nl.Warnf(lbc.Logger, "the default backend %s has no ports", key)
	default:
		endpoints, _, err := lbc.getEndpointsForUpstream(ns, name, uint16(svc.Spec.Ports[0].Port)) //nolint:gosec // a port
		if err != nil {
			nl.Debugf(lbc.Logger, "no endpoints of the default backend %s: %v", key, err)
		}
		ips = getIPAddressesFromEndpoints(endpoints)
	}
	if err := lbc.configurator.SetIngressNginxDefaultBackend(ips); err != nil {
		nl.Errorf(lbc.Logger, "error configuring the default backend %s: %v", key, err)
	}
}
