package configs

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/k8s/secrets"
	api_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIngressNginxBasicAuthSecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations map[string]string
		wantKey     string
		wantRole    secrets.SecretRole
		want        bool
	}{
		{
			name:        "auth-file by default",
			annotations: map[string]string{IngressNginxAuthTypeAnnotation: "basic", IngressNginxAuthSecretAnnotation: "basic-auth"},
			wantKey:     "default/basic-auth", wantRole: secrets.RoleAuthFile, want: true,
		},
		{
			name: "auth-map with the namespace",
			annotations: map[string]string{
				IngressNginxAuthTypeAnnotation:       "basic",
				IngressNginxAuthSecretAnnotation:     "default/users",
				IngressNginxAuthSecretTypeAnnotation: "auth-map",
			},
			wantKey: "default/users", wantRole: secrets.RoleAuthMap, want: true,
		},
		{
			name:        "without auth-type",
			annotations: map[string]string{IngressNginxAuthSecretAnnotation: "basic-auth"},
		},
		{
			name:        "another namespace",
			annotations: map[string]string{IngressNginxAuthTypeAnnotation: "basic", IngressNginxAuthSecretAnnotation: "other/basic-auth"},
		},
		{
			name: "nginx.org/basic-auth-secret wins",
			annotations: map[string]string{
				IngressNginxAuthTypeAnnotation:   "basic",
				IngressNginxAuthSecretAnnotation: "basic-auth",
				BasicAuthSecretAnnotation:        "htpasswd",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			key, role, ok := IngressNginxBasicAuthSecret(ingressNginxTestIngress(test.annotations).Ingress)
			if ok != test.want || key != test.wantKey || role != test.wantRole {
				t.Errorf("IngressNginxBasicAuthSecret() = %q, %q, %v, want %q, %q, %v", key, role, ok, test.wantKey, test.wantRole, test.want)
			}
		})
	}
}

func basicAuthTestIngress(annotations map[string]string, ref *secrets.SecretReference) *IngressEx {
	ingEx := ingressNginxTestIngress(annotations)
	ingEx.SecretRefs = map[secrets.SecretRefKey]*secrets.SecretReference{}
	if ref != nil {
		ingEx.SecretRefs[secrets.RefKey("default/basic-auth", secrets.RoleAuthFile)] = ref
	}
	return ingEx
}

func TestParseIngressNginxBasicAuthAnnotations(t *testing.T) {
	t.Parallel()
	secret := &api_v1.Secret{ObjectMeta: meta_v1.ObjectMeta{Name: "basic-auth", Namespace: "default"}, Data: map[string][]byte{"auth": []byte("foo:bar")}}
	basic := map[string]string{
		IngressNginxAuthTypeAnnotation:   "basic",
		IngressNginxAuthSecretAnnotation: "basic-auth",
		IngressNginxAuthRealmAnnotation:  "Authentication Required - foo",
	}
	tests := []struct {
		name        string
		annotations map[string]string
		ref         *secrets.SecretReference
		want        *version1.BasicAuth
		wantDenied  bool
		wantWarning string
	}{
		{
			name:        "basic",
			annotations: basic,
			ref:         &secrets.SecretReference{Secret: secret, Path: "/etc/nginx/secrets/basic_auth_default_basic-auth_auth-file"},
			want:        &version1.BasicAuth{Secret: "/etc/nginx/secrets/basic_auth_default_basic-auth_auth-file", Realm: "Authentication Required - foo"},
		},
		{
			name:        "a missing Secret denies",
			annotations: basic,
			ref:         &secrets.SecretReference{Error: errors.New("secret default/basic-auth doesn't exist")},
			wantDenied:  true,
			wantWarning: "does not exist",
		},
		{
			name:        "a Secret without auth denies",
			annotations: basic,
			ref:         &secrets.SecretReference{Secret: secret, Error: errors.New(`secret is missing required key "auth"`)},
			wantDenied:  true,
			wantWarning: "is invalid",
		},
		{
			name:        "digest denies",
			annotations: map[string]string{IngressNginxAuthTypeAnnotation: "digest", IngressNginxAuthSecretAnnotation: "basic-auth"},
			wantDenied:  true,
			wantWarning: "digest",
		},
		{
			name:        "an invalid realm denies",
			annotations: map[string]string{IngressNginxAuthTypeAnnotation: "basic", IngressNginxAuthSecretAnnotation: "basic-auth", IngressNginxAuthRealmAnnotation: `a"b`},
			wantDenied:  true,
			wantWarning: IngressNginxAuthRealmAnnotation,
		},
		{
			name:        "auth-type without auth-secret denies",
			annotations: map[string]string{IngressNginxAuthTypeAnnotation: "basic"},
			wantDenied:  true,
			wantWarning: IngressNginxAuthSecretAnnotation + " is not set",
		},
		{
			name:        "nginx.org/basic-auth-secret wins",
			annotations: map[string]string{IngressNginxAuthTypeAnnotation: "basic", IngressNginxAuthSecretAnnotation: "basic-auth", BasicAuthSecretAnnotation: "htpasswd"},
			wantWarning: BasicAuthSecretAnnotation + " is set",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ingEx := basicAuthTestIngress(test.annotations, test.ref)
			cfg := NewDefaultConfigParams(context.Background(), false)
			warnings := parseIngressNginxAnnotations(ingEx, cfg, false, false)[ingEx.Ingress]

			if test.want == nil && cfg.IngressNginxBasicAuth != nil || test.want != nil && (cfg.IngressNginxBasicAuth == nil || *cfg.IngressNginxBasicAuth != *test.want) {
				t.Errorf("basic auth %+v, want %+v", cfg.IngressNginxBasicAuth, test.want)
			}
			if cfg.IngressNginxDenied != test.wantDenied {
				t.Errorf("denied %v, want %v", cfg.IngressNginxDenied, test.wantDenied)
			}
			if test.wantWarning != "" && !hasWarningContaining(warnings, test.wantWarning) {
				t.Errorf("warnings %v lack %q", warnings, test.wantWarning)
			}
		})
	}
}

func TestApplyIngressNginxAuth(t *testing.T) {
	t.Parallel()
	auth := &version1.BasicAuth{Secret: "/etc/nginx/secrets/x", Realm: "r"}
	loc := version1.Location{ProxySetHeaders: []version2.Header{{Name: "X-A", Value: "a"}}}
	applyIngressNginxAuth(&loc, &ConfigParams{IngressNginxBasicAuth: auth})
	if loc.BasicAuth != auth || !slices.Contains(loc.ProxySetHeaders, version2.Header{Name: "Authorization", Value: ""}) {
		t.Errorf("location %+v", loc)
	}

	denied := version1.Location{}
	applyIngressNginxAuth(&denied, &ConfigParams{IngressNginxDenied: true})
	if denied.PoliciesErrorReturn == nil || denied.PoliciesErrorReturn.Code != 503 || denied.BasicAuth != nil {
		t.Errorf("a denied location %+v", denied)
	}
}

func TestAuthMapFileContent(t *testing.T) {
	t.Parallel()
	secret := &api_v1.Secret{Data: map[string][]byte{"bob": []byte("$apr1$x$y"), "alice": []byte("{SHA}abc")}}
	if got, want := string(authMapFileContent(secret)), "alice:{SHA}abc\nbob:$apr1$x$y\n"; got != want {
		t.Errorf("authMapFileContent() = %q, want %q", got, want)
	}
	if got := secretFileName("default/users", secrets.RoleAuthMap); got != "basic_auth_default_users_auth-map" {
		t.Errorf("secretFileName() = %q", got)
	}
}
