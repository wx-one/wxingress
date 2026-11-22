package configs

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/k8s/policies"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
	"github.com/wx-one/wxingress/pkg/apis/dos/v1beta1"

	"github.com/wx-one/wxingress/internal/k8s/secrets"
	nl "github.com/wx-one/wxingress/internal/logger"
	api_v1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/nginx"
)

const (
	emptyHostName  = ""
	emptyHostToken = "_"
	minionContext  = "minion"
)

// AppProtectResources holds namespace names of App Protect resources relevant to an Ingress
type AppProtectResources struct {
	AppProtectPolicy   string
	AppProtectLogconfs []string
}

// AppProtectLog holds a single pair of log config and log destination
type AppProtectLog struct {
	LogConf *unstructured.Unstructured
	Dest    string
}

// IngressEx holds an Ingress along with the resources that are referenced in this Ingress.
type IngressEx struct {
	Ingress   *networking.Ingress
	Endpoints map[string][]string
	// ServiceAppProtocols holds the appProtocol of the Service port backing each configured
	// backend, keyed identically to Endpoints. Absent or unset appProtocols are not stored.
	ServiceAppProtocols map[string]string
	HealthChecks        map[string]*api_v1.Probe
	Policies            map[string]*conf_v1.Policy
	ApPolRefs           map[string]*unstructured.Unstructured
	LogConfRefs         map[string]*unstructured.Unstructured
	PolicyWarnings      []string
	ExternalNameSvcs    map[string]bool
	PodsByIP            map[string]PodInfo
	ValidHosts          map[string]bool
	ValidMinionPaths    map[string]bool
	AppProtectPolicy    *unstructured.Unstructured
	AppProtectLogs      []AppProtectLog
	DosEx               *DosEx
	SecretRefs          map[secrets.SecretRefKey]*secrets.SecretReference
	ZoneSync            bool
	// Canaries holds the canary Ingresses with a host of this Ingress, oldest
	// first, with the endpoints of their backends (-dynamic-upstreams).
	Canaries []*IngressEx
	// CanaryOf is, for a canary Ingress of ingress-nginx's per upstream (-ingress-nginx-defaults), the upstreams it is
	// the canary of - ingress-nginx's names, <namespace>-<service>-<port> - and its backend for each.
	CanaryOf map[string]networking.IngressBackend
	// Merged holds the other Ingresses with a host of this Ingress, oldest
	// first, each with that host as its only valid one. As ingress-nginx
	// does, their paths become locations of the server of the host.
	Merged []*IngressEx
	// WWWRedirects are, for ingress-nginx's from-to-www-redirect, the hosts of
	// the Ingress with the host that redirects to each - www.<host> or the host
	// without www. - where no resource has that host.
	WWWRedirects map[string]string
	// IngressNginxDefaultBackend is the Service of ingress-nginx's default-backend, at its first port.
	IngressNginxDefaultBackend *networking.IngressBackend
	// IngressNginxConfigMaps are the ConfigMaps of the Ingress's namespace its
	// custom-headers and auth-proxy-set-headers name (IngressNginxConfigMapRefs),
	// by namespace/name; nil for one that does not exist.
	IngressNginxConfigMaps map[string]*api_v1.ConfigMap
}

// DosEx holds a DosProtectedResource and the dos policy and log confs it references.
type DosEx struct {
	DosProtected *v1beta1.DosProtectedResource
	DosPolicy    *unstructured.Unstructured
	DosLogConf   *unstructured.Unstructured
}

// JWTKey represents a secret that holds JSON Web Key.
type JWTKey struct {
	Name   string
	Secret *api_v1.Secret
}

func (ingEx *IngressEx) String() string {
	if ingEx.Ingress == nil {
		return "IngressEx has no Ingress"
	}

	return fmt.Sprintf("%v/%v", ingEx.Ingress.Namespace, ingEx.Ingress.Name)
}

// MergeableIngresses is a mergeable ingress of a master and minions.
type MergeableIngresses struct {
	Master  *IngressEx
	Minions []*IngressEx
}

// NginxCfgParams is a collection of parameters
// used by generateNginxCfg() and generateNginxCfgForMergeableIngresses()
type NginxCfgParams struct {
	staticParams              *StaticConfigParams
	ingEx                     *IngressEx
	mergeableIngs             *MergeableIngresses
	apResources               *AppProtectResources
	dosResource               *appProtectDosResource
	BaseCfgParams             *ConfigParams
	isMinion                  bool
	isPlus                    bool
	isResolverConfigured      bool
	isWildcardEnabled         bool
	ingressControllerReplicas int
	oidcNativeLocations       map[string]oidcNativeLocationOwner
	// defaultBackend is the upstream of -default-backend-service, if any
	defaultBackend string
}

type ingressPolicyAnnotationRequirement struct {
	policyType string
	matches    func(*conf_v1.Policy) bool
}

// ingressPoliciesRequiringPlusAnnotation lists policy types that are valid on Ingress
// only when referenced via nginx.com/policies. Keeping this as a small table makes the
// restriction explicit and easy to extend if more Plus-only policy types are added later.
var ingressPoliciesRequiringPlusAnnotation = []ingressPolicyAnnotationRequirement{
	{
		policyType: "WAF",
		matches: func(policy *conf_v1.Policy) bool {
			return policy.Spec.WAF != nil
		},
	},
	{
		policyType: "OIDCNative",
		matches: func(policy *conf_v1.Policy) bool {
			return policy.Spec.OIDCNative != nil
		},
	},
}

var ingressWAFAnnotations = []string{
	"appprotect.f5.com/app-protect-enable",
	AppProtectPolicyAnnotation,
	"appprotect.f5.com/app-protect-security-log-enable",
	AppProtectLogConfAnnotation,
	AppProtectLogConfDstAnnotation,
}

// filterIngressPolicyRefs removes policy references that are not allowed for the
// annotation that introduced them. The Ingress model only keeps a merged policy map,
// so this function reconstructs which refs came from nginx.org/policies and prevents
// Plus-only policy types from becoming effective through that annotation.
//
// The returned bool mirrors generatePolicies() fatal validation behavior: if true,
// the caller must return a 500 for the Ingress because an attached policy is invalid
// in this context.
func filterIngressPolicyRefs(policyRefs []conf_v1.PolicyReference, ingEx *IngressEx) ([]conf_v1.PolicyReference, Warnings, bool) {
	warnings := newWarnings()
	if len(policyRefs) == 0 || ingEx == nil || ingEx.Ingress == nil || len(ingEx.Policies) == 0 {
		return policyRefs, warnings, false
	}

	policyNames, exists := ingEx.Ingress.Annotations[PoliciesAnnotation]
	if !exists {
		return policyRefs, warnings, false
	}

	policyRefsFromOrgAnnotation := make(map[string]bool)
	for _, ref := range policies.GetPolicyRefsFromAnnotation(policyNames, ingEx.Ingress.Namespace) {
		resourceRef := ref.Name
		if ref.Namespace != "" {
			resourceRef = fmt.Sprintf("%s/%s", ref.Namespace, ref.Name)
		}
		namespace, resourceName := ParseResourceReference(resourceRef, ingEx.Ingress.Namespace)
		policyRefsFromOrgAnnotation[fmt.Sprintf("%s/%s", namespace, resourceName)] = true
	}

	filteredPolicyRefs := make([]conf_v1.PolicyReference, 0, len(policyRefs))
	for _, ref := range policyRefs {
		resourceRef := ref.Name
		if ref.Namespace != "" {
			resourceRef = fmt.Sprintf("%s/%s", ref.Namespace, ref.Name)
		}
		namespace, resourceName := ParseResourceReference(resourceRef, ingEx.Ingress.Namespace)
		key := fmt.Sprintf("%s/%s", namespace, resourceName)
		policy, exists := ingEx.Policies[key]
		if exists && policyRefsFromOrgAnnotation[key] {
			for _, requirement := range ingressPoliciesRequiringPlusAnnotation {
				if requirement.matches(policy) {
					warnings.AddWarningf(ingEx.Ingress, "%s policy %s is not supported in annotation %s; use %s", requirement.policyType, key, PoliciesAnnotation, PoliciesAnnotationPlus)
					return nil, warnings, true
				}
			}
		}
		filteredPolicyRefs = append(filteredPolicyRefs, ref)
	}

	return filteredPolicyRefs, warnings, false
}

// getIngressPolicyRefs returns the de-duplicated Policy references from Ingress policy annotations
// while preserving the annotation order: nginx.org/policies first, then nginx.com/policies.
func getIngressPolicyRefs(ingEx *IngressEx) []conf_v1.PolicyReference {
	if ingEx == nil || ingEx.Ingress == nil {
		return nil
	}

	var policyRefs []conf_v1.PolicyReference
	seenPolicyRefs := make(map[string]bool)
	for _, annotation := range []string{PoliciesAnnotation, PoliciesAnnotationPlus} {
		policyNames, exists := ingEx.Ingress.Annotations[annotation]
		if !exists {
			continue
		}

		for _, ref := range policies.GetPolicyRefsFromAnnotation(policyNames, ingEx.Ingress.Namespace) {
			resourceRef := ref.Name
			if ref.Namespace != "" {
				resourceRef = fmt.Sprintf("%s/%s", ref.Namespace, ref.Name)
			}
			namespace, resourceName := ParseResourceReference(resourceRef, ingEx.Ingress.Namespace)
			key := fmt.Sprintf("%s/%s", namespace, resourceName)
			if seenPolicyRefs[key] {
				continue
			}
			seenPolicyRefs[key] = true
			policyRefs = append(policyRefs, ref)
		}
	}

	return policyRefs
}

func resolveIngressAppProtectResources(ingEx *IngressEx, apResources *AppProtectResources, policyCfg policiesCfg) (*AppProtectResources, Warnings) {
	warnings := newWarnings()
	if ingEx == nil || ingEx.Ingress == nil || policyCfg.WAF == nil {
		return apResources, warnings
	}

	var presentAnnotations []string
	for _, annotation := range ingressWAFAnnotations {
		if _, exists := ingEx.Ingress.Annotations[annotation]; exists {
			presentAnnotations = append(presentAnnotations, annotation)
		}
	}

	if len(presentAnnotations) == 0 {
		return apResources, warnings
	}

	warnings.AddWarningf(
		ingEx.Ingress,
		"WAF cannot be configured through both Policy and App Protect annotations on the same Ingress; annotations %s will be ignored because the Policy configuration takes precedence",
		strings.Join(presentAnnotations, ", "),
	)

	return &AppProtectResources{}, warnings
}

