package k8s

import (
	"fmt"
	"maps"
	"slices"

	"github.com/wx-one/wxingress/internal/configs"
	networking "k8s.io/api/networking/v1"
)

// IngressCheck is what the Configuration would make of an Ingress if it
// were created or updated as it is, for the admission webhook.
type IngressCheck struct {
	// Ours tells if the Ingress is of a class the controller serves.
	Ours bool
	// Invalid is why the Ingress would be rejected: its validation, or a path another Ingress has.
	Invalid error
	// Changed are the configs that would change - the Ingress's own, those it is merged into, those whose upstreams
	// it is the canary of - as they would be.
	Changed []*IngressConfiguration
	// Warnings are those of the Ingress's own config.
	Warnings []string
}

// CheckIngress tells what the Ingress would make of the configuration, without changing it.
func (c *Configuration) CheckIngress(ing *networking.Ingress) IngressCheck {
	c.lock.Lock()
	defer c.lock.Unlock()

	key := getResourceKey(&ing.ObjectMeta)
	ing = asIngressNginx(ing, c.allowEmptyIngressHost)
	if !c.hasCorrectIngressClass(ing) {
		return IngressCheck{}
	}
	check := IngressCheck{Ours: true}
	if err := validateIngress(ing, c.isPlus, c.appProtectEnabled, c.appProtectDosEnabled, c.snippetsEnabled, c.isDirectiveAutoadjustEnabled, c.allowEmptyIngressHost).ToAggregate(); err != nil {
		check.Invalid = err
		return check
	}
	if err := c.ingressOverlap(key, ing); err != nil {
		check.Invalid = err
		return check
	}
	if !c.startupComplete {
		return check
	}

	// the configuration with the Ingress, built aside and thrown away
	ingresses, minions := c.ingresses, c.minionsByHost
	c.ingresses = maps.Clone(ingresses)
	c.ingresses[key] = ing
	c.minionsByHost = make(map[string]map[string]bool, len(minions))
	for host, keys := range minions {
		c.minionsByHost[host] = maps.Clone(keys)
	}
	c.updateMinionIndex(key, ing)
	defer func() { c.ingresses, c.minionsByHost = ingresses, minions }()

	newHosts, newResources, _ := c.buildHostsAndResources()
	updateActiveHostsForIngresses(newHosts, newResources)
	removed, updated, added := detectChangesInHosts(c.hosts, newHosts)
	for _, change := range squashResourceChanges(createResourceChangesForHosts(removed, updated, added, c.hosts, newHosts)) {
		if change.Op != AddOrUpdate {
			continue
		}
		r := change.Resource
		if latest, ok := newResources[r.GetKeyWithKind()]; ok {
			r = latest
		}
		if ingConfig, ok := r.(*IngressConfiguration); ok {
			check.Changed = append(check.Changed, ingConfig)
		}
	}
	if own, ok := newResources[getResourceKeyWithKind(ingressKind, &ing.ObjectMeta)].(*IngressConfiguration); ok {
		check.Warnings = slices.Clone(own.Warnings)
	}
	return check
}

// ingressOverlap is ingress-nginx's checkOverlap: a path of the Ingress that
// another Ingress of the class has for the same host - whatever its path
// type - is refused, unless one of the two has the canary annotation (a
// canary takes no location; ingress-nginx refuses two canaries of a path,
// which take none either and so never meet here).
func (c *Configuration) ingressOverlap(key string, ing *networking.Ingress) error {
	if _, canary := ing.Annotations[configs.CanaryAnnotation]; canary {
		return nil
	}
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service == nil {
				continue
			}
			for _, otherKey := range getSortedIngressKeys(c.ingresses) {
				other := c.ingresses[otherKey]
				if otherKey == key || isChallengeIngress(other) {
					continue
				}
				if _, canary := other.Annotations[configs.CanaryAnnotation]; canary {
					continue
				}
				if ingressHasHostPath(other, rule.Host, rootIfEmpty(path.Path)) {
					host := rule.Host
					if host == "" {
						host = "_"
					}
					if other.Namespace != ing.Namespace {
						// nothing of another namespace's Ingress - its namespace, its name - to whoever creates this one
						return fmt.Errorf("host %q and path %q is already defined in another Ingress", host, rootIfEmpty(path.Path))
					}
					return fmt.Errorf("host %q and path %q is already defined in ingress %s/%s", host, rootIfEmpty(path.Path), other.Namespace, other.Name)
				}
			}
		}
	}
	return nil
}

// ingressHasHostPath tells if the Ingress has a path with a Service backend for the host.
func ingressHasHostPath(ing *networking.Ingress, host string, path string) bool {
	for _, rule := range ing.Spec.Rules {
		if rule.Host != host || rule.HTTP == nil {
			continue
		}
		for _, p := range rule.HTTP.Paths {
			if p.Backend.Service != nil && rootIfEmpty(p.Path) == path {
				return true
			}
		}
	}
	return false
}

// HasIngress tells whether the Configuration has the Ingress (of a class the controller serves).
func (c *Configuration) HasIngress(namespace, name string) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	_, exists := c.ingresses[namespace+"/"+name]
	return exists
}
