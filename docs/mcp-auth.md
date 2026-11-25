# Single-user MCP authentication

`attndb serve` can protect Streamable HTTP with an embedded OAuth authorization
server. This is **one shared user, with all tools**, including write and delete.
Client registrations identify applications, not users or isolated vaults. Each
new authorization requires the shared password and shows the client name and
exact redirect URL before approval. Client names are self-declared, not verified.

The package `internal/mcpauth` is independent of the MCP SDK and uses
`golang.org/x/crypto/bcrypt` for password hashing. Its `Handler`
wraps the existing MCP SDK transport without buffering or replacing the response
writer. Auth does not change tool implementations or MCP session semantics.

## Configuration contract

No auth settings means the existing unauthenticated local-development server,
including the root endpoint `http://localhost:8765`. A warning is logged. Keep
that listener loopback-only; explicitly binding it publicly does not enable auth.
**Any auth setting enables validation**: missing/invalid values, unreadable
secrets, unsafe/corrupt state, a second process, or a changed issuer/resource
cause startup to fail, never fall back to unauthenticated service.

| Serve flag | Environment default | Meaning |
| --- | --- | --- |
| `-auth-issuer` | `ATTNDB_AUTH_ISSUER` | Public HTTPS origin, e.g. `https://attn.knusbaum.com`; no path/query/fragment/userinfo |
| `-auth-resource` | `ATTNDB_AUTH_RESOURCE` | Exact MCP URL; defaults to `<issuer>/mcp`; same origin, canonical path |
| `-auth-password-hash-file` | `ATTNDB_AUTH_PASSWORD_HASH_FILE` | Preferred: file containing a salted bcrypt verifier; one trailing LF or CRLF is removed |
| (none, deliberately) | `ATTNDB_AUTH_PASSWORD_HASH` | Alternative bcrypt verifier source; literal `$` characters must survive environment interpolation |
| (none, deliberately) | `ATTNDB_AUTH_PASSWORD` | Compatibility: raw password environment; converted to bcrypt in memory, never persisted by attndb |
| `-auth-password-file` | `ATTNDB_AUTH_PASSWORD_FILE` | Deprecated compatibility: plaintext file; do not use for a no-plaintext-on-disk deployment |
| `-auth-state-dir` | `ATTNDB_AUTH_STATE_DIR` | Required private persistent directory, **outside the document vault** |
| `-auth-allow-http-localhost` | (none) | Explicit test-only allowance for HTTP `localhost` or literal loopback IP issuers |

Flags override corresponding environment defaults. Exactly **one** of the four
password sources is permitted; mixed sources fail startup even if their contents
would agree. Missing/unreadable/empty/malformed hash files fail closed. Hashes
must be canonical 60-byte bcrypt `$2a$` or `$2b$` values with cost **10–14**.
Generation uses cost **12** with a fresh random salt. The bounded cost prevents a
malformed configuration from requesting impractical work. Login comparisons use
bcrypt's constant-time checksum comparison, after the existing CSRF/rate checks;
expensive hashing does not hold the OAuth state mutex.

Passwords must contain **12–72 bytes**, not characters. Longer input is rejected,
never truncated (bcrypt has a 72-byte limit). This also applies to compatibility
raw sources; previously accepted longer passwords must be replaced deliberately.
Whitespace is significant except for the one terminal line ending in a source
file. Hash files may be mounted read-only; restrict access to the verifier since
it permits offline guessing. Changes take effect on restart, not live reload.

### Create a verifier without writing the password to disk

Run on a trusted terminal with echo/logging capture disabled for secret input:

```sh
attndb auth-hash -out /path/to/new-attndb-password.bcrypt
```

The command prompts twice with terminal echo disabled, checks confirmation, and
creates a **new** file with mode 0600. It refuses existing files and symlinks.
It never accepts a plaintext password in argv, an environment variable, piped
stdin, or a file. No plaintext is printed or persisted; temporary prompt byte
buffers are cleared after use. It cannot prevent OS swap/core dumps or external
terminal recording; this is an application-level storage guarantee.

Without `-out`, only the salted hash goes to stdout and prompts go to stderr.
Prefer `-out` to shell redirection: the command then controls permissions and
refuses overwrite. No command flag accepts the hash itself, either; configure
it via a hash file or environment. In Compose environment literals, `$` requires
escaping; a mounted hash file avoids that pitfall.

**Replacing an existing plaintext secret:** do not hash the old file in place.
Generate a new verifier path using the hidden prompt (enter the existing password
from memory/a password manager, or deliberately choose a new one). Update the
service to `ATTNDB_AUTH_PASSWORD_HASH_FILE` / `-auth-password-hash-file`, remove
all raw-password and alternative hash settings, and mount the verifier read-only
with permissions readable by the service UID. Restart and verify a fresh login.
Then have the secret owner retire the old plaintext file and any retained copies
or backups according to their storage policy. Ordinary deletion does not prove
secure erasure on snapshots/SSDs. This implementation does not read, overwrite,
or delete any existing deployment secret. Existing issued tokens survive changing
the verifier; see the explicit revocation procedure below if revocation is needed.

