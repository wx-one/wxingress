package configs

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	nl "github.com/wx-one/wxingress/internal/logger"
	"github.com/wx-one/wxingress/internal/nginx"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

// With -dynamic-upstreams NGINX is reloaded, as ingress-nginx reloads it,
// only when its configuration changes - and the servers of upstreams are not
// part of it there: ingress-nginx compares its configuration without the
// endpoints of its backends (isDynamicConfigurationEnough) and gives those to
// its balancer. The configs here still list the servers of every upstream,
// since a reload starts the upstreams with what the file has; a config that
// changed only in those servers needs no reload, as the servers go to the
// wxingress module through its upstream API.

// upstreamServerLine is a server of an upstream as the templates write it,
// one the upstream API reaches: an address and port, the socket that answers
// for an upstream without endpoints, or 127.0.0.1:8181, which stands for none.
// A server by name - an ExternalName Service, resolved when NGINX loads the
// config - is part of the configuration, and so is a server with any other
// parameter.
var upstreamServerLine = regexp.MustCompile(`(?m)^[ \t]*server (?:unix:\S+|\d{1,3}(?:\.\d{1,3}){3}:\d+|\[[0-9A-Fa-f:.]+\]:\d+) max_fails=\S+ fail_timeout=\S+ max_conns=\S+;[ \t]*\n`)

// configShape is the digest of a config without the servers of its upstreams.
func configShape(content []byte) [sha256.Size]byte {
	return sha256.Sum256(upstreamServerLine.ReplaceAll(content, nil))
}

// dynamicMode reports whether NGINX runs with the wxingress module (-dynamic-upstreams).
func (cnf *Configurator) dynamicMode() bool {
	return cnf.staticCfgParams != nil && cnf.staticCfgParams.UpstreamAPI
}

// configChanged tells, after a config was written, whether NGINX needs a
// reload for it. Without the module that is whenever the file changed; with
// it, when the file changed other than in the servers of its upstreams.
func (cnf *Configurator) configChanged(configName string, content []byte, fileChanged bool) bool {
	if !cnf.dynamicMode() {
		return fileChanged
	}
	if cnf.configShapes == nil {
		cnf.configShapes = make(map[string][sha256.Size]byte)
	}
	shape := configShape(content)
	known, exists := cnf.configShapes[configName]
	cnf.configShapes[configName] = shape
	changed := fileChanged && (!exists || known != shape)
	if changed {
		cnf.configsChanged = true
	}
	return changed
}

// serversKey is what tells whether the module has the servers of an upstream:
// their addresses, in any order, and their parameters.
func serversKey(servers []string, cfg nginx.ServerConfig) string {
	if len(servers) == 0 {
		return ""
	}
	sorted := slices.Sorted(slices.Values(servers))
	return fmt.Sprintf("%s|%d|%d|%s|%s", strings.Join(sorted, ","), cfg.MaxFails, cfg.MaxConns, cfg.FailTimeout, cfg.SlowStart)
}

// setRenderedServers records the servers each upstream of a config has in its
// file: what NGINX starts the upstreams with when it is reloaded.
func (cnf *Configurator) setRenderedServers(configName string, servers map[string]string) {
	if !cnf.dynamicMode() {
		return
	}
	if cnf.renderedServers == nil {
		cnf.renderedServers = make(map[string]map[string]string)
	}
	cnf.renderedServers[configName] = servers
}

// renderedServersOfIngress is the servers of each upstream of an Ingress config.
func renderedServersOfIngress(upstreams []version1.Upstream) map[string]string {
	servers := make(map[string]string, len(upstreams))
	for _, u := range upstreams {
		var addresses []string
		var cfg nginx.ServerConfig
		for _, s := range u.UpstreamServers {
			addresses = append(addresses, s.Address)
			cfg = nginx.ServerConfig{MaxFails: s.MaxFails, MaxConns: s.MaxConns, FailTimeout: s.FailTimeout, SlowStart: s.SlowStart}
		}
		servers[u.Name] = serversKey(addresses, cfg)
	}
	return servers
}