//nolint:gocyclo
func generateNginxCfg(ncp NginxCfgParams) (version1.IngressNginxConfig, Warnings) {
	l := nl.LoggerFromContext(ncp.BaseCfgParams.Context)
	hasAppProtect := ncp.staticParams.MainAppProtectLoadModule
	hasAppProtectDos := ncp.staticParams.MainAppProtectDosLoadModule

	ingExWithAliases, aliasWarnings := withIngressNginxAliases(ncp.ingEx)
	ncp.ingEx = ingExWithAliases
	cfgParams := parseAnnotations(ncp.ingEx, ncp.BaseCfgParams, ncp.isPlus, hasAppProtect, hasAppProtectDos, ncp.staticParams.IsDirectiveAutoadjustEnabled)
	ingressNginxWarnings := parseIngressNginxAnnotations(ncp.ingEx, &cfgParams, ncp.staticParams.IsDirectiveAutoadjustEnabled, ncp.staticParams.EnableSnippets)
	if ncp.staticParams.UpstreamAPI && cfgParams.IngressNginxBalancing != nil {
		// the wxingress module balances by a rule: the upstreams keep their method, and a change reloads nothing
		cfgParams.LBMethod = cfgParams.IngressNginxBalancing.nginxLBMethod
	}
	cfgParams.UpstreamZoneSize = upstreamZoneSize(cfgParams.UpstreamZoneSize, ncp.staticParams.UpstreamAPI)
	cfgParams.SharedUpstreamZone = ncp.staticParams.UpstreamAPI && !ncp.isPlus
	cfgParams.IngressNginxMetrics = ncp.staticParams.IngressNginxMetrics

	wsServices := getWebsocketServices(ncp.ingEx)
	spServices := getSessionPersistenceServices(ncp.BaseCfgParams.Context, ncp.ingEx)
	rewrites := getRewrites(ncp.BaseCfgParams.Context, ncp.ingEx)
	rewriteTarget, rewriteTargetWarnings := getRewriteTarget(ncp.BaseCfgParams.Context, ncp.ingEx)
	upstreamVhost, upstreamVhostWarnings := getUpstreamVhost(ncp.ingEx)
	sslServices := getSSLServices(ncp.ingEx)
	grpcServices := getGrpcServices(ncp.ingEx)

	upstreams := make(map[string]version1.Upstream)
	healthChecks := make(map[string]version1.HealthCheck)
	var canaries map[string]nginx.Canary

	// HTTP2 is required for gRPC to function
	if len(grpcServices) > 0 && !cfgParams.HTTP2 {
		nl.Errorf(l, "Ingress %s/%s: annotation nginx.org/grpc-services requires HTTP2, ignoring", ncp.ingEx.Ingress.Namespace, ncp.ingEx.Ingress.Name)
		grpcServices = make(map[string]bool)
	}

	allWarnings := newWarnings()
	allWarnings.Add(rewriteTargetWarnings)
	allWarnings.Add(upstreamVhostWarnings)
	addIngressNginxWarnings(allWarnings, aliasWarnings)
	addIngressNginxWarnings(allWarnings, ingressNginxWarnings)

	if ncp.ingEx.Ingress.Spec.DefaultBackend != nil && ncp.ingEx.Ingress.Spec.DefaultBackend.Service != nil {
		name := getNameForUpstream(ncp.ingEx.Ingress, emptyHostName, ncp.ingEx.Ingress.Spec.DefaultBackend)
		upstream, upsWarning := createUpstream(ncp.ingEx, name, ncp.ingEx.Ingress.Spec.DefaultBackend, spServices[ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name], &cfgParams,
			ncp.isPlus, ncp.isResolverConfigured, ncp.staticParams.EnableLatencyMetrics)
		if upsWarning != "" {
			allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s", upsWarning)
		}
		upstreams[name] = upstream

		if cfgParams.HealthCheckEnabled {
			if hc, exists := ncp.ingEx.HealthChecks[ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name+GetBackendPortAsString(ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Port)]; exists {
				healthChecks[name] = createHealthCheck(hc, name, &cfgParams)
			}
		}
	}

	// the default-backend of ingress-nginx: the backend of custom-http-errors and of what nginx answers 502/503 itself
	inxErrorBackend := ""
	if backend := ncp.ingEx.IngressNginxDefaultBackend; backend != nil {
		inxErrorBackend = getNameForUpstream(ncp.ingEx.Ingress, "inx-default-backend", backend)
		upstream, upsWarning := createUpstream(ncp.ingEx, inxErrorBackend, backend, "", &cfgParams, ncp.isPlus, ncp.isResolverConfigured, ncp.staticParams.EnableLatencyMetrics)
		if upsWarning != "" {
			allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s", upsWarning)
		}
		upstreams[inxErrorBackend] = upstream
	} else if _, set := ncp.ingEx.Ingress.Annotations[IngressNginxDefaultBackendAnnotation]; set {
		allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s is ignored: the Service does not exist or has no ports", IngressNginxDefaultBackendAnnotation)
	}
	inxFallback := inxErrorBackend != ""
	if len(cfgParams.IngressNginxErrorCodes) > 0 && inxErrorBackend == "" {
		if ncp.defaultBackend != "" {
			// as ingress-nginx: the codes go to the controller's default backend
			inxErrorBackend = ncp.defaultBackend
		} else {
			allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s is ignored: it needs %s or the controller's -default-backend-service", IngressNginxCustomHTTPErrorsAnnotation, IngressNginxDefaultBackendAnnotation)
		}
	}

	// Check for deprecated SSL redirect annotation and add warning
	if _, exists := ncp.ingEx.Ingress.Annotations["ingress.kubernetes.io/ssl-redirect"]; exists {
		allWarnings.AddWarningf(ncp.ingEx.Ingress, "The annotation 'ingress.kubernetes.io/ssl-redirect' is deprecated and will be removed. Please use 'nginx.org/ssl-redirect' instead.")
	}

	for _, h := range cfgParams.ProxySetHeaders {
		if strings.EqualFold(h.Name, "Host") {
			allWarnings.AddWarningf(ncp.ingEx.Ingress, "Host in '%s' creates a duplicate 'proxy_set_header Host' directive; remove it and use '%s' to set the upstream Host.", ProxySetHeadersAnnotation, UpstreamVhostAnnotation)
			break
		}
	}

	var servers []version1.Server
	var limitReqZones []version1.LimitReqZone
	var maps []version2.Map
	var oidcProviders []version2.OIDCProvider
	var keyValZones []version2.KeyValZone

	// Run generate Policies
	policyRefs := getIngressPolicyRefs(ncp.ingEx)
	policyRefs, annotationPolicyWarnings, annotationPolicyError := filterIngressPolicyRefs(policyRefs, ncp.ingEx)
	allWarnings.Add(annotationPolicyWarnings)

	policyAppProtectResources := newAppProtectPolicyResources()
	for policyKey, policy := range ncp.ingEx.ApPolRefs {
		policyAppProtectResources.Policies[policyKey] = appProtectPolicyFileNameFromUnstruct(policy)
	}
	for logConfKey, logConf := range ncp.ingEx.LogConfRefs {
		policyAppProtectResources.LogConfs[logConfKey] = appProtectLogConfFileNameFromUnstruct(logConf)
	}

	bundlePath := ""
	if ncp.staticParams != nil {
		bundlePath = ncp.staticParams.AppProtectBundlePath
	}
	bundleValidator := newInternalBundleValidator(bundlePath)

	var policyCfg policiesCfg
	if annotationPolicyError {
		policyCfg.ErrorReturn = &version2.Return{Code: 500}
	} else if len(policyRefs) > 0 {
		var warnings Warnings
		pathContext := specContext
		ownerDetails := policyOwnerDetails{
			owner:           ncp.ingEx.Ingress,
			ownerName:       ncp.ingEx.Ingress.Name,
			ownerNamespace:  ncp.ingEx.Ingress.Namespace,
			parentName:      ncp.ingEx.Ingress.Name,
			parentNamespace: ncp.ingEx.Ingress.Namespace,
			parentType:      "ing",
		}
		if ncp.isMinion {
			ownerDetails.parentName = ncp.mergeableIngs.Master.Ingress.Name
			ownerDetails.parentNamespace = ncp.mergeableIngs.Master.Ingress.Namespace
			pathContext = minionContext
		}
		oidcNativeLocations := ncp.oidcNativeLocations
		if oidcNativeLocations == nil {
			oidcNativeLocations = make(map[string]oidcNativeLocationOwner)
		}
		policyCfg, warnings = generatePolicies(
			ncp.BaseCfgParams.Context,
			ownerDetails,
			policyRefs,
			ncp.ingEx.Policies,
			pathContext,
			"",
			policyOptions{
				tls:                 ncp.ingEx.Ingress.Spec.TLS != nil,
				zoneSync:            ncp.BaseCfgParams.ZoneSync.Enable,
				secretRefs:          ncp.ingEx.SecretRefs,
				apResources:         policyAppProtectResources,
				defaultCABundle:     ncp.staticParams.DefaultCABundle,
				replicas:            ncp.ingressControllerReplicas,
				oidcPolicyName:      "",
				plmEnabled:          ncp.staticParams.PLMEnabled,
				oidcNativeLocations: oidcNativeLocations,
			},
			bundleValidator,
		)
		allWarnings.Add(warnings)
	}

	apResources, appProtectWarnings := resolveIngressAppProtectResources(ncp.ingEx, ncp.apResources, policyCfg)
	allWarnings.Add(appProtectWarnings)

	if policyCfg.CORSMap != nil {
		// CORS origin validation map is rendered at http{} level and consumed by location headers.
		maps = append(maps, *policyCfg.CORSMap)
	}

	ingressNginxExtAuth := cfgParams.IngressNginxExternalAuth
	if ingressNginxExtAuth != nil && policyCfg.ExternalAuth != nil {
		allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s is ignored: the ExternalAuth policy of the Ingress takes precedence", IngressNginxAuthURLAnnotation)
		ingressNginxExtAuth = nil
	}
	if ingressNginxExtAuth != nil && !ncp.isResolverConfigured {
		// nginx resolves the host of auth-url per request, as ingress-nginx does
		// with the name servers of the pod, so an auth service that does not exist yet fails no reload
		if len(ncp.staticParams.SystemResolvers) == 0 {
			allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s needs a resolver: the pod has no name server and the ConfigMap no resolver-addresses", IngressNginxAuthURLAnnotation)
		} else {
			withResolver := *ingressNginxExtAuth
			withResolver.Resolver = strings.Join(ncp.staticParams.SystemResolvers, " ") + " valid=30s"
			ingressNginxExtAuth = &withResolver
		}
	}

	var mergedRateLimits []version1.IngressNginxRateLimit
	var mergedBalancers map[string]nginx.Balancer
	var mergedAuthCaches []version1.IngressNginxAuthCache
	var mergedAuthUpstreams []version1.IngressNginxAuthUpstream
	mergedStreamSnippets := [][]string{cfgParams.IngressNginxStreamSnippet}

	stickyUpstreams := newIngressNginxStickyUpstreams()
	if cfgParams.IngressNginxAffinity != nil && len(spServices) > 0 {
		allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s is ignored for the services of %s, which takes precedence", IngressNginxAffinityAnnotation, StickyCookieServicesAnnotation)
	}
	if b := cfgParams.IngressNginxBalancing; b != nil && b.algorithm == "chashsubset" && !ncp.staticParams.UpstreamAPI {
		allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s needs -dynamic-upstreams: without it each key is hashed to one endpoint, not to a subset of them", IngressNginxUpstreamHashBySubsetAnnotation)
	}
	if defaultBackend := ncp.ingEx.Ingress.Spec.DefaultBackend; defaultBackend != nil && defaultBackend.Service != nil {
		stickyUpstreams.balance(getNameForUpstream(ncp.ingEx.Ingress, emptyHostName, defaultBackend), cfgParams.IngressNginxBalancing, ncp.ingEx.Ingress.Namespace, defaultBackend)
	}

	// ingress-nginx's server-alias: a name once, on the first server of the Ingress, and none that is a host of the
	// Ingress (those another resource has, or an older Ingress as an alias, are out of the Ingress already)
	aliasesGiven := make(map[string]bool)
	for _, rule := range ncp.ingEx.Ingress.Spec.Rules {
		aliasesGiven[strings.ToLower(rule.Host)] = true
	}
	serverAliases := func() []string {
		var aliases []string
		for _, alias := range cfgParams.IngressNginxServerAliases {
			name := strings.ToLower(alias)
			if aliasesGiven[name] {
				continue
			}
			aliasesGiven[name] = true
			aliases = append(aliases, alias)
		}
		return aliases
	}

	for _, rule := range ncp.ingEx.Ingress.Spec.Rules {
		// skipping invalid hosts
		if !ncp.ingEx.ValidHosts[rule.Host] {
			continue
		}

		httpIngressRuleValue := rule.HTTP

		if httpIngressRuleValue == nil {
			// the code in this loop expects non-nil
			httpIngressRuleValue = &networking.HTTPIngressRuleValue{}
		}

		serverName := rule.Host

		isDefaultServer := rule.Host == emptyHostName
		statusZone := rule.Host
		if statusZone == emptyHostName {
			serverName = emptyHostToken
			statusZone = emptyHostToken
		}

		server := version1.Server{
			AddHeaderInherit:       cfgParams.AddHeaderInherit,
			Name:                   serverName,
			IsDefaultServer:        isDefaultServer,
			ServerTokens:           cfgParams.ServerTokens,
			HTTP2:                  cfgParams.HTTP2,
			RedirectToHTTPS:        cfgParams.RedirectToHTTPS,
			SSLRedirect:            cfgParams.SSLRedirect,
			ForceSSLRedirect:       cfgParams.IngressNginxForceSSLRedirect,
			IngressNginxRedirect:   cfgParams.IngressNginxDefaults,
			PreserveTrailingSlash:  cfgParams.IngressNginxPreserveTrailingSlash,
			ServerAliases:          serverAliases(),
			HTTPRedirectCode:       cfgParams.HTTPRedirectCode,
			SSLCiphers:             cfgParams.ServerSSLCiphers,
			SSLPreferServerCiphers: cfgParams.ServerSSLPreferServerCiphers,
			ProxyProtocol:          cfgParams.ProxyProtocol,
			HSTS:                   cfgParams.HSTS,
			HSTSMaxAge:             cfgParams.HSTSMaxAge,
			HSTSIncludeSubdomains:  cfgParams.HSTSIncludeSubdomains,
			HSTSPreload:            cfgParams.HSTSPreload,
			HSTSBehindProxy:        cfgParams.HSTSBehindProxy,
			StatusZone:             statusZone,
			RealIPHeader:           cfgParams.RealIPHeader,
			SetRealIPFrom:          cfgParams.SetRealIPFrom,
			RealIPRecursive:        cfgParams.RealIPRecursive,
			ProxyHideHeaders:       cfgParams.ProxyHideHeaders,
			ProxyPassHeaders:       cfgParams.ProxyPassHeaders,
			AddHeaders:             cfgParams.AddHeaders,
			ServerSnippets:         cfgParams.ServerSnippets,
			Ports:                  cfgParams.Ports,
			SSLPorts:               cfgParams.SSLPorts,
			TLSPassthrough:         ncp.staticParams.TLSPassthrough,
			AppProtectEnable:       cfgParams.AppProtectEnable,
			AppProtectLogEnable:    cfgParams.AppProtectLogEnable,
			DisableIPV6:            ncp.staticParams.DisableIPV6,
			ProxyRedirectFrom:      cfgParams.ProxyRedirectFrom,
			ProxyRedirectTo:        cfgParams.ProxyRedirectTo,
			AppRoot:                cfgParams.AppRoot,
			Allow:                  policyCfg.Allow,
			Deny:                   policyCfg.Deny,
			OIDCProviderName:       getOIDCProviderName(policyCfg),
			WAF:                    policyCfg.WAF,
			EgressMTLS:             policyCfg.EgressMTLS,
			PoliciesErrorReturn:    policyCfg.ErrorReturn,
		}

		if isDefaultServer {
			server.Ports = []int{ncp.staticParams.DefaultHTTPListenerPort}
			server.SSLPorts = []int{ncp.staticParams.DefaultHTTPSListenerPort}
			server.SSL = true
			server.SSLCertificate = DefaultServerSecretPath
			server.SSLCertificateKey = DefaultServerSecretPath
			server.SSLRejectHandshake = ncp.staticParams.SSLRejectHandshake
			server.AccessLogOff = cfgParams.DefaultServerAccessLogOff
			server.HealthStatus = ncp.staticParams.HealthStatus
			server.HealthStatusURI = ncp.staticParams.HealthStatusURI
			// as ingress-nginx: the catch-all of a host-less Ingress has the default certificate only,
			// which is no TLS of the Ingress - plain http is served, not sent to https
			if cfgParams.IngressNginxDefaults {
				server.SSLRedirect = false
			}
		}

		if ncp.isMinion {
			// Mergeable minions apply egress mTLS at location scope so minion policies can override the master.
			server.EgressMTLS = nil

			if policyCfg.OIDCProvider != nil {
				oidcProviders = append(oidcProviders, *policyCfg.OIDCProvider)
			}
		}

		if !isDefaultServer {
			warnings := addSSLConfig(&server, ncp.ingEx.Ingress, ncp.ingEx.Ingress.Namespace, rule.Host, ncp.ingEx.Ingress.Spec.TLS, ncp.ingEx.SecretRefs, ncp.isWildcardEnabled)
			allWarnings.Add(warnings)
			// as in ingress-nginx, the certificate of the host may come from any of its Ingresses
			for _, merged := range ncp.ingEx.Merged {
				if server.SSL {
					break
				}
				if merged.ValidHosts[rule.Host] && hasTLSForHost(merged.Ingress.Spec.TLS, rule.Host) {
					allWarnings.Add(addSSLConfig(&server, merged.Ingress, merged.Ingress.Namespace, rule.Host, merged.Ingress.Spec.TLS, merged.SecretRefs, ncp.isWildcardEnabled))
				}
			}
		}

		if !isDefaultServer && cfgParams.IngressNginxDefaults {
			applyIngressNginxDefaultTLS(&server)
		}

		if policyCfg.IngressMTLS != nil {
			if server.SSL {
				server.IngressMTLS = policyCfg.IngressMTLS
			} else {
				allWarnings.AddWarningf(
					ncp.ingEx.Ingress,
					"IngressMTLS policy is ignored for host %q because TLS is not enabled for that host",
					rule.Host,
				)
			}
		}

		if auth := cfgParams.IngressNginxAuthTLS; auth != nil && server.IngressMTLS == nil {
			if server.SSL {
				server.IngressMTLS = auth.MTLS
				server.IngressNginxMatchCN = auth.MatchCN
				server.IngressNginxClientCertErrorPage = auth.ErrorPage
				// https with the default certificate only (-ingress-nginx-defaults): plain
				// http is not sent there, and is not served without the check either
				server.IngressNginxClientCertHTTPSOnly = server.SSLCertificate == DefaultServerSecretPath && !server.SSLRedirect
			} else {
				// without TLS on the host there is no client certificate to check: not served without the check
				allWarnings.AddWarningf(ncp.ingEx.Ingress, "the locations of the Ingress answer %d: %s needs TLS for host %q", ingressNginxDeniedCode, IngressNginxAuthTLSSecretAnnotation, rule.Host)
				cfgParams.IngressNginxDenied = true
			}
		}

		if hasAppProtect {
			if apResources != nil {
				server.AppProtectPolicy = apResources.AppProtectPolicy
				server.AppProtectLogConfs = apResources.AppProtectLogconfs
			}
		}

		if hasAppProtectDos && ncp.dosResource != nil {
			server.AppProtectDosEnable = ncp.dosResource.AppProtectDosEnable
			server.AppProtectDosLogEnable = ncp.dosResource.AppProtectDosLogEnable
			server.AppProtectDosMonitorURI = ncp.dosResource.AppProtectDosMonitorURI
			server.AppProtectDosMonitorProtocol = ncp.dosResource.AppProtectDosMonitorProtocol
			server.AppProtectDosMonitorTimeout = ncp.dosResource.AppProtectDosMonitorTimeout
			server.AppProtectDosName = ncp.dosResource.AppProtectDosName
			server.AppProtectDosAllowListPath = ncp.dosResource.AppProtectDosAllowListPath
			server.AppProtectDosAccessLogDst = ncp.dosResource.AppProtectDosAccessLogDst
			server.AppProtectDosPolicyFile = ncp.dosResource.AppProtectDosPolicyFile
			server.AppProtectDosLogConfFile = ncp.dosResource.AppProtectDosLogConfFile
		}

		var locations []version1.Location

		if !ncp.isMinion {
			if cfgParams.JWTKey != "" {
				jwtAuth, redirectLoc, warnings := generateJWTConfig(ncp.ingEx.Ingress, ncp.ingEx.Ingress.Namespace, ncp.ingEx.SecretRefs, &cfgParams, getNameForRedirectLocation(ncp.ingEx.Ingress))
				server.JWTAuth = jwtAuth
				if redirectLoc != nil {
					server.JWTRedirectLocations = append(server.JWTRedirectLocations, *redirectLoc)
				}
				allWarnings.Add(warnings)
			}

			if cfgParams.BasicAuthSecret != "" {
				basicAuth, warnings := generateBasicAuthConfig(ncp.ingEx.Ingress, ncp.ingEx.Ingress.Namespace, ncp.ingEx.SecretRefs, &cfgParams)
				server.BasicAuth = basicAuth
				allWarnings.Add(warnings)
			}

			if policyCfg.ExternalAuth != nil {
				exAuth := policyCfg.ExternalAuth
				authUps, authLocs, portWarning := resolveExternalAuth(exAuth, ncp.ingEx.Ingress, ncp.ingEx.Endpoints, &cfgParams)
				if portWarning != "" {
					allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s", portWarning)
				}
				if authLocs != nil {
					upstreams[exAuth.URI.Upstream] = authUps
					server.ExternalAuth = exAuth
					locations = append(locations, authLocs...)
				}
			}

			if policyCfg.OIDCProvider != nil {
				oidcProviders = append(oidcProviders, *policyCfg.OIDCProvider)
			}

		}

		healthChecks := make(map[string]version1.HealthCheck)

		rootLocation := false

		grpcOnly := len(grpcServices) > 0
		hasGRPCLocations := false

		for i := range httpIngressRuleValue.Paths {
			path := httpIngressRuleValue.Paths[i]
			// skip invalid paths for minions
			if ncp.isMinion && !ncp.ingEx.ValidMinionPaths[path.Path] {
				continue
			}

			isGRPCService := grpcServices[path.Backend.Service.Name]
			// hasGRPCLocations tracks whether the server must emit the named
			// @grpcerror* locations, while grpcOnly stays true only if every rendered
			// content location in the server proxies gRPC.
			if isGRPCService {
				hasGRPCLocations = true
			} else {
				grpcOnly = false
			}

			upsName := getNameForUpstream(ncp.ingEx.Ingress, rule.Host, &path.Backend)

			if cfgParams.HealthCheckEnabled {
				if hc, exists := ncp.ingEx.HealthChecks[path.Backend.Service.Name+GetBackendPortAsString(path.Backend.Service.Port)]; exists {
					healthChecks[upsName] = createHealthCheck(hc, upsName, &cfgParams)
				}
			}

			if _, exists := upstreams[upsName]; !exists {
				upstream, upsWarning := createUpstream(ncp.ingEx, upsName, &path.Backend, spServices[path.Backend.Service.Name], &cfgParams, ncp.isPlus, ncp.isResolverConfigured, ncp.staticParams.EnableLatencyMetrics)
				if upsWarning != "" {
					allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s", upsWarning)
				}
				upstreams[upsName] = upstream
			}

			canary := ""
			if ncp.staticParams.UpstreamAPI {
				if canaryEx, canaryBackend := canaryForPath(ncp.ingEx, rule.Host, &path); canaryEx != nil {
					canaryUpsName, err := canaryUpstreamName(upsName, canaryEx, rule.Host, canaryBackend)
					switch {
					case err != nil:
						allWarnings.AddWarningf(ncp.ingEx.Ingress, "%v", err)
					case rewrites[path.Backend.Service.Name] != "" || cfgParams.ProxyRedirectFrom == "default":
						// with a variable in proxy_pass, nginx neither replaces the location's part of the URI nor takes proxy_redirect default
						allWarnings.AddWarningf(ncp.ingEx.Ingress, "the canary %s/%s is ignored for service %s: canaries do not work with nginx.org/rewrites or proxy_redirect default",
							canaryEx.Ingress.Namespace, canaryEx.Ingress.Name, path.Backend.Service.Name)
					default:
						canaryRule, err := ParseCanaryAnnotations(canaryEx.Ingress)
						if err != nil {
							allWarnings.AddWarningf(ncp.ingEx.Ingress, "the canary %s/%s is ignored: %v", canaryEx.Ingress.Namespace, canaryEx.Ingress.Name, err)
							break
						}
						if _, exists := upstreams[canaryUpsName]; !exists {
							upstream, upsWarning := createUpstream(canaryEx, canaryUpsName, canaryBackend, "", &cfgParams, ncp.isPlus, ncp.isResolverConfigured, ncp.staticParams.EnableLatencyMetrics)
							if upsWarning != "" {
								allWarnings.AddWarningf(ncp.ingEx.Ingress, "canary %s/%s: %s", canaryEx.Ingress.Namespace, canaryEx.Ingress.Name, upsWarning)
							}
							upstreams[canaryUpsName] = upstream
						}
						if canaries == nil {
							canaries = make(map[string]nginx.Canary)
						}
						canaryRule.Canary = canaryUpsName
						// a session pinned by the sticky cookie of the primary stays on the side it is pinned to
						if cookie := stickyCookieName(spServices[path.Backend.Service.Name]); cookie != "" {
							canaryRule.AffinityCookie = cookie
						} else if cfgParams.IngressNginxAffinity != nil {
							// the balancers of both sides pin it, and their cookie names the side
							stickyUpstreams.add(canaryUpsName, canaryAffinity(cfgParams.IngressNginxAffinity, canaryEx.Ingress), pathOrDefault(path.Path), server.SSL && cfgParams.SSLRedirect)
						}
						// the canary's own load-balance and upstream-hash-by, as ingress-nginx builds its backend from it
						stickyUpstreams.balance(canaryUpsName, canaryBalancing(canaryEx.Ingress), canaryEx.Ingress.Namespace, canaryBackend)
						canaries[upsName] = canaryRule
						canary = canaryVariable(upsName)
					}
				}
			}

			ssl := isSSLEnabled(sslServices[path.Backend.Service.Name])
			proxySSLName := generateProxySSLName(path.Backend.Service.Name, ncp.ingEx.Ingress.Namespace)
			proxyHTTPVersion := ncp.ingEx.proxyHTTPVersionForBackend(cfgParams.ProxyHTTPVersion, &path.Backend, isGRPCService)
			allWarnings.Add(warnProxyHTTPVersionConflicts(ncp.ingEx.Ingress, cfgParams.ProxyHTTPVersion, proxyHTTPVersion,
				path.Backend.Service.Name, isGRPCService, wsServices[path.Backend.Service.Name]))
			loc := createLocation(locationParams{
				path:             pathOrDefault(path.Path),
				pathType:         path.PathType,
				upstream:         upstreams[upsName],
				cfg:              &cfgParams,
				serviceName:      path.Backend.Service.Name,
				websocket:        wsServices[path.Backend.Service.Name],
				rewrite:          rewrites[path.Backend.Service.Name],
				rewriteTarget:    rewriteTarget,
				upstreamVhost:    upstreamVhost,
				ssl:              ssl,
				grpc:             isGRPCService,
				proxySSLName:     proxySSLName,
				proxyHTTPVersion: proxyHTTPVersion,
				canary:           canary,
				ingress:          ncp.ingEx.Ingress,
			})
			if ncp.isMinion && policyCfg.EgressMTLS != nil {
				// Minion egress mTLS is rendered per location to match VirtualServer route policy behavior.
				loc.EgressMTLS = policyCfg.EgressMTLS
			}

			// Custom-http-errors location-level rendering exists only for minions so
			// they can override the server-level directive inherited from the master.
			// Standard and master Ingresses render at server context (see below), which
			// avoids duplicating proxy_intercept_errors + error_page per location.
			if ncp.isMinion && len(cfgParams.CustomHTTPErrors) > 0 {
				if isGRPCService {
					allWarnings.AddWarningf(ncp.ingEx.Ingress,
						"%s is ignored for gRPC service %q; gRPC locations use their own error_page mapping",
						CustomHTTPErrorsAnnotation, path.Backend.Service.Name)
				} else {
					loc.CustomHTTPErrorCodes = cfgParams.CustomHTTPErrors
				}
			}

			if ncp.isMinion {

				loc.ProxyRedirectFrom = cfgParams.ProxyRedirectFrom
				loc.ProxyRedirectTo = cfgParams.ProxyRedirectTo

				loc.AddHeaderInherit = cfgParams.AddHeaderInherit
				server.AddHeaderInherit = "" // unset to avoid writing AddHeaderInherit to server block when the ingress is a minion, since it's only relevant for master ingresses

				if cfgParams.JWTKey != "" {
					jwtAuth, redirectLoc, warnings := generateJWTConfig(ncp.ingEx.Ingress, ncp.ingEx.Ingress.Namespace, ncp.ingEx.SecretRefs, &cfgParams, getNameForRedirectLocation(ncp.ingEx.Ingress))
					loc.JWTAuth = jwtAuth
					if redirectLoc != nil {
						server.JWTRedirectLocations = append(server.JWTRedirectLocations, *redirectLoc)
					}
					allWarnings.Add(warnings)
				}

				if cfgParams.BasicAuthSecret != "" {
					basicAuth, warnings := generateBasicAuthConfig(ncp.ingEx.Ingress, ncp.ingEx.Ingress.Namespace, ncp.ingEx.SecretRefs, &cfgParams)
					loc.BasicAuth = basicAuth
					allWarnings.Add(warnings)
				}

				if policyCfg.Allow != nil {
					loc.Allow = policyCfg.Allow
				}
				if policyCfg.Deny != nil {
					loc.Deny = policyCfg.Deny
				}

				if policyCfg.WAF != nil {
					loc.WAF = policyCfg.WAF
				}

				if policyCfg.OIDCProvider != nil {
					loc.OIDCProviderName = policyCfg.OIDCProvider.Name
				}

				if policyCfg.ErrorReturn != nil {
					loc.PoliciesErrorReturn = policyCfg.ErrorReturn
				}

				if policyCfg.ExternalAuth != nil {
					exAuth := policyCfg.ExternalAuth
					authUps, authLocs, portWarning := resolveExternalAuth(exAuth, ncp.ingEx.Ingress, ncp.ingEx.Endpoints, &cfgParams)
					if portWarning != "" {
						allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s", portWarning)
					}
					if authLocs != nil {
						upstreams[exAuth.URI.Upstream] = authUps
						loc.ExternalAuth = exAuth
						locations = append(locations, authLocs...)
					}
				}
			}

			if !loc.CORSEnabled && len(policyCfg.CORSHeaders) > 0 {
				// Apply Ingress-level CORS headers to every generated location unless already set.
				loc.AddHeaders = append(loc.AddHeaders, policyCfg.CORSHeaders...)
				loc.CORSEnabled = true
			}

			if cfgParams.LimitReqRate != "" {
				zoneName := ncp.ingEx.Ingress.Namespace + "/" + ncp.ingEx.Ingress.Name
				if ncp.ingEx.ZoneSync {
					zoneName = fmt.Sprintf("%v_sync", zoneName)
				}
				loc.LimitReq = &version1.LimitReq{
					Zone:       zoneName,
					Burst:      cfgParams.LimitReqBurst,
					Delay:      cfgParams.LimitReqDelay,
					NoDelay:    cfgParams.LimitReqNoDelay,
					DryRun:     cfgParams.LimitReqDryRun,
					LogLevel:   cfgParams.LimitReqLogLevel,
					RejectCode: cfgParams.LimitReqRejectCode,
				}
				if !limitReqZoneExists(limitReqZones, zoneName) {
					rate := cfgParams.LimitReqRate
					if cfgParams.LimitReqScale && ncp.ingressControllerReplicas > 0 {
						if ncp.ingEx.ZoneSync {
							warningText := fmt.Sprintf("Ingress %s/%s: both zone sync and rate limit scale are enabled, the rate limit scale value will not be used.", ncp.ingEx.Ingress.Namespace, ncp.ingEx.Ingress.Name)
							nl.Warn(l, warningText)
						} else {
							rate = scaleRatelimit(rate, ncp.ingressControllerReplicas)
						}
					}
					limitReqZones = append(limitReqZones, version1.LimitReqZone{
						Name: zoneName,
						Key:  cfgParams.LimitReqKey,
						Size: cfgParams.LimitReqZoneSize,
						Rate: rate,
						Sync: ncp.ingEx.ZoneSync,
					})
				}
			}

			// after the minion's own settings, which the annotations of ingress-nginx add to
			applyIngressNginxLocation(&loc, &cfgParams, ingressNginxExtAuth, rule.Host, pathOrDefault(path.Path), path.PathType)
			stickyUpstreams.add(upsName, cfgParams.IngressNginxAffinity, pathOrDefault(path.Path), server.SSL && cfgParams.SSLRedirect)
			stickyUpstreams.balance(upsName, cfgParams.IngressNginxBalancing, ncp.ingEx.Ingress.Namespace, &path.Backend)

			_, hasPathRegex := ncp.ingEx.Ingress.Annotations[PathRegexAnnotation]
			if exact, split := splitIngressNginxPrefixLocation(&loc, path.PathType, rewrites[path.Backend.Service.Name] != "" || rewriteTarget != "" || hasPathRegex, httpIngressRuleValue.Paths); split {
				locations = append(locations, loc, exact)
			} else {
				locations = append(locations, loc)
			}

			if loc.Path == "/" {
				rootLocation = true
			}
		}

		if !rootLocation && ncp.ingEx.Ingress.Spec.DefaultBackend != nil && ncp.ingEx.Ingress.Spec.DefaultBackend.Service != nil {
			upsName := getNameForUpstream(ncp.ingEx.Ingress, emptyHostName, ncp.ingEx.Ingress.Spec.DefaultBackend)
			ssl := isSSLEnabled(sslServices[ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name])
			proxySSLName := generateProxySSLName(ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name, ncp.ingEx.Ingress.Namespace)
			defaultBackendIsGRPC := grpcServices[ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name]
			proxyHTTPVersion := ncp.ingEx.proxyHTTPVersionForBackend(cfgParams.ProxyHTTPVersion,
				ncp.ingEx.Ingress.Spec.DefaultBackend, defaultBackendIsGRPC)
			allWarnings.Add(warnProxyHTTPVersionConflicts(ncp.ingEx.Ingress, cfgParams.ProxyHTTPVersion, proxyHTTPVersion,
				ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name, defaultBackendIsGRPC,
				wsServices[ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name]))
			loc := createLocation(locationParams{
				path:             pathOrDefault("/"),
				pathType:         new(networking.PathTypePrefix),
				upstream:         upstreams[upsName],
				cfg:              &cfgParams,
				serviceName:      ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name,
				websocket:        wsServices[ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name],
				rewrite:          rewrites[ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name],
				rewriteTarget:    rewriteTarget,
				upstreamVhost:    upstreamVhost,
				ssl:              ssl,
				grpc:             defaultBackendIsGRPC,
				proxySSLName:     proxySSLName,
				proxyHTTPVersion: proxyHTTPVersion,
				ingress:          ncp.ingEx.Ingress,
			})
			applyIngressNginxLocation(&loc, &cfgParams, ingressNginxExtAuth, rule.Host, "/", new(networking.PathTypePrefix))
			stickyUpstreams.add(upsName, cfgParams.IngressNginxAffinity, "/", server.SSL && cfgParams.SSLRedirect)
			if ncp.isMinion && policyCfg.EgressMTLS != nil {
				// Keep default-backend locations aligned with other minion locations for egress mTLS overrides.
				loc.EgressMTLS = policyCfg.EgressMTLS
			}
			if !loc.CORSEnabled && len(policyCfg.CORSHeaders) > 0 {
				// Keep default-backend location behavior consistent with path locations for CORS.
				loc.AddHeaders = append(loc.AddHeaders, policyCfg.CORSHeaders...)
				loc.CORSEnabled = true
			}
			locations = append(locations, loc)

			if cfgParams.HealthCheckEnabled {
				if hc, exists := ncp.ingEx.HealthChecks[ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name+GetBackendPortAsString(ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Port)]; exists {
					healthChecks[upsName] = createHealthCheck(hc, upsName, &cfgParams)
				}
			}

			isGRPCService := grpcServices[ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name]
			// Keep the same server-level gRPC bookkeeping for the synthesized
			// defaultBackend root location.
			if isGRPCService {
				hasGRPCLocations = true
			} else {
				grpcOnly = false
			}

			rootLocation = true
		}

		if len(locations) == 0 {
			grpcOnly = false
		}

		if isDefaultServer && !rootLocation {
			server.DefaultServerReturn = cfgParams.DefaultServerReturn
		}

		// Custom-http-errors on a standard or master Ingress renders at server
		// context so all locations inherit proxy_intercept_errors + error_page
		// via standard NGINX inheritance. Minions render per-location instead

		if !ncp.isMinion && len(cfgParams.CustomHTTPErrors) > 0 {
			server.CustomHTTPErrorCodes = cfgParams.CustomHTTPErrors
			if ncp.ingEx.Ingress.Spec.DefaultBackend != nil {
				server.CustomHTTPErrorBackend = getNameForUpstream(ncp.ingEx.Ingress, emptyHostName, ncp.ingEx.Ingress.Spec.DefaultBackend)
				synthesizedPath := pathOrDefault("/")
				for i := range locations {
					if locations[i].Path == synthesizedPath && locations[i].ServiceName == ncp.ingEx.Ingress.Spec.DefaultBackend.Service.Name {
						locations[i].SkipCustomHTTPErrors = true
						break
					}
				}
			} else {
				allWarnings.AddWarningf(ncp.ingEx.Ingress,
					"%s annotation is set but spec.defaultBackend is not; the annotation is a no-op without a default backend to route intercepted responses to.",
					CustomHTTPErrorsAnnotation)
			}
		}

		if ncp.isMinion && len(cfgParams.CustomHTTPErrors) > 0 &&
			ncp.mergeableIngs != nil && ncp.mergeableIngs.Master.Ingress.Spec.DefaultBackend == nil {
			allWarnings.AddWarningf(ncp.ingEx.Ingress,
				"%s annotation is set on a minion but the master Ingress has no spec.defaultBackend; the annotation is a no-op without a default backend on the master to route intercepted responses to.",
				CustomHTTPErrorsAnnotation)
		}

		ownLocations := len(locations)
		// the paths of the other Ingresses of the host, as ingress-nginx merges them
		for _, merged := range ncp.ingEx.Merged {
			if !merged.ValidHosts[rule.Host] {
				continue
			}
			mergedCfg, mergedWarnings := generateMergedIngressCfg(ncp, merged, rule.Host, server.HTTP2, locations)
			allWarnings.Add(mergedWarnings)
			if len(mergedCfg.Servers) == 0 {
				continue
			}
			for _, loc := range mergedCfg.Servers[0].Locations {
				if !loc.Internal {
					if loc.GRPC {
						hasGRPCLocations = true
					} else {
						grpcOnly = false
					}
				}
				locations = append(locations, loc)
			}
			for name, hc := range mergedCfg.Servers[0].HealthChecks {
				healthChecks[name] = hc
			}
			server.JWTRedirectLocations = append(server.JWTRedirectLocations, mergedCfg.Servers[0].JWTRedirectLocations...)
			// the internal locations the merged locations need: mirrors, error pages
			server.IngressNginxMirrors = append(server.IngressNginxMirrors, mergedCfg.Servers[0].IngressNginxMirrors...)
			server.IngressNginxErrorLocations = append(server.IngressNginxErrorLocations, mergedCfg.Servers[0].IngressNginxErrorLocations...)
			if server.MirrorResolver == "" {
				server.MirrorResolver = mergedCfg.Servers[0].MirrorResolver
			}
			for _, ups := range mergedCfg.Upstreams {
				upstreams[ups.Name] = ups
			}
			for primary, canary := range mergedCfg.Canaries {
				if canaries == nil {
					canaries = make(map[string]nginx.Canary)
				}
				canaries[primary] = canary
			}
			mergedBalancers = mergeBalancers(mergedBalancers, mergedCfg.Balancers)
			for _, zone := range mergedCfg.LimitReqZones {
				if !limitReqZoneExists(limitReqZones, zone.Name) {
					limitReqZones = append(limitReqZones, zone)
				}
			}
			maps = append(maps, mergedCfg.Maps...)
			oidcProviders = append(oidcProviders, mergedCfg.OIDCProviders...)
			mergedRateLimits = append(mergedRateLimits, mergedCfg.IngressNginxRateLimits...)
			mergedAuthCaches = append(mergedAuthCaches, mergedCfg.IngressNginxAuthCaches...)
			mergedAuthUpstreams = append(mergedAuthUpstreams, mergedCfg.IngressNginxAuthUpstreams...)
			for _, snippet := range mergedCfg.IngressNginxStreamSnippets {
				mergedStreamSnippets = append(mergedStreamSnippets, []string{snippet})
			}
		}

		if inxErrorBackend != "" {
			applyIngressNginxErrors(&server, locations[:ownLocations], inxErrorBackend, cfgParams.IngressNginxErrorCodes, inxFallback)
		}
		if m := cfgParams.IngressNginxMirror; m != nil {
			server.IngressNginxMirrors = append(server.IngressNginxMirrors, *m)
			if !ncp.isResolverConfigured {
				if len(ncp.staticParams.SystemResolvers) == 0 {
					allWarnings.AddWarningf(ncp.ingEx.Ingress, "%s needs a resolver: the pod has no name server and the ConfigMap no resolver-addresses", IngressNginxMirrorTargetAnnotation)
				} else {
					server.MirrorResolver = strings.Join(ncp.staticParams.SystemResolvers, " ") + " valid=30s"
				}
			}
		}
		server.Locations = locations
		server.IngressNginxHTTPSRedirect = server.IngressNginxRedirect && ((server.SSL && server.SSLRedirect) || server.ForceSSLRedirect)
		enforceIngressNginxRegex(&server, ncp.ingEx.Ingress.Annotations)
		server.ACMEChallengeActive = hasACMEChallengeLocation(locations)
		server.HealthChecks = healthChecks
		server.GRPCOnly = grpcOnly
		server.HasGRPCLocations = hasGRPCLocations
		if ncp.defaultBackend != "" && !ncp.isMinion && !slices.ContainsFunc(locations, func(l version1.Location) bool { return l.Path == "/" }) {
			// as ingress-nginx: a path no Ingress of the host has goes to the controller's default backend
			server.IngressNginxDefaultBackend = ncp.defaultBackend
		}

		servers = append(servers, server)

		// from-to-www-redirect: a server of the other name that sends everything here, as ingress-nginx's does
		if from, ok := ncp.ingEx.WWWRedirects[rule.Host]; ok && !ncp.isMinion {
			redirect := version1.Server{
				Name:             from,
				StatusZone:       from,
				Ports:            server.Ports,
				SSLPorts:         server.SSLPorts,
				ServerTokens:     server.ServerTokens,
				HTTP2:            server.HTTP2,
				ProxyProtocol:    server.ProxyProtocol,
				DisableIPV6:      server.DisableIPV6,
				HTTPRedirectCode: server.HTTPRedirectCode,
				RedirectAllTo:    "$scheme://" + rule.Host + ":$server_port",
			}
			if hasTLSForHost(ncp.ingEx.Ingress.Spec.TLS, from) {
				allWarnings.Add(addSSLConfig(&redirect, ncp.ingEx.Ingress, ncp.ingEx.Ingress.Namespace, from, ncp.ingEx.Ingress.Spec.TLS, ncp.ingEx.SecretRefs, ncp.isWildcardEnabled))
			}
			servers = append(servers, redirect)
		}
	}

	// the sticky cookie of affinity, with the paths of each upstream now known -
	// or with -dynamic-upstreams the balancer rule of each upstream, which the
	// wxingress module applies to every upstream that says meta_balancer
	var balancers map[string]nginx.Balancer
	if ncp.staticParams.UpstreamAPI {
		for name, upstream := range upstreams {
			upstream.MetaBalancer = true
			upstreams[name] = upstream
		}
		balancers = stickyUpstreams.balancers(upstreams)
	} else {
		stickyUpstreams.apply(upstreams)
	}

	// Generate keyval zones for OIDC Native session stores.
	dedupedOIDCProviders := removeDuplicateOIDCProviders(oidcProviders)
	for _, p := range dedupedOIDCProviders {
		if p.SessionStore != "" {
			timeout := p.SessionTimeout
			if p.Sync && timeout == "" {
				timeout = oidcNativeSessionSyncDefaultTimeout
			}
			keyValZones = append(keyValZones, version2.KeyValZone{
				Name:    p.SessionStore,
				Size:    oidcNativeSessionZoneSize,
				Sync:    p.Sync,
				Timeout: timeout,
			})
		}
	}

	return version1.IngressNginxConfig{
		Upstreams:     upstreamMapToSlice(upstreams),
		Servers:       servers,
		CORSHeaders:   policyCfg.CORSHeaders,
		OIDCProviders: dedupedOIDCProviders,
		KeyValZones:   keyValZones,
		Ingress: version1.Ingress{
			Name:        ncp.ingEx.Ingress.Name,
			Namespace:   ncp.ingEx.Ingress.Namespace,
			Annotations: ncp.ingEx.Ingress.Annotations,
		},
		DynamicSSLReloadEnabled: ncp.staticParams.DynamicSSLReload,
		StaticSSLPath:           ncp.staticParams.StaticSSLPath,
		DefaultCertificate:      ncp.staticParams.defaultCertificate(),
		LimitReqZones:           limitReqZones,
		IngressNginxRateLimits:  append(ingressNginxRateLimitsOf(&cfgParams), mergedRateLimits...),
		IngressNginxAuthCaches:  append(ingressNginxAuthCachesOf(ingressNginxExtAuth), mergedAuthCaches...),
		IngressNginxAuthUpstreams: uniqueAuthUpstreams(append(ingressNginxAuthUpstreamsOf(ingressNginxExtAuth, ingressNginxAuthResolver(ncp)),
			mergedAuthUpstreams...)),
		IngressNginxStreamSnippets: uniqueStreamSnippets(mergedStreamSnippets...),
		Maps:                       removeDuplicateMaps(maps),
		AppProtectLoadModule:       ncp.staticParams.MainAppProtectLoadModule,
		HTTP2:                      ncp.BaseCfgParams.HTTP2,
		Canaries:                   canaries,
		Balancers:                  mergeBalancers(balancers, mergedBalancers),
	}, allWarnings
}

