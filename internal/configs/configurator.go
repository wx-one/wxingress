package configs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	nl "github.com/wx-one/wxingress/internal/logger"

	"github.com/wx-one/wxingress/internal/k8s/secrets"
	"github.com/wx-one/wxingress/pkg/apis/dos/v1beta1"

	"github.com/nginx/nginx-prometheus-exporter/collector"

	"github.com/wx-one/wxingress/internal/configs/version2"

	api_v1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/nginx"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	latCollector "github.com/wx-one/wxingress/internal/metrics/collectors"
)

const (
	pemFileNameForWildcardTLSSecret = "/etc/nginx/secrets/wildcard" // #nosec G101
	appProtectPolicyFolder          = "/etc/nginx/waf/nac-policies/"
	appProtectLogConfFolder         = "/etc/nginx/waf/nac-logconfs/"
	appProtectUserSigFolder         = "/etc/nginx/waf/nac-usersigs/"
	appProtectUserSigIndex          = "/etc/nginx/waf/nac-usersigs/index.conf"
	appProtectDosPolicyFolder       = "/etc/nginx/dos/policies/"
	appProtectDosLogConfFolder      = "/etc/nginx/dos/logconfs/"
	appProtectDosAllowListFolder    = "/etc/nginx/dos/allowlist/"

	// sharedInputFailureThreshold is the number of consecutive per-resource config
	// validation failures that triggers early exit from UpdateConfig's resource loop.
	// When reached, it indicates a likely shared root cause (e.g., a bad
	// server-snippets from ConfigMap rendered into every per-resource file) is
	// causing all resources to fail, and continuing would waste O(N) reloads.

	sharedInputFailureThreshold = 2

	// Secret file name prefixes, one per role that produces a file on disk.
	tlsKeyPairFilePrefix   = "ssl_keypair_"
	caCertBundleFilePrefix = "cert_bundle_"
	caCRLBundleFilePrefix  = "crl_bundle_"
	jwtKeyFilePrefix       = "jwt_key_"
	basicAuthFilePrefix    = "basic_auth_"
)

// DefaultServerSecretPath is the full path to the Secret with a TLS cert and a key for the default server. #nosec G101
const DefaultServerSecretPath = "/etc/nginx/secrets/default" //nolint:gosec // G101: Potential hardcoded credentials - false positive

// DefaultServerConfigName is the config name used for the default server file in conf.d.
const DefaultServerConfigName = "_default-server"

// DefaultSecretPath is the full default path to where secrets are stored and accessed.
const DefaultSecretPath = "/etc/nginx/secrets" // #nosec G101

// DefaultServerSecretFileName is the filename of the Secret with a TLS cert and a key for the default server.
const DefaultServerSecretFileName = "default"

// WildcardSecretFileName is the filename of the Secret with a TLS cert and a key for the ingress resources with TLS termination enabled but not secret defined.
const WildcardSecretFileName = "wildcard"

// LicenseSecretFileName is the filename of the Secret for the NGINX PLUS License
const LicenseSecretFileName = "license.jwt"

// ClientAuthCertSecretFileName is the filename of the Secret with a TLS cert and a key for the MGMT block for client authentication certificates.
const ClientAuthCertSecretFileName = "client"

// JWTKeyKey is the key of the data field of a Secret where the JWK must be stored.
const JWTKeyKey = "jwk"

// HtpasswdFileKey is the key of the data field of a Secret where the HTTP basic authorization list must be stored
const HtpasswdFileKey = "htpasswd"

// CACrtKey is the key of the data field of a Secret where the cert must be stored.
const CACrtKey = "ca.crt"

// CACrlKey is the key of the data field of a Secret where the cert revocation list must be stored.
const CACrlKey = "ca.crl"

// ClientSecretKey is the key of the data field of a Secret where the OIDC client secret must be stored.
const ClientSecretKey = "client-secret"

// ExtendedResources holds all extended configuration resources, for which Configurator configures NGINX.
type ExtendedResources struct {
	IngressExes         []*IngressEx
	MergeableIngresses  []*MergeableIngresses
	VirtualServerExes   []*VirtualServerEx
	TransportServerExes []*TransportServerEx
}

// WeightUpdate holds the information about the weight updates for weight changes without reloading.
type WeightUpdate struct {
	Zone  string
	Key   string
	Value string
}

type tlsPassthroughPair struct {
	Host       string
	UnixSocket string
}

// metricLabelsIndex keeps the relations between Ingress Controller resources and NGINX configuration.
// Used to be able to add Prometheus Metrics variable labels grouped by resource key.
type metricLabelsIndex struct {
	ingressUpstreams             map[string][]string
	virtualServerUpstreams       map[string][]string
	transportServerUpstreams     map[string][]string
	ingressServerZones           map[string][]string
	virtualServerServerZones     map[string][]string
	transportServerServerZones   map[string][]string
	ingressUpstreamPeers         map[string][]string
	virtualServerUpstreamPeers   map[string][]string
	transportServerUpstreamPeers map[string][]string
}

// Configurator configures NGINX.
// Until reloads are enabled via EnableReloads(), the Configurator will not reload NGINX and update NGINX Plus
// upstream servers via NGINX Plus API for configuration changes.
// This allows the Ingress Controller to incrementally build the NGINX configuration during the IC start and
// then apply it at the end of the start.
type Configurator struct {
	nginxManager          nginx.Manager
	staticCfgParams       *StaticConfigParams
	CfgParams             *ConfigParams
	MgmtCfgParams         *MGMTConfigParams
	templateExecutor      *version1.TemplateExecutor
	templateExecutorV2    *version2.TemplateExecutor
	ingresses             map[string]*IngressEx
	minions               map[string]map[string]bool
	mergeableIngresses    map[string]*MergeableIngresses
	virtualServers        map[string]*VirtualServerEx
	virtualServerWarnings map[string]Warnings
	transportServers      map[string]*TransportServerEx
	// ingressPassthroughs are the keys of the TransportServers of ssl-passthrough of each Ingress (by file name)
	ingressPassthroughs map[string][]string
	// ingressStreamSnippets are the Ingresses (by file name) with a file of stream-snippet in stream-conf.d
	ingressStreamSnippets map[string]bool
	// ingressNginxStreams are the TransportServers of tcp-services and udp-services, by namespace/name
	ingressNginxStreams map[string]bool
	// hasDefaultBackend tells that -default-backend-service has its upstream (ingress-nginx-default-backend)
	hasDefaultBackend bool
	// certMetrics are the certificates of the hosts for ingress-nginx's ssl metrics
	certMetrics                  certificateMetrics
	tlsPassthroughPairs          map[string]tlsPassthroughPair
	isWildcardEnabled            bool
	isPlus                       bool
	isUpstreamAPI                bool
	labelUpdater                 collector.LabelUpdater
	metricLabelsIndex            *metricLabelsIndex
	isPrometheusEnabled          bool
	latencyCollector             latCollector.LatencyCollector
	isLatencyMetricsEnabled      bool
	isReloadsEnabled             bool
	isDynamicSSLReloadEnabled    bool
	ingressControllerReplicas    int
	effectiveBatchExclusionCount int
	// canaries holds the canary rules of each Ingress config, by its name and then by the primary upstream.
	canaries map[string]map[string]nginx.Canary
	// appliedCanaries holds the canary rules the wxingress module has, by the primary upstream.
	appliedCanaries map[string]nginx.Canary
	// balancers holds the balancer rules of each Ingress config, by its name and then by the upstream.
	balancers map[string]map[string]nginx.Balancer
	// appliedBalancers holds the balancer rules the wxingress module has, by the upstream.
	appliedBalancers map[string]nginx.Balancer
	// certificates holds the certificate file of each host of each Ingress and VirtualServer config, by the config's name and then by the host.
	certificates map[string]map[string]string
	// appliedCertificates holds what the wxingress module was given for each name.
	appliedCertificates map[string]appliedCertificate
	// readCertificate reads a certificate file.
	readCertificate func(file string) ([]byte, error)
	// statCertificate tells the size and time of change of a certificate file.
	statCertificate func(file string) (os.FileInfo, error)
	// certificateDigests holds the digest of each certificate file when it was last read.
	certificateDigests map[string]fileDigest
	// configShapes holds the digest of each config without the servers of its upstreams (-dynamic-upstreams).
	configShapes map[string][sha256.Size]byte
	// renderedServers holds the servers of the upstreams of each config, by the config's name and then by the upstream, as its file has them.
	renderedServers map[string]map[string]string
	// appliedServers holds the servers the wxingress module has for each upstream.
	appliedServers map[string]string
	// staged holds the resources whose configs are to be written with their new endpoints before the next reload.
	staged map[string]stagedKind
	// lastReload is when NGINX was last reloaded (-dynamic-upstreams).
	lastReload time.Time
	// reloadInterval is the shortest time between two reloads (-dynamic-upstreams).
	reloadInterval time.Duration
	// configsChanged is set when a config changed since UpdateConfig began (-dynamic-upstreams).
	configsChanged bool
	// dhParam is the content of the dhparam file last written.
	dhParam string
	// reloadAsked is set when a reload is asked for while reloads are disabled.
	reloadAsked bool
	// certificatesHeld is set when the certificates were not given to the wxingress module while reloads were disabled.
	certificatesHeld bool
	// canariesHeld is set when the canary rules were not given to the wxingress module while reloads were disabled.
	canariesHeld bool
	// balancersHeld is the same for the balancer rules.
	balancersHeld bool
	// canariesFailed, certificatesFailed and balancersFailed are set when the wxingress module did not take all canary rules, certificates or balancer rules.
	canariesFailed, certificatesFailed, balancersFailed bool
	// gatewayHTTPPorts and gatewayHTTPSPorts are the nginx ports of the
	// listeners of Gateways, which have a default server of their own
	gatewayHTTPPorts            []int
	gatewayHTTPSPorts           []int
	gatewayDefaultServerWritten bool
}

// ConfiguratorParams is a collection of parameters used for the
// NewConfigurator() function
type ConfiguratorParams struct {
	NginxManager                        nginx.Manager
	StaticCfgParams                     *StaticConfigParams
	Config                              *ConfigParams
	MGMTCfgParams                       *MGMTConfigParams
	TemplateExecutor                    *version1.TemplateExecutor
	TemplateExecutorV2                  *version2.TemplateExecutor
	LabelUpdater                        collector.LabelUpdater
	LatencyCollector                    latCollector.LatencyCollector
	IsPlus                              bool
	IsPrometheusEnabled                 bool
	IsWildcardEnabled                   bool
	IsLatencyMetricsEnabled             bool
	IsDynamicSSLReloadEnabled           bool
	IsDynamicWeightChangesReloadEnabled bool
	IsDynamicUpstreams                  bool
	// SyncRateLimit is the most reloads a second with -dynamic-upstreams; 0 for no limit.
	SyncRateLimit float64
	NginxVersion  nginx.Version
}

// NewConfigurator creates a new Configurator.
func NewConfigurator(p ConfiguratorParams) *Configurator {
	metricLabelsIndex := &metricLabelsIndex{
		ingressUpstreams:             make(map[string][]string),
		virtualServerUpstreams:       make(map[string][]string),
		transportServerUpstreams:     make(map[string][]string),
		ingressServerZones:           make(map[string][]string),
		virtualServerServerZones:     make(map[string][]string),
		transportServerServerZones:   make(map[string][]string),
		ingressUpstreamPeers:         make(map[string][]string),
		virtualServerUpstreamPeers:   make(map[string][]string),
		transportServerUpstreamPeers: make(map[string][]string),
	}

	cnf := Configurator{
		nginxManager:              p.NginxManager,
		staticCfgParams:           p.StaticCfgParams,
		CfgParams:                 p.Config,
		MgmtCfgParams:             p.MGMTCfgParams,
		ingresses:                 make(map[string]*IngressEx),
		virtualServers:            make(map[string]*VirtualServerEx),
		virtualServerWarnings:     make(map[string]Warnings),
		transportServers:          make(map[string]*TransportServerEx),
		templateExecutor:          p.TemplateExecutor,
		templateExecutorV2:        p.TemplateExecutorV2,
		minions:                   make(map[string]map[string]bool),
		mergeableIngresses:        make(map[string]*MergeableIngresses),
		tlsPassthroughPairs:       make(map[string]tlsPassthroughPair),
		isPlus:                    p.IsPlus,
		isUpstreamAPI:             p.IsPlus || p.IsDynamicUpstreams,
		isWildcardEnabled:         p.IsWildcardEnabled,
		labelUpdater:              p.LabelUpdater,
		metricLabelsIndex:         metricLabelsIndex,
		isPrometheusEnabled:       p.IsPrometheusEnabled,
		latencyCollector:          p.LatencyCollector,
		isLatencyMetricsEnabled:   p.IsLatencyMetricsEnabled,
		isDynamicSSLReloadEnabled: p.IsDynamicSSLReloadEnabled,
		isReloadsEnabled:          false,
		canaries:                  make(map[string]map[string]nginx.Canary),
		appliedCanaries:           make(map[string]nginx.Canary),
		certificates:              make(map[string]map[string]string),
		appliedCertificates:       make(map[string]appliedCertificate),
		readCertificate:           readCertificateFile,
		reloadInterval:            reloadInterval(p.SyncRateLimit),
		statCertificate:           os.Stat,
		certificateDigests:        make(map[string]fileDigest),
	}
	return &cnf
}

// AddOrUpdateDHParam creates a dhparam file with the content of the string.
func (cnf *Configurator) AddOrUpdateDHParam(content string) (string, error) {
	return cnf.nginxManager.CreateDHParam(content)
}

func findRemovedKeys(currentKeys []string, newKeys map[string]bool) []string {
	var removedKeys []string
	for _, name := range currentKeys {
		if _, exists := newKeys[name]; !exists {
			removedKeys = append(removedKeys, name)
		}
	}
	return removedKeys
}

// normalizeServerZoneName maps empty-host ingress zones to the underscore token emitted in NGINX config.
func normalizeServerZoneName(zone string) string {
	if zone == emptyHostName {
		return emptyHostToken
	}

	return zone
}

func (cnf *Configurator) updateIngressMetricsLabels(ingEx *IngressEx, upstreams []version1.Upstream) {
	upstreamServerLabels := make(map[string][]string)
	newUpstreams := make(map[string]bool)
	var newUpstreamsNames []string

	upstreamServerPeerLabels := make(map[string][]string)
	newPeers := make(map[string]bool)
	var newPeersIPs []string

	for _, u := range upstreams {
		upstreamServerLabels[u.Name] = []string{u.UpstreamLabels.Service, u.UpstreamLabels.ResourceType, u.UpstreamLabels.ResourceName, u.UpstreamLabels.ResourceNamespace}
		newUpstreams[u.Name] = true
		newUpstreamsNames = append(newUpstreamsNames, u.Name)
		for _, server := range u.UpstreamServers {
			podInfo := ingEx.PodsByIP[server.Address]
			labelKey := fmt.Sprintf("%v/%v", u.Name, server.Address)
			upstreamServerPeerLabels[labelKey] = []string{podInfo.Name}
			newPeers[labelKey] = true
			newPeersIPs = append(newPeersIPs, labelKey)
		}
	}

	key := fmt.Sprintf("%v/%v", ingEx.Ingress.Namespace, ingEx.Ingress.Name)
	removedUpstreams := findRemovedKeys(cnf.metricLabelsIndex.ingressUpstreams[key], newUpstreams)
	cnf.metricLabelsIndex.ingressUpstreams[key] = newUpstreamsNames
	cnf.latencyCollector.UpdateUpstreamServerLabels(upstreamServerLabels)
	cnf.latencyCollector.DeleteUpstreamServerLabels(removedUpstreams)

	removedPeers := findRemovedKeys(cnf.metricLabelsIndex.ingressUpstreamPeers[key], newPeers)
	cnf.metricLabelsIndex.ingressUpstreamPeers[key] = newPeersIPs
	cnf.latencyCollector.UpdateUpstreamServerPeerLabels(upstreamServerPeerLabels)
	cnf.latencyCollector.DeleteUpstreamServerPeerLabels(removedPeers)
	cnf.latencyCollector.DeleteMetrics(removedPeers)

	if cnf.isPlus {
		cnf.labelUpdater.UpdateUpstreamServerLabels(upstreamServerLabels)
		cnf.labelUpdater.DeleteUpstreamServerLabels(removedUpstreams)
		cnf.labelUpdater.UpdateUpstreamServerPeerLabels(upstreamServerPeerLabels)
		cnf.labelUpdater.DeleteUpstreamServerPeerLabels(removedPeers)
		serverZoneLabels := make(map[string][]string)
		newZones := make(map[string]bool)
		var newZonesNames []string
		for _, rule := range ingEx.Ingress.Spec.Rules {
			if ingEx.ValidHosts != nil && !ingEx.ValidHosts[rule.Host] {
				continue
			}
			zoneName := normalizeServerZoneName(rule.Host)
			serverZoneLabels[zoneName] = []string{"ingress", ingEx.Ingress.Name, ingEx.Ingress.Namespace}
			newZones[zoneName] = true
			newZonesNames = append(newZonesNames, zoneName)
		}

		removedZones := findRemovedKeys(cnf.metricLabelsIndex.ingressServerZones[key], newZones)
		cnf.metricLabelsIndex.ingressServerZones[key] = newZonesNames
		cnf.labelUpdater.UpdateServerZoneLabels(serverZoneLabels)
		cnf.labelUpdater.DeleteServerZoneLabels(removedZones)
	}
}

func (cnf *Configurator) deleteIngressMetricsLabels(key string) {
	cnf.latencyCollector.DeleteUpstreamServerLabels(cnf.metricLabelsIndex.ingressUpstreams[key])
	cnf.latencyCollector.DeleteUpstreamServerPeerLabels(cnf.metricLabelsIndex.ingressUpstreamPeers[key])
	cnf.latencyCollector.DeleteMetrics(cnf.metricLabelsIndex.ingressUpstreamPeers[key])

	if cnf.isPlus {
		cnf.labelUpdater.DeleteUpstreamServerLabels(cnf.metricLabelsIndex.ingressUpstreams[key])
		cnf.labelUpdater.DeleteServerZoneLabels(cnf.metricLabelsIndex.ingressServerZones[key])
		cnf.labelUpdater.DeleteUpstreamServerPeerLabels(cnf.metricLabelsIndex.ingressUpstreamPeers[key])
	}

	delete(cnf.metricLabelsIndex.ingressUpstreams, key)
	delete(cnf.metricLabelsIndex.ingressServerZones, key)
	delete(cnf.metricLabelsIndex.ingressUpstreamPeers, key)
}

