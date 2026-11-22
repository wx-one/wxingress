package k8s

import (
	"testing"

	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// TestValidatePathRefusesWhatPCRERefuses checks that a path, which nginx
// compiles with PCRE as the regular expression of use-regex, path-regex or
// rewrite-target, is refused where PCRE would refuse it although regexp2
// compiles it: nginx -t would fail, and with it the reload of every resource.
func TestValidatePathRefusesWhatPCRERefuses(t *testing.T) {
	t.Parallel()
	pathType := networking.PathTypeImplementationSpecific
	for _, path := range []string{
		"/(?<a>x)(?<-a>y)",
		"/(?<a>x)(?<b-a>y)",
		"/(?<1>x)",
		"/(?(abc)x|y)",
		"/(?<=a+)b",
		`/\p{IsGreek}`,
	} {
		if allErrs := validatePath(path, &pathType, field.NewPath("path")); len(allErrs) == 0 {
			t.Errorf("validatePath(%q) returned no errors", path)
		}
	}
	for _, path := range []string{
		"/(?<name>[a-z]+)/(.*)",
		"/api/(v[0-9]+)/(.*)",
		"/(?<=ab)c",
	} {
		if allErrs := validatePath(path, &pathType, field.NewPath("path")); len(allErrs) != 0 {
			t.Errorf("validatePath(%q) returned errors %v", path, allErrs)
		}
	}
}
