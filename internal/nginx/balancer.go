package nginx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	nl "github.com/wx-one/wxingress/internal/logger"
)

// balancersURL is where the wxingress module keeps the balancer rules, on the socket of the upstream API.
const balancersURL = "http://nginx-plus-api/api/wxingress/1/balancers/"

// Balancer is the rule by which the wxingress module chooses the server of
// each request to an upstream: ingress-nginx's balancer for the backend, with
// the algorithm get_implementation gives it and the configuration it reads.
type Balancer struct {
	// Algorithm is round_robin, ewma, chash, chashsubset, sticky_balanced or sticky_persistent.
	Algorithm string `json:"algorithm"`
	// HashBy is upstream-hash-by, nginx variables and text.
	HashBy string `json:"hashBy,omitempty"`
	// SubsetSize is upstream-hash-by-subset-size, for chashsubset.
	SubsetSize int `json:"subsetSize,omitempty"`
	// Backend is ingress-nginx's name of the backend, <namespace>-<service>-<port>:
	// it salts the persistent cookie and names the backend in every cookie.
	Backend string `json:"backend,omitempty"`

	CookieName                    string `json:"cookieName,omitempty"`
	CookiePath                    string `json:"cookiePath,omitempty"`
	CookieDomain                  string `json:"cookieDomain,omitempty"`
	CookieExpires                 string `json:"cookieExpires,omitempty"`
	CookieMaxAge                  string `json:"cookieMaxAge,omitempty"`
	CookieSecure                  bool   `json:"cookieSecure,omitempty"`
	CookieSameSite                string `json:"cookieSameSite,omitempty"`
	CookieConditionalSameSiteNone bool   `json:"cookieConditionalSameSiteNone,omitempty"`
	ChangeOnFailure               bool   `json:"changeOnFailure,omitempty"`
}

var errBalancerNotFound = errors.New("no balancer for that upstream")

// SetBalancer sets the balancer rule of the upstream in the wxingress module.
// The rules live in shared memory and outlive reloads, as the canary rules do.
func (lm *LocalManager) SetBalancer(upstream string, balancer Balancer) error {
	body, err := json.Marshal(balancer)
	if err != nil {
		return fmt.Errorf("error encoding the balancer of %v: %w", upstream, err)
	}

	if err := lm.moduleRequest(balancersURL, http.MethodPut, upstream, body, errBalancerNotFound); err != nil {
		return fmt.Errorf("error setting the balancer of %v: %w", upstream, err)
	}

	nl.Debugf(lm.logger, "Set the balancer of %v: %+v", upstream, balancer)
	return nil
}

// DeleteBalancer deletes the balancer rule of the upstream in the wxingress
// module, which then balances it with the method of its block. A rule that is
// not there is not an error.
func (lm *LocalManager) DeleteBalancer(upstream string) error {
	err := lm.moduleRequest(balancersURL, http.MethodDelete, upstream, nil, errBalancerNotFound)
	if err != nil && !errors.Is(err, errBalancerNotFound) {
		return fmt.Errorf("error deleting the balancer of %v: %w", upstream, err)
	}

	nl.Debugf(lm.logger, "Deleted the balancer of %v", upstream)
	return nil
}
