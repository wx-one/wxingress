package k8s

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/wx-one/wxingress/internal/configs"
	"github.com/wx-one/wxingress/internal/gateway"
	nl "github.com/wx-one/wxingress/internal/logger"
	"github.com/wx-one/wxingress/internal/nsutils"
	internalValidation "github.com/wx-one/wxingress/internal/validation"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
	"github.com/wx-one/wxingress/pkg/apis/configuration/validation"
	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	ingressKind                        = "Ingress"
	virtualServerKind                  = "VirtualServer"
	virtualServerRouteKind             = "VirtualServerRoute"
	transportServerKind                = "TransportServer"
	policyKind                         = "Policy"
	secretKind                         = "Secret"
	serviceKind                        = "Service"
	namespaceKind                      = "Namespace"
	endpointSliceKind                  = "EndpointSlice"
	appProtectKind                     = "APPolicy"
	appProtectLogConfKind              = "APLogConf"
	appProtectUserSigKind              = "APUserSig"
	appProtectDosKind                  = "APDosPolicy"
	appProtectDosProtectedResourceKind = "APDosProtectedResource"
	appProtectDosLogConfKind           = "APDosLogConf"
)

// Operation defines an operation to perform for a resource.
type Operation int

const (
	// Delete the config of the resource
	Delete Operation = iota
	// AddOrUpdate the config of the resource
	AddOrUpdate
	// UpdateStatus updates resource status and events without regenerating NGINX config.
	// Used when a VirtualServer warning set changes but the rendered configuration does not.
	UpdateStatus
)

// Resource represents a configuration resource.
// A Resource can be a top level configuration object:
// - Regular or Master Ingress
// - VirtualServer
// - TransportServer
type Resource interface {
	GetObjectMeta() *metav1.ObjectMeta
	GetKeyWithKind() string
	Wins(resource Resource) bool
	AddWarning(warning string)
	IsEqual(resource Resource) bool
}

func chooseObjectMetaWinner(meta1 *metav1.ObjectMeta, meta2 *metav1.ObjectMeta) bool {
	if meta1.CreationTimestamp.Equal(&meta2.CreationTimestamp) {
		return meta1.UID > meta2.UID
	}

	return meta1.CreationTimestamp.Before(&meta2.CreationTimestamp)
}

// ResourceChange represents a change of the resource that needs to be reflected in the NGINX config.
type ResourceChange struct {
	// Op is an operation that needs be performed on the resource.
	Op Operation
	// Resource is the target resource.
	Resource Resource
	// Error is the error associated with the resource.
	Error string
}

// ConfigurationProblem is a problem associated with a configuration object.
type ConfigurationProblem struct {
	// Object is a configuration object.
	Object runtime.Object
	// IsError tells if the problem is an error. If it is an error, then it is expected that the status of the object
	// will be updated to the state 'invalid'. Otherwise, the state will be 'warning'.
	IsError bool
	// Reason tells the reason. It matches the reason in the events/status of our configuration objects.
	Reason string
	// Messages gives the details about the problem. It matches the message in the events/status of our configuration objects.
	Message string
}

func compareConfigurationProblems(problem1 *ConfigurationProblem, problem2 *ConfigurationProblem) bool {
	return problem1.IsError == problem2.IsError &&
		problem1.Reason == problem2.Reason &&
		problem1.Message == problem2.Message
}

// IngressConfiguration holds an Ingress resource with its minions. It implements the Resource interface.
type IngressConfiguration struct {
	// Ingress holds a regular Ingress or a master Ingress.
	Ingress *networking.Ingress
	// IsMaster is true when the Ingress is a master.
	IsMaster bool
	// Minions contains minions if the Ingress is a master.
	Minions []*MinionConfiguration
	// ValidHosts marks the hosts of the Ingress as valid (true) or invalid (false).
	// Regular Ingress resources can have multiple hosts. It is possible that some of the hosts are taken by other
	// resources. In that case, those hosts will be marked as invalid.
	ValidHosts map[string]bool
	// Warnings includes all the warnings for the resource.
	Warnings []string
	// ChildWarnings includes the warnings of the minions. The key is the namespace/name.
	ChildWarnings map[string][]string
	// Canaries contains the canary Ingresses with a host, path and path type of the Ingress or its minions, oldest first;
	// with canaryPerUpstream those that are the canary of an upstream (namespace, Service, port) of a path of the Ingress.
	Canaries []*networking.Ingress
	// CanaryBackends are, with canaryPerUpstream, the canary of each upstream of a path of the Ingress (or of one merged
	// into it, or of its minions) that has one, by ingress-nginx's name of the upstream (<namespace>-<service>-<port>).
	CanaryBackends map[string]CanaryBackend
	// Merged contains the other regular Ingresses with a host of this regular Ingress, oldest first:
	// as ingress-nginx does, their paths for the host are locations of its server.
	Merged []MergedIngress
	// DroppedServerAliases are the names of ingress-nginx's server-alias of the Ingress that are left out of its
	// server: a host another resource has, or the server-alias of an older Ingress (lower case).
	DroppedServerAliases map[string]bool
}

// CanaryBackend is the canary of an upstream: the canary Ingress and the backend of its path that matched the upstream's location.
type CanaryBackend struct {
	Ingress *networking.Ingress
	Backend networking.IngressBackend
}

// MergedIngress is a regular Ingress whose paths for the host are merged into the server of another Ingress.
type MergedIngress struct {
	Ingress *networking.Ingress
	Host    string
}

type listenerHostKey struct {
	ListenerName string
	Host         string
}

// used for sorting
func (lhk listenerHostKey) String() string {
	return fmt.Sprintf("%s|%s", lhk.ListenerName, lhk.Host)
}

// NewRegularIngressConfiguration creates an IngressConfiguration from an Ingress resource.
func NewRegularIngressConfiguration(ing *networking.Ingress) *IngressConfiguration {
	return &IngressConfiguration{
		Ingress:       ing,
		IsMaster:      false,
		ValidHosts:    make(map[string]bool),
		ChildWarnings: make(map[string][]string),
	}
}

// NewMasterIngressConfiguration creates an IngressConfiguration from a master Ingress resource.
func NewMasterIngressConfiguration(ing *networking.Ingress, minions []*MinionConfiguration, childWarnings map[string][]string) *IngressConfiguration {
	return &IngressConfiguration{
		Ingress:       ing,
		IsMaster:      true,
		Minions:       minions,
		ValidHosts:    make(map[string]bool),
		ChildWarnings: childWarnings,
	}
}

// GetObjectMeta returns the resource ObjectMeta.
func (ic *IngressConfiguration) GetObjectMeta() *metav1.ObjectMeta {
	return &ic.Ingress.ObjectMeta
}

// GetKeyWithKind returns the key of the resource with its kind. For example, Ingress/my-namespace/my-name.
func (ic *IngressConfiguration) GetKeyWithKind() string {
	key := getResourceKey(&ic.Ingress.ObjectMeta)
	return fmt.Sprintf("%s/%s", ingressKind, key)
}

// Wins tells if this resource wins over the specified resource.
func (ic *IngressConfiguration) Wins(resource Resource) bool {
	return chooseObjectMetaWinner(ic.GetObjectMeta(), resource.GetObjectMeta())
}

// AddWarning adds a warning.
func (ic *IngressConfiguration) AddWarning(warning string) {
	ic.Warnings = append(ic.Warnings, warning)
}

// IsEqual tests if the IngressConfiguration is equal to the resource.
func (ic *IngressConfiguration) IsEqual(resource Resource) bool {
	ingConfig, ok := resource.(*IngressConfiguration)
	if !ok {
		return false
	}

	if !compareObjectMetasWithAnnotations(&ic.Ingress.ObjectMeta, &ingConfig.Ingress.ObjectMeta) {
		return false
	}

	if !reflect.DeepEqual(ic.ValidHosts, ingConfig.ValidHosts) {
		return false
	}

	if ic.IsMaster != ingConfig.IsMaster {
		return false
	}

	if len(ic.Minions) != len(ingConfig.Minions) {
		return false
	}

	for i := range ic.Minions {
		if !compareObjectMetasWithAnnotations(&ic.Minions[i].Ingress.ObjectMeta, &ingConfig.Minions[i].Ingress.ObjectMeta) {
			return false
		}
	}

	if len(ic.Canaries) != len(ingConfig.Canaries) {
		return false
	}

	for i := range ic.Canaries {
		if !compareObjectMetasWithAnnotations(&ic.Canaries[i].ObjectMeta, &ingConfig.Canaries[i].ObjectMeta) {
			return false
		}
	}

	if len(ic.CanaryBackends) != len(ingConfig.CanaryBackends) {
		return false
	}

	for upstream, cb := range ic.CanaryBackends {
		other, ok := ingConfig.CanaryBackends[upstream]
		if !ok || !compareObjectMetasWithAnnotations(&cb.Ingress.ObjectMeta, &other.Ingress.ObjectMeta) || !reflect.DeepEqual(cb.Backend, other.Backend) {
			return false
		}
	}

	if len(ic.Merged) != len(ingConfig.Merged) {
		return false
	}

	for i := range ic.Merged {
		if ic.Merged[i].Host != ingConfig.Merged[i].Host ||
			!compareObjectMetasWithAnnotations(&ic.Merged[i].Ingress.ObjectMeta, &ingConfig.Merged[i].Ingress.ObjectMeta) {
			return false
		}
	}

	return maps.Equal(ic.DroppedServerAliases, ingConfig.DroppedServerAliases)
}

// MinionConfiguration holds a Minion resource.
type MinionConfiguration struct {
	// Ingress is the Ingress behind a minion.
	Ingress *networking.Ingress
	// ValidPaths marks the paths of the Ingress as valid (true) or invalid (false).
	// Minion Ingress resources can have multiple paths. It is possible that some of the paths are taken by other
	// Minions. In that case, those paths will be marked as invalid.
	ValidPaths map[string]bool
}

// NewMinionConfiguration creates a new MinionConfiguration.
func NewMinionConfiguration(ing *networking.Ingress) *MinionConfiguration {
	return &MinionConfiguration{
		Ingress:    ing,
		ValidPaths: make(map[string]bool),
	}
}

// VirtualServerConfiguration holds a VirtualServer along with its VirtualServerRoutes.
type VirtualServerConfiguration struct {
	VirtualServer       *conf_v1.VirtualServer
	VirtualServerRoutes []*conf_v1.VirtualServerRoute
	// ChallengeRoutes holds synthetic VirtualServerRoutes built from cert-manager ACME HTTP-01 solver Ingresses.
	// They are kept apart from VirtualServerRoutes so they never enter the VSR->VS index or VSR status updates.
	ChallengeRoutes             []*conf_v1.VirtualServerRoute
	VirtualServerRouteSelectors map[string][]string
	Warnings                    []string
	HTTPPort                    int
	HTTPSPort                   int
	HTTPIPv4                    string
	HTTPIPv6                    string
	HTTPSIPv4                   string
	HTTPSIPv6                   string
}

// NewVirtualServerConfiguration creates a VirtualServerConfiguration.
func NewVirtualServerConfiguration(vs *conf_v1.VirtualServer, vsrs []*conf_v1.VirtualServerRoute, vsrSelectors map[string][]string, warnings []string) *VirtualServerConfiguration {
	return &VirtualServerConfiguration{
		VirtualServer:               vs,
		VirtualServerRoutes:         vsrs,
		VirtualServerRouteSelectors: vsrSelectors,
		Warnings:                    warnings,
	}
}

// GetObjectMeta returns the resource ObjectMeta.
func (vsc *VirtualServerConfiguration) GetObjectMeta() *metav1.ObjectMeta {
	return &vsc.VirtualServer.ObjectMeta
}

// GetKeyWithKind returns the key of the resource with its kind. For example, VirtualServer/my-namespace/my-name.
func (vsc *VirtualServerConfiguration) GetKeyWithKind() string {
	key := getResourceKey(&vsc.VirtualServer.ObjectMeta)
	return fmt.Sprintf("%s/%s", virtualServerKind, key)
}

// Wins tells if this resource wins over the specified resource.
// It is used to determine which resource should win over a host.
func (vsc *VirtualServerConfiguration) Wins(resource Resource) bool {
	return chooseObjectMetaWinner(vsc.GetObjectMeta(), resource.GetObjectMeta())
}

// AddWarning adds a warning.
func (vsc *VirtualServerConfiguration) AddWarning(warning string) {
	vsc.Warnings = append(vsc.Warnings, warning)
}

// IsEqual tests if the VirtualServerConfiguration is equal to the resource.
func (vsc *VirtualServerConfiguration) IsEqual(resource Resource) bool {
	vsConfig, ok := resource.(*VirtualServerConfiguration)
	if !ok {
		return false
	}

	if !compareObjectMetas(&vsc.VirtualServer.ObjectMeta, &vsConfig.VirtualServer.ObjectMeta) {
		return false
	}

	if len(vsc.VirtualServerRoutes) != len(vsConfig.VirtualServerRoutes) {
		return false
	}

	for i := range vsc.VirtualServerRoutes {
		if !compareObjectMetas(&vsc.VirtualServerRoutes[i].ObjectMeta, &vsConfig.VirtualServerRoutes[i].ObjectMeta) {
			return false
		}
	}

	if len(vsc.ChallengeRoutes) != len(vsConfig.ChallengeRoutes) {
		return false
	}

	for i := range vsc.ChallengeRoutes {
		if !compareObjectMetas(&vsc.ChallengeRoutes[i].ObjectMeta, &vsConfig.ChallengeRoutes[i].ObjectMeta) {
			return false
		}
	}

	// Check VirtualServerRouteSelectors maps for equality
	if len(vsc.VirtualServerRouteSelectors) != len(vsConfig.VirtualServerRouteSelectors) {
		return false
	}

	for selector, routes := range vsc.VirtualServerRouteSelectors {
		otherRoutes, exists := vsConfig.VirtualServerRouteSelectors[selector]
		if !exists {
			return false
		}

		if len(routes) != len(otherRoutes) {
			return false
		}

		// Create maps for O(1) lookup to compare route slices
		routeSet := make(map[string]bool)
		for _, route := range routes {
			routeSet[route] = true
		}

		for _, otherRoute := range otherRoutes {
			if !routeSet[otherRoute] {
				return false
			}
		}
	}

	return true
}

// TransportServerConfiguration holds a TransportServer resource.
type TransportServerConfiguration struct {
	ListenerPort    int
	IPv4            string
	IPv6            string
	TransportServer *conf_v1.TransportServer
	Warnings        []string
}

