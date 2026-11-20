// Command upstreamapi drives the data plane's upstream API the way the
// controller does, with the same client, and checks what nginx then does.
//
// It is run by tests/upstream-api.sh against an nginx it started. Every step
// is a call NGINX Ingress Controller makes in its LocalManager
// (internal/nginx/manager.go), with the server parameters it sends, followed
// by requests through the proxy to see which backend answers.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nginx/nginx-plus-go-client/v3/client"
)

var failed int

func check(what string, got, want any) {
	if fmt.Sprint(got) == fmt.Sprint(want) {
		fmt.Printf("ok       %s\n", what)
		return
	}
	fmt.Printf("FAIL     %s\n  wanted: %v\n  got:    %v\n", what, want, got)
	failed++
}

// servers is what the controller sends for a list of endpoints: its
// defaults from the ConfigMap, max_fails 1, max_conns 0, fail_timeout 10s.
func servers(maxFails int, addresses ...string) []client.UpstreamServer {
	maxConns := 0
	var out []client.UpstreamServer
	for _, address := range addresses {
		out = append(out, client.UpstreamServer{
			Server:      address,
			MaxFails:    &maxFails,
			MaxConns:    &maxConns,
			FailTimeout: "10s",
		})
	}
	return out
}

// streamServers is what the controller sends for the endpoints of a
// TransportServer upstream: the parameters the upstream gives its servers.
func streamServers(maxFails int, failTimeout string, addresses ...string) []client.StreamUpstreamServer {
	maxConns := 0
	var out []client.StreamUpstreamServer
	for _, address := range addresses {
		out = append(out, client.StreamUpstreamServer{
			Server:      address,
			MaxFails:    &maxFails,
			MaxConns:    &maxConns,
			FailTimeout: failTimeout,
		})
	}
	return out
}

// fresh asks with a connection of its own each time: the stream block
// proxies a connection to one server for as long as it lasts.
var fresh = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