// AddOrUpdateIngress adds or updates NGINX configuration for the Ingress resource.
func (cnf *Configurator) AddOrUpdateIngress(ingEx *IngressEx) (Warnings, error) {
	changed, warnings, err := cnf.addOrUpdateIngress(ingEx)
	if err != nil {
		return warnings, fmt.Errorf("error adding or updating ingress %v/%v: %w", ingEx.Ingress.Namespace, ingEx.Ingress.Name, err)
	}

	if !changed && cnf.dynamicMode() {
		// what changed - servers, canary rules, certificates - goes to the wxingress module, without a reload
		if err := cnf.applyDynamically([]*IngressEx{ingEx}, nil, nil, nil); err != nil {
			return warnings, fmt.Errorf("error updating the servers, canaries or certificates of %v/%v: %w", ingEx.Ingress.Namespace, ingEx.Ingress.Name, err)
		}
		return warnings, nil
	}

	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		return warnings, fmt.Errorf("error reloading NGINX for %v/%v: %w", ingEx.Ingress.Namespace, ingEx.Ingress.Name, err)
	}

	return warnings, nil
}

// AddOrUpdateIngresses adds or updates NGINX configuration for the list of Ingress resources.
func (cnf *Configurator) AddOrUpdateIngresses(ingExes []*IngressEx) (Warnings, error) {
	allWarnings := newWarnings()
	changed := false

	for _, ingEx := range ingExes {
		ingChanged, warnings, err := cnf.addOrUpdateIngress(ingEx)
		if err != nil {
			return allWarnings, err
		}
		allWarnings.Add(warnings)
		changed = changed || ingChanged
	}

	if !changed && cnf.dynamicMode() {
		return allWarnings, cnf.applyDynamically(ingExes, nil, nil, nil)
	}

	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		return allWarnings, fmt.Errorf("error when reloading NGINX when updating Ingresses: %w", err)
	}

	return allWarnings, nil
}

// virtualServerExForHost takes a hostname and returns a VirtualServerEx for the given hostname.
func (cnf *Configurator) virtualServerExForHost(hostname string) *VirtualServerEx {
	for _, vsEx := range cnf.virtualServers {
		if vsEx.VirtualServer.Spec.Host == hostname {
			return vsEx
		}
	}
	return nil
}

// upstreamsForVirtualServer takes a VirtualServerEx and returns a list of associated upstreams.
func (cnf *Configurator) upstreamsForVirtualServer(vsEx *VirtualServerEx) []string {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	vs := vsEx.VirtualServer
	var upstreamNames []string

	nl.Debugf(l, "Get upstreamName for vs: %s", vs.Spec.Host)

	virtualServerUpstreamNamer := NewUpstreamNamerForVirtualServer(vs)

	for _, u := range vs.Spec.Upstreams {
		upstreamName := virtualServerUpstreamNamer.GetNameForUpstream(u.Name)
		nl.Debugf(l, "upstream: %s, upstreamName: %s", u.Name, upstreamName)
		upstreamNames = append(upstreamNames, upstreamName)
	}

	for _, vsr := range slices.Concat(vsEx.VirtualServerRoutes, vsEx.ChallengeRoutes) {
		upstreamNamer := NewUpstreamNamerForVirtualServerRoute(vs, vsr)
		for _, u := range vsr.Spec.Upstreams {
			upstreamName := upstreamNamer.GetNameForUpstream(u.Name)
			nl.Debugf(l, "upstream: %s, upstreamName: %s", u.Name, upstreamName)
			upstreamNames = append(upstreamNames, upstreamName)

		}
	}

	return upstreamNames
}

// UpstreamsForHost takes a hostname and returns upstreams for the given hostname.
func (cnf *Configurator) UpstreamsForHost(hostname string) []string {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	nl.Debugf(l, "Get upstream for host: %s", hostname)
	vsEx := cnf.virtualServerExForHost(hostname)
	if vsEx != nil {
		return cnf.upstreamsForVirtualServer(vsEx)
	}
	return nil
}

// StreamUpstreamsForName takes a name and returns stream upstreams
// associated with this name. The name represents TS's
// (TransportServer) action name.
func (cnf *Configurator) StreamUpstreamsForName(name string) []string {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	nl.Debugf(l, "Get stream upstreams for name: '%s'", name)
	ts := cnf.transportServerForActionName(name)
	if ts != nil {
		return cnf.streamUpstreamsForTransportServer(ts)
	}
	return nil
}

// transportServerForActionName takes an action name and returns
// Transport Server obj associated with that name.
func (cnf *Configurator) transportServerForActionName(name string) *conf_v1.TransportServer {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	for _, tsEx := range cnf.transportServers {
		nl.Debugf(l, "Check ts action '%s' for requested name: '%s'", tsEx.TransportServer.Spec.Action.Pass, name)
		if tsEx.TransportServer.Spec.Action.Pass == name {
			return tsEx.TransportServer
		}
	}
	return nil
}

// streamUpstreamsForTransportServer takes TransportServer obj and returns
// a list of stream upstreams associated with this TransportServer.
func (cnf *Configurator) streamUpstreamsForTransportServer(ts *conf_v1.TransportServer) []string {
	upstreamNames := make([]string, 0, len(ts.Spec.Upstreams))
	n := newUpstreamNamerForTransportServer(ts)
	for _, u := range ts.Spec.Upstreams {
		un := n.GetNameForUpstream(u.Name)
		upstreamNames = append(upstreamNames, un)
	}
	return upstreamNames
}

// GenerateDefaultServerConfig builds the fallback default server config written to conf.d.
func GenerateDefaultServerConfig(staticCfgParams *StaticConfigParams, cfgParams *ConfigParams) version1.IngressNginxConfig {
	return version1.IngressNginxConfig{
		Servers: []version1.Server{{
			Name:                emptyHostToken,
			StatusZone:          emptyHostToken,
			IsDefaultServer:     true,
			Ports:               []int{staticCfgParams.DefaultHTTPListenerPort},
			SSLPorts:            []int{staticCfgParams.DefaultHTTPSListenerPort},
			SSL:                 true,
			SSLCertificate:      DefaultServerSecretPath,
			SSLCertificateKey:   DefaultServerSecretPath,
			SSLRejectHandshake:  staticCfgParams.SSLRejectHandshake,
			AccessLogOff:        cfgParams.DefaultServerAccessLogOff,
			DefaultServerReturn: cfgParams.DefaultServerReturn,
			HealthStatus:        staticCfgParams.HealthStatus,
			HealthStatusURI:     staticCfgParams.HealthStatusURI,
			ServerTokens:        cfgParams.ServerTokens,
			TLSPassthrough:      staticCfgParams.TLSPassthrough,
			HTTP2:               cfgParams.HTTP2,
			ProxyProtocol:       cfgParams.ProxyProtocol,
			RealIPHeader:        cfgParams.RealIPHeader,
			SetRealIPFrom:       cfgParams.SetRealIPFrom,
			RealIPRecursive:     cfgParams.RealIPRecursive,
			DisableIPV6:         staticCfgParams.DisableIPV6,
			// as ingress-nginx's default server: HSTS on its https answers too
			HSTS:                  cfgParams.IngressNginxDefaults && cfgParams.HSTS,
			HSTSMaxAge:            cfgParams.HSTSMaxAge,
			HSTSIncludeSubdomains: cfgParams.HSTSIncludeSubdomains,
			HSTSPreload:           cfgParams.HSTSPreload,
		}},
		Ingress:                 version1.Ingress{},
		DynamicSSLReloadEnabled: staticCfgParams.DynamicSSLReload,
		StaticSSLPath:           staticCfgParams.StaticSSLPath,
		DefaultCertificate:      staticCfgParams.defaultCertificate(),
	}
}

func (cnf *Configurator) buildDefaultServerConfig() version1.IngressNginxConfig {
	cfg := GenerateDefaultServerConfig(cnf.staticCfgParams, cnf.CfgParams)
	if backend := cnf.ingressNginxDefaultBackend(); backend != "" && cfg.Servers[0].DefaultServerReturn == defaultServerReturnDefault {
		// the default backend answers in place of the default 404; a default-server-return of the ConfigMap wins
		cfg.Servers[0].IngressNginxDefaultBackend = backend
		cfg.Servers[0].DefaultServerReturn = ""
	}
	return cfg
}

func (cnf *Configurator) hasActiveEmptyHostIngress() bool {
	for _, ingEx := range cnf.ingresses {
		if ingEx != nil && ingEx.ValidHosts[emptyHostName] {
			return true
		}
	}
	return false
}

func (cnf *Configurator) syncDefaultServerConfig() error {
	if cnf.hasActiveEmptyHostIngress() {
		return nil
	}

	defaultCfg := cnf.buildDefaultServerConfig()
	content, err := cnf.templateExecutor.ExecuteIngressConfigTemplate(&defaultCfg)
	if err != nil {
		return fmt.Errorf("error generating default server config: %w", err)
	}
	changed, err := cnf.nginxManager.CreateConfig(DefaultServerConfigName, content)
	if err != nil {
		return fmt.Errorf("error writing default server config: %w", err)
	}
	cnf.configChanged(DefaultServerConfigName, content, changed)
	// the default server of the Gateways' ports follows the main one
	if _, err := cnf.writeGatewayDefaultServer(); err != nil {
		return err
	}
	return nil
}

// generateIngressConfig generates the config of an Ingress.
func (cnf *Configurator) generateIngressConfig(ingEx *IngressEx) (version1.IngressNginxConfig, Warnings) {
	apResources := cnf.updateApResources(ingEx)

	cnf.updateDosResource(ingEx.DosEx)
	dosResource := getAppProtectDosResource(ingEx.DosEx)

	isMinion := false
	nginxCfg, warnings := generateNginxCfg(NginxCfgParams{
		staticParams:              cnf.staticCfgParams,
		ingEx:                     ingEx,
		apResources:               apResources,
		dosResource:               dosResource,
		isMinion:                  isMinion,
		isPlus:                    cnf.isPlus,
		BaseCfgParams:             cnf.CfgParams,
		isResolverConfigured:      cnf.IsResolverConfigured(),
		isWildcardEnabled:         cnf.isWildcardEnabled,
		ingressControllerReplicas: cnf.ingressControllerReplicas,
		defaultBackend:            cnf.ingressNginxDefaultBackend(),
	})
	return nginxCfg, warnings
}

// addOrUpdateIngress returns a bool that specifies if the underlying config
// file has changed, and any warnings or errors
func (cnf *Configurator) addOrUpdateIngress(ingEx *IngressEx) (bool, Warnings, error) {
	nginxCfg, warnings := cnf.generateIngressConfig(ingEx)

	name := objectMetaToFileName(&ingEx.Ingress.ObjectMeta)
	// Empty-host Ingresses keep their normal ingress-derived key in cnf.ingresses, but write through the shared default-server config file.
	configName := name
	if ingEx.ValidHosts[emptyHostName] {
		configName = DefaultServerConfigName
	}

	content, err := cnf.templateExecutor.ExecuteIngressConfigTemplate(&nginxCfg)
	if err != nil {
		return false, warnings, fmt.Errorf("error generating Ingress Config %v: %w", configName, err)
	}
	configChanged, err := cnf.nginxManager.CreateConfig(configName, content)
	if err != nil {
		return false, warnings, fmt.Errorf("error validating Ingress config %v: %w", configName, err)
	}
	configChanged = cnf.configChanged(configName, content, configChanged)
	cnf.setRenderedServers(configName, renderedServersOfIngress(nginxCfg.Upstreams))
	// If this ingress moved from its own config file to the shared default-server file, delete the stale dedicated file after the new write succeeds.
	if configName != name {
		if current, exists := cnf.ingresses[name]; exists && !current.ValidHosts[emptyHostName] {
			cnf.nginxManager.DeleteConfig(name)
		}
	}

	cnf.ingresses[name] = ingEx
	delete(cnf.staged, name)
	cnf.setCanaries(name, nginxCfg.Canaries)
	cnf.setBalancers(name, nginxCfg.Balancers)
	cnf.setCertificates(name, certificatesOfIngress(nginxCfg))
	if (cnf.isPlus && cnf.isPrometheusEnabled) || cnf.isLatencyMetricsEnabled {
		cnf.updateIngressMetricsLabels(ingEx, nginxCfg.Upstreams)
	}
	if err := cnf.syncDefaultServerConfig(); err != nil {
		return false, warnings, fmt.Errorf("error syncing default server config for ingress %v: %w", name, err)
	}
	ptChanged, err := cnf.syncIngressNginxPassthroughs(name, ingEx, warnings)
	if err != nil {
		return false, warnings, err
	}
	ssChanged := cnf.syncIngressNginxStreamSnippets(name, nginxCfg.IngressNginxStreamSnippets)
	return configChanged || ptChanged || ssChanged, warnings, nil
}

// ingressNginxStreamSnippetsPrefix is the name of the files in stream-conf.d with the stream-snippet of an Ingress.
const ingressNginxStreamSnippetsPrefix = "ingress-snippets_"

// syncIngressNginxStreamSnippets puts the stream-snippet annotations of the
// Ingress (and those merged into it) into a file of the stream block, or
// removes the file. It tells whether nginx must reload.
func (cnf *Configurator) syncIngressNginxStreamSnippets(name string, snippets []string) bool {
	if cnf.ingressStreamSnippets == nil {
		cnf.ingressStreamSnippets = map[string]bool{}
	}
	file := ingressNginxStreamSnippetsPrefix + name
	if len(snippets) == 0 {
		if !cnf.ingressStreamSnippets[name] {
			return false
		}
		cnf.nginxManager.DeleteStreamConfig(file)
		delete(cnf.ingressStreamSnippets, name)
		return true
	}
	content := []byte("# stream-snippet of " + name + "\n" + strings.Join(snippets, "\n") + "\n")
	changed, err := cnf.nginxManager.CreateStreamConfig(file, content)
	if err != nil {
		nl.Errorf(nl.LoggerFromContext(cnf.CfgParams.Context), "error writing the stream-snippet of %v: %v", name, err)
	}
	cnf.ingressStreamSnippets[name] = true
	// a change of the file always reloads: its upstreams are no business of the module
	return changed
}

// syncIngressNginxPassthroughs puts the TLS passthrough TransportServers of
// ingress-nginx's ssl-passthrough of the Ingress in place of those it had.
func (cnf *Configurator) syncIngressNginxPassthroughs(name string, ingEx *IngressEx, warnings Warnings) (bool, error) {
	var tsExes []*TransportServerEx
	if ingEx != nil {
		var w Warnings
		tsExes, w = ingressNginxPassthroughs(ingEx)
		warnings.Add(w)
		if len(tsExes) > 0 && !cnf.staticCfgParams.TLSPassthrough {
			warnings.AddWarningf(ingEx.Ingress, "%s needs -enable-tls-passthrough (controller.enableTLSPassthrough)", IngressNginxSSLPassthroughAnnotation)
			tsExes = nil
		}
	}
	if cnf.ingressPassthroughs == nil {
		cnf.ingressPassthroughs = map[string][]string{}
	}
	changed := false
	keep := map[string]bool{}
	for _, tsEx := range tsExes {
		key := tsEx.TransportServer.Namespace + "/" + tsEx.TransportServer.Name
		keep[key] = true
		c, w, err := cnf.addOrUpdateTransportServer(tsEx)
		if err != nil {
			return false, fmt.Errorf("error adding the TLS passthrough of %v: %w", name, err)
		}
		warnings.Add(w)
		if !c && cnf.dynamicMode() {
			if err := cnf.applyDynamically(nil, nil, nil, []*TransportServerEx{tsEx}); err != nil {
				return false, fmt.Errorf("error updating the servers of the TLS passthrough of %v: %w", name, err)
			}
		}
		changed = changed || c
	}
	for _, key := range cnf.ingressPassthroughs[name] {
		if !keep[key] {
			if err := cnf.deleteTransportServer(key); err != nil {
				return false, err
			}
			changed = true
		}
	}
	cnf.ingressPassthroughs[name] = slices.Collect(maps.Keys(keep))
	if len(keep) == 0 {
		delete(cnf.ingressPassthroughs, name)
	}
	return changed, nil
}

// AddOrUpdateMergeableIngress adds or updates NGINX configuration for the Ingress resources with Mergeable Types.
func (cnf *Configurator) AddOrUpdateMergeableIngress(mergeableIngs *MergeableIngresses) (Warnings, error) {
	changed, warnings, err := cnf.addOrUpdateMergeableIngress(mergeableIngs)
	if err != nil {
		return warnings, fmt.Errorf("error when adding or updating ingress %v/%v: %w", mergeableIngs.Master.Ingress.Namespace, mergeableIngs.Master.Ingress.Name, err)
	}

	if !changed && cnf.dynamicMode() {
		// what changed - servers, canary rules, certificates - goes to the wxingress module, without a reload
		if err := cnf.applyDynamically(nil, []*MergeableIngresses{mergeableIngs}, nil, nil); err != nil {
			return warnings, fmt.Errorf("error updating the servers, canaries or certificates of %v/%v: %w", mergeableIngs.Master.Ingress.Namespace, mergeableIngs.Master.Ingress.Name, err)
		}
		return warnings, nil
	}

	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		return warnings, fmt.Errorf("error reloading NGINX for %v/%v: %w", mergeableIngs.Master.Ingress.Namespace, mergeableIngs.Master.Ingress.Name, err)
	}

	return warnings, nil
}

// generateMergeableIngressConfig generates the config of a master and its minions.
func (cnf *Configurator) generateMergeableIngressConfig(mergeableIngs *MergeableIngresses) (version1.IngressNginxConfig, Warnings) {
	apResources := cnf.updateApResourcesForMergeableIngresses(mergeableIngs)
	cnf.updateDosResource(mergeableIngs.Master.DosEx)
	dosResource := getAppProtectDosResource(mergeableIngs.Master.DosEx)

	nginxCfg, warnings := generateNginxCfgForMergeableIngresses(NginxCfgParams{
		mergeableIngs:             mergeableIngs,
		apResources:               apResources,
		dosResource:               dosResource,
		BaseCfgParams:             cnf.CfgParams,
		isPlus:                    cnf.isPlus,
		isResolverConfigured:      cnf.IsResolverConfigured(),
		staticParams:              cnf.staticCfgParams,
		isWildcardEnabled:         cnf.isWildcardEnabled,
		ingressControllerReplicas: cnf.ingressControllerReplicas,
		defaultBackend:            cnf.ingressNginxDefaultBackend(),
	})
	return nginxCfg, warnings
}

