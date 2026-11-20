// Package s3 provides a WAF bundle fetcher for PLM's S3-compatible storage (SeaweedFS).
package s3

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-logr/logr"

	sharedsecrets "github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph/shared/secrets"
)

// Credentials holds S3 auth credentials resolved from Kubernetes Secrets.
type Credentials struct {
	// AccessKeyID is the S3 access key ID.
	AccessKeyID string
	// SecretAccessKey is the S3 secret access key.
	SecretAccessKey string
}

// TLSConfig holds TLS configuration resolved from Kubernetes Secrets.
type TLSConfig struct {
	// CAData is the PEM-encoded CA certificate for verifying the S3 endpoint.
	CAData []byte
	// CertData is the PEM-encoded client certificate for mutual TLS.
	CertData []byte
	// KeyData is the PEM-encoded client private key for mutual TLS.
	KeyData []byte
}

// Fetcher downloads WAF bundles from PLM's S3-compatible storage (SeaweedFS).
type Fetcher struct {
	logger     logr.Logger
	endpoint   string
	skipVerify bool
}

// NewFetcher creates a new S3 fetcher for the given endpoint.
// The endpoint and skipVerify are fixed at startup from CLI flags.
func NewFetcher(logger logr.Logger, endpoint string, skipVerify bool) *Fetcher {
	return &Fetcher{
		logger:     logger,
		endpoint:   endpoint,
		skipVerify: skipVerify,
	}
}

// FetchBundle downloads a bundle from the given S3 location and verifies its SHA-256 checksum.
// A new S3 client is constructed per call with the provided credentials and TLS config
// (fetches are infrequent and event-driven, so client caching is unnecessary).
// The location must be an s3:// URI (e.g. "s3://bucket/path/bundle.tgz").
// creds may be nil for anonymous access; tlsCfg may be nil to use system CAs.
func (f *Fetcher) FetchBundle(
	ctx context.Context,
	location string,
	expectedSHA256 string,
	creds *Credentials,
	tlsCfg *TLSConfig,
) ([]byte, error) {
	bucket, key, err := parseS3URI(location)
	if err != nil {
		return nil, fmt.Errorf("invalid S3 location: %w", err)
	}

	body, err := f.getObject(ctx, bucket, key, creds, tlsCfg)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("failed to read S3 object body: %w", err)
	}

	if expectedSHA256 != "" {
		actualHash := sha256.Sum256(data)
		actualHex := hex.EncodeToString(actualHash[:])
		if !strings.EqualFold(actualHex, expectedSHA256) {
			return nil, fmt.Errorf(
				"SHA-256 mismatch for %s: expected %s, got %s",
				location,
				expectedSHA256,
				actualHex,
			)
		}
	}

	f.logger.V(1).Info(
		"Successfully fetched PLM bundle",
		"bucket", bucket,
		"key", key,
		"size", len(data),
	)

	return data, nil
}

// parseS3URI parses an s3://bucket/key URI into bucket and key components.
func parseS3URI(uri string) (bucket, key string, err error) {
	if uri == "" {
		return "", "", fmt.Errorf("empty S3 URI")
	}

	if !strings.HasPrefix(uri, "s3://") {
		return "", "", fmt.Errorf("URI must start with s3://: %s", uri)
	}

	path := strings.TrimPrefix(uri, "s3://")
	if path == "" {
		return "", "", fmt.Errorf("missing bucket in S3 URI: %s", uri)
	}

	slashIdx := strings.IndexByte(path, '/')
	if slashIdx < 0 || slashIdx == len(path)-1 {
		return "", "", fmt.Errorf("missing key in S3 URI: %s", uri)
	}

	bucket = path[:slashIdx]
	key = path[slashIdx+1:]

	if bucket == "" {
		return "", "", fmt.Errorf("empty bucket in S3 URI: %s", uri)
	}

	if key == "" {
		return "", "", fmt.Errorf("empty key in S3 URI: %s", uri)
	}

	return bucket, key, nil
}

// buildTLSTransport creates an HTTP transport with custom TLS configuration.
// It clones http.DefaultTransport to preserve default proxy/timeout/connection-reuse settings
// and appends any custom CA to the system cert pool.
// Returns (nil, nil) if no custom TLS is needed.
func buildTLSTransport(tlsCfg *TLSConfig, skipVerify bool) (*http.Transport, error) {
	if tlsCfg == nil && !skipVerify {
		return nil, nil //nolint:nilnil // nil transport means "use default", not an error
	}

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("http.DefaultTransport is not *http.Transport")
	}
	transport := defaultTransport.Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: skipVerify, //nolint:gosec // controlled by CLI flag
	}

	if tlsCfg != nil {
		if len(tlsCfg.CAData) > 0 {
			pool, err := x509.SystemCertPool()
			if err != nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(tlsCfg.CAData) {
				return nil, fmt.Errorf("failed to append CA certificate to pool")
			}
			transport.TLSClientConfig.RootCAs = pool
		}

		if len(tlsCfg.CertData) > 0 && len(tlsCfg.KeyData) > 0 {
			cert, err := tls.X509KeyPair(tlsCfg.CertData, tlsCfg.KeyData)
			if err != nil {
				return nil, fmt.Errorf("failed to load client TLS key pair: %w", err)
			}
			transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
		}
	}

	return transport, nil
}

func validateTLSConfig(tlsCfg *TLSConfig) error {
	// This validates TLS semantics for the assembled client config. It intentionally does not
	// know about individual Kubernetes Secrets; that context is handled during secret resolution.
	if tlsCfg == nil {
		return nil
	}

	if len(tlsCfg.CAData) > 0 {
		if err := sharedsecrets.ValidateCA(tlsCfg.CAData); err != nil {
			return fmt.Errorf("invalid CA bundle: %w", err)
		}
	}

	hasCert := len(tlsCfg.CertData) > 0
	hasKey := len(tlsCfg.KeyData) > 0

	if hasCert != hasKey {
		return errors.New("client certificate and key must both be provided")
	}

	if hasCert {
		if err := sharedsecrets.ValidateTLS(tlsCfg.CertData, tlsCfg.KeyData); err != nil {
			return fmt.Errorf("invalid client certificate or key: %w", err)
		}
	}

	return nil
}
