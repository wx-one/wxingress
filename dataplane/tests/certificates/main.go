// Command certificates drives the certificate API of the data plane the way
// the controller does and asks nginx for certificates by name, the way a
// client does, to see which one it is given.
//
// It is run by tests/certificates.sh against an nginx it started, whose TLS
// servers all name the default certificate, as the controller's templates
// write them with -dynamic-upstreams: every other certificate comes from the
// API, without a reload.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
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

// pair is a certificate and its key in PEM, the leaf named `name` and made
// by `by`, which ends up in the organization, so that two certificates of
// one name can be told apart.
type pair struct {
	certificate string
	key         string
}

func issue(name, by string, padding int) pair {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name, Organization: []string{by}},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)

	certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	// a chain made long with copies of the leaf, which nobody verifies here
	for range padding {
		certificate += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}

	return pair{
		certificate: certificate,
		key:         string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}
}

var api *http.Client

const base = "http://nginx-plus-api/api/wxingress/1/certificates"

func call(method, name string, body any) (int, string) {
	var reader io.Reader
	if body != nil {
		if text, ok := body.(string); ok {
			reader = strings.NewReader(text)
		} else {
			encoded, _ := json.Marshal(body)
			reader = bytes.NewReader(encoded)
		}
	}
	target := base
	if name != "" {
		target += "/" + url.PathEscape(name)
	}
	request, _ := http.NewRequest(method, target, reader)
	response, err := api.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(answer)
}

func put(name string, p pair) int {
	status, _ := call(http.MethodPut, name, map[string]string{"certificate": p.certificate, "key": p.key})
	return status
}

var address string

// served is the leaf's "CN/O" a handshake with that name is given, and how
// long the handshake took. "" sends no name at all.
func served(name string) (string, time.Duration) {
	config := &tls.Config{InsecureSkipVerify: true, ServerName: name} //nolint:gosec // the certificate is what is looked at
	started := time.Now()
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, config)
	took := time.Since(started)
	if err != nil {
		return "refused: " + err.Error(), took
	}
	defer connection.Close()
	leaf := connection.ConnectionState().PeerCertificates[0]
	return leaf.Subject.CommonName + "/" + strings.Join(leaf.Subject.Organization, ""), took
}

func subject(name string) string {
	got, _ := served(name)
	return got
}

// always is what `count` handshakes with the name were given, if it was the
// same every time; with two workers, both answer some of them.
func always(name string, count int) string {
	seen := map[string]int{}
	for range count {
		seen[subject(name)]++
	}
	var all []string
	for got, times := range seen {
		all = append(all, fmt.Sprintf("%s x%d", got, times))
	}
	sort.Strings(all)
	return strings.Join(all, ", ")
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	return sorted[int(float64(len(sorted)-1)*p)]
}

func timings(what string, durations []time.Duration) {
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	var total time.Duration
	for _, d := range durations {
		total += d
	}
	fmt.Printf("time     %-44s n=%d mean=%v p50=%v p99=%v\n", what, len(durations),
		(total / time.Duration(len(durations))).Round(time.Microsecond),
		percentile(durations, 0.5).Round(time.Microsecond),
		percentile(durations, 0.99).Round(time.Microsecond))
}

