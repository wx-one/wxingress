package configs

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	nl "github.com/wx-one/wxingress/internal/logger"
)

// The certificates of hosts as ingress-nginx serves them: not named in the
// configuration, where every change would be a reload, but set by name in the
// wxingress module, which picks one for each handshake by the name the client
// sent (exact, then the wildcard one level up, then "_"), as ingress-nginx's
// certificate.lua does. Every TLS server names the default server's
// certificate, which is what a name without one gets, as ingress-nginx's
// fake certificate is. The files are still written, as without the module:
// the module is given what is in them, and a host's certificate changes when
// its file does.

// defaultCertificateName is the name of the certificate for every name that
// has none of its own, ingress-nginx's DEFAULT_CERT_HOSTNAME.
const defaultCertificateName = "_"

// defaultCertificate is the certificate every TLS server names when the
// certificates come from the wxingress module, and "" when they do not.
func (p *StaticConfigParams) defaultCertificate() string {
	if p == nil || !p.UpstreamAPI {
		return ""
	}
	return DefaultServerSecretPath
}

// certificatesOfIngress is the certificate file of each host of an Ingress
// config, as the config would have named it without the module. A host whose
// TLS Secret is missing or invalid has none - ingress-nginx serves it the
// default certificate.
func certificatesOfIngress(cfg version1.IngressNginxConfig) map[string]string {
	hosts := make(map[string]string)
	for _, server := range cfg.Servers {
		// a host served with the default certificate needs no name of its own: the module answers it with that one
		if !server.SSL || server.IsDefaultServer || server.SSLRejectHandshake || server.SSLCertificate == "" || server.SSLCertificate == DefaultServerSecretPath {
			continue
		}
		hosts[server.Name] = server.SSLCertificate
	}
	return hosts
}

// certificatesOfVirtualServer is the certificate file of the host of a VirtualServer config.
// The host of a VirtualServer on a listener of a Gateway with an address of
// its own is bound to the listener's nginx port (host:port), as the wxingress
// module takes it: another Gateway can have the same host with another
// certificate, and the catch-all host of a listener without a hostname is
// the port's default (_:port), where "_" is the controller's for every port.
func certificatesOfVirtualServer(cfg version2.VirtualServerConfig) map[string]string {
	hosts := make(map[string]string)
	ssl := cfg.Server.SSL
	if ssl != nil && !ssl.RejectHandshake && ssl.Certificate != "" {
		name := cfg.Server.ServerName
		if cfg.Server.CustomListeners && cfg.Server.HTTPSPort > 0 && strings.HasPrefix(cfg.Server.VSName, gatewayNamePrefix) {
			if name == gatewayCatchAllHost {
				name = defaultCertificateName
			}
			name += ":" + strconv.Itoa(cfg.Server.HTTPSPort)
		}
		hosts[name] = ssl.Certificate
	}
	return hosts
}

// gatewayCatchAllHost is the host of a listener of a Gateway without a
// hostname (internal/gateway).
const gatewayCatchAllHost = "~^"

// setCertificates records the certificate files of the hosts of a config.
func (cnf *Configurator) setCertificates(name string, hosts map[string]string) {
	if len(hosts) == 0 {
		delete(cnf.certificates, name)
		return
	}
	cnf.certificates[name] = hosts
}

// wantedCertificates is the certificate file of every name the module is to
// have: every host of every config, and "_" with the default server's.
func (cnf *Configurator) wantedCertificates() map[string]string {
	wanted := make(map[string]string)
	// sorted, so that a host two configs claim gets the same file every time
	for _, name := range slices.Sorted(maps.Keys(cnf.certificates)) {
		for host, file := range cnf.certificates[name] {
			if _, exists := wanted[host]; !exists {
				wanted[host] = file
			}
		}
	}
	wanted[defaultCertificateName] = DefaultServerSecretPath
	return wanted
}

// appliedCertificate is what the module was given for a name: from which
// file, and what was in it.
type appliedCertificate struct {
	file   string
	digest [sha256.Size]byte
}

// givenCertificates is what the module is to be given for every name: the
// file of each name and what is in it. A name whose file is not there has
// none: a Secret that went, or no default certificate - either way the name
// gets the configured one. The files are read only when they changed since
// they were last read, which their size and time of change tell: every
// Ingress update that changes no config compares them all.
func (cnf *Configurator) givenCertificates() map[string]appliedCertificate {
	l := nl.LoggerFromContext(cnf.CfgParams.Context)
	given := make(map[string]appliedCertificate)
	for host, file := range cnf.wantedCertificates() {
		digest, err := cnf.certificateDigest(file)
		if err != nil {
			if host != defaultCertificateName {
				nl.Warnf(l, "The certificate of %v is not in %v: %v", host, file, err)
			}
			continue
		}
		given[host] = appliedCertificate{file: file, digest: digest}
	}
	return given
}

// fileDigest is the digest of a file when it had a size and a time of change,
// and whether that time was old enough when it was read to tell the next
// change apart.
type fileDigest struct {
	size    int64
	modTime time.Time
	digest  [sha256.Size]byte
	settled bool
}

// modTimeGranularity is how long a file's time of change may stay the same
// while it is written again: file systems take it from a clock that moves on
// by a tick - a few milliseconds on Linux without fine-grained timestamps, a
// second on some - so a Secret changed twice within one tick to as many bytes
// keeps its size and time of change.
const modTimeGranularity = time.Second

