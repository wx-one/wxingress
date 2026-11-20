package configs

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/k8s/secrets"
	"github.com/wx-one/wxingress/internal/nginx"
)

// certificateManager remembers configs to tell if they changed, and counts
// reloads and the certificates set in and taken away from the module.
type certificateManager struct {
	*canaryManager
	certificates map[string]string
	set          []string
	deleted      []string
}

func newCertificateManager() *certificateManager {
	return &certificateManager{canaryManager: newCanaryManager(), certificates: make(map[string]string)}
}

func (m *certificateManager) SetCertificate(name string, certificate string, key string) error {
	m.set = append(m.set, name)
	m.certificates[name] = certificate + key
	return nil
}

func (m *certificateManager) DeleteCertificate(name string) error {
	m.deleted = append(m.deleted, name)
	delete(m.certificates, name)
	return nil
}

func (m *certificateManager) reset() {
	m.canaryManager.reset()
	m.set = nil
	m.deleted = nil
}

// files is a secrets directory in memory.
type files map[string]string

func (f files) read(file string) ([]byte, error) {
	content, exists := f[file]
	if !exists {
		return nil, fs.ErrNotExist
	}
	return []byte(content), nil
}

func pemOf(kind, text string) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: []byte(text)}))
}

func secretFile(name string) string {
	return pemOf("CERTIFICATE", name+" leaf") + pemOf("CERTIFICATE", name+" chain") + "\n" + pemOf("PRIVATE KEY", name+" key")
}

const cafeSecretFile = "/etc/nginx/secrets/default-cafe-secret"

