package nginx

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
)

// APITokenHeader is the header the controller names itself with on the socket
// of the upstream API. The server of that socket refuses a request without it
// before the request reaches the wxingress module.
const APITokenHeader = "X-Wxingress-Api-Token"

// APITokenFile is the name of the file, in the secrets directory, with the
// check of the token that the server of the upstream API includes. It starts
// with an underscore, which no namespace and no Secret has, so the file of a
// Secret (<namespace>-<name>) is never this one.
const APITokenFile = "_wxingress-api-token.conf"

// NewAPIToken makes the token of this run of the controller: 32 random bytes,
// in hex.
func NewAPIToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("error making the token of the upstream API: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// APITokenConf is what the server of the upstream API includes: every request
// without the token is refused. It is a file of its own, written with mode
// 0600, so that the token is in neither nginx.conf nor the controller's log of
// it.
func APITokenConf(token string) []byte {
	return []byte(fmt.Sprintf("if ($http_x_wxingress_api_token != \"%s\") {\n    return 403;\n}\n", token))
}

// apiTokenTransport sends the token with every request.
type apiTokenTransport struct {
	token string
	next  http.RoundTripper
}

func (t apiTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(APITokenHeader, t.token)
	return t.next.RoundTrip(r)
}

// WithAPIToken makes the client send the token with every request. Without a
// token it is the client as it was.
func WithAPIToken(c *http.Client, token string) *http.Client {
	if token == "" {
		return c
	}
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.Transport = apiTokenTransport{token: token, next: next}
	return c
}
