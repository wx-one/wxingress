package k8s

import (
	networking "k8s.io/api/networking/v1"
)

// asIngressNginx gives an Ingress the shape ingress-nginx serves it in:
//
//   - rules with the same host are one rule: ingress-nginx merges them, where
//     NGINX Ingress Controller refuses the second as a duplicate - charts that
//     write a rule per path do that;
//   - a path a rule of the host has already (the same path, Exact or not) is
//     left out, as ingress-nginx keeps the first location of a path - two
//     rules of one host with path / (a chart that lists a host twice) were
//     two "location /" and a configuration nginx refuses, for every host;
//   - an Ingress without rules but with spec.defaultBackend is a host-less
//     rule for every path to that backend, the catch-all ingress-nginx makes
//     of it, when host-less rules are allowed (-allow-empty-ingress-host).
//
// The Ingress is copied only when it changes.
func asIngressNginx(ing *networking.Ingress, allowEmptyHost bool) *networking.Ingress {
	duplicate := false
	seen := map[string]bool{}
	for _, r := range ing.Spec.Rules {
		if seen[r.Host] || hasDuplicatePath(r) {
			duplicate = true
			break
		}
		seen[r.Host] = true
	}
	catchAll := len(ing.Spec.Rules) == 0 && ing.Spec.DefaultBackend != nil && allowEmptyHost
	if !duplicate && !catchAll {
		return ing
	}

	out := ing.DeepCopy()
	if duplicate {
		var rules []networking.IngressRule
		at := map[string]int{}
		for _, r := range out.Spec.Rules {
			i, ok := at[r.Host]
			if !ok {
				at[r.Host] = len(rules)
				rules = append(rules, r)
				continue
			}
			if r.HTTP == nil {
				continue
			}
			if rules[i].HTTP == nil {
				rules[i].HTTP = &networking.HTTPIngressRuleValue{}
			}
			rules[i].HTTP.Paths = append(rules[i].HTTP.Paths, r.HTTP.Paths...)
		}
		for i := range rules {
			if rules[i].HTTP != nil {
				rules[i].HTTP.Paths = firstOfEachPath(rules[i].HTTP.Paths)
			}
		}
		out.Spec.Rules = rules
	}
	if catchAll {
		prefix := networking.PathTypePrefix
		out.Spec.Rules = []networking.IngressRule{{
			IngressRuleValue: networking.IngressRuleValue{HTTP: &networking.HTTPIngressRuleValue{
				Paths: []networking.HTTPIngressPath{{Path: "/", PathType: &prefix, Backend: *out.Spec.DefaultBackend}},
			}},
		}}
	}
	return out
}

// pathKey is what makes a path one location: the path, and whether it is
// Exact (location =) or not.
func pathKey(p networking.HTTPIngressPath) string {
	if p.PathType != nil && *p.PathType == networking.PathTypeExact {
		return "=" + p.Path
	}
	return p.Path
}

func hasDuplicatePath(r networking.IngressRule) bool {
	if r.HTTP == nil {
		return false
	}
	seen := map[string]bool{}
	for _, p := range r.HTTP.Paths {
		if seen[pathKey(p)] {
			return true
		}
		seen[pathKey(p)] = true
	}
	return false
}

func firstOfEachPath(paths []networking.HTTPIngressPath) []networking.HTTPIngressPath {
	seen := map[string]bool{}
	out := paths[:0]
	for _, p := range paths {
		if seen[pathKey(p)] {
			continue
		}
		seen[pathKey(p)] = true
		out = append(out, p)
	}
	return out
}