func TestConfiguratorCertificateLifecycle(t *testing.T) {
	t.Parallel()
	manager := newCertificateManager()
	cnf := createCanaryConfigurator(t, manager)
	disk := files{cafeSecretFile: secretFile("one"), DefaultServerSecretPath: secretFile("default")}
	cnf.readCertificate = disk.read

	ingEx := createCafeIngressEx()
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.canaryManager.reloads != 1 {
		t.Errorf("a new Ingress: reloads = %d, want 1", manager.canaryManager.reloads)
	}
	if want := []string{"_", "cafe.example.com"}; !slices.Equal(manager.set, want) {
		t.Errorf("a new Ingress: set %v, want %v", manager.set, want)
	}
	if got := manager.certificates["cafe.example.com"]; !strings.Contains(got, "BEGIN CERTIFICATE") || !strings.Contains(got, "PRIVATE KEY") {
		t.Errorf("the module was given %q", got)
	}

	// the config names the default certificate, never the Secret's file
	config := manager.canaryManager.configs["default-cafe-ingress"]
	if !strings.Contains(config, "ssl_certificate /etc/nginx/secrets/default;") || strings.Contains(config, "default-cafe-secret") {
		t.Errorf("the config names another certificate:\n%s", config)
	}

	// the same again: nothing to give
	manager.reset()
	if _, err := cnf.AddOrUpdateResources(ExtendedResources{IngressExes: []*IngressEx{&ingEx}}, false); err != nil {
		t.Fatal(err)
	}
	if manager.canaryManager.reloads != 0 || len(manager.set) != 0 || len(manager.deleted) != 0 {
		t.Errorf("nothing changed: reloads %d, set %v, deleted %v", manager.canaryManager.reloads, manager.set, manager.deleted)
	}

	// the Secret changes: its file does, the config does not
	manager.reset()
	disk[cafeSecretFile] = secretFile("two")
	if _, err := cnf.AddOrUpdateResources(ExtendedResources{IngressExes: []*IngressEx{&ingEx}}, false); err != nil {
		t.Fatal(err)
	}
	if manager.canaryManager.reloads != 0 {
		t.Errorf("a changed Secret: reloads = %d, want 0", manager.canaryManager.reloads)
	}
	if want := []string{"cafe.example.com"}; !slices.Equal(manager.set, want) {
		t.Errorf("a changed Secret: set %v, want %v", manager.set, want)
	}

	// the Secret goes: the host has the default certificate, and still no reload
	manager.reset()
	delete(disk, cafeSecretFile)
	ingEx.SecretRefs[secrets.RefKey("default/cafe-secret", secrets.RoleTLS)] = &secrets.SecretReference{
		Path:  cafeSecretFile,
		Error: errors.New("secret default/cafe-secret doesn't exist"),
	}
	if _, err := cnf.AddOrUpdateResources(ExtendedResources{IngressExes: []*IngressEx{&ingEx}}, false); err != nil {
		t.Fatal(err)
	}
	if manager.canaryManager.reloads != 0 {
		t.Errorf("a Secret that went: reloads = %d, want 0", manager.canaryManager.reloads)
	}
	if want := []string{"cafe.example.com"}; !slices.Equal(manager.deleted, want) || len(manager.set) != 0 {
		t.Errorf("a Secret that went: set %v, deleted %v, want deleted %v", manager.set, manager.deleted, want)
	}
	if config := manager.canaryManager.configs["default-cafe-ingress"]; strings.Contains(config, "ssl_reject_handshake") {
		t.Errorf("a host without its Secret rejects handshakes rather than having the default certificate:\n%s", config)
	}

	// and comes back
	manager.reset()
	disk[cafeSecretFile] = secretFile("three")
	ingEx.SecretRefs[secrets.RefKey("default/cafe-secret", secrets.RoleTLS)] = &secrets.SecretReference{Path: cafeSecretFile}
	if _, err := cnf.AddOrUpdateResources(ExtendedResources{IngressExes: []*IngressEx{&ingEx}}, false); err != nil {
		t.Fatal(err)
	}
	if manager.canaryManager.reloads != 0 || !slices.Equal(manager.set, []string{"cafe.example.com"}) {
		t.Errorf("a Secret that came back: reloads %d, set %v", manager.canaryManager.reloads, manager.set)
	}

	// the default certificate changes
	manager.reset()
	disk[DefaultServerSecretPath] = secretFile("default two")
	if err := cnf.SyncCertificates(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(manager.set, []string{"_"}) {
		t.Errorf("a new default certificate: set %v", manager.set)
	}

	// the Ingress goes, and its host's certificate with it
	manager.reset()
	if err := cnf.DeleteIngress("default/cafe-ingress", false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(manager.deleted, []string{"cafe.example.com"}) {
		t.Errorf("a deleted Ingress: deleted %v", manager.deleted)
	}
}

func TestConfiguratorCertificateChangeWithoutReload(t *testing.T) {
	t.Parallel()
	manager := newCertificateManager()
	cnf := createCanaryConfigurator(t, manager)
	disk := files{cafeSecretFile: secretFile("one"), "/etc/nginx/secrets/default-other-secret": secretFile("other")}
	cnf.readCertificate = disk.read

	ingEx := createCafeIngressEx()
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}

	// the Ingress names another Secret: the config is the same, so only the module changes
	manager.reset()
	ingEx.Ingress.Spec.TLS[0].SecretName = "other-secret"
	ingEx.SecretRefs = map[secrets.SecretRefKey]*secrets.SecretReference{
		secrets.RefKey("default/other-secret", secrets.RoleTLS): {Path: "/etc/nginx/secrets/default-other-secret"},
	}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.canaryManager.reloads != 0 || !slices.Equal(manager.set, []string{"cafe.example.com"}) {
		t.Errorf("another Secret: reloads %d, set %v", manager.canaryManager.reloads, manager.set)
	}
	if !strings.Contains(manager.certificates["cafe.example.com"], pemOf("CERTIFICATE", "other leaf")) {
		t.Errorf("the module has %q", manager.certificates["cafe.example.com"])
	}
}

func TestConfiguratorCertificatesNeedTheModule(t *testing.T) {
	t.Parallel()
	manager := newCertificateManager()
	cnf := createCanaryConfigurator(t, manager)
	cnf.staticCfgParams.UpstreamAPI = false
	cnf.readCertificate = files{cafeSecretFile: secretFile("one")}.read

	ingEx := createCafeIngressEx()
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if len(manager.set) != 0 {
		t.Errorf("without the module: set %v", manager.set)
	}
	if config := manager.canaryManager.configs["default-cafe-ingress"]; !strings.Contains(config, "default-cafe-secret") {
		t.Errorf("without the module the config does not name the Secret:\n%s", config)
	}
}

func TestCertificatesOfVirtualServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ssl  *version2.SSL
		want map[string]string
	}{
		{name: "no TLS", ssl: nil, want: map[string]string{}},
		{name: "a Secret", ssl: &version2.SSL{Certificate: "/s/a"}, want: map[string]string{"cafe.example.com": "/s/a"}},
		{name: "an invalid Secret", ssl: &version2.SSL{RejectHandshake: true}, want: map[string]string{}},
	}
	for _, test := range tests {
		got := certificatesOfVirtualServer(version2.VirtualServerConfig{Server: version2.Server{ServerName: "cafe.example.com", SSL: test.ssl}})
		if fmt.Sprint(got) != fmt.Sprint(test.want) {
			t.Errorf("%s: %v, want %v", test.name, got, test.want)
		}
	}
}