// applyIngressNginxDefaultTLS serves the server over https as ingress-nginx
// serves every host: with the default certificate where the host has no TLS
// of its own - plain http stays plain, the host has no TLS to send it to -
// and where its TLS has no usable Secret, instead of refusing the handshake.
func applyIngressNginxDefaultTLS(server *version1.Server) {
	switch {
	case !server.SSL:
		server.SSL = true
		server.SSLCertificate = DefaultServerSecretPath
		server.SSLCertificateKey = DefaultServerSecretPath
		server.SSLRedirect = false
	case server.SSLRejectHandshake:
		server.SSLRejectHandshake = false
		server.SSLCertificate = DefaultServerSecretPath
		server.SSLCertificateKey = DefaultServerSecretPath
	}
}

// ingressNginxMetricsLabels is the value of $wxingress_metrics of a location
// (dataplane/src/metrics.c): "c" for a location with a canary, else "-",
// then its primary upstream, namespace, Ingress, Service and path, separated
// by tabs. One constant string, so that the module reads one variable and
// knows the location by where its value is. A tab in a label would end it;
// a "$" would make the string a variable's: both become a space.
func ingressNginxMetricsLabels(canary bool, upstream, namespace, ingress, service, path string) string {
	flag := "-"
	if canary {
		flag = "c"
	}
	clean := strings.NewReplacer("\t", " ", "\n", " ", "$", " ")
	return flag + strings.Join([]string{upstream, namespace, ingress, service, clean.Replace(path)}, "\t")
}

