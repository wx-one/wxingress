package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"

	"github.com/wx-one/wxingress/internal/gateway/extras"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/dataplane"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph"
)

// TLS passthrough, TCP and UDP listeners go onto TransportServers, the
// controller's stream servers - whose endpoints change without reloads with
// -dynamic-upstreams (6eaafba99) - on the nginx port of the listener:
//
//   - TLSRoute (passthrough): a TransportServer per hostname, whose stream
//     server has server_name and ssl_preread, so that nginx picks it by the
//     name the client sent; a TransportServer without a host is the port's
//     default and closes what no hostname takes.
//   - TCPRoute, UDPRoute: a TransportServer per listener with the backends
//     of its route; with more than one, split_clients by weight.
//
// A TransportServer is in the namespace of its first backend - or, with TLS
// termination, of its certificate - and exists only in memory; a backend in
// another namespace is its upstream's <namespace>/<name>, as a VirtualServer
// upstream names one. That a route may refer to it, a ReferenceGrant says,
// which NGF's graph checks.
//
// Only Gateways with addresses of their own have such listeners: the default
// Gateway is HTTP on 80 and HTTPS on 443.

const (
	// noBackend is the Service of the upstream of a TransportServer that
	// has none: it does not exist, and its server closes the connection.
	noBackend = "wxingress-gateway-no-backend"
	// closedSocket is where a stream server sends what it does not take:
	// nothing listens there, and the connection is closed.
	closedSocket = "unix:/var/lib/nginx/non-existing-unix-socket.sock"
)

// IsGatewayTransportServer tells whether ts was made from a Gateway.
func IsGatewayTransportServer(ts *conf_v1.TransportServer) bool {
	return ts != nil && strings.HasPrefix(ts.Name, NamePrefix)
}

// streamListenerName names the nginx listener of a TLS, TCP or UDP port of a
// Gateway.
func streamListenerName(gw types.NamespacedName, p listenerPort) string {
	return ListenerName(gw, p.port) + "-" + strings.ToLower(string(p.protocol))
}

// transportServerName names a TransportServer of a Gateway: its port,
// protocol and hostname.
func transportServerName(gw types.NamespacedName, p listenerPort, host string) string {
	sum := sha256.Sum256([]byte(gw.String() + "\n" + p.portName() + "\n" + host))
	return NamePrefix + hex.EncodeToString(sum[:8])
}

// mappedL4 is what the stream listeners of one Gateway map onto.
type mappedL4 struct {
	transportServers []*conf_v1.TransportServer
	listeners        []conf_v1.Listener
	skipped          []string
}

// toTransportServers maps the TLS passthrough, TCP and UDP servers of a
// Gateway with an address of its own onto TransportServers.
func toTransportServers(gw *graph.Gateway, cfg dataplane.Configuration, idx upstreamIndex, opts mapOptions, addr gatewayAddress) mappedL4 {
	var out mappedL4
	gwName := types.NamespacedName{Namespace: gw.Source.Namespace, Name: gw.Source.Name}
	if addr.shared {
		return out
	}

	listeners := make(map[listenerPort]conf_v1.Listener)
	listener := func(port int32, proto corev1.Protocol) (string, bool) {
		p := listenerPort{port: port, protocol: proto}
		n, ok := addr.nginxPorts[p]
		if !ok {
			out.skipped = append(out.skipped, fmt.Sprintf("Gateway %s: port %s has no nginx port yet", gwName, p.portName()))
			return "", false
		}
		name := streamListenerName(gwName, p)
		listeners[p] = conf_v1.Listener{Name: name, Protocol: string(proto), Port: int(n)}
		return name, true
	}

	// TLS: a server per hostname, and one per port for the rest. With
	// Passthrough nginx reads the name from the ClientHello (ssl_preread);
	// with Terminate the stream server terminates TLS with the listener's
	// certificate, and the name is the one of the handshake.
	defaults := make(map[int32]bool)
	for _, s := range cfg.TLSServers {
		name, ok := listener(s.Port, corev1.ProtocolTCP)
		if !ok {
			continue
		}
		p := listenerPort{port: s.Port, protocol: corev1.ProtocolTCP}
		var secret *types.NamespacedName
		if s.SSL != nil {
			if len(s.SSL.KeyPairIDs) == 0 || s.SSL.VerifyClient != "" || s.SSL.ClientCertBundleID != "" {
				// client certificate validation is not carried into the
				// TransportServer: not served rather than served without it
				continue
			}
			sec, ok := secretOfKeyPair(s.SSL.KeyPairIDs[0])
			if !ok {
				continue
			}
			secret = &sec
		}
		if !defaults[s.Port] {
			defaults[s.Port] = true
			ns := gwName.Namespace
			e := &extras.Extras{ProxyPass: closedSocket, DefaultServer: true}
			if secret == nil {
				e.StreamServer = []string{"ssl_preread on;"}
			} else {
				ns = secret.Namespace
			}
			def := newTransportServer(gwName, gw, p, name, "", ns, opts,
				[]conf_v1.TransportServerUpstream{{Name: "none", Service: noBackend, Port: 1}}, e)
			if secret != nil {
				def.Spec.TLS = &conf_v1.TransportServerTLS{Secret: secret.Name}
			}
			out.add(def)
		}
		if s.IsDefault || s.Hostname == "" || len(s.Upstreams) == 0 {
			continue
		}
		within := ""
		if secret != nil {
			within = secret.Namespace
		}
		ups, ns := out.upstreams(s.Upstreams, idx, within)
		if len(ups) == 0 {
			continue
		}
		if secret == nil {
			out.add(newTransportServer(gwName, gw, p, name, s.Hostname, ns, opts, ups[:1],
				&extras.Extras{StreamServer: []string{"ssl_preread on;"}}))
			continue
		}
		ts := newTransportServer(gwName, gw, p, name, s.Hostname, ns, opts, ups[:1], nil)
		ts.Spec.TLS = &conf_v1.TransportServerTLS{Secret: secret.Name}
		out.add(ts)
	}

	// TCP and UDP: the route of a listener
	for _, set := range []struct {
		servers []dataplane.Layer4VirtualServer
		proto   corev1.Protocol
	}{{cfg.TCPServers, corev1.ProtocolTCP}, {cfg.UDPServers, corev1.ProtocolUDP}} {
		for _, s := range set.servers {
			name, ok := listener(s.Port, set.proto)
			if !ok {
				continue
			}
			ups, ns := out.upstreams(s.Upstreams, idx, "")
			if len(ups) == 0 {
				continue
			}
			p := listenerPort{port: s.Port, protocol: set.proto}
			var e *extras.Extras
			if len(ups) > 1 {
				e = weighted(transportServerName(gwName, p, ""), ups, s.Upstreams)
			}
			out.add(newTransportServer(gwName, gw, p, name, "", ns, opts, ups, e))
		}
	}

	for _, p := range slices.SortedFunc(maps.Keys(listeners), func(a, b listenerPort) int {
		return strings.Compare(a.portName(), b.portName())
	}) {
		out.listeners = append(out.listeners, listeners[p])
	}
	return out
}

