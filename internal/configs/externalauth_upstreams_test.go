package configs

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

func TestExternalAuthUpstreamsAreNamedAsInTheConfig(t *testing.T) {
	t.Parallel()
	pol := &conf_v1.Policy{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "auth"}}
	vs := &conf_v1.VirtualServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cafe"},
		Spec:       conf_v1.VirtualServerSpec{Routes: []conf_v1.Route{{Path: "/", Policies: []conf_v1.PolicyReference{{Name: "auth"}}}}},
	}
	vsr := &conf_v1.VirtualServerRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "tea"},
		Spec:       conf_v1.VirtualServerRouteSpec{Subroutes: []conf_v1.Route{{Path: "/tea", Policies: []conf_v1.PolicyReference{{Name: "auth", Namespace: "default"}}}}},
	}
	got := externalAuthUpstreams(&VirtualServerEx{VirtualServer: vs, VirtualServerRoutes: []*conf_v1.VirtualServerRoute{vsr}}, pol)
	want := []string{"vs_default_cafe_vs_exauth_default_auth", "vs_default_cafe_vsr_default_tea_vs_exauth_default_auth"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
