package configs

import "testing"

// A server with an HTTPS port of its own - a custom listener, a Gateway with
// an address of its own - listens on it with TLS passthrough on; only the
// default port and the passthrough listener's are behind the passthrough
// socket.
func TestBehindTLSPassthrough(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		passthrough     bool
		port            int
		customListeners bool
		httpsPort       int
		want            bool
	}{
		{name: "passthrough off", passthrough: false, want: false},
		{name: "the default port", passthrough: true, port: 443, want: true},
		{name: "a custom listener with HTTP only", passthrough: true, port: 443, customListeners: true, want: true},
		{name: "a port of its own", passthrough: true, port: 443, customListeners: true, httpsPort: 27445, want: false},
		{name: "the passthrough port", passthrough: true, port: 443, customListeners: true, httpsPort: 443, want: true},
		{name: "the passthrough port, unset", passthrough: true, customListeners: true, httpsPort: 443, want: true},
		{name: "443 with passthrough elsewhere", passthrough: true, port: 8443, customListeners: true, httpsPort: 443, want: false},
		{name: "another passthrough port", passthrough: true, port: 8443, customListeners: true, httpsPort: 8443, want: true},
	}
	for _, tc := range tests {
		vsc := &virtualServerConfigurator{isTLSPassthrough: tc.passthrough, tlsPassthroughPort: tc.port}
		if got := vsc.behindTLSPassthrough(tc.customListeners, tc.httpsPort); got != tc.want {
			t.Errorf("%s: behindTLSPassthrough(%v, %d) = %v, want %v", tc.name, tc.customListeners, tc.httpsPort, got, tc.want)
		}
	}
}
