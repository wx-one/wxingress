package configs

import (
	"context"
	"testing"
)

func TestUpstreamsShareOneZoneWithTheModule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		upstreamAPI bool
		isPlus      bool
		shared      bool
	}{
		{"the module", true, false, true},
		{"no module", false, false, false},
		{"NGINX Plus", true, true, false},
	} {
		result, _ := generateNginxCfg(NginxCfgParams{
			staticParams:  &StaticConfigParams{UpstreamAPI: tc.upstreamAPI},
			ingEx:         new(createCafeIngressEx()),
			isPlus:        tc.isPlus,
			BaseCfgParams: NewDefaultConfigParams(context.Background(), tc.isPlus),
		})
		if len(result.Upstreams) == 0 {
			t.Fatalf("%s: no upstreams", tc.name)
		}
		for _, u := range result.Upstreams {
			if got := u.Zone == SharedUpstreamZone && u.UpstreamZoneSize == SharedUpstreamZoneSize; got != tc.shared {
				t.Errorf("%s: upstream %s has zone %q of %s, shared want %v", tc.name, u.Name, u.Zone, u.UpstreamZoneSize, tc.shared)
			}
		}
	}
}