func (cnf *Configurator) addOrUpdateMergeableIngress(mergeableIngs *MergeableIngresses) (bool, Warnings, error) {
	nginxCfg, warnings := cnf.generateMergeableIngressConfig(mergeableIngs)

	name := objectMetaToFileName(&mergeableIngs.Master.Ingress.ObjectMeta)
	// Empty-host mergeable masters keep their normal ingress-derived key in cnf.ingresses, but write through the shared default-server config file.
	configName := name
	if mergeableIngs.Master.ValidHosts[emptyHostName] {
		configName = DefaultServerConfigName
	}

	content, err := cnf.templateExecutor.ExecuteIngressConfigTemplate(&nginxCfg)
	if err != nil {
		return false, warnings, fmt.Errorf("error generating Ingress Config %v: %w", configName, err)
	}
	changed, err := cnf.nginxManager.CreateConfig(configName, content)
	if err != nil {
		return false, warnings, fmt.Errorf("error validating Ingress config %v: %w", configName, err)
	}
	changed = cnf.configChanged(configName, content, changed)
	cnf.setRenderedServers(configName, renderedServersOfIngress(nginxCfg.Upstreams))
	// If this ingress moved from its own config file to the shared default-server file, delete the stale dedicated file after the new write succeeds.
	if configName != name {
		if current, exists := cnf.ingresses[name]; exists && !current.ValidHosts[emptyHostName] {
			cnf.nginxManager.DeleteConfig(name)
		}
	}

	cnf.ingresses[name] = mergeableIngs.Master
	cnf.minions[name] = make(map[string]bool)
	for _, minion := range mergeableIngs.Minions {
		minionName := objectMetaToFileName(&minion.Ingress.ObjectMeta)
		cnf.minions[name][minionName] = true
	}

	cnf.mergeableIngresses[name] = mergeableIngs
	delete(cnf.staged, name)
	cnf.setCanaries(name, nginxCfg.Canaries)
	cnf.setBalancers(name, nginxCfg.Balancers)
	cnf.setCertificates(name, certificatesOfIngress(nginxCfg))

	if (cnf.isPlus && cnf.isPrometheusEnabled) || cnf.isLatencyMetricsEnabled {
		cnf.updateIngressMetricsLabels(mergeableIngs.Master, nginxCfg.Upstreams)
	}
	if err := cnf.syncDefaultServerConfig(); err != nil {
		return false, warnings, fmt.Errorf("error syncing default server config for mergeable ingress %v: %w", name, err)
	}
	ssChanged := cnf.syncIngressNginxStreamSnippets(name, nginxCfg.IngressNginxStreamSnippets)

	return changed || ssChanged, warnings, nil
}

func (cnf *Configurator) updateVirtualServerMetricsLabels(virtualServerEx *VirtualServerEx, upstreams []version2.Upstream) {
	labels := make(map[string][]string)
	newUpstreams := make(map[string]bool)
	var newUpstreamsNames []string

	upstreamServerPeerLabels := make(map[string][]string)
	newPeers := make(map[string]bool)
	var newPeersIPs []string

	for _, u := range upstreams {
		labels[u.Name] = []string{u.UpstreamLabels.Service, u.UpstreamLabels.ResourceType, u.UpstreamLabels.ResourceName, u.UpstreamLabels.ResourceNamespace}
		newUpstreams[u.Name] = true
		newUpstreamsNames = append(newUpstreamsNames, u.Name)
		for _, server := range u.Servers {
			podInfo := virtualServerEx.PodsByIP[server.Address]
			labelKey := fmt.Sprintf("%v/%v", u.Name, server.Address)
			upstreamServerPeerLabels[labelKey] = []string{podInfo.Name}
			newPeers[labelKey] = true
			newPeersIPs = append(newPeersIPs, labelKey)
		}
	}

	key := fmt.Sprintf("%v/%v", virtualServerEx.VirtualServer.Namespace, virtualServerEx.VirtualServer.Name)

	removedPeers := findRemovedKeys(cnf.metricLabelsIndex.virtualServerUpstreamPeers[key], newPeers)
	cnf.metricLabelsIndex.virtualServerUpstreamPeers[key] = newPeersIPs
	cnf.latencyCollector.UpdateUpstreamServerPeerLabels(upstreamServerPeerLabels)
	cnf.latencyCollector.DeleteUpstreamServerPeerLabels(removedPeers)

	removedUpstreams := findRemovedKeys(cnf.metricLabelsIndex.virtualServerUpstreams[key], newUpstreams)
	cnf.latencyCollector.UpdateUpstreamServerLabels(labels)
	cnf.metricLabelsIndex.virtualServerUpstreams[key] = newUpstreamsNames

	cnf.latencyCollector.DeleteUpstreamServerLabels(removedUpstreams)
	cnf.latencyCollector.DeleteMetrics(removedPeers)

	if cnf.isPlus {
		cnf.labelUpdater.UpdateUpstreamServerPeerLabels(upstreamServerPeerLabels)
		cnf.labelUpdater.DeleteUpstreamServerPeerLabels(removedPeers)
		cnf.labelUpdater.UpdateUpstreamServerLabels(labels)
		cnf.labelUpdater.DeleteUpstreamServerLabels(removedUpstreams)

		serverZoneLabels := make(map[string][]string)
		newZones := make(map[string]bool)
		newZonesNames := []string{virtualServerEx.VirtualServer.Spec.Host}

		serverZoneLabels[virtualServerEx.VirtualServer.Spec.Host] = []string{
			"virtualserver", virtualServerEx.VirtualServer.Name, virtualServerEx.VirtualServer.Namespace,
		}

		newZones[virtualServerEx.VirtualServer.Spec.Host] = true

		removedZones := findRemovedKeys(cnf.metricLabelsIndex.virtualServerServerZones[key], newZones)
		cnf.metricLabelsIndex.virtualServerServerZones[key] = newZonesNames
		cnf.labelUpdater.UpdateServerZoneLabels(serverZoneLabels)
		cnf.labelUpdater.DeleteServerZoneLabels(removedZones)
	}
}

func (cnf *Configurator) deleteVirtualServerMetricsLabels(key string) {
	cnf.latencyCollector.DeleteUpstreamServerLabels(cnf.metricLabelsIndex.virtualServerUpstreams[key])
	cnf.latencyCollector.DeleteUpstreamServerPeerLabels(cnf.metricLabelsIndex.virtualServerUpstreamPeers[key])
	cnf.latencyCollector.DeleteMetrics(cnf.metricLabelsIndex.virtualServerUpstreamPeers[key])

	if cnf.isPlus {
		cnf.labelUpdater.DeleteUpstreamServerLabels(cnf.metricLabelsIndex.virtualServerUpstreams[key])
		cnf.labelUpdater.DeleteServerZoneLabels(cnf.metricLabelsIndex.virtualServerServerZones[key])
		cnf.labelUpdater.DeleteUpstreamServerPeerLabels(cnf.metricLabelsIndex.virtualServerUpstreamPeers[key])
	}

	delete(cnf.metricLabelsIndex.virtualServerUpstreams, key)
	delete(cnf.metricLabelsIndex.virtualServerServerZones, key)
	delete(cnf.metricLabelsIndex.virtualServerUpstreamPeers, key)
}

// GetVirtualServerWarnings returns the warnings produced when the VirtualServer
// with the given namespace/name key was last rendered, or nil if it has not been
// rendered. It lets status-only updates keep reporting warnings that come from
// config generation (for example a missing Service or TLS secret) without
// regenerating the config.
func (cnf *Configurator) GetVirtualServerWarnings(key string) Warnings {
	return cnf.virtualServerWarnings[getFileNameForVirtualServerFromKey(key)]
}

// AddOrUpdateVirtualServer adds or updates NGINX configuration for the VirtualServer resource.
func (cnf *Configurator) AddOrUpdateVirtualServer(virtualServerEx *VirtualServerEx) (Warnings, error) {
	changed, warnings, weightUpdates, err := cnf.addOrUpdateVirtualServer(virtualServerEx)
	if err != nil {
		return warnings, fmt.Errorf("error adding or updating VirtualServer %v/%v: %w", virtualServerEx.VirtualServer.Namespace, virtualServerEx.VirtualServer.Name, err)
	}

	if !changed && len(weightUpdates) == 0 && cnf.dynamicMode() {
		// what changed - servers, certificates - goes to the wxingress module, without a reload
		if err := cnf.applyDynamically(nil, nil, []*VirtualServerEx{virtualServerEx}, nil); err != nil {
			return warnings, fmt.Errorf("error updating the servers or certificates of VirtualServer %v/%v: %w", virtualServerEx.VirtualServer.Namespace, virtualServerEx.VirtualServer.Name, err)
		}
		return warnings, nil
	}

	if len(weightUpdates) > 0 {
		cnf.EnableReloads()
	}

	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		return warnings, fmt.Errorf("error reloading NGINX for VirtualServer %v/%v: %w", virtualServerEx.VirtualServer.Namespace, virtualServerEx.VirtualServer.Name, err)
	}

	for _, weightUpdate := range weightUpdates {
		cnf.nginxManager.UpsertSplitClientsKeyVal(weightUpdate.Zone, weightUpdate.Key, weightUpdate.Value)
	}

	return warnings, nil
}

// generateVirtualServerConfig generates the config of a VirtualServer.
func (cnf *Configurator) generateVirtualServerConfig(virtualServerEx *VirtualServerEx) (version2.VirtualServerConfig, Warnings) {
	apResources := cnf.updateApResourcesForVs(virtualServerEx)
	dosResources := map[string]*appProtectDosResource{}
	for k, v := range virtualServerEx.DosProtectedEx {
		cnf.updateDosResource(v)
		dosRes := getAppProtectDosResource(v)
		if dosRes != nil {
			dosResources[k] = dosRes
		}
	}

	vsc := newVirtualServerConfigurator(cnf.CfgParams, cnf.isPlus, cnf.IsResolverConfigured(), cnf.staticCfgParams, cnf.isWildcardEnabled, nil)
	vsc.IngressControllerReplicas = cnf.ingressControllerReplicas
	vsCfg, warnings := vsc.GenerateVirtualServerConfig(virtualServerEx, apResources, dosResources)
	cnf.applyGatewayExtras(virtualServerEx.VirtualServer, &vsCfg, warnings)
	return vsCfg, warnings
}

func (cnf *Configurator) addOrUpdateVirtualServer(virtualServerEx *VirtualServerEx) (bool, Warnings, []WeightUpdate, error) {
	var weightUpdates []WeightUpdate
	vsCfg, warnings := cnf.generateVirtualServerConfig(virtualServerEx)
	name := getFileNameForVirtualServer(virtualServerEx.VirtualServer)

	content, err := cnf.templateExecutorV2.ExecuteVirtualServerTemplate(&vsCfg)
	if err != nil {
		return false, warnings, weightUpdates, fmt.Errorf("error generating VirtualServer config: %v: %w", name, err)
	}

	// Order matters only when -enable-config-safety=true: ConfigRollbackManager.CreateConfig
	// runs `nginx -t` deletes the new file on failure.
	// VS template emits `include oidc-conf.d/oidc_<ns>_<vs>.conf;`, so the OIDC config must
	// exist on disk before the VS config is written. Without config safety the order is
	// immaterial because validation only runs at Reload().
	var oidcChanged bool
	// without NGINX Plus the wxingress module is the relying party and the
	// VS template has what it needs; the njs config is NGINX Plus's
	if vsCfg.Server.OIDC != nil && cnf.isPlus {
		oidcName := getFileNameForOIDCVirtualServer(virtualServerEx.VirtualServer)

		oidcContent, err := cnf.templateExecutorV2.ExecuteOIDCTemplate(vsCfg.Server.OIDC)
		if err != nil {
			return false, warnings, weightUpdates, fmt.Errorf("error generating VirtualServer OIDC config: %v: %w", oidcName, err)
		}
		oidcChanged = cnf.nginxManager.CreateOIDCConfig(oidcName, oidcContent)
	}

	changed, err := cnf.nginxManager.CreateConfig(name, content)
	if err != nil {
		return false, warnings, weightUpdates, fmt.Errorf("error validating VirtualServer config %v: %w", name, err)
	}
	changed = cnf.configChanged(name, content, changed)
	cnf.setRenderedServers(name, renderedServersOfVirtualServer(vsCfg.Upstreams))
	if oidcChanged {
		changed = true
	}
	cnf.virtualServers[name] = virtualServerEx
	cnf.virtualServerWarnings[name] = warnings
	delete(cnf.staged, name)
	cnf.setCertificates(name, certificatesOfVirtualServer(vsCfg))
	cnf.setBalancers(name, gatewayBalancers(virtualServerEx.VirtualServer))
	if canaries := gatewayCanaries(virtualServerEx.VirtualServer); canaries != nil || strings.HasPrefix(virtualServerEx.VirtualServer.Name, gatewayNamePrefix) {
		cnf.setCanaries(name, canaries)
	}

	if (cnf.isPlus && cnf.isPrometheusEnabled) || cnf.isLatencyMetricsEnabled {
		cnf.updateVirtualServerMetricsLabels(virtualServerEx, vsCfg.Upstreams)
	}

	if cnf.staticCfgParams.DynamicWeightChangesReload && len(vsCfg.TwoWaySplitClients) > 0 {
		for _, splitClient := range vsCfg.TwoWaySplitClients {
			if len(splitClient.Weights) != 2 {
				continue
			}
			variableNamer := *NewVSVariableNamer(virtualServerEx.VirtualServer)
			value := variableNamer.GetNameOfKeyOfMapForWeights(splitClient.SplitClientsIndex, splitClient.Weights[0], splitClient.Weights[1])
			weightUpdates = append(weightUpdates, WeightUpdate{Zone: splitClient.ZoneName, Key: splitClient.Key, Value: value})
		}
	}
	return changed, warnings, weightUpdates, nil
}

// AddOrUpdateVirtualServers adds or updates NGINX configuration for multiple VirtualServer resources.
func (cnf *Configurator) AddOrUpdateVirtualServers(virtualServerExes []*VirtualServerEx) (Warnings, error) {
	allWarnings := newWarnings()
	allWeightUpdates := []WeightUpdate{}
	changed := false

	for _, vsEx := range virtualServerExes {
		vsChanged, warnings, weightUpdates, err := cnf.addOrUpdateVirtualServer(vsEx)
		if err != nil {
			return allWarnings, err
		}
		allWarnings.Add(warnings)
		allWeightUpdates = append(allWeightUpdates, weightUpdates...)
		changed = changed || vsChanged
	}

	if !changed && len(allWeightUpdates) == 0 && cnf.dynamicMode() {
		return allWarnings, cnf.applyDynamically(nil, nil, virtualServerExes, nil)
	}

	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		return allWarnings, fmt.Errorf("error when reloading NGINX when updating Policy: %w", err)
	}

	for _, weightUpdate := range allWeightUpdates {
		cnf.nginxManager.UpsertSplitClientsKeyVal(weightUpdate.Zone, weightUpdate.Key, weightUpdate.Value)
	}

	return allWarnings, nil
}

func (cnf *Configurator) updateTransportServerMetricsLabels(transportServerEx *TransportServerEx, upstreams []version2.StreamUpstream) {
	labels := make(map[string][]string)
	newUpstreams := make(map[string]bool)
	var newUpstreamsNames []string

	upstreamServerPeerLabels := make(map[string][]string)
	newPeers := make(map[string]bool)
	var newPeersIPs []string

	for _, u := range upstreams {
		labels[u.Name] = []string{u.UpstreamLabels.Service, u.UpstreamLabels.ResourceType, u.UpstreamLabels.ResourceName, u.UpstreamLabels.ResourceNamespace}
		newUpstreams[u.Name] = true
		newUpstreamsNames = append(newUpstreamsNames, u.Name)

		for _, server := range u.Servers {
			podName := transportServerEx.PodsByIP[server.Address]
			labelKey := fmt.Sprintf("%v/%v", u.Name, server.Address)
			upstreamServerPeerLabels[labelKey] = []string{podName}

			newPeers[labelKey] = true
			newPeersIPs = append(newPeersIPs, labelKey)
		}
	}

	key := fmt.Sprintf("%v/%v", transportServerEx.TransportServer.Namespace, transportServerEx.TransportServer.Name)

	removedPeers := findRemovedKeys(cnf.metricLabelsIndex.transportServerUpstreamPeers[key], newPeers)
	cnf.metricLabelsIndex.transportServerUpstreamPeers[key] = newPeersIPs

	removedUpstreams := findRemovedKeys(cnf.metricLabelsIndex.transportServerUpstreams[key], newUpstreams)
	cnf.metricLabelsIndex.transportServerUpstreams[key] = newUpstreamsNames
	cnf.labelUpdater.UpdateStreamUpstreamServerPeerLabels(upstreamServerPeerLabels)
	cnf.labelUpdater.DeleteStreamUpstreamServerPeerLabels(removedPeers)
	cnf.labelUpdater.UpdateStreamUpstreamServerLabels(labels)
	cnf.labelUpdater.DeleteStreamUpstreamServerLabels(removedUpstreams)

	streamServerZoneLabels := make(map[string][]string)
	newZones := make(map[string]bool)
	zoneName := transportServerEx.TransportServer.Spec.Listener.Name

	if transportServerEx.TransportServer.Spec.Host != "" {
		zoneName = transportServerEx.TransportServer.Spec.Host
	}

	newZonesNames := []string{zoneName}

	streamServerZoneLabels[zoneName] = []string{
		"transportserver", transportServerEx.TransportServer.Name, transportServerEx.TransportServer.Namespace,
	}

	newZones[zoneName] = true
	removedZones := findRemovedKeys(cnf.metricLabelsIndex.transportServerServerZones[key], newZones)
	cnf.metricLabelsIndex.transportServerServerZones[key] = newZonesNames
	cnf.labelUpdater.UpdateStreamServerZoneLabels(streamServerZoneLabels)
	cnf.labelUpdater.DeleteStreamServerZoneLabels(removedZones)
}

func (cnf *Configurator) deleteTransportServerMetricsLabels(key string) {
	cnf.labelUpdater.DeleteStreamUpstreamServerLabels(cnf.metricLabelsIndex.transportServerUpstreams[key])
	cnf.labelUpdater.DeleteStreamServerZoneLabels(cnf.metricLabelsIndex.transportServerServerZones[key])
	cnf.labelUpdater.DeleteStreamUpstreamServerPeerLabels(cnf.metricLabelsIndex.transportServerUpstreamPeers[key])

	delete(cnf.metricLabelsIndex.transportServerUpstreams, key)
	delete(cnf.metricLabelsIndex.transportServerServerZones, key)
	delete(cnf.metricLabelsIndex.transportServerUpstreamPeers, key)
}

