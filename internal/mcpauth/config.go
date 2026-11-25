// Package mcpauth implements a single-user OAuth authorization server and HTTP
// resource protection without depending on an MCP SDK.
package mcpauth

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
)

// Config deliberately has no default password or state location. All auth
// settings absent means local development; any partial configuration is an error.
type Config struct {
	Issuer, Resource, Password, PasswordFile, PasswordHash, PasswordHashFile, StateDir string
	AllowHTTP                                                                          bool
}

// Flags registers serve flags, with environment defaults. Passwords are never
// command-line flags (which would expose them in process listings).
func Flags(fs *flag.FlagSet) *Config {
	c := &Config{Password: os.Getenv("ATTNDB_AUTH_PASSWORD"), PasswordHash: os.Getenv("ATTNDB_AUTH_PASSWORD_HASH")}
	fs.StringVar(&c.Issuer, "auth-issuer", os.Getenv("ATTNDB_AUTH_ISSUER"), "public OAuth origin (HTTPS, no path)")
	fs.StringVar(&c.Resource, "auth-resource", os.Getenv("ATTNDB_AUTH_RESOURCE"), "exact public MCP URL (default: issuer + /mcp)")
	fs.StringVar(&c.PasswordFile, "auth-password-file", os.Getenv("ATTNDB_AUTH_PASSWORD_FILE"), "DEPRECATED: plaintext password file; prefer auth-password-hash-file")
	fs.StringVar(&c.PasswordHashFile, "auth-password-hash-file", os.Getenv("ATTNDB_AUTH_PASSWORD_HASH_FILE"), "file containing salted bcrypt verifier (preferred)")
	fs.StringVar(&c.StateDir, "auth-state-dir", os.Getenv("ATTNDB_AUTH_STATE_DIR"), "private persistent OAuth state directory, outside the vault")
	fs.BoolVar(&c.AllowHTTP, "auth-allow-http-localhost", false, "TEST ONLY: allow an HTTP loopback issuer")
	return c
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c Config) enabled() bool {
	return c.Issuer != "" || c.Resource != "" || c.Password != "" || c.PasswordFile != "" || c.PasswordHash != "" || c.PasswordHashFile != "" || c.StateDir != "" || c.AllowHTTP
}

func (c Config) validate() (Config, error) {
	sources := 0
	for _, value := range []string{c.Password, c.PasswordFile, c.PasswordHash, c.PasswordHashFile} {
		if value != "" {
			sources++
		}
	}
	if sources != 1 {
		return c, errors.New("auth requires exactly one password source: hash file, hash environment, raw environment, or legacy raw file")
	}
	if c.StateDir == "" || c.Issuer == "" {
		return c, errors.New("auth requires issuer and state directory")
	}
	file, maxBytes := c.PasswordHashFile, 60
	if c.PasswordFile != "" {
		file, maxBytes = c.PasswordFile, 72
	}
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return c, fmt.Errorf("open auth password source: %w", err)
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, int64(maxBytes+3)))
		if err != nil || len(b) > maxBytes+2 {
			return c, errors.New("invalid auth password source size")
		}
		value := strings.TrimSuffix(strings.TrimSuffix(string(b), "\r\n"), "\n")
		if c.PasswordHashFile != "" {
			c.PasswordHash = value
		} else {
			c.Password = value
		}
	}
	if c.PasswordHashFile != "" || c.PasswordHash != "" {
		if err := validatePasswordHash(c.PasswordHash); err != nil {
			return c, err
		}
	} else if len(c.Password) < 12 || len(c.Password) > 72 {
		return c, errors.New("auth password must contain 12..72 bytes (not characters)")
	}
	u, err := url.Parse(c.Issuer)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(c.Issuer, "#") || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return c, errors.New("auth issuer must be an origin without path, credentials, query or fragment")
	}
	if u.Scheme != "https" && !(c.AllowHTTP && u.Scheme == "http" && loopback(u.Hostname())) {
		return c, errors.New("auth issuer requires HTTPS (HTTP loopback needs explicit testing flag)")
	}
	c.Issuer = strings.TrimSuffix(c.Issuer, "/")
	if c.Resource == "" {
		c.Resource = c.Issuer + "/mcp"
	}
	r, err := url.Parse(c.Resource)
	if err != nil || r.Scheme != u.Scheme || r.Host != u.Host || r.User != nil || r.RawQuery != "" || r.ForceQuery || r.Fragment != "" || strings.Contains(c.Resource, "#") || r.RawPath != "" || r.Opaque != "" || (r.Path != "" && path.Clean(r.Path) != r.Path) || strings.HasPrefix(r.Path, "/oauth") || strings.HasPrefix(r.Path, "/.well-known") {
		return c, errors.New("auth resource must be a canonical URL on the issuer origin, outside reserved OAuth paths")
	}
	return c, nil
}