### Future HTTPS configuration (not a deployment recipe)

```sh
attndb serve -store qdrant -docs /path/to/vault \
  -addr 127.0.0.1:8765 \
  -auth-issuer https://attn.knusbaum.com \
  -auth-password-hash-file /run/secrets/attndb-password.bcrypt \
  -auth-state-dir /var/lib/attndb/oauth
```

Add the existing encoder/model/Qdrant flags appropriate for the installation.
The future client endpoint is `https://attn.knusbaum.com/mcp`. A TLS reverse proxy
(such as hub) must forward `/mcp`, `/oauth/*`, and `/.well-known/*` to this listener
without path rewriting, preserving `Authorization`, MCP headers, and streaming
responses. Avoid response buffering and short stream timeouts. The configured
issuer is authoritative; forwarded host/proto/IP headers are never trusted to
construct URLs or bypass throttles. Keep the backend and the separate optional
`-debug-addr` pprof listener private. Pprof is **not** covered by this middleware.
No proxy configuration or deployment is included in this change.

In authenticated mode only, the MCP SDK's loopback Host and cross-origin checks
are bypassed **inside** the bearer-protected handler. This permits a proxy to
preserve the public Host and OAuth browser clients to call across origins. No
cookie or URL token can authorize MCP, so ordinary cross-site forms cannot pass
that boundary. Unauthenticated local mode retains the SDK defaults. Login-page
CSRF protections are separate and remain enabled.

### Local OAuth testing

Use `attndb auth-hash` to create a test verifier file and a dedicated private state directory, not production
state. Keep existing vault documents intact; tests use temporary fixtures.

```sh
attndb serve -store qdrant -docs /path/to/test-vault \
  -addr localhost:8765 \
  -auth-issuer http://localhost:8765 \
  -auth-allow-http-localhost \
  -auth-password-hash-file /path/to/test-password.bcrypt \
  -auth-state-dir /path/to/private-test-oauth-state
```

Connect to `http://localhost:8765/mcp`. HTTP login cookies are permitted only in
this explicit loopback test mode. HTTPS remains mandatory for public issuers.
A native client's registered HTTP loopback callback is independently permitted,
including when the issuer is HTTPS. Redirect URIs must match registration exactly,
including the port. Custom URI schemes and wildcard callbacks are not supported.

## Protocol and persistence

- Discovery: RFC 9728 protected-resource metadata at
  `/.well-known/oauth-protected-resource` and its resource-path variant; RFC 8414
  authorization-server metadata at `/.well-known/oauth-authorization-server`.
  Unauthenticated MCP requests return a `401` with `WWW-Authenticate` discovery
  information. Metadata and API endpoints support non-credentialed CORS.
- Registration: `POST /oauth/register` accepts public clients with HTTPS or
  loopback HTTP redirect URIs and `token_endpoint_auth_method: none`. Supported
  grants are authorization code and refresh token. No client secrets are issued.
  Each registration is limited to eight redirect URIs, 2048 bytes per URI and
  4096 bytes in total, plus a 128-byte client name. Dynamic registration is the
  supported discovery registration path. Client ID
  Metadata Documents and confidential-client authentication are not implemented
  or advertised; no client-supplied metadata URLs are fetched.
- Authorization: `GET /oauth/authorize` requires `response_type=code`, registered
  `client_id` and `redirect_uri`, S256 PKCE, and the **exact** `resource` URL.
  Optional `scope` is `mcp` (all tools). State is echoed; the issuer is included
  in the redirect. The form displays consent, uses an expiring server-side
  transaction and an HttpOnly/SameSite cookie plus a body CSRF value. Login POST
  accepts password/CSRF only in its body, rejects all query parameters and foreign
  Origins, and verifies the submitted password against the salted bcrypt hash. Password errors
  consume the transaction: restart authorization to retry. Multiple simultaneous
  login tabs share one cookie, so complete one flow at a time.
- Token exchange: `POST /oauth/token` requires URL-encoded body parameters, a
  public `client_id`, and exact `resource` on **both** code and refresh exchanges.
  Code exchange also requires the original redirect URI and PKCE verifier.
  Duplicate parameters are rejected. Tokens are opaque random 256-bit secrets;
  they are accepted only from the bearer Authorization header, not URLs/cookies.
  All auth responses are `no-store`; login pages forbid framing and cross-origin
  referrers. Login HTML uses `Referrer-Policy: same-origin` so native form POSTs
  preserve their Origin; POST responses (including the callback 303) use
  `no-referrer`.
- Expiry: browser transactions last 5 minutes, single-use codes 1 minute, access
  tokens at most 1 hour, refresh families an absolute 30 days (rotation does not
  extend this), and client registrations 90 days since registration or last token
  issuance. Expired data is removed lazily on OAuth activity and on startup.
- Every refresh rotates the refresh token. Used-token hashes are retained until
  the family's original expiry. Reusing one revokes that family's remaining
  access and refresh tokens, including across restarts. Clients must serialize
  refreshes; a lost response followed by retry is treated as replay and requires
  login again. An already-open authenticated stream is not forcibly interrupted
  by expiry/revocation; subsequent HTTP requests are checked again.