// NewTransportServerConfiguration creates a new TransportServerConfiguration.
func NewTransportServerConfiguration(ts *conf_v1.TransportServer) *TransportServerConfiguration {
	return &TransportServerConfiguration{
		TransportServer: ts,
	}
}

// GetObjectMeta returns the resource ObjectMeta.
func (tsc *TransportServerConfiguration) GetObjectMeta() *metav1.ObjectMeta {
	return &tsc.TransportServer.ObjectMeta
}

// GetKeyWithKind returns the key of the resource with its kind. For example, TransportServer/my-namespace/my-name.
func (tsc *TransportServerConfiguration) GetKeyWithKind() string {
	key := getResourceKey(&tsc.TransportServer.ObjectMeta)
	return fmt.Sprintf("%s/%s", transportServerKind, key)
}

// Wins tells if this resource wins over the specified resource.
// It is used to determine which resource should win over a host or port.
func (tsc *TransportServerConfiguration) Wins(resource Resource) bool {
	return chooseObjectMetaWinner(tsc.GetObjectMeta(), resource.GetObjectMeta())
}

// AddWarning adds a warning.
func (tsc *TransportServerConfiguration) AddWarning(warning string) {
	tsc.Warnings = append(tsc.Warnings, warning)
}

// IsEqual tests if the TransportServerConfiguration is equal to the resource.
func (tsc *TransportServerConfiguration) IsEqual(resource Resource) bool {
	tsConfig, ok := resource.(*TransportServerConfiguration)
	if !ok {
		return false
	}

	return compareObjectMetas(tsc.GetObjectMeta(), resource.GetObjectMeta()) && tsc.ListenerPort == tsConfig.ListenerPort
}

func compareObjectMetas(meta1 *metav1.ObjectMeta, meta2 *metav1.ObjectMeta) bool {
	return meta1.Namespace == meta2.Namespace &&
		meta1.Name == meta2.Name &&
		meta1.Generation == meta2.Generation
}

func compareObjectMetasWithAnnotations(meta1 *metav1.ObjectMeta, meta2 *metav1.ObjectMeta) bool {
	return compareObjectMetas(meta1, meta2) && reflect.DeepEqual(meta1.Annotations, meta2.Annotations)
}

// TransportServerMetrics holds metrics about TransportServer resources
type TransportServerMetrics struct {
	TotalTLSPassthrough int
	TotalTCP            int
	TotalUDP            int
}

// Configuration represents the configuration of the Ingress Controller - a collection of configuration objects
// (Ingresses, VirtualServers, VirtualServerRoutes) ready to be transformed into NGINX config.
// It holds the latest valid state of those objects.
// The IC needs to ensure that at any point in time the NGINX config on the filesystem reflects the state
// of the objects in the Configuration.
type Configuration struct {
	hosts         map[string]Resource
	listenerHosts map[listenerHostKey]*TransportServerConfiguration
	listenerMap   map[string]conf_v1.Listener

	// only valid resources with the matching IngressClass are stored
	ingresses           map[string]*networking.Ingress
	virtualServers      map[string]*conf_v1.VirtualServer
	virtualServerRoutes map[string]*conf_v1.VirtualServerRoute
	transportServers    map[string]*conf_v1.TransportServer

	// minionsByHost indexes minion Ingresses by their host for O(1) lookup.
	// Outer key: host string, inner key: ingress resource key (namespace/name).
	// Maintained by AddOrUpdateIngress/DeleteIngress; consumed by buildMinionConfigs.
	minionsByHost map[string]map[string]bool

	// vsrToVSConfigs indexes accepted VirtualServers by VSR key (namespace/name) in deterministic key order.
	vsrToVSConfigs map[string][]*conf_v1.VirtualServer

	// vsrsWithChangedRefs tracks reference changes from the latest rebuild even when surviving VSs are not re-rendered.
	vsrsWithChangedRefs []*conf_v1.VirtualServerRoute

	globalConfiguration *conf_v1.GlobalConfiguration
	// gatewayListeners are the listeners of the Gateways with addresses of
	// their own, which their VirtualServers and TransportServers name as if
	// they were in the GlobalConfiguration
	gatewayListeners []conf_v1.Listener
	// gatewayListenersChanged tells the TransportServers of Gateways that
	// the listeners changed since they were last replaced
	gatewayListenersChanged bool

	hostProblems     map[string]ConfigurationProblem
	listenerProblems map[string]ConfigurationProblem

	hasCorrectIngressClass       func(interface{}) bool
	virtualServerValidator       *validation.VirtualServerValidator
	globalConfigurationValidator *validation.GlobalConfigurationValidator
	transportServerValidator     *validation.TransportServerValidator

	secretReferenceChecker     *secretReferenceChecker
	serviceReferenceChecker    *serviceReferenceChecker
	endpointReferenceChecker   *serviceReferenceChecker
	policyReferenceChecker     *policyReferenceChecker
	appPolicyReferenceChecker  *appProtectResourceReferenceChecker
	appLogConfReferenceChecker *appProtectResourceReferenceChecker
	appDosProtectedChecker     *dosResourceReferenceChecker

	isPlus                       bool
	appProtectEnabled            bool
	appProtectDosEnabled         bool
	isTLSPassthroughEnabled      bool
	snippetsEnabled              bool
	isCertManagerEnabled         bool
	isIPV6Disabled               bool
	isDirectiveAutoadjustEnabled bool
	allowEmptyIngressHost        bool
	// isDynamicUpstreams tells if NGINX runs with the wxingress module (-dynamic-upstreams), which canary Ingresses need.
	isDynamicUpstreams bool
	// canaryPerUpstream makes a canary Ingress the canary of the upstream - namespace, Service, port - of the location
	// with its host, path and path type, for every path of every Ingress with that upstream, as ingress-nginx's
	// alternative backends are (-ingress-nginx-defaults); without it, the canary of that location's Ingress alone.
	canaryPerUpstream bool
	// crossNamespace lets Ingresses of other namespaces share a host with the Ingress that holds it - their paths
	// merged into its server - and canary Ingresses attach to Ingresses of other namespaces, as ingress-nginx does
	// (-ingress-nginx-cross-namespace). Without it, the host of an Ingress is its namespace's: an Ingress of another
	// namespace with that host loses it, and a canary attaches only to Ingresses of its own namespace.
	crossNamespace bool

	// startupComplete indicates whether the initial informer cache sync and
	// queue drain have finished. When false, rebuildHosts() is skipped in
	// AddOrUpdate*/Delete* methods to avoid O(N) full rebuilds during startup.
	// CompleteStartup() flips this to true and performs a single rebuild.
	startupComplete bool

	lock sync.RWMutex
}

// NewConfiguration creates a new Configuration.
func NewConfiguration(
	hasCorrectIngressClass func(interface{}) bool,
	isPlus bool,
	appProtectEnabled bool,
	appProtectDosEnabled bool,
	virtualServerValidator *validation.VirtualServerValidator,
	globalConfigurationValidator *validation.GlobalConfigurationValidator,
	transportServerValidator *validation.TransportServerValidator,
	isTLSPassthroughEnabled bool,
	snippetsEnabled bool,
	isCertManagerEnabled bool,
	isIPV6Disabled bool,
	isDirectiveAutoadjustEnabled bool,
	allowEmptyIngressHost bool,
) *Configuration {
	policyServiceRefs := make(map[string]string)
	return &Configuration{
		hosts:                        make(map[string]Resource),
		listenerHosts:                make(map[listenerHostKey]*TransportServerConfiguration),
		ingresses:                    make(map[string]*networking.Ingress),
		virtualServers:               make(map[string]*conf_v1.VirtualServer),
		virtualServerRoutes:          make(map[string]*conf_v1.VirtualServerRoute),
		transportServers:             make(map[string]*conf_v1.TransportServer),
		minionsByHost:                make(map[string]map[string]bool),
		vsrToVSConfigs:               make(map[string][]*conf_v1.VirtualServer),
		hostProblems:                 make(map[string]ConfigurationProblem),
		hasCorrectIngressClass:       hasCorrectIngressClass,
		virtualServerValidator:       virtualServerValidator,
		globalConfigurationValidator: globalConfigurationValidator,
		transportServerValidator:     transportServerValidator,
		secretReferenceChecker:       newSecretReferenceChecker(isPlus),
		serviceReferenceChecker:      newServiceReferenceChecker(false, policyServiceRefs),
		endpointReferenceChecker:     newServiceReferenceChecker(true, policyServiceRefs),
		policyReferenceChecker:       newPolicyReferenceChecker(),
		appPolicyReferenceChecker:    newAppProtectResourceReferenceChecker(configs.AppProtectPolicyAnnotation),
		appLogConfReferenceChecker:   newAppProtectResourceReferenceChecker(configs.AppProtectLogConfAnnotation),
		appDosProtectedChecker:       newDosResourceReferenceChecker(configs.AppProtectDosProtectedAnnotation),
		isPlus:                       isPlus,
		appProtectEnabled:            appProtectEnabled,
		appProtectDosEnabled:         appProtectDosEnabled,
		isTLSPassthroughEnabled:      isTLSPassthroughEnabled,
		snippetsEnabled:              snippetsEnabled,
		isCertManagerEnabled:         isCertManagerEnabled,
		isIPV6Disabled:               isIPV6Disabled,
		isDirectiveAutoadjustEnabled: isDirectiveAutoadjustEnabled,
		allowEmptyIngressHost:        allowEmptyIngressHost,
	}
}

// AddOrUpdateIngress adds or updates the Ingress resource.
func (c *Configuration) AddOrUpdateIngress(ing *networking.Ingress) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	key := getResourceKey(&ing.ObjectMeta)
	var validationError error

	// duplicate hosts merged, a rule-less defaultBackend a catch-all: as ingress-nginx
	ing = asIngressNginx(ing, c.allowEmptyIngressHost)

	if !c.hasCorrectIngressClass(ing) {
		delete(c.ingresses, key)
		c.updateMinionIndex(key, nil)
	} else {
		validationError = validateIngress(ing, c.isPlus, c.appProtectEnabled, c.appProtectDosEnabled, c.snippetsEnabled, c.isDirectiveAutoadjustEnabled, c.allowEmptyIngressHost).ToAggregate()
		if validationError != nil {
			delete(c.ingresses, key)
			c.updateMinionIndex(key, nil)
		} else {
			c.ingresses[key] = ing
			c.updateMinionIndex(key, ing)
		}
	}

	if !c.startupComplete {
		var problems []ConfigurationProblem
		if validationError != nil {
			problems = append(problems, ConfigurationProblem{
				Object:  ing,
				IsError: true,
				Reason:  nl.EventReasonRejected,
				Message: validationError.Error(),
			})
		}
		return nil, problems
	}

	changes, problems := c.rebuildHosts()

	if validationError != nil {
		// If the invalid resource has any active hosts, rebuildHosts will create a change
		// to remove the resource.
		// Here we add the validationErr to that change.
		keyWithKind := getResourceKeyWithKind(ingressKind, &ing.ObjectMeta)
		for i := range changes {
			k := changes[i].Resource.GetKeyWithKind()

			if k == keyWithKind {
				changes[i].Error = validationError.Error()
				return changes, problems
			}
		}

		// On the other hand, the invalid resource might not have any active hosts.
		// Or the resource was invalid before and is still invalid (in some different way).
		// In those cases,  rebuildHosts will create no change for that resource.
		// To make sure the validationErr is reported to the user, we create a problem.
		p := ConfigurationProblem{
			Object:  ing,
			IsError: true,
			Reason:  nl.EventReasonRejected,
			Message: validationError.Error(),
		}
		problems = append(problems, p)
	}

	return changes, problems
}

// DeleteIngress deletes an Ingress resource by the key.
func (c *Configuration) DeleteIngress(key string) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	_, exists := c.ingresses[key]
	if !exists {
		return nil, nil
	}

	delete(c.ingresses, key)
	c.removeMinionFromIndex(key)

	if !c.startupComplete {
		return nil, nil
	}

	return c.rebuildHosts()
}

// AddOrUpdateVirtualServer adds or updates the VirtualServer resource.
func (c *Configuration) AddOrUpdateVirtualServer(vs *conf_v1.VirtualServer) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	key := getResourceKey(&vs.ObjectMeta)
	var validationError error

	if !c.hasCorrectIngressClass(vs) {
		delete(c.virtualServers, key)
	} else {
		validationError = c.virtualServerValidator.ValidateVirtualServer(vs)
		if validationError != nil {
			delete(c.virtualServers, key)
		} else {
			c.balanceUpstreamProxies(vs.Spec.Upstreams)
			c.virtualServers[key] = vs
		}
	}

	if !c.startupComplete {
		var problems []ConfigurationProblem
		if validationError != nil {
			problems = append(problems, ConfigurationProblem{
				Object:  vs,
				IsError: true,
				Reason:  nl.EventReasonRejected,
				Message: fmt.Sprintf("VirtualServer %s was rejected with error: %s", getResourceKey(&vs.ObjectMeta), validationError.Error()),
			})
		}
		return nil, problems
	}

	changes, problems := c.rebuildHosts()

	if validationError != nil {
		// If the invalid resource has an active host, rebuildHosts will create a change
		// to remove the resource.
		// Here we add the validationErr to that change.
		kind := getResourceKeyWithKind(virtualServerKind, &vs.ObjectMeta)
		for i := range changes {
			k := changes[i].Resource.GetKeyWithKind()

			if k == kind {
				changes[i].Error = validationError.Error()
				return changes, problems
			}
		}

		// On the other hand, the invalid resource might not have any active host.
		// Or the resource was invalid before and is still invalid (in some different way).
		// In those cases,  rebuildHosts will create no change for that resource.
		// To make sure the validationErr is reported to the user, we create a problem.
		p := ConfigurationProblem{
			Object:  vs,
			IsError: true,
			Reason:  nl.EventReasonRejected,
			Message: fmt.Sprintf("VirtualServer %s was rejected with error: %s", getResourceKey(&vs.ObjectMeta), validationError.Error()),
		}
		problems = append(problems, p)
	}

	return changes, problems
}

// DeleteVirtualServer deletes a VirtualServerResource by the key.
func (c *Configuration) DeleteVirtualServer(key string) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	_, exists := c.virtualServers[key]
	if !exists {
		return nil, nil
	}

	delete(c.virtualServers, key)

	if !c.startupComplete {
		return nil, nil
	}

	return c.rebuildHosts()
}