func TestCertificatesOfGatewayListenersAreBoundToTheirPorts(t *testing.T) {
	t.Parallel()
	server := func(vs, host string) version2.Server {
		return version2.Server{
			VSName: vs, ServerName: host, CustomListeners: true, HTTPSPort: 20443,
			SSL: &version2.SSL{Certificate: "/s/gw"},
		}
	}
	for _, test := range []struct {
		name   string
		server version2.Server
		want   map[string]string
	}{
		{name: "a host", server: server("GW-0123", "x.example.com"), want: map[string]string{"x.example.com:20443": "/s/gw"}},
		{name: "the catch-all", server: server("GW-0123", "~^"), want: map[string]string{"_:20443": "/s/gw"}},
		{name: "a VirtualServer from the API on a listener", server: server("cafe", "x.example.com"), want: map[string]string{"x.example.com": "/s/gw"}},
	} {
		got := certificatesOfVirtualServer(version2.VirtualServerConfig{Server: test.server})
		if fmt.Sprint(got) != fmt.Sprint(test.want) {
			t.Errorf("%s: %v, want %v", test.name, got, test.want)
		}
	}
}

func TestSplitCertificateAndKey(t *testing.T) {
	t.Parallel()
	certificate, key := splitCertificateAndKey([]byte(secretFile("one")))
	if certificate != pemOf("CERTIFICATE", "one leaf")+pemOf("CERTIFICATE", "one chain") {
		t.Errorf("certificate %q", certificate)
	}
	if key != pemOf("PRIVATE KEY", "one key") {
		t.Errorf("key %q", key)
	}

	certificate, key = splitCertificateAndKey([]byte(pemOf("CERTIFICATE", "c") + pemOf("EC PRIVATE KEY", "k")))
	if certificate != pemOf("CERTIFICATE", "c") || key != pemOf("EC PRIVATE KEY", "k") {
		t.Errorf("an EC key: %q %q", certificate, key)
	}
}

func TestGenerateFakeCertificate(t *testing.T) {
	t.Parallel()
	content, err := GenerateFakeCertificate()
	if err != nil {
		t.Fatal(err)
	}
	certificate, key := splitCertificateAndKey(content)
	block, _ := pem.Decode([]byte(certificate))
	if block == nil {
		t.Fatal("no certificate")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subject.CommonName != "Kubernetes Ingress Controller Fake Certificate" ||
		!slices.Equal(parsed.Subject.Organization, []string{"Acme Co"}) ||
		!slices.Equal(parsed.DNSNames, []string{"ingress.local"}) {
		t.Errorf("the fake certificate is %v %v", parsed.Subject, parsed.DNSNames)
	}
	keyBlock, _ := pem.Decode([]byte(key))
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		t.Fatalf("the key is %q", key)
	}
	if _, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		t.Error(err)
	}
}

var _ nginx.Manager = newCertificateManager()