// ingressNginxAuthResolver is the resolver of the names of auth services
// with auth-keepalive, unless the http block has one.
func ingressNginxAuthResolver(ncp NginxCfgParams) string {
	if ncp.isResolverConfigured || len(ncp.staticParams.SystemResolvers) == 0 {
		return ""
	}
	return strings.Join(ncp.staticParams.SystemResolvers, " ") + " valid=30s"
}

// mergeBalancers returns the balancer rules of both, those of the first winning.
func mergeBalancers(own map[string]nginx.Balancer, merged map[string]nginx.Balancer) map[string]nginx.Balancer {
	if len(merged) == 0 {
		return own
	}
	all := make(map[string]nginx.Balancer, len(own)+len(merged))
	for name, rule := range merged {
		all[name] = rule
	}
	for name, rule := range own {
		all[name] = rule
	}
	return all
}

// splitIngressNginxPrefixLocation makes the location of a Prefix path /p match
// /p and /p/..., not /px, as the Kubernetes API and ingress-nginx have it: it
// becomes /p/, and the exact location = /p it returns goes beside it, unless
// the Ingress has an Exact path /p. As in ingress-nginx, paths that are
// regular expressions or are rewritten are left as they are.
func splitIngressNginxPrefixLocation(loc *version1.Location, pathType *networking.PathType, rewritten bool, paths []networking.HTTPIngressPath) (version1.Location, bool) {
	if pathType == nil || *pathType != networking.PathTypePrefix || loc.Path == "/" || strings.HasSuffix(loc.Path, "/") ||
		loc.UseRegex || loc.RewriteTarget != "" || rewritten || loc.ACMEChallenge || loc.Internal {
		return version1.Location{}, false
	}

	exactPath := loc.Path
	loc.Path += "/"
	for _, p := range paths {
		if p.PathType != nil && *p.PathType == networking.PathTypeExact && p.Path == exactPath {
			return version1.Location{}, false
		}
	}

	exact := *loc
	exact.Path = "= " + exactPath
	exact.IngressNginxExternalAuthShared = true
	return exact, true
}

