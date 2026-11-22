package configs

import (
	"fmt"
	"strings"
)

// ingressNginxKnownVariables are the nginx variables an annotation of
// ingress-nginx may use where it takes variables: those of nginx's core,
// proxy and FastCGI modules, which every http context knows, and those the
// main template defines for every server. nginx -t fails on a variable it
// does not know, and with it the reload of every Ingress, so an annotation
// with any other variable is refused like any other invalid value.
var ingressNginxKnownVariables = map[string]bool{
	"args":                      true,
	"binary_remote_addr":        true,
	"content_length":            true,
	"content_type":              true,
	"document_root":             true,
	"document_uri":              true,
	"fastcgi_path_info":         true,
	"fastcgi_script_name":       true,
	"host":                      true,
	"hostname":                  true,
	"https":                     true,
	"is_args":                   true,
	"proxy_add_x_forwarded_for": true,
	"proxy_host":                true,
	"proxy_port":                true,
	"query_string":              true,
	"realpath_root":             true,
	"remote_addr":               true,
	"remote_port":               true,
	"remote_user":               true,
	"request_filename":          true,
	"request_id":                true,
	"request_method":            true,
	"request_uri":               true,
	"scheme":                    true,
	"server_addr":               true,
	"server_name":               true,
	"server_port":               true,
	"server_protocol":           true,
	"uri":                       true,
	// main template: a map of every http block (njs or the wxingress module)
	"escaped_request_uri": true,
}

// ingressNginxVariablePrefixes are the prefixes of the variables nginx makes
// of a request header, a query argument and a cookie: $http_x_user,
// $arg_page, $cookie_session.
var ingressNginxVariablePrefixes = []string{"http_", "arg_", "cookie_"}

// isIngressNginxVariableChar tells whether c may be part of the name of an nginx variable.
func isIngressNginxVariableChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// isKnownIngressNginxVariable tells whether nginx knows the variable in every
// http context. nginx takes the names of variables without regard to case.
func isKnownIngressNginxVariable(name string) bool {
	name = strings.ToLower(name)
	if ingressNginxKnownVariables[name] {
		return true
	}
	for _, prefix := range ingressNginxVariablePrefixes {
		if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" {
			return true
		}
	}
	return false
}

// checkIngressNginxVariables returns an error when the value has a variable
// nginx would not take: a "$" without a name, a name nginx does not know in
// every http context (isKnownIngressNginxVariable) or a "${" without its
// "}". The captures of a regular expression, $1 to $9, are taken as nginx
// takes them: a digit after the "$" is the capture, what follows is text.
// Where bareBraces is false, a "{" or "}" may only be those of ${name}: the
// value goes into the configuration unquoted, where a brace opens or ends a
// block.
func checkIngressNginxVariables(value string, bareBraces bool) error {
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '{', '}':
			if !bareBraces {
				return fmt.Errorf("a %q is only allowed in ${name}", value[i])
			}
			continue
		case '$':
		default:
			continue
		}
		i++
		if i == len(value) {
			return fmt.Errorf("a $ ends it without a variable name")
		}
		if value[i] >= '1' && value[i] <= '9' {
			continue
		}
		braced := value[i] == '{'
		if braced {
			i++
		}
		start := i
		for i < len(value) && isIngressNginxVariableChar(value[i]) {
			i++
		}
		name := value[start:i]
		if name == "" {
			return fmt.Errorf("a $ is not followed by a variable name")
		}
		if braced {
			if i == len(value) || value[i] != '}' {
				return fmt.Errorf("${%s has no closing }", name)
			}
		} else {
			i--
		}
		if !isKnownIngressNginxVariable(name) {
			return fmt.Errorf("$%s is not a variable nginx knows in every server; allowed are $1 to $9, $http_*, $arg_*, $cookie_* and %s",
				name, strings.Join(sortedKnownIngressNginxVariables(), ", "))
		}
	}
	return nil
}

// sortedKnownIngressNginxVariables are the names of ingressNginxKnownVariables as $name, sorted.
func sortedKnownIngressNginxVariables() []string {
	names := make([]string, 0, len(ingressNginxKnownVariables))
	for _, name := range sortedKeys(ingressNginxKnownVariables) {
		names = append(names, "$"+name)
	}
	return names
}