// AddOrUpdateTransportServer adds or updates NGINX configuration for the TransportServer resource.
// It is a responsibility of the caller to check that the TransportServer references an existing listener.
func (cnf *Configurator) AddOrUpdateTransportServer(transportServerEx *TransportServerEx) (Warnings, error) {
	changed, warnings, err := cnf.addOrUpdateTransportServer(transportServerEx)
	if err != nil {
		return nil, fmt.Errorf("error adding or updating TransportServer %v/%v: %w", transportServerEx.TransportServer.Namespace, transportServerEx.TransportServer.Name, err)
	}
	if !changed && cnf.dynamicMode() {
		// only the servers of its upstreams changed: they go to the wxingress module, without a reload
		if err := cnf.applyDynamically(nil, nil, nil, []*TransportServerEx{transportServerEx}); err != nil {
			return warnings, fmt.Errorf("error updating the servers of TransportServer %v/%v: %w", transportServerEx.TransportServer.Namespace, transportServerEx.TransportServer.Name, err)
		}
		return warnings, nil
	}
	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		return nil, fmt.Errorf("error reloading NGINX for TransportServer %v/%v: %w", transportServerEx.TransportServer.Namespace, transportServerEx.TransportServer.Name, err)
	}
	return warnings, nil
}

func (cnf *Configurator) addOrUpdateTransportServer(transportServerEx *TransportServerEx) (bool, Warnings, error) {
	name := getFileNameForTransportServer(transportServerEx.TransportServer)
	tsCfg, warnings := generateTransportServerConfig(transportServerConfigParams{
		transportServerEx:      transportServerEx,
		listenerPort:           transportServerEx.ListenerPort,
		isPlus:                 cnf.isPlus,
		isResolverConfigured:   cnf.IsResolverConfigured(),
		isDynamicReloadEnabled: cnf.staticCfgParams.DynamicSSLReload,
		staticSSLPath:          cnf.staticCfgParams.StaticSSLPath,
	})
	cnf.applyGatewayStreamExtras(transportServerEx.TransportServer, tsCfg, warnings)

	content, err := cnf.templateExecutorV2.ExecuteTransportServerTemplate(tsCfg)
	if err != nil {
		return false, nil, fmt.Errorf("error generating TransportServer config %v: %w", name, err)
	}
	if cnf.isPlus && cnf.isPrometheusEnabled {
		cnf.updateTransportServerMetricsLabels(transportServerEx, tsCfg.Upstreams)
	}
	changed, err := cnf.nginxManager.CreateStreamConfig(name, content)
	if err != nil {
		return false, nil, fmt.Errorf("error validating TransportServer config %v: %w", name, err)
	}
	if cnf.dynamicMode() {
		changed = cnf.configChanged(name, content, changed)
		cnf.setRenderedServers(name, renderedServersOfTransportServer(tsCfg.Upstreams))
	} else if changed {
		cnf.configsChanged = true
	}

	cnf.transportServers[name] = transportServerEx

	// update TLS Passthrough Hosts config in case we have a TLS Passthrough TransportServer
	// A non empty Host, may be a TLS Passthrough TransportServer but we have to check for the existence of the TLS Passthrough listener also, as TransportServers that terminate at the NGINX level can have non empty Hosts now too
	isTLSPassthrough := transportServerEx.TransportServer.Spec.Listener.Name == conf_v1.TLSPassthroughListenerName
	if transportServerEx.TransportServer.Spec.Host != "" && isTLSPassthrough {
		key := generateNamespaceNameKey(&transportServerEx.TransportServer.ObjectMeta)
		cnf.tlsPassthroughPairs[key] = tlsPassthroughPair{
			Host:       transportServerEx.TransportServer.Spec.Host,
			UnixSocket: generateUnixSocket(transportServerEx),
		}
		ptChanged, err := cnf.updateTLSPassthroughHostsConfig()
		if err != nil {
			return false, nil, err
		}
		if ptChanged {
			cnf.configsChanged = true
		}
		return changed || ptChanged, warnings, nil
	}
	return changed, warnings, nil
}

// GetVirtualServerRoutesForVirtualServer returns the virtualServerRoutes that a virtualServer
// references, if that virtualServer exists
func (cnf *Configurator) GetVirtualServerRoutesForVirtualServer(key string) []*conf_v1.VirtualServerRoute {
	vsFileName := getFileNameForVirtualServerFromKey(key)
	if cnf.virtualServers[vsFileName] != nil {
		return cnf.virtualServers[vsFileName].VirtualServerRoutes
	}
	return nil
}

func (cnf *Configurator) updateTLSPassthroughHostsConfig() (bool, error) {
	cfg := generateTLSPassthroughHostsConfig(cnf.tlsPassthroughPairs)

	content, err := cnf.templateExecutorV2.ExecuteTLSPassthroughHostsTemplate(cfg)
	if err != nil {
		return false, fmt.Errorf("error generating config for TLS Passthrough Unix Sockets map: %w", err)
	}

	return cnf.nginxManager.CreateTLSPassthroughHostsConfig(content), nil
}

func generateTLSPassthroughHostsConfig(tlsPassthroughPairs map[string]tlsPassthroughPair) *version2.TLSPassthroughHostsConfig {
	cfg := version2.TLSPassthroughHostsConfig{}

	for _, pair := range tlsPassthroughPairs {
		cfg[pair.Host] = pair.UnixSocket
	}

	return &cfg
}

// AddOrUpdateCASecret writes the secret content to disk returning the files added/updated
func (cnf *Configurator) AddOrUpdateCASecret(secret *api_v1.Secret, crtFileName, crlFileName string) string {
	crtData, crlData := GenerateCAFileContent(secret)
	crtFilePath := cnf.nginxManager.CreateSecret(crtFileName, crtData, nginx.ReadWriteOnlyFileMode)
	crlFilePath := cnf.nginxManager.CreateSecret(crlFileName, crlData, nginx.ReadWriteOnlyFileMode)
	return fmt.Sprintf("%s %s", crtFilePath, crlFilePath)
}

// addOrUpdateCASecretForRole writes a RoleCA Secret under its role-derived file names.
func (cnf *Configurator) addOrUpdateCASecretForRole(secret *api_v1.Secret, key string) secrets.Materialized {
	crtData, crlData := GenerateCAFileContent(secret)

	m := secrets.Materialized{Path: cnf.nginxManager.CreateSecret(
		secretFileName(key, secrets.RoleCA),
		crtData,
		nginx.ReadWriteOnlyFileMode,
	)}

	crlFileName := secretCRLFileName(key)
	if _, hasCRL := secret.Data[secrets.CACrlKey]; hasCRL {
		m.CRLPath = cnf.nginxManager.CreateSecret(
			crlFileName,
			crlData,
			nginx.ReadWriteOnlyFileMode,
		)
	} else {
		cnf.nginxManager.DeleteSecret(crlFileName)
	}

	return m
}

// AddOrUpdateResources adds or updates configuration for resources.
func (cnf *Configurator) AddOrUpdateResources(resources ExtendedResources, reloadIfUnchanged bool) (Warnings, error) {
	allWarnings := newWarnings()
	allWeightUpdates := []WeightUpdate{}
	configsChanged := false

	updateResource := func(updateFunc func() (bool, Warnings, error), namespace, name string) error {
		changed, warnings, err := updateFunc()
		if err != nil {
			return fmt.Errorf("error adding or updating resource %v/%v: %w", namespace, name, err)
		}
		allWarnings.Add(warnings)
		if changed {
			configsChanged = true
		}
		return nil
	}

	updateVSResource := func(updateFunc func() (bool, Warnings, []WeightUpdate, error), namespace, name string) error {
		changed, warnings, weightUpdates, err := updateFunc()
		if err != nil {
			return fmt.Errorf("error adding or updating resource %v/%v: %w", namespace, name, err)
		}
		allWarnings.Add(warnings)
		allWeightUpdates = append(allWeightUpdates, weightUpdates...)

		if changed {
			configsChanged = true
		}
		return nil
	}

	for _, ingEx := range resources.IngressExes {
		err := updateResource(func() (bool, Warnings, error) {
			return cnf.addOrUpdateIngress(ingEx)
		}, ingEx.Ingress.Namespace, ingEx.Ingress.Name)
		if err != nil {
			return nil, err
		}
	}

	for _, m := range resources.MergeableIngresses {
		err := updateResource(func() (bool, Warnings, error) {
			return cnf.addOrUpdateMergeableIngress(m)
		}, m.Master.Ingress.Namespace, m.Master.Ingress.Name)
		if err != nil {
			return nil, err
		}
	}

	for _, vsEx := range resources.VirtualServerExes {
		err := updateVSResource(func() (bool, Warnings, []WeightUpdate, error) {
			return cnf.addOrUpdateVirtualServer(vsEx)
		}, vsEx.VirtualServer.Namespace, vsEx.VirtualServer.Name)
		if err != nil {
			return nil, err
		}
	}

	for _, tsEx := range resources.TransportServerExes {
		err := updateResource(func() (bool, Warnings, error) {
			return cnf.addOrUpdateTransportServer(tsEx)
		}, tsEx.TransportServer.Namespace, tsEx.TransportServer.Name)
		if err != nil {
			return nil, err
		}
	}

	if configsChanged || reloadIfUnchanged {
		if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
			return nil, fmt.Errorf("error when reloading NGINX when updating resources: %w", err)
		}
		return allWarnings, nil
	}

	if cnf.dynamicMode() {
		// what changed - servers, canary rules, certificates - goes to the wxingress module, without a reload
		if err := cnf.applyDynamically(resources.IngressExes, resources.MergeableIngresses, resources.VirtualServerExes, resources.TransportServerExes); err != nil {
			return nil, fmt.Errorf("error when updating the servers, canaries or certificates when updating resources: %w", err)
		}
		return allWarnings, nil
	}

	// a TLS Secret that came, changed or went changes no config when the wxingress module has the certificates
	if err := cnf.syncCertificates(); err != nil {
		return nil, fmt.Errorf("error when updating the certificates when updating resources: %w", err)
	}
	return allWarnings, nil
}

// AddOrUpdateLicenseSecret adds or updates NGINX Plus license secret.
func (cnf *Configurator) AddOrUpdateLicenseSecret(secret *api_v1.Secret) error {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	nl.Debugf(l, "AddOrUpdateLicenseSecret: [%v]", secret.Name)
	data, err := GenerateLicenseSecret(secret)
	if err != nil {
		return err
	}
	cnf.nginxManager.CreateSecret(LicenseSecretFileName, data, nginx.ReadWriteOnlyFileMode)

	return nil
}

// AddOrUpdateSpecialTLSSecrets adds or updates a file with a TLS cert and a key from a Special TLS Secret (eg. DefaultServerSecret, WildcardTLSSecret).
func (cnf *Configurator) AddOrUpdateSpecialTLSSecrets(secret *api_v1.Secret, secretNames []string) {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	nl.Debugf(l, "AddOrUpdateSpecialTLSSecrets: secrets [%v]", secretNames)
	data := GenerateCertAndKeyFileContent(secret)

	for _, secretName := range secretNames {
		cnf.nginxManager.CreateSecret(secretName, data, nginx.ReadWriteOnlyFileMode)
	}

	// the default certificate, and that of the hosts with the wildcard one, from the files just written
	if err := cnf.syncCertificates(); err != nil {
		nl.Errorf(l, "Error updating the certificates: %v", err)
	}
}

// AddOrUpdateMGMTClientAuthSecret adds or updates the MGMT Client Auth Secret file with a TLS cert and key.
func (cnf *Configurator) AddOrUpdateMGMTClientAuthSecret(secret *api_v1.Secret) {
	data := GenerateCertAndKeyFileContent(secret)
	cnf.nginxManager.CreateSecret("mgmt/client", data, nginx.ReadWriteOnlyFileMode)
}

// GenerateCertAndKeyFileContent generates a pem file content from the TLS secret.
func GenerateCertAndKeyFileContent(secret *api_v1.Secret) []byte {
	var res bytes.Buffer

	res.Write(secret.Data[api_v1.TLSCertKey])
	res.WriteString("\n")
	res.Write(secret.Data[api_v1.TLSPrivateKeyKey])

	return res.Bytes()
}

// GenerateCAFileContent generates a pem file content from the TLS secret.
func GenerateCAFileContent(secret *api_v1.Secret) ([]byte, []byte) {
	var caKey bytes.Buffer
	var caCrl bytes.Buffer

	caKey.Write(secret.Data[CACrtKey])
	caCrl.Write(secret.Data[CACrlKey])

	return caKey.Bytes(), caCrl.Bytes()
}

// GenerateLicenseSecret generates jwt content from the License secret which is required for NGINX Plus.
func GenerateLicenseSecret(secret *api_v1.Secret) ([]byte, error) {
	var licenseKey bytes.Buffer

	data, exists := secret.Data[LicenseSecretFileName]
	if !exists {
		return nil, fmt.Errorf("license secret %s/%s must contain the key %s", secret.Namespace, secret.Name, LicenseSecretFileName)
	}
	licenseKey.Write(data)

	return licenseKey.Bytes(), nil
}

// DeleteIngress deletes NGINX configuration for the Ingress resource.
func (cnf *Configurator) DeleteIngress(key string, skipReload bool) error {
	name := keyToFileName(key)

	if ingEx, exists := cnf.ingresses[name]; !exists || !ingEx.ValidHosts[emptyHostName] {
		cnf.nginxManager.DeleteConfig(name)
	}

	if ingEx, exists := cnf.ingresses[name]; !exists || !ingEx.ValidHosts[emptyHostName] {
		cnf.forgetConfig(name)
	}
	if _, err := cnf.syncIngressNginxPassthroughs(name, nil, newWarnings()); err != nil {
		return err
	}
	cnf.syncIngressNginxStreamSnippets(name, nil)
	delete(cnf.ingresses, name)
	delete(cnf.staged, name)
	delete(cnf.minions, name)
	delete(cnf.mergeableIngresses, name)
	delete(cnf.canaries, name)
	cnf.deleteBalancers(name)
	delete(cnf.certificates, name)
	if err := cnf.syncDefaultServerConfig(); err != nil {
		return fmt.Errorf("error syncing default server config after deleting ingress %v: %w", key, err)
	}

	if (cnf.isPlus && cnf.isPrometheusEnabled) || cnf.isLatencyMetricsEnabled {
		cnf.deleteIngressMetricsLabels(key)
	}

	if !skipReload {
		if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
			return fmt.Errorf("error when removing ingress %v: %w", key, err)
		}
	}

	return nil
}

// DeleteVirtualServer deletes NGINX configuration for the VirtualServer resource.
func (cnf *Configurator) DeleteVirtualServer(key string, skipReload bool) error {
	name := getFileNameForVirtualServerFromKey(key)
	cnf.nginxManager.DeleteConfig(name)
	if cnf.virtualServers[name] != nil {
		for _, policy := range cnf.virtualServers[name].Policies {
			if policy.Spec.OIDC != nil {
				oidcName := getFileNameForOIDCVirtualServer(cnf.virtualServers[name].VirtualServer)
				cnf.nginxManager.DeleteOIDCConfig(oidcName)
			}
		}
	}

	if cnf.isPlus {
		cnf.nginxManager.DeleteKeyValStateFiles(name)
	}

	cnf.forgetConfig(name)
	delete(cnf.virtualServers, name)
	delete(cnf.staged, name)
	delete(cnf.virtualServerWarnings, name)
	delete(cnf.certificates, name)
	cnf.deleteBalancers(name)
	if (cnf.isPlus && cnf.isPrometheusEnabled) || cnf.isLatencyMetricsEnabled {
		cnf.deleteVirtualServerMetricsLabels(key)
	}

	if !skipReload {
		if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
			return fmt.Errorf("error when removing VirtualServer %v: %w", key, err)
		}
	}

	return nil
}

// DeleteTransportServer deletes NGINX configuration for the TransportServer resource.
func (cnf *Configurator) DeleteTransportServer(key string) error {
	if cnf.isPlus && cnf.isPrometheusEnabled {
		cnf.deleteTransportServerMetricsLabels(key)
	}

	err := cnf.deleteTransportServer(key)
	if err != nil {
		return fmt.Errorf("error when removing TransportServer %v: %w", key, err)
	}

	err = cnf.Reload(nginx.ReloadForOtherUpdate)
	if err != nil {
		return fmt.Errorf("error when removing TransportServer %v: %w", key, err)
	}

	return nil
}

func (cnf *Configurator) deleteTransportServer(key string) error {
	name := getFileNameForTransportServerFromKey(key)
	cnf.nginxManager.DeleteStreamConfig(name)
	cnf.forgetConfig(name)

	delete(cnf.transportServers, name)
	// update TLS Passthrough Hosts config in case we have a TLS Passthrough TransportServer
	if _, exists := cnf.tlsPassthroughPairs[key]; exists {
		delete(cnf.tlsPassthroughPairs, key)
		_, err := cnf.updateTLSPassthroughHostsConfig()
		return err
	}

	return nil
}

// UpdateEndpoints updates endpoints in NGINX configuration for the Ingress resources.
func (cnf *Configurator) UpdateEndpoints(ingExes []*IngressEx) (Warnings, error) {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	reloadPlus := false
	allWarnings := newWarnings()

	for _, ingEx := range ingExes {
		if cnf.dynamicMode() {
			allWarnings.Add(cnf.stageIngress(ingEx))
		} else {
			_, warnings, err := cnf.addOrUpdateIngress(ingEx)
			if err != nil {
				return allWarnings, fmt.Errorf("error adding or updating ingress %v/%v: %w", ingEx.Ingress.Namespace, ingEx.Ingress.Name, err)
			}
			allWarnings.Add(warnings)
		}

		if cnf.isUpstreamAPI {
			err := cnf.updatePlusEndpoints(ingEx, ingEx.Ingress)
			if err != nil {
				nl.Warnf(l, "Couldn't update the endpoints via the API: %v; reloading configuration instead", err)
				reloadPlus = true
			}
		}
	}

	if cnf.isUpstreamAPI && !reloadPlus {
		nl.Debug(l, "No need to reload nginx")
		return allWarnings, nil
	}

	if err := cnf.Reload(nginx.ReloadForEndpointsUpdate); err != nil {
		return allWarnings, fmt.Errorf("error reloading NGINX when updating endpoints: %w", err)
	}

	return allWarnings, nil
}

