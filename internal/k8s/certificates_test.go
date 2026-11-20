package k8s

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wx-one/wxingress/internal/configs"
	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/k8s/secrets"
	"github.com/wx-one/wxingress/internal/nginx"
	api_v1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// certificatesManager writes the Secret files for real, as the configurator
// reads them back for the wxingress module, tells if a config changed, and
// counts what reaches NGINX.
type certificatesManager struct {
	*nginx.FakeManager
	dir     string
	configs int
	written map[string]string
	reloads int
	set     []string
	deleted []string
	module  map[string]string
	// fail is how many more certificates the module refuses
	fail int
}

func newCertificatesManager(tb testing.TB) *certificatesManager {
	dir := tb.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		tb.Fatal(err)
	}
	return &certificatesManager{FakeManager: nginx.NewFakeManager(dir), dir: dir, module: make(map[string]string), written: make(map[string]string)}
}

func (m *certificatesManager) CreateSecret(name string, content []byte, _ os.FileMode) string {
	file := m.GetFilenameForSecret(name)
	_ = os.WriteFile(file, content, 0o600)
	return file
}

func (m *certificatesManager) DeleteSecret(name string) {
	_ = os.Remove(m.GetFilenameForSecret(name))
}

func (m *certificatesManager) CreateConfig(name string, content []byte) (bool, error) {
	if m.written[name] == string(content) {
		return false, nil
	}
	m.configs++
	m.written[name] = string(content)
	return true, nil
}

func (m *certificatesManager) Reload(isEndpointsUpdate bool) error {
	m.reloads++
	return m.FakeManager.Reload(isEndpointsUpdate)
}

func (m *certificatesManager) SetCertificate(name string, certificate string, _ string) error {
	if m.fail > 0 {
		m.fail--
		return fmt.Errorf("the module refused %v", name)
	}
	m.set = append(m.set, name)
	m.module[name] = certificate
	return nil
}

func (m *certificatesManager) DeleteCertificate(name string) error {
	m.deleted = append(m.deleted, name)
	delete(m.module, name)
	return nil
}

func (m *certificatesManager) reset() {
	m.configs, m.reloads, m.set, m.deleted = 0, 0, nil, nil
}

func tlsSecret(tb testing.TB, name string, cn string) *api_v1.Secret {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		tb.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		tb.Fatal(err)
	}
	return &api_v1.Secret{
		ObjectMeta: meta_v1.ObjectMeta{Name: name, Namespace: "default"},
		Type:       api_v1.SecretTypeTLS,
		Data: map[string][]byte{
			api_v1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			api_v1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		},
	}
}