// GetVirtualServer returns the last-applied VirtualServer with the given key,
// or nil if none is tracked. Safe to call from any goroutine.
func (c *Configuration) GetVirtualServer(key string) *conf_v1.VirtualServer {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.virtualServers[key]
}

// AddOrUpdateVirtualServerRoute adds or updates the VirtualServerRoute.
func (c *Configuration) AddOrUpdateVirtualServerRoute(vsr *conf_v1.VirtualServerRoute) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	key := getResourceKey(&vsr.ObjectMeta)
	var validationError error

	if !c.hasCorrectIngressClass(vsr) {
		delete(c.virtualServerRoutes, key)
	} else {
		validationError = c.virtualServerValidator.ValidateVirtualServerRoute(vsr)
		if validationError != nil {
			delete(c.virtualServerRoutes, key)
		} else {
			// Balance proxy buffer sizes for all upstreams before storing
			c.balanceUpstreamProxies(vsr.Spec.Upstreams)
			c.virtualServerRoutes[key] = vsr
		}
	}

	if !c.startupComplete {
		var problems []ConfigurationProblem
		if validationError != nil {
			problems = append(problems, ConfigurationProblem{
				Object:  vsr,
				IsError: true,
				Reason:  nl.EventReasonRejected,
				Message: fmt.Sprintf("VirtualServerRoute %s was rejected with error: %s", getResourceKey(&vsr.ObjectMeta), validationError.Error()),
			})
		}
		return nil, problems
	}

	changes, problems := c.rebuildHosts()

	if validationError != nil {
		p := ConfigurationProblem{
			Object:  vsr,
			IsError: true,
			Reason:  nl.EventReasonRejected,
			Message: fmt.Sprintf("VirtualServerRoute %s was rejected with error: %s", getResourceKey(&vsr.ObjectMeta), validationError.Error()),
		}
		problems = append(problems, p)
	}

	return changes, problems
}

// DeleteVirtualServerRoute deletes a VirtualServerRoute by the key.
func (c *Configuration) DeleteVirtualServerRoute(key string) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	_, exists := c.virtualServerRoutes[key]
	if !exists {
		return nil, nil
	}

	delete(c.virtualServerRoutes, key)

	if !c.startupComplete {
		return nil, nil
	}

	return c.rebuildHosts()
}

// GetVirtualServerRoute returns the last-applied VirtualServerRoute with the given key,
// or nil if none is tracked. Safe to call from any goroutine.
func (c *Configuration) GetVirtualServerRoute(key string) *conf_v1.VirtualServerRoute {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.virtualServerRoutes[key]
}

// AddOrUpdateGlobalConfiguration adds or updates the GlobalConfiguration.
func (c *Configuration) AddOrUpdateGlobalConfiguration(gc *conf_v1.GlobalConfiguration) ([]ResourceChange, []ConfigurationProblem, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	var changes []ResourceChange
	var problems []ConfigurationProblem

	validationErr := c.globalConfigurationValidator.ValidateGlobalConfiguration(gc)

	c.globalConfiguration = gc
	c.setGlobalConfigListenerMap()

	listenerChanges, listenerProblems := c.rebuildListenerHosts()

	changes = append(changes, listenerChanges...)
	problems = append(problems, listenerProblems...)

	if c.startupComplete {
		hostChanges, hostProblems := c.rebuildHosts()
		changes = append(changes, hostChanges...)
		problems = append(problems, hostProblems...)
	}

	return changes, problems, validationErr
}

// DeleteGlobalConfiguration deletes GlobalConfiguration.
func (c *Configuration) DeleteGlobalConfiguration() ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	var changes []ResourceChange
	var problems []ConfigurationProblem

	c.globalConfiguration = nil
	c.setGlobalConfigListenerMap()
	listenerChanges, listenerProblems := c.rebuildListenerHosts()
	changes = append(changes, listenerChanges...)
	problems = append(problems, listenerProblems...)

	if c.startupComplete {
		hostChanges, hostProblems := c.rebuildHosts()
		changes = append(changes, hostChanges...)
		problems = append(problems, hostProblems...)
	}

	return changes, problems
}

// GetGlobalConfiguration returns the current GlobalConfiguration.
func (c *Configuration) GetGlobalConfiguration() *conf_v1.GlobalConfiguration {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.globalConfiguration
}

// AddOrUpdateTransportServer adds or updates the TransportServer.
func (c *Configuration) AddOrUpdateTransportServer(ts *conf_v1.TransportServer) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	key := getResourceKey(&ts.ObjectMeta)
	var validationErr error

	if !c.hasCorrectIngressClass(ts) {
		delete(c.transportServers, key)
	} else {
		validationErr = c.transportServerValidator.ValidateTransportServer(ts)
		if validationErr != nil {
			delete(c.transportServers, key)
		} else {
			c.transportServers[key] = ts
		}
	}

	changes, problems := c.rebuildListenerHosts()

	if c.isTLSPassthroughEnabled && c.startupComplete {
		hostChanges, hostProblems := c.rebuildHosts()

		changes = append(changes, hostChanges...)
		problems = append(problems, hostProblems...)
	}

	if validationErr != nil {
		// If the invalid resource has an active host/listener, rebuildHosts/rebuildListenerHosts will create a change
		// to remove the resource.
		// Here we add the validationErr to that change.
		kind := getResourceKeyWithKind(transportServerKind, &ts.ObjectMeta)
		for i := range changes {
			k := changes[i].Resource.GetKeyWithKind()

			if k == kind {
				changes[i].Error = validationErr.Error()
				return changes, problems
			}
		}

		// On the other hand, the invalid resource might not have any active host/listener.
		// Or the resource was invalid before and is still invalid (in some different way).
		// In those cases,  rebuildHosts/rebuildListenerHosts will create no change for that resource.
		// To make sure the validationErr is reported to the user, we create a problem.
		p := ConfigurationProblem{
			Object:  ts,
			IsError: true,
			Reason:  nl.EventReasonRejected,
			Message: fmt.Sprintf("TransportServer %s was rejected with error: %s", getResourceKey(&ts.ObjectMeta), validationErr.Error()),
		}
		problems = append(problems, p)
	}

	return changes, problems
}

// DeleteTransportServer deletes a TransportServer by the key.
func (c *Configuration) DeleteTransportServer(key string) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	_, exists := c.transportServers[key]
	if !exists {
		return nil, nil
	}

	delete(c.transportServers, key)

	changes, problems := c.rebuildListenerHosts()

	if c.isTLSPassthroughEnabled && c.startupComplete {
		hostChanges, hostProblems := c.rebuildHosts()

		changes = append(changes, hostChanges...)
		problems = append(problems, hostProblems...)
	}

	return changes, problems
}

// updateMinionIndex adds or removes an ingress from the minionsByHost index.
// Must be called with the lock held.
func (c *Configuration) updateMinionIndex(key string, ing *networking.Ingress) {
	// Remove old entry for this key from any host it was previously under.
	c.removeMinionFromIndex(key)

	// If the ingress is a valid minion, add it to the index under its host.
	if ing != nil && isMinion(ing) && len(ing.Spec.Rules) > 0 {
		host := ing.Spec.Rules[0].Host
		if c.minionsByHost[host] == nil {
			c.minionsByHost[host] = make(map[string]bool)
		}
		c.minionsByHost[host][key] = true
	}
}

// removeMinionFromIndex removes a key from the minionsByHost index.
// Must be called with the lock held.
func (c *Configuration) removeMinionFromIndex(key string) {
	for host, keys := range c.minionsByHost {
		if keys[key] {
			delete(keys, key)
			if len(keys) == 0 {
				delete(c.minionsByHost, host)
			}
			return
		}
	}
}

// CompleteStartup marks startup as complete and performs a single rebuildHosts()
// to compute the definitive host→resource mapping and all ConfigurationProblems
// (host conflicts, orphaned minions, orphaned VSRs). This must be called exactly
// once after the initial informer cache sync and queue drain, before
// updateAllConfigs() generates and writes NGINX config files.
func (c *Configuration) CompleteStartup() ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.startupComplete = true
	return c.rebuildHosts()
}

func (c *Configuration) rebuildListenerHosts() ([]ResourceChange, []ConfigurationProblem) {
	newListenerHosts, newTSConfigs := c.buildListenerHostsAndTSConfigurations()

	removedListenerHosts, updatedListenerHosts, addedListenerHosts := detectChangesInListenerHosts(c.listenerHosts, newListenerHosts)
	changes := createResourceChangesForListeners(removedListenerHosts, updatedListenerHosts, addedListenerHosts, c.listenerHosts, newListenerHosts)

	c.listenerHosts = newListenerHosts

	changes = squashResourceChanges(changes)

	// Note that the change will not refer to the latest version, if the TransportServerConfiguration is being removed.
	// However, referring to the latest version is necessary so that the resource latest Warnings are reported and not lost.
	// So here we make sure that changes always refer to the latest version of TransportServerConfigurations.
	for i := range changes {
		key := changes[i].Resource.GetKeyWithKind()
		if r, exists := newTSConfigs[key]; exists {
			changes[i].Resource = r
		}
	}

	newProblems := make(map[string]ConfigurationProblem)

	c.addProblemsForTSConfigsWithoutActiveListener(newTSConfigs, newProblems)

	newOrUpdatedProblems := detectChangesInProblems(newProblems, c.listenerProblems)

	// safe to update problems
	c.listenerProblems = newProblems

	return changes, newOrUpdatedProblems
}

func (c *Configuration) buildListenerHostsAndTSConfigurations() (map[listenerHostKey]*TransportServerConfiguration, map[string]*TransportServerConfiguration) {
	newListenerHosts := make(map[listenerHostKey]*TransportServerConfiguration)
	newTSConfigs := make(map[string]*TransportServerConfiguration)

	for key, ts := range c.transportServers {
		if ts.Spec.Listener.Protocol == conf_v1.TLSPassthroughListenerProtocol {
			continue
		}
		tsc := NewTransportServerConfiguration(ts)
		newTSConfigs[key] = tsc

		// the listeners of the GlobalConfiguration and those of Gateways
		listener, found := c.listenerMap[ts.Spec.Listener.Name]
		if !found || ts.Spec.Listener.Protocol != listener.Protocol {
			continue
		}

		tsc.ListenerPort = listener.Port
		tsc.IPv4 = listener.IPv4
		tsc.IPv6 = listener.IPv6

		host := ts.Spec.Host
		listenerKey := listenerHostKey{ListenerName: listener.Name, Host: host}

		holder, exists := newListenerHosts[listenerKey]
		if !exists {
			newListenerHosts[listenerKey] = tsc
			continue
		}

		// another TransportServer exists with the same listener and host
		warning := fmt.Sprintf("listener %s and host %s are taken by another resource", listener.Name, host)

		if !holder.Wins(tsc) {
			holder.AddWarning(warning)
			newListenerHosts[listenerKey] = tsc
		} else {
			tsc.AddWarning(warning)
		}
	}

	return newListenerHosts, newTSConfigs
}

func (c *Configuration) buildListenersForVSConfiguration(vsc *VirtualServerConfiguration) {
	vs := vsc.VirtualServer
	if vs.Spec.Listener == nil || len(c.listenerMap) == 0 {
		return
	}

	assignListener := func(listenerName string, isSSL bool, port *int, ipv4 *string, ipv6 *string) {
		if gcListener, ok := c.listenerMap[listenerName]; ok && gcListener.Protocol == conf_v1.HTTPProtocol && gcListener.Ssl == isSSL {
			*port = gcListener.Port
			*ipv4 = gcListener.IPv4
			*ipv6 = gcListener.IPv6
		}
	}

	assignListener(vs.Spec.Listener.HTTP, false, &vsc.HTTPPort, &vsc.HTTPIPv4, &vsc.HTTPIPv6)
	assignListener(vs.Spec.Listener.HTTPS, true, &vsc.HTTPSPort, &vsc.HTTPSIPv4, &vsc.HTTPSIPv6)
}

// GetResources returns all configuration resources.
func (c *Configuration) GetResources() []Resource {
	return c.GetResourcesWithFilter(resourceFilter{
		Ingresses:        true,
		VirtualServers:   true,
		TransportServers: true,
	})
}

type resourceFilter struct {
	Ingresses        bool
	VirtualServers   bool
	TransportServers bool
}

// GetResourcesWithFilter returns resources using the filter.
func (c *Configuration) GetResourcesWithFilter(filter resourceFilter) []Resource {
	c.lock.RLock()
	defer c.lock.RUnlock()

	resources := make(map[string]Resource)

	for _, r := range c.hosts {
		switch r.(type) {
		case *IngressConfiguration:
			if filter.Ingresses {
				resources[r.GetKeyWithKind()] = r
			}
		case *VirtualServerConfiguration:
			if filter.VirtualServers {
				resources[r.GetKeyWithKind()] = r
			}
		case *TransportServerConfiguration:
			if filter.TransportServers {
				resources[r.GetKeyWithKind()] = r
			}
		}
	}

	if filter.TransportServers {
		for _, r := range c.listenerHosts {
			resources[r.GetKeyWithKind()] = r
		}
	}

	var result []Resource
	for _, key := range getSortedResourceKeys(resources) {
		result = append(result, resources[key])
	}

	return result
}

// FindResourcesForService finds resources that reference the specified service.
func (c *Configuration) FindResourcesForService(svcNamespace string, svcName string) []Resource {
	return c.findResourcesForResourceReference(svcNamespace, svcName, c.serviceReferenceChecker)
}

// IsServiceReferencedByVirtualServer checks if the specified service is referenced by the VirtualServer.
func (c *Configuration) IsServiceReferencedByVirtualServer(svcNamespace, svcName string, vs *conf_v1.VirtualServer) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.serviceReferenceChecker.IsReferencedByVirtualServer(svcNamespace, svcName, vs)
}

// IsServiceReferencedByVirtualServerRoute checks if the specified service is referenced by the VirtualServerRoute.
func (c *Configuration) IsServiceReferencedByVirtualServerRoute(svcNamespace, svcName string, vsr *conf_v1.VirtualServerRoute) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.serviceReferenceChecker.IsReferencedByVirtualServerRoute(svcNamespace, svcName, vsr)
}

// IsServiceReferencedByIngress checks if the specified service is referenced by the Ingress.
func (c *Configuration) IsServiceReferencedByIngress(svcNamespace, svcName string, ing *networking.Ingress) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.serviceReferenceChecker.IsReferencedByIngress(svcNamespace, svcName, ing)
}

// IsServiceReferencedByMinion checks if the specified service is referenced by the minion Ingress.
func (c *Configuration) IsServiceReferencedByMinion(svcNamespace, svcName string, ing *networking.Ingress) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.serviceReferenceChecker.IsReferencedByMinion(svcNamespace, svcName, ing)
}

