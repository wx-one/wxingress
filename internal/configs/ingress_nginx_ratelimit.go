package configs

import (
	"encoding/base64"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/configs/version1"
)

// The rate limit annotations of ingress-nginx.
const (
	IngressNginxLimitRPSAnnotation             = IngressNginxAnnotationPrefix + "limit-rps"
	IngressNginxLimitRPMAnnotation             = IngressNginxAnnotationPrefix + "limit-rpm"
	IngressNginxLimitConnectionsAnnotation     = IngressNginxAnnotationPrefix + "limit-connections"
	IngressNginxLimitBurstMultiplierAnnotation = IngressNginxAnnotationPrefix + "limit-burst-multiplier"
	IngressNginxLimitAllowlistAnnotation       = IngressNginxAnnotationPrefix + "limit-allowlist"
	IngressNginxLimitWhitelistAnnotation       = IngressNginxAnnotationPrefix + "limit-whitelist"
	IngressNginxLimitRateAnnotation            = IngressNginxAnnotationPrefix + "limit-rate"
	IngressNginxLimitRateAfterAnnotation       = IngressNginxAnnotationPrefix + "limit-rate-after"
)

// What ingress-nginx renders for a rate limit when its ConfigMap does not change it.
const (
	ingressNginxDefaultBurstMultiplier = 5
	ingressNginxLimitZoneSize          = "5m"
	ingressNginxLimitConnZoneVariable  = "$binary_remote_addr"
)

var ingressNginxLimitVariableReplacer = strings.NewReplacer("_", "__", "-", "_d")

// parseIngressNginxRateLimitAnnotations reads the rate limit of ingress-nginx.
// As in ingress-nginx, an invalid limit-rps, limit-rpm, limit-connections or
// allowlist drops the whole rate limit, an invalid limit-burst-multiplier takes 5.
// nginx.org/limit-req-rate wins over limit-rps and limit-rpm.
//
//gocyclo:ignore
func parseIngressNginxRateLimitAnnotations(a *ingressNginxAnnotations, ingEx *IngressEx, cfgParams *ConfigParams) {
	if n, ok := a.wholeNumber(IngressNginxLimitRateAnnotation); ok && n > 0 {
		cfgParams.IngressNginxLimitRate = fmt.Sprintf("%dk", n)
	}
	if n, ok := a.wholeNumber(IngressNginxLimitRateAfterAnnotation); ok && n > 0 {
		cfgParams.IngressNginxLimitRateAfter = fmt.Sprintf("%dk", n)
	}

	invalid := false
	limit := func(name string, nginxOrg ...string) int {
		value, exists := a.value(name, nginxOrg...)
		if !exists {
			return 0
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			a.invalid(name, value, "it must be a whole number")
			invalid = true
			return 0
		}
		return n
	}
	rps := limit(IngressNginxLimitRPSAnnotation, "nginx.org/limit-req-rate")
	rpm := limit(IngressNginxLimitRPMAnnotation, "nginx.org/limit-req-rate")
	connections := limit(IngressNginxLimitConnectionsAnnotation)

	multiplier := ingressNginxDefaultBurstMultiplier
	if n, ok := a.wholeNumber(IngressNginxLimitBurstMultiplierAnnotation); ok {
		multiplier = n
	}

	allowlistName := IngressNginxLimitAllowlistAnnotation
	if strings.TrimSpace(a.ing.Annotations[allowlistName]) == "" {
		allowlistName = IngressNginxLimitWhitelistAnnotation
	}
	var allowlist []string
	if value, exists := a.value(allowlistName); exists {
		var err error
		if allowlist, err = parseIngressNginxCIDRs(value); err != nil {
			a.invalid(allowlistName, value, err.Error())
			invalid = true
		}
	}

	if invalid {
		a.warnings.AddWarningf(a.ing, "the rate limit annotations of %s are ignored, as in ingress-nginx", IngressNginxAnnotationPrefix)
		return
	}
	if rps <= 0 && rpm <= 0 && connections <= 0 {
		return
	}

	name := fmt.Sprintf("%v_%v_%v", ingEx.Ingress.Namespace, ingEx.Ingress.Name, ingEx.Ingress.UID)
	rl := &version1.IngressNginxRateLimit{
		Name:      name,
		ID:        ingressNginxLimitVariableReplacer.Replace(strings.ReplaceAll(base64.URLEncoding.EncodeToString([]byte(name)), "=", "")),
		Key:       ingressNginxLimitConnZoneVariable,
		Allowlist: allowlist,
		ZoneSize:  ingressNginxLimitZoneSize,
	}
	if connections > 0 {
		rl.Connections = connections
	}
	if rps > 0 {
		rl.RPS = rps
		rl.RPSBurst = rps * multiplier
	}
	if rpm > 0 {
		rl.RPM = rpm
		rl.RPMBurst = rpm * multiplier
	}
	cfgParams.IngressNginxRateLimit = rl
}

// wholeNumber returns an annotation that is a whole number of at least 0, else warns.
func (a *ingressNginxAnnotations) wholeNumber(name string, nginxOrg ...string) (int, bool) {
	return a.intAtLeast(name, 0, nginxOrg...)
}

// parseIngressNginxCIDRs parses a comma-separated list of addresses and CIDRs
// as ingress-nginx does: networks in their canonical form, sorted.
func parseIngressNginxCIDRs(value string) ([]string, error) {
	seen := make(map[string]bool)
	for _, spec := range strings.Split(value, ",") {
		spec = strings.TrimSpace(spec)
		if _, ipnet, err := net.ParseCIDR(spec); err == nil {
			seen[ipnet.String()] = true
			continue
		}
		ip := net.ParseIP(spec)
		if ip == nil {
			return nil, fmt.Errorf("%q is no address or CIDR", spec)
		}
		seen[ip.String()] = true
	}
	cidrs := make([]string, 0, len(seen))
	for cidr := range seen {
		cidrs = append(cidrs, cidr)
	}
	sort.Strings(cidrs)
	return cidrs, nil
}

// ingressNginxRateLimitsOf returns the rate limit of ingress-nginx the config has to render, if any.
func ingressNginxRateLimitsOf(cfgParams *ConfigParams) []version1.IngressNginxRateLimit {
	if cfgParams.IngressNginxRateLimit == nil {
		return nil
	}
	return []version1.IngressNginxRateLimit{*cfgParams.IngressNginxRateLimit}
}

// applyIngressNginxRateLimit puts the rate limit of ingress-nginx on the location.
func applyIngressNginxRateLimit(loc *version1.Location, cfgParams *ConfigParams) {
	loc.IngressNginxRateLimit = cfgParams.IngressNginxRateLimit
	loc.LimitRate = cfgParams.IngressNginxLimitRate
	loc.LimitRateAfter = cfgParams.IngressNginxLimitRateAfter
}
