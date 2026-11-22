package k8s

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// reloadingCertificate is the certificate of a pair of files, read again
// when one of them changed - cert-manager or the patch job renews the
// Secret, and the kubelet swaps the files of its volume.
type reloadingCertificate struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   string
	checked time.Time
}

func (r *reloadingCertificate) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cert != nil && time.Since(r.checked) < 10*time.Second {
		return r.cert, nil
	}
	r.checked = time.Now()
	stamp := ""
	for _, f := range []string{r.certFile, r.keyFile} {
		info, err := os.Stat(f)
		if err != nil {
			if r.cert != nil {
				return r.cert, nil
			}
			return nil, err
		}
		stamp += fmt.Sprintf("%d/%d;", info.ModTime().UnixNano(), info.Size())
	}
	if r.cert != nil && stamp == r.stamp {
		return r.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cert != nil {
			return r.cert, nil
		}
		return nil, err
	}
	r.cert, r.stamp = &cert, stamp
	return r.cert, nil
}

// ServeAdmission serves the admission webhook on the address with TLS, ingress-nginx's paths as well as any other.
func ServeAdmission(ctx context.Context, address, certFile, keyFile string, handler http.Handler) error {
	certificate := &reloadingCertificate{certFile: certFile, keyFile: keyFile}
	if _, err := certificate.get(nil); err != nil {
		return fmt.Errorf("the certificate of the webhook: %w", err)
	}
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certificate.get},
		// the controller's context, with its logger
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	return server.ListenAndServeTLS("", "")
}