// FindResourcesForEndpoints finds resources that reference the specified endpoints.
func (c *Configuration) FindResourcesForEndpoints(endpointsNamespace string, endpointsName string) []Resource {
	// Resources reference not endpoints but the corresponding service, which has the same namespace and name
	return c.findResourcesForResourceReference(endpointsNamespace, endpointsName, c.endpointReferenceChecker)
}

// FindResourcesForSecret finds resources that reference the specified secret.
func (c *Configuration) FindResourcesForSecret(secretNamespace string, secretName string) []Resource {
	return c.findResourcesForResourceReference(secretNamespace, secretName, c.secretReferenceChecker)
}

// FindResourcesForPolicy finds resources that reference the specified policy.
func (c *Configuration) FindResourcesForPolicy(policyNamespace string, policyName string) []Resource {
	return c.findResourcesForResourceReference(policyNamespace, policyName, c.policyReferenceChecker)
}

// UpdatePolicyServiceRef tracks an external auth service reference for a policy.
// This allows service/endpoint changes to be correlated back to VirtualServers
// that reference the auth service via the policy.
func (c *Configuration) UpdatePolicyServiceRef(policyNamespace, policyName, authServiceName string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.serviceReferenceChecker.policyServices[policyNamespace+"/"+policyName] = authServiceName
}

// DeletePolicyServiceRef removes the external auth service reference tracking for a policy.
func (c *Configuration) DeletePolicyServiceRef(policyNamespace, policyName string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.serviceReferenceChecker.policyServices, policyNamespace+"/"+policyName)
}

// FindResourcesForAppProtectPolicyAnnotation finds resources that reference the specified AppProtect policy via annotation.
func (c *Configuration) FindResourcesForAppProtectPolicyAnnotation(policyNamespace string, policyName string) []Resource {
	return c.findResourcesForResourceReference(policyNamespace, policyName, c.appPolicyReferenceChecker)
}

// FindResourcesForAppProtectLogConfAnnotation finds resources that reference the specified AppProtect LogConf.
func (c *Configuration) FindResourcesForAppProtectLogConfAnnotation(logConfNamespace string, logConfName string) []Resource {
	return c.findResourcesForResourceReference(logConfNamespace, logConfName, c.appLogConfReferenceChecker)
}

// FindResourcesForAppProtectDosProtected finds resources that reference the specified AppProtectDos DosLogConf.
func (c *Configuration) FindResourcesForAppProtectDosProtected(namespace string, name string) []Resource {
	return c.findResourcesForResourceReference(namespace, name, c.appDosProtectedChecker)
}

// FindIngressesWithRatelimitScaling finds ingresses that use rate limit scaling
func (c *Configuration) FindIngressesWithRatelimitScaling(svcNamespace string) []Resource {
	return c.findResourcesForResourceReference(svcNamespace, "", &ratelimitScalingAnnotationChecker{})
}

func (c *Configuration) findResourcesForResourceReference(namespace string, name string, checker resourceReferenceChecker) []Resource {
	c.lock.RLock()
	defer c.lock.RUnlock()

	var result []Resource
	// an Ingress is there once for each of its hosts, but is checked once
	checked := make(map[Resource]bool)

	for _, h := range getSortedResourceKeys(c.hosts) {
		r := c.hosts[h]
		if checked[r] {
			continue
		}
		checked[r] = true

		switch impl := r.(type) {
		case *IngressConfiguration:
			if checker.IsReferencedByIngress(namespace, name, impl.Ingress) {
				result = append(result, r)
				continue
			}

			found := false
			for _, fm := range impl.Minions {
				if checker.IsReferencedByMinion(namespace, name, fm.Ingress) {
					result = append(result, r)
					found = true
					break
				}
			}
			if found {
				continue
			}

			// The services of canary Ingresses are upstreams of the Ingress they are attached to.
			for _, canary := range impl.Canaries {
				if checker.IsReferencedByIngress(namespace, name, canary) {
					result = append(result, r)
					found = true
					break
				}
			}
			if found {
				continue
			}

			// So are the services and Secrets of the Ingresses merged into it.
			for _, merged := range impl.Merged {
				if checker.IsReferencedByIngress(namespace, name, merged.Ingress) {
					result = append(result, r)
					break
				}
			}
		case *VirtualServerConfiguration:
			if checker.IsReferencedByVirtualServer(namespace, name, impl.VirtualServer) {
				result = append(result, r)
				continue
			}

			found := false
			for _, vsr := range impl.VirtualServerRoutes {
				if checker.IsReferencedByVirtualServerRoute(namespace, name, vsr) {
					result = append(result, r)
					found = true
					break
				}
			}
			if found {
				continue
			}

			// ACME challenge routes reference only their solver Service. Match them for Service/Endpoints
			// lookups so solver endpoint changes reach the challenge upstream; never for other reference kinds.
			if _, isServiceChecker := checker.(*serviceReferenceChecker); isServiceChecker {
				for _, cr := range impl.ChallengeRoutes {
					if checker.IsReferencedByVirtualServerRoute(namespace, name, cr) {
						result = append(result, r)
						break
					}
				}
			}
		case *TransportServerConfiguration:
			if checker.IsReferencedByTransportServer(namespace, name, impl.TransportServer) {
				result = append(result, r)
				continue
			}
		}
	}

	for _, lh := range getSortedListenerHostKeys(c.listenerHosts) {
		tsConfig := c.listenerHosts[lh]

		if checker.IsReferencedByTransportServer(namespace, name, tsConfig.TransportServer) {
			result = append(result, tsConfig)
			continue
		}
	}

	return result
}

func getResourceKey(meta *metav1.ObjectMeta) string {
	return fmt.Sprintf("%s/%s", meta.Namespace, meta.Name)
}

// rebuildHosts rebuilds the Configuration and returns the changes to it and the new problems.
func (c *Configuration) rebuildHosts() ([]ResourceChange, []ConfigurationProblem) {
	newHosts, newResources, newVSRToVSConfigs := c.buildHostsAndResources()

	updateActiveHostsForIngresses(newHosts, newResources)

	removedHosts, updatedHosts, addedHosts := detectChangesInHosts(c.hosts, newHosts)
	changes := createResourceChangesForHosts(removedHosts, updatedHosts, addedHosts, c.hosts, newHosts)

	c.vsrsWithChangedRefs = detectChangesInVSRReferences(c.vsrToVSConfigs, newVSRToVSConfigs, c.virtualServerRoutes)

	// Retain the previous hosts so warning-only diffs can be detected after
	// listener warnings are attached to newResources.
	oldHosts := c.hosts

	// safe to update hosts
	c.hosts = newHosts
	c.vsrToVSConfigs = newVSRToVSConfigs

	changes = squashResourceChanges(changes)

	// Note that the change will not refer to the latest version, if the resource is being removed.
	// However, referring to the latest version is necessary so that the resource latest Warnings are reported and not lost.
	// So here we make sure that changes always refer to the latest version of resources.
	for i := range changes {
		key := changes[i].Resource.GetKeyWithKind()
		if r, exists := newResources[key]; exists {
			changes[i].Resource = r
		}
	}
	newProblems := make(map[string]ConfigurationProblem)

	c.addProblemsForResourcesWithoutActiveHost(newResources, newProblems)
	c.addProblemsForOrphanMinions(newProblems)
	c.addProblemsForCanaries(newProblems)
	c.addProblemsForOrphanOrIgnoredVsrs(newProblems)
	c.addWarningsForVirtualServersWithMissConfiguredListeners(newResources)

	// Report VirtualServer warning-set changes even when IsEqual skipped a reload.
	// IsEqual is the reload predicate and intentionally ignores Warnings; reporting
	// must not depend on that predicate or rejected resources stay silently Valid.
	changes = append(changes, createVirtualServerWarningChanges(oldHosts, newHosts, changes)...)

	newOrUpdatedProblems := detectChangesInProblems(newProblems, c.hostProblems)

	// safe to update problems
	c.hostProblems = newProblems

	return changes, newOrUpdatedProblems
}

func updateActiveHostsForIngresses(hosts map[string]Resource, resources map[string]Resource) {
	for _, r := range resources {
		ingConfig, ok := r.(*IngressConfiguration)
		if !ok {
			continue
		}

		for _, rule := range ingConfig.Ingress.Spec.Rules {
			res := hosts[rule.Host]
			ingConfig.ValidHosts[rule.Host] = res.GetKeyWithKind() == r.GetKeyWithKind()
		}
	}
}

func detectChangesInProblems(newProblems map[string]ConfigurationProblem, oldProblems map[string]ConfigurationProblem) []ConfigurationProblem {
	var result []ConfigurationProblem

	for _, key := range getSortedProblemKeys(newProblems) {
		newP := newProblems[key]

		oldP, exists := oldProblems[key]
		if !exists {
			result = append(result, newP)
			continue
		}

		if !compareConfigurationProblems(&newP, &oldP) {
			result = append(result, newP)
		}
	}

	return result
}

func (c *Configuration) addProblemsForTSConfigsWithoutActiveListener(
	tsConfigs map[string]*TransportServerConfiguration,
	problems map[string]ConfigurationProblem,
) {
	for _, tsc := range tsConfigs {
		listenerName := tsc.TransportServer.Spec.Listener.Name
		host := tsc.TransportServer.Spec.Host
		hostDescription := "empty host"
		if host != "" {
			hostDescription = host
		}
		key := listenerHostKey{ListenerName: listenerName, Host: host}
		holder, exists := c.listenerHosts[key]
		if !exists {
			p := ConfigurationProblem{
				Object:  tsc.TransportServer,
				IsError: false,
				Reason:  nl.EventReasonRejected,
				Message: fmt.Sprintf("Listener %s doesn't exist", listenerName),
			}
			problems[tsc.GetKeyWithKind()] = p
			continue
		}

		if !tsc.IsEqual(holder) {
			p := ConfigurationProblem{
				Object:  tsc.TransportServer,
				IsError: false,
				Reason:  nl.EventReasonRejected,
				Message: fmt.Sprintf("Listener %s with host %s is taken by another resource", listenerName, hostDescription),
			}
			problems[tsc.GetKeyWithKind()] = p
		}
	}
}

func (c *Configuration) addProblemsForResourcesWithoutActiveHost(resources map[string]Resource, problems map[string]ConfigurationProblem) {
	merged := mergedIngressKeys(c.hosts)
	for _, r := range resources {
		switch impl := r.(type) {
		case *IngressConfiguration:
			atLeastOneValidHost := false
			for _, v := range impl.ValidHosts {
				if v {
					atLeastOneValidHost = true
					break
				}
			}
			if !atLeastOneValidHost && !allHostsMerged(impl, merged[r.GetKeyWithKind()]) {
				p := ConfigurationProblem{
					Object:  impl.Ingress,
					IsError: false,
					Reason:  nl.EventReasonRejected,
					Message: "All hosts are taken by other resources",
				}
				problems[r.GetKeyWithKind()] = p
			}
		case *VirtualServerConfiguration:
			res := c.hosts[virtualServerHostKey(impl.VirtualServer)]

			if res.GetKeyWithKind() != r.GetKeyWithKind() {
				p := ConfigurationProblem{
					Object:  impl.VirtualServer,
					IsError: false,
					Reason:  nl.EventReasonRejected,
					Message: "Host is taken by another resource",
				}
				problems[r.GetKeyWithKind()] = p
			}
		case *TransportServerConfiguration:
			res := c.hosts[impl.TransportServer.Spec.Host]

			if res.GetKeyWithKind() != r.GetKeyWithKind() {
				p := ConfigurationProblem{
					Object:  impl.TransportServer,
					IsError: false,
					Reason:  nl.EventReasonRejected,
					Message: "Host is taken by another resource",
				}
				problems[r.GetKeyWithKind()] = p
			}
		}
	}
}

// allHostsMerged tells if every host of the Ingress is merged into the server of another Ingress.
func allHostsMerged(ingConfig *IngressConfiguration, mergedHosts map[string]bool) bool {
	if len(mergedHosts) == 0 {
		return false
	}
	for host := range ingConfig.ValidHosts {
		if !mergedHosts[host] {
			return false
		}
	}
	return true
}

func (c *Configuration) addWarningsForVirtualServersWithMissConfiguredListeners(resources map[string]Resource) {
	// Sorted so that VirtualServers sharing a host append their warnings in a
	// stable order; otherwise slices.Equal on the warnings would flap between
	// rebuilds and emit spurious UpdateStatus changes.
	for _, key := range getSortedResourceKeys(resources) {
		vsc, ok := resources[key].(*VirtualServerConfiguration)
		if !ok || gateway.IsGatewayVirtualServer(vsc.VirtualServer) {
			continue
		}
		if vsc.VirtualServer.Spec.Listener != nil {
			if c.globalConfiguration == nil {
				warningMsg := "Listeners defined, but no GlobalConfiguration is deployed"
				c.hosts[vsc.VirtualServer.Spec.Host].AddWarning(warningMsg)
				continue
			}

			if !c.isListenerInCorrectBlock(vsc.VirtualServer.Spec.Listener.HTTP, false) {
				warningMsg := fmt.Sprintf("Listener %s can't be use in `listener.http` context as SSL is enabled for that listener.",
					vsc.VirtualServer.Spec.Listener.HTTP)
				c.hosts[vsc.VirtualServer.Spec.Host].AddWarning(warningMsg)
				continue
			}

			if !c.isListenerInCorrectBlock(vsc.VirtualServer.Spec.Listener.HTTPS, true) {
				warningMsg := fmt.Sprintf("Listener %s can't be use in `listener.https` context as SSL is not enabled for that listener.",
					vsc.VirtualServer.Spec.Listener.HTTPS)
				c.hosts[vsc.VirtualServer.Spec.Host].AddWarning(warningMsg)
				continue
			}

			if vsc.VirtualServer.Spec.Listener.HTTP != "" {
				if _, exists := c.listenerMap[vsc.VirtualServer.Spec.Listener.HTTP]; !exists {
					warningMsg := fmt.Sprintf("Listener %s is not defined in GlobalConfiguration",
						vsc.VirtualServer.Spec.Listener.HTTP)
					c.hosts[vsc.VirtualServer.Spec.Host].AddWarning(warningMsg)
					continue
				}
			}

			if vsc.VirtualServer.Spec.Listener.HTTPS != "" {
				if _, exists := c.listenerMap[vsc.VirtualServer.Spec.Listener.HTTPS]; !exists {
					warningMsg := fmt.Sprintf("Listener %s is not defined in GlobalConfiguration",
						vsc.VirtualServer.Spec.Listener.HTTPS)
					c.hosts[vsc.VirtualServer.Spec.Host].AddWarning(warningMsg)
					continue
				}
			}
		}
	}
}