// UpdateEndpointsMergeableIngress updates endpoints in NGINX configuration for a mergeable Ingress resource.
func (cnf *Configurator) UpdateEndpointsMergeableIngress(mergeableIngresses []*MergeableIngresses) (Warnings, error) {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	reloadPlus := false
	allWarnings := newWarnings()

	for i := range mergeableIngresses {
		mergeableIng := mergeableIngresses[i]
		if cnf.dynamicMode() {
			allWarnings.Add(cnf.stageMergeableIngress(mergeableIng))
		} else {
			_, warnings, err := cnf.addOrUpdateMergeableIngress(mergeableIngresses[i])
			if err != nil {
				return allWarnings, fmt.Errorf("error adding or updating mergeableIngress %v/%v: %w", mergeableIngresses[i].Master.Ingress.Namespace, mergeableIngresses[i].Master.Ingress.Name, err)
			}
			allWarnings.Add(warnings)
		}

		if cnf.isUpstreamAPI {

			err := cnf.updatePlusEndpoints(mergeableIng.Master, mergeableIng.Master.Ingress)
			if err != nil {
				nl.Warnf(l, "Couldn't update the endpoints via the API: %v; reloading configuration instead", err)
				reloadPlus = true
			}

			for _, ing := range mergeableIngresses[i].Minions {
				err = cnf.updatePlusEndpoints(ing, mergeableIng.Master.Ingress)
				if err != nil {
					nl.Warnf(l, "Couldn't update the endpoints via the API: %v; reloading configuration instead", err)
					reloadPlus = true
				}
			}
		}
	}

	if cnf.isUpstreamAPI && !reloadPlus {
		nl.Debug(l, "No need to reload nginx")
		return allWarnings, nil
	}

	if err := cnf.Reload(nginx.ReloadForEndpointsUpdate); err != nil {
		return allWarnings, fmt.Errorf("error reloading NGINX when updating endpoints for %v: %w", mergeableIngresses, err)
	}

	return allWarnings, nil
}

// UpdateEndpointsForVirtualServers updates endpoints in NGINX configuration for the VirtualServer resources.
func (cnf *Configurator) UpdateEndpointsForVirtualServers(virtualServerExes []*VirtualServerEx) (Warnings, error) {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	reloadPlus := false
	allWarnings := newWarnings()

	for _, vs := range virtualServerExes {
		if cnf.dynamicMode() {
			allWarnings.Add(cnf.stageVirtualServer(vs))
		} else {
			_, warnings, _, err := cnf.addOrUpdateVirtualServer(vs)
			if err != nil {
				return allWarnings, fmt.Errorf("error adding or updating VirtualServer %v/%v: %w", vs.VirtualServer.Namespace, vs.VirtualServer.Name, err)
			}
			allWarnings.Add(warnings)
		}

		if cnf.isUpstreamAPI {
			err := cnf.updatePlusEndpointsForVirtualServer(vs)
			if err != nil {
				nl.Warnf(l, "Couldn't update the endpoints via the API: %v; reloading configuration instead", err)
				reloadPlus = true
			}
		}
	}

	if cnf.isUpstreamAPI && !reloadPlus {
		nl.Debug(l, "No need to reload nginx")
		return allWarnings, nil
	}

	if err := cnf.Reload(nginx.ReloadForEndpointsUpdate); err != nil {
		return allWarnings, fmt.Errorf("error reloading NGINX when updating endpoints: %w", err)
	}

	return allWarnings, nil
}

func (cnf *Configurator) updatePlusEndpointsForVirtualServer(virtualServerEx *VirtualServerEx) error {
	upstreams := createUpstreamsForPlus(virtualServerEx, cnf.CfgParams, cnf.staticCfgParams)
	for _, upstream := range upstreams {
		serverCfg := createUpstreamServersConfigForPlus(upstream)

		endpoints := createEndpointsFromUpstream(upstream)

		err := cnf.updateServersInPlus(upstream.Name, endpoints, serverCfg)
		if err != nil {
			return fmt.Errorf("couldn't update the endpoints for %v: %w", upstream.Name, err)
		}
	}

	// Update external auth upstreams via the Plus API.
	// These are generated from policies referenced by the VirtualServer
	// and are not part of the VirtualServer spec upstreams, so they need separate handling.
	for _, pol := range virtualServerEx.Policies {
		if pol.Spec.ExternalAuth == nil || pol.Spec.ExternalAuth.AuthServiceName == "" {
			continue
		}
		exAuth := pol.Spec.ExternalAuth
		port := getExternalAuthPort(exAuth)
		ns, svcName := ParseServiceReference(exAuth.AuthServiceName, pol.Namespace)
		endpointKey := fmt.Sprintf("%s/%s:%d", ns, svcName, port)
		if endps, exists := virtualServerEx.Endpoints[endpointKey]; exists {
			cfg := nginx.ServerConfig{
				MaxFails:    1,
				MaxConns:    0,
				FailTimeout: "10s",
			}
			for _, upstreamName := range externalAuthUpstreams(virtualServerEx, pol) {
				err := cnf.updateServersInPlus(upstreamName, endps, cfg)
				if err != nil {
					return fmt.Errorf("couldn't update the endpoints for external auth upstream %v: %w", upstreamName, err)
				}
			}
		}
	}

	return nil
}

// externalAuthUpstreams are the nginx names of the upstreams of an external
// auth policy in the config of a VirtualServer: vs_exauth_<namespace>_<name>
// named as the config generation names it, by the VirtualServer when the
// VirtualServer or one of its routes refers to the policy, by the
// VirtualServerRoute when one of its subroutes does.
func externalAuthUpstreams(vsEx *VirtualServerEx, pol *conf_v1.Policy) []string {
	name := fmt.Sprintf("vs_exauth_%s_%s", pol.Namespace, pol.Name)
	refers := func(refs []conf_v1.PolicyReference, namespace string) bool {
		for _, r := range refs {
			ns := r.Namespace
			if ns == "" {
				ns = namespace
			}
			if r.Name == pol.Name && ns == pol.Namespace {
				return true
			}
		}
		return false
	}
	vs := vsEx.VirtualServer
	var out []string
	used := refers(vs.Spec.Policies, vs.Namespace)
	for _, r := range vs.Spec.Routes {
		used = used || refers(r.Policies, vs.Namespace)
	}
	if used {
		out = append(out, NewUpstreamNamerForVirtualServer(vs).GetNameForUpstream(name))
	}
	for _, vsr := range vsEx.VirtualServerRoutes {
		for _, r := range vsr.Spec.Subroutes {
			if refers(r.Policies, vsr.Namespace) {
				out = append(out, NewUpstreamNamerForVirtualServerRoute(vs, vsr).GetNameForUpstream(name))
				break
			}
		}
	}
	return out
}

// getExternalAuthPort returns the port for the external auth service.
// It checks AuthServicePorts first, then falls back to 443 (SSL) or 80.
func getExternalAuthPort(exAuth *conf_v1.ExternalAuth) int {
	if len(exAuth.AuthServicePorts) > 0 && exAuth.AuthServicePorts[0] > 0 {
		return exAuth.AuthServicePorts[0]
	}
	if exAuth.SSLEnabled {
		return 443
	}
	return 80
}

// UpdateEndpointsForTransportServers updates endpoints in NGINX configuration for the TransportServer resources.
func (cnf *Configurator) UpdateEndpointsForTransportServers(transportServerExes []*TransportServerEx) error {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	reloadPlus := false

	for _, tsEx := range transportServerExes {
		if cnf.dynamicMode() {
			cnf.stageTransportServer(tsEx)
		} else {
			// Ignore warnings here as no new warnings should appear when updating Endpoints for TransportServers
			_, _, err := cnf.addOrUpdateTransportServer(tsEx)
			if err != nil {
				return fmt.Errorf("error adding or updating TransportServer %v/%v: %w", tsEx.TransportServer.Namespace, tsEx.TransportServer.Name, err)
			}
		}
		if cnf.isUpstreamAPI {
			err := cnf.updatePlusEndpointsForTransportServer(tsEx)
			if err != nil {
				nl.Warnf(l, "Couldn't update the endpoints via the API: %v; reloading configuration instead", err)
				reloadPlus = true
			}
		}
	}

	if cnf.isUpstreamAPI && !reloadPlus {
		nl.Debug(l, "No need to reload nginx")
		return nil
	}
	if err := cnf.Reload(nginx.ReloadForEndpointsUpdate); err != nil {
		return fmt.Errorf("error reloading NGINX when updating endpoints: %w", err)
	}
	return nil
}

func (cnf *Configurator) updatePlusEndpointsForTransportServer(transportServerEx *TransportServerEx) error {
	upstreamNamer := newUpstreamNamerForTransportServer(transportServerEx.TransportServer)

	for _, u := range transportServerEx.TransportServer.Spec.Upstreams {
		name := upstreamNamer.GetNameForUpstream(u.Name)

		// subselector is not supported yet in TransportServer upstreams. That's why we pass "nil" here
		svcNamespace, svcName := ParseServiceReference(u.Service, transportServerEx.TransportServer.Namespace)
		endpointsKey := GenerateEndpointsKey(svcNamespace, svcName, nil, uint16(u.Port))
		endpoints := transportServerEx.Endpoints[endpointsKey]

		err := cnf.updateStreamServersInPlus(name, endpoints, streamServerConfig(u))
		if err != nil {
			return fmt.Errorf("couldn't update the endpoints for %v: %w", u.Name, err)
		}
	}

	return nil
}

func (cnf *Configurator) updatePlusEndpoints(ingEx *IngressEx, parentIngress *networking.Ingress) error {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	ingCfg := parseAnnotations(ingEx, cnf.CfgParams, cnf.isPlus, cnf.staticCfgParams.MainAppProtectLoadModule, cnf.staticCfgParams.MainAppProtectDosLoadModule, cnf.staticCfgParams.IsDirectiveAutoadjustEnabled)

	cfg := nginx.ServerConfig{
		MaxFails:    ingCfg.MaxFails,
		MaxConns:    ingCfg.MaxConns,
		FailTimeout: ingCfg.FailTimeout,
		SlowStart:   ingCfg.SlowStart,
	}

	if ingEx.Ingress.Spec.DefaultBackend != nil {
		endps, exists := ingEx.Endpoints[ingEx.Ingress.Spec.DefaultBackend.Service.Name+GetBackendPortAsString(ingEx.Ingress.Spec.DefaultBackend.Service.Port)]
		if exists {
			if _, isExternalName := ingEx.ExternalNameSvcs[ingEx.Ingress.Spec.DefaultBackend.Service.Name]; isExternalName {
				nl.Debugf(l, "Service %s is Type ExternalName, skipping NGINX Plus endpoints update via API", ingEx.Ingress.Spec.DefaultBackend.Service.Name)
			} else {
				name := getNameForUpstream(ingEx.Ingress, emptyHostName, ingEx.Ingress.Spec.DefaultBackend)
				err := cnf.updateServersInPlus(name, endps, cfg)
				if err != nil {
					return fmt.Errorf("couldn't update the endpoints for %v: %w", name, err)
				}
			}
		}
	}

	// the default-backend of ingress-nginx
	if backend := ingEx.IngressNginxDefaultBackend; backend != nil {
		if endps, exists := ingEx.Endpoints[backend.Service.Name+GetBackendPortAsString(backend.Service.Port)]; exists {
			name := getNameForUpstream(ingEx.Ingress, "inx-default-backend", backend)
			if err := cnf.updateServersInPlus(name, endps, cfg); err != nil {
				return fmt.Errorf("couldn't update the endpoints for %v: %w", name, err)
			}
		}
	}

	if err := cnf.updatePlusRuleEndpoints(ingEx, cfg); err != nil {
		return err
	}

	// the Ingresses merged into the servers of its hosts have their own upstreams there
	for _, mergedEx := range ingEx.Merged {
		mergedCfg := parseAnnotations(mergedEx, cnf.CfgParams, cnf.isPlus, cnf.staticCfgParams.MainAppProtectLoadModule, cnf.staticCfgParams.MainAppProtectDosLoadModule, cnf.staticCfgParams.IsDirectiveAutoadjustEnabled)
		err := cnf.updatePlusRuleEndpoints(mergedEx, nginx.ServerConfig{
			MaxFails:    mergedCfg.MaxFails,
			MaxConns:    mergedCfg.MaxConns,
			FailTimeout: mergedCfg.FailTimeout,
			SlowStart:   mergedCfg.SlowStart,
		})
		if err != nil {
			return err
		}
	}

	if err := cnf.updatePlusCanaryEndpoints(ingEx, parentIngress, cfg); err != nil {
		return err
	}

	return cnf.updatePlusExternalAuthEndpoints(ingEx.Policies, ingEx.Endpoints, parentIngress, cfg)
}

// updatePlusRuleEndpoints updates the upstreams of the paths of the valid hosts of the Ingress via the API.
func (cnf *Configurator) updatePlusRuleEndpoints(ingEx *IngressEx, cfg nginx.ServerConfig) error {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	for _, rule := range ingEx.Ingress.Spec.Rules {
		// the config has no upstreams for a host another resource has, or another Ingress merged it into its server
		if rule.IngressRuleValue.HTTP == nil || (ingEx.ValidHosts != nil && !ingEx.ValidHosts[rule.Host]) {
			continue
		}

		for _, path := range rule.HTTP.Paths {
			path := path // address gosec G601
			endps, exists := ingEx.Endpoints[path.Backend.Service.Name+GetBackendPortAsString(path.Backend.Service.Port)]
			if exists {
				if _, isExternalName := ingEx.ExternalNameSvcs[path.Backend.Service.Name]; isExternalName {
					nl.Debugf(l, "Service %s is Type ExternalName, skipping NGINX Plus endpoints update via API", path.Backend.Service.Name)
					continue
				}

				name := getNameForUpstream(ingEx.Ingress, rule.Host, &path.Backend)
				err := cnf.updateServersInPlus(name, endps, cfg)
				if err != nil {
					return fmt.Errorf("couldn't update the endpoints for %v: %w", name, err)
				}
			}
		}
	}
	return nil
}

// updatePlusCanaryEndpoints updates the upstreams of the canaries of the Ingress
// via the API. They come from the canary Ingresses, not from the Ingress spec,
// and only those its config has are updated.
func (cnf *Configurator) updatePlusCanaryEndpoints(ingEx *IngressEx, parentIngress *networking.Ingress, cfg nginx.ServerConfig) error {
	rendered := make(map[string]bool)
	for _, canary := range cnf.canaries[objectMetaToFileName(&parentIngress.ObjectMeta)] {
		rendered[canary.Canary] = true
	}

	for _, canaryEx := range ingEx.Canaries {
		for _, hb := range canaryUpstreamBackends(ingEx, canaryEx) {
			if hb.backend.Service == nil {
				continue
			}
			name := getNameForUpstream(canaryEx.Ingress, hb.host, hb.backend)
			if !rendered[name] {
				continue
			}
			delete(rendered, name)

			endps, exists := canaryEx.Endpoints[hb.backend.Service.Name+GetBackendPortAsString(hb.backend.Service.Port)]
			if !exists || canaryEx.ExternalNameSvcs[hb.backend.Service.Name] {
				continue
			}
			if err := cnf.updateServersInPlus(name, endps, cfg); err != nil {
				return fmt.Errorf("couldn't update the endpoints for %v: %w", name, err)
			}
		}
	}

	return nil
}

// canaryHostBackend is a host and a backend a canary upstream of a config is named by.
type canaryHostBackend struct {
	host    string
	backend *networking.IngressBackend
}

// canaryUpstreamBackends returns the hosts and backends the canary upstreams of the canary in the config of the
// Ingress may be named by: the canary's own rules, or for a canary of upstreams (CanaryOf) its backends on the
// hosts of the Ingress, which name its upstreams there.
func canaryUpstreamBackends(ingEx *IngressEx, canaryEx *IngressEx) []canaryHostBackend {
	var result []canaryHostBackend
	if canaryEx.CanaryOf != nil {
		for _, rule := range ingEx.Ingress.Spec.Rules {
			for upstream := range canaryEx.CanaryOf {
				backend := canaryEx.CanaryOf[upstream]
				result = append(result, canaryHostBackend{host: rule.Host, backend: &backend})
			}
		}
		return result
	}
	for _, rule := range canaryEx.Ingress.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for i := range rule.HTTP.Paths {
			result = append(result, canaryHostBackend{host: rule.Host, backend: &rule.HTTP.Paths[i].Backend})
		}
	}
	return result
}

// updatePlusExternalAuthEndpoints updates external auth upstreams via the Plus API.
// These are generated from policies referenced by the Ingress
// and are not part of the Ingress spec, so they need separate handling.
func (cnf *Configurator) updatePlusExternalAuthEndpoints(policies map[string]*conf_v1.Policy, endpoints map[string][]string, parentIngress *networking.Ingress, cfg nginx.ServerConfig) error {
	for _, pol := range policies {
		if pol.Spec.ExternalAuth == nil || pol.Spec.ExternalAuth.AuthServiceName == "" {
			continue
		}
		exAuth := pol.Spec.ExternalAuth
		upstreamName := fmt.Sprintf("ing_%s_%s_exauth_%s_%s", parentIngress.Namespace, parentIngress.Name, pol.Namespace, pol.Name)
		port := getExternalAuthPort(exAuth)
		ns, svcName := ParseServiceReference(exAuth.AuthServiceName, pol.Namespace)
		endpointKey := fmt.Sprintf("%s/%s:%d", ns, svcName, port)
		if endps, exists := endpoints[endpointKey]; exists {
			err := cnf.updateServersInPlus(upstreamName, endps, cfg)
			if err != nil {
				return fmt.Errorf("couldn't update the endpoints for external auth upstream %v: %w", upstreamName, err)
			}
		}
	}
	return nil
}

// EnableReloads enables NGINX reloads meaning that configuration changes will be followed by a reload.
func (cnf *Configurator) EnableReloads() {
	cnf.isReloadsEnabled = true
}

// DisableReloads disables NGINX reloads meaning that configuration changes will not be followed by a reload.
func (cnf *Configurator) DisableReloads() {
	cnf.isReloadsEnabled = false
	if !cnf.dynamicMode() {
		cnf.reloadAsked = false
	}
}

// ReloadAsked reports whether a reload was asked for since reloads were disabled.
func (cnf *Configurator) ReloadAsked() bool {
	return cnf.reloadAsked
}

// EnableBatchMode enables batch mode on the underlying ConfigRollbackManager, if config safety is active.
// In batch mode, CreateConfig/CreateStreamConfig writes are deferred from per-file nginx -t validation.
// This is used during startup to avoid O(N²) validation cost and all configs are written directly,
// then validated with a single nginx -t in CompleteBatch().
func (cnf *Configurator) EnableBatchMode() {
	if rm, ok := cnf.nginxManager.(*nginx.ConfigRollbackManager); ok {
		rm.EnableBatchMode()
	}
}

// IsConfigSafetyEnabled reports whether the controller is running with -enable-config-safety.
// It is true iff the underlying nginx manager is a *ConfigRollbackManager
func (cnf *Configurator) IsConfigSafetyEnabled() bool {
	_, ok := cnf.nginxManager.(*nginx.ConfigRollbackManager)
	return ok
}

