package nginx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	nl "github.com/wx-one/wxingress/internal/logger"
)

// certificatesURL is where the wxingress module keeps the certificates of hosts, on the socket of the upstream API.
const certificatesURL = "http://nginx-plus-api/api/wxingress/1/certificates/"

// SetCertificate sets the certificate of a name in the wxingress module: a
// host, a wildcard, or "_", the one for every name without one. The
// certificates live in shared memory, outlive reloads, and are used from the
// next TLS handshake on.
func (lm *LocalManager) SetCertificate(name string, certificate string, key string) error {
	body, err := json.Marshal(map[string]string{"certificate": certificate, "key": key})
	if err != nil {
		return fmt.Errorf("error encoding the certificate of %v: %w", name, err)
	}

	if err := lm.certificateRequest(http.MethodPut, name, body); err != nil {
		return fmt.Errorf("error setting the certificate of %v: %w", name, err)
	}

	nl.Debugf(lm.logger, "Set the certificate of %v", name)
	return nil
}

// DeleteCertificate takes the certificate of a name away from the wxingress module.
// A certificate that is not there is not an error.
func (lm *LocalManager) DeleteCertificate(name string) error {
	err := lm.certificateRequest(http.MethodDelete, name, nil)
	if err != nil && !errors.Is(err, errCertificateNotFound) {
		return fmt.Errorf("error deleting the certificate of %v: %w", name, err)
	}

	nl.Debugf(lm.logger, "Deleted the certificate of %v", name)
	return nil
}

var errCertificateNotFound = errors.New("no certificate for that name")

func (lm *LocalManager) certificateRequest(method string, name string, body []byte) error {
	if lm.plusConfigVersionCheckClient == nil {
		return errors.New("no client for the upstream API")
	}

	ctx, cancel := context.WithTimeout(context.Background(), lm.verifyClient.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, certificatesURL+url.PathEscape(name), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := lm.plusConfigVersionCheckClient.Do(req)
	if err != nil {
		return fmt.Errorf("error doing request: %w", err)
	}
	defer resp.Body.Close()

	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusNotFound && method == http.MethodDelete:
		return errCertificateNotFound
	default:
		return fmt.Errorf("API returned %v: %s", resp.StatusCode, bytes.TrimSpace(answer))
	}
}