func (c *Configuration) isListenerInCorrectBlock(listenerName string, expectedSsl bool) bool {
	if listener, ok := c.listenerMap[listenerName]; listener.Ssl != expectedSsl && ok {
		return false
	}
	return true
}

func (c *Configuration) addProblemsForOrphanMinions(problems map[string]ConfigurationProblem) {
	// Iterate only over indexed minions instead of all ingresses.
	for host, minionKeys := range c.minionsByHost {
		r, exists := c.hosts[host]
		ingressConf, ok := r.(*IngressConfiguration)
		hasMaster := exists && ok && ingressConf.IsMaster

		if hasMaster {
			continue
		}

		for key := range minionKeys {
			ing, ingExists := c.ingresses[key]
			if !ingExists {
				continue
			}
			p := ConfigurationProblem{
				Object:  ing,
				IsError: false,
				Reason:  nl.EventReasonNoIngressMasterFound,
				Message: "Ingress master is invalid or doesn't exist",
			}
			k := getResourceKeyWithKind(ingressKind, &ing.ObjectMeta)
			problems[k] = p
		}
	}
}

// GetVirtualServersForVirtualServerRoute returns VirtualServers accepting the VSR in sorted VS-key order.
func (c *Configuration) GetVirtualServersForVirtualServerRoute(vsr *conf_v1.VirtualServerRoute) []*conf_v1.VirtualServer {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.vsrToVSConfigs[getResourceKey(&vsr.ObjectMeta)]
}

// GetVirtualServerRoutesWithChangedReferences returns VSRs whose referencing VS set changed in the latest rebuild.
func (c *Configuration) GetVirtualServerRoutesWithChangedReferences() []*conf_v1.VirtualServerRoute {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.vsrsWithChangedRefs
}

// addProblemsForOrphanOrIgnoredVsrs emits orphan and ignored-route problems for VSRs.
func (c *Configuration) addProblemsForOrphanOrIgnoredVsrs(problems map[string]ConfigurationProblem) {
	for _, key := range getSortedVirtualServerRouteKeys(c.virtualServerRoutes) {
		vsr := c.virtualServerRoutes[key]
		vsrKey := getResourceKey(&vsr.ObjectMeta)
		k := getResourceKeyWithKind(virtualServerRouteKind, &vsr.ObjectMeta)

		if vsr.Spec.Host != "" {
			// Host-based VSR: use the host owner to report when its VirtualServer ignores the route.
			r, exists := c.hosts[vsr.Spec.Host]
			vsConfig, ok := r.(*VirtualServerConfiguration)

			if !exists || !ok {
				problems[k] = ConfigurationProblem{
					Object:  vsr,
					IsError: false,
					Reason:  nl.EventReasonNoVirtualServerFound,
					Message: "VirtualServer is invalid or doesn't exist",
				}
				continue
			}

			found := false
			for _, v := range vsConfig.VirtualServerRoutes {
				if vsr.Namespace == v.Namespace && vsr.Name == v.Name {
					found = true
					break
				}
			}
			if !found {
				problems[k] = ConfigurationProblem{
					Object:  vsr,
					IsError: false,
					Reason:  nl.EventReasonIgnored,
					Message: fmt.Sprintf("VirtualServer %s ignores VirtualServerRoute", getResourceKey(&vsConfig.VirtualServer.ObjectMeta)),
				}
			}
		} else {
			// Hostless VSR: use the O(1) reverse index built in buildHostsAndResources.
			// A VSR with no accepted VS is an orphan; accepted by ≥1 VS means no problem.
			if len(c.vsrToVSConfigs[vsrKey]) == 0 {
				problems[k] = ConfigurationProblem{
					Object:  vsr,
					IsError: false,
					Reason:  nl.EventReasonNoVirtualServerFound,
					Message: "VirtualServer is invalid or doesn't exist",
				}
			}
		}
	}
}

func getResourceKeyWithKind(kind string, objectMeta *metav1.ObjectMeta) string {
	return fmt.Sprintf("%s/%s/%s", kind, objectMeta.Namespace, objectMeta.Name)
}

func createResourceChangesForHosts(removedHosts []string, updatedHosts []string, addedHosts []string, oldHosts map[string]Resource, newHosts map[string]Resource) []ResourceChange {
	var changes []ResourceChange
	var deleteChanges []ResourceChange

	for _, h := range removedHosts {
		change := ResourceChange{
			Op:       Delete,
			Resource: oldHosts[h],
		}
		deleteChanges = append(deleteChanges, change)
	}

	for _, h := range updatedHosts {
		if oldHosts[h].GetKeyWithKind() != newHosts[h].GetKeyWithKind() {
			deleteChange := ResourceChange{
				Op:       Delete,
				Resource: oldHosts[h],
			}
			deleteChanges = append(deleteChanges, deleteChange)
		}

		change := ResourceChange{
			Op:       AddOrUpdate,
			Resource: newHosts[h],
		}
		changes = append(changes, change)
	}

	for _, h := range addedHosts {
		change := ResourceChange{
			Op:       AddOrUpdate,
			Resource: newHosts[h],
		}
		changes = append(changes, change)
	}

	// We need to ensure that delete changes come first.
	// This way an addOrUpdate change, which might include a resource that uses the same host as a resource
	// in a delete change, will be processed only after the config of the delete change is removed.
	// That will prevent any host collisions in the NGINX config in the state between the changes.
	return append(deleteChanges, changes...)
}

// createVirtualServerWarningChanges emits UpdateStatus changes for VirtualServers
// whose warning set changed while IsEqual still considers the resource unchanged.
// Resources already present in existing changes are skipped so a reload (or
// delete) remains the single reporting path for that object.
func createVirtualServerWarningChanges(oldHosts map[string]Resource, newHosts map[string]Resource, existing []ResourceChange) []ResourceChange {
	alreadyChanged := make(map[string]struct{}, len(existing))
	for _, c := range existing {
		alreadyChanged[c.Resource.GetKeyWithKind()] = struct{}{}
	}

	var changes []ResourceChange

	for _, h := range getSortedResourceKeys(newHosts) {
		newVSC, ok := newHosts[h].(*VirtualServerConfiguration)
		if !ok {
			continue
		}

		key := newVSC.GetKeyWithKind()
		if _, skip := alreadyChanged[key]; skip {
			continue
		}

		oldR, exists := oldHosts[h]
		if !exists {
			continue
		}
		oldVSC, ok := oldR.(*VirtualServerConfiguration)
		if !ok {
			continue
		}

		if !slices.Equal(oldVSC.Warnings, newVSC.Warnings) {
			changes = append(changes, ResourceChange{
				Op:       UpdateStatus,
				Resource: newVSC,
			})
		}
	}

	return changes
}

func createResourceChangesForListeners(
	removedListeners []listenerHostKey,
	updatedListeners []listenerHostKey,
	addedListeners []listenerHostKey,
	oldListeners map[listenerHostKey]*TransportServerConfiguration,
	newListeners map[listenerHostKey]*TransportServerConfiguration,
) []ResourceChange {
	var changes []ResourceChange
	var deleteChanges []ResourceChange

	for _, l := range removedListeners {
		change := ResourceChange{
			Op:       Delete,
			Resource: oldListeners[l],
		}
		deleteChanges = append(deleteChanges, change)
	}

	for _, l := range updatedListeners {
		if oldListeners[l].GetKeyWithKind() != newListeners[l].GetKeyWithKind() {
			deleteChange := ResourceChange{
				Op:       Delete,
				Resource: oldListeners[l],
			}
			deleteChanges = append(deleteChanges, deleteChange)
		}

		change := ResourceChange{
			Op:       AddOrUpdate,
			Resource: newListeners[l],
		}
		changes = append(changes, change)
	}

	for _, l := range addedListeners {
		change := ResourceChange{
			Op:       AddOrUpdate,
			Resource: newListeners[l],
		}
		changes = append(changes, change)
	}

	// We need to ensure that delete changes come first.
	// This way an addOrUpdate change, which might include a resource that uses the same listener as a resource
	// in a delete change, will be processed only after the config of the delete change is removed.
	// That will prevent any listener collisions in the NGINX config in the state between the changes.
	return append(deleteChanges, changes...)
}

func squashResourceChanges(changes []ResourceChange) []ResourceChange {
	// deletes for the same resource become a single delete
	// updates for the same resource become a single update
	// delete and update for the same resource become a single update

	var deletes []ResourceChange
	var updates []ResourceChange

	changesPerResource := make(map[string][]ResourceChange)

	for _, c := range changes {
		key := c.Resource.GetKeyWithKind()
		changesPerResource[key] = append(changesPerResource[key], c)
	}

	// we range over the changes again to preserver the original order
	for _, c := range changes {
		key := c.Resource.GetKeyWithKind()
		resChanges, exists := changesPerResource[key]

		if !exists {
			continue
		}

		// the last element will be an update (if it exists) or a delete
		squashedChanged := resChanges[len(resChanges)-1]
		if squashedChanged.Op == Delete {
			deletes = append(deletes, squashedChanged)
		} else {
			updates = append(updates, squashedChanged)
		}

		delete(changesPerResource, key)
	}

	// We need to ensure that delete changes come first.
	// This way an addOrUpdate change, which might include a resource that uses the same host/listener as a resource
	// in a delete change, will be processed only after the config of the delete change is removed.
	// That will prevent any host/listener collisions in the NGINX config in the state between the changes.
	return append(deletes, updates...)
}

//nolint:gocyclo // complexity is pre-existing; refactoring planned as a follow-up
func (c *Configuration) buildHostsAndResources() (newHosts map[string]Resource, newResources map[string]Resource, vsrToVSConfigs map[string][]*conf_v1.VirtualServer) {
	newHosts = make(map[string]Resource)
	newResources = make(map[string]Resource)
	vsrToVSConfigs = make(map[string][]*conf_v1.VirtualServer)
	var challengesVSR []*conf_v1.VirtualServerRoute

	// Step 1 - Build hosts from Ingress resources. Regular Ingresses that
	// share a host are merged into the server of the one that wins it, as
	// ingress-nginx merges them, unless another resource wins the host.

	regularByHost := make(map[string][]*IngressConfiguration)

	for _, key := range getSortedIngressKeys(c.ingresses) {
		ing := c.ingresses[key]

		if isMinion(ing) || configs.IsCanaryIngress(ing) {
			continue
		}

		var resource *IngressConfiguration

		if val := c.isChallengeIngress(ing); val {
			vsr := c.convertIngressToVSR(ing)
			if vsr != nil {
				challengesVSR = append(challengesVSR, vsr)
				continue
			}
		}

		if isMaster(ing) {
			minions, childWarnings := c.buildMinionConfigs(ing.Spec.Rules[0].Host)
			resource = NewMasterIngressConfiguration(ing, minions, childWarnings)
		} else {
			resource = NewRegularIngressConfiguration(ing)
		}

		newResources[resource.GetKeyWithKind()] = resource

		for _, rule := range ing.Spec.Rules {
			if !resource.IsMaster && !slices.Contains(regularByHost[rule.Host], resource) {
				regularByHost[rule.Host] = append(regularByHost[rule.Host], resource)
			}

			holder, exists := newHosts[rule.Host]
			if !exists {
				newHosts[rule.Host] = resource
				continue
			}
			if holder == Resource(resource) {
				continue
			}

			// regular Ingresses that share a host are merged below, without a warning
			holderConfig, holderIsIngress := holder.(*IngressConfiguration)
			merge := !resource.IsMaster && holderIsIngress && !holderConfig.IsMaster

			warning := fmt.Sprintf("host %s is taken by another resource", rule.Host)

			if !holder.Wins(resource) {
				if !merge {
					holder.AddWarning(warning)
				}
				newHosts[rule.Host] = resource
			} else if !merge {
				resource.AddWarning(warning)
			}
		}
	}
	ingressHolders := make(map[string]Resource)
	for host := range regularByHost {
		ingressHolders[host] = newHosts[host]
	}

	// Step 2 - Build hosts from VirtualServer resources

	for _, key := range getSortedVirtualServerKeys(c.virtualServers) {
		vs := c.virtualServers[key]

		vsrs, vsrSelectors, warnings := c.buildVirtualServerRoutes(vs)
		resource := NewVirtualServerConfiguration(vs, vsrs, vsrSelectors, warnings)

		// Challenge routes are kept out of VirtualServerRoutes so they never enter the VSR->VS index or VSR status updates.
		for _, vsr := range challengesVSR {
			if vs.Spec.Host == vsr.Spec.Host && vs.Namespace == vsr.Namespace {
				resource.ChallengeRoutes = append(resource.ChallengeRoutes, vsr)
			}
		}

		c.buildListenersForVSConfiguration(resource)

		newResources[resource.GetKeyWithKind()] = resource

		// Index accepted VirtualServers by VSR key in sorted VS order.
		for _, vsr := range resource.VirtualServerRoutes {
			vsrKey := getResourceKey(&vsr.ObjectMeta)
			vsrToVSConfigs[vsrKey] = append(vsrToVSConfigs[vsrKey], vs)
		}

		hostKey := virtualServerHostKey(vs)
		holder, exists := newHosts[hostKey]
		if !exists {
			newHosts[hostKey] = resource
			continue
		}

		warning := fmt.Sprintf("host %s is taken by another resource", vs.Spec.Host)

		if !holder.Wins(resource) {
			newHosts[hostKey] = resource
			holder.AddWarning(warning)
		} else {
			resource.AddWarning(warning)
		}
	}

	// Step - 3 - Build hosts from TransportServer resources if TLS Passthrough is enabled

	if c.isTLSPassthroughEnabled {
		for _, key := range getSortedTransportServerKeys(c.transportServers) {
			ts := c.transportServers[key]

			if ts.Spec.Listener.Name != conf_v1.TLSPassthroughListenerName && ts.Spec.Listener.Protocol != conf_v1.TLSPassthroughListenerProtocol {
				continue
			}

			resource := NewTransportServerConfiguration(ts)
			newResources[resource.GetKeyWithKind()] = resource

			holder, exists := newHosts[ts.Spec.Host]
			if !exists {
				newHosts[ts.Spec.Host] = resource
				continue
			}

			warning := fmt.Sprintf("host %s is taken by another resource", ts.Spec.Host)

			if !holder.Wins(resource) {
				newHosts[ts.Spec.Host] = resource
				holder.AddWarning(warning)
			} else {
				resource.AddWarning(warning)
			}
		}
	}

	// Step 4 - Merge the regular Ingresses of a host into the one that won it

	mergeIngressesOfHosts(newHosts, regularByHost, ingressHolders, c.crossNamespace)

	// Step 5 - Attach canary Ingresses to the Ingresses that won their hosts

	c.attachCanaries(newHosts)

	// Step 6 - Leave out the server aliases that are hosts of other resources or aliases of older Ingresses

	dropTakenServerAliases(newHosts)

	return newHosts, newResources, vsrToVSConfigs
}

