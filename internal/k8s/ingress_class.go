package k8s

import (
	"strings"

	nl "github.com/wx-one/wxingress/internal/logger"
	networking "k8s.io/api/networking/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// The IngressClasses served are those whose spec.controller is one of
// -ingress-class-controllers - nginx.wx1.eu/ingress-controller by default,
// and k8s.io/ingress-nginx or nginx.org/ingress-controller to run on a class
// another controller made, beside it or in its place - and the class named by
// -ingress-class, which an Ingress may name in the kubernetes.io/ingress.class
// annotation without any IngressClass. An Ingress without a class is served
// with -watch-ingress-without-class, as ingress-nginx's
// --watch-ingress-without-class, or when a served IngressClass is the
// cluster's default (ingressclass.kubernetes.io/is-default-class).

// IngressClassDefaultAnnotation marks the cluster's default IngressClass.
const IngressClassDefaultAnnotation = "ingressclass.kubernetes.io/is-default-class"

// addIngressClassInformer watches the IngressClasses, cluster-wide. A class
// that comes, goes or changes its controller changes which Ingresses are
// served: every Ingress is synced again.
func (lbc *LoadBalancerController) addIngressClassInformer() {
	factory := informers.NewSharedInformerFactory(lbc.client, lbc.resync)
	informer := factory.Networking().V1().IngressClasses().Informer()
	resync := func(interface{}) { lbc.resyncAllIngresses() }
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    resync,
		DeleteFunc: resync,
		UpdateFunc: func(old, current interface{}) {
			o, c := old.(*networking.IngressClass), current.(*networking.IngressClass)
			if o.Spec.Controller != c.Spec.Controller || o.Annotations[IngressClassDefaultAnnotation] != c.Annotations[IngressClassDefaultAnnotation] {
				lbc.resyncAllIngresses()
			}
		},
	}); err != nil {
		nl.Fatalf(lbc.Logger, "Failed to add the IngressClass handler: %v", err)
	}
	lbc.ingressClassLister = informer.GetStore()
	lbc.ingressClassFactory = factory
	lbc.cacheSyncs = append(lbc.cacheSyncs, informer.HasSynced)
}

// resyncAllIngresses syncs every Ingress again: whether it is served may have changed.
func (lbc *LoadBalancerController) resyncAllIngresses() {
	lbc.namespacedInformers.ForEach(func(nsi *namespacedInformer) {
		list, err := nsi.ingressLister.List()
		if err != nil {
			return
		}
		for i := range list.Items {
			lbc.AddSyncQueue(&list.Items[i])
		}
	})
}

// servesController tells whether an IngressClass controller is one of ours.
func (lbc *LoadBalancerController) servesController(controller string) bool {
	for _, c := range lbc.ingressClassControllers {
		if c == controller {
			return true
		}
	}
	return false
}

// servesIngressClass tells whether Ingresses of a class name are ours.
func (lbc *LoadBalancerController) servesIngressClass(name string) bool {
	if name == "" {
		if lbc.watchIngressWithoutClass {
			return true
		}
		return lbc.servesDefaultIngressClass()
	}
	if lbc.ingressClassLister != nil {
		if obj, exists, err := lbc.ingressClassLister.GetByKey(name); err == nil && exists {
			if class, ok := obj.(*networking.IngressClass); ok && lbc.servesController(class.Spec.Controller) {
				return true
			}
		}
	}
	return name == lbc.ingressClass
}

// servesDefaultIngressClass tells whether the cluster's default IngressClass is one of ours.
func (lbc *LoadBalancerController) servesDefaultIngressClass() bool {
	if lbc.ingressClassLister == nil {
		return false
	}
	for _, obj := range lbc.ingressClassLister.List() {
		class, ok := obj.(*networking.IngressClass)
		if ok && strings.EqualFold(class.Annotations[IngressClassDefaultAnnotation], "true") &&
			(lbc.servesController(class.Spec.Controller) || class.Name == lbc.ingressClass) {
			return true
		}
	}
	return false
}
