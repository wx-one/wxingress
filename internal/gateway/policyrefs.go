package gateway

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

// A route or Gateway names Policies with the annotation
// wxingress.wizardtales.com/policies. A Policy of its own namespace it may
// always name; one of another namespace only if a ReferenceGrant in the
// Policy's namespace allows it: from the kind of the route (HTTPRoute,
// GRPCRoute) or Gateway in its namespace, to the Policy (group k8s.nginx.org,
// kind Policy, and its name or none). Otherwise anyone who can write a route
// could attach - and with a Gateway that routes from all namespaces, learn
// the effect of - the Policies of every other namespace.
//
// A Policy named without that consent is not applied, and what named it
// fails closed: its rules answer 500, as for a Policy that does not exist.

const (
	policyGroup = "k8s.nginx.org"
	policyKind  = "Policy"

	kindHTTPRoute = "HTTPRoute"
	kindGRPCRoute = "GRPCRoute"
	kindGateway   = "Gateway"
)

// policyAccess decides which Policies of other namespaces a route or Gateway
// may name.
type policyAccess struct {
	grants []*gatewayv1.ReferenceGrant
	// kinds are the kinds of the routes, by their ObjectMeta: the source of
	// a match rule
	kinds map[*metav1.ObjectMeta]string
}

func newPolicyAccess(grants map[types.NamespacedName]*gatewayv1.ReferenceGrant, g *graph.Graph) *policyAccess {
	a := &policyAccess{kinds: make(map[*metav1.ObjectMeta]string)}
	for _, rg := range grants {
		a.grants = append(a.grants, rg)
	}
	if g != nil {
		for _, r := range g.Routes {
			switch o := r.Source.(type) {
			case *gatewayv1.HTTPRoute:
				a.kinds[&o.ObjectMeta] = kindHTTPRoute
			case *gatewayv1.GRPCRoute:
				a.kinds[&o.ObjectMeta] = kindGRPCRoute
			}
		}
	}
	return a
}

// kindOf is the kind of the route src is the ObjectMeta of, "" if unknown.
func (a *policyAccess) kindOf(src *metav1.ObjectMeta) string {
	if a == nil {
		return ""
	}
	return a.kinds[src]
}

// allows tells whether an object of kind in namespace from may name the
// Policy ref.
func (a *policyAccess) allows(kind, from string, ref conf_v1.PolicyReference) bool {
	if ref.Namespace == from {
		return true
	}
	if a == nil || kind == "" {
		return false
	}
	for _, rg := range a.grants {
		if rg.Namespace != ref.Namespace || !grantsFrom(rg, kind, from) {
			continue
		}
		for _, to := range rg.Spec.To {
			if string(to.Group) == policyGroup && string(to.Kind) == policyKind &&
				(to.Name == nil || string(*to.Name) == ref.Name) {
				return true
			}
		}
	}
	return false
}

func grantsFrom(rg *gatewayv1.ReferenceGrant, kind, ns string) bool {
	for _, f := range rg.Spec.From {
		if string(f.Group) == gatewayv1.GroupName && string(f.Kind) == kind && string(f.Namespace) == ns {
			return true
		}
	}
	return false
}

// permittedPolicies are the Policies an object of kind names that it may
// name, and those it may not, as "namespace/name".
func (a *policyAccess) permittedPolicies(meta *metav1.ObjectMeta, kind string) (allowed []conf_v1.PolicyReference, denied []string) {
	if meta == nil {
		return nil, nil
	}
	for _, ref := range policiesOf(meta) {
		if a.allows(kind, meta.Namespace, ref) {
			allowed = append(allowed, ref)
			continue
		}
		denied = append(denied, ref.Namespace+"/"+ref.Name)
	}
	return allowed, denied
}

// deniedMessage says which Policies an object may not name.
func deniedMessage(what string, denied []string) string {
	return fmt.Sprintf("%s names Policies of another namespace that no ReferenceGrant there allows it, %s: not applied, its rules answer 500",
		what, strings.Join(denied, ", "))
}