// mergeIngressesOfHosts merges the regular Ingresses of each host into the
// regular Ingress that holds it, oldest first. Where a VirtualServer or a
// TransportServer took the host from the regular Ingress that held it after
// the Ingresses, the others lose it as they would have lost it to that one,
// with the warning that one got. Unless crossNamespace, only the Ingresses of
// the holder's namespace are merged: those of other namespaces lose the host,
// with a warning that names neither the holder nor its namespace.
func mergeIngressesOfHosts(hosts map[string]Resource, regularByHost map[string][]*IngressConfiguration, ingressHolders map[string]Resource, crossNamespace bool) {
	for host, ingConfigs := range regularByHost {
		if len(ingConfigs) < 2 {
			continue
		}
		holder, isIngress := hosts[host].(*IngressConfiguration)
		if !isIngress || holder.IsMaster {
			warning := fmt.Sprintf("host %s is taken by another resource", host)
			for _, ingConfig := range ingConfigs {
				if Resource(ingConfig) != ingressHolders[host] && !slices.Contains(ingConfig.Warnings, warning) {
					ingConfig.AddWarning(warning)
				}
			}
			continue
		}

		var merged []*IngressConfiguration
		for _, ingConfig := range ingConfigs {
			if ingConfig == holder {
				continue
			}
			if !crossNamespace && ingConfig.Ingress.Namespace != holder.Ingress.Namespace {
				ingConfig.AddWarning(fmt.Sprintf("host %s is taken by an Ingress of another namespace", host))
				continue
			}
			merged = append(merged, ingConfig)
		}
		slices.SortFunc(merged, func(a, b *IngressConfiguration) int {
			if a.Wins(b) {
				return -1
			}
			return 1
		})
		for _, ingConfig := range merged {
			holder.Merged = append(holder.Merged, MergedIngress{Ingress: ingConfig.Ingress, Host: host})
		}
	}
	for _, r := range hosts {
		if ingConfig, ok := r.(*IngressConfiguration); ok && len(ingConfig.Merged) > 1 {
			slices.SortStableFunc(ingConfig.Merged, func(a, b MergedIngress) int { return strings.Compare(a.Host, b.Host) })
		}
	}
}

// mergedIngressKeys returns the keys of the Ingresses merged into others, by the hosts merged.
func mergedIngressKeys(hosts map[string]Resource) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	for _, r := range hosts {
		ingConfig, ok := r.(*IngressConfiguration)
		if !ok {
			continue
		}
		for _, merged := range ingConfig.Merged {
			key := getResourceKeyWithKind(ingressKind, &merged.Ingress.ObjectMeta)
			if result[key] == nil {
				result[key] = make(map[string]bool)
			}
			result[key][merged.Host] = true
		}
	}
	return result
}

// serverNameMatches tells if nginx would give the name to a server with the server name pattern: the same name, or
// a name under a wildcard pattern (*.example.com, which nginx does not match to example.com itself). Lower case.
func serverNameMatches(pattern, name string) bool {
	if pattern == name {
		return true
	}
	suffix, isWildcard := strings.CutPrefix(pattern, "*")
	return isWildcard && strings.HasPrefix(suffix, ".") && strings.HasSuffix(name, suffix) && len(name) > len(suffix)
}

// serverNamesCollide tells if a server with one name and a server with the other would compete for a request: the
// same name, or one a wildcard the other falls under (nginx prefers the exact name to the wildcard, and the longer
// wildcard to the shorter, so either may take requests away from the other).
func serverNamesCollide(a, b string) bool {
	return serverNameMatches(a, b) || serverNameMatches(b, a)
}

// dropTakenServerAliases leaves out of the servers of the Ingresses the server aliases that would take requests
// from another resource or from an older Ingress, as ingress-nginx skips an alias that is a host or an alias
// already: aliases rank below every host of every resource - Ingress, VirtualServer, TransportServer - so an alias
// that is such a host, matches it as a wildcard, or falls under its wildcard, is dropped, as is one that collides
// with an alias of an older Ingress. Every dropped alias is a warning of its Ingress.
func dropTakenServerAliases(hosts map[string]Resource) {
	type hostHolder struct {
		host   string
		holder Resource
	}
	var taken []hostHolder
	var holders []*IngressConfiguration
	for _, key := range getSortedResourceKeys(hosts) {
		r := hosts[key]
		// a VirtualServer of a Gateway has its host with the listener's ports
		host, _, _ := strings.Cut(key, " ")
		if host != "" {
			taken = append(taken, hostHolder{host: strings.ToLower(host), holder: r})
		}
		if ingConfig, ok := r.(*IngressConfiguration); ok && !slices.Contains(holders, ingConfig) {
			holders = append(holders, ingConfig)
		}
	}
	slices.SortFunc(holders, func(a, b *IngressConfiguration) int {
		if a == b {
			return 0
		}
		if a.Wins(b) {
			return -1
		}
		return 1
	})
	var claimed []string
	for _, ingConfig := range holders {
		for _, alias := range configs.IngressNginxServerAliases(ingConfig.Ingress) {
			alias = strings.ToLower(alias)
			own, other := false, false
			for _, t := range taken {
				if !serverNamesCollide(alias, t.host) {
					continue
				}
				if t.holder == Resource(ingConfig) && t.host == alias {
					own = true
				} else if t.holder != Resource(ingConfig) {
					other = true
				}
			}
			var warning string
			switch {
			case other:
				warning = fmt.Sprintf("server-alias %s is ignored: the host is taken by another resource", alias)
			case own:
				// a host of its own: the server has the name already
				continue
			case slices.ContainsFunc(claimed, func(c string) bool { return serverNamesCollide(alias, c) }):
				warning = fmt.Sprintf("server-alias %s is ignored: it is the server-alias of another Ingress", alias)
			default:
				claimed = append(claimed, alias)
				continue
			}
			if ingConfig.DroppedServerAliases == nil {
				ingConfig.DroppedServerAliases = make(map[string]bool)
			}
			ingConfig.DroppedServerAliases[alias] = true
			ingConfig.AddWarning(warning)
		}
	}
}

// ingressForServer is the Ingress of the config without the server aliases that dropTakenServerAliases dropped.
func (ic *IngressConfiguration) ingressForServer() *networking.Ingress {
	if len(ic.DroppedServerAliases) == 0 {
		return ic.Ingress
	}
	return configs.WithoutIngressNginxServerAliases(ic.Ingress, func(alias string) bool {
		return ic.DroppedServerAliases[strings.ToLower(alias)]
	})
}

// canaryError tells why a canary Ingress is ignored, if it is.
func (c *Configuration) canaryError(ing *networking.Ingress) error {
	if !c.isDynamicUpstreams {
		return errors.New("canary Ingresses need -dynamic-upstreams")
	}
	if _, err := configs.ParseCanaryAnnotations(ing); err != nil {
		return err
	}
	return nil
}

// getSortedCanaries returns the canary Ingresses that are not ignored, oldest first, as ingress-nginx orders them.
func (c *Configuration) getSortedCanaries() []*networking.Ingress {
	var canaries []*networking.Ingress
	for _, ing := range c.ingresses {
		if configs.IsCanaryIngress(ing) && c.canaryError(ing) == nil {
			canaries = append(canaries, ing)
		}
	}
	slices.SortFunc(canaries, func(a, b *networking.Ingress) int {
		if chooseObjectMetaWinner(&a.ObjectMeta, &b.ObjectMeta) {
			return -1
		}
		return 1
	})
	return canaries
}

// attachCanaries attaches each canary Ingress to the Ingress that has one of
// its hosts and, for that host, one of its paths with the same path type.
// The canary makes no server of its own and so takes no host.
func (c *Configuration) attachCanaries(hosts map[string]Resource) {
	if c.canaryPerUpstream {
		c.attachCanariesPerUpstream(hosts)
		return
	}
	for _, canary := range c.getSortedCanaries() {
		for _, rule := range canary.Spec.Rules {
			ingConfig, ok := hosts[rule.Host].(*IngressConfiguration)
			if !ok || rule.HTTP == nil || slices.Contains(ingConfig.Canaries, canary) {
				continue
			}
			if !c.crossNamespace && ingConfig.Ingress.Namespace != canary.Namespace {
				continue
			}
			if ingressConfigurationHasPath(ingConfig, rule.Host, rule.HTTP.Paths) {
				ingConfig.Canaries = append(ingConfig.Canaries, canary)
			}
		}
	}
}

// attachCanariesPerUpstream attaches the canaries as ingress-nginx merges its
// alternative backends: a canary's path finds the location with its host,
// path and path type, and the canary's backend becomes the canary of that
// location's upstream - ingress-nginx's backend of namespace, Service and
// port - so it holds for every path of every Ingress whose backend is that
// upstream, on any host. An upstream takes the first canary that names it,
// oldest first (ingress-nginx uses only its first alternative backend), and
// none whose backend is the upstream itself. Unless crossNamespace, a canary
// is the canary only of upstreams of its own namespace.
func (c *Configuration) attachCanariesPerUpstream(hosts map[string]Resource) {
	bindings := make(map[backendKey]CanaryBackend)
	for _, canary := range c.getSortedCanaries() {
		for _, rule := range canary.Spec.Rules {
			ingConfig, ok := hosts[rule.Host].(*IngressConfiguration)
			if !ok || rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service == nil {
					continue
				}
				primaryNamespace, primary := ingressConfigurationBackendForPath(ingConfig, rule.Host, path)
				if primary == nil {
					continue
				}
				if !c.crossNamespace && primaryNamespace != canary.Namespace {
					continue
				}
				upstream := newBackendKey(primaryNamespace, primary)
				if upstream == newBackendKey(canary.Namespace, &path.Backend) {
					continue
				}
				if _, taken := bindings[upstream]; !taken {
					bindings[upstream] = CanaryBackend{Ingress: canary, Backend: path.Backend}
				}
			}
		}
	}
	if len(bindings) == 0 {
		return
	}

	seen := make(map[*IngressConfiguration]bool)
	for _, r := range hosts {
		ingConfig, ok := r.(*IngressConfiguration)
		if !ok || seen[ingConfig] {
			continue
		}
		seen[ingConfig] = true
		for _, upstream := range ingressConfigurationUpstreams(ingConfig) {
			cb, ok := bindings[upstream.key]
			if !ok {
				continue
			}
			if ingConfig.CanaryBackends == nil {
				ingConfig.CanaryBackends = make(map[string]CanaryBackend)
			}
			ingConfig.CanaryBackends[upstream.name] = cb
			if !slices.Contains(ingConfig.Canaries, cb.Ingress) {
				ingConfig.Canaries = append(ingConfig.Canaries, cb.Ingress)
			}
		}
		// oldest first, as the canaries of a config are everywhere else
		slices.SortStableFunc(ingConfig.Canaries, func(a, b *networking.Ingress) int {
			if a == b {
				return 0
			}
			if chooseObjectMetaWinner(&a.ObjectMeta, &b.ObjectMeta) {
				return -1
			}
			return 1
		})
	}
}

// ingressConfigurationIngresses returns the Ingresses of the config whose paths are its locations, with the
// paths valid for each (nil for all): the Ingress or its minions, then those merged into it with their host.
func ingressConfigurationIngresses(ingConfig *IngressConfiguration) (ings []*networking.Ingress, validPaths []map[string]bool, onlyHost []string) {
	if ingConfig.IsMaster {
		for _, minion := range ingConfig.Minions {
			ings, validPaths, onlyHost = append(ings, minion.Ingress), append(validPaths, minion.ValidPaths), append(onlyHost, "")
		}
		return ings, validPaths, onlyHost
	}
	ings, validPaths, onlyHost = append(ings, ingConfig.Ingress), append(validPaths, nil), append(onlyHost, "")
	for _, merged := range ingConfig.Merged {
		ings, validPaths, onlyHost = append(ings, merged.Ingress), append(validPaths, nil), append(onlyHost, merged.Host)
	}
	return ings, validPaths, onlyHost
}

// ingressConfigurationBackendForPath returns the backend of the location of the config for the host, path and path
// type, and the namespace of its Ingress: of the first Ingress that has it, as the first one's path is the location.
func ingressConfigurationBackendForPath(ingConfig *IngressConfiguration, host string, canaryPath networking.HTTPIngressPath) (string, *networking.IngressBackend) {
	ings, validPaths, onlyHost := ingressConfigurationIngresses(ingConfig)
	for i, ing := range ings {
		if onlyHost[i] != "" && onlyHost[i] != host {
			continue
		}
		for _, rule := range ing.Spec.Rules {
			if rule.Host != host || rule.HTTP == nil {
				continue
			}
			for j := range rule.HTTP.Paths {
				p := &rule.HTTP.Paths[j]
				if validPaths[i] != nil && !validPaths[i][p.Path] {
					continue
				}
				if rootIfEmpty(p.Path) == rootIfEmpty(canaryPath.Path) && reflect.DeepEqual(p.PathType, canaryPath.PathType) {
					if p.Backend.Service == nil {
						return "", nil
					}
					return ing.Namespace, &p.Backend
				}
			}
		}
	}
	return "", nil
}

// backendKey is the upstream of ingress-nginx - namespace, Service, port - without the ambiguity of its name
// (<namespace>-<service>-<port>, the same for namespace a-b and Service c as for namespace a and Service b-c).
type backendKey struct {
	namespace, service, port string
}

func newBackendKey(namespace string, backend *networking.IngressBackend) backendKey {
	if backend == nil || backend.Service == nil {
		return backendKey{namespace: namespace}
	}
	return backendKey{namespace: namespace, service: backend.Service.Name, port: configs.GetBackendPortAsString(backend.Service.Port)}
}

// ingressUpstream is an upstream of a path of an Ingress: its key, and ingress-nginx's name of it.
type ingressUpstream struct {
	key  backendKey
	name string
}

