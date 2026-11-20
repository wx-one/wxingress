package configs

import (
	"errors"
	"maps"
	"slices"

	"github.com/wx-one/wxingress/internal/nginx"
)

// The balancer rules of ingress-nginx's load-balance, upstream-hash-by and
// affinity go to the wxingress module the way the canary rules go: each
// Ingress config has its rules by upstream, the module has what was applied,
// and a change of rules alone is set in the module without a reload - every
// upstream of an Ingress says meta_balancer whatever its rule, so the
// configuration does not change with it.

// setBalancers records the balancer rules of a config.
func (cnf *Configurator) setBalancers(name string, balancers map[string]nginx.Balancer) {
	if cnf.balancers == nil {
		cnf.balancers = make(map[string]map[string]nginx.Balancer)
	}
	if len(balancers) == 0 {
		delete(cnf.balancers, name)
		return
	}
	cnf.balancers[name] = balancers
}

// deleteBalancers forgets the balancer rules of a config.
func (cnf *Configurator) deleteBalancers(name string) {
	delete(cnf.balancers, name)
}

// wantedBalancers returns the balancer rules of all configs.
func (cnf *Configurator) wantedBalancers() map[string]nginx.Balancer {
	wanted := make(map[string]nginx.Balancer)
	for _, balancers := range cnf.balancers {
		maps.Copy(wanted, balancers)
	}
	return wanted
}

// syncBalancers sets the balancer rules of all configs in the wxingress module
// and deletes those of no config. The module keeps its rules over reloads but
// starts without any, so the first reload sets them all.
func (cnf *Configurator) syncBalancers() error {
	if !cnf.staticCfgParams.UpstreamAPI {
		return nil
	}
	if !cnf.isReloadsEnabled || cnf.reloadAsked {
		// a rule may name an upstream the reload at the end of the batch, or the one that waits, brings
		cnf.balancersHeld = true
		return nil
	}
	cnf.balancersHeld = false
	if cnf.appliedBalancers == nil {
		cnf.appliedBalancers = make(map[string]nginx.Balancer)
	}

	wanted := cnf.wantedBalancers()

	var errs []error
	defer func() { cnf.balancersFailed = len(errs) > 0 }()
	for _, upstream := range slices.Sorted(maps.Keys(cnf.appliedBalancers)) {
		if _, exists := wanted[upstream]; exists {
			continue
		}
		if err := cnf.nginxManager.DeleteBalancer(upstream); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(cnf.appliedBalancers, upstream)
	}
	for _, upstream := range slices.Sorted(maps.Keys(wanted)) {
		if applied, exists := cnf.appliedBalancers[upstream]; exists && applied == wanted[upstream] {
			continue
		}
		if err := cnf.nginxManager.SetBalancer(upstream, wanted[upstream]); err != nil {
			errs = append(errs, err)
			continue
		}
		cnf.appliedBalancers[upstream] = wanted[upstream]
	}
	return errors.Join(errs...)
}