// CompleteBatch finalizes batch mode on the underlying ConfigRollbackManager.
// Runs a single nginx -t against the full config tree. On failure, isolates and excludes
// bad configs. Returns the list of excluded resources (nil if all valid) and an error
// if isolation completely failed (caller should fall back to per-file replay).
func (cnf *Configurator) CompleteBatch() ([]nginx.BatchExclusion, error) {
	rm, ok := cnf.nginxManager.(*nginx.ConfigRollbackManager)
	if !ok {
		return nil, nil
	}
	if err := rm.CompleteBatch(); err != nil {
		return rm.BatchExclusions(), err
	}
	return rm.BatchExclusions(), nil
}

// GetBatchExclusions returns the list of resources excluded during the most recent
// CompleteBatch() call. Used by the controller to log excluded resources after pod ready.
// Returns nil if no batch exclusions occurred or if config safety is not active.
func (cnf *Configurator) GetBatchExclusions() []nginx.BatchExclusion {
	rm, ok := cnf.nginxManager.(*nginx.ConfigRollbackManager)
	if !ok {
		return nil
	}
	return rm.BatchExclusions()
}

// EffectiveBatchExclusionCount returns the number of unique resources whose configs
// were effectively excluded during the most recent completeBatchAndIsolate call.
// When multiple resources share a single config file (empty-host Ingresses share
// _default-server.conf), a single BatchExclusion covers N resources; this count
// reflects the resource-level total rather than the file-level total, and is what
// the controller's readiness gate uses to detect the "all resources broken" case.
func (cnf *Configurator) EffectiveBatchExclusionCount() int {
	return cnf.effectiveBatchExclusionCount
}

// Reload reloads nginx if reloads is enabled - with -dynamic-upstreams at
// most once per reload interval: a reload asked for sooner waits, see
// ReloadIfAsked.
func (cnf *Configurator) Reload(isEndpointsUpdate bool) error {
	if !cnf.isReloadsEnabled || cnf.reloadWaits() {
		cnf.reloadAsked = true
		return nil
	}
	return cnf.reload(isEndpointsUpdate)
}

func (cnf *Configurator) reload(isEndpointsUpdate bool) error {
	cnf.reloadAsked = false

	// the configs whose servers went only to the module are written with them, so that the reload keeps them
	cnf.writeStaged()
	started := time.Now()
	if err := cnf.nginxManager.Reload(isEndpointsUpdate); err != nil {
		return err
	}
	cnf.reloaded(started)

	// the configuration now has the canary upstreams of the rules and no longer those it lost
	if err := cnf.syncCanaries(); err != nil {
		l := nl.LoggerFromContext(cnf.CfgParams.Context)
		nl.Errorf(l, "Error updating the canaries: %v", err)
	}
	// and the hosts the certificates are for
	if err := cnf.syncCertificates(); err != nil {
		l := nl.LoggerFromContext(cnf.CfgParams.Context)
		nl.Errorf(l, "Error updating the certificates: %v", err)
	}
	// and the balancers of the upstreams it has now
	if err := cnf.syncBalancers(); err != nil {
		l := nl.LoggerFromContext(cnf.CfgParams.Context)
		nl.Errorf(l, "Error updating the balancers: %v", err)
	}
	return nil
}

func (cnf *Configurator) updateServersInPlus(upstream string, servers []string, config nginx.ServerConfig) error {
	if !cnf.isReloadsEnabled && !cnf.pushWhileReloadsWait() {
		return nil
	}
	if cnf.dynamicMode() {
		return cnf.updateServers(upstream, servers, config)
	}

	return cnf.nginxManager.UpdateServersInPlus(upstream, servers, config)
}

func (cnf *Configurator) updateStreamServersInPlus(upstream string, servers []string, config nginx.ServerConfig) error {
	if !cnf.isReloadsEnabled && !cnf.pushWhileReloadsWait() {
		return nil
	}
	if cnf.dynamicMode() {
		return cnf.updateStreamServers(upstream, servers, config)
	}

	return cnf.nginxManager.UpdateStreamServersInPlus(upstream, servers, config)
}

// resourceUpdateTask captures one addOrUpdate<Kind> call together with the metadata
// needed to attribute a failure back to a Kubernetes resource. It exists so that
// UpdateConfig can process Ingresses, MergeableIngresses, VirtualServers, and
// TransportServers through a single loop with unified error handling.
type resourceUpdateTask struct {
	kind      string
	namespace string
	name      string
	update    func() (Warnings, []WeightUpdate, error)
}

// buildResourceUpdateTasks flattens ExtendedResources into a single ordered list of
// update tasks. Order matches the historical per-type loop order
// (Ingress → MergeableIngress → VirtualServer → TransportServer) so shared-input
// early-exit behavior is preserved: two consecutive failures still terminate
// processing at the same point.
func (cnf *Configurator) buildResourceUpdateTasks(resources ExtendedResources) []resourceUpdateTask {
	tasks := make([]resourceUpdateTask, 0,
		len(resources.IngressExes)+len(resources.MergeableIngresses)+
			len(resources.VirtualServerExes)+len(resources.TransportServerExes))
	for _, ingEx := range resources.IngressExes {
		tasks = append(tasks, resourceUpdateTask{
			kind:      "Ingress",
			namespace: ingEx.Ingress.Namespace,
			name:      ingEx.Ingress.Name,
			update: func() (Warnings, []WeightUpdate, error) {
				_, warnings, err := cnf.addOrUpdateIngress(ingEx)
				return warnings, nil, err
			},
		})
	}
	for _, mergeableIng := range resources.MergeableIngresses {
		tasks = append(tasks, resourceUpdateTask{
			kind:      "Ingress",
			namespace: mergeableIng.Master.Ingress.Namespace,
			name:      mergeableIng.Master.Ingress.Name,
			update: func() (Warnings, []WeightUpdate, error) {
				_, warnings, err := cnf.addOrUpdateMergeableIngress(mergeableIng)
				return warnings, nil, err
			},
		})
	}
	for _, vsEx := range resources.VirtualServerExes {
		tasks = append(tasks, resourceUpdateTask{
			kind:      "VirtualServer",
			namespace: vsEx.VirtualServer.Namespace,
			name:      vsEx.VirtualServer.Name,
			update: func() (Warnings, []WeightUpdate, error) {
				_, warnings, weightUpdates, err := cnf.addOrUpdateVirtualServer(vsEx)
				return warnings, weightUpdates, err
			},
		})
	}
	for _, tsEx := range resources.TransportServerExes {
		tasks = append(tasks, resourceUpdateTask{
			kind:      "TransportServer",
			namespace: tsEx.TransportServer.Namespace,
			name:      tsEx.TransportServer.Name,
			update: func() (Warnings, []WeightUpdate, error) {
				_, warnings, err := cnf.addOrUpdateTransportServer(tsEx)
				return warnings, nil, err
			},
		})
	}
	return tasks
}

// resourceUpdateResult carries the outcome of applyResourceUpdates for a single pass.
type resourceUpdateResult struct {
	warnings            Warnings
	weightUpdates       []WeightUpdate
	sharedInputFailure  bool // set true when the early-exit threshold was reached
	consecutiveFailures int  // trailing consecutive per-resource failures at loop exit (equals the threshold when sharedInputFailure is true; otherwise the count of trailing failures, or 0 if the last task succeeded)
	processedCount      int  // number of tasks fully attempted (excludes tasks skipped after early exit)
}

// applyResourceUpdates runs each task and records per-resource errors.
//
// When enableEarlyExit is true (post-startup, non-batch mode), two consecutive per-resource
// failures set sharedInputFailure=true and the remaining tasks are skipped. This detects
// bad shared inputs (e.g. a broken server-snippets in the ConfigMap) without churning
// through every resource. The threshold is fixed at sharedInputFailureThreshold and is
// not tunable — see the constant's comment for the rationale.
//
// When isRollbackManager is false, per-resource errors are propagated to the caller
// immediately (fail-fast); this preserves the legacy non-safety semantics.
func (cnf *Configurator) applyResourceUpdates(
	tasks []resourceUpdateTask,
	isRollbackManager, enableEarlyExit bool,
	resourceErrors ResourceErrors,
) (resourceUpdateResult, error) {
	res := resourceUpdateResult{warnings: newWarnings()}
	for _, task := range tasks {
		if res.sharedInputFailure {
			break
		}
		res.processedCount++
		warnings, weightUpdates, err := task.update()
		if err != nil {
			if !isRollbackManager {
				return res, err
			}
			key := MakeResourceErrorKey(task.kind, task.namespace, task.name)
			// This resource failed validation during a shared-input regeneration pass
			// (ConfigMap or special Secret change). The nginx error follows; check the
			// events on the triggering ConfigMap/Secret for the root-cause change.
			resourceErrors[key] = fmt.Errorf("resource config rejected during shared-input regeneration "+
				"(check the ConfigMap/Secret events for the triggering change): %w", err)
			if enableEarlyExit {
				res.consecutiveFailures++
				if res.consecutiveFailures >= sharedInputFailureThreshold {
					res.sharedInputFailure = true
				}
			}
			continue
		}
		res.consecutiveFailures = 0
		res.warnings.Add(warnings)
		res.weightUpdates = append(res.weightUpdates, weightUpdates...)
	}
	return res, nil
}

// buildFilenameToErrorKey maps each generated config filename to the kind-qualified
// error keys (Kind/namespace/name) of resources whose content ended up in that file.
// Most files have a single owner. Exceptions are files shared by multiple resources:
// empty-host Ingresses and empty-host mergeable-Ingress masters both write to the
// shared _default-server.conf, so a single filename can map to multiple owner keys.
// The controller uses each owner key to emit a per-resource error event, and the
// number of unique owners feeds the readiness gate's "all resources broken" check.
func buildFilenameToErrorKey(resources ExtendedResources) map[string][]string {
	m := make(map[string][]string,
		len(resources.IngressExes)+len(resources.MergeableIngresses)+
			len(resources.VirtualServerExes)+len(resources.TransportServerExes))
	for _, ingEx := range resources.IngressExes {
		errKey := MakeResourceErrorKey("Ingress", ingEx.Ingress.Namespace, ingEx.Ingress.Name)
		// Empty-host Ingresses route their generated content into the shared
		// _default-server.conf via addOrUpdateIngress; the per-Ingress filename
		// ("namespace-name") is never written, so it will never appear in
		// batchFiles and adding it to the map would be dead attribution.
		if ingEx.ValidHosts[emptyHostName] {
			m[DefaultServerConfigName] = append(m[DefaultServerConfigName], errKey)
			continue
		}
		fileName := objectMetaToFileName(&ingEx.Ingress.ObjectMeta)
		m[fileName] = append(m[fileName], errKey)
	}
	for _, mergeableIng := range resources.MergeableIngresses {
		errKey := MakeResourceErrorKey("Ingress",
			mergeableIng.Master.Ingress.Namespace, mergeableIng.Master.Ingress.Name)
		// Same as above: an empty-host mergeable master writes only into
		// _default-server.conf, so skip the per-master filename entry.
		if mergeableIng.Master.ValidHosts[emptyHostName] {
			m[DefaultServerConfigName] = append(m[DefaultServerConfigName], errKey)
			continue
		}
		fileName := objectMetaToFileName(&mergeableIng.Master.Ingress.ObjectMeta)
		m[fileName] = append(m[fileName], errKey)
	}
	for _, vsEx := range resources.VirtualServerExes {
		fileName := getFileNameForVirtualServer(vsEx.VirtualServer)
		errKey := MakeResourceErrorKey("VirtualServer", vsEx.VirtualServer.Namespace, vsEx.VirtualServer.Name)
		m[fileName] = append(m[fileName], errKey)
	}
	for _, tsEx := range resources.TransportServerExes {
		fileName := getFileNameForTransportServer(tsEx.TransportServer)
		errKey := MakeResourceErrorKey("TransportServer", tsEx.TransportServer.Namespace, tsEx.TransportServer.Name)
		m[fileName] = append(m[fileName], errKey)
	}
	return m
}

// completeBatchAndIsolate finalizes the startup batch: runs the single full-tree
// nginx -t via CompleteBatch, attributes any excluded configs back to their
// Kubernetes resources (recorded into resourceErrors), and if CompleteBatch
// cannot isolate the bad file at all, clears the caller's accumulators and
// re-runs a per-file replay so we get a definitive per resource picture.
//
// resourceErrors is mutated in place on the happy path. On the nuclear-fallback
// path resourceErrors is cleared (via the built-in clear) before being repopulated
// by the replay, so its identity remains the caller's map. The returned warnings
// and weightUpdates are either the passed-in values (happy path) or fresh values
// from the replay (nuclear-fallback path), the caller must re-assign both.
//
// Callers must only invoke this when the underlying nginx manager is a
// ConfigRollbackManager that is currently in batch mode.
func (cnf *Configurator) completeBatchAndIsolate(
	tasks []resourceUpdateTask,
	resources ExtendedResources,
	allWarnings Warnings,
	allWeightUpdates []WeightUpdate,
	resourceErrors ResourceErrors,
	isRollbackManager bool,
) (Warnings, []WeightUpdate, error) {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	filenameToErrorKey := buildFilenameToErrorKey(resources)
	cnf.effectiveBatchExclusionCount = 0

	exclusions, batchErr := cnf.CompleteBatch()

	// Record excluded resources as errors using the kind-qualified key format
	// that matches r.GetKeyWithKind() in the controller's event emission loop.
	// A single file exclusion can correspond to multiple resource-level errors when
	// resources share a config file (empty-host Ingresses → _default-server.conf),
	// so we iterate every owner and also track the unique set for the effective
	// exclusion count consumed by the controller's readiness gate.
	uniqueExcluded := make(map[string]struct{})
	for _, excl := range exclusions {
		errorKeys, mapped := filenameToErrorKey[excl.ResourceName]
		if !mapped {
			// Should not happen: every file written via batchWriteConfig has a
			// corresponding entry in filenameToErrorKey. If it does happen, log
			// loudly and attribute to the raw filename so the operator still gets
			// a starting point to investigate.
			nl.Warnf(l, "Config safety: batch exclusion for %q has no resource mapping — event attribution may be incorrect", excl.ResourceName)
			errorKeys = []string{excl.ResourceName}
		}
		for _, errorKey := range errorKeys {
			resourceErrors[errorKey] = fmt.Errorf("config safety: excluded due to invalid configuration: %w", excl.Error)
			uniqueExcluded[errorKey] = struct{}{}
		}
	}
	cnf.effectiveBatchExclusionCount = len(uniqueExcluded)

	if batchErr == nil {
		return allWarnings, allWeightUpdates, nil
	}

	// Nuclear fallback: CompleteBatch could not isolate the bad file(s). All batch
	// files have been deleted, leaving only the pre-batch bootstrap on disk. Before
	// doing the O(N) per-file replay, probe the on-disk state once. If nginx -t
	// still fails, the bad content lives in a file we never tracked (bootstrap
	// nginx.conf from a bad ConfigMap render, bad WAF policy file, etc.) and
	// replaying every resource against that broken foundation would just fail N
	// times and mis-attribute each failure to the resource instead of the real
	// root cause. Skip the replay and mark every resource with a clear
	// unrecoverable-main-config error.
	if rm, ok := cnf.nginxManager.(*nginx.ConfigRollbackManager); ok {
		if probeErr := rm.TestConfig(); probeErr != nil {
			nl.Errorf(l, "NGINX config still invalid after batch cleanup; skipping per-file replay "+
				"(pre-batch bootstrap alone does not pass nginx -t). Correct the triggering "+
				"ConfigMap/Secret to recover: %v", probeErr)
			clear(resourceErrors)
			uniqueUnrecoverable := make(map[string]struct{}, len(tasks))
			for _, task := range tasks {
				key := MakeResourceErrorKey(task.kind, task.namespace, task.name)
				resourceErrors[key] = fmt.Errorf("NGINX main configuration is invalid; "+
					"no resources can be applied until the triggering ConfigMap/Secret is corrected: %w",
					probeErr)
				uniqueUnrecoverable[key] = struct{}{}
			}
			cnf.effectiveBatchExclusionCount = len(uniqueUnrecoverable)
			return allWarnings, allWeightUpdates, fmt.Errorf("batch validation failed and "+
				"NGINX config remains invalid after batch cleanup: %w", probeErr)
		}
	}

	// Nginx -t passes against the pre-batch bootstrap on disk, so it is a viable
	// foundation for per-file replay. Replay each write through createConfigWithRollback
	// (real per-file nginx -t + rollback). Early exit is disabled — we want a
	// definitive per-resource picture rather than the shared-input heuristic.
	nl.Warnf(l, "Batch validation failed completely (%v), falling back to per-file validation", batchErr)
	clear(resourceErrors)
	replayWarnings := newWarnings()
	var replayWeightUpdates []WeightUpdate
	replayRes, replayErr := cnf.applyResourceUpdates(tasks, isRollbackManager, false, resourceErrors)
	if replayErr != nil {
		return replayWarnings, replayWeightUpdates, replayErr
	}
	replayWarnings.Add(replayRes.warnings)
	replayWeightUpdates = append(replayWeightUpdates, replayRes.weightUpdates...)
	return replayWarnings, replayWeightUpdates, nil
}

