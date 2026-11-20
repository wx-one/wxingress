// Command certs is what dataplane/e2e/certificates.sh runs inside the kind
// node: it writes TLS Secrets and Ingresses for kubectl, and asks the
// controller's TLS port for a certificate by name, the way a client does.
//
//	certs secret <namespace> <name> <host> <tag>           a TLS Secret, as YAML
//	certs many <namespace> <n> <tag> [ingress <class>]     n of them, m<i>.many.certs.test,
//	                                                       and an Ingress "many" with their hosts
//	certs probe <address> <name>                           "<CN>/<O>" of what the handshake gives
//	certs wait <address> <name> <want> <seconds>           until probe says want; how long it took
//	certs bulk <address> <n> <tag> <repeats>               every m<i>.many.certs.test, with timings
//
// Each certificate is self-signed for its host, with the tag as its
// organization, so that two certificates of one host can be told apart.
//
// wait and bulk fail - exit 1 - when the certificate never comes or a name
// has another one: bulk prints a line "wrong: <name> <what it got>" for each
// such name and "wrong certificates: <wrong> of <n>" at the end, whatever the
// caller does with its output.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  certs secret <namespace> <name> <host> <tag>
  certs many <namespace> <n> <tag> [ingress <class>]
  certs probe <address> <name>
  certs wait <address> <name> <want> <seconds>
  certs bulk <address> <n> <tag> <repeats>`)
	os.Exit(2)
}

// issue is a self-signed certificate for host, with tag as its organization, and its key, in PEM.
func issue(host, tag string) (string, string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{tag}},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(48 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func secret(namespace, name, host, tag string, labels string) string {
	certificate, key := issue(host, tag)
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
type: kubernetes.io/tls
metadata:
  name: %s
  namespace: %s%s
data:
  tls.crt: %s
  tls.key: %s
`, name, namespace, labels, base64.StdEncoding.EncodeToString([]byte(certificate)), base64.StdEncoding.EncodeToString([]byte(key)))
}

func manyHost(i int) string { return fmt.Sprintf("m%d.many.certs.test", i) }

func many(namespace string, n int, tag string, ingressClass string) {
	for i := range n {
		fmt.Print(secret(namespace, fmt.Sprintf("m%d-tls", i), manyHost(i), tag, "\n  labels: {certs-e2e: many}"))
		fmt.Println("---")
	}
	if ingressClass == "" {
		return
	}
	fmt.Printf(`apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: many
  namespace: %s
spec:
  ingressClassName: %s
  tls:
`, namespace, ingressClass)
	for i := range n {
		fmt.Printf("  - hosts: [%s]\n    secretName: m%d-tls\n", manyHost(i), i)
	}
	fmt.Println("  rules:")
	for i := range n {
		fmt.Printf("  - host: %s\n    http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: echo, port: {number: 80}}}}]}\n", manyHost(i))
	}
}

// probe is "<CN>/<O>" of the certificate a handshake with that name gets,
// or what went wrong, and how long the handshake took.
func probe(address, name string) (string, time.Duration) {
	start := time.Now()
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, &tls.Config{
		ServerName:         name,
		InsecureSkipVerify: true, //nolint:gosec // the certificates are self-signed, and which one comes is the question
	})
	took := time.Since(start)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return "refused", took
		}
		return err.Error(), took
	}
	defer connection.Close()
	certificates := connection.ConnectionState().PeerCertificates
	if len(certificates) == 0 {
		return "no certificate", took
	}
	subject := certificates[0].Subject
	got := subject.CommonName
	if len(subject.Organization) > 0 {
		got += "/" + subject.Organization[0]
	}
	return got, took
}

func wait(address, name, want string, seconds int) bool {
	start := time.Now()
	last := ""
	for time.Since(start) < time.Duration(seconds)*time.Second {
		if last, _ = probe(address, name); last == want {
			fmt.Printf("%.2fs\n", time.Since(start).Seconds())
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("timeout (last %s)\n", last)
	return false
}

func summary(what string, durations []time.Duration) {
	if len(durations) == 0 {
		return
	}
	slices.Sort(durations)
	var total time.Duration
	for _, d := range durations {
		total += d
	}
	at := func(p int) time.Duration { return durations[min(len(durations)-1, len(durations)*p/100)] }
	fmt.Printf("%-40s n=%d mean=%v p50=%v p90=%v p99=%v\n", what, len(durations),
		(total / time.Duration(len(durations))).Round(time.Microsecond), at(50).Round(time.Microsecond),
		at(90).Round(time.Microsecond), at(99).Round(time.Microsecond))
}

// bulk asks for every name of `many` once and then `repeats` times more, and
// a name nobody has as often; a name whose handshake gives another
// certificate than its own, even once, is wrong.
func bulk(address string, n int, tag string, repeats int) bool {
	var first, later, unknown []time.Duration
	wrong := 0
	for i := range n {
		name := manyHost(i)
		want := name + "/" + tag
		bad := ""
		for try := 0; try <= repeats; try++ {
			got, took := probe(address, name)
			if try == 0 {
				first = append(first, took)
			} else {
				later = append(later, took)
			}
			if got != want && bad == "" {
				bad = got
			}
		}
		if bad != "" {
			wrong++
			fmt.Printf("wrong: %s %s\n", name, bad)
		}
		for range repeats {
			_, took := probe(address, fmt.Sprintf("u%d.unknown.certs.test", i))
			unknown = append(unknown, took)
		}
	}
	fmt.Printf("wrong certificates: %d of %d\n", wrong, n)
	summary("first handshake per name", first)
	summary("later handshakes per name", later)
	summary("unknown name (configured cert)", unknown)
	return wrong == 0
}

func number(text string) int {
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 {
		usage()
	}
	return n
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
	}
	ok := true
	switch {
	case args[0] == "secret" && len(args) == 5:
		fmt.Print(secret(args[1], args[2], args[3], args[4], ""))
	case args[0] == "many" && (len(args) == 4 || len(args) == 6 && args[4] == "ingress"):
		class := ""
		if len(args) == 6 {
			class = args[5]
		}
		many(args[1], number(args[2]), args[3], class)
	case args[0] == "probe" && len(args) == 3:
		got, _ := probe(args[1], args[2])
		fmt.Println(got)
	case args[0] == "wait" && len(args) == 5:
		ok = wait(args[1], args[2], args[3], number(args[4]))
	case args[0] == "bulk" && len(args) == 5:
		ok = bulk(args[1], number(args[2]), args[3], number(args[4]))
	default:
		usage()
	}
	if !ok {
		os.Exit(1)
	}
}