// newCertificatesTestLBC is a controller whose certificates come from the
// wxingress module, with one Ingress of n hosts h<i>.example.com, each with
// its Secret s<i>, and the Secrets in the cache that are given.
func newCertificatesTestLBC(t *testing.T, n int, given ...*api_v1.Secret) (*LoadBalancerController, *certificatesManager, cache.Store) {
	t.Helper()
	manager := newCertificatesManager(t)
	lbc := newBatchTestLBC(t, manager)

	templateExecutor, err := version1.NewTemplateExecutor(
		filepath.Join("..", "configs", "version1", "nginx.tmpl"),
		filepath.Join("..", "configs", "version1", "nginx.ingress.tmpl"),
	)
	if err != nil {
		t.Fatal(err)
	}
	templateExecutorV2, err := version2.NewTemplateExecutor(
		filepath.Join("..", "configs", "version2", "nginx.virtualserver.tmpl"),
		filepath.Join("..", "configs", "version2", "nginx.transportserver.tmpl"),
		filepath.Join("..", "configs", "version2", "oidc.tmpl"),
	)
	if err != nil {
		t.Fatal(err)
	}
	lbc.configurator = configs.NewConfigurator(configs.ConfiguratorParams{
		NginxManager:       manager,
		StaticCfgParams:    &configs.StaticConfigParams{NginxVersion: nginx.NewVersion("nginx version: nginx/1.25.3"), UpstreamAPI: true},
		Config:             configs.NewDefaultConfigParams(context.Background(), false),
		MGMTCfgParams:      configs.NewDefaultMGMTConfigParams(context.Background()),
		TemplateExecutor:   templateExecutor,
		TemplateExecutorV2: templateExecutorV2,
	})
	lbc.configurator.EnableReloads()

	secretCache := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, s := range given {
		if err := secretCache.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	lbc.namespacedInformers.Get("default").secretLister = secretCache
	lbc.secretStore = secrets.NewLocalSecretStore(lbc.configurator, secrets.WithSecretResolver(lbc.getSecret))
	lbc.areCustomResourcesEnabled = false
	lbc.plmEnabled = false
	lbc.configuration.CompleteStartup()

	ing := &networking.Ingress{
		ObjectMeta: meta_v1.ObjectMeta{
			Name: "many", Namespace: "default", CreationTimestamp: meta_v1.Now(),
			Annotations: map[string]string{"kubernetes.io/ingress.class": "nginx"},
		},
	}
	for i := 0; i < n; i++ {
		host := fmt.Sprintf("h%d.example.com", i)
		ing.Spec.TLS = append(ing.Spec.TLS, networking.IngressTLS{Hosts: []string{host}, SecretName: fmt.Sprintf("s%d", i)})
		ing.Spec.Rules = append(ing.Spec.Rules, networking.IngressRule{Host: host})
	}
	if _, problems := lbc.configuration.AddOrUpdateIngress(ing); len(problems) > 0 {
		t.Fatalf("AddOrUpdateIngress() problems = %v", problems)
	}
	resources := lbc.configuration.GetResources()
	if _, err := lbc.configurator.AddOrUpdateResources(lbc.createExtendedResources(resources), true); err != nil {
		t.Fatal(err)
	}
	return lbc, manager, secretCache
}

// syncSecrets syncs Secrets as the queue would, then runs what that queued.
func syncSecrets(t *testing.T, lbc *LoadBalancerController, keys ...string) {
	t.Helper()
	for _, key := range keys {
		lbc.sync(task{Kind: secret, Key: key})
	}
	drainSyncQueue(t, lbc)
}

func TestSyncSecretOnlyCertificateOfHosts(t *testing.T) {
	t.Parallel()

	const n = 50
	var given []*api_v1.Secret
	for i := 0; i < n-1; i++ { // the last host's Secret is missing
		given = append(given, tlsSecret(t, fmt.Sprintf("s%d", i), fmt.Sprintf("h%d.example.com", i)))
	}
	lbc, manager, secretCache := newCertificatesTestLBC(t, n, given...)
	if len(manager.module) != n-1 {
		t.Fatalf("the module has %d certificates after the Ingress came, want %d", len(manager.module), n-1)
	}
	last := fmt.Sprintf("h%d.example.com", n-1)
	if _, exists := manager.module[last]; exists {
		t.Fatalf("%v has a certificate without its Secret", last)
	}

	// every Secret changes: nothing is regenerated, nothing reloads, and the module gets all of them at once
	manager.reset()
	var keys []string
	for i := 0; i < n-1; i++ {
		if err := secretCache.Update(tlsSecret(t, fmt.Sprintf("s%d", i), fmt.Sprintf("h%d.example.com", i))); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, fmt.Sprintf("default/s%d", i))
	}
	for _, key := range keys {
		lbc.sync(task{Kind: secret, Key: key})
	}
	if len(manager.set) != 0 {
		t.Errorf("the module was given %d certificates before the certificates task ran", len(manager.set))
	}
	if lbc.syncQueue.queue.Len() != 1 {
		t.Errorf("%d tasks queued for %d Secrets, want the one certificates task", lbc.syncQueue.queue.Len(), len(keys))
	}
	drainSyncQueue(t, lbc)
	if manager.configs != 0 || manager.reloads != 0 {
		t.Errorf("changed Secrets: %d configs written, %d reloads, want none", manager.configs, manager.reloads)
	}
	if len(manager.set) != n-1 || len(manager.deleted) != 0 {
		t.Errorf("changed Secrets: set %d, deleted %v, want %d set", len(manager.set), manager.deleted, n-1)
	}
	for i := 0; i < n-1; i++ {
		host := fmt.Sprintf("h%d.example.com", i)
		want, _, _ := secretCache.GetByKey(fmt.Sprintf("default/s%d", i))
		if manager.module[host] != string(want.(*api_v1.Secret).Data[api_v1.TLSCertKey]) {
			t.Errorf("%v does not have the certificate of its changed Secret", host)
		}
	}

	// the missing Secret comes: its Ingress is regenerated for the host the module is to have, but nothing reloads
	manager.reset()
	lbc.certificatesTask.started = time.Time{}
	if err := secretCache.Add(tlsSecret(t, fmt.Sprintf("s%d", n-1), last)); err != nil {
		t.Fatal(err)
	}
	syncSecrets(t, lbc, fmt.Sprintf("default/s%d", n-1))
	if manager.reloads != 0 || !slices.Equal(manager.set, []string{last}) {
		t.Errorf("a missing Secret that came: %d reloads, set %v, want none and %v", manager.reloads, manager.set, last)
	}

	// a Secret goes and another becomes invalid: their hosts have the default certificate
	manager.reset()
	lbc.certificatesTask.started = time.Time{}
	gone, _, _ := secretCache.GetByKey("default/s0")
	if err := secretCache.Delete(gone); err != nil {
		t.Fatal(err)
	}
	invalid := tlsSecret(t, "s1", "h1.example.com")
	invalid.Data[api_v1.TLSPrivateKeyKey] = []byte("no key")
	if err := secretCache.Update(invalid); err != nil {
		t.Fatal(err)
	}
	syncSecrets(t, lbc, "default/s0", "default/s1")
	slices.Sort(manager.deleted)
	if manager.reloads != 0 || len(manager.set) != 0 || !slices.Equal(manager.deleted, []string{"h0.example.com", "h1.example.com"}) {
		t.Errorf("a deleted and an invalid Secret: %d reloads, set %v, deleted %v", manager.reloads, manager.set, manager.deleted)
	}

	// the invalid one is fixed
	manager.reset()
	lbc.certificatesTask.started = time.Time{}
	if err := secretCache.Update(tlsSecret(t, "s1", "h1.example.com")); err != nil {
		t.Fatal(err)
	}
	syncSecrets(t, lbc, "default/s1")
	if manager.reloads != 0 || !slices.Equal(manager.set, []string{"h1.example.com"}) {
		t.Errorf("a fixed Secret: %d reloads, set %v", manager.reloads, manager.set)
	}
}

