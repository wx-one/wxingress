// Package conformance runs the Gateway API conformance suite against
// wxingress: see run.sh.
package conformance

import (
	"testing"

	"sigs.k8s.io/gateway-api/conformance"
)

func TestConformance(t *testing.T) {
	conformance.RunConformance(t)
}