// hasTLSForHost tells if one of the TLS entries is for the host.
func hasTLSForHost(tls []networking.IngressTLS, host string) bool {
	for _, t := range tls {
		if slices.Contains(t.Hosts, host) {
			return true
		}
	}
	return false
}

// generateMergedIngressCfg generates the locations of an Ingress merged into
// the server of the host of another, as a minion of that one, but with
// neither its annotations nor its default backend: in ingress-nginx the
// annotations of an Ingress apply to its own paths. A path the server has
// already is left to the Ingress that has it first, as in ingress-nginx.
func generateMergedIngressCfg(ncp NginxCfgParams, merged *IngressEx, host string, http2 bool, taken []version1.Location) (version1.IngressNginxConfig, Warnings) {
	warnings := newWarnings()
	minion := *merged
	minion.Ingress = merged.Ingress.DeepCopy()
	minion.Ingress.Spec.DefaultBackend = nil
	minion.Merged = nil
	// its locations take its nginx.org/path-regex and name it in the config, as a minion's do
	if minion.Ingress.Annotations == nil {
		minion.Ingress.Annotations = make(map[string]string)
	}
	minion.Ingress.Annotations["nginx.org/mergeable-ingress-type"] = "minion"

	takenPaths := make(map[string]bool)
	for _, loc := range taken {
		if !loc.Internal {
			takenPaths[loc.Path] = true
		}
	}
	minion.ValidMinionPaths = make(map[string]bool)
	for _, rule := range minion.Ingress.Spec.Rules {
		if rule.Host != host || rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if takenPaths[generateIngressPath(pathOrDefault(path.Path), path.PathType)] {
				warnings.AddWarningf(merged.Ingress, "path %s of host %s is ignored: Ingress %s/%s has it already", pathOrDefault(path.Path), host, ncp.ingEx.Ingress.Namespace, ncp.ingEx.Ingress.Name)
				continue
			}
			minion.ValidMinionPaths[path.Path] = true
		}
	}

	baseCfgParams := *ncp.BaseCfgParams
	baseCfgParams.HTTP2 = http2
	cfg, minionWarnings := generateNginxCfg(NginxCfgParams{
		mergeableIngs:             &MergeableIngresses{Master: ncp.ingEx},
		staticParams:              ncp.staticParams,
		ingEx:                     &minion,
		apResources:               &AppProtectResources{},
		dosResource:               &appProtectDosResource{},
		isMinion:                  true,
		isPlus:                    ncp.isPlus,
		BaseCfgParams:             &baseCfgParams,
		isResolverConfigured:      ncp.isResolverConfigured,
		isWildcardEnabled:         ncp.isWildcardEnabled,
		ingressControllerReplicas: ncp.ingressControllerReplicas,
		defaultBackend:            ncp.defaultBackend,
		oidcNativeLocations:       ncp.oidcNativeLocations,
	})
	// the warnings are about the Ingress, not its copy
	if messages, exists := minionWarnings[minion.Ingress]; exists {
		minionWarnings[merged.Ingress] = append(minionWarnings[merged.Ingress], messages...)
		delete(minionWarnings, minion.Ingress)
	}
	warnings.Add(minionWarnings)
	for i := range cfg.Servers {
		for j := range cfg.Servers[i].Locations {
			if cfg.Servers[i].Locations[j].MinionIngress == nil {
				cfg.Servers[i].Locations[j].MinionIngress = &cfg.Ingress
			}
		}
	}
	return cfg, warnings
}