// streamUpstreamKey is the key of a stream upstream among the servers the
// module has: the stream block's upstreams are another list than the http
// ones, also under the same name.
func streamUpstreamKey(upstream string) string {
	return "stream " + upstream
}

// renderedServersOfTransportServer is the servers of each upstream of a TransportServer config.
func renderedServersOfTransportServer(upstreams []version2.StreamUpstream) map[string]string {
	servers := make(map[string]string, len(upstreams))
	for _, u := range upstreams {
		var addresses []string
		var cfg nginx.ServerConfig
		for _, s := range u.Servers {
			// the socket that stands for no endpoints is what the module has when the controller sends none
			if s.Address != nginxNonExistingUnixSocket {
				addresses = append(addresses, s.Address)
			}
			cfg = nginx.ServerConfig{MaxFails: s.MaxFails, MaxConns: s.MaxConnections, FailTimeout: s.FailTimeout}
		}
		servers[streamUpstreamKey(u.Name)] = serversKey(addresses, cfg)
	}
	return servers
}

// streamServerConfig is the parameters a TransportServer upstream gives its
// servers, as generateStreamUpstream writes them.
func streamServerConfig(upstream conf_v1.TransportServerUpstream) nginx.ServerConfig {
	return nginx.ServerConfig{
		MaxFails:    generateIntFromPointer(upstream.MaxFails, 1),
		MaxConns:    generateIntFromPointer(upstream.MaxConns, 0),
		FailTimeout: generateTimeWithDefault(upstream.FailTimeout, "10s"),
	}
}

// renderedServersOfVirtualServer is the servers of each upstream of a VirtualServer config.
func renderedServersOfVirtualServer(upstreams []version2.Upstream) map[string]string {
	servers := make(map[string]string, len(upstreams))
	for _, u := range upstreams {
		servers[u.Name] = serversKey(createEndpointsFromUpstream(u), createUpstreamServersConfigForPlus(u))
	}
	return servers
}

// forgetConfig forgets what was recorded for a config that was deleted.
func (cnf *Configurator) forgetConfig(configName string) {
	delete(cnf.configShapes, configName)
	delete(cnf.renderedServers, configName)
}

// reloaded records that NGINX was reloaded, in a reload that started then:
// its upstreams have the servers their files have. The reload interval
// counts from the start, as ingress-nginx's rate limit lets a sync start at
// most so often - counted from the end, every change that waited for the
// interval waited the length of a reload more.
func (cnf *Configurator) reloaded(started time.Time) {
	if !cnf.dynamicMode() {
		return
	}
	cnf.lastReload = started
	cnf.appliedServers = make(map[string]string)
	for _, servers := range cnf.renderedServers {
		maps.Copy(cnf.appliedServers, servers)
	}
}

// pushWhileReloadsWait reports whether the servers of upstreams go to the
// module while reloads wait for the end of a batch. They do with the module
// once NGINX was reloaded with the controller's configs: the servers of an
// upstream NGINX has do not wait for the configuration, and one it does not
// have yet is refused and asks for the reload that brings it.
func (cnf *Configurator) pushWhileReloadsWait() bool {
	return cnf.dynamicMode() && !cnf.lastReload.IsZero()
}

// updateServers gives the module the servers of an upstream, unless it has them.
func (cnf *Configurator) updateServers(upstream string, servers []string, config nginx.ServerConfig) error {
	key := serversKey(servers, config)
	if applied, exists := cnf.appliedServers[upstream]; exists && applied == key {
		return nil
	}
	if err := cnf.nginxManager.UpdateServersInPlus(upstream, servers, config); err != nil {
		delete(cnf.appliedServers, upstream)
		return err
	}
	if cnf.appliedServers == nil {
		cnf.appliedServers = make(map[string]string)
	}
	cnf.appliedServers[upstream] = key
	return nil
}