`state.json` contains client metadata and SHA-256 hashes of access/refresh tokens,
never raw bearer tokens or passwords. Pending logins and authorization codes are
intentionally transient; restart those flows after a daemon restart. Issued tokens
and registrations survive normal restarts. State writes use a private temporary
file, fsync, atomic rename and directory fsync before returning new credentials.
A failed write makes the running auth server fail closed until restart/repair.

The state directory must be a real `0700` directory, with regular `0600` state
and lock files. It is created if absent. Use a persistent **local filesystem**
volume; Unix advisory locking permits one live process per directory. This is
not a clustered/NFS auth store. Do not share/clone the directory between live
servers. Back it up privately: a backup reveals client metadata, and restoring
old snapshots can roll back revocations/refresh rotation. Corrupt state is never
automatically discarded. Changing the issuer/resource requires a separate state
directory and fresh client authorization.

Password rotation alone does **not** revoke existing sessions. To revoke all
registrations/tokens, stop the daemon, move its state directory aside securely,
and restart with a fresh private state directory (and a new password if needed).
All clients must register/login again. There is no remote admin/revocation API.

## Diagnosing browser authorization denials

The public response deliberately remains `{"error":"access_denied"}`. Server
stderr logs only fixed reason codes, for example:

```text
mcpauth authorization_rejected reason=csrf_cookie_mismatch
```

No password, verifier, cookie/CSRF value, client ID, token, raw Origin, URL,
query, or form is included. Do not enable request-body logging to troubleshoot
this flow.

| Reason | Next check |
| --- | --- |
| `origin_mismatch` | Use the updated image and open a fresh authorization page at the configured public issuer. The login HTML must return `Referrer-Policy: same-origin`, not `no-referrer`; ensure a proxy does not override it. Never allow `Origin: null` to bypass the check. |
| `login_cookie_missing` | Check HTTPS and whether the browser accepted the Secure host-only login cookie; use the same public hostname throughout. |
| `csrf_invalid_length` | Restart authorization; the submitted form field is missing or malformed. |
| `csrf_cookie_mismatch` | Close competing login tabs and restart; each login page replaces the single login cookie. |
| `transaction_missing_or_expired` | Start a fresh authorization flow. Expiry, repeated submission, restart, or routing to another process can cause this. |
| `transaction_expired_during_verification` | Start a fresh flow; the five-minute transaction expired while verification ran. |
| `password_verification_mismatch` | Check that the deployed image has the current 12–72-byte policy and the intended verifier mount; an earlier failed attempt requires a fresh form before retrying. |

The earlier login HTML used `Referrer-Policy: no-referrer`. The
[Fetch Origin-header algorithm](https://fetch.spec.whatwg.org/#append-a-request-origin-header)
sets Origin to `null` for non-CORS POSTs under this policy, including HTML form
navigation even when same-origin. See also
[MDN's explanation](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Referrer-Policy#effect_on_the_origin_header).
The corrected HTML policy is `same-origin`; POST/303 responses keep
`no-referrer` to suppress callback referrers. Exact Origin validation, CSRF
cookie/token binding, and all other checks remain intact. A cached/already-open
old login page must be replaced by a fresh flow. If the mismatch persists after
updating, inspect **only the POST Origin request-header value** in DevTools and
the login response's Referrer-Policy; do not copy the URL, form body, cookies,
Authorization header, or a HAR capture.

The normal OAuth cleanup removes expired pending entries before validation, so
the server cannot distinguish an expired transaction from a missing/replayed one
without retaining extra state. No extra sensitive state is retained for logging.
Other authorization rejections log fixed validation/capacity/storage codes while
keeping their original public status/error.

These diagnostics require rebuilding the application image and recreating the
attndb container; rebuilding the separate temporary `auth-hash` helper or merely
restarting an old image does not update the running server.

## Abuse limits and scope

Fixed per-process global one-minute limits allow 10 registration requests,
10 password submissions, 120 login-page requests, and 120 token exchanges.
These deliberately do not use spoofable forwarded IP addresses; behind a proxy
all clients share the budget. `429` includes `Retry-After`. State is capped at
1024 registrations, 1024 pending logins, 1024 codes, and 8192 combined access,
refresh, and replay-tombstone records. Capacity failures reject new issuance
rather than evict live credentials. Deployment-edge rate limiting can further
reduce unauthenticated registration/login denial of service. Requests have bounded
bodies/queries and the daemon has HTTP header/read deadlines, but no write timeout
that would terminate SSE streams. In-memory rate counters reset at restart.

This is a compact single-user auth layer, not an identity provider: no MFA,
per-user authorization, per-tool scopes, client trust verification, federation,
CIMD fetching, or management UI. The shared password is the approval authority.

Primary protocol references: [MCP 2025-11-25 authorization](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization),
[RFC 7636 PKCE](https://www.rfc-editor.org/rfc/rfc7636),
[RFC 8707 resource indicators](https://www.rfc-editor.org/rfc/rfc8707), and
[RFC 9700 refresh-token replay protection](https://www.rfc-editor.org/rfc/rfc9700#section-4.14).
