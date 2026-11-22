package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/wx-one/wxingress/internal/configs"
	"github.com/wx-one/wxingress/internal/k8s/secrets"
	nl "github.com/wx-one/wxingress/internal/logger"
	admissionv1 "k8s.io/api/admission/v1"
	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AdmissionChecks counts the checks of the admission webhook, as ingress-nginx's check_success and check_errors.
type AdmissionChecks interface {
	CheckDone(namespace, name string, ok bool)
}

// admission is the state of the admission webhook of the controller.
type admission struct {
	// mu keeps a check apart from the sync of the controller: a check
	// reads the configuration, the listers' Ingresses and the configurator
	// as the sync leaves them, and writes nothing they keep. It is held
	// while a check builds and renders the configs, not while nginx -t of
	// them runs, which tests a copy of the configuration taken under it.
	mu sync.Mutex
	// checking makes createIngressEx remember nothing of the Ingress it makes (the ConfigMaps it names, its www
	// redirects), and Secrets be resolved for the check only (secretRef).
	checking bool
	// heldSecrets are the Secrets the check under mu materialized for nginx -t, to be let go when it is done.
	heldSecrets []secrets.SecretRefKey
	// test serializes the nginx -t of the checks: one at a time, as a sync would run it.
	test sync.Mutex
	// nginxTest runs nginx -t of the configs a check renders.
	nginxTest bool
	checks    AdmissionChecks
}

// checkSecretStore is a SecretStore that resolves Secrets for a check of the admission webhook without keeping
// them (secrets.LocalSecretStore).
type checkSecretStore interface {
	GetSecretForCheck(key string, role secrets.SecretRole) (*secrets.SecretReference, bool)
	ReleaseCheckSecret(key string, role secrets.SecretRole)
}

// secretRef returns the reference of the Secret in the role. During a check of the admission webhook, a Secret no
// resource resolved yet is held by the check and removed again when it is done, instead of staying on disk.
func (lbc *LoadBalancerController) secretRef(key string, role secrets.SecretRole) *secrets.SecretReference {
	if lbc.admission.checking {
		if store, ok := lbc.secretStore.(checkSecretStore); ok {
			ref, held := store.GetSecretForCheck(key, role)
			if held {
				lbc.admission.heldSecrets = append(lbc.admission.heldSecrets, secrets.RefKey(key, role))
			}
			return ref
		}
	}
	return lbc.secretStore.GetSecret(key, role)
}

// releaseCheckSecrets lets go of the Secrets a check held, under mu.
func (lbc *LoadBalancerController) releaseCheckSecrets(held []secrets.SecretRefKey) {
	store, ok := lbc.secretStore.(checkSecretStore)
	if !ok || len(held) == 0 {
		return
	}
	lbc.admission.mu.Lock()
	defer lbc.admission.mu.Unlock()
	for _, k := range held {
		store.ReleaseCheckSecret(k.Key, k.Role)
	}
}

// EnableAdmission prepares the controller for the admission webhook: nginx -t of what a check renders unless
// snippets are enabled (as ingress-nginx stopped doing for CVE-2025-1974, since a snippet would run in nginx -t
// for anyone who can reach the webhook), and the counts of the checks.
func (lbc *LoadBalancerController) EnableAdmission(nginxTest bool, checks AdmissionChecks) {
	lbc.admission.nginxTest = nginxTest && !lbc.configuration.snippetsEnabled
	lbc.admission.checks = checks
	if c, ok := checks.(interface {
		SetIngressExists(func(namespace, name string) bool)
	}); ok {
		c.SetIngressExists(lbc.configuration.HasIngress)
	}
}

