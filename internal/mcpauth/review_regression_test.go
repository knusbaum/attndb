package mcpauth

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise a real form POST and 303 to a different origin. HTTP clients do not
// enforce CSP, so also guard the policy that would block this flow in Chromium.
func TestCrossOriginAuthorizationCallback(t *testing.T) {
	callbacks := make(chan url.Values, 1)
	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("callback method = %s, want GET", r.Method)
		}
		callbacks <- r.URL.Query()
		io.WriteString(w, "callback reached")
	}))
	defer callbackServer.Close()
	authServer := httptest.NewUnstartedServer(nil)
	c := Config{
		Issuer:       "http://" + authServer.Listener.Addr().String(),
		PasswordHash: testPasswordHash, StateDir: filepath.Join(t.TempDir(), "auth"),
		AllowHTTP: true,
	}
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	authServer.Config.Handler = s.Handler(http.NotFoundHandler())
	authServer.Start()
	defer authServer.Close()
	redirect := callbackServer.URL + "/callback"
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{redirect}})
	id := data(t, request(s, "POST", "/oauth/register", string(body), "application/json"), 201)["client_id"].(string)
	q := authQuery(s, id)
	q.Set("redirect_uri", redirect)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	resp, err := client.Get(authServer.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login: status=%d err=%v", resp.StatusCode, err)
	}
	policy := resp.Header.Get("Content-Security-Policy")
	if strings.Contains(policy, "form-action") {
		t.Fatalf("CSP may block cross-origin 303 callback: %s", policy)
	}
	for _, directive := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Fatalf("missing CSP protection %q", directive)
		}
	}
	if !strings.Contains(string(page), `action="`+s.cfg.Issuer+`/oauth/authorize"`) {
		t.Fatal("login action is not fixed to the issuer")
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || !strings.Contains(string(page), `name="csrf" value="`+cookies[0].Value+`"`) {
		t.Fatal("missing CSRF cookie/field binding")
	}
	resp, err = client.PostForm(authServer.URL+"/oauth/authorize", url.Values{
		"csrf": {cookies[0].Value}, "password": {testPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || string(result) != "callback reached" {
		t.Fatalf("callback failed: status=%d body=%q err=%v", resp.StatusCode, result, err)
	}
	received := <-callbacks
	if received.Get("state") != "client-state" || received.Get("iss") != s.cfg.Issuer || received.Get("code") == "" {
		t.Fatalf("invalid callback parameters: %v", received)
	}
	exchange := codeForm(s, id, received.Get("code"))
	exchange.Set("redirect_uri", redirect)
	data(t, tokenRequest(s, exchange), 200)
}

func TestRejectIncompleteStateWithoutRewrite(t *testing.T) {
	valid := map[string]any{
		"Version": 1, "Issuer": "https://attn.example", "Resource": "https://attn.example/mcp",
		"Clients": map[string]any{}, "Access": map[string]any{}, "Refresh": map[string]any{},
	}
	cases := map[string][]byte{"null": []byte("null"), "empty object": []byte("{}")}
	for key := range valid {
		for _, mode := range []string{"missing", "null"} {
			fields := make(map[string]any)
			for k, v := range valid {
				fields[k] = v
			}
			if mode == "missing" {
				delete(fields, key)
			} else {
				fields[key] = nil
			}
			cases[mode+" "+key], _ = json.Marshal(fields)
		}
	}
	for key, value := range map[string]any{"Version": 2, "Issuer": "https://wrong.example", "Resource": "https://wrong.example/mcp"} {
		fields := make(map[string]any)
		for k, v := range valid {
			fields[k] = v
		}
		fields[key] = value
		cases["wrong "+key], _ = json.Marshal(fields)
	}
	for name, original := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "auth")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{Issuer: "https://attn.example", PasswordHash: testPasswordHash, StateDir: dir}
			s, err := New(c)
			if s != nil {
				s.Close()
			}
			if err == nil || s != nil {
				t.Fatal("invalid persisted state accepted")
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("rejected startup rewrote state")
			}
			// Failed startup must release the lock so repaired state can open.
			repaired, _ := json.Marshal(valid)
			if err := os.WriteFile(path, repaired, 0600); err != nil {
				t.Fatal(err)
			}
			s, err = New(c)
			if err != nil {
				t.Fatalf("cannot open repaired state: %v", err)
			}
			s.Close()
		})
	}
}
