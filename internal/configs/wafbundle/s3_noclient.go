//go:build !s3

package wafbundle

import (
	"context"
	"errors"
	"io"
)

// getS3Object: this build has no S3 client - the tag "s3" builds it in - so
// a PLM bundle cannot be fetched, and never will be by retrying.
func getS3Object(_ context.Context, _, _ string, cfg S3Config, _ int) (io.ReadCloser, error) {
	if err := cfg.Validate(); err != nil {
		return nil, newNonTransient(err)
	}
	return nil, newNonTransient(errors.New("s3: this build fetches no bundles from S3 (it is built without the tag s3)"))
}