func TestConfiguratorCertificatesOfABatch(t *testing.T) {
	t.Parallel()
	manager := newCertificateManager()
	cnf := createCanaryConfigurator(t, manager)
	disk := files{cafeSecretFile: secretFile("one")}
	cnf.readCertificate = disk.read

	ingEx := createCafeIngressEx()
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}

	// Secrets changed in a batch: nothing is given while reloads are off, and no reload is asked for
	manager.reset()
	cnf.DisableReloads()
	disk[cafeSecretFile] = secretFile("two")
	if _, err := cnf.AddOrUpdateResources(ExtendedResources{IngressExes: []*IngressEx{&ingEx}}, false); err != nil {
		t.Fatal(err)
	}
	if cnf.ReloadAsked() || len(manager.set) != 0 {
		t.Errorf("during the batch: reload asked %v, set %v", cnf.ReloadAsked(), manager.set)
	}

	// the batch ends without a reload, and the module gets what changed
	cnf.EnableReloads()
	if err := cnf.ReloadForBatchUpdates(false); err != nil {
		t.Fatal(err)
	}
	if manager.canaryManager.reloads != 0 || !slices.Equal(manager.set, []string{"cafe.example.com"}) {
		t.Errorf("after the batch: reloads %d, set %v", manager.canaryManager.reloads, manager.set)
	}

	// a change in a batch that does ask for one
	cnf.DisableReloads()
	if _, err := cnf.AddOrUpdateResources(ExtendedResources{IngressExes: []*IngressEx{&ingEx}}, true); err != nil {
		t.Fatal(err)
	}
	if !cnf.ReloadAsked() {
		t.Error("a reload asked for during the batch is not remembered")
	}
}

func TestConfiguratorCertificateDigestsAreCached(t *testing.T) {
	t.Parallel()
	manager := newCertificateManager()
	cnf := createCanaryConfigurator(t, manager)
	dir := t.TempDir()
	file := dir + "/default-cafe-secret"
	if err := os.WriteFile(file, []byte(secretFile("one")), 0o600); err != nil {
		t.Fatal(err)
	}
	// written a while ago: its digest is kept
	written := time.Now().Add(-time.Minute)
	if err := os.Chtimes(file, written, written); err != nil {
		t.Fatal(err)
	}
	reads := 0
	cnf.readCertificate = func(name string) ([]byte, error) {
		if name == file {
			reads++
		}
		return os.ReadFile(name) //nolint:gosec // a file of the test
	}
	cnf.setCertificates("default-cafe-ingress", map[string]string{"cafe.example.com": file})

	if err := cnf.SyncCertificates(); err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Errorf("the first push read %d times, want 2: the digest and what is given", reads)
	}

	// unchanged: compared without reading
	reads = 0
	for range 3 {
		if cnf.certificatesChanged() {
			t.Error("an unchanged file is taken for a changed one")
		}
	}
	if reads != 0 {
		t.Errorf("unchanged files were read %d times", reads)
	}

	// changed - written anew, as the secret store writes it: read again, and given
	later := time.Now().Add(time.Second)
	if err := os.WriteFile(file, []byte(secretFile("two")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	if !cnf.certificatesChanged() {
		t.Error("a changed file is taken for an unchanged one")
	}
	manager.reset()
	if err := cnf.SyncCertificates(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(manager.set, []string{"cafe.example.com"}) || !strings.Contains(manager.certificates["cafe.example.com"], pemOf("CERTIFICATE", "two leaf")) {
		t.Errorf("after the change: set %v, the module has %q", manager.set, manager.certificates["cafe.example.com"])
	}

	// written again within one tick of the file system's clock, to as many
	// bytes: the same size and time of change, and still taken for a change
	tick := time.Now().Truncate(time.Millisecond)
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(file, []byte(secretFile(name)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, tick, tick); err != nil {
			t.Fatal(err)
		}
		if !cnf.certificatesChanged() {
			t.Errorf("%v written again within one tick is taken for an unchanged file", name)
		}
		manager.reset()
		if err := cnf.SyncCertificates(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(manager.set, []string{"cafe.example.com"}) || !strings.Contains(manager.certificates["cafe.example.com"], pemOf("CERTIFICATE", name+" leaf")) {
			t.Errorf("%v written again within one tick: set %v, the module has %q", name, manager.set, manager.certificates["cafe.example.com"])
		}
	}

	// gone: no digest, and the name is taken away
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	manager.reset()
	if err := cnf.SyncCertificates(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(manager.deleted, []string{"cafe.example.com"}) {
		t.Errorf("a file that went: deleted %v", manager.deleted)
	}
	if _, exists := cnf.certificateDigests[file]; exists {
		t.Error("the digest of a file that went is kept")
	}
}