// CheckIngress is the admission webhook's check of an Ingress to be created
// or updated, as ingress-nginx's CheckIngress: none of another class or
// being deleted is refused; one is refused that has a snippet annotation
// while snippets are disabled, that would be invalid, that has the host and
// path of another Ingress, or whose configs - its own, those of the
// Ingresses it is merged into or is the canary of - would not render or
// would fail nginx -t. The warnings of its config are returned either way.
func (lbc *LoadBalancerController) CheckIngress(ing *networking.Ingress) (warnings []string, err error) {
	if ing.DeletionTimestamp != nil {
		return nil, nil
	}
	ours, warnings, test, held, err := lbc.renderIngressCheck(ing)
	defer lbc.releaseCheckSecrets(held)
	if !ours {
		return nil, nil
	}
	defer func() {
		if lbc.admission.checks != nil {
			lbc.admission.checks.CheckDone(ing.Namespace, ing.Name, err == nil)
		}
	}()
	if err != nil {
		return warnings, err
	}
	if test != nil {
		lbc.admission.test.Lock()
		defer lbc.admission.test.Unlock()
		if err := test(context.Background()); err != nil {
			return warnings, fmt.Errorf("the configuration of the Ingress fails nginx -t: %w", err)
		}
	}
	return dedupe(warnings), nil
}

// renderIngressCheck is the part of CheckIngress under the lock of the sync: whether the Ingress is ours, its
// warnings, why it is refused, and the nginx -t of its configs, prepared on a copy of the configuration to run
// without the lock (nil for none). held are the Secrets the check materialized, to be released when it is done.
func (lbc *LoadBalancerController) renderIngressCheck(ing *networking.Ingress) (ours bool, warnings []string, test func(context.Context) error, held []secrets.SecretRefKey, err error) {
	lbc.admission.mu.Lock()
	defer lbc.admission.mu.Unlock()

	if !lbc.HasCorrectIngressClass(ing) {
		return false, nil, nil, nil, nil
	}

	if !lbc.configuration.snippetsEnabled {
		keys := make([]string, 0, len(ing.Annotations))
		for key := range ing.Annotations {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if strings.HasSuffix(key, "-snippet") || strings.HasSuffix(key, "-snippets") {
				return true, nil, nil, nil, fmt.Errorf("%s annotation cannot be used. Snippet directives are disabled by the Ingress administrator", key)
			}
		}
	}

	check := lbc.configuration.CheckIngress(ing)
	if !check.Ours {
		return false, nil, nil, nil, nil
	}
	warnings = check.Warnings
	if check.Invalid != nil {
		return true, warnings, nil, nil, check.Invalid
	}

	lbc.admission.checking = true
	lbc.admission.heldSecrets = nil
	defer func() {
		lbc.admission.checking = false
		held, lbc.admission.heldSecrets = lbc.admission.heldSecrets, nil
	}()

	var rendered []configs.RenderedConfig
	for _, ingConfig := range check.Changed {
		var (
			r      configs.RenderedConfig
			cfgWar configs.Warnings
			rerr   error
		)
		if ingConfig.IsMaster {
			r, cfgWar, rerr = lbc.configurator.RenderMergeableIngresses(lbc.createMergeableIngresses(ingConfig))
		} else {
			ingEx := lbc.createIngressEx(ingConfig.ingressForServer(), ingConfig.ValidHosts, nil)
			ingEx.Canaries = lbc.createCanaryIngressExes(ingConfig.Canaries, ingConfig.CanaryBackends)
			ingEx.Merged = lbc.createMergedIngressExes(ingConfig.Merged, ingEx.Canaries)
			r, cfgWar, rerr = lbc.configurator.RenderIngress(ingEx)
		}
		if rerr != nil {
			return true, warnings, nil, nil, rerr
		}
		for obj, ws := range cfgWar {
			if m, ok := obj.(metav1.Object); ok && m.GetNamespace() == ing.Namespace && m.GetName() == ing.Name {
				warnings = append(warnings, ws...)
			}
		}
		rendered = append(rendered, r)
	}

	if lbc.admission.nginxTest && len(rendered) > 0 {
		test, err = lbc.configurator.PrepareTestConfigs(rendered)
		if err != nil {
			return true, warnings, nil, nil, fmt.Errorf("the configuration of the Ingress fails nginx -t: %w", err)
		}
	}
	return true, warnings, test, nil, nil
}

