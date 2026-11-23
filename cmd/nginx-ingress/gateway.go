package main

import (
	"context"
	"time"

	api_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	"github.com/wx-one/wxingress/internal/gateway"
	"github.com/wx-one/wxingress/internal/k8s"
	nl "github.com/wx-one/wxingress/internal/logger"
)

// The rate of the clients of the Gateway API.
const (
	gatewayClientQPS   = 50
	gatewayClientBurst = 100
)

// newGatewayAPI makes what serves the Gateway API, or nil without
// -enable-gateway-api.
func newGatewayAPI(ctx context.Context, config *rest.Config, kubeClient kubernetes.Interface, namespace string) *gateway.Controller {
	if !*enableGatewayAPI {
		return nil
	}
	l := nl.LoggerFromContext(ctx)
	// a client of its own: one change of a Gateway is the status of every
	// route and listener, which client-go's default of 5 requests a second
	// would spread over seconds
	config = rest.CopyConfig(config)
	config.QPS = gatewayClientQPS
	config.Burst = gatewayClientBurst
	gwClient, err := gwclient.NewForConfig(config)
	if err != nil {
		nl.Fatalf(l, "Failed to create the Gateway API client: %v", err)
	}
	kubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		nl.Fatalf(l, "Failed to create the Kubernetes client of the Gateway API: %v", err)
	}
	var defaultGW types.NamespacedName
	if *defaultGateway != "" {
		ns, name, _ := k8s.ParseNamespaceName(*defaultGateway)
		defaultGW = types.NamespacedName{Namespace: ns, Name: name}
	}
	first, last, _ := parsePortRange(*gatewayListenerPorts)
	return gateway.New(gateway.Config{
		KubeClient:       kubeClient,
		GatewayClient:    gwClient,
		Logger:           l,
		ControllerName:   *gatewayControllerName,
		GatewayClass:     *gatewayClass,
		IngressClass:     *ingressClass,
		HTTPPort:         int32(*defaultHTTPListenerPort),  //nolint:gosec // validated as a port
		HTTPSPort:        int32(*defaultHTTPSListenerPort), //nolint:gosec // validated as a port
		DefaultGateway:   defaultGW,
		DynamicUpstreams: *dynamicUpstreams,
		EnableSnippets:   *enableSnippets,
		Namespace:        namespace,
		ExternalService:  *externalService,
		ServiceType:      api_v1.ServiceType(*gatewayServiceType),
		ListenerPortMin:  first,
		ListenerPortMax:  last,
		ReservedPorts:    gatewayReservedPorts(),
		Resync:           30 * time.Second,
		ListenerSets:     hasListenerSets(ctx, kubeClient),
	})
}

// hasListenerSets tells whether the cluster has the ListenerSet CRD, which
// the standard channel of the Gateway API has since 1.5: without it, an
// informer of ListenerSets would never sync. The CRD is looked for once, at
// the start - one installed later takes a restart.
func hasListenerSets(ctx context.Context, kubeClient kubernetes.Interface) bool {
	l := nl.LoggerFromContext(ctx)
	res, err := kubeClient.Discovery().ServerResourcesForGroupVersion(gatewayv1.GroupVersion.String())
	if err != nil {
		nl.Warnf(l, "Could not tell whether the cluster has the ListenerSet CRD, ListenerSets are not served: %v", err)
		return false
	}
	for _, r := range res.APIResources {
		if r.Name == "listenersets" {
			return true
		}
	}
	nl.Warnf(l, "The cluster has no ListenerSet CRD (%s), ListenerSets are not served: install the CRDs of Gateway API 1.5 or later and restart", gatewayv1.GroupVersion)
	return false
}
