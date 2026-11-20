//go:build !s3

package s3

import (
	"context"
	"errors"
	"io"
)

// getObject (wxingress patch, hack/ngf-patches): this build has no S3
// client - the tag "s3" builds it in - so a PLM bundle cannot be fetched.
func (f *Fetcher) getObject(_ context.Context, _, _ string, _ *Credentials, tlsCfg *TLSConfig) (io.ReadCloser, error) {
	if err := validateTLSConfig(tlsCfg); err != nil {
		return nil, err
	}
	return nil, errors.New("this build fetches no bundles from S3 (it is built without the tag s3)")
}
