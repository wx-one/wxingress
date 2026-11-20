//go:build s3

package wafbundle

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The S3 client of PLM bundles, in a build made with the tag "s3": WAF
// bundles are NGINX App Protect's, which wxingress does not ship, and the
// AWS SDK was the largest part of the controller's binary.

// getS3Object opens the object's body, for the caller to read and close.
func getS3Object(ctx context.Context, bucket, key string, cfg S3Config, maxAttempts int) (io.ReadCloser, error) {
	client, err := buildS3Client(cfg, maxAttempts)
	if err != nil {
		return nil, newNonTransient(err)
	}
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3: get object %s/%s: %w", bucket, key, err)
	}
	return out.Body, nil
}

// buildS3Client constructs an aws-sdk-go-v2 S3 client pointing at cfg.Endpoint
// with static credentials and path-style addressing (required by SeaweedFS).
// maxAttempts caps the SDK's built-in retryer, which already backs off on
// transient 5xx / throttling / network errors and does not retry 4xx.
func buildS3Client(cfg S3Config, maxAttempts int) (*s3.Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	httpClient, err := buildS3HTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return s3.New(s3.Options{
		// SeaweedFS ignores the region, but the AWS SDK requires a non-empty value.
		Region:           "us-east-1",
		Credentials:      credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		BaseEndpoint:     aws.String(cfg.Endpoint),
		UsePathStyle:     true,
		HTTPClient:       httpClient,
		RetryMaxAttempts: maxAttempts,
	}), nil
}

// buildS3HTTPClient returns an HTTP client with TLS 1.2+ configured per cfg.
func buildS3HTTPClient(cfg S3Config) (*awshttp.BuildableClient, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	tlsCfg.InsecureSkipVerify = cfg.InsecureSkipVerify //nolint:gosec // documented dev/test only

	if len(cfg.CACert) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(cfg.CACert) {
			return nil, errors.New("s3: failed to parse CA certificate bundle")
		}
		tlsCfg.RootCAs = pool
	}
	if len(cfg.ClientCert) > 0 && len(cfg.ClientKey) > 0 {
		cert, err := tls.X509KeyPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("s3: invalid client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.TLSClientConfig = tlsCfg
	}), nil
}