func generateJWTConfig(
	owner runtime.Object,
	namespace string,
	secretRefs map[secrets.SecretRefKey]*secrets.SecretReference,
	cfgParams *ConfigParams,
	redirectLocationName string,
) (*version1.JWTAuth, *version1.JWTRedirectLocation, Warnings) {
	warnings := newWarnings()

	var keyPath string
	secretRef := secretRefs[secrets.RefKey(namespace+"/"+cfgParams.JWTKey, secrets.RoleJWK)]
	if secretRef != nil {
		if secretRef.Error != nil {
			warnings.AddWarningf(owner, "JWK secret %s is invalid: %v", cfgParams.JWTKey, secretRef.Error)
		}
		keyPath = secretRef.Path
	} else {
		warnings.AddWarningf(owner, "JWK secret %s is missing from secret references", cfgParams.JWTKey)
	}

	// Key is configured even when the secret is missing or invalid. auth_jwt_key_file is
	// read by NGINX Plus at request time, so an absent file yields a 500 for the affected
	// URLs rather than a configuration load failure. Rendering an empty path here would
	// produce "auth_jwt_key_file ;" and NGINX would reject the entire configuration.
	jwtAuth := &version1.JWTAuth{
		Key:   keyPath,
		Realm: cfgParams.JWTRealm,
		Token: cfgParams.JWTToken,
	}

	var redirectLocation *version1.JWTRedirectLocation

	if cfgParams.JWTLoginURL != "" {
		jwtAuth.RedirectLocationName = redirectLocationName
		redirectLocation = &version1.JWTRedirectLocation{
			Name:     redirectLocationName,
			LoginURL: cfgParams.JWTLoginURL,
		}
	}

	return jwtAuth, redirectLocation, warnings
}

func generateBasicAuthConfig(owner runtime.Object, namespace string, secretRefs map[secrets.SecretRefKey]*secrets.SecretReference, cfgParams *ConfigParams) (*version1.BasicAuth, Warnings) {
	warnings := newWarnings()

	var secretPath string
	secretRef := secretRefs[secrets.RefKey(namespace+"/"+cfgParams.BasicAuthSecret, secrets.RoleHtpasswd)]
	if secretRef != nil {
		if secretRef.Error != nil {
			warnings.AddWarningf(owner, "Basic auth secret %s is invalid: %v", cfgParams.BasicAuthSecret, secretRef.Error)
		}
		secretPath = secretRef.Path
	} else {
		warnings.AddWarningf(owner, "Basic auth secret %s is missing from secret references", cfgParams.BasicAuthSecret)
	}

	basicAuth := &version1.BasicAuth{
		Secret: secretPath,
		Realm:  cfgParams.BasicAuthRealm,
	}

	return basicAuth, warnings
}

// createExternalAuthUpstream creates a version1.Upstream for the external auth service
// from the resolved endpoints.
func createExternalAuthUpstream(name string, endpoints []string, cfgParams *ConfigParams) (version1.Upstream, string) {
	if len(endpoints) == 0 {
		return version1.NewUpstreamWithDefaultServer(name), fmt.Sprintf("No endpoints found for external auth upstream %v", name)
	}
	upsServers := make([]version1.UpstreamServer, 0, len(endpoints))
	for _, ep := range endpoints {
		upsServers = append(upsServers, version1.UpstreamServer{
			Address:     ep,
			MaxFails:    1,
			MaxConns:    0,
			FailTimeout: "10s",
		})
	}
	sort.Slice(upsServers, func(i, j int) bool {
		return upsServers[i].Address < upsServers[j].Address
	})
	ups := version1.Upstream{
		Name:             name,
		UpstreamServers:  upsServers,
		UpstreamZoneSize: "256k",
	}
	if cfgParams.Keepalive > 0 {
		ups.Keepalive = fmt.Sprint(cfgParams.Keepalive)
	}
	return ups, ""
}

// resolveExternalAuth resolves the external auth upstream and generates the
// associated internal NGINX locations. It returns the upstream, locations to
// append, and an optional warning string.
func resolveExternalAuth(
	exAuth *version2.ExternalAuth,
	ingress *networking.Ingress,
	endpoints map[string][]string,
	cfgParams *ConfigParams,
) (version1.Upstream, []version1.Location, string) {
	port, warning := getExternalAuthServicePort(exAuth)
	upsName := exAuth.URI.Upstream

	if port == 0 {
		return version1.NewUpstreamWithDefaultServer(upsName), nil, warning
	}

	ns, svcName := ParseServiceReference(exAuth.URI.Service, ingress.Namespace)
	endpointKey := fmt.Sprintf("%s/%s:%d", ns, svcName, port)
	authUps, upsWarning := createExternalAuthUpstream(upsName, endpoints[endpointKey], cfgParams)
	if upsWarning != "" {
		if warning != "" {
			warning = fmt.Sprintf("%s. %s", warning, upsWarning)
		} else {
			warning = upsWarning
		}
	}
	var locs []version1.Location
	locs = append(locs, generateIngressExternalAuthLocation(exAuth, authUps, cfgParams))
	if exAuth.SigninURL != "" {
		locs = append(locs, generateIngressExternalAuthOAuth2Location(exAuth, authUps, cfgParams))
	}

	return authUps, locs, warning
}

// generateIngressExternalAuthLocation builds a version1.Location for the
// internal NGINX location that proxies auth subrequests to the external auth service.
func generateIngressExternalAuthLocation(externalAuth *version2.ExternalAuth, upstream version1.Upstream, cfg *ConfigParams) version1.Location {
	var svcName string
	_, svcName = ParseServiceReference(externalAuth.URI.Service, "")
	loc := version1.Location{
		Path:           externalAuth.URI.InternalPath,
		Internal:       true,
		Upstream:       upstream,
		DisableWAF:     true,
		ProxyPass:      fmt.Sprintf("%s://%s%s", generateProxyPassProtocol(externalAuth.SSLEnabled), upstream.Name, externalAuth.URI.Path),
		AuthRequestOff: true,
		ProxySetHeaders: []version2.Header{
			{Name: "Content-Length", Value: "0"},
			{Name: "X-Scheme", Value: "$scheme"},
			{Name: "X-Forwarded-Prefix", Value: ""},
			{Name: "X-Forwarded-URI", Value: "$request_uri"},
			{Name: "X-Forwarded-Method", Value: "$request_method"},
			{Name: "X-Original-URI", Value: "$request_uri"},
		},
		ProxyConnectTimeout:      generateTimeWithDefault(cfg.ProxyConnectTimeout, cfg.ProxyConnectTimeout),
		ProxyReadTimeout:         generateTimeWithDefault(cfg.ProxyReadTimeout, cfg.ProxyReadTimeout),
		ProxySendTimeout:         generateTimeWithDefault(cfg.ProxySendTimeout, cfg.ProxySendTimeout),
		ProxyPassRequestBody:     "off",
		ClientMaxBodySize:        "0",
		ProxyNextUpstream:        "error timeout",
		ProxyNextUpstreamTimeout: generateTimeWithDefault(cfg.ProxyNextUpstreamTimeout, "0s"),
		SkipCustomHTTPErrors:     true,
		LocationSnippets:         splitSnippets(externalAuth.Snippets),
		ServiceName:              svcName,
	}
	if externalAuth.SSLVerify {
		loc.ProxySSLVerify = true
		loc.ProxySSLVerifyDepth = externalAuth.SSLVerifyDepth
		loc.ProxySSLTrustedCertificate = externalAuth.SSLTrustedCert
		loc.ProxySSLName = externalAuth.SNIName
	}
	return loc
}

// generateIngressExternalAuthOAuth2Location builds a version1.Location
// for the NGINX location that handles OAuth2 signin redirects.
func generateIngressExternalAuthOAuth2Location(externalAuth *version2.ExternalAuth, upstream version1.Upstream, cfg *ConfigParams) version1.Location {
	var svcName string
	_, svcName = ParseServiceReference(externalAuth.URI.Service, "")
	loc := version1.Location{
		Path:                     externalAuth.SigninRedirectBasePath,
		AuthRequestOff:           true,
		Upstream:                 upstream,
		ProxyPass:                fmt.Sprintf("%s://%s", generateProxyPassProtocol(externalAuth.SSLEnabled), upstream.Name),
		ProxySetHeaders:          []version2.Header{{Name: "X-Auth-Request-Redirect", Value: "$request_uri"}, {Name: "X-Scheme", Value: "$scheme"}},
		ProxyConnectTimeout:      generateTimeWithDefault(cfg.ProxyConnectTimeout, cfg.ProxyConnectTimeout),
		ProxyReadTimeout:         generateTimeWithDefault(cfg.ProxyReadTimeout, cfg.ProxyReadTimeout),
		ProxySendTimeout:         generateTimeWithDefault(cfg.ProxySendTimeout, cfg.ProxySendTimeout),
		ClientMaxBodySize:        "0",
		ProxyNextUpstream:        "error timeout",
		ProxyNextUpstreamTimeout: generateTimeWithDefault(cfg.ProxyNextUpstreamTimeout, "0s"),
		LocationSnippets:         splitSnippets(externalAuth.Snippets),
		ServiceName:              svcName,
		ProxyPassRequestHeaders:  "on",
	}
	if externalAuth.SSLVerify {
		loc.ProxySSLVerify = true
		loc.ProxySSLVerifyDepth = externalAuth.SSLVerifyDepth
		loc.ProxySSLTrustedCertificate = externalAuth.SSLTrustedCert
		loc.ProxySSLName = externalAuth.SNIName
	}
	return loc
}

