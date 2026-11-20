package k8s

import (
	"maps"
	"slices"
	"time"

	"github.com/wx-one/wxingress/internal/k8s/secrets"
	nl "github.com/wx-one/wxingress/internal/logger"
	api_v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/tools/cache"
)

// What -dynamic-upstreams changes in how the controller applies changes: as
// ingress-nginx does, NGINX is reloaded only when its configuration changes,
// and everything else - the servers of upstreams, canaries, certificates, the
// files NGINX reads per request - goes to the wxingress module or to disk
// without a reload.

// dynamicUpstreams reports whether NGINX runs with the wxingress module (-dynamic-upstreams).
func (lbc *LoadBalancerController) dynamicUpstreams() bool {
	return lbc.configuration != nil && lbc.configuration.isDynamicUpstreams
}

// secretRolesNeedReload reports whether NGINX must be reloaded on a Secret
// update even if the rendered config bytes are unchanged, for the roles the
// Secret has. A certificate (RoleTLS) is re-read per handshake with
// -ssl-dynamic-reload; the user file of basic auth (RoleHtpasswd, RoleAuthFile,
// RoleAuthMap) is read by NGINX for every request, at a path that does not
// change, so with readFilesPerRequest - with -dynamic-upstreams, as in
// ingress-nginx, which writes the file and reloads only when the config
// changes - its new content needs no reload. So is the JWK set of a jwt
// Policy (RoleJWK) there: the wxingress module reads the file again when it
// changes. Any other role is read when the config is loaded.
func secretRolesNeedReload(roles []secrets.SecretRole, dynamicSSLReloadEnabled bool, readFilesPerRequest bool) bool {
	if len(roles) == 0 {
		return true
	}
	for _, role := range roles {
		switch role {
		case secrets.RoleTLS:
			if !dynamicSSLReloadEnabled {
				return true
			}
		case secrets.RoleHtpasswd, secrets.RoleAuthFile, secrets.RoleAuthMap, secrets.RoleJWK:
			if !readFilesPerRequest {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// serviceNeedsReload reports whether a Service that changed needs a reload
// even when the configs of the resources that use it stay the same. Without
// -dynamic-upstreams it always does. With it, as in ingress-nginx, only when
// the configs change: the servers of the upstreams go to the wxingress module.
// An ExternalName Service is the exception: its name is a server NGINX
// resolves when it loads the config, which the module does not reach.
func serviceNeedsReload(dynamicUpstreams bool, obj interface{}, exists bool) bool {
	if !dynamicUpstreams {
		return true
	}
	svc, isService := obj.(*api_v1.Service)
	return exists && isService && svc.Spec.Type == api_v1.ServiceTypeExternalName
}

// hasHeadlessServiceChanged reports, for a Service update that
// hasServicedChanged lets through, whether it changed what the controller
// takes from the Service. hasServicedChanged lets every update of a headless
// Service through - its labels, annotations, status - while with
// -dynamic-upstreams only its ports, its ExternalName and whether it has a
// cluster IP matter: its endpoints come with its EndpointSlices.
func hasHeadlessServiceChanged(oldSvc, curSvc *api_v1.Service) bool {
	if !isHeadless(oldSvc) && !isHeadless(curSvc) {
		return true
	}
	return hasServicePortChanges(oldSvc.Spec.Ports, curSvc.Spec.Ports) ||
		hasServiceExternalNameChanges(oldSvc, curSvc) ||
		oldSvc.Spec.Type != curSvc.Spec.Type ||
		oldSvc.Spec.ClusterIP != curSvc.Spec.ClusterIP ||
		!slices.Equal(oldSvc.Spec.ClusterIPs, curSvc.Spec.ClusterIPs)
}

// endpointsSyncInterval is the shortest time between two runs of the
// endpoints task: the EndpointSlices of a rollout change many times a second,
// and their Services get their servers together, at most ten times a second.
const endpointsSyncInterval = 100 * time.Millisecond

// enqueueServiceEndpoints queues, with -dynamic-upstreams, the Service of an
// EndpointSlice that was added, changed or deleted, rather than the
// EndpointSlice: the queue holds a Service once however many of its
// EndpointSlices change, and a deleted EndpointSlice still names its Service
// in its labels, where the EndpointSlice is gone from the lister when its task
// runs. It reports whether it did; the EndpointSlices of the controller's own
// Service, which count its replicas, keep their tasks.
func (lbc *LoadBalancerController) enqueueServiceEndpoints(endpointSlice *discovery_v1.EndpointSlice) bool {
	if !lbc.dynamicUpstreams() {
		return false
	}
	svcName := endpointSlice.Labels[discovery_v1.LabelServiceName]
	if lbc.statusUpdater != nil && lbc.statusUpdater.namespace == endpointSlice.Namespace && lbc.statusUpdater.externalServiceName == svcName {
		return false
	}
	if svcName != "" {
		lbc.syncQueue.Add(task{Kind: serviceEndpoints, Key: endpointSlice.Namespace + "/" + svcName})
	}
	return true
}

// serviceEndpointsChanged makes the endpoints task give the module the
// servers of a Service whose EndpointSlices changed. Before NGINX is ready
// there is nothing to do: the first sync takes every Service as it is then.
func (lbc *LoadBalancerController) serviceEndpointsChanged(key string) {
	if !lbc.isNginxReady {
		return
	}
	if lbc.servicesWithNewEndpoints == nil {
		lbc.servicesWithNewEndpoints = make(map[string]bool)
	}
	lbc.servicesWithNewEndpoints[key] = true
	lbc.endpointsTask.task = task{Kind: endpoints, Key: "endpoints"}
	lbc.endpointsTask.interval = endpointsSyncInterval
	lbc.syncQueue.Schedule(&lbc.endpointsTask)
}

// syncEndpoints gives the module the servers of every Service whose
// EndpointSlices changed since it last ran: only the upstreams whose servers
// changed are sent, and the configs are written before the next reload.
func (lbc *LoadBalancerController) syncEndpoints() bool {
	found := false
	for _, key := range slices.Sorted(maps.Keys(lbc.servicesWithNewEndpoints)) {
		delete(lbc.servicesWithNewEndpoints, key)
		ns, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil || lbc.getNamespacedInformer(ns) == nil {
			continue
		}
		// ingress-nginx's tcp/udp services and default backend
		lbc.syncIngressNginxServicesOf(key)
		if lbc.syncServiceEndpoints(ns, name, name, lbc.configuration.FindResourcesForService(ns, name)) {
			found = true
		}
	}
	return found
}

// batchNeedsReload reports whether a batch ends in a reload. With
// -dynamic-upstreams that is when something in it asked for one - a config
// that changed, a server the module refused - as ingress-nginx reloads when
// its configuration changed, whatever the events were. Without it, when the
// batch had an event other than of endpoints or certificates.
func (lbc *LoadBalancerController) batchNeedsReload() bool {
	if lbc.dynamicUpstreams() {
		return lbc.configurator.ReloadAsked()
	}
	return lbc.enableBatchReload
}

// scheduleReload makes the reload that waits for the end of the reload
// interval, when one waits and no batch is going to make it.
func (lbc *LoadBalancerController) scheduleReload() {
	if !lbc.dynamicUpstreams() || !lbc.isNginxReady || lbc.batchSyncEnabled || !lbc.configurator.ReloadAsked() {
		return
	}
	lbc.reloadTask.task = task{Kind: reload, Key: "reload"}
	lbc.reloadTask.interval = 0
	if !lbc.reloadTask.scheduled {
		// the task waits for the reload interval, not for its own
		lbc.reloadTask.started = lbc.configurator.NextReload()
	}
	lbc.syncQueue.Schedule(&lbc.reloadTask)
}

// The canary rules and certificates the wxingress module did not take are
// given to it again by the retry task, first after a second, then after twice
// as long each time it fails again, up to half a minute.
const (
	retryFirstBackoff = time.Second
	retryMostBackoff  = 30 * time.Second
)

// scheduleRetry makes the retry task run when the module did not take
// everything it was given.
func (lbc *LoadBalancerController) scheduleRetry() {
	if !lbc.dynamicUpstreams() || !lbc.isNginxReady || lbc.retryTask.scheduled || !lbc.configurator.PushesFailed() {
		return
	}
	if lbc.retryBackoff == 0 {
		lbc.retryBackoff = retryFirstBackoff
	}
	lbc.retryTask.task = task{Kind: retry, Key: "retry"}
	lbc.retryTask.interval = 0
	lbc.retryTask.started = time.Now().Add(lbc.retryBackoff)
	lbc.syncQueue.Schedule(&lbc.retryTask)
}

// retryPushes gives the module again what it did not take, and doubles the
// backoff when it fails again.
func (lbc *LoadBalancerController) retryPushes() {
	if err := lbc.configurator.RetryPushes(); err != nil {
		lbc.retryBackoff = min(2*lbc.retryBackoff, retryMostBackoff)
		nl.Errorf(lbc.Logger, "Error giving the wxingress module the canaries and certificates again, next try in %v: %v", lbc.retryBackoff, err)
		return
	}
	lbc.retryBackoff = 0
}