// ingressConfigurationUpstreams returns the upstreams of the paths of the config, with ingress-nginx's names.
func ingressConfigurationUpstreams(ingConfig *IngressConfiguration) []ingressUpstream {
	var upstreams []ingressUpstream
	ings, validPaths, onlyHost := ingressConfigurationIngresses(ingConfig)
	for i, ing := range ings {
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil || (onlyHost[i] != "" && onlyHost[i] != rule.Host) {
				continue
			}
			for j := range rule.HTTP.Paths {
				p := &rule.HTTP.Paths[j]
				if p.Backend.Service == nil || (validPaths[i] != nil && !validPaths[i][p.Path]) {
					continue
				}
				u := ingressUpstream{key: newBackendKey(ing.Namespace, &p.Backend), name: configs.IngressNginxBackendName(ing.Namespace, &p.Backend)}
				if !slices.Contains(upstreams, u) {
					upstreams = append(upstreams, u)
				}
			}
		}
	}
	return upstreams
}

// ingressConfigurationHasPath tells if the Ingress or one of its minions has one of the paths for the host.
func ingressConfigurationHasPath(ingConfig *IngressConfiguration, host string, paths []networking.HTTPIngressPath) bool {
	hasPath := func(ing *networking.Ingress, validPaths map[string]bool) bool {
		for _, rule := range ing.Spec.Rules {
			if rule.Host != host || rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				if validPaths != nil && !validPaths[p.Path] {
					continue
				}
				for _, canaryPath := range paths {
					if rootIfEmpty(p.Path) == rootIfEmpty(canaryPath.Path) && reflect.DeepEqual(p.PathType, canaryPath.PathType) {
						return true
					}
				}
			}
		}
		return false
	}

	if !ingConfig.IsMaster {
		if hasPath(ingConfig.Ingress, nil) {
			return true
		}
		for _, merged := range ingConfig.Merged {
			if merged.Host == host && hasPath(merged.Ingress, nil) {
				return true
			}
		}
		return false
	}
	for _, minion := range ingConfig.Minions {
		if hasPath(minion.Ingress, minion.ValidPaths) {
			return true
		}
	}
	return false
}

// rootIfEmpty returns the path, or / for an empty one, as NGINX config has it.
func rootIfEmpty(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// addProblemsForCanaries adds a warning for every canary Ingress that is ignored or attached to no Ingress.
func (c *Configuration) addProblemsForCanaries(problems map[string]ConfigurationProblem) {
	attached := make(map[*networking.Ingress]bool)
	for _, r := range c.hosts {
		if ingConfig, ok := r.(*IngressConfiguration); ok {
			for _, canary := range ingConfig.Canaries {
				attached[canary] = true
			}
		}
	}

	for _, key := range getSortedIngressKeys(c.ingresses) {
		ing := c.ingresses[key]
		if !configs.IsCanaryIngress(ing) || attached[ing] {
			continue
		}

		message := "The canary has no Ingress with the same host, path and path type"
		if !c.crossNamespace {
			message += " in its namespace"
		}
		if err := c.canaryError(ing); err != nil {
			message = fmt.Sprintf("The canary is ignored: %v", err)
		}
		problems[getResourceKeyWithKind(ingressKind, &ing.ObjectMeta)] = ConfigurationProblem{
			Object:  ing,
			IsError: false,
			Reason:  nl.EventReasonIgnored,
			Message: message,
		}
	}
}

func (c *Configuration) isChallengeIngress(ing *networking.Ingress) bool {
	if !c.isCertManagerEnabled {
		return false
	}
	return ing.Labels["acme.cert-manager.io/http01-solver"] == "true"
}

// convertIngressToVSR converts a cert-manager HTTP-01 solver Ingress into a synthetic VirtualServerRoute.
// It returns nil, so the Ingress is handled as a regular Ingress, unless the Ingress path and backend
// service look like an ACME challenge and a VirtualServer in the same namespace owns the host.
// The subroute path is the raw Ingress path, regardless of PathType.
func (c *Configuration) convertIngressToVSR(ing *networking.Ingress) *conf_v1.VirtualServerRoute {
	rule := ing.Spec.Rules[0]
	path := rule.HTTP.Paths[0]

	if !configs.IsACMEChallengeLocation(path.Path, path.Backend.Service.Name) {
		return nil
	}

	if !c.isChallengeIngressOwnerVs(rule.Host, ing.Namespace) {
		return nil
	}

	vs := &conf_v1.VirtualServerRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ing.Namespace,
			Name:      ing.Name,
			// IsEqual compares challenge routes by generation, so an in-place solver Ingress update
			// must change it.
			Generation: ing.Generation,
		},
		Spec: conf_v1.VirtualServerRouteSpec{
			Host: rule.Host,
			Upstreams: []conf_v1.Upstream{
				{
					Name:    "challenge",
					Service: rule.HTTP.Paths[0].Backend.Service.Name,
					Port:    uint16(rule.HTTP.Paths[0].Backend.Service.Port.Number),
				},
			},
			Subroutes: []conf_v1.Route{
				{
					Path: rule.HTTP.Paths[0].Path,
					Action: &conf_v1.Action{
						Pass: "challenge",
					},
				},
			},
		},
	}

	return vs
}

func (c *Configuration) isChallengeIngressOwnerVs(host, namespace string) bool {
	for _, key := range getSortedVirtualServerKeys(c.virtualServers) {
		vs := c.virtualServers[key]
		if host == vs.Spec.Host && namespace == vs.Namespace {
			return true
		}
	}
	return false
}

func (c *Configuration) buildMinionConfigs(masterHost string) ([]*MinionConfiguration, map[string][]string) {
	var minionConfigs []*MinionConfiguration
	childWarnings := make(map[string][]string)
	paths := make(map[string]*MinionConfiguration)

	// Use the minionsByHost index for O(1) host lookup instead of scanning all ingresses.
	minionKeys := c.minionsByHost[masterHost]
	sortedKeys := make([]string, 0, len(minionKeys))
	for k := range minionKeys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	for _, minionKey := range sortedKeys {
		ingress, exists := c.ingresses[minionKey]
		if !exists {
			continue
		}

		minionConfig := NewMinionConfiguration(ingress)

		for _, p := range ingress.Spec.Rules[0].HTTP.Paths {
			holder, exists := paths[p.Path]
			if !exists {
				paths[p.Path] = minionConfig
				minionConfig.ValidPaths[p.Path] = true
				continue
			}

			warning := fmt.Sprintf("path %s is taken by another resource", p.Path)

			if !chooseObjectMetaWinner(&holder.Ingress.ObjectMeta, &ingress.ObjectMeta) {
				paths[p.Path] = minionConfig
				minionConfig.ValidPaths[p.Path] = true

				holder.ValidPaths[p.Path] = false
				key := getResourceKey(&holder.Ingress.ObjectMeta)
				childWarnings[key] = append(childWarnings[key], warning)
			} else {
				key := getResourceKey(&minionConfig.Ingress.ObjectMeta)
				childWarnings[key] = append(childWarnings[key], warning)
			}
		}

		minionConfigs = append(minionConfigs, minionConfig)
	}

	return minionConfigs, childWarnings
}

func (c *Configuration) validateVSRs(r *conf_v1.Route, vsHost, vsNamespace string) ([]*conf_v1.VirtualServerRoute, []string) {
	var vsrs []*conf_v1.VirtualServerRoute
	var warnings []string

	vsrKey := r.Route

	// if route is defined without a namespace, use the namespace of VirtualServer.
	if !nsutils.HasNamespace(vsrKey) {
		vsrKey = fmt.Sprintf("%s/%s", vsNamespace, r.Route)
	}

	vsr, exists := c.virtualServerRoutes[vsrKey]

	// if route is defined
	if !exists {
		warning := fmt.Sprintf("VirtualServerRoute %s doesn't exist or invalid", vsrKey)
		warnings = append(warnings, warning)
		return vsrs, warnings
	}

	err := c.virtualServerValidator.ValidateVirtualServerRouteForVirtualServer(vsr, vsHost, []string{r.Path})
	if err != nil {
		warning := fmt.Sprintf("VirtualServerRoute %s is invalid: %v", vsrKey, err)
		warnings = append(warnings, warning)
		return vsrs, warnings
	}

	vsrs = append(vsrs, vsr)
	return vsrs, warnings
}

func (c *Configuration) validateVSRSelectors(r *conf_v1.Route, vsHost string) ([]*conf_v1.VirtualServerRoute, map[string][]string, []string) {
	var vsrs []*conf_v1.VirtualServerRoute
	var warnings []string
	vsrSelectors := make(map[string][]string)

	selector := &metav1.LabelSelector{
		MatchLabels: r.RouteSelector.MatchLabels,
	}
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		warning := fmt.Sprintf("VirtualServerRoute LabelSelector %s is invalid: %v", selector, err)
		warnings = append(warnings, warning)
		return vsrs, vsrSelectors, warnings
	}

	selectorStr := sel.String()
	// Initialize the selector entry regardless of whether routes match
	if vsrSelectors[selectorStr] == nil {
		vsrSelectors[selectorStr] = make([]string, 0)
	}

	type matchedVSR struct {
		key string
		vsr *conf_v1.VirtualServerRoute
	}
	var matched []matchedVSR
	// Count label matches separately from the validated result set. A selector
	// that matches a VSR later rejected by ValidateVirtualServerRouteForVirtualServer
	// already emits an accurate per-VSR warning; treating an empty validated
	// set as "matched nothing" would add a spurious second warning.
	labelMatches := 0

	for vsrKey, vsr := range c.virtualServerRoutes {
		if sel.Matches(labels.Set(vsr.Labels)) {
			labelMatches++
			err := c.virtualServerValidator.ValidateVirtualServerRouteForVirtualServer(vsr, vsHost, []string{r.Path})
			if err != nil {
				warning := fmt.Sprintf("VirtualServerRoute %s is invalid: %v", vsrKey, err)
				warnings = append(warnings, warning)
				continue
			}
			matched = append(matched, matchedVSR{key: vsrKey, vsr: vsr})
		}
	}

	if labelMatches == 0 {
		warnings = append(warnings, fmt.Sprintf("VirtualServerRoute routeSelector %s matched no VirtualServerRoutes", selectorStr))
	}

	// The loop above ranges over a map, so the per-route "is invalid" warnings
	// arrive in a random order. Warnings are compared with slices.Equal when
	// deciding whether to emit an UpdateStatus, so a reorder alone would look
	// like a change and flood the status with differently ordered messages.
	sort.Strings(warnings)

	// Sort before building the output slices.  The vsrs slice ends up as
	// VirtualServerConfiguration.VirtualServerRoutes, which
	// GenerateVirtualServerConfig walks in order to assign split_clients
	// indices, upstream names and location ordering, and which
	// VirtualServerConfiguration.IsEqual compares positionally.  The loop above
	// ranges over a map, and Go randomizes map iteration order by design, so
	// this is the only place that ordering guarantee can be established.
	// Without it, an unchanged VirtualServer compares as changed and gets
	// needlessly re-rendered and reloaded.
	sort.Slice(matched, func(i, j int) bool { return matched[i].key < matched[j].key })

	for _, m := range matched {
		vsrs = append(vsrs, m.vsr)
		// Built in sorted order, so no separate sort of the tracking map is
		// needed.
		vsrSelectors[selectorStr] = append(vsrSelectors[selectorStr], m.key)
	}

	return vsrs, vsrSelectors, warnings
}

func validateDuplicateVSRPaths(vsrs []*conf_v1.VirtualServerRoute) ([]*conf_v1.VirtualServerRoute, []string) {
	var warnings []string

	paths := make(map[string]string)
	var vsrsToRemove []string

	for _, vsr := range vsrs {
		for _, subroute := range vsr.Spec.Subroutes {
			normPath := validation.NormalizePath(subroute.Path)
			if path, exists := paths[normPath]; exists {
				subRoutes := fmt.Sprintf("%s and %s", fmt.Sprintf("%s/%s", vsr.Namespace, vsr.Name), path)
				if fmt.Sprintf("%s/%s", vsr.Namespace, vsr.Name) == path {
					// both subroutes are from the same VSR
					subRoutes = path
				}
				pathWarning := fmt.Sprintf("path %s has conflicting subroutes on %s", subroute.Path, subRoutes)
				warnings = append(warnings, pathWarning)

				vsrsToRemove = append(vsrsToRemove, getResourceKeyWithKind(virtualServerRouteKind, &vsr.ObjectMeta))
			} else {
				paths[normPath] = fmt.Sprintf("%s/%s", vsr.Namespace, vsr.Name)
			}
		}
	}

	if len(vsrsToRemove) != 0 {
		for _, vsrToRemove := range vsrsToRemove {
			for i, vsr := range vsrs {
				if getResourceKeyWithKind(virtualServerRouteKind, &vsr.ObjectMeta) == vsrToRemove {
					vsrs = removeFromVSRSlice(vsrs, i)
					break
				}
			}
		}
	}
	return vsrs, warnings
}

func (c *Configuration) buildVirtualServerRoutes(vs *conf_v1.VirtualServer) ([]*conf_v1.VirtualServerRoute, map[string][]string, []string) {
	// Step 1: Single pass over VS routes — classify each route, collect regex
	// entries for deferred validation, eagerly validate non-regex/selector routes,
	// and deduplicate VSR references.
	collected := c.classifyAndCollectVSRs(vs)

	// Step 2: Validate regex VSRs with their full set of collected paths.
	regexVsrs, regexWarnings := c.validateAndBuildRegexVSRs(collected.regexEntries, vs.Spec.Host)
	vsrs := append(collected.vsrs, regexVsrs...)
	collected.warnings = append(collected.warnings, regexWarnings...)

	// Step 3: Remove any duplicate VSR identity that survived steps 1-2.
	// This can occur when a selector-matched VSR and a named regex route
	// reference the same VSR.
	vsrs, dupWarnings := deduplicateVSRSlice(vsrs, vs.Namespace, vs.Name)
	collected.warnings = append(collected.warnings, dupWarnings...)

	// Step 4: Remove VSRs whose subroutes have conflicting paths.
	vsrs, pathWarnings := validateDuplicateVSRPaths(vsrs)
	collected.warnings = append(collected.warnings, pathWarnings...)

	// Step 5: Sync vsrSelectors with the final VSR list.  Any VSR removed by
	// steps 3-4 must also be removed from vsrSelectors to keep the two structures
	// consistent.  Empty selector entries are preserved — they signal that the IC
	// should keep watching for label changes.
	syncVSRSelectors(collected.vsrSelectors, vsrs)

	return vsrs, collected.vsrSelectors, collected.warnings
}