// UpdateConfig updates NGINX configuration parameters.
//
//gocyclo:ignore
func (cnf *Configurator) UpdateConfig(resources ExtendedResources) (Warnings, ResourceErrors, error) {
	allWarnings := newWarnings()
	allWeightUpdates := []WeightUpdate{}
	resourceErrors := make(ResourceErrors)
	rm, isRollbackManager := cnf.nginxManager.(*nginx.ConfigRollbackManager)

	// Post-startup shared-input early exit: when not in batch mode (post-startup),
	// track consecutive per-resource failures. If 2+ consecutive resources fail,
	// it indicates a shared input (e.g., bad server-snippets from ConfigMap) is the
	// root cause. Break early to avoid O(N²) nginx -t + O(N) reloads.
	// The state is owned by applyResourceUpdates; the returned resourceUpdateResult
	// carries sharedInputFailure and processedCount back to this function.
	isPostStartupRollback := isRollbackManager && !rm.IsBatchMode()

	// with -dynamic-upstreams the reload at the end is made only when a config changed
	cnf.configsChanged = false

	if cnf.CfgParams.MainServerSSLDHParamFileContent != nil {
		if content := *cnf.CfgParams.MainServerSSLDHParamFileContent; content != cnf.dhParam {
			cnf.dhParam = content
			cnf.configsChanged = true
		}
		fileName, err := cnf.nginxManager.CreateDHParam(*cnf.CfgParams.MainServerSSLDHParamFileContent)
		if err != nil {
			return allWarnings, nil, fmt.Errorf("error when updating dhparams: %w", err)
		}
		cnf.CfgParams.MainServerSSLDHParam = fileName
	}

	// Apply custom main-template defined in ConfigMap obj
	if cnf.CfgParams.MainTemplate != nil {
		err := cnf.templateExecutor.UpdateMainTemplate(cnf.CfgParams.MainTemplate)
		if err != nil {
			return allWarnings, nil, fmt.Errorf("error when parsing the main template: %w", err)
		}
	} else {
		// Reverse to default Main template parsed at NIC startup.
		cnf.templateExecutor.UseOriginalMainTemplate()
	}

	if cnf.CfgParams.IngressTemplate != nil {
		err := cnf.templateExecutor.UpdateIngressTemplate(cnf.CfgParams.IngressTemplate)
		if err != nil {
			return allWarnings, nil, fmt.Errorf("error when parsing the ingress template: %w", err)
		}
	} else {
		// Reverse to default Ingress template parsed at NIC startup.
		cnf.templateExecutor.UseOriginalIngressTemplate()
	}

	if cnf.CfgParams.VirtualServerTemplate != nil {
		err := cnf.templateExecutorV2.UpdateVirtualServerTemplate(cnf.CfgParams.VirtualServerTemplate)
		if err != nil {
			return allWarnings, nil, fmt.Errorf("error when parsing the VirtualServer template: %w", err)
		}
	} else {
		// Reverse to default TransportServer template parsed at NIC startup.
		cnf.templateExecutorV2.UseOriginalVStemplate()
	}

	if cnf.CfgParams.TransportServerTemplate != nil {
		err := cnf.templateExecutorV2.UpdateTransportServerTemplate(cnf.CfgParams.TransportServerTemplate)
		if err != nil {
			return allWarnings, nil, fmt.Errorf("error when parsing the TransportServer template: %w", err)
		}
	} else {
		// Reverse to default TransportServer template parsed at NIC startup.
		cnf.templateExecutorV2.UseOriginalTStemplate()
	}

	mainCfg := GenerateNginxMainConfig(cnf.staticCfgParams, cnf.CfgParams, cnf.MgmtCfgParams)
	mainCfgContent, err := cnf.templateExecutor.ExecuteMainConfigTemplate(mainCfg)
	if err != nil {
		return allWarnings, nil, fmt.Errorf("error when writing main Config")
	}
	mainChanged, err := cnf.nginxManager.CreateMainConfig(mainCfgContent)
	if mainChanged {
		cnf.configsChanged = true
	}
	if err != nil {
		if isRollbackManager {
			l := nl.LoggerFromContext(cnf.CfgParams.Context)

			// Post-startup only: createConfigWithRollback has already restored the previous-good
			// main config on disk, so NGINX is unaffected. We continue to the resource loops
			// because each per-file createConfigWithRollback will still validate against the
			// rolled-back main config and reject any resource that depends on the rejected changes.
			nl.Warnf(l, "Main config validation failed and was rolled back to previous-good; "+
				"NGINX continues serving previous configuration. Resource updates will use rolled-back main config. "+
				"Root cause: %v", err)
		} else {
			return allWarnings, nil, err
		}
	}

	if err := cnf.syncDefaultServerConfig(); err != nil {
		return allWarnings, nil, fmt.Errorf("error syncing default server config: %w", err)
	}

	// Process every resource type through a single unified loop. Ordering
	// (Ingress → MergeableIngress → VirtualServer → TransportServer) is preserved
	// by buildResourceUpdateTasks so shared-input early-exit behavior is unchanged.
	tasks := cnf.buildResourceUpdateTasks(resources)
	updateRes, updateErr := cnf.applyResourceUpdates(tasks, isRollbackManager, isPostStartupRollback, resourceErrors)
	if updateErr != nil {
		return allWarnings, nil, updateErr
	}
	allWarnings.Add(updateRes.warnings)
	allWeightUpdates = append(allWeightUpdates, updateRes.weightUpdates...)

	// If running under ConfigRollbackManager in batch mode, validate the entire
	// config tree with a single nginx -t and, on failure, isolate bad configs
	// or fall back to per-file replay. See completeBatchAndIsolate for details.
	if rm, ok := cnf.nginxManager.(*nginx.ConfigRollbackManager); ok && rm.IsBatchMode() {
		var err error
		allWarnings, allWeightUpdates, err = cnf.completeBatchAndIsolate(
			tasks, resources, allWarnings, allWeightUpdates, resourceErrors, isRollbackManager,
		)
		if err != nil {
			return allWarnings, resourceErrors, err
		}
	}

	// Shared-input early exit: if consecutive failures detected a shared-input problem
	// (e.g., bad server-snippets from ConfigMap), skip the final reload. All failed files
	// were already rolled back to previous-good by createConfigWithRollback, and NGINX was
	// reloaded to the previous-good state during rollback. No further action needed.
	if updateRes.sharedInputFailure {
		l := nl.LoggerFromContext(cnf.CfgParams.Context)
		nl.Warnf(l, "Shared-input failure detected: %d consecutive resource configs failed validation. "+
			"Likely cause is a bad ConfigMap snippet (e.g., server-snippets). "+
			"Skipping remaining %d resource(s) and final reload — NGINX continues serving previous-good config.",
			updateRes.consecutiveFailures, len(tasks)-updateRes.processedCount)
		return allWarnings, resourceErrors, nil
	}

	if cnf.dynamicMode() && !cnf.configsChanged && !cnf.lastReload.IsZero() && !isRollbackManager {
		// no config changed - a ConfigMap key that renders the same: what changed goes to the wxingress module, without a reload
		if err := cnf.applyDynamically(resources.IngressExes, resources.MergeableIngresses, resources.VirtualServerExes, resources.TransportServerExes); err != nil {
			return allWarnings, resourceErrors, fmt.Errorf("error updating the servers, canaries or certificates: %w", err)
		}
	} else if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		return allWarnings, resourceErrors, fmt.Errorf("NGINX reload failed after shared-input regeneration: %w", err)
	}

	for _, weightUpdate := range allWeightUpdates {
		cnf.nginxManager.UpsertSplitClientsKeyVal(weightUpdate.Zone, weightUpdate.Key, weightUpdate.Value)
	}

	if len(resourceErrors) > 0 {
		return allWarnings, resourceErrors, nil
	}

	return allWarnings, nil, nil
}

// ReloadForBatchUpdates reloads NGINX after a batch event.
func (cnf *Configurator) ReloadForBatchUpdates(batchReloadsEnabled bool) error {
	if !batchReloadsEnabled {
		// what the batch changed in the canaries and certificates was not given to the wxingress module while reloads were off
		var errs []error
		if cnf.canariesHeld {
			errs = append(errs, cnf.syncCanaries())
		}
		if cnf.certificatesHeld {
			errs = append(errs, cnf.syncCertificates())
		}
		if cnf.balancersHeld {
			errs = append(errs, cnf.syncBalancers())
		}
		return errors.Join(errs...)
	}
	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		return fmt.Errorf("error when reloading NGINX after a batch event: %w", err)
	}
	return nil
}

// UpdateVirtualServers updates VirtualServers.
func (cnf *Configurator) UpdateVirtualServers(updatedVSExes []*VirtualServerEx, deletedKeys []string) []error {
	var errList []error
	var allWeightUpdates []WeightUpdate
	for _, vsEx := range updatedVSExes {
		_, _, weightUpdates, err := cnf.addOrUpdateVirtualServer(vsEx)
		if err != nil {
			errList = append(errList, fmt.Errorf("error adding or updating VirtualServer %v/%v: %w", vsEx.VirtualServer.Namespace, vsEx.VirtualServer.Name, err))
		}
		allWeightUpdates = append(allWeightUpdates, weightUpdates...)
	}

	for _, key := range deletedKeys {
		err := cnf.DeleteVirtualServer(key, true)
		if err != nil {
			errList = append(errList, fmt.Errorf("error when removing VirtualServer %v: %w", key, err))
		}
	}

	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		errList = append(errList, fmt.Errorf("error when updating VirtualServer: %w", err))
	}

	for _, weightUpdate := range allWeightUpdates {
		cnf.nginxManager.UpsertSplitClientsKeyVal(weightUpdate.Zone, weightUpdate.Key, weightUpdate.Value)
	}

	return errList
}

// UpdateTransportServers updates TransportServers.
func (cnf *Configurator) UpdateTransportServers(updatedTSExes []*TransportServerEx, deletedKeys []string) []error {
	var errList []error
	for _, tsEx := range updatedTSExes {
		_, _, err := cnf.addOrUpdateTransportServer(tsEx)
		if err != nil {
			errList = append(errList, fmt.Errorf("error adding or updating TransportServer %v/%v: %w", tsEx.TransportServer.Namespace, tsEx.TransportServer.Name, err))
		}
	}

	for _, key := range deletedKeys {
		err := cnf.deleteTransportServer(key)
		if err != nil {
			errList = append(errList, fmt.Errorf("error when removing TransportServer %v: %w", key, err))
		}
	}

	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		errList = append(errList, fmt.Errorf("error when updating TransportServers: %w", err))
	}

	return errList
}

// BatchDeleteVirtualServers takes a list of VirtualServer resource keys, deletes their configuration, and reloads once
func (cnf *Configurator) BatchDeleteVirtualServers(deletedKeys []string) []error {
	var errList []error
	for _, key := range deletedKeys {
		err := cnf.DeleteVirtualServer(key, true)
		if err != nil {
			errList = append(errList, fmt.Errorf("error when removing VirtualServer %v: %w", key, err))
		}
	}

	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		errList = append(errList, fmt.Errorf("error when reloading NGINX for deleted VirtualServers: %w", err))
	}

	return errList
}

// BatchDeleteIngresses takes a list of Ingress resource keys, deletes their configuration, and reloads once
func (cnf *Configurator) BatchDeleteIngresses(deletedKeys []string) []error {
	var errList []error
	for _, key := range deletedKeys {
		err := cnf.DeleteIngress(key, true)
		if err != nil {
			errList = append(errList, fmt.Errorf("error when removing Ingress %v: %w", key, err))
		}
	}

	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		errList = append(errList, fmt.Errorf("error when reloading NGINX for deleted Ingresses: %w", err))
	}

	return errList
}

// setCanaries records the canary rules of the Ingress config.
func (cnf *Configurator) setCanaries(name string, canaries map[string]nginx.Canary) {
	if len(canaries) == 0 {
		delete(cnf.canaries, name)
		return
	}
	cnf.canaries[name] = canaries
}

// canariesChanged tells if the Ingress has canary rules the wxingress module does not have yet.
func (cnf *Configurator) canariesChanged(meta *meta_v1.ObjectMeta) bool {
	for primary, canary := range cnf.canaries[objectMetaToFileName(meta)] {
		if applied, exists := cnf.appliedCanaries[primary]; !exists || applied != canary {
			return true
		}
	}
	return false
}

// syncCanaries sets the canary rules of all Ingresses in the wxingress module
// and deletes those of no Ingress. The module keeps its rules over reloads but
// starts without any, so the first reload sets them all.
func (cnf *Configurator) syncCanaries() error {
	if !cnf.staticCfgParams.UpstreamAPI {
		return nil
	}
	if !cnf.isReloadsEnabled || cnf.reloadAsked {
		// a rule may name a canary upstream the reload at the end of the batch, or the one that waits, brings
		cnf.canariesHeld = true
		return nil
	}
	cnf.canariesHeld = false

	wanted := make(map[string]nginx.Canary)
	for _, canaries := range cnf.canaries {
		maps.Copy(wanted, canaries)
	}

	var errs []error
	defer func() { cnf.canariesFailed = len(errs) > 0 }()
	for _, primary := range slices.Sorted(maps.Keys(cnf.appliedCanaries)) {
		if _, exists := wanted[primary]; exists {
			continue
		}
		if err := cnf.nginxManager.DeleteCanary(primary); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(cnf.appliedCanaries, primary)
	}
	for _, primary := range slices.Sorted(maps.Keys(wanted)) {
		if applied, exists := cnf.appliedCanaries[primary]; exists && applied == wanted[primary] {
			continue
		}
		if err := cnf.nginxManager.SetCanary(primary, wanted[primary]); err != nil {
			errs = append(errs, err)
			continue
		}
		cnf.appliedCanaries[primary] = wanted[primary]
	}
	return errors.Join(errs...)
}

// keyToFileName returns the name of the config file (without .conf) of the Ingress of the key (<namespace>/<name>).
func keyToFileName(key string) string {
	return strings.Replace(key, "/", "_", 1)
}

// objectMetaToFileName returns the name of the config file (without .conf) of the Ingress: <namespace>_<name>, as
// neither a namespace nor a name has an underscore - with a hyphen, namespace a-b and name c had the file of
// namespace a and name b-c, and each would overwrite the other's config.
func objectMetaToFileName(meta *meta_v1.ObjectMeta) string {
	return meta.Namespace + "_" + meta.Name
}

func generateNamespaceNameKey(objectMeta *meta_v1.ObjectMeta) string {
	return fmt.Sprintf("%s/%s", objectMeta.Namespace, objectMeta.Name)
}

func getFileNameForVirtualServer(virtualServer *conf_v1.VirtualServer) string {
	return fmt.Sprintf("vs_%s_%s", virtualServer.Namespace, virtualServer.Name)
}

func getFileNameForOIDCVirtualServer(virtualServer *conf_v1.VirtualServer) string {
	return fmt.Sprintf("oidc_%s_%s", virtualServer.Namespace, virtualServer.Name)
}

func getFileNameForTransportServer(transportServer *conf_v1.TransportServer) string {
	return fmt.Sprintf("ts_%s_%s", transportServer.Namespace, transportServer.Name)
}

func getFileNameForVirtualServerFromKey(key string) string {
	replaced := strings.Replace(key, "/", "_", -1)
	return fmt.Sprintf("vs_%s", replaced)
}

func getFileNameForTransportServerFromKey(key string) string {
	replaced := strings.Replace(key, "/", "_", -1)
	return fmt.Sprintf("ts_%s", replaced)
}

// HasIngress checks if the Ingress resource is present in NGINX configuration.
func (cnf *Configurator) HasIngress(ing *networking.Ingress) bool {
	name := objectMetaToFileName(&ing.ObjectMeta)
	_, exists := cnf.ingresses[name]
	return exists
}

// HasMinion checks if the minion Ingress resource of the master is present in NGINX configuration.
func (cnf *Configurator) HasMinion(master *networking.Ingress, minion *networking.Ingress) bool {
	masterName := objectMetaToFileName(&master.ObjectMeta)

	if _, exists := cnf.minions[masterName]; !exists {
		return false
	}

	return cnf.minions[masterName][objectMetaToFileName(&minion.ObjectMeta)]
}

// IsResolverConfigured checks if a DNS resolver is present in NGINX configuration.
func (cnf *Configurator) IsResolverConfigured() bool {
	return len(cnf.CfgParams.ResolverAddresses) != 0
}

// GetIngressCounts returns the total count of Ingress resources that are handled by the Ingress Controller grouped by their type
func (cnf *Configurator) GetIngressCounts() map[string]int {
	counters := map[string]int{
		"master":  0,
		"regular": 0,
		"minion":  0,
	}

	// cnf.ingresses contains only master and regular Ingress Resources
	for _, ing := range cnf.ingresses {
		if ing.Ingress.Annotations["nginx.org/mergeable-ingress-type"] == "master" {
			counters["master"]++
		} else {
			counters["regular"]++
		}
	}

	for _, minion := range cnf.minions {
		counters["minion"] += len(minion)
	}

	return counters
}

// GetIngressAnnotations returns a list of annotation keys set across all Ingress resources
func (cnf *Configurator) GetIngressAnnotations() []string {
	if cnf == nil || cnf.ingresses == nil {
		return nil
	}

	annotationSet := make(map[string]bool)
	annotationSet = cnf.getMinionIngressAnnotations(annotationSet)
	annotationSet = cnf.getStandardIngressAnnotations(annotationSet)

	var annotations []string
	for key := range annotationSet {
		annotations = append(annotations, key)
	}

	return annotations
}

// getStandardIngressAnnotations returns a map of allowedAnnotations detected in Standard or Master Ingresses
func (cnf *Configurator) getStandardIngressAnnotations(annotationSet map[string]bool) map[string]bool {
	for _, ing := range cnf.ingresses {
		if ing != nil && ing.Ingress != nil && ing.Ingress.Annotations != nil {
			for key := range ing.Ingress.Annotations {
				for _, allowedAnnotationKey := range allowedAnnotationKeys {
					if strings.Contains(key, allowedAnnotationKey) {
						annotationSet[key] = true
					}
				}
			}
		}
	}
	return annotationSet
}

// getMinionIngressAnnotations returns a map of allowedAnnotations detected in Minion Ingresses
func (cnf *Configurator) getMinionIngressAnnotations(annotationSet map[string]bool) map[string]bool {
	for _, ing := range cnf.mergeableIngresses {
		for _, minionIng := range ing.Minions {
			if minionIng != nil && minionIng.Ingress.Annotations != nil {
				for key := range minionIng.Ingress.Annotations {
					for _, allowedAnnotationKey := range allowedAnnotationKeys {
						if strings.Contains(key, allowedAnnotationKey) {
							annotationSet[key] = true
						}
					}
				}
			}
		}
	}
	return annotationSet
}

// GetVirtualServerCounts returns the total count of
// VirtualServer and VirtualServerRoute resources that are handled by the Ingress Controller.
// A hostless VirtualServerRoute that is shared across multiple VirtualServers is counted once.
func (cnf *Configurator) GetVirtualServerCounts() (int, int) {
	vsCount := len(cnf.virtualServers)
	seen := make(map[string]struct{})
	for _, vs := range cnf.virtualServers {
		for _, vsr := range vs.VirtualServerRoutes {
			key := vsr.Namespace + "/" + vsr.Name
			seen[key] = struct{}{}
		}
	}
	return vsCount, len(seen)
}

// GetTransportServerCounts returns the total count of
// TransportServer resources that are handled by the Ingress Controller
func (cnf *Configurator) GetTransportServerCounts() (tsCount int) {
	return len(cnf.transportServers)
}

func (cnf *Configurator) updateApResourcesForMergeableIngresses(mergeableIngs *MergeableIngresses) *AppProtectResources {
	apResources := cnf.updateApResources(mergeableIngs.Master)
	for _, minion := range mergeableIngs.Minions {
		cnf.updateApResources(minion)
	}
	return apResources
}

