package mcpauth

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuthorizationDenialDiagnostics(t *testing.T) {
	for _, reason := range []string{
		"origin_mismatch", "login_cookie_missing", "csrf_invalid_length",
		"csrf_cookie_mismatch", "transaction_missing_or_expired",
		"password_verification_mismatch",
	} {
		t.Run(reason, func(t *testing.T) {
			s, _ := setup(t)
			var logs bytes.Buffer
			s.logger = log.New(&logs, "", 0)
			id := registerClient(t, s)
			cookie, csrf := loginPage(t, s, authQuery(s, id))
			q := url.Values{"csrf": {csrf}, "password": {testPassword}}
			origin := s.cfg.Issuer
			sendCookie := true
			switch reason {
			case "origin_mismatch":
				origin = "https://private-origin.example"
			case "login_cookie_missing":
				sendCookie = false
			case "csrf_invalid_length":
				q.Set("csrf", "synthetic-short-csrf")
			case "csrf_cookie_mismatch":
				q.Set("csrf", strings.Repeat("z", 43))
			case "transaction_missing_or_expired":
				g := s.pending[hash(csrf)]
				g.Expires = time.Now().Add(-time.Second)
				s.pending[hash(csrf)] = g
			case "password_verification_mismatch":
				q.Set("password", "synthetic-wrong-password")
			}
			r := httptest.NewRequest("POST", s.cfg.Issuer+"/oauth/authorize", strings.NewReader(q.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", origin)
			r.Header.Set("Authorization", "Bearer synthetic-private-token")
			if sendCookie {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			s.Handler(http.NotFoundHandler()).ServeHTTP(w, r)
			if w.Code != 403 || w.Body.String() != "{\"error\":\"access_denied\"}\n" {
				t.Fatalf("public denial changed: %d %s", w.Code, w.Body)
			}
			want := "mcpauth authorization_rejected reason=" + reason + "\n"
			if logs.String() != want {
				t.Fatalf("unexpected diagnostic: %q", logs.String())
			}
			for _, secret := range []string{testPassword, testPasswordHash, csrf, id, origin, "synthetic-private-token", q.Get("password"), q.Get("csrf")} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("request/credential material leaked")
				}
			}
		})
	}
}

func TestCanonicalOriginAndCookieLogin(t *testing.T) {
	s, _ := setup(t)
	var logs bytes.Buffer
	s.logger = log.New(&logs, "", 0)
	id := registerClient(t, s)
	cookie, csrf := loginPage(t, s, authQuery(s, id))
	if cookie.Name != "__Host-attndb-login" || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatal("HTTPS host-cookie contract changed")
	}
	r := httptest.NewRequest("POST", s.cfg.Issuer+"/oauth/authorize",
		strings.NewReader(url.Values{"csrf": {csrf}, "password": {testPassword}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.cfg.Issuer)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Handler(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != 303 || logs.Len() != 0 {
		t.Fatalf("valid browser-origin login failed: %d %s", w.Code, logs.String())
	}
	// A second submit of the same form is deliberately indistinguishable publicly.
	w = request(s, "POST", "/oauth/authorize", url.Values{"csrf": {csrf}, "password": {testPassword}}.Encode(), "application/x-www-form-urlencoded", cookie)
	if w.Code != 403 || logs.String() != "mcpauth authorization_rejected reason=transaction_missing_or_expired\n" {
		t.Fatal("replayed transaction diagnosis incorrect")
	}
}
