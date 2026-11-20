package version1

import (
	"fmt"
	"strings"
)

// HTTP is the config of the rate limit in the http context: the allowlist,
// the key of who is limited and the zones. The Ingress templates render it,
// and the Gateway path puts it into the http context of a route's
// VirtualServer, so that both have the same.
func (rl IngressNginxRateLimit) HTTP() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# rate limit %s\n", rl.Name)
	fmt.Fprintf(&b, "geo $remote_addr $allowlist_%s {\n\tdefault 0;\n", rl.ID)
	for _, ip := range rl.Allowlist {
		fmt.Fprintf(&b, "\t%s 1;\n", ip)
	}
	b.WriteString("}\n")
	fmt.Fprintf(&b, "map $allowlist_%s $limit_%s {\n\t0 %s;\n\t1 \"\";\n}", rl.ID, rl.ID, rl.Key)
	if rl.Connections > 0 {
		fmt.Fprintf(&b, "\nlimit_conn_zone $limit_%s zone=%s_conn:%s;", rl.ID, rl.Name, rl.ZoneSize)
	}
	if rl.RPM > 0 {
		fmt.Fprintf(&b, "\nlimit_req_zone $limit_%s zone=%s_rpm:%s rate=%dr/m;", rl.ID, rl.Name, rl.ZoneSize, rl.RPM)
	}
	if rl.RPS > 0 {
		fmt.Fprintf(&b, "\nlimit_req_zone $limit_%s zone=%s_rps:%s rate=%dr/s;", rl.ID, rl.Name, rl.ZoneSize, rl.RPS)
	}
	return b.String()
}

// Directives are the location directives of the rate limit, each ending
// with ';'.
func (rl IngressNginxRateLimit) Directives() []string {
	var out []string
	if rl.Connections > 0 {
		out = append(out, fmt.Sprintf("limit_conn %s_conn %d;", rl.Name, rl.Connections), "limit_conn_status 503;")
	}
	if rl.RPS > 0 {
		out = append(out, fmt.Sprintf("limit_req zone=%s_rps burst=%d nodelay;", rl.Name, rl.RPSBurst))
	}
	if rl.RPM > 0 {
		out = append(out, fmt.Sprintf("limit_req zone=%s_rpm burst=%d nodelay;", rl.Name, rl.RPMBurst))
	}
	if rl.RPS > 0 || rl.RPM > 0 {
		out = append(out, "limit_req_status 503;")
	}
	return out
}