// vsrCollection holds the results of classifyAndCollectVSRs.
type vsrCollection struct {
	vsrs         []*conf_v1.VirtualServerRoute
	regexEntries map[string]*regexVSREntry
	vsrSelectors map[string][]string
	warnings     []string
}

// regexVSREntry holds a VSR and all the regex VS paths that reference it.
type regexVSREntry struct {
	vsr          *conf_v1.VirtualServerRoute
	paths        []string // regex paths from VS routes
	firstSeenIdx int      // index in vs.Spec.Routes of the first route referencing this VSR
}

// classifyAndCollectVSRs makes a single pass over the VS routes to:
//   - collect regex named-route entries for deferred multi-path validation
//   - eagerly validate non-regex named routes and all selector routes
//   - deduplicate VSR references (same VSR from multiple routes/selectors)
func (c *Configuration) classifyAndCollectVSRs(vs *conf_v1.VirtualServer) vsrCollection {
	col := vsrCollection{
		regexEntries: make(map[string]*regexVSREntry),
		vsrSelectors: make(map[string][]string),
	}

	regexSeenPaths := make(map[string]map[string]struct{}) // per-VSR dedup of paths
	seenVSRs := make(map[string]bool)                      // VSR ns/name dedup

	for i, r := range vs.Spec.Routes {
		isRegex := strings.HasPrefix(r.Path, "~")

		switch {
		case r.Route != "" && isRegex:
			col.collectRegexNamedRoute(c, vs, r.Route, r.Path, i, regexSeenPaths)
		case r.Route != "" && !isRegex:
			col.collectNonRegexNamedRoute(c, vs, &r, seenVSRs)
		case r.RouteSelector != nil:
			col.collectSelectorRoute(c, vs, &r, seenVSRs)
		}
	}
	return col
}

// collectRegexNamedRoute adds a regex named-route entry for deferred validation.
// Any resulting duplicate is resolved by deduplicateVSRSlice later.
func (col *vsrCollection) collectRegexNamedRoute(
	c *Configuration, vs *conf_v1.VirtualServer,
	routeName, path string, routeIdx int,
	regexSeenPaths map[string]map[string]struct{},
) {
	normPath := validation.NormalizePath(path)
	vsrKey := routeName
	if !nsutils.HasNamespace(vsrKey) {
		vsrKey = fmt.Sprintf("%s/%s", vs.Namespace, routeName)
	}
	vsr, exists := c.virtualServerRoutes[vsrKey]
	if !exists {
		col.warnings = append(col.warnings, fmt.Sprintf("VirtualServerRoute %s doesn't exist or invalid", vsrKey))
		return
	}
	if entry, found := col.regexEntries[vsrKey]; found {
		if _, seen := regexSeenPaths[vsrKey][normPath]; !seen {
			regexSeenPaths[vsrKey][normPath] = struct{}{}
			entry.paths = append(entry.paths, path)
		}
	} else {
		col.regexEntries[vsrKey] = &regexVSREntry{vsr: vsr, paths: []string{path}, firstSeenIdx: routeIdx}
		regexSeenPaths[vsrKey] = map[string]struct{}{normPath: {}}
	}
}

// collectNonRegexNamedRoute validates a non-regex named route eagerly and adds
// the resulting VSR to the collection, deduplicating by namespace/name.
func (col *vsrCollection) collectNonRegexNamedRoute(
	c *Configuration, vs *conf_v1.VirtualServer,
	r *conf_v1.Route, seenVSRs map[string]bool,
) {
	validVsrs, vsrWarnings := c.validateVSRs(r, vs.Spec.Host, vs.Namespace)
	col.warnings = append(col.warnings, vsrWarnings...)
	for _, vsr := range validVsrs {
		nsName := fmt.Sprintf("%s/%s", vsr.Namespace, vsr.Name)
		if seenVSRs[nsName] {
			col.warnings = append(col.warnings, fmt.Sprintf(
				"VS %s/%s has duplicate VirtualServerRoutes %s", vs.Namespace, vs.Name, nsName,
			))
			continue
		}
		seenVSRs[nsName] = true
		col.vsrs = append(col.vsrs, vsr)
	}
}

// collectSelectorRoute validates a selector route eagerly and adds the
// resulting VSRs to the collection, deduplicating by namespace/name.
func (col *vsrCollection) collectSelectorRoute(
	c *Configuration, vs *conf_v1.VirtualServer,
	r *conf_v1.Route, seenVSRs map[string]bool,
) {
	validVsrs, selectors, vsrWarnings := c.validateVSRSelectors(r, vs.Spec.Host)
	col.warnings = append(col.warnings, vsrWarnings...)
	maps.Copy(col.vsrSelectors, selectors)
	for _, vsr := range validVsrs {
		nsName := fmt.Sprintf("%s/%s", vsr.Namespace, vsr.Name)
		if seenVSRs[nsName] {
			col.warnings = append(col.warnings, fmt.Sprintf(
				"VS %s/%s has duplicate VirtualServerRoutes %s", vs.Namespace, vs.Name, nsName,
			))
			continue
		}
		seenVSRs[nsName] = true
		col.vsrs = append(col.vsrs, vsr)
	}
}

// deduplicateVSRSlice removes subsequent occurrences of the same VSR (by
// namespace/name) from the slice, keeping the first occurrence and emitting a
// warning for each duplicate dropped.  This handles the rare case where a
// selector-matched VSR and a regex-named-route VSR turn out to be the same object.
func deduplicateVSRSlice(vsrs []*conf_v1.VirtualServerRoute, vsNamespace, vsName string) ([]*conf_v1.VirtualServerRoute, []string) {
	var result []*conf_v1.VirtualServerRoute
	var warnings []string
	seen := make(map[string]bool, len(vsrs))
	for _, vsr := range vsrs {
		nsName := fmt.Sprintf("%s/%s", vsr.Namespace, vsr.Name)
		if seen[nsName] {
			warnings = append(warnings, fmt.Sprintf(
				"VS %s/%s has duplicate VirtualServerRoutes %s", vsNamespace, vsName, nsName,
			))
			continue
		}
		seen[nsName] = true
		result = append(result, vsr)
	}
	return result, warnings
}

// syncVSRSelectors prunes vsrSelectors to only reference VSRs still present in
// the final list.  Empty selector entries are preserved for change-tracking.
func syncVSRSelectors(vsrSelectors map[string][]string, vsrs []*conf_v1.VirtualServerRoute) {
	finalKeys := make(map[string]bool, len(vsrs))
	for _, vsr := range vsrs {
		finalKeys[fmt.Sprintf("%s/%s", vsr.Namespace, vsr.Name)] = true
	}
	for sel, keys := range vsrSelectors {
		if len(keys) == 0 {
			continue
		}
		var kept []string
		for _, k := range keys {
			if finalKeys[k] {
				kept = append(kept, k)
			}
		}
		if kept == nil {
			vsrSelectors[sel] = []string{}
		} else {
			vsrSelectors[sel] = kept
		}
	}
}

func removeFromVSRSlice(s []*conf_v1.VirtualServerRoute, i int) []*conf_v1.VirtualServerRoute {
	return append(s[:i], s[i+1:]...)
}

func (c *Configuration) validateAndBuildRegexVSRs(entries map[string]*regexVSREntry, vsHost string) ([]*conf_v1.VirtualServerRoute, []string) {
	type entryWithKey struct {
		key   string
		entry *regexVSREntry
	}
	sorted := make([]entryWithKey, 0, len(entries))
	for k, e := range entries {
		sorted = append(sorted, entryWithKey{k, e})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].entry.firstSeenIdx < sorted[j].entry.firstSeenIdx
	})

	var vsrs []*conf_v1.VirtualServerRoute
	var warnings []string
	for _, ek := range sorted {
		err := c.virtualServerValidator.ValidateVirtualServerRouteForVirtualServer(ek.entry.vsr, vsHost, ek.entry.paths)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("VirtualServerRoute %s is invalid: %v", ek.key, err))
			continue
		}
		vsrs = append(vsrs, ek.entry.vsr)
	}
	return vsrs, warnings
}

// GetTransportServerMetrics returns metrics about TransportServers
func (c *Configuration) GetTransportServerMetrics() *TransportServerMetrics {
	var metrics TransportServerMetrics

	if c.isTLSPassthroughEnabled {
		for _, resource := range c.hosts {
			_, ok := resource.(*TransportServerConfiguration)
			if ok {
				metrics.TotalTLSPassthrough++
			}
		}
	}

	for _, tsConfig := range c.listenerHosts {
		if tsConfig.TransportServer.Spec.Listener.Protocol == "TCP" {
			metrics.TotalTCP++
		} else {
			metrics.TotalUDP++
		}
	}

	return &metrics
}

func (c *Configuration) setGlobalConfigListenerMap() {
	c.listenerMap = make(map[string]conf_v1.Listener)

	if c.globalConfiguration != nil {
		for _, listener := range c.globalConfiguration.Spec.Listeners {
			c.listenerMap[listener.Name] = listener
		}
	}
	for _, listener := range c.gatewayListeners {
		c.listenerMap[listener.Name] = listener
	}
}

func getSortedIngressKeys(m map[string]*networking.Ingress) []string {
	var keys []string

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func getSortedVirtualServerKeys(m map[string]*conf_v1.VirtualServer) []string {
	var keys []string

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func getSortedVirtualServerRouteKeys(m map[string]*conf_v1.VirtualServerRoute) []string {
	var keys []string

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func getSortedProblemKeys(m map[string]ConfigurationProblem) []string {
	var keys []string

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func getSortedResourceKeys(m map[string]Resource) []string {
	var keys []string

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func getSortedTransportServerKeys(m map[string]*conf_v1.TransportServer) []string {
	var keys []string

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func getSortedListenerHostKeys(m map[listenerHostKey]*TransportServerConfiguration) []listenerHostKey {
	var keys []listenerHostKey

	for k := range m {
		keys = append(keys, k)
	}

	sort.Slice(keys, func(i, j int) bool {
		return keys[i].String() < keys[j].String()
	})

	return keys
}

func detectChangesInHosts(oldHosts map[string]Resource, newHosts map[string]Resource) (removedHosts []string, updatedHosts []string, addedHosts []string) {
	for _, h := range getSortedResourceKeys(oldHosts) {
		_, exists := newHosts[h]
		if !exists {
			removedHosts = append(removedHosts, h)
		}
	}

	for _, h := range getSortedResourceKeys(newHosts) {
		_, exists := oldHosts[h]
		if !exists {
			addedHosts = append(addedHosts, h)
		}
	}

	for _, h := range getSortedResourceKeys(newHosts) {
		oldR, exists := oldHosts[h]
		if !exists {
			continue
		}
		if !oldR.IsEqual(newHosts[h]) {
			updatedHosts = append(updatedHosts, h)
			continue
		}

		newVsc, newHostOk := newHosts[h].(*VirtualServerConfiguration)
		oldVsc, oldHostOk := oldHosts[h].(*VirtualServerConfiguration)
		if !newHostOk || !oldHostOk {
			continue
		}

		if newVsc.HTTPPort != oldVsc.HTTPPort || newVsc.HTTPSPort != oldVsc.HTTPSPort {
			updatedHosts = append(updatedHosts, h)
		}

		if newVsc.HTTPIPv4 != oldVsc.HTTPIPv4 {
			updatedHosts = append(updatedHosts, h)
		}

		if newVsc.HTTPIPv6 != oldVsc.HTTPIPv6 {
			updatedHosts = append(updatedHosts, h)
		}

	}

	return removedHosts, updatedHosts, addedHosts
}

// detectChangesInVSRReferences compares ordered VS identities and skips deleted VSRs with no status to refresh.
func detectChangesInVSRReferences(
	oldIndex map[string][]*conf_v1.VirtualServer,
	newIndex map[string][]*conf_v1.VirtualServer,
	vsrs map[string]*conf_v1.VirtualServerRoute,
) []*conf_v1.VirtualServerRoute {
	var changed []*conf_v1.VirtualServerRoute

	for _, key := range getSortedVirtualServerRouteKeys(vsrs) {
		if vsSlicesReferenceSameVirtualServers(oldIndex[key], newIndex[key]) {
			continue
		}
		changed = append(changed, vsrs[key])
	}

	return changed
}

// vsSlicesReferenceSameVirtualServers reports whether two slices of
// VirtualServers reference the same VirtualServers, in the same order,
// identified by namespace/name.
func vsSlicesReferenceSameVirtualServers(a, b []*conf_v1.VirtualServer) bool {
	return slices.EqualFunc(a, b, func(x, y *conf_v1.VirtualServer) bool {
		if x == nil || y == nil {
			return x == y
		}
		return x.Namespace == y.Namespace && x.Name == y.Name
	})
}

func detectChangesInListenerHosts(
	oldListenerHosts map[listenerHostKey]*TransportServerConfiguration,
	newListenerHosts map[listenerHostKey]*TransportServerConfiguration,
) (removedListenerHosts []listenerHostKey, updatedListenerHosts []listenerHostKey, addedListenerHosts []listenerHostKey) {
	oldKeys := getSortedListenerHostKeys(oldListenerHosts)
	newKeys := getSortedListenerHostKeys(newListenerHosts)

	oldKeysSet := make(map[listenerHostKey]struct{})
	for _, key := range oldKeys {
		oldKeysSet[key] = struct{}{}
		if _, exists := newListenerHosts[key]; !exists {
			removedListenerHosts = append(removedListenerHosts, key)
		}
	}

	for _, key := range newKeys {
		if _, exists := oldListenerHosts[key]; !exists {
			addedListenerHosts = append(addedListenerHosts, key)
		} else {
			oldConfig := oldListenerHosts[key]
			if !oldConfig.IsEqual(newListenerHosts[key]) {
				updatedListenerHosts = append(updatedListenerHosts, key)
			}
		}
	}

	return removedListenerHosts, updatedListenerHosts, addedListenerHosts
}

// balanceUpstreamProxies balances proxy buffer sizes for all upstreams.
// This is the unified function that handles proxy buffer balancing for both
// VirtualServer and VirtualServerRoute. We need this here because upstreams are
// values in the slice, but the balancing function takes pointers as it modifies
// the upstreams.
func (c *Configuration) balanceUpstreamProxies(upstreams []conf_v1.Upstream) {
	for i := range upstreams {
		internalValidation.BalanceProxiesForUpstreams(&upstreams[i], c.isDirectiveAutoadjustEnabled)
	}
}