// updateStreamServers gives the module the servers of a stream upstream, unless it has them.
func (cnf *Configurator) updateStreamServers(upstream string, servers []string, config nginx.ServerConfig) error {
	key := serversKey(servers, config)
	if applied, exists := cnf.appliedServers[streamUpstreamKey(upstream)]; exists && applied == key {
		return nil
	}
	if err := cnf.nginxManager.UpdateStreamServersInPlus(upstream, servers, config); err != nil {
		delete(cnf.appliedServers, streamUpstreamKey(upstream))
		return err
	}
	if cnf.appliedServers == nil {
		cnf.appliedServers = make(map[string]string)
	}
	cnf.appliedServers[streamUpstreamKey(upstream)] = key
	return nil
}

// applyDynamically gives the wxingress module what changed for resources
// whose configs did not change, without a reload: the servers of their
// upstreams, their canaries and the certificates of their hosts. A server the
// module cannot take falls back to a reload, as for endpoints.
func (cnf *Configurator) applyDynamically(ingExes []*IngressEx, mergeableIngs []*MergeableIngresses, vsExes []*VirtualServerEx, tsExes []*TransportServerEx) error {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	reload := false
	push := func(err error) {
		if err != nil {
			nl.Warnf(l, "Couldn't update the endpoints via the API: %v; reloading configuration instead", err)
			reload = true
		}
	}
	for _, ingEx := range ingExes {
		push(cnf.updatePlusEndpoints(ingEx, ingEx.Ingress))
	}
	for _, m := range mergeableIngs {
		push(cnf.updatePlusEndpoints(m.Master, m.Master.Ingress))
		for _, minion := range m.Minions {
			push(cnf.updatePlusEndpoints(minion, m.Master.Ingress))
		}
	}
	for _, vsEx := range vsExes {
		push(cnf.updatePlusEndpointsForVirtualServer(vsEx))
	}
	for _, tsEx := range tsExes {
		push(cnf.updatePlusEndpointsForTransportServer(tsEx))
	}
	if reload {
		return cnf.Reload(nginx.ReloadForEndpointsUpdate)
	}
	return errors.Join(cnf.syncCanaries(), cnf.syncCertificates(), cnf.syncBalancers())
}

// A config whose resource has new endpoints is not written at once with
// -dynamic-upstreams: its servers go to the module, and its file - the same
// but for those servers - is written before the next reload, so that a reload
// never starts the upstreams with servers they no longer have.

// stagedKind is the kind of resource whose config waits to be written.
type stagedKind int

const (
	stagedIngress stagedKind = iota
	stagedMergeableIngress
	stagedVirtualServer
	stagedTransportServer
)

// stage records a resource whose config is to be written before the next reload.
func (cnf *Configurator) stage(name string, kind stagedKind) {
	if cnf.staged == nil {
		cnf.staged = make(map[string]stagedKind)
	}
	cnf.staged[name] = kind
}

// stageIngress takes the new endpoints of an Ingress without writing its config.
func (cnf *Configurator) stageIngress(ingEx *IngressEx) Warnings {
	nginxCfg, warnings := cnf.generateIngressConfig(ingEx)
	name := objectMetaToFileName(&ingEx.Ingress.ObjectMeta)
	cnf.ingresses[name] = ingEx
	if cnf.isLatencyMetricsEnabled {
		cnf.updateIngressMetricsLabels(ingEx, nginxCfg.Upstreams)
	}
	cnf.stage(name, stagedIngress)
	return warnings
}

// stageMergeableIngress takes the new endpoints of a master and its minions without writing their config.
func (cnf *Configurator) stageMergeableIngress(mergeableIngs *MergeableIngresses) Warnings {
	nginxCfg, warnings := cnf.generateMergeableIngressConfig(mergeableIngs)
	name := objectMetaToFileName(&mergeableIngs.Master.Ingress.ObjectMeta)
	cnf.ingresses[name] = mergeableIngs.Master
	cnf.mergeableIngresses[name] = mergeableIngs
	if cnf.isLatencyMetricsEnabled {
		cnf.updateIngressMetricsLabels(mergeableIngs.Master, nginxCfg.Upstreams)
	}
	cnf.stage(name, stagedMergeableIngress)
	return warnings
}