func main() {
	socket := flag.String("socket", "", "the API's unix socket")
	flag.StringVar(&address, "tls", "", "where nginx speaks TLS")
	many := flag.Int("many", 1000, "how many names the bulk test sets")
	flag.Parse()

	api = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", *socket)
		},
	}, Timeout: 10 * time.Second}

	// ------------------------------------------------------------ one name

	check("an unknown name has the configured certificate", always("unknown.test", 6), "default/ x6")
	check("so does a handshake without a name", subject(""), "default/")

	first := issue("a.test", "one", 0)
	check("a certificate is set", put("a.test", first), 200)
	check("and served for its name by every worker, without a reload", always("a.test", 20), "a.test/one x20")
	check("in whatever case the client writes the name", subject("A.Test"), "a.test/one")

	status, listed := call(http.MethodGet, "", nil)
	check("it is listed", fmt.Sprint(status, strings.Contains(listed, `"name":"a.test"`)), "200 true")
	status, shown := call(http.MethodGet, "a.test", nil)
	check("and shown with its certificate", fmt.Sprint(status, strings.Contains(shown, `BEGIN CERTIFICATE`)), "200 true")
	check("but never its key", strings.Contains(shown, "PRIVATE KEY"), false)

	check("a changed certificate is set", put("a.test", issue("a.test", "two", 0)), 200)
	check("and every next handshake has it", always("a.test", 20), "a.test/two x20")

	// ------------------------------------------------------------ wildcards

	check("a wildcard is set", put("*.w.test", issue("*.w.test", "wild", 0)), 200)
	check("a name one level below it has it", subject("x.w.test"), "*.w.test/wild")
	check("two levels below do not", subject("a.x.w.test"), "default/")
	check("nor the name itself", subject("w.test"), "default/")
	check("a name of its own is set beside it", put("x.w.test", issue("x.w.test", "exact", 0)), 200)
	check("and wins over the wildcard", subject("x.w.test"), "x.w.test/exact")
	check("while the others still have the wildcard", subject("y.w.test"), "*.w.test/wild")

	// ------------------------------------------------------------ the default

	check("_ is set", put("_", issue("fallback", "default", 0)), 200)
	check("an unknown name has _", subject("unknown.test"), "fallback/default")
	check("so does no name", subject(""), "fallback/default")
	check("a known one keeps its own", subject("a.test"), "a.test/two")
	status, _ = call(http.MethodDelete, "_", nil)
	check("_ is taken away", status, 200)
	check("and the configured one is back", subject("unknown.test"), "default/")

	// ------------------------------------------------------------ removal

	status, _ = call(http.MethodDelete, "a.test", nil)
	check("a certificate is taken away", status, 200)
	check("and its name has the configured one again", always("a.test", 10), "default/ x10")
	status, _ = call(http.MethodDelete, "a.test", nil)
	check("taking it away again is 404", status, 404)
	status, _ = call(http.MethodGet, "a.test", nil)
	check("as is showing it", status, 404)
	check("it can be set again", put("a.test", first), 200)
	check("and is served again", subject("a.test"), "a.test/one")

	// ------------------------------------------------------------ refusals

	other := issue("b.test", "other", 0)
	status, _ = call(http.MethodPut, "b.test", map[string]string{"certificate": "nonsense", "key": other.key})
	check("what is not PEM is refused", status, 400)
	status, said := call(http.MethodPut, "b.test", map[string]string{"certificate": other.certificate, "key": first.key})
	check("a key of another certificate is refused", fmt.Sprint(status, strings.Contains(said, "does not belong")), "400 true")
	status, _ = call(http.MethodPut, "b.test", map[string]string{"certificate": other.certificate})
	check("a certificate without a key is refused", status, 400)
	status, _ = call(http.MethodPut, "b.test", "{nope")
	check("a body that is not JSON is refused", status, 415)
	for _, name := range []string{"a b.test", "foo.*.test", "*", "*.", "a..test", "a.test.", ":443"} {
		status, _ = call(http.MethodPut, name, map[string]string{"certificate": other.certificate, "key": other.key})
		check(fmt.Sprintf("the name %q is refused", name), status, 400)
	}
	check("and nothing of it was kept", subject("b.test"), "default/")

	// ------------------------------------------------------------ many

	pairs := make([]pair, *many)
	for at := range pairs {
		pairs[at] = issue(fmt.Sprintf("n%d.many.test", at), "many", 0)
	}

	started := time.Now()
	bad := 0
	for at, p := range pairs {
		if put(fmt.Sprintf("n%d.many.test", at), p) != 200 {
			bad++
		}
	}
	took := time.Since(started)
	check(fmt.Sprintf("%d certificates are set (%v, %v each)", *many, took.Round(time.Millisecond),
		(took/time.Duration(*many)).Round(time.Microsecond)), bad, 0)

	var cold, warm, configured []time.Duration
	wrong := 0
	for at := range pairs {
		name := fmt.Sprintf("n%d.many.test", at)
		got, d := served(name)
		if got != name+"/many" {
			wrong++
		}
		cold = append(cold, d)
	}
	check(fmt.Sprintf("each of the %d names has its own certificate", *many), wrong, 0)
	// side by side, so that whatever else the machine does weighs on both
	for round := 0; round < 3; round++ {
		for at := range pairs {
			_, d := served(fmt.Sprintf("n%d.many.test", at))
			warm = append(warm, d)
			_, d = served("unknown.test")
			configured = append(configured, d)
		}
	}
	timings("handshakes, first per name (parsed in a worker)", cold)
	timings("handshakes, again (the worker's parsed pair)", warm)
	timings("handshakes for the configured certificate", configured)

	// ------------------------------------------------------------ the heap

	// one name changed until what it left behind fills the heap and it is
	// compacted; everything else keeps its certificate
	big := issue("big.test", "big", 14)
	size := len(big.certificate) + len(big.key)
	rounds := (128<<20)/size + 200
	started = time.Now()
	bad = 0
	var last pair
	for round := range rounds {
		if round%500 == 0 || round == rounds-1 {
			last = issue("big.test", fmt.Sprintf("big%d", round), 14)
		}
		if put("big.test", last) != 200 {
			bad++
		}
	}
	check(fmt.Sprintf("one name set %d times with %d bytes, past the heap (%v)", rounds, size,
		time.Since(started).Round(time.Millisecond)), bad, 0)
	check("it has its last certificate", subject("big.test"), fmt.Sprintf("big.test/big%d", rounds-1))
	wrong = 0
	for at := 0; at < len(pairs); at += 7 {
		name := fmt.Sprintf("n%d.many.test", at)
		if subject(name) != name+"/many" {
			wrong++
		}
	}
	check("and the others still have theirs", wrong, 0)
	status, shown = call(http.MethodGet, "n3.many.test", nil)
	check("with their PEM intact", fmt.Sprint(status, strings.Contains(shown, strings.ReplaceAll(strings.TrimSpace(pairs[3].certificate), "\n", `\n`))), "200 true")

	if failed > 0 {
		fmt.Printf("%d failed\n", failed)
		os.Exit(1)
	}
}