// certificateDigest is the digest of what is in a certificate file, read
// again only when the file's size or time of change differ from when it was
// last read. The controller writes a Secret's file anew - to a temporary file
// it renames - for every change, so a change has a time of its own, but only
// to a tick of the file system's clock: as git trusts its index only for files
// that changed before it was written, a digest is trusted only for a file
// whose time of change was more than modTimeGranularity before it was looked
// at, and a file that changed later than that is read every time.
func (cnf *Configurator) certificateDigest(file string) ([sha256.Size]byte, error) {
	now := time.Now()
	info, statErr := cnf.statCertificate(file)
	if statErr == nil {
		if known, exists := cnf.certificateDigests[file]; exists && known.settled && known.size == info.Size() && known.modTime.Equal(info.ModTime()) {
			return known.digest, nil
		}
	}
	content, err := cnf.readCertificate(file)
	if err != nil {
		delete(cnf.certificateDigests, file)
		return [sha256.Size]byte{}, err
	}
	digest := sha256.Sum256(content)
	if statErr == nil {
		settled := info.ModTime().Before(now.Add(-modTimeGranularity))
		cnf.certificateDigests[file] = fileDigest{size: info.Size(), modTime: info.ModTime(), digest: digest, settled: settled}
	}
	return digest, nil
}

// certificatesChanged tells if the module's certificates are not those of the configs.
func (cnf *Configurator) certificatesChanged() bool {
	if !cnf.staticCfgParams.UpstreamAPI {
		return false
	}
	return !maps.Equal(cnf.givenCertificates(), cnf.appliedCertificates)
}

// syncCertificates gives the wxingress module the certificate of every host
// whose file changed since it was given it, and takes away those of hosts
// that have none any more. The module keeps its certificates over reloads but
// starts without any, so the first reload after NGINX starts sets them all,
// as ingress-nginx sends all of them after it starts.
func (cnf *Configurator) syncCertificates() error {
	cnf.refreshCertificateInfos()
	if !cnf.isReloadsEnabled {
		cnf.certificatesHeld = true
		return nil
	}
	return cnf.pushCertificates()
}

func (cnf *Configurator) pushCertificates() error {
	if !cnf.staticCfgParams.UpstreamAPI {
		return nil
	}
	cnf.certificatesHeld = false

	var errs []error
	defer func() { cnf.certificatesFailed = len(errs) > 0 }()
	given := cnf.givenCertificates()

	for _, host := range slices.Sorted(maps.Keys(given)) {
		if applied, exists := cnf.appliedCertificates[host]; exists && applied == given[host] {
			continue
		}
		content, err := cnf.readCertificate(given[host].file)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		certificate, key := splitCertificateAndKey(content)
		if err := cnf.nginxManager.SetCertificate(host, certificate, key); err != nil {
			errs = append(errs, err)
			continue
		}
		// what the module has is what was read now
		cnf.appliedCertificates[host] = appliedCertificate{file: given[host].file, digest: sha256.Sum256(content)}
	}

	for _, host := range slices.Sorted(maps.Keys(cnf.appliedCertificates)) {
		if _, exists := given[host]; exists {
			continue
		}
		if err := cnf.nginxManager.DeleteCertificate(host); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(cnf.appliedCertificates, host)
	}

	return errors.Join(errs...)
}

// SyncCertificates gives the wxingress module the certificates whose files
// changed without a change to the configuration - a TLS Secret that was
// changed, added or deleted, or that of the default server or the wildcard
// Secret. It does so even while reloads wait for the end of a batch: the
// module has the certificates by name, not by server, so they do not wait for
// the configuration, and the controller calls it only once NGINX runs.
func (cnf *Configurator) SyncCertificates() error {
	return cnf.pushCertificates()
}

// DynamicCertificates reports whether the certificates of hosts are set in the
// wxingress module rather than named in the configuration.
func (cnf *Configurator) DynamicCertificates() bool {
	return cnf.staticCfgParams.UpstreamAPI
}

// splitCertificateAndKey takes the file the controller writes for a TLS
// Secret - tls.crt, then tls.key - apart again: the certificates, and the key.
func splitCertificateAndKey(content []byte) (certificate, key string) {
	var certificates, keys bytes.Buffer
	rest := content
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			_ = pem.Encode(&certificates, block)
		} else {
			_ = pem.Encode(&keys, block)
		}
	}
	return certificates.String(), keys.String()
}

func readCertificateFile(file string) ([]byte, error) {
	return os.ReadFile(file) //nolint:gosec // the files are the controller's own
}

// GenerateFakeCertificate makes the certificate ingress-nginx serves for a
// name that has none, when no default certificate is given: self-signed for
// ingress.local, valid for a year, the cert and the key in one PEM.
func GenerateFakeCertificate() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate the fake certificate's key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate the fake certificate's serial number: %w", err)
	}

	notBefore := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Acme Co"},
			CommonName:   "Kubernetes Ingress Controller Fake Certificate",
		},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"ingress.local"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("failed to create the fake certificate: %w", err)
	}

	var content bytes.Buffer
	_ = pem.Encode(&content, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = pem.Encode(&content, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return content.Bytes(), nil
}
