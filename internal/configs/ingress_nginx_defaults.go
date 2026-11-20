package configs

import (
	"context"
	"fmt"
)

// The defaults of ingress-nginx (internal/ingress/controller/config/config.go
// and its nginx.tmpl) where they differ from those of the controller or of
// nginx, for -ingress-nginx-defaults. The ConfigMap and the annotations still
// win over them.
const (
	ingressNginxDefaultProxyConnectTimeout  = "5s"
	ingressNginxDefaultClientBodyBufferSize = "8k"
	ingressNginxDefaultProxyNextUpstream    = "error timeout"
	ingressNginxDefaultProxyNextTimeout     = "0"
	ingressNginxDefaultProxyNextTries       = 3
	ingressNginxDefaultProxyRequestBuffer   = "on"
	ingressNginxDefaultProxyRedirect        = "off"
	ingressNginxDefaultHTTPRedirectCode     = 308
	ingressNginxDefaultHSTSMaxAge           = 31536000
	ingressNginxDefaultWorkerConnections    = "16384"
	ingressNginxDefaultWorkerShutdown       = "240s"
	ingressNginxDefaultVariablesHashMaxSize = 2048
	ingressNginxDefaultUpstreamKeepalive    = 64
	ingressNginxDefaultLimitStatusCode      = 503
	ingressNginxDefaultSSLProtocols         = "TLSv1.2 TLSv1.3"
	ingressNginxDefaultSSLCiphers           = "ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305:DHE-RSA-AES256-GCM-SHA384:DHE-RSA-AES128-GCM-SHA256"
)

// NewDefaultConfigParamsFor returns the defaults of the controller or, with
// ingressNginxDefaults, those of ingress-nginx.
func NewDefaultConfigParamsFor(ctx context.Context, isPlus bool, ingressNginxDefaults bool) *ConfigParams {
	cfgParams := NewDefaultConfigParams(ctx, isPlus)
	if ingressNginxDefaults {
		ApplyIngressNginxDefaults(cfgParams)
	}
	return cfgParams
}

// ApplyIngressNginxDefaults sets every value ingress-nginx has a default for
// and the controller renders to the default of ingress-nginx. Where the
// default of the controller is already that of ingress-nginx, or nginx's own
// default is, nothing changes.
func ApplyIngressNginxDefaults(cfgParams *ConfigParams) {
	cfgParams.IngressNginxDefaults = true

	// proxy-connect-timeout 5, proxy-read-timeout and proxy-send-timeout 60 as here
	cfgParams.ProxyConnectTimeout = ingressNginxDefaultProxyConnectTimeout
	// client-body-buffer-size, rendered in every location
	cfgParams.ClientBodyBufferSize = ingressNginxDefaultClientBodyBufferSize
	// proxy-buffering off, proxy_buffer_size 4k and proxy_buffers 4 4k
	cfgParams.ProxyBuffering = false
	cfgParams.ProxyBufferSize = ingressNginxDefaultProxyBufferSize
	cfgParams.ProxyBuffers = proxyBuffersOf(ingressNginxDefaultProxyBuffersNumber, ingressNginxDefaultProxyBufferSize)
	cfgParams.ProxyRequestBuffering = ingressNginxDefaultProxyRequestBuffer
	// proxy-next-upstream "error timeout", its timeout 0 and its tries 3, where nginx tries every server
	cfgParams.ProxyNextUpstream = ingressNginxDefaultProxyNextUpstream
	cfgParams.ProxyNextUpstreamTimeout = ingressNginxDefaultProxyNextTimeout
	tries := uint64(ingressNginxDefaultProxyNextTries)
	cfgParams.ProxyNextUpstreamTries = &tries
	// proxy-redirect-from off: nginx's own default is proxy_redirect default
	cfgParams.ProxyRedirectFrom = ingressNginxDefaultProxyRedirect
	cfgParams.ProxyRedirectTo = ""
	// upstream-keepalive-connections: ingress-nginx's 320 is the idle
	// connections a worker keeps for all backends together, in the one
	// upstream its Lua balancer is; here every Service port is an upstream of
	// its own, and nginx allocates each one's cache whole at configuration
	// (some 140 bytes an entry, 45 kB an upstream at 320 - 5 MB of every
	// pod's memory for a hundred Ingresses). 64 idle connections per worker
	// and Service; the ConfigMap key sets any other number.
	cfgParams.Keepalive = ingressNginxDefaultUpstreamKeepalive

	// ssl-redirect true as here, with http-redirect-code 308
	cfgParams.HTTPRedirectCode = ingressNginxDefaultHTTPRedirectCode
	// hsts true, hsts-include-subdomains true, hsts-max-age 31536000, hsts-preload false
	cfgParams.HSTS = true
	cfgParams.HSTSIncludeSubdomains = true
	cfgParams.HSTSMaxAge = ingressNginxDefaultHSTSMaxAge
	cfgParams.HSTSPreload = false
	// use-http2 true
	cfgParams.HTTP2 = true
	// server-tokens false
	cfgParams.ServerTokens = "off"

	// limit-req-status-code and limit-conn-status-code 503
	cfgParams.LimitReqRejectCode = ingressNginxDefaultLimitStatusCode

	// ssl-protocols, ssl-ciphers and with them ssl_prefer_server_ciphers on
	cfgParams.MainServerSSLProtocols = ingressNginxDefaultSSLProtocols
	cfgParams.MainServerSSLCiphers = ingressNginxDefaultSSLCiphers
	cfgParams.MainServerSSLPreferServerCiphers = true

	// max-worker-connections 16384, worker-shutdown-timeout 240s, variables-hash-max-size 2048
	cfgParams.MainWorkerConnections = ingressNginxDefaultWorkerConnections
	cfgParams.MainWorkerShutdownTimeout = ingressNginxDefaultWorkerShutdown
	cfgParams.VariablesHashMaxSize = ingressNginxDefaultVariablesHashMaxSize
}

// proxyBuffersOf renders proxy_buffers as ingress-nginx does: the number of
// buffers and the size of proxy_buffer_size.
func proxyBuffersOf(number int, size string) string {
	return fmt.Sprintf("%d %s", number, size)
}