// stageVirtualServer takes the new endpoints of a VirtualServer without writing its config.
func (cnf *Configurator) stageVirtualServer(vsEx *VirtualServerEx) Warnings {
	vsCfg, warnings := cnf.generateVirtualServerConfig(vsEx)
	name := getFileNameForVirtualServer(vsEx.VirtualServer)
	cnf.virtualServers[name] = vsEx
	cnf.virtualServerWarnings[name] = warnings
	if cnf.isLatencyMetricsEnabled {
		// the latency metrics name the pods behind the servers
		cnf.updateVirtualServerMetricsLabels(vsEx, vsCfg.Upstreams)
	}
	cnf.stage(name, stagedVirtualServer)
	return warnings
}

// stageTransportServer takes the new endpoints of a TransportServer without writing its config.
func (cnf *Configurator) stageTransportServer(tsEx *TransportServerEx) {
	name := getFileNameForTransportServer(tsEx.TransportServer)
	cnf.transportServers[name] = tsEx
	cnf.stage(name, stagedTransportServer)
}

// writeStaged writes the configs that wait for it, before a reload.
func (cnf *Configurator) writeStaged() {
	if len(cnf.staged) == 0 {
		return
	}
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	for _, name := range slices.Sorted(maps.Keys(cnf.staged)) {
		var err error
		switch cnf.staged[name] {
		case stagedIngress:
			if ingEx, exists := cnf.ingresses[name]; exists {
				_, _, err = cnf.addOrUpdateIngress(ingEx)
			}
		case stagedMergeableIngress:
			if mergeableIngs, exists := cnf.mergeableIngresses[name]; exists {
				_, _, err = cnf.addOrUpdateMergeableIngress(mergeableIngs)
			}
		case stagedVirtualServer:
			if vsEx, exists := cnf.virtualServers[name]; exists {
				_, _, _, err = cnf.addOrUpdateVirtualServer(vsEx)
			}
		case stagedTransportServer:
			if tsEx, exists := cnf.transportServers[name]; exists {
				_, _, err = cnf.addOrUpdateTransportServer(tsEx)
			}
		}
		if err != nil {
			nl.Errorf(l, "Error writing the config %v with its new endpoints: %v", name, err)
		}
		delete(cnf.staged, name)
	}
}

// With -dynamic-upstreams NGINX is reloaded at most once per reload
// interval, as ingress-nginx syncs at most --sync-rate-limit times a second:
// a reload asked for sooner after the last one waits, and every change that
// comes meanwhile is in the one reload at the end of the interval. A change
// after a quiet time is reloaded at once. What needs no reload does not wait.

// reloadInterval is the shortest time between two reloads for a rate limit
// of reloads a second, 0 for none.
func reloadInterval(rateLimit float64) time.Duration {
	if rateLimit <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / rateLimit)
}

// reloadWaits reports whether a reload asked for now waits for the end of the reload interval.
func (cnf *Configurator) reloadWaits() bool {
	return cnf.dynamicMode() && cnf.reloadInterval > 0 && !cnf.lastReload.IsZero() && time.Since(cnf.lastReload) < cnf.reloadInterval
}

// NextReload is when a reload that waits can be made.
func (cnf *Configurator) NextReload() time.Time {
	return cnf.lastReload.Add(cnf.reloadInterval)
}

// ReloadIfAsked makes the reload that waits, if one does and reloads are enabled.
func (cnf *Configurator) ReloadIfAsked() error {
	if !cnf.reloadAsked || !cnf.isReloadsEnabled {
		return nil
	}
	return cnf.reload(nginx.ReloadForOtherUpdate)
}

// PushesFailed reports whether the wxingress module did not take all canary
// rules, certificates or balancer rules the last time they were given to it.
func (cnf *Configurator) PushesFailed() bool {
	return cnf.canariesFailed || cnf.certificatesFailed || cnf.balancersFailed
}

// RetryPushes gives the module the canary rules, certificates and balancer rules it does not have.
func (cnf *Configurator) RetryPushes() error {
	return errors.Join(cnf.syncCanaries(), cnf.pushCertificates(), cnf.syncBalancers())
}
