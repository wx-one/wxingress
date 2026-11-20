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

// canariesURL is where the wxingress module keeps the canary rules, on the socket of the upstream API.
const canariesURL = "http://nginx-plus-api/api/wxingress/1/canaries/"

// Canary is the rule that sends requests for a primary upstream to its canary
// upstream, with the fields and the meaning of ingress-nginx's canary annotations.
type Canary struct {
	Canary        string `json:"canary"`
	Weight        int    `json:"weight"`
	WeightTotal   int    `json:"weightTotal"`
	Header        string `json:"header"`
	HeaderValue   string `json:"headerValue"`
	HeaderPattern string `json:"headerPattern"`
	Cookie        string `json:"cookie"`
	// AffinityCookie is the name of the cookie of sticky cookie of the primary,
	// whose sessions stay on the side they are pinned to.
	AffinityCookie string `json:"affinityCookie,omitempty"`
}

// SetCanary sets the canary rule of the primary upstream in the wxingress module.
// The rules live in shared memory and outlive reloads, so unlike the servers
// of an upstream they need no check of the config version.
func (lm *LocalManager) SetCanary(primary string, canary Canary) error {
	body, err := json.Marshal(canary)
	if err != nil {
		return fmt.Errorf("error encoding the canary of %v: %w", primary, err)
	}

	if err := lm.moduleRequest(canariesURL, http.MethodPut, primary, body, errCanaryNotFound); err != nil {
		return fmt.Errorf("error setting the canary of %v: %w", primary, err)
	}

	nl.Debugf(lm.logger, "Set the canary of %v: %+v", primary, canary)
	return nil
}

// DeleteCanary deletes the canary rule of the primary upstream in the wxingress module.
// A rule that is not there is not an error.
func (lm *LocalManager) DeleteCanary(primary string) error {
	err := lm.moduleRequest(canariesURL, http.MethodDelete, primary, nil, errCanaryNotFound)
	if err != nil && !errors.Is(err, errCanaryNotFound) {
		return fmt.Errorf("error deleting the canary of %v: %w", primary, err)
	}

	nl.Debugf(lm.logger, "Deleted the canary of %v", primary)
	return nil
}

var errCanaryNotFound = errors.New("no canary for that upstream")

// moduleRequest asks the wxingress module to set or delete the rule of an
// upstream under base; notFound is the error of a deletion of a rule it does not have.
func (lm *LocalManager) moduleRequest(base string, method string, upstream string, body []byte, notFound error) error {
	// the client of the config version check is a plain HTTP client on the socket of the upstream API
	if lm.plusConfigVersionCheckClient == nil {
		return errors.New("no client for the upstream API")
	}

	ctx, cancel := context.WithTimeout(context.Background(), lm.verifyClient.timeout)
	defer cancel()

	target := base
	if upstream != "" {
		target += url.PathEscape(upstream)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
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
		return notFound
	default:
		return fmt.Errorf("API returned %v: %s", resp.StatusCode, bytes.TrimSpace(answer))
	}
}
