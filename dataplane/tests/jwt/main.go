// Command jwt checks the data plane's JWT validation against an nginx that
// tests/jwt.sh started: it makes the keys, writes the JWK set the file
// locations read, serves another over HTTP for the jwksURI locations, signs
// tokens with every algorithm the module takes, and asks.
//
//	-nginx <addr>  the server with the locations
//	-jwks  <addr>  where to serve the JWK set of the URL locations
//	-keys  <dir>   where keys.json is, to rotate it
package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"hash"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

var b64 = base64.RawURLEncoding

// key is a signing key and its public JWK.
type key struct {
	kid    string
	alg    string
	signer any
	jwk    map[string]string
}

func fixed(n *big.Int, size int) []byte {
	b := n.Bytes()
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

// the keys are deterministic per run only in their kids; they are made afresh
func makeKeys() []key {
	var keys []key
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaJWK := map[string]string{"kty": "RSA", "n": b64.EncodeToString(rsaKey.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes())}
	for _, alg := range []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"} {
		jwk := map[string]string{"kid": "rsa-" + alg}
		for k, v := range rsaJWK {
			jwk[k] = v
		}
		keys = append(keys, key{kid: "rsa-" + alg, alg: alg, signer: rsaKey, jwk: jwk})
	}
	for _, c := range []struct {
		alg   string
		curve elliptic.Curve
		crv   string
		size  int
	}{{"ES256", elliptic.P256(), "P-256", 32}, {"ES384", elliptic.P384(), "P-384", 48}, {"ES512", elliptic.P521(), "P-521", 66}} {
		k, _ := ecdsa.GenerateKey(c.curve, rand.Reader)
		keys = append(keys, key{kid: "ec-" + c.alg, alg: c.alg, signer: k, jwk: map[string]string{
			"kid": "ec-" + c.alg, "kty": "EC", "crv": c.crv,
			"x": b64.EncodeToString(fixed(k.X, c.size)), "y": b64.EncodeToString(fixed(k.Y, c.size)),
		}})
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys = append(keys, key{kid: "ed", alg: "EdDSA", signer: priv, jwk: map[string]string{"kid": "ed", "kty": "OKP", "crv": "Ed25519", "x": b64.EncodeToString(pub)}})
	for _, alg := range []string{"HS256", "HS384", "HS512"} {
		secret := make([]byte, 64)
		_, _ = rand.Read(secret)
		keys = append(keys, key{kid: "oct-" + alg, alg: alg, signer: secret, jwk: map[string]string{"kid": "oct-" + alg, "kty": "oct", "k": b64.EncodeToString(secret)}})
	}
	return keys
}

func digest(alg string) (crypto.Hash, func() hash.Hash) {
	switch alg[2:] {
	case "384":
		return crypto.SHA384, sha512.New384
	case "512":
		return crypto.SHA512, sha512.New
	}
	return crypto.SHA256, sha256.New
}

// sign makes a compact JWS of the claims with k, with extra header fields.
func sign(k key, claims map[string]any, header map[string]any) string {
	h := map[string]any{"alg": k.alg, "typ": "JWT", "kid": k.kid}
	for name, v := range header {
		if v == nil {
			delete(h, name)
		} else {
			h[name] = v
		}
	}
	hj, _ := json.Marshal(h)
	cj, _ := json.Marshal(claims)
	return signed(k, string(hj), string(cj))
}

// signed makes a compact JWS of the header and the claims as they are
// written, which a map cannot say: a name twice, an escape.
func signed(k key, hj, cj string) string {
	input := b64.EncodeToString([]byte(hj)) + "." + b64.EncodeToString([]byte(cj))
	var sig []byte
	switch s := k.signer.(type) {
	case *rsa.PrivateKey:
		hh, newHash := digest(k.alg)
		d := newHash()
		d.Write([]byte(input))
		if strings.HasPrefix(k.alg, "PS") {
			sig, _ = rsa.SignPSS(rand.Reader, s, hh, d.Sum(nil), &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		} else {
			sig, _ = rsa.SignPKCS1v15(rand.Reader, s, hh, d.Sum(nil))
		}
	case *ecdsa.PrivateKey:
		_, newHash := digest(k.alg)
		d := newHash()
		d.Write([]byte(input))
		r, ss, _ := ecdsa.Sign(rand.Reader, s, d.Sum(nil))
		size := (s.Curve.Params().BitSize + 7) / 8
		sig = append(fixed(r, size), fixed(ss, size)...)
	case ed25519.PrivateKey:
		sig = ed25519.Sign(s, []byte(input))
	case []byte:
		_, newHash := digest(k.alg)
		m := hmac.New(newHash, s)
		m.Write([]byte(input))
		sig = m.Sum(nil)
	}
	return input + "." + b64.EncodeToString(sig)
}

func set(keys ...key) []byte {
	var out []map[string]string
	for _, k := range keys {
		out = append(out, k.jwk)
	}
	b, _ := json.Marshal(map[string]any{"keys": out})
	return b
}

func claims(extra map[string]any) map[string]any {
	c := map[string]any{"sub": "alice", "iss": "e2e", "groups": []string{"a", "b"}, "admin": true, "level": 3, "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

// ask requests a path of nginx; the status, the X-Sub header and WWW-Authenticate.
func ask(addr, path string, set func(*http.Request)) (int, string, string) {
	req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	if set != nil {
		set(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error(), ""
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Sub") + "|" + resp.Header.Get("X-Groups") + "|" + resp.Header.Get("X-Alg"), resp.Header.Get("WWW-Authenticate")
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func main() {
	nginx := flag.String("nginx", "", "")
	jwksAddr := flag.String("jwks", "", "")
	tlsAddr := flag.String("tls", "", "where to serve the JWK set over TLS, with a certificate of its own")
	keysDir := flag.String("keys", "", "")
	mint := flag.Bool("mint", false, "write keys.json and one token per algorithm (<alg>.token) into -keys, for benchmarks, and exit")
	flag.Parse()

	keys := makeKeys()
	if *mint {
		_ = os.WriteFile(filepath.Join(*keysDir, "keys.json"), set(keys...), 0o644)
		for _, k := range keys {
			_ = os.WriteFile(filepath.Join(*keysDir, k.alg+".token"), []byte(sign(k, claims(nil), nil)), 0o644)
		}
		return
	}

	// the file of the file locations: every key but the last HS one, which
	// stays unknown until the rotation
	file := filepath.Join(*keysDir, "keys.json")
	late := keys[len(keys)-1]
	if err := os.WriteFile(file, set(keys[:len(keys)-1]...), 0o644); err != nil {
		fmt.Println(err)
		os.Exit(2)
	}

	// the URL's set: the same, also without the late key at first
	var mu sync.Mutex
	served := set(keys[:len(keys)-1]...)
	var fetches, slowFetches, brokenFetches, tlsFetches atomic.Int32
	serve := func(w http.ResponseWriter) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(served)
	}
	go func() {
		_ = http.ListenAndServe(*jwksAddr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/slow":
				slowFetches.Add(1)
				time.Sleep(500 * time.Millisecond)
			case "/broken":
				brokenFetches.Add(1)
				http.Error(w, "no", http.StatusInternalServerError)
				return
			default:
				fetches.Add(1)
			}
			serve(w)
		}))
	}()
	if *tlsAddr != "" {
		listener, err := net.Listen("tcp", *tlsAddr)
		if err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		secure := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			tlsFetches.Add(1)
			serve(w)
		}))
		secure.Listener = listener
		secure.StartTLS()
		defer secure.Close()
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: secure.Certificate().Raw})
		_ = os.WriteFile(filepath.Join(*keysDir, "ca.pem"), ca, 0o644)
	}
	time.Sleep(300 * time.Millisecond)

	good := "200 alice|a,b|"
	for _, k := range keys[:len(keys)-1] {
		code, got, _ := ask(*nginx, "/file", bearer(sign(k, claims(nil), nil)))
		check(k.alg+" from the file: 200 and the claims", fmt.Sprint(code, " ", got), good+k.alg)
	}
	code, got, _ := ask(*nginx, "/url", bearer(sign(keys[0], claims(nil), nil)))
	check("RS256 from the URL", fmt.Sprint(code, " ", got), good+"RS256")
	check("the URL's set was fetched once", fetches.Load(), 1)
	code, got, _ = ask(*nginx, "/url", bearer(sign(keys[6], claims(nil), nil)))
	check("ES256 from the URL, the set kept", fmt.Sprint(code, " ", got, " ", fetches.Load()), good+"ES256 1")

	// what is not good
	code, _, challenge := ask(*nginx, "/file", nil)
	check("no token: 401 with the realm", fmt.Sprint(code, " ", challenge), `401 Bearer realm="file"`)
	tampered := sign(keys[0], claims(nil), nil)
	parts := strings.Split(tampered, ".")
	other, _ := json.Marshal(claims(map[string]any{"sub": "mallory"}))
	parts[1] = b64.EncodeToString(other)
	code, _, challenge = ask(*nginx, "/file", bearer(strings.Join(parts, ".")))
	check("another payload under the signature: 401, invalid_token", fmt.Sprint(code, " ", challenge), `401 Bearer realm="file", error="invalid_token"`)
	code, _, _ = ask(*nginx, "/file", bearer(sign(keys[0], claims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}), nil)))
	check("expired: 401", code, 401)
	code, _, _ = ask(*nginx, "/leeway", bearer(sign(keys[0], claims(map[string]any{"exp": time.Now().Add(-30 * time.Second).Unix()}), nil)))
	check("expired 30s ago with 60s of leeway: 200", code, 200)
	code, _, _ = ask(*nginx, "/file", bearer(sign(keys[0], claims(map[string]any{"nbf": time.Now().Add(time.Hour).Unix()}), nil)))
	check("not before an hour from now: 401", code, 401)
	none := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + b64.EncodeToString(other) + "."
	code, _, _ = ask(*nginx, "/file", bearer(none))
	check("alg none: 401", code, 401)
	// the RSA public key's modulus used as an HMAC secret: the key is RSA, HS256 needs oct
	confused := sign(key{kid: "rsa-RS256", alg: "HS256", signer: []byte(keys[0].jwk["n"])}, claims(nil), nil)
	code, _, _ = ask(*nginx, "/file", bearer(confused))
	check("HS256 with the RSA key's kid: 401", code, 401)
	code, _, _ = ask(*nginx, "/file", bearer(sign(keys[0], claims(nil), map[string]any{"kid": "nobody"})))
	check("a kid the set does not have: 401", code, 401)
	code, _, _ = ask(*nginx, "/file", bearer(sign(keys[0], claims(nil), map[string]any{"kid": nil})))
	check("no kid: the keys are tried, 200", code, 200)
	code, _, _ = ask(*nginx, "/file", bearer("a.b"))
	check("not a JWS: 401", code, 401)
	code, _, _ = ask(*nginx, "/open", nil)
	check("a location without a realm needs no token", code, 200)

	// a token whose JSON has no one meaning: a name twice - a lookup takes
	// the first, another reader the last - or a NUL in a string, which C reads
	// as its end. Signed all the same, as an IdP could sign one.
	exp := time.Now().Add(time.Hour).Unix()
	header := fmt.Sprintf(`{"alg":%q,"typ":"JWT","kid":%q}`, keys[0].alg, keys[0].kid)
	code, _, _ = ask(*nginx, "/file", bearer(signed(keys[0], header, fmt.Sprintf(`{"sub":"alice","sub":"admin","exp":%d}`, exp))))
	check("a claim named twice: 401", code, 401)
	code, _, _ = ask(*nginx, "/file", bearer(signed(keys[0], header, fmt.Sprintf(`{"sub":"admin\u0000alice","exp":%d}`, exp))))
	check("a NUL in a claim: 401", code, 401)
	code, got, _ = ask(*nginx, "/file", bearer(signed(keys[0], header, fmt.Sprintf(`{"sub":"alice\\u0000","groups":["a"],"exp":%d}`, exp))))
	check("an escaped backslash before u0000 is no NUL: 200", fmt.Sprint(code, " ", got), `200 alice\u0000|a|RS256`)
	code, _, _ = ask(*nginx, "/file", bearer(signed(keys[0], fmt.Sprintf(`{"alg":%q,"kid":%q,"alg":"none"}`, keys[0].alg, keys[0].kid), fmt.Sprintf(`{"sub":"alice","exp":%d}`, exp))))
	check("a header field named twice: 401", code, 401)

	// a claim read for every answer, a 401's too, whose token nobody verified
	deep := func(levels int) string {
		return strings.Repeat("[", levels) + `"x"` + strings.Repeat("]", levels)
	}
	always := func(token string) (int, string) {
		req, _ := http.NewRequest("GET", "http://"+*nginx+"/always", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("X-Deep")
	}
	code, got = always(signed(keys[0], header, fmt.Sprintf(`{"deep":[[1,2],[3]],"exp":%d}`, exp)))
	check("arrays in an array, as one list", fmt.Sprint(code, " ", got), "200 1,2,3")
	code, got = always(signed(keys[0], header, fmt.Sprintf(`{"deep":%s,"exp":%d}`, deep(8), exp)))
	check("eight arrays deep: written out", fmt.Sprint(code, " ", got), "200 x")
	code, got = always(signed(keys[0], header, fmt.Sprintf(`{"deep":%s,"exp":%d}`, deep(9), exp)))
	check("nine: not", fmt.Sprint(code, " ", got), "200 ")
	unsigned := b64.EncodeToString([]byte(header)) + "." + b64.EncodeToString([]byte(`{"deep":`+deep(5900)+`}`)) + ".AAAA"
	code, _ = always(unsigned)
	check("a token of 5900 arrays deep that nobody signed: 401", code, 401)
	code, _ = always(sign(keys[0], claims(nil), nil))
	check("and the worker is still there", code, 200)

	// a request nginx redirects inside itself is asked where it lands
	signed := bearer(sign(keys[0], claims(nil), nil))
	for _, c := range []struct{ path, realm string }{
		{"/rewrite-break", "file"},
		{"/split", "split"},
		{"/tried", "named"},
		{"/tried-uri", "split"},
		{"/errored", "named"},
	} {
		code, _, challenge = ask(*nginx, c.path, nil)
		check(c.path+" without a token: 401 with the realm it lands in", fmt.Sprint(code, " ", challenge), `401 Bearer realm="`+c.realm+`"`)
		code, got, _ = ask(*nginx, c.path, signed)
		check(c.path+" with one: 200 and the claims", fmt.Sprint(code, " ", got), good+"RS256")
	}

	// a key set is fetched once at a time: the requests that come while it is
	// fetched wait for that fetch rather than each sending one
	var wg sync.WaitGroup
	var slowCodes sync.Map
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _, _ := ask(*nginx, "/slow-url", signed)
			slowCodes.Store(i, code)
		}(i)
	}
	wg.Wait()
	served200 := 0
	slowCodes.Range(func(_, v any) bool {
		if v.(int) == 200 {
			served200++
		}
		return true
	})
	check("20 requests at once while the set is fetched: served, with one fetch", fmt.Sprint(served200, " ", slowFetches.Load()), "20 1")

	// and a set that cannot be had is not asked for again by every request
	for i := 0; i < 5; i++ {
		code, _, _ = ask(*nginx, "/broken-url", signed)
	}
	check("a set whose server fails: 401, and one fetch for five requests", fmt.Sprint(code, " ", brokenFetches.Load()), "401 1")

	if *tlsAddr != "" {
		code, _, _ = ask(*nginx, "/tls-off", signed)
		check("over TLS without verification, as the Policy says: 200", fmt.Sprint(code, " ", tlsFetches.Load()), "200 1")
		code, _, _ = ask(*nginx, "/tls-default", signed)
		check("the same URL verified by default: not the set fetched without, and the certificate is not trusted: 401",
			fmt.Sprint(code, " ", tlsFetches.Load()), "401 1")
		code, _, _ = ask(*nginx, "/tls-ca", signed)
		check("verified against the Policy's CA: 200", fmt.Sprint(code, " ", tlsFetches.Load()), "200 2")
	}

	// where the token is
	token := sign(keys[9], claims(nil), nil)
	code, got, _ = ask(*nginx, "/cookie", func(r *http.Request) { r.Header.Set("Cookie", "x=1; auth="+token) })
	check("the token in a cookie", fmt.Sprint(code, " ", got), good+"EdDSA")
	code, _, _ = ask(*nginx, "/cookie", bearer(token))
	check("and not in the header there", code, 401)
	code, got, _ = ask(*nginx, "/arg?token="+token, nil)
	check("the token in an argument", fmt.Sprint(code, " ", got), good+"EdDSA")

	// a key that comes: the file is read again, the URL fetched again for its kid
	lateToken := sign(late, claims(nil), nil)
	code, _, _ = ask(*nginx, "/file", bearer(lateToken))
	check("the late key is not in the file yet", code, 401)
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(file, set(keys...), 0o644); err != nil {
		fmt.Println(err)
	}
	code, _, _ = ask(*nginx, "/file", bearer(lateToken))
	check("the file rewritten: the late key is taken without a reload", code, 200)

	// a token verified before is remembered - and forgotten when its key leaves the set
	rs := sign(keys[0], claims(nil), nil)
	code, _, _ = ask(*nginx, "/file", bearer(rs))
	code2, _, _ := ask(*nginx, "/file", bearer(rs))
	check("an RS256 token, twice: verified, then remembered", fmt.Sprint(code, " ", code2), "200 200")
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(file, set(keys[1:]...), 0o644); err != nil {
		fmt.Println(err)
	}
	time.Sleep(1100 * time.Millisecond)
	code, _, _ = ask(*nginx, "/file", bearer(rs))
	check("its key gone from the file: the remembered token is not good any more", code, 401)
	_ = os.WriteFile(file, set(keys...), 0o644)

	mu.Lock()
	served = set(keys...)
	mu.Unlock()
	code, _, _ = ask(*nginx, "/url", bearer(lateToken))
	check("the URL's set has the late key, but was fetched less than 10s ago: 401", fmt.Sprint(code, " ", fetches.Load()), "401 1")
	time.Sleep(10500 * time.Millisecond)
	before := fetches.Load()
	code, _, _ = ask(*nginx, "/url", bearer(lateToken))
	check("10s later the unknown kid fetches the set again: 200", fmt.Sprint(code, " ", fetches.Load()-before), "200 1")
	code, _, _ = ask(*nginx, "/url", bearer(lateToken))
	check("and keeps it", fmt.Sprint(code, " ", fetches.Load()-before), "200 1")

	if failed > 0 {
		fmt.Printf("%d failed\n", failed)
		os.Exit(1)
	}
}
