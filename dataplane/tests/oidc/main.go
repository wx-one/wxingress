// Command oidc checks the data plane's OpenID Connect relying party against
// an nginx that tests/oidc.sh started: it is the identity provider itself -
// authorization, token, JWKS and end session endpoints - and walks the
// flows a browser walks, cookie by cookie.
//
//	-nginx <addr>  the server with the protected location
//	-idp   <addr>  where to serve the identity provider
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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

// the client: confidential with a secret, or public (PKCE alone) with -public
var (
	client = "e2e-client"
	secret = "e2e secret: with & and ="
	// the secret of -long's server, longer than a worker remembers of one
	longSecret string
)

// idp is the identity provider.
type idp struct {
	key      *rsa.PrivateKey
	mu       sync.Mutex
	codes    map[string]login
	refresh  map[string]string
	lifetime time.Duration
	// iss of its tokens; the base URL with -discovery
	issuer string
	// the sid of the last login
	sid string
	// what it saw
	challenges int
	refreshes  int
	badSecret  int
}

type login struct{ nonce, challenge, redirect string }

func (p *idp) idToken(sub, nonce string) string {
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "idp"})
	c := map[string]any{"iss": p.issuer, "sid": p.sid, "sub": sub, "aud": client, "email": sub + "@example.com", "exp": time.Now().Add(p.lifetime).Unix(), "iat": time.Now().Unix()}
	if nonce != "" {
		c["nonce"] = nonce
	}
	cj, _ := json.Marshal(c)
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(cj)
	d := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, d[:])
	return input + "." + b64.EncodeToString(sig)
}

// logoutToken is a back-channel logout token for a sid.
func (p *idp) logoutToken(sid string) string {
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "logout+jwt", "kid": "idp"})
	c, _ := json.Marshal(map[string]any{"iss": p.issuer, "aud": client, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "jti": random(), "sid": sid,
		"events": map[string]any{"http://schemas.openid.net/event/backchannel-logout": map[string]any{}}})
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	d := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, d[:])
	return input + "." + b64.EncodeToString(sig)
}

func random() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b64.EncodeToString(b)
}

func (p *idp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		base := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.issuer, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
			"jwks_uri": base + "/jwks", "end_session_endpoint": base + "/end_session",
		})
	case "/jwks":
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "idp", "alg": "RS256", "use": "sig",
			"n": b64.EncodeToString(p.key.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(p.key.E)).Bytes()),
		}}})
	case "/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != client || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
			!strings.Contains(q.Get("scope"), "openid") || q.Get("prompt") != "login" {
			http.Error(w, "bad authorization request: "+r.URL.RawQuery, 400)
			return
		}
		p.challenges++
		p.sid = "sid-" + random()
		code := random()
		p.codes[code] = login{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	case "/token":
		// RFC 6749 2.3.1: both are form-urlencoded before they go into Basic
		user, pass, ok := r.BasicAuth()
		user, _ = url.QueryUnescape(user)
		pass, _ = url.QueryUnescape(pass)
		_ = r.ParseForm()
		if secret == "" {
			// a public client: no credentials, its id in the form
			ok, user, pass = !ok && r.Form.Get("client_id") == client, client, ""
		}
		if !ok || user != client || (pass != secret && (longSecret == "" || pass != longSecret)) {
			p.badSecret++
			http.Error(w, `{"error":"invalid_client"}`, 401)
			return
		}
		var nonce, sub string
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			l, found := p.codes[r.Form.Get("code")]
			delete(p.codes, r.Form.Get("code"))
			d := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !found || b64.EncodeToString(d[:]) != l.challenge || r.Form.Get("redirect_uri") != l.redirect {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			nonce, sub = l.nonce, "alice"
		case "refresh_token":
			s, found := p.refresh[r.Form.Get("refresh_token")]
			if !found {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			delete(p.refresh, r.Form.Get("refresh_token"))
			p.refreshes++
			sub = s
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, 400)
			return
		}
		rt := random()
		p.refresh[rt] = sub
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-" + random(), "token_type": "Bearer", "expires_in": int(p.lifetime.Seconds()),
			"refresh_token": rt, "id_token": p.idToken(sub, nonce),
		})
	case "/end_session":
		_, _ = fmt.Fprintf(w, "ended %s %s\n", r.URL.Query().Get("post_logout_redirect_uri"), r.URL.Query().Get("client_id"))
	default:
		http.NotFound(w, r)
	}
}