func dedupe(s []string) []string {
	seen := make(map[string]bool, len(s))
	out := s[:0]
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// IngressChecker checks an Ingress for the admission webhook.
type IngressChecker interface {
	CheckIngress(ing *networking.Ingress) ([]string, error)
}

// maxAdmissionReview is the most of an AdmissionReview the webhook reads: the API server's limit of an object is
// 3 MiB (etcd's request limit is 1.5 MiB), and the review carries the object, and its old version only for an update.
const maxAdmissionReview = 3<<20 + 512<<10

// maxAdmissionChecks is the most reviews the webhook takes at once, those that wait for the sync or another
// check included: one more is answered 503 at once, a failure of the webhook call, so the webhook's
// failurePolicy decides, instead of piling up behind the lock of the sync.
const maxAdmissionChecks = 4

// AdmissionHandler answers the API server's AdmissionReviews (admission.k8s.io/v1) of networking.k8s.io/v1
// Ingresses with the checker's verdict, as ingress-nginx's /networking/v1/ingresses: a request of another kind is
// refused, an Ingress the checker refuses is not allowed, with its error as the message, and its warnings go along.
// At most maxAdmissionChecks reviews are taken at once, and none bigger than maxAdmissionReview.
func AdmissionHandler(checker IngressChecker) http.Handler {
	slots := make(chan struct{}, maxAdmissionChecks)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := nl.LoggerFromContext(r.Context())
		if r.Method != http.MethodPost {
			http.Error(w, "the webhook takes a POST of an AdmissionReview", http.StatusMethodNotAllowed)
			return
		}
		if r.ContentLength > maxAdmissionReview {
			http.Error(w, "the AdmissionReview is too big", http.StatusRequestEntityTooLarge)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "the webhook is busy", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdmissionReview))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "the AdmissionReview is too big", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		review := admissionv1.AdmissionReview{}
		if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
			http.Error(w, "not an AdmissionReview", http.StatusBadRequest)
			return
		}
		review.Response = reviewIngress(checker, review.Request, l)
		review.Request = nil
		review.APIVersion, review.Kind = admissionv1.SchemeGroupVersion.String(), "AdmissionReview"
		out, err := json.Marshal(&review)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
}

var ingressGVK = metav1.GroupVersionKind{Group: networking.GroupName, Version: "v1", Kind: "Ingress"}

func reviewIngress(checker IngressChecker, req *admissionv1.AdmissionRequest, l interface{ Info(string, ...any) }) *admissionv1.AdmissionResponse {
	response := &admissionv1.AdmissionResponse{UID: req.UID}
	deny := func(err error) *admissionv1.AdmissionResponse {
		response.Allowed = false
		response.Result = &metav1.Status{Status: metav1.StatusFailure, Code: http.StatusBadRequest, Reason: metav1.StatusReasonBadRequest, Message: err.Error()}
		return response
	}
	if req.Kind != ingressGVK {
		return deny(fmt.Errorf("rejecting admission review because the request does not contain an Ingress resource but %s with name %s in namespace %s",
			req.Kind.String(), req.Name, req.Namespace))
	}
	if req.Operation == admissionv1.Delete || len(req.Object.Raw) == 0 {
		response.Allowed = true
		return response
	}
	ing := &networking.Ingress{}
	if err := json.Unmarshal(req.Object.Raw, ing); err != nil {
		return deny(err)
	}
	if ing.Namespace == "" {
		ing.Namespace = req.Namespace
	}
	if ing.Name == "" {
		ing.Name = req.Name
	}
	warnings, err := checker.CheckIngress(ing)
	if len(warnings) > 0 {
		response.Warnings = warnings
	}
	if err != nil {
		l.Info("refusing the Ingress", "namespace", ing.Namespace, "name", ing.Name, "error", err.Error())
		return deny(err)
	}
	response.Allowed = true
	return response
}
