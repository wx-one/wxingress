package configs

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/wx-one/wxingress/internal/metrics/collectors"
)

// certificateMetrics is what ingress-nginx's ssl_certificate_info and
// ssl_expire_time_seconds say of the certificate of each host: made by the
// sync loop when the certificates are synced, read by the metrics handler.
type certificateMetrics struct {
	mu    sync.Mutex
	infos []collectors.CertificateInfo
	// parsed are the certificates of the files read, by file and digest
	parsed map[string]parsedCertificate
}

type parsedCertificate struct {
	digest [32]byte
	cert   *x509.Certificate
}

// CertificateInfos are the certificates of the hosts served, for the
// metrics. Safe to call beside the sync loop.
func (cnf *Configurator) CertificateInfos() []collectors.CertificateInfo {
	cnf.certMetrics.mu.Lock()
	defer cnf.certMetrics.mu.Unlock()
	return cnf.certMetrics.infos
}

// refreshCertificateInfos makes the infos of the certificates of every host
// a config has, the Secret of each from the references of the resources.
// A file is parsed again only when its digest changed, and its digest is
// read again only when its size or time of change did (certificateDigest).
func (cnf *Configurator) refreshCertificateInfos() {
	if !cnf.isPrometheusEnabled {
		return
	}
	secretOf := map[string][2]string{}
	for _, ingEx := range cnf.ingresses {
		for _, ref := range ingEx.SecretRefs {
			if ref != nil && ref.Secret != nil && ref.Path != "" {
				secretOf[ref.Path] = [2]string{ref.Secret.Namespace, ref.Secret.Name}
			}
		}
	}
	for _, vsEx := range cnf.virtualServers {
		for _, ref := range vsEx.SecretRefs {
			if ref != nil && ref.Secret != nil && ref.Path != "" {
				secretOf[ref.Path] = [2]string{ref.Secret.Namespace, ref.Secret.Name}
			}
		}
	}
	if cnf.certMetrics.parsed == nil {
		cnf.certMetrics.parsed = map[string]parsedCertificate{}
	}
	wanted := cnf.wantedCertificates()
	infos := make([]collectors.CertificateInfo, 0, len(wanted))
	seen := map[string]bool{}
	for _, host := range slices.Sorted(maps.Keys(wanted)) {
		file := wanted[host]
		secret, known := secretOf[file]
		if host == defaultCertificateName || !known {
			continue
		}
		digest, err := cnf.certificateDigest(file)
		if err != nil {
			continue
		}
		seen[file] = true
		p, ok := cnf.certMetrics.parsed[file]
		if !ok || p.digest != digest {
			content, err := cnf.readCertificate(file)
			if err != nil {
				continue
			}
			p = parsedCertificate{digest: digest, cert: firstCertificate(content)}
			cnf.certMetrics.parsed[file] = p
		}
		if p.cert == nil {
			continue
		}
		info := collectors.CertificateInfo{
			Host: host, Namespace: secret[0], SecretName: secret[1],
			// as ingress-nginx's: the serial number and the issuer's key
			Identifier:         fmt.Sprintf("%s-%x", p.cert.SerialNumber.String(), p.cert.AuthorityKeyId),
			IssuerCommonName:   p.cert.Issuer.CommonName,
			SerialNumber:       p.cert.SerialNumber.String(),
			PublicKeyAlgorithm: p.cert.PublicKeyAlgorithm.String(),
			NotAfter:           p.cert.NotAfter,
		}
		if len(p.cert.Issuer.Organization) > 0 {
			info.IssuerOrganization = p.cert.Issuer.Organization[0]
		}
		infos = append(infos, info)
	}
	for file := range cnf.certMetrics.parsed {
		if !seen[file] {
			delete(cnf.certMetrics.parsed, file)
		}
	}
	cnf.certMetrics.mu.Lock()
	cnf.certMetrics.infos = infos
	cnf.certMetrics.mu.Unlock()
}

// firstCertificate is the first certificate of a PEM file (a Secret's file has the key beside it).
func firstCertificate(content []byte) *x509.Certificate {
	for {
		var block *pem.Block
		block, content = pem.Decode(content)
		if block == nil {
			return nil
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err == nil {
				return cert
			}
		}
	}
}
