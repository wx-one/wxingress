package nginx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewAPITokenIsRandomAndLong(t *testing.T) {
	t.Parallel()

	first, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 || strings.Trim(first, "0123456789abcdef") != "" {
		t.Errorf("token %q is not 64 hex digits", first)
	}
	if first == second {
		t.Error("two tokens are the same")
	}
}

func TestAPITokenConfRefusesWithoutTheToken(t *testing.T) {
	t.Parallel()

	got := string(APITokenConf("abc"))
	want := "if ($http_x_wxingress_api_token != \"abc\") {\n    return 403;\n}\n"
	if got != want {
		t.Errorf("APITokenConf() = %q, want %q", got, want)
	}
}

func TestWithAPITokenSendsTheToken(t *testing.T) {
	t.Parallel()

	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(APITokenHeader))
	}))
	defer server.Close()

	c := WithAPIToken(&http.Client{}, "the-token")
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if req.Header.Get(APITokenHeader) != "" {
		t.Error("the caller's request was changed")
	}

	resp, err = WithAPIToken(&http.Client{}, "").Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(seen) != 2 || seen[0] != "the-token" || seen[1] != "" {
		t.Errorf("the server saw %q, want [the-token \"\"]", seen)
	}
}
