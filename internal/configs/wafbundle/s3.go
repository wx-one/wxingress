package wafbundle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// S3Config carries the parameters needed to build an S3 client pointing at
// the SeaweedFS filer that the F5 WAF Policy Controller (PLM) publishes
// bundles to.
type S3Config struct {
	// Endpoint is the base URL of the SeaweedFS filer, e.g.
	// https://seaweed.plm.svc.cluster.local:8333. Must include the scheme.
	Endpoint string

	AccessKeyID     string
	SecretAccessKey string

	// CACert, when non-empty, is a PEM bundle appended to the system CA pool.
	CACert []byte

	// ClientCert + ClientKey, when both non-empty, enable mTLS to the filer.
	ClientCert []byte
	ClientKey  []byte //nolint:gosec // G117: PEM private-key bytes for mTLS, not a hardcoded credential

	// InsecureSkipVerify disables TLS verification. Dev/test only.
	InsecureSkipVerify bool
}

// Validate returns an error if the config is missing required fields.
func (c S3Config) Validate() error {
	if c.Endpoint == "" {
		return errors.New("s3: endpoint must not be empty")
	}
	if c.AccessKeyID == "" {
		return errors.New("s3: access key ID must not be empty")
	}
	if c.SecretAccessKey == "" {
		return errors.New("s3: secret access key must not be empty")
	}
	if (len(c.ClientCert) == 0) != (len(c.ClientKey) == 0) {
		return errors.New("s3: client cert and key must both be provided")
	}
	return nil
}

// S3Fetcher fetches WAF bundles from an S3-compatible object store, configured
// for the PLM SeaweedFS filer (path-style addressing, static credentials).
// A fresh S3 client is built per fetch call from the passed-in S3Config.
type S3Fetcher struct{}

// NewS3Fetcher returns a new S3Fetcher.
func NewS3Fetcher() *S3Fetcher { return &S3Fetcher{} }

// FetchPolicyBundle fetches a policy bundle from the s3://bucket/key URL in
// req.URL, using the credentials + TLS material in cfg.
//
// req.Type must be SourceTypePLM. When req.ExpectedChecksum is set, the
// downloaded bytes are rejected unless the hex SHA-256 matches. Transient
// network / 5xx errors are retried by the AWS SDK's own retryer; only the
// SHA mismatch is surfaced as non-transient so the poller stops re-fetching a
// poisoned bundle.
func (f *S3Fetcher) FetchPolicyBundle(ctx context.Context, req *Request, cfg S3Config) (Result, error) {
	return fetchS3Object(ctx, req, cfg)
}

// FetchLogProfileBundle fetches a log-profile bundle. Identical to
// FetchPolicyBundle at the transport level; separate method for symmetry
// with the Fetcher interface.
func (f *S3Fetcher) FetchLogProfileBundle(ctx context.Context, req *Request, cfg S3Config) (Result, error) {
	return fetchS3Object(ctx, req, cfg)
}

func fetchS3Object(ctx context.Context, req *Request, cfg S3Config) (Result, error) {
	if req == nil {
		return Result{}, newNonTransient(errors.New("s3: nil request"))
	}
	if req.Type != SourceTypePLM {
		return Result{}, newNonTransient(fmt.Errorf("s3: unsupported request type %q", req.Type))
	}
	bucket, key, err := parseS3URL(req.URL)
	if err != nil {
		return Result{}, newNonTransient(err)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, effectiveTimeout(req))
	defer cancel()

	bodyReader, err := getS3Object(fetchCtx, bucket, key, cfg, effectiveRetries(req))
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = bodyReader.Close() }()

	body, err := io.ReadAll(io.LimitReader(bodyReader, MaxBundleSize+1))
	if err != nil {
		return Result{}, fmt.Errorf("s3: read bundle body: %w", err)
	}
	if int64(len(body)) > MaxBundleSize {
		return Result{}, newNonTransient(fmt.Errorf("s3: bundle exceeds max size %d bytes", MaxBundleSize))
	}

	computed := ComputeChecksum(body)
	if req.ExpectedChecksum != "" && !strings.EqualFold(computed, req.ExpectedChecksum) {
		return Result{}, newNonTransient(fmt.Errorf(
			"s3: bundle sha256 mismatch: expected %s, got %s", req.ExpectedChecksum, computed))
	}

	return Result{Data: body, Checksum: computed}, nil
}

// parseS3URL splits an "s3://bucket/key" URL into its bucket and key.
func parseS3URL(rawURL string) (bucket, key string, err error) {
	if rawURL == "" {
		return "", "", errors.New("s3: url must not be empty")
	}
	u, parseErr := url.Parse(rawURL)
	if parseErr != nil {
		return "", "", fmt.Errorf("s3: parse url %q: %w", rawURL, parseErr)
	}
	if u.Scheme != "s3" {
		return "", "", fmt.Errorf("s3: url scheme must be s3, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("s3: url must contain a bucket, got %q", rawURL)
	}
	key = strings.TrimPrefix(u.Path, "/")
	if key == "" {
		return "", "", fmt.Errorf("s3: url must contain a key, got %q", rawURL)
	}
	return u.Host, key, nil
}

// newNonTransient wraps err so isNonTransient returns true.
func newNonTransient(err error) error {
	if err == nil {
		return nil
	}
	return &nonTransientError{cause: err}
}
