package nginx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// oidcRevocationsURL is where the wxingress module keeps the revocations of oidc sessions.
const oidcRevocationsURL = "http://nginx-plus-api/api/wxingress/1/oidc/revocations"

// OIDCRevocation ends the sessions of a key - "sub:<sub>", "sid:<sid>" or
// "ses:<session>" - that were made before Before (unix seconds).
type OIDCRevocation struct {
	Key    string `json:"key"`
	Before int64  `json:"before"`
	// Local is set on the ones a logout of the IdP or /logout added on this replica.
	Local bool `json:"local,omitempty"`
}

// OIDCRevocations are all of them, and All ends every session made before it.
type OIDCRevocations struct {
	All     int64            `json:"all"`
	Entries []OIDCRevocation `json:"entries"`
}

// SetOIDCRevocations gives the wxingress module the revocations, which replace
// what it has except its local entries it was not given yet. No reload: they
// live in shared memory.
func (lm *LocalManager) SetOIDCRevocations(r OIDCRevocations) error {
	if r.Entries == nil {
		r.Entries = []OIDCRevocation{}
	}
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("error encoding the oidc revocations: %w", err)
	}
	return lm.moduleRequest(oidcRevocationsURL, http.MethodPut, "", body, nil)
}

// LocalOIDCRevocations are the revocations a logout added in the module of this replica.
func (lm *LocalManager) LocalOIDCRevocations() ([]OIDCRevocation, error) {
	if lm.plusConfigVersionCheckClient == nil {
		return nil, fmt.Errorf("no client for the upstream API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), lm.verifyClient.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oidcRevocationsURL+"?local=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := lm.plusConfigVersionCheckClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error asking for the local oidc revocations: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned %v: %s", resp.StatusCode, raw)
	}
	var r OIDCRevocations
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("error reading the local oidc revocations: %w", err)
	}
	return r.Entries, nil
}