func TestCertificatesTaskWaitsForTheInterval(t *testing.T) {
	t.Parallel()
	retryWhenSlow(t, certificatesTaskWaitsForTheInterval)
}

func certificatesTaskWaitsForTheInterval(t *testing.T) bool {
	t.Helper()
	lbc, manager, secretCache := newCertificatesTestLBC(t, 2, tlsSecret(t, "s0", "h0.example.com"), tlsSecret(t, "s1", "h1.example.com"))

	manager.reset()
	if err := secretCache.Update(tlsSecret(t, "s0", "h0.example.com")); err != nil {
		t.Fatal(err)
	}
	syncSecrets(t, lbc, "default/s0")
	if !slices.Equal(manager.set, []string{"h0.example.com"}) {
		t.Fatalf("the first change after a while: set %v, want it at once", manager.set)
	}

	// the next one, right after it, waits for the interval from when the task started
	started := lbc.certificatesTask.started
	manager.reset()
	if err := secretCache.Update(tlsSecret(t, "s1", "h1.example.com")); err != nil {
		t.Fatal(err)
	}
	lbc.sync(task{Kind: secret, Key: "default/s1"})
	if lbc.syncQueue.queue.Len() != 0 {
		if time.Since(started) >= certificatesSyncInterval {
			return false // the runner took longer than the interval: not right after
		}
		t.Fatal("the certificates task was queued at once right after it ran")
	}
	obj, _ := lbc.syncQueue.queue.Get()
	if waited := time.Since(started); waited < certificatesSyncInterval {
		t.Errorf("the certificates task came %v after it last started, want %v", waited, certificatesSyncInterval)
	}
	lbc.sync(obj.(task))
	lbc.syncQueue.queue.Done(obj)
	if !slices.Equal(manager.set, []string{"h1.example.com"}) {
		t.Errorf("after the interval: set %v", manager.set)
	}
	return true
}

func TestIsReferencedOnlyForTLS(t *testing.T) {
	t.Parallel()

	ingress := func(annotations map[string]string) *IngressConfiguration {
		return &IngressConfiguration{Ingress: &networking.Ingress{
			ObjectMeta: meta_v1.ObjectMeta{Name: "ing", Namespace: "default", Annotations: annotations},
			Spec:       networking.IngressSpec{TLS: []networking.IngressTLS{{Hosts: []string{"a.example.com"}, SecretName: "s"}}},
		}}
	}

	if !isReferencedOnlyForTLS("default", "s", ingress(nil)) {
		t.Error("an Ingress that names a Secret in spec.tls only")
	}
	if isReferencedOnlyForTLS("default", "s", ingress(map[string]string{configs.BasicAuthSecretAnnotation: "s"})) {
		t.Error("an Ingress that names its TLS Secret for basic auth too")
	}
	if isReferencedOnlyForTLS("default", "s", ingress(map[string]string{configs.JWTKeyAnnotation: "s"})) {
		t.Error("an Ingress that names its TLS Secret as a JWK too")
	}
	withMinion := ingress(nil)
	withMinion.Minions = []*MinionConfiguration{{Ingress: &networking.Ingress{
		ObjectMeta: meta_v1.ObjectMeta{Name: "minion", Namespace: "default", Annotations: map[string]string{configs.BasicAuthSecretAnnotation: "s"}},
	}}}
	if isReferencedOnlyForTLS("default", "s", withMinion) {
		t.Error("a master whose minion names the Secret for basic auth")
	}
	if isReferencedOnlyForTLS("default", "s", &TransportServerConfiguration{}) {
		t.Error("a TransportServer")
	}
}

// A coalesced task waiting in the queue is no event: with one event besides
// it the controller does not go into batch mode, where endpoints would wait
// for a reload.
func TestCoalescedTaskMakesNoBatch(t *testing.T) {
	t.Parallel()

	lbc := newBatchTestLBC(t, newRecordingBatchManager())
	lbc.scheduleCertificates()
	lbc.scheduleCertificates()
	lbc.syncQueue.queue.Add(task{Kind: endpointslice, Key: "default/es"})
	if got := lbc.syncQueue.queue.Len(); got != 2 {
		t.Fatalf("queue length %d, want the certificates task once and the event", got)
	}
	if got := lbc.syncQueue.Pending(); got != 1 {
		t.Errorf("Pending() = %d, want 1", got)
	}
	obj, _ := lbc.syncQueue.queue.Get()
	lbc.sync(obj.(task))
	lbc.syncQueue.queue.Done(obj)
	if lbc.batchSyncEnabled {
		t.Error("a certificates task and one event made a batch")
	}
	if got := lbc.syncQueue.Pending(); got != 1 {
		t.Errorf("Pending() after the certificates task = %d, want 1", got)
	}
}
