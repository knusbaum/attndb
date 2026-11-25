package mcpauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// HTTP fixtures do not implement browser Fetch policy. Assert the policy that
// preserves navigation POST Origin per Fetch, then exercise the resulting
// canonical Origin as well as null and foreign Origins explicitly.
func TestLoginReferrerPolicyAndOrigin(t *testing.T) {
	s, err := New(Config{
		Issuer: "https://attn.knusbaum.com", PasswordHash: testPasswordHash,
		StateDir: filepath.Join(t.TempDir(), "oauth"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := registerClient(t, s)
	w := request(s, "GET", "/oauth/authorize?"+authQuery(s, id).Encode(), "", "")
	if w.Code != 200 || w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("login HTML must preserve same-origin form POST Origin: %d %q", w.Code, w.Header().Get("Referrer-Policy"))
	}
	if !strings.Contains(w.Body.String(), `method="post" action="https://attn.knusbaum.com/oauth/authorize"`) {
		t.Fatal("form action must stay on canonical issuer")
	}
	if strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("sandbox can force an opaque Origin")
	}
	cookie := w.Result().Cookies()[0]
	formBody := url.Values{"csrf": {cookie.Value}, "password": {testPassword}}.Encode()
	for _, origin := range []string{"null", "https://foreign.example", "https://attn.knusbaum.com"} {
		r := httptest.NewRequest("POST", s.cfg.Issuer+"/oauth/authorize", strings.NewReader(formBody))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		r.AddCookie(cookie)
		result := httptest.NewRecorder()
		s.Handler(http.NotFoundHandler()).ServeHTTP(result, r)
		if result.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("POST responses must suppress callback referrers")
		}
		if origin != s.cfg.Issuer {
			if result.Code != 403 || result.Body.String() != "{\"error\":\"access_denied\"}\n" {
				t.Fatal("null/foreign origin bypassed rejection")
			}
			continue
		}
		if result.Code != 303 {
			t.Fatalf("canonical origin rejected: %d", result.Code)
		}
		target, err := url.Parse(result.Header().Get("Location"))
		if err != nil || target.Scheme+"://"+target.Host != "http://127.0.0.1:9898" || target.Query().Get("code") == "" {
			t.Fatal("registered cross-origin callback not issued")
		}
	}
}