func (out *mappedL4) add(ts *conf_v1.TransportServer) {
	out.transportServers = append(out.transportServers, ts)
}

// upstreams are the TransportServer upstreams of the backends of a server
// and the namespace of the TransportServer: ns, or that of the first
// backend. The Service of a backend in another namespace is
// <namespace>/<name>.
func (out *mappedL4) upstreams(backends []dataplane.Layer4Upstream, idx upstreamIndex, ns string) ([]conf_v1.TransportServerUpstream, string) {
	var ups []conf_v1.TransportServerUpstream
	for _, b := range backends {
		sp, ok := idx[b.Name]
		if !ok {
			continue
		}
		if ns == "" {
			ns = sp.svc.Namespace
		}
		svc := sp.svc.Name
		if sp.svc.Namespace != ns {
			svc = sp.svc.Namespace + "/" + sp.svc.Name
		}
		sum := sha256.Sum256([]byte(b.Name))
		ups = append(ups, conf_v1.TransportServerUpstream{
			Name:    "be-" + hex.EncodeToString(sum[:5]),
			Service: svc,
			Port:    int(sp.port),
		})
	}
	return ups, ns
}

// weighted splits the connections of a TransportServer among its upstreams
// by the weights of the backends.
func weighted(tsName string, ups []conf_v1.TransportServerUpstream, backends []dataplane.Layer4Upstream) *extras.Extras {
	weights := make([]int64, 0, len(ups))
	byName := make(map[string]int64)
	for _, b := range backends {
		sum := sha256.Sum256([]byte(b.Name))
		byName["be-"+hex.EncodeToString(sum[:5])] = int64(b.Weight)
	}
	for _, u := range ups {
		weights = append(weights, byName[u.Name])
	}
	v := "wxgw_" + strings.ToLower(strings.TrimPrefix(tsName, NamePrefix)) + "_split"
	var b strings.Builder
	fmt.Fprintf(&b, "split_clients \"${remote_addr}${remote_port}${msec}\" $%s {\n", v)
	pcts := percentages(weights)
	last := -1
	for i, pct := range pcts {
		if pct > 0 {
			last = i
		}
	}
	for i, pct := range pcts {
		switch {
		case pct == 0:
		case i == last:
			fmt.Fprintf(&b, "    * %s;\n", extras.UpstreamRef(ups[i].Name))
		default:
			fmt.Fprintf(&b, "    %d%% %s;\n", pct, extras.UpstreamRef(ups[i].Name))
		}
	}
	b.WriteString("}")
	return &extras.Extras{Stream: []string{b.String()}, ProxyPass: "$" + v}
}

func newTransportServer(
	gwName types.NamespacedName,
	gw *graph.Gateway,
	p listenerPort,
	listener, host, ns string,
	opts mapOptions,
	ups []conf_v1.TransportServerUpstream,
	e *extras.Extras,
) *conf_v1.TransportServer {
	ts := &conf_v1.TransportServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              transportServerName(gwName, p, host),
			CreationTimestamp: gw.Source.CreationTimestamp,
			Annotations: map[string]string{
				AnnotationGateway:  gwName.String(),
				AnnotationHostname: host,
				AnnotationPort:     strconv.Itoa(int(p.port)),
			},
		},
		Spec: conf_v1.TransportServerSpec{
			IngressClass: opts.ingressClass,
			Listener:     conf_v1.TransportServerListener{Name: listener, Protocol: string(p.protocol)},
			Host:         host,
			Upstreams:    ups,
			Action:       &conf_v1.TransportServerAction{Pass: ups[0].Name},
		},
	}
	if e != nil && !e.Empty() {
		ts.Annotations[extras.Annotation] = e.Encode()
	}
	return ts
}

// TransportServerForValidation returns ts as the TransportServer validator
// is to see it: the host of a TLS passthrough server is the server_name nginx
// picks it by, which the validator allows only with TLS termination, and the
// listener's name is one no GlobalConfiguration can have.
func TransportServerForValidation(ts *conf_v1.TransportServer) *conf_v1.TransportServer {
	cp := ts.DeepCopy()
	cp.Spec.Host = ""
	cp.Spec.Listener.Name = "gateway-listener"
	return cp
}
