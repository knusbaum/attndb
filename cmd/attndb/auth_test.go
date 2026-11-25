package main

import (
	"encoding/json"
	"github.com/kjn/attndb/internal/mcpauth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"time"

	"strings"
	"testing"
)

// Auth must fail before opening a vault, contacting Qdrant, or loading encoders.
func TestServeAuthStartup(t *testing.T) {
	for _, k := range []string{"ATTNDB_AUTH_ISSUER", "ATTNDB_AUTH_RESOURCE", "ATTNDB_AUTH_PASSWORD", "ATTNDB_AUTH_PASSWORD_FILE", "ATTNDB_AUTH_PASSWORD_HASH", "ATTNDB_AUTH_PASSWORD_HASH_FILE", "ATTNDB_AUTH_STATE_DIR"} {
		t.Setenv(k, "")
	}
	for _, args := range [][]string{{"-auth-issuer", "https://attn.example"}, {"-auth-password-file", "/does-not-exist"}, {"-auth-password-hash-file", "/does-not-exist"}, {"-auth-allow-http-localhost"}} {
		err := runServe(args)
		if err == nil || !strings.HasPrefix(err.Error(), "MCP auth:") {
			t.Fatalf("partial auth did not fail at startup: %v", err)
		}
	}
	t.Setenv("ATTNDB_AUTH_ISSUER", "https://attn.example")
	if err := runServe(nil); err == nil || !strings.HasPrefix(err.Error(), "MCP auth:") {
		t.Fatalf("partial environment did not fail closed: %v", err)
	}
	t.Setenv("ATTNDB_AUTH_ISSUER", "")
	if err := runServe([]string{"-store", "memory"}); err == nil || !strings.Contains(err.Error(), "serve requires -store qdrant") {
		t.Fatalf("unauthenticated local startup changed: %v", err)
	}
}

// Real loopback HTTP requests with a public Host and cross-site browser headers
// exercise the SDK 1.4.x guards that otherwise break reverse-proxy deployments.
func TestAuthenticatedMCPBehindProxy(t *testing.T) {
	hash, err := mcpauth.GeneratePasswordHash([]byte("a-long-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := mcpauth.New(mcpauth.Config{Issuer: "https://attn.example", PasswordHash: string(hash), StateDir: filepath.Join(t.TempDir(), "auth")})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "proxy-test", Version: "1"}, nil)
	ts := httptest.NewServer(newMCPHTTPHandler(server, auth))
	defer ts.Close()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(method, path, body, ct, bearer string, cookie *http.Cookie) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		r.Host = "attn.example"
		r.Header.Set("Content-Type", ct)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if path == "/mcp" {
			r.Header.Set("Origin", "https://client.example")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Accept", "application/json, text/event-stream")
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	decode := func(resp *http.Response, want int) map[string]any {
		t.Helper()
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("status %d want %d: %s", resp.StatusCode, want, b)
		}
		var d map[string]any
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	id := decode(send("POST", "/oauth/register", `{"redirect_uris":["http://127.0.0.1:9999/cb"],"token_endpoint_auth_method":"none"}`, "application/json", "", nil), 201)["client_id"].(string)
	q := url.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {"http://127.0.0.1:9999/cb"}, "resource": {"https://attn.example/mcp"}, "code_challenge_method": {"S256"}, "code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}}
	resp := send("GET", "/oauth/authorize?"+q.Encode(), "", "", "", nil)
	if resp.StatusCode != 200 || len(resp.Cookies()) != 1 {
		t.Fatal("login failed")
	}
	cookie := resp.Cookies()[0]
	resp.Body.Close()
	resp = send("POST", "/oauth/authorize", url.Values{"csrf": {cookie.Value}, "password": {"a-long-test-password"}}.Encode(), "application/x-www-form-urlencoded", "", cookie)
	if resp.StatusCode != 303 {
		t.Fatal("authorization failed", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	q = url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "client_id": {id}, "redirect_uri": {"http://127.0.0.1:9999/cb"}, "resource": {"https://attn.example/mcp"}, "code_verifier": {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"}}
	at := decode(send("POST", "/oauth/token", q.Encode(), "application/x-www-form-urlencoded", "", nil), 200)["access_token"].(string)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	decode(send("POST", "/mcp", body, "application/json", "", nil), 401)
	resp = send("POST", "/mcp", body, "application/json", at, nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "proxy-test") || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("proxy/browser MCP: %d %s", resp.StatusCode, b)
	}
	// Disabling auth retains the SDK's default protections against public Host
	// rebinding and cross-site requests; the compatibility override is not global.
	local := httptest.NewServer(newMCPHTTPHandler(server, nil))
	defer local.Close()
	r, _ := http.NewRequest("POST", local.URL, strings.NewReader(body))
	r.Host = "attn.example"
	r.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("unauthenticated SDK guard disabled: %d", resp.StatusCode)
	}
}
