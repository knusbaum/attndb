package mcpauth

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/bcrypt"
)

const testPassword = "test-only-long-password"
const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

var testPasswordHash = func() string {
	b, err := bcrypt.GenerateFromPassword([]byte(testPassword), 10)
	if err != nil {
		panic(err)
	}
	return string(b)
}()

const callback = "http://127.0.0.1:9898/callback"

func setup(t *testing.T) (*Server, Config) {
	t.Helper()
	c := Config{Issuer: "https://attn.example", PasswordHash: testPasswordHash, StateDir: filepath.Join(t.TempDir(), "auth")}
	s, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s, c
}
func request(s *Server, method, path, body, ct string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://attn.example"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", ct)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
	return w
}
func data(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	var out map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	return out
}
func registerClient(t *testing.T, s *Server) string {
	t.Helper()
	return data(t, request(s, "POST", "/oauth/register", `{"redirect_uris":["`+callback+`"],"client_name":"test client","token_endpoint_auth_method":"none"}`, "application/json"), 201)["client_id"].(string)
}
func authQuery(s *Server, id string) url.Values {
	return url.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {callback}, "state": {"client-state"}, "resource": {s.cfg.Resource}, "code_challenge_method": {"S256"}, "code_challenge": {challenge}, "scope": {"mcp"}}
}
func loginPage(t *testing.T, s *Server, q url.Values) (*http.Cookie, string) {
	t.Helper()
	w := request(s, "GET", "/oauth/authorize?"+q.Encode(), "", "")
	if w.Code != 200 {
		t.Fatalf("login status %d: %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing login cookie")
	}
	c := cookies[0]
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatal("unsafe cookie")
	}
	if !strings.Contains(w.Body.String(), `name="csrf" value="`+c.Value+`"`) {
		t.Fatal("missing csrf field")
	}
	return c, c.Value
}
func authorize(t *testing.T, s *Server, id string) string {
	t.Helper()
	cookie, csrf := loginPage(t, s, authQuery(s, id))
	w := request(s, "POST", "/oauth/authorize", url.Values{"csrf": {csrf}, "password": {testPassword}}.Encode(), "application/x-www-form-urlencoded", cookie)
	if w.Code != 303 {
		t.Fatalf("authorize: %d %s", w.Code, w.Body)
	}
	u, e := url.Parse(w.Header().Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	if u.Query().Get("state") != "client-state" || u.Query().Get("iss") != s.cfg.Issuer {
		t.Fatal("state/issuer missing")
	}
	return u.Query().Get("code")
}
func codeForm(s *Server, id, code string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {id}, "redirect_uri": {callback}, "code_verifier": {verifier}, "resource": {s.cfg.Resource}}
}
func tokenRequest(s *Server, q url.Values) *httptest.ResponseRecorder {
	return request(s, "POST", "/oauth/token", q.Encode(), "application/x-www-form-urlencoded")
}
func tokens(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	return data(t, tokenRequest(s, codeForm(s, id, authorize(t, s, id))), 200)
}
func bearer(s *Server, at, path string) int {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+at)
	w := httptest.NewRecorder()
	s.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
	return w.Code
}
func refreshForm(s *Server, id, rt string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {rt}, "resource": {s.cfg.Resource}}
}

func TestDiscoveryAndFlow(t *testing.T) {
	s, _ := setup(t)
	w := request(s, "GET", "/mcp", "", "")
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), `resource_metadata="https://attn.example/.well-known/oauth-protected-resource"`) {
		t.Fatal("missing challenge")
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		d := data(t, request(s, "GET", p, "", ""), 200)
		if d["resource"] != s.cfg.Resource {
			t.Fatal("bad resource")
		}
	}
	d := data(t, request(s, "GET", "/.well-known/oauth-authorization-server", "", ""), 200)
	if d["issuer"] != s.cfg.Issuer || d["registration_endpoint"] != s.cfg.Issuer+"/oauth/register" {
		t.Fatal("bad discovery")
	}
	id := registerClient(t, s)
	q := codeForm(s, id, authorize(t, s, id))
	w = tokenRequest(s, q)
	tok := data(t, w, 200)
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Pragma") != "no-cache" {
		t.Fatal("token cacheable")
	}
	if bearer(s, tok["access_token"].(string), "/mcp") != 204 {
		t.Fatal("valid bearer rejected")
	}
	if bearer(s, tok["refresh_token"].(string), "/mcp") != 401 {
		t.Fatal("refresh accepted as bearer")
	}
	if bearer(s, tok["access_token"].(string), "/other") != 404 {
		t.Fatal("wrong resource path accepted")
	}
	data(t, tokenRequest(s, q), 400)
}