// browser keeps cookies as a browser would and follows nothing by itself.
type browser struct {
	cookies map[string]string
	client  *http.Client
	// a connection for each request, which any worker may take
	close bool
}

func newBrowser() *browser {
	return &browser{cookies: map[string]string{}, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

func (b *browser) get(u string) (int, http.Header, string) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Close = b.close
	var pairs []string
	for k, v := range b.cookies {
		pairs = append(pairs, k+"="+v)
	}
	if len(pairs) > 0 {
		req.Header.Set("Cookie", strings.Join(pairs, "; "))
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, err.Error()
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return resp.StatusCode, resp.Header, string(body)
}

// login walks from a protected page to it again, through the IdP; the hops.
func (b *browser) login(start string) (string, int) {
	hops := 0
	u := start
	for hops < 6 {
		code, h, body := b.get(u)
		if code != 302 && code != 307 {
			return fmt.Sprintf("%d %s|%s", code, h.Get("X-User"), strings.TrimSpace(body)), hops
		}
		next, _ := url.Parse(h.Get("Location"))
		base, _ := url.Parse(u)
		u = base.ResolveReference(next).String()
		hops++
	}
	return "too many redirects", hops
}

func main() {
	nginx := flag.String("nginx", "", "")
	idpAddr := flag.String("idp", "", "")
	public := flag.Bool("public", false, "a public client: no secret, PKCE alone")
	discovery := flag.Bool("discovery", false, "the server names only the issuer: endpoints from discovery, iss checked, logouts and revocations")
	api := flag.String("api", "", "the unix socket of the module's API")
	otherNginx := flag.String("other", "", "a server of another Policy with the same client and key file")
	longNginx := flag.String("long", "", "a server of the same client whose secret is -long-secret")
	flag.StringVar(&longSecret, "long-secret", "", "")
	flag.Parse()
	if *public {
		client, secret = "e2e-public", ""
	}

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := &idp{key: key, codes: map[string]login{}, refresh: map[string]string{}, lifetime: 4 * time.Second, issuer: "e2e"}
	if *discovery {
		p.issuer = "http://" + *idpAddr
	}
	go func() { _ = http.ListenAndServe(*idpAddr, p) }()
	time.Sleep(300 * time.Millisecond)
	app := "http://" + *nginx + "/app/page?x=1"

	b := newBrowser()
	code, h, _ := b.get(app)
	check("no session: to the IdP", fmt.Sprint(code, " ", strings.HasPrefix(h.Get("Location"), "http://"+*idpAddr+"/authorize?")), "302 true")
	check("with the state in a cookie", b.cookies["wxoidc_state"] != "", true)

	b = newBrowser()
	got, hops := b.login(app)
	check("logged in through the IdP, back where it started, the claim upstream", got, "200 alice|ok /app/page?x=1")
	check("in three hops: IdP, callback, back", hops, 3)
	check("the state cookie is gone, the session is there", fmt.Sprint(b.cookies["wxoidc_state"] == "", b.cookies["wxoidc_s0"] != ""), "true true")
	check("the IdP saw a PKCE challenge and the client's credentials", fmt.Sprint(p.challenges, " ", p.badSecret), "1 0")

	code, h, body := b.get(app)
	check("with the session: served, the access token upstream", fmt.Sprint(code, " ", h.Get("X-User"), " ", strings.HasPrefix(h.Get("X-Token"), "access-")), "200 alice true")
	_ = body

	// the session is the cookie alone: another worker, or another nginx with the same Secret, opens it
	for i := 0; i < 6; i++ {
		code, _, _ = b.get(app)
		if code != 200 {
			break
		}
	}
	check("every worker opens the session", code, 200)

	if *longNginx != "" {
		// A worker remembers the session it opened last, and answers the
		// next request with the same cookie from it. A session it could not
		// remember - its Policy's client secret is longer than what is kept
		// of one - must not leave the one before remembered: the next request
		// with that one's cookie was answered with this one's tokens.
		a := newBrowser()
		got, _ = a.login(app)
		check("a login at the server", got, "200 alice|ok /app/page?x=1")
		_, h, _ = a.get(app)
		mine := h.Get("X-Token")
		l := newBrowser()
		got, _ = l.login("http://" + *longNginx + "/app/page")
		check("a login at a server whose client secret is long", strings.HasPrefix(got, "200 alice"), true)
		_, h, _ = l.get("http://" + *longNginx + "/app/page")
		theirs := h.Get("X-Token")
		a.close, l.close = true, true
		mixed := 0
		for i := 0; i < 40; i++ {
			l.get("http://" + *longNginx + "/app/page")
			if _, h, _ = a.get(app); h.Get("X-Token") != mine {
				mixed++
			}
		}
		check("each request answered from its own session, whichever worker opened the other one", fmt.Sprint(mine != theirs, " ", mixed), "true 0")
	}

	if *otherNginx != "" {
		// the session of one Policy is no session of another's server, though
		// the client ID and the controller's key file are the same
		stolen := newBrowser()
		for k, v := range b.cookies {
			stolen.cookies[k] = v
		}
		code, h, _ = stolen.get("http://" + *otherNginx + "/app/page")
		check("a session at another Policy's server with the same client: to the IdP", fmt.Sprint(code, " ", strings.Contains(h.Get("Location"), "/authorize?")), "302 true")

		// and a session another tenant's server made is not taken here
		o := newBrowser()
		got, _ = o.login("http://" + *otherNginx + "/app/page")
		check("a login at the other Policy's server", strings.HasPrefix(got, "200 alice"), true)
		forged := newBrowser()
		for k, v := range o.cookies {
			forged.cookies[k] = v
		}
		code, h, _ = forged.get(app)
		check("its session here: to the IdP", fmt.Sprint(code, " ", strings.Contains(h.Get("Location"), "/authorize?")), "302 true")
	}

	tampered := newBrowser()
	for k, v := range b.cookies {
		tampered.cookies[k] = v
	}
	s := []byte(tampered.cookies["wxoidc_s0"] + "xx")
	s[len(s)/2] ^= 1
	tampered.cookies["wxoidc_s0"] = string(s)
	code, h, _ = tampered.get(app)
	check("a changed session cookie is no session: to the IdP", fmt.Sprint(code, " ", strings.Contains(h.Get("Location"), "/authorize?")), "302 true")

	// a callback with a state of another login
	other := newBrowser()
	other.get(app)
	code, _, _ = other.get("http://" + *nginx + "/_codexch?code=x&state=someone-elses")
	check("a callback with another state: 400", code, 400)
	code, _, _ = newBrowser().get("http://" + *nginx + "/_codexch?code=x&state=y")
	check("a callback without a login started: 400", code, 400)

	// the ID token expires: refreshed, the request sent to itself again
	time.Sleep(p.lifetime + time.Second)
	code, h, _ = b.get(app)
	check("expired: refreshed, and sent to itself with 307", fmt.Sprint(code, " ", h.Get("Location")), "307 /app/page?x=1")
	code, h, _ = b.get(app)
	check("then served with the new session", fmt.Sprint(code, " ", h.Get("X-User"), " ", p.refreshes), "200 alice 1")

	// a refresh at //app/x - which nginx serves as /app/x - sends the browser to
	// /, not to the host "app" a browser reads in a Location of //app/x
	time.Sleep(p.lifetime + time.Second)
	code, h, _ = b.get("http://" + *nginx + "//app/x")
	check("expired at //app/x: refreshed, and sent to / of this host", fmt.Sprint(code, " ", h.Get("Location")), "307 /")

	// logout: the session goes, to the IdP's end session endpoint
	code, h, _ = b.get("http://" + *nginx + "/logout")
	loc, _ := url.Parse(h.Get("Location"))
	check("logout: to the end session endpoint", fmt.Sprint(code, " ", loc.Path), "302 /end_session")
	check("with the hint, the client and where to come back to", fmt.Sprint(loc.Query().Get("id_token_hint") != "", " ", loc.Query().Get("client_id"), " ", loc.Query().Get("post_logout_redirect_uri")),
		"true "+client+" http://"+*nginx+"/_logout")
	check("the session cookies are gone", b.cookies["wxoidc_s0"] == "", true)
	code, h, _ = b.get(app)
	check("and the next request logs in again", fmt.Sprint(code, " ", strings.Contains(h.Get("Location"), "/authorize?")), "302 true")
	code, _, body = b.get("http://" + *nginx + "/_logout")
	check("the post-logout page", fmt.Sprint(code, " ", strings.TrimSpace(body)), "200 You have been logged out.")
	// a login that started at //app/x - which nginx serves as /app/x - is
	// sent back to /, not to the host "app" a browser would read there
	evil := newBrowser()
	_, h, _ = evil.get("http://" + *nginx + "//app/x")
	_, h, _ = evil.get(h.Get("Location"))
	_, h, _ = evil.get(h.Get("Location"))
	check("a login that started at //app/x comes back to /", h.Get("Location"), "/")
	code, _, _ = newBrowser().get("http://" + *nginx + "/open")
	check("a location without the Policy needs no session", code, 200)

	if *discovery {
		discovered(p, *nginx, *api)
	}

	if failed > 0 {
		fmt.Printf("%d failed\n", failed)
		os.Exit(1)
	}
}

// discovered checks what needs an issuer: logouts of the IdP, revocations, the iss check.
func discovered(p *idp, nginx, api string) {
	app := "http://" + nginx + "/app/page"
	fresh := func() *browser {
		b := newBrowser()
		got, _ := b.login(app)
		if !strings.HasPrefix(got, "200 alice") {
			fmt.Printf("FAIL     a login for the next check: %s\n", got)
			failed++
		}
		return b
	}
	served := func(b *browser) int { code, _, _ := b.get(app); return code }

	// back channel: the IdP posts a logout token for the browser's sid
	b := fresh()
	sid := p.sid
	resp, err := http.PostForm("http://"+nginx+"/_backchannel_logout", url.Values{"logout_token": {p.logoutToken(sid)}})
	code := 0
	if err == nil {
		code = resp.StatusCode
		resp.Body.Close()
	}
	check("back-channel logout: a good logout token is taken", code, 200)
	check("and the browser's session ends", served(b), 302)
	resp, err = http.PostForm("http://"+nginx+"/_backchannel_logout", url.Values{"logout_token": {p.idToken("alice", "")}})
	if err == nil {
		code = resp.StatusCode
		resp.Body.Close()
	}
	check("an ID token is no logout token: 400", code, 400)

	// front channel: the IdP's iframe with iss and sid - which anybody can send
	b = fresh()
	front := "http://" + nginx + "/front_channel_logout?iss=" + url.QueryEscape(p.issuer) + "&sid=" + url.QueryEscape(p.sid)
	c2, _, _ := newBrowser().get(front)
	check("front-channel logout answers the iframe", c2, 200)
	check("without the browser's own session it ends nothing", served(b), 200)
	copied := newBrowser()
	for k, v := range b.cookies {
		copied.cookies[k] = v
	}
	c2, _, _ = b.get(front)
	check("from the browser of the session it is answered too", c2, 200)
	check("and its cookies go", b.cookies["wxoidc_s0"] == "", true)
	check("and the sessions of its sid end, a copy of the cookie too", served(copied), 302)

	// RP-initiated logout ends a copy of the cookie too
	b = fresh()
	copied = newBrowser()
	for k, v := range b.cookies {
		copied.cookies[k] = v
	}
	check("a copy of the session cookie is served", served(copied), 200)
	b.get("http://" + nginx + "/logout")
	check("after /logout the copy is not", served(copied), 302)

	// an administrator's revocation, as the controller puts it from its ConfigMap
	b = fresh()
	unix := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", api)
	}}}
	body := fmt.Sprintf(`{"all":0,"entries":[{"key":"sub:alice","before":%d}]}`, time.Now().Unix()+1)
	req, _ := http.NewRequest("PUT", "http://api/api/wxingress/1/oidc/revocations", strings.NewReader(body))
	resp, err = unix.Do(req)
	code = 0
	if err == nil {
		code = resp.StatusCode
		resp.Body.Close()
	}
	check("the controller sets the revocations", code, 200)
	check("a session of a revoked sub ends", served(b), 302)
	resp, err = unix.Get("http://api/api/wxingress/1/oidc/revocations?local=1")
	listed := ""
	if err == nil {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		listed = string(raw)
	}
	check("the logouts stay as local entries for the controller, the ConfigMap's are not local",
		fmt.Sprint(strings.Count(listed, `"local":true`) >= 3, " ", strings.Contains(listed, "sub:alice")), "true false")
	time.Sleep(1100 * time.Millisecond)
	b = fresh()
	check("a login after the revocation is served", served(b), 200)

	// an ID token from another issuer
	p.mu.Lock()
	p.issuer = "http://elsewhere.example"
	p.mu.Unlock()
	got, _ := newBrowser().login(app)
	check("an ID token whose iss is not the issuer: no session", strings.HasPrefix(got, "401"), true)
}
