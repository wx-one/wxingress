package k8s

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/wx-one/wxingress/internal/configs"
	"github.com/wx-one/wxingress/internal/k8s/secrets"
	nl "github.com/wx-one/wxingress/internal/logger"
	"github.com/wx-one/wxingress/internal/metrics/collectors"
	api_v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

// createServiceUpstreamLBC returns a controller with the Service tea-svc with
// the ClusterIP and the endpoint 10.0.0.5:8080.
func createServiceUpstreamLBC(t *testing.T, clusterIP string) *LoadBalancerController {
	t.Helper()
	port := int32(8080)
	ready := true
	svc := &api_v1.Service{
		ObjectMeta: meta_v1.ObjectMeta{Name: "tea-svc", Namespace: "default"},
		Spec: api_v1.ServiceSpec{
			ClusterIP: clusterIP,
			Ports:     []api_v1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080)}},
			Selector:  map[string]string{"app": "tea"},
		},
	}
	slice := &discovery_v1.EndpointSlice{
		ObjectMeta: meta_v1.ObjectMeta{
			Name: "tea-svc-abc", Namespace: "default",
			Labels: map[string]string{discovery_v1.LabelServiceName: "tea-svc"},
		},
		Ports:     []discovery_v1.EndpointPort{{Port: &port}},
		Endpoints: []discovery_v1.Endpoint{{Addresses: []string{"10.0.0.5"}, Conditions: discovery_v1.EndpointConditions{Ready: &ready}}},
	}
	svcStore := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := svcStore.Add(svc); err != nil {
		t.Fatal(err)
	}
	esStore := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := esStore.Add(slice); err != nil {
		t.Fatal(err)
	}
	nsi := &namespacedInformer{
		svcLister:           svcStore,
		endpointSliceLister: storeToEndpointSliceLister{Store: esStore},
		podLister:           indexerToPodLister{Indexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})},
	}
	return &LoadBalancerController{
		client:              fake.NewClientset(svc),
		Logger:              nl.LoggerFromContext(context.Background()),
		metricsCollector:    collectors.NewControllerFakeCollector(),
		namespacedInformers: registryFrom(map[string]*namespacedInformer{"default": nsi}),
	}
}

func TestSecretIsReferencedByIngressNginxBasicAuth(t *testing.T) {
	t.Parallel()
	rc := newSecretReferenceChecker(false)
	ing := createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", time.Now())
	ing.Annotations[configs.IngressNginxAuthTypeAnnotation] = "basic"
	ing.Annotations[configs.IngressNginxAuthSecretAnnotation] = "default/basic-auth"

	if !rc.IsReferencedByIngress("default", "basic-auth", ing) || !rc.IsReferencedByMinion("default", "basic-auth", ing) {
		t.Error("the auth-secret is not referenced")
	}
	if rc.IsReferencedByIngress("default", "other", ing) {
		t.Error("another Secret is referenced")
	}
}

func TestCreateIngressExWithIngressNginxBasicAuth(t *testing.T) {
	t.Parallel()
	secret := &api_v1.Secret{
		ObjectMeta: meta_v1.ObjectMeta{Name: "users", Namespace: "default"},
		Type:       api_v1.SecretTypeOpaque,
		Data:       map[string][]byte{"alice": []byte("{SHA}x")},
	}
	lbc := createServiceUpstreamLBC(t, "10.96.0.10")
	lbc.secretStore = secrets.NewFakeSecretsStore(map[secrets.SecretRefKey]*secrets.SecretReference{
		secrets.RefKey("default/users", secrets.RoleAuthMap): {Secret: secret, Path: "/etc/nginx/secrets/basic_auth_default_users_auth-map"},
	})
	ing := createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", time.Now())
	ing.Annotations[configs.IngressNginxAuthTypeAnnotation] = "basic"
	ing.Annotations[configs.IngressNginxAuthSecretAnnotation] = "users"
	ing.Annotations[configs.IngressNginxAuthSecretTypeAnnotation] = "auth-map"

	ingEx := lbc.createIngressEx(ing, map[string]bool{"cafe.example.com": true}, nil)

	ref := ingEx.SecretRefs[secrets.RefKey("default/users", secrets.RoleAuthMap)]
	if ref == nil || ref.Secret != secret || ref.Error != nil {
		t.Errorf("the reference of the auth-secret is %+v", ref)
	}
}

func TestCreateIngressExWithServiceUpstream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		clusterIP   string
		annotations map[string]string
		want        []string
		wantUpdate  bool
	}{
		{
			name:        "service-upstream proxies to the ClusterIP",
			clusterIP:   "10.96.0.10",
			annotations: map[string]string{configs.IngressNginxServiceUpstreamAnnotation: "true"},
			want:        []string{"10.96.0.10:80"},
		},
		{
			name:        "service-upstream false takes the endpoints",
			clusterIP:   "10.96.0.10",
			annotations: map[string]string{configs.IngressNginxServiceUpstreamAnnotation: "false"},
			want:        []string{"10.0.0.5:8080"},
			wantUpdate:  true,
		},
		{
			name:        "a headless Service takes the endpoints, as in ingress-nginx",
			clusterIP:   api_v1.ClusterIPNone,
			annotations: map[string]string{configs.IngressNginxServiceUpstreamAnnotation: "true"},
			want:        []string{"10.0.0.5:8080"},
			wantUpdate:  true,
		},
		{
			name:      "nginx.org/use-cluster-ip wins",
			clusterIP: "10.96.0.10",
			annotations: map[string]string{
				configs.IngressNginxServiceUpstreamAnnotation: "true",
				configs.UseClusterIPAnnotation:                "false",
			},
			want:       []string{"10.0.0.5:8080"},
			wantUpdate: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lbc := createServiceUpstreamLBC(t, test.clusterIP)
			ing := createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", time.Now())
			for k, v := range test.annotations {
				ing.Annotations[k] = v
			}

			ingEx := lbc.createIngressEx(ing, map[string]bool{"cafe.example.com": true}, nil)

			if got := ingEx.Endpoints["tea-svc80"]; !slices.Equal(got, test.want) {
				t.Errorf("endpoints %v, want %v", got, test.want)
			}
			if got := lbc.ingressRequiresEndpointsUpdate(ingEx, "tea-svc"); got != test.wantUpdate {
				t.Errorf("ingressRequiresEndpointsUpdate() = %v, want %v", got, test.wantUpdate)
			}
		})
	}
}
