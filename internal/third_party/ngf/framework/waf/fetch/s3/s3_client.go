//go:build s3

package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// wxingress patch (hack/ngf-patches): the S3 client of PLM bundles, built
// only with the tag "s3". WAF bundles are NGINX App Protect's, which
// wxingress does not ship, and the AWS SDK was the largest part of the
// controller's binary.

// getObject opens the object's body, for the caller to read and close.
func (f *Fetcher) getObject(ctx context.Context, bucket, key string, creds *Credentials, tlsCfg *TLSConfig) (io.ReadCloser, error) {
	client, err := f.buildClient(ctx, creds, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to build S3 client: %w", err)
	}

	f.logger.V(1).Info(
		"Fetching PLM bundle from S3",
		"bucket", bucket,
		"key", key,
	)

	result, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("S3 GetObject failed for s3://%s/%s: %w", bucket, key, err)
	}
	return result.Body, nil
}

// buildClient creates a new S3 client with the provided credentials and TLS configuration.
func (f *Fetcher) buildClient(
	ctx context.Context,
	creds *Credentials,
	tlsCfg *TLSConfig,
) (*s3.Client, error) {
	// Validate the fully assembled TLS config at the fetcher boundary. Secret resolution only
	// checks Secret shape/presence so it can log which Kubernetes Secret is misconfigured.
	if err := validateTLSConfig(tlsCfg); err != nil {
		return nil, fmt.Errorf("invalid TLS configuration: %w", err)
	}

	var opts []func(*awsconfig.LoadOptions) error

	// Set credentials
	if creds != nil && creds.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				creds.AccessKeyID,
				creds.SecretAccessKey,
				"",
			),
		))
	} else {
		opts = append(opts, awsconfig.WithCredentialsProvider(aws.AnonymousCredentials{}))
	}

	// Build TLS config
	transport, err := buildTLSTransport(tlsCfg, f.skipVerify)
	if err != nil {
		return nil, fmt.Errorf("failed to build TLS transport: %w", err)
	}
	if transport != nil {
		opts = append(opts, awsconfig.WithHTTPClient(&http.Client{
			Transport: transport,
		}))
	}

	// Use a dummy region since SeaweedFS doesn't care about regions
	opts = append(opts, awsconfig.WithRegion("us-east-1"))

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(f.endpoint)
		o.UsePathStyle = true // Required for SeaweedFS
	})

	return client, nil
}
