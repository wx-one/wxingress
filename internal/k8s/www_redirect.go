package k8s

import (
	"maps"
	"strings"

	"github.com/wx-one/wxingress/internal/configs"
	networking "k8s.io/api/networking/v1"
)

// setWWWRedirects puts the redirects of from-to-www-redirect on the IngressEx and remembers them.
func (lbc *LoadBalancerController) setWWWRedirects(ingEx *configs.IngressEx) {
	redirects := lbc.wwwRedirectsOf(ingEx.Ingress, ingEx.ValidHosts)
	ingEx.WWWRedirects = redirects
	if lbc.admission.checking {
		return
	}
	key := "Ingress/" + ingEx.Ingress.Namespace + "/" + ingEx.Ingress.Name
	lbc.wwwRedirectsLock.Lock()
	defer lbc.wwwRedirectsLock.Unlock()
	if lbc.wwwRedirects == nil {
		lbc.wwwRedirects = map[string]map[string]string{}
	}
	if redirects == nil {
		delete(lbc.wwwRedirects, key)
	} else {
		lbc.wwwRedirects[key] = redirects
	}
}

// HasHost tells whether a resource of the Configuration has the host.
func (c *Configuration) HasHost(host string) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	_, exists := c.hosts[host]
	return exists
}

// wwwRedirectsOf are the redirects ingress-nginx's from-to-www-redirect makes
// for the Ingress: for each of its hosts, from www.<host> (or from <host>
// without www. for a www. host) - unless a resource has that host itself.
func (lbc *LoadBalancerController) wwwRedirectsOf(ing *networking.Ingress, validHosts map[string]bool) map[string]string {
	if lbc.configuration == nil || !configs.IngressNginxFromToWWW(ing) {
		return nil
	}
	out := map[string]string{}
	for _, rule := range ing.Spec.Rules {
		to := rule.Host
		if to == "" || strings.HasPrefix(to, "*") || !validHosts[to] {
			continue
		}
		from := "www." + to
		if strings.HasPrefix(to, "www.") {
			from = strings.TrimPrefix(to, "www.")
		}
		if lbc.configuration.HasHost(from) {
			continue
		}
		out[to] = from
	}
	return out
}

// wwwRedirectChanges are AddOrUpdate changes for the Ingresses with
// from-to-www-redirect whose redirects are no longer those they were
// configured with - a host they redirect from was taken or freed - and
// that the changes do not already have.
func (lbc *LoadBalancerController) wwwRedirectChanges(changes []ResourceChange) []ResourceChange {
	if lbc.configuration == nil {
		return nil
	}
	inChanges := map[string]bool{}
	for _, c := range changes {
		inChanges[c.Resource.GetKeyWithKind()] = true
	}
	var more []ResourceChange
	for _, r := range lbc.configuration.GetResources() {
		ingConf, ok := r.(*IngressConfiguration)
		if !ok || !configs.IngressNginxFromToWWW(ingConf.Ingress) || inChanges[r.GetKeyWithKind()] {
			continue
		}
		lbc.wwwRedirectsLock.Lock()
		last := lbc.wwwRedirects[r.GetKeyWithKind()]
		lbc.wwwRedirectsLock.Unlock()
		if !maps.Equal(last, lbc.wwwRedirectsOf(ingConf.Ingress, ingConf.ValidHosts)) {
			more = append(more, ResourceChange{Op: AddOrUpdate, Resource: r})
		}
	}
	return more
}