func (cnf *Configurator) updateApResources(ingEx *IngressEx) *AppProtectResources {
	var apResources AppProtectResources

	// Policy attached WAF resources must exist on disk before template rendering/reload.
	for _, apPol := range ingEx.ApPolRefs {
		policyFileName := appProtectPolicyFileNameFromUnstruct(apPol)
		policyContent := generateApResourceFileContent(apPol)
		cnf.nginxManager.CreateAppProtectResourceFile(policyFileName, policyContent)
	}

	for _, logConf := range ingEx.LogConfRefs {
		logConfFileName := appProtectLogConfFileNameFromUnstruct(logConf)
		logConfContent := generateApResourceFileContent(logConf)
		cnf.nginxManager.CreateAppProtectResourceFile(logConfFileName, logConfContent)
	}

	// annotation attached WAF resources must exist on disk before template rendering/reload.
	if ingEx.AppProtectPolicy != nil {
		policyFileName := appProtectPolicyFileNameFromUnstruct(ingEx.AppProtectPolicy)
		policyContent := generateApResourceFileContent(ingEx.AppProtectPolicy)
		cnf.nginxManager.CreateAppProtectResourceFile(policyFileName, policyContent)
		apResources.AppProtectPolicy = policyFileName
	}

	for _, logConf := range ingEx.AppProtectLogs {
		logConfFileName := appProtectLogConfFileNameFromUnstruct(logConf.LogConf)
		logConfContent := generateApResourceFileContent(logConf.LogConf)
		cnf.nginxManager.CreateAppProtectResourceFile(logConfFileName, logConfContent)
		apResources.AppProtectLogconfs = append(apResources.AppProtectLogconfs, logConfFileName+" "+logConf.Dest)
	}

	return &apResources
}

func (cnf *Configurator) updateDosResource(dosEx *DosEx) {
	if dosEx != nil {
		if dosEx.DosProtected != nil {
			allowListFileName := appProtectDosAllowListFileName(dosEx.DosProtected.GetNamespace(), dosEx.DosProtected.GetName())
			allowListContent := generateApDosAllowListFileContent(dosEx.DosProtected.Spec.AllowList)
			cnf.nginxManager.CreateAppProtectResourceFile(allowListFileName, allowListContent)
		}
		if dosEx.DosPolicy != nil {
			policyFileName := appProtectDosPolicyFileName(dosEx.DosPolicy.GetNamespace(), dosEx.DosPolicy.GetName())
			policyContent := generateApResourceFileContent(dosEx.DosPolicy)
			cnf.nginxManager.CreateAppProtectResourceFile(policyFileName, policyContent)
		}
		if dosEx.DosLogConf != nil {
			logConfFileName := appProtectDosLogConfFileName(dosEx.DosLogConf.GetNamespace(), dosEx.DosLogConf.GetName())
			logConfContent := generateApResourceFileContent(dosEx.DosLogConf)
			cnf.nginxManager.CreateAppProtectResourceFile(logConfFileName, logConfContent)
		}
	}
}

func (cnf *Configurator) updateApResourcesForVs(vsEx *VirtualServerEx) *appProtectPolicyResources {
	resources := newAppProtectPolicyResources()

	for apPolKey, apPol := range vsEx.ApPolRefs {
		policyFileName := appProtectPolicyFileNameFromUnstruct(apPol)
		policyContent := generateApResourceFileContent(apPol)
		cnf.nginxManager.CreateAppProtectResourceFile(policyFileName, policyContent)
		resources.Policies[apPolKey] = policyFileName
	}

	for logConfKey, logConf := range vsEx.LogConfRefs {
		logConfFileName := appProtectLogConfFileNameFromUnstruct(logConf)
		logConfContent := generateApResourceFileContent(logConf)
		cnf.nginxManager.CreateAppProtectResourceFile(logConfFileName, logConfContent)
		resources.LogConfs[logConfKey] = logConfFileName
	}

	return resources
}

func appProtectPolicyFileNameFromUnstruct(unst *unstructured.Unstructured) string {
	return fmt.Sprintf("%s%s_%s", appProtectPolicyFolder, unst.GetNamespace(), unst.GetName())
}

func appProtectLogConfFileNameFromUnstruct(unst *unstructured.Unstructured) string {
	return fmt.Sprintf("%s%s_%s", appProtectLogConfFolder, unst.GetNamespace(), unst.GetName())
}

func appProtectUserSigFileNameFromUnstruct(unst *unstructured.Unstructured) string {
	return fmt.Sprintf("%s%s_%s", appProtectUserSigFolder, unst.GetNamespace(), unst.GetName())
}

func generateDosLogDest(dest string) string {
	if dest == "stderr" {
		return dest
	}
	return "syslog:server=" + dest
}

func generateApResourceFileContent(apResource *unstructured.Unstructured) []byte {
	// Safe to ignore errors since validation already checked those
	spec, _, _ := unstructured.NestedMap(apResource.Object, "spec")
	data, _ := json.Marshal(spec)
	return data
}

func generateApDosAllowListFileContent(allowList []v1beta1.AllowListEntry) []byte {
	type IPAddress struct {
		IPAddress string `json:"ipAddress"`
	}

	type IPAddressList struct {
		IPAddresses   []IPAddress `json:"ipAddresses"`
		BlockRequests string      `json:"blockRequests"`
	}

	type Policy struct {
		IPAddressLists []IPAddressList `json:"ip-address-lists"`
	}

	type AllowListPolicy struct {
		Policy Policy `json:"policy"`
	}

	ipAddresses := make([]IPAddress, len(allowList))
	for i, entry := range allowList {
		ipAddresses[i] = IPAddress{IPAddress: entry.IPWithMask}
	}

	allowListPolicy := AllowListPolicy{
		Policy: Policy{
			IPAddressLists: []IPAddressList{
				{
					IPAddresses:   ipAddresses,
					BlockRequests: "transparent",
				},
			},
		},
	}

	data, err := json.Marshal(allowListPolicy)
	if err != nil {
		return nil
	}

	return data
}

// ResourceOperation represents a function that changes configuration in relation to an unstructured resource.
type ResourceOperation func(resource *v1beta1.DosProtectedResource, ingExes []*IngressEx, mergeableIngresses []*MergeableIngresses, vsExes []*VirtualServerEx) (Warnings, error)

// AddOrUpdateAppProtectResource updates Ingresses and VirtualServers that use App Protect or App Protect DoS resources.
func (cnf *Configurator) AddOrUpdateAppProtectResource(resource *unstructured.Unstructured, ingExes []*IngressEx, mergeableIngresses []*MergeableIngresses, vsExes []*VirtualServerEx) (Warnings, error) {
	warnings, err := cnf.addOrUpdateIngressesAndVirtualServers(ingExes, mergeableIngresses, vsExes)
	if err != nil {
		return warnings, fmt.Errorf("error when updating %v %v/%v: %w", resource.GetKind(), resource.GetNamespace(), resource.GetName(), err)
	}

	err = cnf.Reload(nginx.ReloadForOtherUpdate)
	if err != nil {
		return warnings, fmt.Errorf("error when reloading NGINX when updating %v %v/%v: %w", resource.GetKind(), resource.GetNamespace(), resource.GetName(), err)
	}

	return warnings, nil
}

// AddOrUpdateResourcesThatUseDosProtected updates Ingresses and VirtualServers that use DoS resources.
func (cnf *Configurator) AddOrUpdateResourcesThatUseDosProtected(ingExes []*IngressEx, mergeableIngresses []*MergeableIngresses, vsExes []*VirtualServerEx) (Warnings, error) {
	warnings, err := cnf.addOrUpdateIngressesAndVirtualServers(ingExes, mergeableIngresses, vsExes)
	if err != nil {
		return warnings, fmt.Errorf("error when updating resources that use Dos: %w", err)
	}

	err = cnf.Reload(nginx.ReloadForOtherUpdate)
	if err != nil {
		return warnings, fmt.Errorf("error when updating resources that use Dos: %w", err)
	}

	return warnings, nil
}

func (cnf *Configurator) addOrUpdateIngressesAndVirtualServers(ingExes []*IngressEx, mergeableIngresses []*MergeableIngresses, vsExes []*VirtualServerEx) (Warnings, error) {
	allWarnings := newWarnings()
	allWeightUpdates := []WeightUpdate{}

	for _, ingEx := range ingExes {
		_, warnings, err := cnf.addOrUpdateIngress(ingEx)
		if err != nil {
			return allWarnings, fmt.Errorf("error adding or updating ingress %v/%v: %w", ingEx.Ingress.Namespace, ingEx.Ingress.Name, err)
		}
		allWarnings.Add(warnings)
	}

	for _, m := range mergeableIngresses {
		_, warnings, err := cnf.addOrUpdateMergeableIngress(m)
		if err != nil {
			return allWarnings, fmt.Errorf("error adding or updating mergeableIngress %v/%v: %w", m.Master.Ingress.Namespace, m.Master.Ingress.Name, err)
		}
		allWarnings.Add(warnings)
	}

	for _, vs := range vsExes {
		_, warnings, weightUpdates, err := cnf.addOrUpdateVirtualServer(vs)
		if err != nil {
			return allWarnings, fmt.Errorf("error adding or updating VirtualServer %v/%v: %w", vs.VirtualServer.Namespace, vs.VirtualServer.Name, err)
		}
		allWeightUpdates = append(allWeightUpdates, weightUpdates...)
		allWarnings.Add(warnings)
	}

	for _, weightUpdate := range allWeightUpdates {
		cnf.nginxManager.UpsertSplitClientsKeyVal(weightUpdate.Zone, weightUpdate.Key, weightUpdate.Value)
	}

	return allWarnings, nil
}

// DeleteAppProtectPolicy updates Ingresses and VirtualServers that use AP Policy after that policy is deleted
func (cnf *Configurator) DeleteAppProtectPolicy(resource *unstructured.Unstructured, ingExes []*IngressEx, mergeableIngresses []*MergeableIngresses, vsExes []*VirtualServerEx) (Warnings, error) {
	warnings := newWarnings()
	var err error
	if len(ingExes)+len(mergeableIngresses)+len(vsExes) > 0 {
		warnings, err = cnf.AddOrUpdateAppProtectResource(resource, ingExes, mergeableIngresses, vsExes)
	}
	cnf.nginxManager.DeleteAppProtectResourceFile(appProtectPolicyFileNameFromUnstruct(resource))
	return warnings, err
}

// DeleteAppProtectLogConf updates Ingresses and VirtualServers that use AP Log Configuration after that policy is deleted
func (cnf *Configurator) DeleteAppProtectLogConf(resource *unstructured.Unstructured, ingExes []*IngressEx, mergeableIngresses []*MergeableIngresses, vsExes []*VirtualServerEx) (Warnings, error) {
	warnings := newWarnings()
	var err error
	if len(ingExes)+len(mergeableIngresses)+len(vsExes) > 0 {
		warnings, err = cnf.AddOrUpdateAppProtectResource(resource, ingExes, mergeableIngresses, vsExes)
	}
	cnf.nginxManager.DeleteAppProtectResourceFile(appProtectLogConfFileNameFromUnstruct(resource))
	return warnings, err
}

// RefreshAppProtectUserSigs writes all valid UDS files to fs and reloads NGINX
func (cnf *Configurator) RefreshAppProtectUserSigs(
	userSigs []*unstructured.Unstructured, delPols []string, ingExes []*IngressEx, mergeableIngresses []*MergeableIngresses, vsExes []*VirtualServerEx,
) (Warnings, error) {
	allWarnings, err := cnf.addOrUpdateIngressesAndVirtualServers(ingExes, mergeableIngresses, vsExes)
	if err != nil {
		return allWarnings, err
	}

	for _, file := range delPols {
		cnf.nginxManager.DeleteAppProtectResourceFile(file)
	}

	var builder strings.Builder
	cnf.nginxManager.ClearAppProtectFolder(appProtectUserSigFolder)
	for _, sig := range userSigs {
		fName := appProtectUserSigFileNameFromUnstruct(sig)
		data := generateApResourceFileContent(sig)
		cnf.nginxManager.CreateAppProtectResourceFile(fName, data)
		fmt.Fprintf(&builder, "app_protect_user_defined_signatures %s;\n", fName)
	}
	cnf.nginxManager.CreateAppProtectResourceFile(appProtectUserSigIndex, []byte(builder.String()))
	return allWarnings, cnf.Reload(nginx.ReloadForOtherUpdate)
}

func appProtectDosPolicyFileName(namespace string, name string) string {
	return fmt.Sprintf("%s%s_%s.json", appProtectDosPolicyFolder, namespace, name)
}

func appProtectDosLogConfFileName(namespace string, name string) string {
	return fmt.Sprintf("%s%s_%s.json", appProtectDosLogConfFolder, namespace, name)
}

func appProtectDosAllowListFileName(namespace string, name string) string {
	return fmt.Sprintf("%s%s_%s.json", appProtectDosAllowListFolder, namespace, name)
}

// DeleteAppProtectDosPolicy updates Ingresses and VirtualServers that use AP Dos Policy after that policy is deleted
func (cnf *Configurator) DeleteAppProtectDosPolicy(resource *unstructured.Unstructured) {
	cnf.nginxManager.DeleteAppProtectResourceFile(appProtectDosPolicyFileName(resource.GetNamespace(), resource.GetName()))
}

// DeleteAppProtectDosLogConf updates Ingresses and VirtualServers that use AP Log Configuration after that policy is deleted
func (cnf *Configurator) DeleteAppProtectDosLogConf(resource *unstructured.Unstructured) {
	cnf.nginxManager.DeleteAppProtectResourceFile(appProtectDosLogConfFileName(resource.GetNamespace(), resource.GetName()))
}

// DeleteAppProtectDosAllowList updates Ingresses and VirtualServers that use AP Allow List Configuration after that policy is deleted
func (cnf *Configurator) DeleteAppProtectDosAllowList(obj *v1beta1.DosProtectedResource) {
	cnf.nginxManager.DeleteAppProtectResourceFile(appProtectDosAllowListFileName(obj.Namespace, obj.Name))
}

// AddOrUpdateSecret writes a Secret to disk for the given role and returns the resulting paths.
func (cnf *Configurator) AddOrUpdateSecret(secret *api_v1.Secret, role secrets.SecretRole) secrets.Materialized {
	key := generateNamespaceNameKey(&secret.ObjectMeta)

	switch role {
	case secrets.RoleTLS:
		return secrets.Materialized{
			Path: cnf.nginxManager.CreateSecret(
				secretFileName(key, role),
				GenerateCertAndKeyFileContent(secret),
				nginx.ReadWriteOnlyFileMode,
			),
		}

	case secrets.RoleCA:
		return cnf.addOrUpdateCASecretForRole(secret, key)

	case secrets.RoleJWK:
		return secrets.Materialized{
			Path: cnf.nginxManager.CreateSecret(
				secretFileName(key, role),
				secret.Data[secrets.JWTKeyKey],
				nginx.JWKSecretFileMode,
			),
		}
	case secrets.RoleHtpasswd:
		return secrets.Materialized{
			Path: cnf.nginxManager.CreateSecret(
				secretFileName(key, role),
				secret.Data[secrets.HtpasswdFileKey],
				nginx.HtpasswdSecretFileMode,
			),
		}
	case secrets.RoleAuthFile:
		return secrets.Materialized{
			Path: cnf.nginxManager.CreateSecret(
				secretFileName(key, role),
				secret.Data[secrets.AuthFileKey],
				nginx.HtpasswdSecretFileMode,
			),
		}
	case secrets.RoleAuthMap:
		return secrets.Materialized{
			Path: cnf.nginxManager.CreateSecret(
				secretFileName(key, role),
				authMapFileContent(secret),
				nginx.HtpasswdSecretFileMode,
			),
		}
	}
	return secrets.Materialized{}
}

// DeleteSecret removes the files a Secret occupies for the given role.
func (cnf *Configurator) DeleteSecret(key string, role secrets.SecretRole) {
	name := secretFileName(key, role)
	if name == "" {
		return
	}
	cnf.nginxManager.DeleteSecret(name)

	if role == secrets.RoleCA {
		cnf.nginxManager.DeleteSecret(secretCRLFileName(key))
	}
}

// SecretPaths returns the paths a Secret would occupy for role, without touching
// the file system.
func (cnf *Configurator) SecretPaths(key string, role secrets.SecretRole) secrets.Materialized {
	name := secretFileName(key, role)
	if name == "" {
		return secrets.Materialized{}
	}
	return secrets.Materialized{Path: cnf.nginxManager.GetFilenameForSecret(name)}
}

// secretFileName returns the file name a Secret identified by key
// ("<namespace>/<name>") occupies for role, or "" for roles that produce no file.
func secretFileName(key string, role secrets.SecretRole) string {
	name := strings.ReplaceAll(key, "/", "_")

	switch role {
	case secrets.RoleTLS:
		return tlsKeyPairFilePrefix + name + ".pem"
	case secrets.RoleCA:
		return caCertBundleFilePrefix + name + ".crt"
	case secrets.RoleJWK:
		return jwtKeyFilePrefix + name
	case secrets.RoleHtpasswd:
		return basicAuthFilePrefix + name
	case secrets.RoleAuthFile, secrets.RoleAuthMap:
		return basicAuthFilePrefix + name + "_" + string(role)
	default:
		// OIDC and API key data is inlined into the generated configuration,
		// WAF bundle credentials are read from the Secret at fetch time, and the
		// license is written to a fixed path by AddOrUpdateLicenseSecret.
		return ""
	}
}

// authMapFileContent returns the htpasswd file of an auth-map Secret of
// ingress-nginx: a line user:hash for each of its keys, sorted by user.
func authMapFileContent(secret *api_v1.Secret) []byte {
	var b bytes.Buffer
	for _, user := range slices.Sorted(maps.Keys(secret.Data)) {
		b.WriteString(user)
		b.WriteByte(':')
		b.Write(secret.Data[user])
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// secretCRLFileName returns the certificate revocation list file name for a
// RoleCA Secret. Separate from secretFileName because RoleCA is the only role
// that produces two files.
func secretCRLFileName(key string) string {
	return caCRLBundleFilePrefix + strings.ReplaceAll(key, "/", "_") + ".pem"
}

// DynamicSSLReloadEnabled is used to check if dynamic reloading of SSL certificates is enabled
func (cnf *Configurator) DynamicSSLReloadEnabled() bool {
	return cnf.isDynamicSSLReloadEnabled
}

// UpsertSplitClientsKeyVal upserts a key-value pair in a keyzal zone for weight changes without reloads.
func (cnf *Configurator) UpsertSplitClientsKeyVal(zoneName, key, value string) {
	cnf.nginxManager.UpsertSplitClientsKeyVal(zoneName, key, value)
}

// GetIngressControllerReplicas returns the number of ingresscontroller-replicas (previously stored via SetIngressControllerReplicas)
func (cnf *Configurator) GetIngressControllerReplicas() int {
	return cnf.ingressControllerReplicas
}

// SetIngressControllerReplicas sets the number of ingresscontroller-replicas
// Is used for calculating ratelimits
func (cnf *Configurator) SetIngressControllerReplicas(replicas int) {
	cnf.ingressControllerReplicas = replicas
}