// splitSnippets splits a snippets string by newline, returning nil for empty input.
func splitSnippets(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// getExternalAuthServicePort resolves the service port for an ExternalAuth policy.
// Returns the port and an optional warning message.
func getExternalAuthServicePort(externalAuth *version2.ExternalAuth) (uint16, string) {
	if len(externalAuth.ServicePorts) > 0 {
		port := externalAuth.ServicePorts[0]
		if port > 0 && port <= math.MaxUint16 {
			return uint16(port), ""
		}
	}
	if externalAuth.URI.Port != "" {
		value, err := strconv.ParseUint(externalAuth.URI.Port, 10, 16)
		if err != nil {
			return 0, fmt.Sprintf("Invalid port in ExternalAuth URI: %v. ExternalAuth location will be generated without a port. Error: %v", externalAuth.URI.Port, err)
		}
		return uint16(value), ""
	}
	if externalAuth.SSLEnabled {
		return 443, ""
	}
	return 80, ""
}

func addSSLConfig(server *version1.Server, owner runtime.Object, namespace string, host string, ingressTLS []networking.IngressTLS,
	secretRefs map[secrets.SecretRefKey]*secrets.SecretReference, isWildcardEnabled bool,
) Warnings {
	warnings := newWarnings()

	var tlsEnabled bool
	var tlsSecret string

	for _, tls := range ingressTLS {
		for _, h := range tls.Hosts {
			if h == host {
				tlsEnabled = true
				tlsSecret = tls.SecretName
				break
			}
		}
	}

	if !tlsEnabled {
		return warnings
	}

	var pemFile string
	var rejectHandshake bool

	if tlsSecret != "" {
		secretRef := secretRefs[secrets.RefKey(namespace+"/"+tlsSecret, secrets.RoleTLS)]
		if secretRef != nil && secretRef.Error != nil {
			rejectHandshake = true
			warnings.AddWarningf(owner, "TLS secret %s is invalid: %v", tlsSecret, secretRef.Error)
		} else if secretRef != nil {
			pemFile = secretRef.Path
		} else {
			rejectHandshake = true
			warnings.AddWarningf(owner, "TLS secret %s is missing from secret references", tlsSecret)
		}
	} else if isWildcardEnabled {
		pemFile = pemFileNameForWildcardTLSSecret
	} else {
		rejectHandshake = true
		warnings.AddWarningf(owner, "TLS termination for host '%s' requires specifying a TLS secret or configuring a global wildcard TLS secret", host)
	}

	server.SSL = true
	server.SSLCertificate = pemFile
	server.SSLCertificateKey = pemFile
	server.SSLRejectHandshake = rejectHandshake

	return warnings
}

func generateIngressPath(path string, pathType *networking.PathType) string {
	if pathType == nil {
		return path
	}
	if *pathType == networking.PathTypeExact {
		path = "= " + path
	}

	return path
}

type locationParams struct {
	path          string
	pathType      *networking.PathType
	upstream      version1.Upstream
	cfg           *ConfigParams
	serviceName   string
	websocket     bool
	rewrite       string
	rewriteTarget string
	upstreamVhost string
	ssl           bool
	grpc          bool
	proxySSLName  string
	// proxyHTTPVersion is the already-resolved value for proxy_http_version. An empty value
	// means the directive is not rendered, either because nothing configured it or because
	// this is a gRPC location, which is proxied with grpc_pass.
	proxyHTTPVersion string
	// canary is the variable that names the upstream or its canary, if the location has one.
	canary string
	// ingress is the Ingress of the location, for the labels of its metrics
	ingress *networking.Ingress
}

// warnProxyHTTPVersionConflicts reports configurations that are accepted by validation but
// cannot work: proxy_http_version has no meaning for a gRPC backend, and WebSocket relies on
// the HTTP/1.1 Upgrade mechanism, which exists in neither HTTP/1.0 nor HTTP/2.
func warnProxyHTTPVersionConflicts(ing *networking.Ingress, configured string, resolved string, serviceName string, isGRPC bool, isWebsocket bool) Warnings {
	warnings := newWarnings()

	if isGRPC && configured != "" {
		warnings.AddWarningf(ing,
			"%s is ignored for gRPC service %q; gRPC locations always use HTTP/2",
			ProxyHTTPVersionAnnotation, serviceName)
		return warnings
	}

	if isWebsocket && (resolved == proxyHTTPVersion10 || resolved == proxyHTTPVersion2) {
		warnings.AddWarningf(ing,
			"service %q is configured for WebSocket but resolves to an HTTP/%s upstream connection; WebSocket requires HTTP/1.1",
			serviceName, resolved)
	}

	return warnings
}

// proxyHTTPVersionForBackend determines the HTTP version used for upstream connections of a
// single Ingress location, applying the annotation > Service appProtocol > unset precedence.
// gRPC backends are left unset: grpc_pass always uses HTTP/2 and proxy_http_version is never
// rendered for them.
func (ingEx *IngressEx) proxyHTTPVersionForBackend(configured string, backend *networking.IngressBackend, isGRPC bool) string {
	if isGRPC {
		return ""
	}
	key := backend.Service.Name + GetBackendPortAsString(backend.Service.Port)
	return resolveProxyHTTPVersion(configured, ingEx.ServiceAppProtocols[key])
}

// hasACMEChallengeLocation reports whether any of the locations serves an ACME HTTP-01 challenge.
func hasACMEChallengeLocation(locs []version1.Location) bool {
	for _, loc := range locs {
		if loc.ACMEChallenge {
			return true
		}
	}
	return false
}

func createLocation(p locationParams) version1.Location {
	cfg := p.cfg
	loc := version1.Location{
		Path:                     generateIngressPath(p.path, p.pathType),
		Upstream:                 p.upstream,
		ProxyPass:                fmt.Sprintf("%s://%s", generateProxyPassProtocol(p.ssl), p.upstream.Name),
		Canary:                   p.canary,
		ProxyConnectTimeout:      cfg.ProxyConnectTimeout,
		ProxyReadTimeout:         cfg.ProxyReadTimeout,
		ProxySendTimeout:         cfg.ProxySendTimeout,
		ProxySetHeaders:          cfg.ProxySetHeaders,
		ProxyHTTPVersion:         p.proxyHTTPVersion,
		ClientMaxBodySize:        cfg.ClientMaxBodySize,
		ClientBodyBufferSize:     cfg.ClientBodyBufferSize,
		Websocket:                p.websocket,
		IngressNginxProxyHeaders: cfg.IngressNginxDefaults,
		Rewrite:                  p.rewrite,
		RewriteTarget:            p.rewriteTarget,
		UpstreamVhost:            p.upstreamVhost,
		SSL:                      p.ssl,
		GRPC:                     p.grpc,
		ProxyBuffering:           cfg.ProxyBuffering,
		ProxyBuffers:             cfg.ProxyBuffers,
		ProxyBufferSize:          cfg.ProxyBufferSize,
		ProxyBusyBuffersSize:     cfg.ProxyBusyBuffersSize,
		ProxyMaxTempFileSize:     cfg.ProxyMaxTempFileSize,
		ProxyRequestBuffering:    cfg.ProxyRequestBuffering,
		DisableForwardedHeaders:  cfg.DisableForwardedHeaders,
		UseForwardedHeaders:      cfg.UseForwardedHeaders,
		ProxySSLName:             p.proxySSLName,
		ProxyNextUpstream:        cfg.ProxyNextUpstream,
		ProxyNextUpstreamTimeout: cfg.ProxyNextUpstreamTimeout,
		ProxyNextUpstreamTries:   cfg.ProxyNextUpstreamTries,
		LocationSnippets:         cfg.LocationSnippets,
		ServiceName:              p.serviceName,
	}
	loc.ACMEChallenge = IsACMEChallengeLocation(loc.Path, p.serviceName)
	if p.canary != "" {
		loc.ProxyPass = fmt.Sprintf("%s://%s", generateProxyPassProtocol(p.ssl), p.canary)
	}

	if cfg.IngressNginxMetrics && p.ingress != nil {
		loc.MetricsLabels = ingressNginxMetricsLabels(p.canary != "", p.upstream.Name, p.ingress.Namespace, p.ingress.Name, p.serviceName, p.path)
	}
	return loc
}

// upstreamRequiresQueue checks if the upstream requires a queue.
// Mandatory Health Checks can cause nginx to return errors on reload, since all Upstreams start
// Unhealthy. By adding a queue to the Upstream we can avoid returning errors, at the cost of a short delay.
func upstreamRequiresQueue(name string, ingEx *IngressEx, cfg *ConfigParams) (n int64, timeout int64) {
	if cfg.HealthCheckEnabled && cfg.HealthCheckMandatory && cfg.HealthCheckMandatoryQueue > 0 {
		if hc, exists := ingEx.HealthChecks[name]; exists {
			return cfg.HealthCheckMandatoryQueue, int64(hc.TimeoutSeconds)
		}
	}
	return 0, 0
}

func createUpstream(ingEx *IngressEx, name string, backend *networking.IngressBackend, stickyCookie string, cfg *ConfigParams,
	isPlus bool, isResolverConfigured bool, isLatencyMetricsEnabled bool,
) (version1.Upstream, string) {
	var ups version1.Upstream
	labels := version1.UpstreamLabels{
		Service:           backend.Service.Name,
		ResourceType:      "ingress",
		ResourceName:      ingEx.Ingress.Name,
		ResourceNamespace: ingEx.Ingress.Namespace,
	}
	l := nl.LoggerFromContext(cfg.Context)
	if isPlus {
		queue, timeout := upstreamRequiresQueue(backend.Service.Name+GetBackendPortAsString(backend.Service.Port), ingEx, cfg)
		ups = version1.Upstream{Name: name, Queue: queue, QueueTimeout: timeout, UpstreamLabels: labels}
	} else {
		ups = version1.NewUpstreamWithDefaultServer(name)
		if isLatencyMetricsEnabled {
			ups.UpstreamLabels = labels
		}
	}

	var warning string
	endps, exists := ingEx.Endpoints[backend.Service.Name+GetBackendPortAsString(backend.Service.Port)]
	if exists {
		if endps != nil && len(endps) == 0 {
			warning = fmt.Sprintf("No endpoints found for service %v", backend.Service.Name)
		}

		var upsServers []version1.UpstreamServer
		// Always false for NGINX OSS
		_, isExternalNameSvc := ingEx.ExternalNameSvcs[backend.Service.Name]
		if isExternalNameSvc && !isResolverConfigured {
			nl.Warnf(l, "A resolver must be configured for Type ExternalName service %s, no upstream servers will be created", backend.Service.Name)
			endps = []string{}
		}

		for _, endp := range endps {
			upsServers = append(upsServers, version1.UpstreamServer{
				Address:     endp,
				MaxFails:    cfg.MaxFails,
				MaxConns:    cfg.MaxConns,
				FailTimeout: cfg.FailTimeout,
				SlowStart:   cfg.SlowStart,
				Resolve:     isExternalNameSvc,
			})
		}
		if len(upsServers) > 0 {
			sort.Slice(upsServers, func(i, j int) bool {
				return upsServers[i].Address < upsServers[j].Address
			})
			ups.UpstreamServers = upsServers
		}
	}

	ups.LBMethod = cfg.LBMethod
	ups.UpstreamZoneSize = cfg.UpstreamZoneSize
	if cfg.SharedUpstreamZone {
		ups.Zone, ups.UpstreamZoneSize = SharedUpstreamZone, SharedUpstreamZoneSize
	}
	ups.StickyCookie = stickyCookie
	if cfg.Keepalive > 0 {
		ups.Keepalive = fmt.Sprint(cfg.Keepalive)
	}
	return ups, warning
}

func createHealthCheck(hc *api_v1.Probe, upstreamName string, cfg *ConfigParams) version1.HealthCheck {
	return version1.HealthCheck{
		UpstreamName:   upstreamName,
		Fails:          hc.FailureThreshold,
		Interval:       hc.PeriodSeconds,
		Passes:         hc.SuccessThreshold,
		URI:            hc.HTTPGet.Path,
		Scheme:         strings.ToLower(string(hc.HTTPGet.Scheme)),
		Mandatory:      cfg.HealthCheckMandatory,
		Headers:        headersToString(hc.HTTPGet.HTTPHeaders),
		TimeoutSeconds: int64(hc.TimeoutSeconds),
	}
}

func headersToString(headers []api_v1.HTTPHeader) map[string]string {
	m := make(map[string]string)
	for _, header := range headers {
		m[header.Name] = header.Value
	}
	return m
}

func pathOrDefault(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// getNameForUpstream returns the name of the upstream of the backend of the Ingress for the host:
// <namespace>_<name>_<host>_<service>_<port>. None of the parts has an underscore - but the host _ of an empty one,
// which is a part of its own - so no two Ingresses, hosts or backends share a name, as they did when the parts were
// joined with hyphens (namespace a-b and name c, namespace a and name b-c), when the upstream of one Ingress took
// the servers of another's.
func getNameForUpstream(ing *networking.Ingress, host string, backend *networking.IngressBackend) string {
	if host == emptyHostName {
		host = emptyHostToken
	}
	return fmt.Sprintf("%v_%v_%v_%v_%v", ing.Namespace, ing.Name, host, backend.Service.Name, GetBackendPortAsString(backend.Service.Port))
}

func getNameForRedirectLocation(ing *networking.Ingress) string {
	return fmt.Sprintf("@login_url_%v_%v", ing.Namespace, ing.Name)
}

func upstreamMapToSlice(upstreams map[string]version1.Upstream) []version1.Upstream {
	keys := make([]string, 0, len(upstreams))
	for k := range upstreams {
		keys = append(keys, k)
	}

	// this ensures that the slice 'result' is sorted, which preserves the order of upstream servers
	// in the generated configuration file from one version to another and is also required for repeatable
	// Unit test results
	sort.Strings(keys)

	result := make([]version1.Upstream, 0, len(upstreams))

	for _, k := range keys {
		result = append(result, upstreams[k])
	}

	return result
}

func generateNginxCfgForMergeableIngresses(ncp NginxCfgParams) (version1.IngressNginxConfig, Warnings) {
	l := nl.LoggerFromContext(ncp.BaseCfgParams.Context)
	var masterServer version1.Server
	var locations []version1.Location
	var upstreams []version1.Upstream
	healthChecks := make(map[string]version1.HealthCheck)
	var limitReqZones []version1.LimitReqZone
	var ingressNginxRateLimits []version1.IngressNginxRateLimit
	var ingressNginxAuthCaches []version1.IngressNginxAuthCache
	var ingressNginxAuthUpstreams []version1.IngressNginxAuthUpstream
	var ingressNginxStreamSnippets [][]string
	var maps []version2.Map
	var oidcProviders []version2.OIDCProvider
	var canaries map[string]nginx.Canary

	// replace master with a deepcopy because we will modify it
	originalMaster := ncp.mergeableIngs.Master.Ingress
	ncp.mergeableIngs.Master.Ingress = ncp.mergeableIngs.Master.Ingress.DeepCopy()

	removedAnnotations := filterMasterAnnotations(ncp.mergeableIngs.Master.Ingress.Annotations)
	if len(removedAnnotations) != 0 {
		nl.Errorf(l, "Ingress Resource %v/%v with the annotation 'nginx.org/mergeable-ingress-type' set to 'master' cannot contain the '%v' annotation(s). They will be ignored",
			ncp.mergeableIngs.Master.Ingress.Namespace, ncp.mergeableIngs.Master.Ingress.Name, strings.Join(removedAnnotations, ","))
	}
	isMinion := false

	sharedOIDCLocations := make(map[string]oidcNativeLocationOwner)

	masterNginxCfg, warnings := generateNginxCfg(NginxCfgParams{
		staticParams:              ncp.staticParams,
		ingEx:                     ncp.mergeableIngs.Master,
		apResources:               ncp.apResources,
		dosResource:               ncp.dosResource,
		isMinion:                  isMinion,
		isPlus:                    ncp.isPlus,
		BaseCfgParams:             ncp.BaseCfgParams,
		isResolverConfigured:      ncp.isResolverConfigured,
		isWildcardEnabled:         ncp.isWildcardEnabled,
		ingressControllerReplicas: ncp.ingressControllerReplicas,
		defaultBackend:            ncp.defaultBackend,
		oidcNativeLocations:       sharedOIDCLocations,
	})

	// because ncp.mergeableIngs.Master.Ingress is a deepcopy of the original master
	// we need to change the key in the warnings to the original master
	if _, exists := warnings[ncp.mergeableIngs.Master.Ingress]; exists {
		warnings[originalMaster] = warnings[ncp.mergeableIngs.Master.Ingress]
		delete(warnings, ncp.mergeableIngs.Master.Ingress)
	}

	masterServer = masterNginxCfg.Servers[0]
	// Preserve internal locations (e.g. /_external_auth/...) from the master
	// config. These are needed for server-level auth_request subrequests set
	// by policies on the master Ingress. Minion locations are added later.
	masterInternalLocations := filterInternalLocations(masterServer.Locations)
	masterServer.Locations = []version1.Location{}
	masterPolicyCfg := policiesCfg{CORSHeaders: masterNginxCfg.CORSHeaders}

	upstreams = append(upstreams, masterNginxCfg.Upstreams...)
	balancers := masterNginxCfg.Balancers
	maps = append(maps, masterNginxCfg.Maps...)

	if masterNginxCfg.OIDCProviders != nil {
		oidcProviders = append(oidcProviders, masterNginxCfg.OIDCProviders...)
	}

	// http2 is server-level and minions share the master's server, so the minions' gRPC check
	// must use the master's value (nginx.org/http2 is removed from minions).
	minionBaseCfgParams := *ncp.BaseCfgParams
	minionBaseCfgParams.HTTP2 = masterServer.HTTP2

	minions := ncp.mergeableIngs.Minions
	grpcOnly := true
	hasGRPCLocations := false
	for _, minion := range minions {
		// replace minion with a deepcopy because we will modify it
		originalMinion := minion.Ingress
		minion.Ingress = minion.Ingress.DeepCopy()

		// Remove the default backend so that "/" will not be generated
		minion.Ingress.Spec.DefaultBackend = nil

		// Add acceptable master annotations to minion
		mergeMasterAnnotationsIntoMinion(minion.Ingress.Annotations, ncp.mergeableIngs.Master.Ingress.Annotations)

		// Parse the minion's add-header annotation once, outside the location loop.
		// The parsed headers are placed directly on each loc.AddHeaders below.
		// Parsing here avoids repeating the work once per location for multi-path minions.
		minionAddHeaders := version1.ParseAddHeaders(minion.Ingress.Annotations[AddHeaderAnnotation])

		removedAnnotations = filterMinionAnnotations(minion.Ingress.Annotations)
		if len(removedAnnotations) != 0 {
			nl.Errorf(l, "Ingress Resource %v/%v with the annotation 'nginx.org/mergeable-ingress-type' set to 'minion' cannot contain the %v annotation(s). They will be ignored",
				minion.Ingress.Namespace, minion.Ingress.Name, strings.Join(removedAnnotations, ","))
		}

		isMinion := true
		// App Protect Resources not allowed in minions - pass empty struct
		dummyApResources := &AppProtectResources{}
		dummyDosResource := &appProtectDosResource{}
		minionNginxCfg, minionWarnings := generateNginxCfg(NginxCfgParams{
			mergeableIngs:             ncp.mergeableIngs,
			staticParams:              ncp.staticParams,
			ingEx:                     minion,
			apResources:               dummyApResources,
			dosResource:               dummyDosResource,
			isMinion:                  isMinion,
			isPlus:                    ncp.isPlus,
			BaseCfgParams:             &minionBaseCfgParams,
			isResolverConfigured:      ncp.isResolverConfigured,
			isWildcardEnabled:         ncp.isWildcardEnabled,
			ingressControllerReplicas: ncp.ingressControllerReplicas,
			defaultBackend:            ncp.defaultBackend,
			oidcNativeLocations:       sharedOIDCLocations,
		})
		warnings.Add(minionWarnings)

		// because minion.Ingress is a deepcopy of the original minion
		// we need to change the key in the warnings to the original minion
		if _, exists := warnings[minion.Ingress]; exists {
			warnings[originalMinion] = warnings[minion.Ingress]
			delete(warnings, minion.Ingress)
		}

		for _, server := range minionNginxCfg.Servers {
			for _, loc := range server.Locations {
				// Skip internal auth locations that duplicate the master's.
				// The master already generated these for server-level auth_request.
				if loc.Internal && isMasterInternalLocation(loc, masterInternalLocations) {
					continue
				}

				// If the minion location references the same external auth policy
				// as the master, clear the location-level ExternalAuth. The
				// server-level auth_request directive from the master already
				// covers this location via NGINX directive inheritance.
				if loc.ExternalAuth != nil && masterServer.ExternalAuth != nil &&
					loc.ExternalAuth.URI.Upstream == masterServer.ExternalAuth.URI.Upstream {
					loc.ExternalAuth = nil
				}

				if !loc.CORSEnabled && len(masterPolicyCfg.CORSHeaders) > 0 {
					// Mergeable mode fallback: master CORS applies when minion location has no own CORS.
					loc.AddHeaders = append(loc.AddHeaders, masterPolicyCfg.CORSHeaders...)
					loc.CORSEnabled = true
				}
				loc.MinionIngress = &minionNginxCfg.Ingress
				// Merge proxy-set-headers: minion headers take priority over master headers.
				// Skip auth-infrastructure locations (internal auth subrequest and
				// oauth2 signin redirect) — they carry purpose-specific headers
				// that must not be overwritten by the annotation.
				if !loc.Internal && !loc.AuthRequestOff {
					masterAnnotation := ncp.mergeableIngs.Master.Ingress.Annotations[ProxySetHeadersAnnotation]
					minionAnnotation := minion.Ingress.Annotations[ProxySetHeadersAnnotation]
					loc.ProxySetHeaders = version1.MergeProxySetHeaders(masterAnnotation, minionAnnotation)
				}

				// nginx.org/add-header on a minion goes directly to the location block.
				// Master annotation headers stay in the server block (masterServer.AddHeaders)
				// and are not injected here
				// Locations with their own add_header will not inherit server-level master
				// headers via NGINX (known override rule); add-header-inherit will address that.
				if !loc.Internal && len(minionAddHeaders) > 0 {
					// Prepend before any CORS headers already on loc.AddHeaders so that
					// annotation-sourced headers appear first in the location block.
					loc.AddHeaders = append(minionAddHeaders, loc.AddHeaders...)
				}

				if !loc.Internal {
					// Imported minion locations participate in the same server-level gRPC
					// bookkeeping: any gRPC location requires @grpcerror* handlers, and any
					// HTTP location makes the merged server non-gRPC-only.
					if loc.GRPC {
						hasGRPCLocations = true
					} else {
						grpcOnly = false
					}
				}

				locations = append(locations, loc)
			}
			for hcName, healthCheck := range server.HealthChecks {
				healthChecks[hcName] = healthCheck
			}
			masterServer.JWTRedirectLocations = append(masterServer.JWTRedirectLocations, server.JWTRedirectLocations...)
		}

		if minionNginxCfg.OIDCProviders != nil {
			oidcProviders = append(oidcProviders, minionNginxCfg.OIDCProviders...)
		}

		upstreams = append(upstreams, minionNginxCfg.Upstreams...)
		balancers = mergeBalancers(balancers, minionNginxCfg.Balancers)
		for primary, canary := range minionNginxCfg.Canaries {
			if canaries == nil {
				canaries = make(map[string]nginx.Canary)
			}
			canaries[primary] = canary
		}
		limitReqZones = append(limitReqZones, minionNginxCfg.LimitReqZones...)
		ingressNginxRateLimits = append(ingressNginxRateLimits, minionNginxCfg.IngressNginxRateLimits...)
		ingressNginxAuthCaches = append(ingressNginxAuthCaches, minionNginxCfg.IngressNginxAuthCaches...)
		ingressNginxAuthUpstreams = append(ingressNginxAuthUpstreams, minionNginxCfg.IngressNginxAuthUpstreams...)
		for _, snippet := range minionNginxCfg.IngressNginxStreamSnippets {
			ingressNginxStreamSnippets = append(ingressNginxStreamSnippets, []string{snippet})
		}
		maps = append(maps, minionNginxCfg.Maps...)
	}

	var keyValZones []version2.KeyValZone
	dedupedOIDCProviders := removeDuplicateOIDCProviders(oidcProviders)
	for _, p := range dedupedOIDCProviders {
		if p.SessionStore != "" {
			timeout := p.SessionTimeout
			if p.Sync && timeout == "" {
				timeout = oidcNativeSessionSyncDefaultTimeout
			}
			keyValZones = append(keyValZones, version2.KeyValZone{
				Name:    p.SessionStore,
				Size:    oidcNativeSessionZoneSize,
				Sync:    p.Sync,
				Timeout: timeout,
			})
		}
	}

	masterServer.HealthChecks = healthChecks
	masterServer.Locations = append(masterInternalLocations, locations...)
	// a use-regex or rewrite-target of one minion makes the paths of all of them regular expressions
	enforceIngressNginxRegex(&masterServer, ncp.mergeableIngs.Master.Ingress.Annotations)
	masterServer.ACMEChallengeActive = hasACMEChallengeLocation(masterServer.Locations)
	masterServer.HasGRPCLocations = hasGRPCLocations
	masterServer.GRPCOnly = hasGRPCLocations && grpcOnly

	// Enable the shared @custom_default_backend location on the master server if
	// any minion location carries custom-http-errors codes. The handler upstream
	// is the master's spec.defaultBackend (already created in generateNginxCfg
	// for the master); if the master has no defaultBackend, per-minion warnings
	// were already emitted in generateNginxCfg and nothing is rendered here.
	if originalMaster.Spec.DefaultBackend != nil {
		for _, loc := range locations {
			if len(loc.CustomHTTPErrorCodes) > 0 {
				masterServer.CustomHTTPErrorBackend = getNameForUpstream(originalMaster, emptyHostName, originalMaster.Spec.DefaultBackend)
				break
			}
		}
	}

	return version1.IngressNginxConfig{
		Servers:                    []version1.Server{masterServer},
		Upstreams:                  upstreams,
		OIDCProviders:              dedupedOIDCProviders,
		KeyValZones:                keyValZones,
		Ingress:                    masterNginxCfg.Ingress,
		DynamicSSLReloadEnabled:    ncp.staticParams.DynamicSSLReload,
		StaticSSLPath:              ncp.staticParams.StaticSSLPath,
		DefaultCertificate:         ncp.staticParams.defaultCertificate(),
		LimitReqZones:              limitReqZones,
		IngressNginxRateLimits:     ingressNginxRateLimits,
		IngressNginxAuthCaches:     ingressNginxAuthCaches,
		IngressNginxAuthUpstreams:  uniqueAuthUpstreams(ingressNginxAuthUpstreams),
		IngressNginxStreamSnippets: uniqueStreamSnippets(ingressNginxStreamSnippets...),
		Maps:                       removeDuplicateMaps(maps),
		AppProtectLoadModule:       ncp.staticParams.MainAppProtectLoadModule,
		HTTP2:                      ncp.BaseCfgParams.HTTP2,
		Canaries:                   canaries,
		Balancers:                  balancers,
	}, warnings
}

// filterInternalLocations returns only the locations marked as Internal.
// This preserves internal auth subrequest locations (e.g. /_external_auth/...)
// from the master config when clearing master locations for mergeable ingresses.
func filterInternalLocations(locations []version1.Location) []version1.Location {
	var internal []version1.Location
	for _, loc := range locations {
		if loc.Internal {
			internal = append(internal, loc)
		}
	}
	return internal
}

// isMasterInternalLocation reports whether loc is a duplicate of one of the
// master's internal locations (matched by Path). This is used to avoid
// emitting duplicate internal auth subrequest locations when a minion
// references the same external auth policy as the master.
func isMasterInternalLocation(loc version1.Location, masterInternalLocs []version1.Location) bool {
	for _, masterLoc := range masterInternalLocs {
		if loc.Path == masterLoc.Path {
			return true
		}
	}
	return false
}

func limitReqZoneExists(zones []version1.LimitReqZone, zoneName string) bool {
	for _, zone := range zones {
		if zone.Name == zoneName {
			return true
		}
	}
	return false
}

func isSSLEnabled(isSSLService bool) bool {
	return isSSLService
}

// GetBackendPortAsString returns the port of a ServiceBackend of an Ingress resource as a string.
func GetBackendPortAsString(port networking.ServiceBackendPort) string {
	if port.Name != "" {
		return port.Name
	}
	return strconv.Itoa(int(port.Number))
}

// scaleRatelimit divides a given ratelimit by the given number of replicas, adjusting the unit and flooring the result as needed
func scaleRatelimit(ratelimit string, replicas int) string {
	if replicas < 1 {
		return ratelimit
	}

	match := rateRegexp.FindStringSubmatch(ratelimit)
	if match == nil {
		return ratelimit
	}

	number, err := strconv.Atoi(match[1])
	if err != nil {
		return ratelimit
	}

	numberf := float64(number) / float64(replicas)

	unit := match[2]
	if unit == "r/s" && numberf < 1 {
		numberf = numberf * 60
		unit = "r/m"
	}

	return strconv.Itoa(int(numberf)) + unit
}
