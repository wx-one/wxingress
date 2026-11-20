package k8s

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func TestConfigurationCheckIngress(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	now := time.Now()
	tea := createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", now.Add(-time.Hour))
	c.AddOrUpdateIngress(tea)
	before := c.GetResources()

	// another path of the host: merged into the server of the Ingress that has the host, which changes
	coffee := createTestIngressWithPath("coffee", "cafe.example.com", "/coffee", "coffee-svc", now)
	check := c.CheckIngress(coffee)
	if !check.Ours || check.Invalid != nil {
		t.Fatalf("check %+v, want an Ingress of ours that is valid", check)
	}
	if len(check.Changed) != 1 || check.Changed[0].GetKeyWithKind() != "Ingress/default/cafe" || len(check.Changed[0].Merged) != 1 {
		t.Errorf("changed %+v, want the config of cafe with coffee merged", check.Changed)
	}

	// the same host and path as another Ingress: ingress-nginx's checkOverlap
	dup := createTestIngressWithPath("dup", "cafe.example.com", "/tea", "other-svc", now)
	check = c.CheckIngress(dup)
	if check.Invalid == nil || !strings.Contains(check.Invalid.Error(), `host "cafe.example.com" and path "/tea" is already defined in ingress default/cafe`) {
		t.Errorf("an Ingress with the host and path of another: %v", check.Invalid)
	}
	// a canary may have them
	canary := createTestCanaryIngress("dup-canary", "cafe.example.com", "/tea", "other-svc", now, nil)
	if check = c.CheckIngress(canary); check.Invalid != nil && strings.Contains(check.Invalid.Error(), "already defined") {
		t.Errorf("a canary with the host and path of another: %v", check.Invalid)
	}
	// and the Ingress itself, updated
	if check = c.CheckIngress(tea.DeepCopy()); check.Invalid != nil {
		t.Errorf("the Ingress itself: %v", check.Invalid)
	}

	// invalid
	invalid := createTestIngressWithPath("bad", "bad.example.com", "/tea", "tea-svc", now)
	invalid.Annotations["nginx.org/proxy-connect-timeout"] = "soon"
	if check = c.CheckIngress(invalid); check.Invalid == nil {
		t.Error("an invalid annotation is not refused")
	}

	// another class: not ours
	foreign := createTestIngressWithPath("foreign", "cafe.example.com", "/tea", "tea-svc", now)
	foreign.Annotations["kubernetes.io/ingress.class"] = "other"
	if check = c.CheckIngress(foreign); check.Ours {
		t.Errorf("an Ingress of another class is ours: %+v", check)
	}

	// nothing of it stays
	after := c.GetResources()
	if len(after) != len(before) || len(after[0].(*IngressConfiguration).Merged) != 0 {
		t.Errorf("the configuration changed: %+v", after)
	}
	if _, exists := c.ingresses["default/coffee"]; exists {
		t.Error("the checked Ingress stayed")
	}
}

type fakeChecker struct {
	warnings []string
	err      error
	got      *networking.Ingress
}

func (f *fakeChecker) CheckIngress(ing *networking.Ingress) ([]string, error) {
	f.got = ing
	return f.warnings, f.err
}

func review(t *testing.T, handler http.Handler, kind metav1.GroupVersionKind, op admissionv1.Operation, obj runtime.Object) *admissionv1.AdmissionResponse {
	t.Helper()
	raw, _ := json.Marshal(obj)
	body, _ := json.Marshal(&admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID: types.UID("u-1"), Kind: kind, Operation: op, Namespace: "default", Name: "cafe",
			Object: runtime.RawExtension{Raw: raw},
		},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/networking/v1/ingresses", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := admissionv1.AdmissionReview{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Response == nil {
		t.Fatalf("answer %s: %v", rec.Body.String(), err)
	}
	if out.Response.UID != "u-1" || out.APIVersion != "admission.k8s.io/v1" || out.Kind != "AdmissionReview" {
		t.Errorf("answer %+v", out)
	}
	return out.Response
}

func TestAdmissionHandler(t *testing.T) {
	t.Parallel()
	ing := createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", time.Now())

	checker := &fakeChecker{warnings: []string{"a warning"}}
	resp := review(t, AdmissionHandler(checker), ingressGVK, admissionv1.Create, ing)
	if !resp.Allowed || len(resp.Warnings) != 1 || checker.got == nil || checker.got.Name != "cafe" {
		t.Errorf("a good Ingress: %+v", resp)
	}

	checker = &fakeChecker{err: errors.New("bad path")}
	resp = review(t, AdmissionHandler(checker), ingressGVK, admissionv1.Update, ing)
	if resp.Allowed || resp.Result == nil || resp.Result.Message != "bad path" || resp.Result.Code != http.StatusBadRequest {
		t.Errorf("a bad Ingress: %+v", resp)
	}

	resp = review(t, AdmissionHandler(checker), metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, admissionv1.Create, ing)
	if resp.Allowed {
		t.Errorf("a Pod: %+v", resp)
	}

	checker = &fakeChecker{err: errors.New("never")}
	if resp = review(t, AdmissionHandler(checker), ingressGVK, admissionv1.Delete, ing); !resp.Allowed || checker.got != nil {
		t.Errorf("a deletion: %+v", resp)
	}

	rec := httptest.NewRecorder()
	AdmissionHandler(checker).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("garbage: %d", rec.Code)
	}
}
