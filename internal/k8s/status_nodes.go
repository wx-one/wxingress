package k8s

import (
	"slices"

	nl "github.com/wx-one/wxingress/internal/logger"
	api_v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// syncStatusFromNodes gives the status the addresses of the nodes the
// controller's pods run on, as ingress-nginx does without --publish-service:
// a DaemonSet on the nodes' ports behind a ClusterIP Service has no other
// address, and its Ingresses had none, which Argo CD waits on as Progressing.
// It runs with -ingress-nginx-defaults when an EndpointSlice of the
// controller's own Service changes, and updates the Ingresses (and
// VirtualServers) when the addresses changed and nothing else gives one.
func (lbc *LoadBalancerController) syncStatusFromNodes(ns, svcName string) {
	if !lbc.ingressNginxDefaults || !lbc.reportIngressStatus || lbc.statusUpdater == nil {
		return
	}
	nsi := lbc.getNamespacedInformer(ns)
	if nsi == nil || nsi.endpointSliceLister.Store == nil {
		return
	}
	var ownSlices []*discovery_v1.EndpointSlice
	for _, obj := range nsi.endpointSliceLister.Store.List() {
		if s, ok := obj.(*discovery_v1.EndpointSlice); ok && s.Namespace == ns && s.Labels[discovery_v1.LabelServiceName] == svcName {
			ownSlices = append(ownSlices, s)
		}
	}
	addresses, ok := lbc.nodeAddressesOf(controllerNodes(ownSlices))
	if !ok || !lbc.statusUpdater.SaveStatusFromNodes(addresses) {
		return
	}
	nl.Infof(lbc.Logger, "The status addresses are the controller's nodes: %v", addresses)
	if lbc.reportStatusEnabled() {
		ingresses := lbc.configuration.GetResourcesWithFilter(resourceFilter{Ingresses: true})
		if err := lbc.statusUpdater.UpdateExternalEndpointsForResources(ingresses); err != nil {
			nl.Errorf(lbc.Logger, "error updating the status of Ingresses to the controller's nodes: %v", err)
		}
	}
	if lbc.areCustomResourcesEnabled && lbc.reportCustomResourceStatusEnabled() {
		virtualServers := lbc.configuration.GetResourcesWithFilter(resourceFilter{VirtualServers: true})
		if err := lbc.statusUpdater.UpdateExternalEndpointsForResources(virtualServers); err != nil {
			nl.Errorf(lbc.Logger, "error updating the status of VirtualServers to the controller's nodes: %v", err)
		}
	}
}

// controllerNodes returns the nodes of the endpoints of the controller's
// Service, sorted, without those of pods that are stopping. A pod that is not
// ready yet counts, as ingress-nginx counts its running pods, so that a
// restart does not take its node out of every Ingress's status and put it
// back.
func controllerNodes(endpointSlices []*discovery_v1.EndpointSlice) []string {
	var nodes []string
	for _, s := range endpointSlices {
		for _, e := range s.Endpoints {
			if e.NodeName == nil || *e.NodeName == "" {
				continue
			}
			if e.Conditions.Terminating != nil && *e.Conditions.Terminating {
				continue
			}
			nodes = append(nodes, *e.NodeName)
		}
	}
	slices.Sort(nodes)
	return slices.Compact(nodes)
}

// nodeAddressesOf returns the address of each node, its ExternalIP or else its
// InternalIP, as ingress-nginx reports them, sorted and without duplicates.
// The nodes are listed, which the ClusterRole allows, rather than read one by
// one, which it does not. A node not listed is left out. When the nodes
// cannot be listed it reports false, and the status stays as it is.
func (lbc *LoadBalancerController) nodeAddressesOf(nodes []string) ([]string, bool) {
	addresses := []string{}
	if len(nodes) == 0 {
		return addresses, true
	}
	list, err := lbc.client.CoreV1().Nodes().List(lbc.ctx, meta_v1.ListOptions{})
	if err != nil {
		nl.Warnf(lbc.Logger, "The nodes' addresses for the status: %v", err)
		return nil, false
	}
	for i := range list.Items {
		if slices.Contains(nodes, list.Items[i].Name) {
			if a := nodeAddress(&list.Items[i]); a != "" {
				addresses = append(addresses, a)
			}
		}
	}
	slices.Sort(addresses)
	return slices.Compact(addresses), true
}

func nodeAddress(node *api_v1.Node) string {
	internal := ""
	for _, a := range node.Status.Addresses {
		switch a.Type {
		case api_v1.NodeExternalIP:
			if a.Address != "" {
				return a.Address
			}
		case api_v1.NodeInternalIP:
			if internal == "" {
				internal = a.Address
			}
		}
	}
	return internal
}