// through asks the proxy `count` times and answers who replied, sorted.
func through(front string, count int) string {
	seen := map[string]bool{}
	for range count {
		response, err := fresh.Get(front)
		if err != nil {
			seen["error"] = true
			continue
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			seen[fmt.Sprint(response.StatusCode)] = true
			continue
		}
		seen[strings.TrimSpace(string(body))] = true
	}
	var names []string
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

func main() {
	socket := flag.String("socket", "", "the upstream API's unix socket")
	front := flag.String("front", "", "the proxy, as a URL")
	upstream := flag.String("upstream", "", "the zoned upstream behind the proxy")
	first := flag.String("first", "", "the first backend's address")
	second := flag.String("second", "", "the second backend's address")
	streamFront := flag.String("stream-front", "", "the TCP proxy of the stream block, as a URL")
	streamUpstream := flag.String("stream-upstream", "", "the zoned stream upstream behind it")
	flag.Parse()

	// as cmd/nginx-ingress/main.go getSocketClient and createPlusClient build it
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", *socket)
			},
		},
	}
	api, err := client.NewNginxClient("http://nginx-plus-api/api", client.WithHTTPClient(httpClient))
	if err != nil {
		fmt.Printf("FAIL     the client: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()

	// the config-version guard the manager asks before every update
	request, _ := http.NewRequest(http.MethodGet, "http://nginx-plus-api/configVersionCheck", nil)
	request.Header.Set("x-expected-config-version", "1")
	response, err := httpClient.Do(request)
	if err == nil {
		check("the configuration version is the expected one", response.StatusCode, 200)
		response.Body.Close()
	} else {
		check("the configuration version is the expected one", err, nil)
	}

	check("before: the placeholder answers", through(*front, 4), "502")

	added, removed, updated, err := api.UpdateHTTPServers(ctx, *upstream, servers(1, *first))
	check("endpoints replace the placeholder", fmt.Sprint(len(added), len(removed), len(updated), err), "1 1 0 <nil>")
	check("and are used", through(*front, 8), "first")

	added, removed, updated, err = api.UpdateHTTPServers(ctx, *upstream, servers(1, *first, *second))
	check("a pod more", fmt.Sprint(len(added), len(removed), len(updated), err), "1 0 0 <nil>")
	check("and every worker uses it", through(*front, 16), "first second")

	// the controller sends the whole list on every sync; nothing must change
	added, removed, updated, err = api.UpdateHTTPServers(ctx, *upstream, servers(1, *first, *second))
	check("the same list again changes nothing", fmt.Sprint(len(added), len(removed), len(updated), err), "0 0 0 <nil>")

	added, removed, updated, err = api.UpdateHTTPServers(ctx, *upstream, servers(3, *first, *second))
	check("a changed max_fails is an update", fmt.Sprint(len(added), len(removed), len(updated), err), "0 0 2 <nil>")
	listed, err := api.GetHTTPServers(ctx, *upstream)
	if err == nil && len(listed) == 2 {
		check("and is what nginx has", *listed[0].MaxFails, 3)
	} else {
		check("and is what nginx has", err, "two servers")
	}

	added, removed, updated, err = api.UpdateHTTPServers(ctx, *upstream, servers(3, *second))
	check("a pod less", fmt.Sprint(len(added), len(removed), len(updated), err), "0 1 0 <nil>")
	check("and it gets nothing", through(*front, 8), "second")

	added, removed, updated, err = api.UpdateHTTPServers(ctx, *upstream, nil)
	check("no pods at all", fmt.Sprint(len(added), len(removed), len(updated), err), "0 1 0 <nil>")
	check("is a 502, as with the placeholder", through(*front, 4), "502")

	_, _, _, err = api.UpdateHTTPServers(ctx, "no-such-upstream", servers(1, *first))
	check("an upstream that is not there is an error", err != nil, true)

	// a TransportServer's upstream, in the stream block: TCP proxied to the same backends
	check("stream: before, the placeholder socket takes no connection", through(*streamFront, 4), "error")

	sAdded, sRemoved, sUpdated, err := api.UpdateStreamServers(ctx, *streamUpstream, streamServers(3, "30s", *first))
	check("stream: endpoints replace the placeholder", fmt.Sprint(len(sAdded), len(sRemoved), len(sUpdated), err), "1 1 0 <nil>")
	check("stream: and are used", through(*streamFront, 8), "first")

	sAdded, sRemoved, sUpdated, err = api.UpdateStreamServers(ctx, *streamUpstream, streamServers(3, "30s", *first, *second))
	check("stream: a pod more", fmt.Sprint(len(sAdded), len(sRemoved), len(sUpdated), err), "1 0 0 <nil>")
	check("stream: and every worker uses it", through(*streamFront, 16), "first second")

	sAdded, sRemoved, sUpdated, err = api.UpdateStreamServers(ctx, *streamUpstream, streamServers(3, "30s", *first, *second))
	check("stream: the same list again changes nothing", fmt.Sprint(len(sAdded), len(sRemoved), len(sUpdated), err), "0 0 0 <nil>")

	streamListed, err := api.GetStreamServers(ctx, *streamUpstream)
	if err == nil && len(streamListed) == 2 {
		check("stream: the servers have the upstream's parameters", fmt.Sprint(*streamListed[0].MaxFails, " ", streamListed[0].FailTimeout), "3 30s")
	} else {
		check("stream: the servers have the upstream's parameters", err, "two servers")
	}

	sAdded, sRemoved, sUpdated, err = api.UpdateStreamServers(ctx, *streamUpstream, streamServers(5, "30s", *first, *second))
	check("stream: a changed max_fails is an update", fmt.Sprint(len(sAdded), len(sRemoved), len(sUpdated), err), "0 0 2 <nil>")

	httpListed, err := api.GetHTTPServers(ctx, *streamUpstream)
	check("stream: the http upstream of the same name is another", fmt.Sprint(len(httpListed), err), "1 <nil>")

	sAdded, sRemoved, sUpdated, err = api.UpdateStreamServers(ctx, *streamUpstream, streamServers(5, "30s", *second))
	check("stream: a pod less", fmt.Sprint(len(sAdded), len(sRemoved), len(sUpdated), err), "0 1 0 <nil>")
	check("stream: and it gets nothing", through(*streamFront, 8), "second")

	sAdded, sRemoved, sUpdated, err = api.UpdateStreamServers(ctx, *streamUpstream, nil)
	check("stream: no pods at all", fmt.Sprint(len(sAdded), len(sRemoved), len(sUpdated), err), "0 1 0 <nil>")
	check("stream: takes no connection, and nginx stays up", through(*streamFront, 4), "error")

	sAdded, _, _, err = api.UpdateStreamServers(ctx, *streamUpstream, streamServers(1, "10s", *first))
	check("stream: and a pod again", fmt.Sprint(len(sAdded), err), "1 <nil>")
	check("stream: is used", through(*streamFront, 4), "first")

	_, _, _, err = api.UpdateStreamServers(ctx, "no-such-upstream", streamServers(1, "10s", *first))
	check("stream: an upstream that is not there is an error, so the controller reloads", err != nil, true)

	if failed > 0 {
		fmt.Printf("%d failed\n", failed)
		os.Exit(1)
	}
}
