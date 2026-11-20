package gateway

import (
	"slices"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/features"
)

// SupportedFeatures is what wxingress claims of the Gateway API, in the
// status of its GatewayClass - where the conformance suite reads it - in
// place of NGF's list, which names what NGF serves. A feature goes here
// when its conformance tests pass (docs/developer/upstream.md).
var SupportedFeatures = []features.FeatureName{
	// core
	features.SupportGateway,
	features.SupportHTTPRoute,
	features.SupportReferenceGrant,
	features.SupportGRPCRoute,
	features.SupportTLSRoute,
	features.SupportTCPRoute,
	features.SupportUDPRoute,

	// TLSRoute, extended (Terminate on a TransportServer with TLS)
	features.SupportTLSRouteModeTerminate,

	// Gateway, extended
	features.SupportGatewayPort8080,
	features.SupportGatewayHTTPListenerIsolation,
	features.SupportGatewayAddressEmpty,
	features.SupportGatewayInfrastructure,

	// HTTPRoute, extended: matches
	features.SupportHTTPRouteMethodMatching,
	features.SupportHTTPRouteQueryParamMatching,
	features.SupportHTTPRouteParentRefPort,
	features.SupportHTTPRouteDestinationPortMatching,

	// HTTPRoute, extended: filters (internal/gateway/filters.go)
	features.SupportHTTPRouteResponseHeaderModification,
	features.SupportHTTPRoutePathRewrite,
	features.SupportHTTPRouteHostRewrite,
	features.SupportHTTPRoutePathRedirect,
	features.SupportHTTPRoutePortRedirect,
	features.SupportHTTPRouteSchemeRedirect,
	features.SupportHTTPRoute303RedirectStatusCode,
	features.SupportHTTPRoute307RedirectStatusCode,
	features.SupportHTTPRoute308RedirectStatusCode,
	features.SupportHTTPRouteRequestMirror,
	features.SupportHTTPRouteRequestMultipleMirrors,
	features.SupportHTTPRouteRequestPercentageMirror,
	features.SupportHTTPRouteCORS,

	// HTTPRoute, extended: backends
	features.SupportHTTPRouteBackendProtocolWebSocket,
	features.SupportHTTPRouteBackendProtocolH2C,
}

func supportedFeatures() []gatewayv1.SupportedFeature {
	out := make([]gatewayv1.SupportedFeature, 0, len(SupportedFeatures))
	for _, f := range SupportedFeatures {
		out = append(out, gatewayv1.SupportedFeature{Name: gatewayv1.FeatureName(f)})
	}
	slices.SortFunc(out, func(a, b gatewayv1.SupportedFeature) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out
}
