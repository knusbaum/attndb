package mcpauth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

type grant struct {
	Client, Redirect, Challenge, Resource, State string
	Expires                                      time.Time
}
type budget struct {
	Start time.Time
	Count int
}

// Server is a single-user, single-process embedded authorization server. A
// successful login grants all MCP tools; client names are untrusted labels.
type Server struct {
	cfg            Config
	password       string
	mu             sync.Mutex
	disk           diskState
	pending, codes map[string]grant
	limits         map[string]budget
	lock           *os.File
	failed         bool
	logger         *log.Logger
}

// New returns nil,nil only when no auth setting is supplied. Callers must never
// recover from an error by serving an unauthenticated handler.
func New(c Config) (*Server, error) {
	if !c.enabled() {
		return nil, nil
	}
	c, err := c.validate()
	if err != nil {
		return nil, err
	}
	if c.PasswordHash == "" {
		generated, err := GeneratePasswordHash([]byte(c.Password))
		if err != nil {
			return nil, err
		}
		c.PasswordHash = string(generated)
	}
	s := &Server{logger: log.Default(), cfg: c, password: c.PasswordHash, pending: map[string]grant{}, codes: map[string]grant{}, limits: map[string]budget{}}
	s.cfg.Password = ""
	s.cfg.PasswordHash = ""
	s.disk = diskState{Version: 1, Issuer: c.Issuer, Resource: c.Resource, Clients: map[string]client{}, Access: map[string]token{}, Refresh: map[string]token{}}
	if err = s.openState(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func random() string      { return base64.RawURLEncoding.EncodeToString(randomBytes()) }
func randomBytes() []byte { b := make([]byte, 32); _, _ = rand.Read(b); return b } // crypto/rand is fail-stop in supported Go
func hash(v string) string {
	h := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func reject(w http.ResponseWriter, status int, code string) {
	reply(w, status, map[string]string{"error": code})
}
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// Handler mounts metadata and OAuth endpoints and protects only the configured
// MCP resource path. It never wraps ResponseWriter, so flushing/SSE survives.
func (s *Server) Handler(next http.Handler) http.Handler {
	if s == nil {
		return next
	}
	resource, _ := url.Parse(s.cfg.Resource)
	resourcePath := resource.Path
	if resourcePath == "" {
		resourcePath = "/"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		noStore(w)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if p != "/oauth/authorize" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version, MCP-Session-Id, Last-Event-ID")
			w.Header().Set("Access-Control-Expose-Headers", "MCP-Session-Id, WWW-Authenticate")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		switch p {
		case "/.well-known/oauth-authorization-server":
			if r.Method != "GET" {
				reject(w, 405, "invalid_request")
				return
			}
			reply(w, 200, map[string]any{"issuer": s.cfg.Issuer, "authorization_endpoint": s.cfg.Issuer + "/oauth/authorize", "token_endpoint": s.cfg.Issuer + "/oauth/token", "registration_endpoint": s.cfg.Issuer + "/oauth/register", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{"mcp"}, "authorization_response_iss_parameter_supported": true})
		case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource" + resourcePath:
			if r.Method != "GET" {
				reject(w, 405, "invalid_request")
				return
			}
			reply(w, 200, map[string]any{"resource": s.cfg.Resource, "authorization_servers": []string{s.cfg.Issuer}, "bearer_methods_supported": []string{"header"}, "scopes_supported": []string{"mcp"}})
		case "/oauth/register", "/oauth/authorize", "/oauth/token":
			s.oauth(w, r)
		default:
			if p != resourcePath || r.URL.RawPath != "" {
				http.NotFound(w, r)
				return
			}
			s.mu.Lock()
			fields := strings.Fields(r.Header.Get("Authorization"))
			var t token
			var ok bool
			if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && len(r.Header.Values("Authorization")) == 1 {
				t, ok = s.disk.Access[hash(fields[1])]
			}
			valid := ok && !s.failed && t.Resource == s.cfg.Resource && time.Now().Before(t.Expires)
			s.mu.Unlock()
			if !valid {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource", scope="mcp"`, s.cfg.Issuer))
				reject(w, 401, "invalid_token")
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}

func (s *Server) oauth(w http.ResponseWriter, r *http.Request) {
	if (r.URL.Path != "/oauth/authorize" && r.Method != "POST") || (r.URL.Path == "/oauth/authorize" && r.Method != "GET" && r.Method != "POST") {
		reject(w, 405, "invalid_request")
		return
	}
	if len(r.URL.RawQuery) > 8192 {
		reject(w, 400, "invalid_request")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	// Read bounded bodies before taking the state mutex: a slow upload must not
	// block bearer validation or other clients. The embedding server sets read deadlines.
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			reject(w, 400, "invalid_request")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	// Serialize mutations and persistence. The protected stream is never under
	// this mutex. Fixed global buckets avoid trusting forwarded client IP headers.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		reject(w, 503, "temporarily_unavailable")
		return
	}
	s.cleanup(time.Now())
	key := r.URL.Path + ":" + r.Method
	limit := 120
	if key == "/oauth/register:POST" {
		limit = 10
	}
	if key == "/oauth/authorize:POST" {
		limit = 10
	}
	b := s.limits[key]
	now := time.Now()
	if now.Sub(b.Start) >= time.Minute {
		b = budget{Start: now}
	}
	b.Count++
	s.limits[key] = b
	if b.Count > limit {
		w.Header().Set("Retry-After", "60")
		reject(w, 429, "temporarily_unavailable")
		return
	}
	switch r.URL.Path {
	case "/oauth/register":
		s.register(w, r)
	case "/oauth/authorize":
		s.authorize(w, r)
	case "/oauth/token":
		s.exchange(w, r)
	}
}

func validRedirect(raw string) bool {
	if len(raw) > 2048 || strings.ContainsAny(raw, "\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Hostname() != "" && u.User == nil && u.Fragment == "" && !strings.Contains(raw, "#") && u.Opaque == "" && (u.Scheme == "https" || (u.Scheme == "http" && loopback(u.Hostname())))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || ct != "application/json" {
		reject(w, 400, "invalid_client_metadata")
		return
	}
	var in struct {
		Redirects []string `json:"redirect_uris"`
		Name      string   `json:"client_name"`
		Auth      string   `json:"token_endpoint_auth_method"`
		Grants    []string `json:"grant_types"`
		Responses []string `json:"response_types"`
	}
	d := json.NewDecoder(r.Body)
	if err := d.Decode(&in); err != nil {
		reject(w, 400, "invalid_client_metadata")
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF || len(in.Redirects) == 0 || len(in.Redirects) > 8 || len(in.Name) > 128 || (in.Auth != "" && in.Auth != "none") {
		reject(w, 400, "invalid_client_metadata")
		return
	}
	for _, v := range in.Grants {
		if v != "authorization_code" && v != "refresh_token" {
			reject(w, 400, "invalid_client_metadata")
			return
		}
	}
	for _, v := range in.Responses {
		if v != "code" {
			reject(w, 400, "invalid_client_metadata")
			return
		}
	}
	redirectBytes := 0
	for _, v := range in.Redirects {
		redirectBytes += len(v)
		if !validRedirect(v) || redirectBytes > 4096 {
			reject(w, 400, "invalid_redirect_uri")
			return
		}
	}
	if len(s.disk.Clients) >= maxClients {
		reject(w, 503, "temporarily_unavailable")
		return
	}
	id := random()
	s.disk.Clients[id] = client{Redirects: in.Redirects, Name: in.Name, Expires: time.Now().Add(clientTTL)}
	if s.save() != nil {
		reject(w, 503, "temporarily_unavailable")
		return
	}
	reply(w, 201, map[string]any{"client_id": id, "client_id_issued_at": time.Now().Unix(), "redirect_uris": in.Redirects, "client_name": in.Name, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
}

var login = template.Must(template.New("login").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Authorize attndb</title></head><body><h1>Authorize attndb</h1><p>Client: {{.Name}}</p><p>Redirect: {{.Redirect}}</p><p>This grants access to ALL vault tools, including writing and deleting documents. Only continue if you initiated this connection and trust this client.</p><form method="post" action="{{.Action}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>Shared password <input type="password" name="password" autocomplete="current-password" required></label><button type="submit">Authorize all tools</button></form></body></html>`))

func (s *Server) cookieName() string {
	if strings.HasPrefix(s.cfg.Issuer, "https:") {
		return "__Host-attndb-login"
	}
	return "attndb-login"
}
func single(v url.Values) bool {
	for _, vs := range v {
		if len(vs) != 1 {
			return false
		}
	}
	return true
}
func challengeOK(v string) bool {
	b, e := base64.RawURLEncoding.DecodeString(v)
	return e == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == v
}
func verifierOK(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c)) {
			return false
		}
	}
	return true
}
func form(r *http.Request) bool {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && ct == "application/x-www-form-urlencoded" && r.ParseForm() == nil && single(r.PostForm)
}

// authorizeReject logs only programmer-defined reason codes. Never pass request
// values or errors here: URLs, origins, cookies and credentials stay out of logs.
func (s *Server) authorizeReject(w http.ResponseWriter, status int, publicError, reason string) {
	s.logger.Printf("mcpauth authorization_rejected reason=%s", reason)
	reject(w, status, publicError)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Do not set form-action: Chromium applies it to the POST redirect chain,
	// blocking registered cross-origin OAuth callbacks. The form action is fixed
	// to our issuer, and POST still requires the transaction cookie and CSRF token.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	if r.Method == "GET" {
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || !single(q) || q.Has("password") || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !challengeOK(q.Get("code_challenge")) {
			s.authorizeReject(w, 400, "invalid_request", "invalid_authorization_parameters")
			return
		}
		c, ok := s.disk.Clients[q.Get("client_id")]
		if !ok || !slices.Contains(c.Redirects, q.Get("redirect_uri")) {
			s.authorizeReject(w, 400, "invalid_request", "unregistered_client_or_redirect")
			return
		}
		if q.Get("resource") != s.cfg.Resource {
			s.authorizeReject(w, 400, "invalid_target", "resource_mismatch")
			return
		}
		if q.Get("scope") != "" && q.Get("scope") != "mcp" {
			s.authorizeReject(w, 400, "invalid_scope", "invalid_scope")
			return
		}
		if len(s.pending) >= maxPending {
			s.authorizeReject(w, 503, "temporarily_unavailable", "pending_capacity")
			return
		}
		csrf := random()
		s.pending[hash(csrf)] = grant{Client: q.Get("client_id"), Redirect: q.Get("redirect_uri"), Challenge: q.Get("code_challenge"), Resource: s.cfg.Resource, State: q.Get("state"), Expires: time.Now().Add(5 * time.Minute)}
		http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: csrf, Path: "/", Secure: strings.HasPrefix(s.cfg.Issuer, "https:"), HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 300})
		// Fetch serializes Origin as null for navigation-mode POSTs under
		// no-referrer. Preserve Origin on our same-origin HTML form submission;
		// cross-origin referrers remain suppressed. POST/303 responses retain
		// no-referrer above, so callbacks receive no authorization-page referrer.
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = login.Execute(w, map[string]string{"Name": c.Name, "Redirect": q.Get("redirect_uri"), "Action": s.cfg.Issuer + "/oauth/authorize", "CSRF": csrf})
		return
	}
	// POST accepts no query parameters. Credentials and transaction identifiers
	// must come from the body, never ParseForm's query/body merged view.
	if r.URL.RawQuery != "" || !form(r) {
		s.authorizeReject(w, 400, "invalid_request", "invalid_post_parameters")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.cfg.Issuer {
		s.authorizeReject(w, 403, "access_denied", "origin_mismatch")
		return
	}
	csrf := r.PostForm.Get("csrf")
	cookie, err := r.Cookie(s.cookieName())
	if err != nil {
		s.authorizeReject(w, 403, "access_denied", "login_cookie_missing")
		return
	}
	if len(csrf) != 43 {
		s.authorizeReject(w, 403, "access_denied", "csrf_invalid_length")
		return
	}
	if subtle.ConstantTimeCompare([]byte(csrf), []byte(cookie.Value)) != 1 {
		s.authorizeReject(w, 403, "access_denied", "csrf_cookie_mismatch")
		return
	}
	g, ok := s.pending[hash(csrf)]
	delete(s.pending, hash(csrf))
	// Consume the transaction under lock, but do expensive password work outside
	// it so bearer requests and token operations remain responsive.
	s.mu.Unlock()
	valid := ok && passwordMatches(s.password, r.PostForm.Get("password"))
	s.mu.Lock()
	if s.failed {
		s.authorizeReject(w, 503, "temporarily_unavailable", "storage_failed")
		return
	}
	if !valid || !time.Now().Before(g.Expires) {
		// cleanup removes expired transactions before this handler, so a missing
		// entry cannot distinguish expiry, replay, restart, or an unknown token.
		reason := "password_verification_mismatch"
		if !ok {
			reason = "transaction_missing_or_expired"
		} else if !time.Now().Before(g.Expires) {
			reason = "transaction_expired_during_verification"
		}
		s.authorizeReject(w, 403, "access_denied", reason)
		return
	}
	if _, ok := s.disk.Clients[g.Client]; !ok {
		s.authorizeReject(w, 400, "invalid_request", "client_missing")
		return
	}
	if len(s.codes) >= maxPending {
		s.authorizeReject(w, 503, "temporarily_unavailable", "code_capacity")
		return
	}
	code := random()
	g.Expires = time.Now().Add(time.Minute)
	s.codes[hash(code)] = g
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Path: "/", Secure: strings.HasPrefix(s.cfg.Issuer, "https:"), HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	target, _ := url.Parse(g.Redirect)
	q := target.Query()
	q.Set("code", code)
	q.Set("iss", s.cfg.Issuer)
	if g.State != "" {
		q.Set("state", g.State)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

func (s *Server) revoke(family string) {
	for _, m := range []map[string]token{s.disk.Access, s.disk.Refresh} {
		for k, v := range m {
			if v.Family == family {
				delete(m, k)
			}
		}
	}
}

func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || !form(r) {
		reject(w, 400, "invalid_request")
		return
	}
	q := r.PostForm
	if q.Get("resource") != s.cfg.Resource {
		reject(w, 400, "invalid_target")
		return
	}
	c, ok := s.disk.Clients[q.Get("client_id")]
	if !ok {
		reject(w, 400, "invalid_client")
		return
	}
	if q.Get("scope") != "" && q.Get("scope") != "mcp" {
		reject(w, 400, "invalid_scope")
		return
	}
	var family string
	expires := time.Now().Add(refreshTTL)
	switch q.Get("grant_type") {
	case "authorization_code":
		key := hash(q.Get("code"))
		g, ok := s.codes[key]
		delete(s.codes, key)
		v := q.Get("code_verifier")
		if !ok || g.Client != q.Get("client_id") || g.Redirect != q.Get("redirect_uri") || g.Resource != q.Get("resource") || !verifierOK(v) || hash(v) != g.Challenge {
			reject(w, 400, "invalid_grant")
			return
		}
		family = random()
	case "refresh_token":
		key := hash(q.Get("refresh_token"))
		t, ok := s.disk.Refresh[key]
		if !ok || t.Client != q.Get("client_id") || t.Resource != q.Get("resource") {
			reject(w, 400, "invalid_grant")
			return
		}
		if t.Used {
			s.revoke(t.Family)
			if s.save() != nil {
				reject(w, 503, "temporarily_unavailable")
				return
			}
			reject(w, 400, "invalid_grant")
			return
		}
		t.Used = true
		s.disk.Refresh[key] = t
		family = t.Family
		expires = t.Expires
	default:
		reject(w, 400, "unsupported_grant_type")
		return
	}
	if len(s.disk.Access)+len(s.disk.Refresh)+2 > maxTokens {
		s.revoke(family)
		if s.save() != nil {
			reject(w, 503, "temporarily_unavailable")
			return
		}
		reject(w, 503, "temporarily_unavailable")
		return
	}
	at, rt := random(), random()
	s.disk.Access[hash(at)] = token{Client: q.Get("client_id"), Resource: s.cfg.Resource, Family: family, Expires: minTime(time.Now().Add(accessTTL), expires)}
	s.disk.Refresh[hash(rt)] = token{Client: q.Get("client_id"), Resource: s.cfg.Resource, Family: family, Expires: expires}
	c.Expires = time.Now().Add(clientTTL)
	s.disk.Clients[q.Get("client_id")] = c
	if s.save() != nil {
		reject(w, 503, "temporarily_unavailable")
		return
	}
	ttl := int(time.Until(s.disk.Access[hash(at)].Expires).Seconds())
	reply(w, 200, map[string]any{"access_token": at, "refresh_token": rt, "token_type": "Bearer", "expires_in": ttl, "scope": "mcp"})
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