func TestPKCEAndExchangeRejections(t *testing.T) {
	for _, test := range []struct{ name, key, value string }{
		{"wrong verifier", "code_verifier", strings.Repeat("a", 43)},
		{"short verifier", "code_verifier", "short"},
		{"wrong redirect", "redirect_uri", "https://evil.example/callback"},
		{"wrong client", "client_id", "unknown"},
		{"missing resource", "resource", ""},
		{"wrong audience", "resource", "https://elsewhere.example/mcp"},
		{"unsupported grant", "grant_type", "password"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := setup(t)
			id := registerClient(t, s)
			q := codeForm(s, id, authorize(t, s, id))
			q.Set(test.key, test.value)
			data(t, tokenRequest(s, q), 400)
		})
	}
	// RFC 7636 known-answer vector, not a self-consistent implementation-only test.
	if hash(verifier) != challenge {
		t.Fatal("S256 RFC vector mismatch")
	}
}

func TestAuthorizeValidationAndCSRF(t *testing.T) {
	for _, test := range []struct{ name, key, value string }{
		{"redirect", "redirect_uri", "https://evil.example/cb"}, {"client", "client_id", "bogus"},
		{"plain PKCE", "code_challenge_method", "plain"}, {"empty PKCE", "code_challenge", ""},
		{"malformed PKCE", "code_challenge", strings.Repeat("!", 43)}, {"implicit", "response_type", "token"},
		{"resource", "resource", "https://elsewhere.example"}, {"scope", "scope", "admin"}, {"password query", "password", testPassword},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := setup(t)
			id := registerClient(t, s)
			q := authQuery(s, id)
			q.Set(test.key, test.value)
			data(t, request(s, "GET", "/oauth/authorize?"+q.Encode(), "", ""), 400)
		})
	}
	for _, mode := range []string{"missing cookie", "wrong cookie", "wrong csrf", "query password", "bad password", "foreign origin", "expired", "replay"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := setup(t)
			id := registerClient(t, s)
			cookie, csrf := loginPage(t, s, authQuery(s, id))
			q := url.Values{"csrf": {csrf}, "password": {testPassword}}
			path := "/oauth/authorize"
			status := 403
			switch mode {
			case "wrong cookie":
				cookie.Value = random()
			case "wrong csrf":
				q.Set("csrf", random())
			case "query password":
				path += "?password=" + testPassword
				q.Del("password")
				status = 400
			case "bad password":
				q.Set("password", "bad")
			case "expired":
				g := s.pending[hash(csrf)]
				g.Expires = time.Now().Add(-time.Second)
				s.pending[hash(csrf)] = g
			case "replay":
				w := request(s, "POST", path, q.Encode(), "application/x-www-form-urlencoded", cookie)
				if w.Code != 303 {
					t.Fatal(w.Code)
				}
			}
			r := httptest.NewRequest("POST", "https://attn.example"+path, strings.NewReader(q.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if mode != "missing cookie" {
				r.AddCookie(cookie)
			}
			if mode == "foreign origin" {
				r.Header.Set("Origin", "https://evil.example")
			}
			w := httptest.NewRecorder()
			s.Handler(http.NotFoundHandler()).ServeHTTP(w, r)
			data(t, w, status)
		})
	}
}

func TestRegistrationRejections(t *testing.T) {
	for _, raw := range []string{"http://example.com/cb", "https://example.com/cb#fragment", "https://user@example.com/cb", "javascript:alert(1)", "/relative", "https://example.com/cb#"} {
		t.Run(raw, func(t *testing.T) {
			s, _ := setup(t)
			b, _ := json.Marshal(map[string]any{"redirect_uris": []string{raw}})
			data(t, request(s, "POST", "/oauth/register", string(b), "application/json"), 400)
		})
	}
	s, _ := setup(t)
	for _, body := range []string{`{}`, `{"redirect_uris":["https://example.com/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, `{"redirect_uris":["https://example.com/cb"],"grant_types":["password"]}`, `{"redirect_uris":["https://example.com/cb"]} {}`, strings.Repeat("x", 17000)} {
		data(t, request(s, "POST", "/oauth/register", body, "application/json"), 400)
	}
	for _, path := range []string{"/oauth/token", "/oauth/register"} {
		data(t, request(s, "GET", path, "", ""), 405)
	}
}

func TestRefreshPersistenceReplayAndExpiry(t *testing.T) {
	s, c := setup(t)
	id := registerClient(t, s)
	tok := tokens(t, s, id)
	at, rt := tok["access_token"].(string), tok["refresh_token"].(string)
	b, e := os.ReadFile(filepath.Join(c.StateDir, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{at, rt, testPassword} {
		if strings.Contains(string(b), secret) {
			t.Fatal("plaintext secret on disk")
		}
	}
	info, _ := os.Stat(filepath.Join(c.StateDir, "state.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("state is not private")
	}
	if _, e = New(c); e == nil {
		t.Fatal("second process lock accepted")
	}
	s.Close()
	reopened, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	s = reopened
	if bearer(s, at, "/mcp") != 204 {
		t.Fatal("access lost at restart")
	}
	wrong := refreshForm(s, id, rt)
	wrong.Set("resource", "https://wrong.example/mcp")
	data(t, tokenRequest(s, wrong), 400)
	rotated := data(t, tokenRequest(s, refreshForm(s, id, rt)), 200)
	if rotated["refresh_token"] == rt {
		t.Fatal("not rotated")
	}
	s.Close()
	s, e = New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	// Old-token tombstone survives restart. Replay revokes new refresh AND access.
	data(t, tokenRequest(s, refreshForm(s, id, rt)), 400)
	if bearer(s, rotated["access_token"].(string), "/mcp") != 401 || bearer(s, at, "/mcp") != 401 {
		t.Fatal("replay did not revoke family")
	}
	data(t, tokenRequest(s, refreshForm(s, id, rotated["refresh_token"].(string))), 400)
	fresh := tokens(t, s, id)
	key := hash(fresh["refresh_token"].(string))
	v := s.disk.Refresh[key]
	v.Expires = time.Now().Add(-time.Second)
	s.disk.Refresh[key] = v
	data(t, tokenRequest(s, refreshForm(s, id, fresh["refresh_token"].(string))), 400)
	key = hash(fresh["access_token"].(string))
	v = s.disk.Access[key]
	v.Expires = time.Now().Add(-time.Second)
	s.disk.Access[key] = v
	if bearer(s, fresh["access_token"].(string), "/mcp") != 401 {
		t.Fatal("expired access accepted")
	}
}

func TestBoundsCleanupAndStorageFailure(t *testing.T) {
	s, _ := setup(t)
	id := registerClient(t, s)
	for i := 0; i < maxPending; i++ {
		s.pending[string(rune(i))] = grant{Expires: time.Now().Add(time.Hour)}
	}
	data(t, request(s, "GET", "/oauth/authorize?"+authQuery(s, id).Encode(), "", ""), 503)
	for k := range s.pending {
		s.pending[k] = grant{Expires: time.Now().Add(-time.Second)}
	}
	loginPage(t, s, authQuery(s, id))
	if len(s.pending) != 1 {
		t.Fatal("pending not swept")
	}
	s.limits["/oauth/register:POST"] = budget{Start: time.Now(), Count: 10}
	data(t, request(s, "POST", "/oauth/register", `{}`, "application/json"), 429)
	s.limits["/oauth/authorize:POST"] = budget{Start: time.Now(), Count: 10}
	data(t, request(s, "POST", "/oauth/authorize", "", "application/x-www-form-urlencoded"), 429)
}

func TestStorageFailureFailsClosed(t *testing.T) {
	s, _ := setup(t)
	id := registerClient(t, s)
	tok := tokens(t, s, id)
	// Deterministic write failure even under root, without touching real state.
	s.cfg.StateDir = filepath.Join(s.cfg.StateDir, "nonexistent", "child")
	data(t, tokenRequest(s, refreshForm(s, id, tok["refresh_token"].(string))), 503)
	if bearer(s, tok["access_token"].(string), "/mcp") != 401 {
		t.Fatal("storage failure did not fail closed")
	}
	data(t, request(s, "POST", "/oauth/register", `{}`, "application/json"), 503)
}

func TestStreamingIntegration(t *testing.T) {
	s, _ := setup(t)
	id := registerClient(t, s)
	tok := tokens(t, s, id)
	server := mcp.NewServer(&mcp.Implementation{Name: "auth-test", Version: "1"}, nil)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(s.Handler(transport))
	defer ts.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	r, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok["access_token"].(string))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "auth-test") || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("MCP initialize failed: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	// A held-open stream must deliver its first event before the handler returns.
	release := make(chan struct{})
	streaming := httptest.NewServer(s.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		if e := http.NewResponseController(w).Flush(); e != nil {
			t.Error(e)
		}
		<-release
	})))
	defer streaming.Close()
	defer close(release)
	r, _ = http.NewRequest("GET", streaming.URL+"/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+tok["access_token"].(string))
	resp, e = client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	line, e := bufio.NewReader(resp.Body).ReadString('\n')
	if e != nil || line != "data: first\n" {
		t.Fatalf("stream not flushed: %q %v", line, e)
	}
}

func TestDuplicateParametersAndURLTokens(t *testing.T) {
	s, _ := setup(t)
	id := registerClient(t, s)
	q := authQuery(s, id)
	q.Add("client_id", id)
	data(t, request(s, "GET", "/oauth/authorize?"+q.Encode(), "", ""), 400)
	code := authorize(t, s, id)
	q = codeForm(s, id, code)
	q.Add("resource", s.cfg.Resource)
	data(t, tokenRequest(s, q), 400)
	tok := data(t, tokenRequest(s, codeForm(s, id, code)), 200)
	at := tok["access_token"].(string)
	data(t, request(s, "GET", "/mcp?access_token="+at, "", ""), 401)
	q = refreshForm(s, id, tok["refresh_token"].(string))
	rt := s.disk.Refresh[hash(q.Get("refresh_token"))]
	renewed := data(t, tokenRequest(s, q), 200)
	if !s.disk.Refresh[hash(renewed["refresh_token"].(string))].Expires.Equal(rt.Expires) {
		t.Fatal("refresh extended absolute lifetime")
	}
}

// Receiving a slow body must not hold the global state mutex and stall MCP.
func TestSlowOAuthBodyDoesNotBlockBearer(t *testing.T) {
	s, _ := setup(t)
	id := registerClient(t, s)
	tok := tokens(t, s, id)
	reader, writer := io.Pipe()
	defer writer.Close()
	started := make(chan struct{})
	done := make(chan struct{})
	r := httptest.NewRequest("POST", "https://attn.example/oauth/token", reader)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	go func() {
		close(started)
		s.Handler(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), r)
		close(done)
	}()
	<-started
	result := make(chan int, 1)
	go func() { result <- bearer(s, tok["access_token"].(string), "/mcp") }()
	select {
	case status := <-result:
		if status != 204 {
			t.Fatal(status)
		}
	case <-time.After(time.Second):
		t.Error("slow upload blocked bearer")
	}
	writer.Close()
	<-done
}
