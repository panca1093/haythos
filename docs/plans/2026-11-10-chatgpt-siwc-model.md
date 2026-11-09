# ChatGPT Subscription Model (Sign in with ChatGPT) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A new model kind `chatgpt` runs one Alfred `Step` on the user's ChatGPT plan through OpenAI's official "Sign in with ChatGPT" token sharing for open-source / locally hosted apps.

**Architecture:** One adapter package `adapters/model/chatgpt` with two files. `auth.go` owns the OAuth session: browser login (dynamic registration + PKCE + loopback), ID-token verification against the published JWKS, and serialized refresh of the rotating refresh token. Every credential lives in one vault entry (`secret://chatgpt`, JSON). `chatgpt.go` implements `app.Model.Step` on the public `POST /v1/responses` (`store:false`, `stream:true`, SSE) with tools grouped in one namespace. `cmd/haythosd` gets `case "chatgpt"` and `haythosd auth chatgpt`.

**Tech Stack:** Go stdlib only (`net/http`, `crypto/rsa`, `crypto/ecdsa`, `crypto/sha256`, `bufio`, `encoding/json`). No new dependency: ID-token verification is RS256/ES256 over a JWKS, about 60 lines of stdlib, which is smaller than pulling `go-oidc` + `go-jose` + `x/oauth2` into a repo that is going public.

**Spec:** https://developers.openai.com/siwc/token-sharing-open-source (+ `/sign-in`, `/profiles-and-sessions`, `/models-and-inference`, `/preview-limitations`, `/self-hosted-vms`, `/token-reference`, `/errors-and-recovery`). Reference implementation (read-only, ported not imported): `~/Documents/MyBook/Project/jaze` `internal/chatgpt/`, `internal/ai/subscription.go`, `internal/ai/stream.go`. `CLAUDE.md` rules 1, 3, 5.

## Global Constraints

- Rule 3: access, refresh and ID tokens never appear in model context, logs, events, error strings or the repo. Only the vault holds them. Every error built from an HTTP response has the current access token scrubbed, and auth-endpoint bodies are never echoed.
- Rule 5: our own registration only. Authorize with `client_id=dynamic_agent_client` + `agent_name_hint=HaythOS` + a stable `ext_agent_host_id` (`urn:uuid:` v4). Never call `chatgpt.com/backend-api`, never reuse Codex/Claude client ids, never set originator or spoofed user-agent headers.
- Endpoints (fixed, overridable only in tests): authorize `https://auth.openai.com/api/accounts/authorize`, token `https://auth.openai.com/api/accounts/oauth/token`, JWKS from `/.well-known/openid-configuration` (`jwks_uri` must be on the auth host), inference `https://api.openai.com/v1/responses`, models `https://api.openai.com/v1/models`.
- Scopes `openid profile email offline_access resource.invoke chatgpt.tokens.use.direct`; `resource=https://api.openai.com/v1` on authorize, code exchange and refresh. Redirect `http://127.0.0.1:<port>/auth/callback`, never `localhost`.
- Refresh: access token lives 1 h, refresh token 30 d and rotates on every refresh. Refresh when < 1 min is left, under one `sync.Mutex` per session (single process: `haythosd` already holds an exclusive flock on the DB). The rotated record is written to the vault before the new access token is returned.
- Responses request: `model`, `instructions` (all system messages), `input` (full history, no `system` role items), `tools` (one `namespace` named `alfred`), `store:false`, `stream:true`. Never send `temperature`, `max_output_tokens`, `previous_response_id`, `metadata`, `user`, `truncation`, etc. (/preview-limitations).
- Usage errors (`subscription_sharing_usage_limit_exceeded`, `..._usage_unavailable`, and the other `subscription_sharing_*` / `chatpass_v2_*` codes, as HTTP error or `response.failed`) return an error. No retry, no API-key fallback.
- Do not touch `adapters/model/claudesdk/`, `sidecar/`, `tests/acceptance/`, `Makefile`. Edits in `cmd/haythosd/main.go` and `adapters/config/config.go` stay small and local.
- Gate: `gofmt -l .` empty, `go vet ./...`, `golangci-lint run`, `go test -race ./...`.

## Review Focus

- Rotated refresh token persisted before the new access token is used; a failed vault write returns an error and the old token is not used either.
- Two concurrent `Step` calls on an expiring token cause exactly one refresh request.
- Callback: `state` compared in constant time; `error=` stops before exchange; new registration without `client_id` (or `client_id=dynamic_agent_client`) is rejected; on reauth a different `client_id` is rejected.
- ID token: signature (RS256/ES256 via JWKS), `iss`, `aud` contains the issued client id, `exp`, `nonce`, `sub`; reauth `sub` must match the stored one. Granted scope must contain `chatgpt.tokens.use.direct`.
- Tool-call parse: wire names `vmhealth__read` (namespace `alfred`) map back to `vmhealth.read`; a call in a foreign namespace is an error.
- `response.failed` with a usage code → error, one HTTP request only. Stream ending without `response.completed` → error.
- `ctx` cancel → `Step` returns promptly with an error that `errors.Is(ctx.Err())`.
- No returned error contains the access or refresh token (test with a fake server that echoes the Authorization header).

---

### Task 1: Session: vault record, ID-token verification, serialized refresh

**Files:**
- Create: `adapters/model/chatgpt/auth.go`
- Test: `adapters/model/chatgpt/auth_test.go`

**Interfaces:**
- Produces: `type Store interface { Resolve(handle string) (string, error); Set(name, value string) error }` (satisfied by `*file.Vault`), `type AuthConfig struct { Issuer string; HTTP *http.Client; Now func() time.Time }`, `func NewSession(store Store, cfg AuthConfig) *Session`, `func (s *Session) AccessToken(ctx) (string, error)`, `var ErrSignInRequired`.
- Vault entry `chatgpt` = JSON `{host_id, client_id, subject, email, id_token, access_token, refresh_token, expires_at, scopes}`.

- [ ] **Step 1: Failing tests** (httptest fake auth server: discovery, JWKS from a test RSA key, token endpoint; real `file.Vault` in `t.TempDir()`):
  - `TestAccessTokenFresh`: unexpired record → returned, no HTTP call.
  - `TestAccessTokenRefreshRotates`: expiring record → one refresh POST with `grant_type=refresh_token`, issued `client_id`, old refresh token, `resource`; the reopened vault holds the new access + rotated refresh token and new expiry.
  - `TestAccessTokenRefreshPersistFails`: store `Set` fails → error, new token not returned.
  - `TestAccessTokenConcurrentRefreshOnce`: 8 goroutines → token endpoint hit once.
  - `TestAccessTokenRefreshInvalidGrant`: `{"error":"invalid_grant"}` → `errors.Is(err, ErrSignInRequired)`; error text contains neither token.
  - `TestAccessTokenMissing`: empty vault → `ErrSignInRequired`.
  - `TestVerifyIDToken` table: valid; bad signature; wrong `iss`; wrong `aud`; expired; wrong nonce; `alg:none`.
- [ ] **Step 2:** `go test ./adapters/model/chatgpt/` → FAIL (undefined).
- [ ] **Step 3: Implement** in `auth.go`.
- [ ] **Step 4:** PASS with `-race`.
- [ ] **Step 5: Commit** `chatgpt: session with serialized refresh of the rotating token`

### Task 2: Browser login (dynamic registration, PKCE, loopback)

**Files:**
- Modify: `adapters/model/chatgpt/auth.go`
- Test: `adapters/model/chatgpt/auth_test.go`

**Interfaces:**
- Produces: `func (s *Session) Login(ctx context.Context, open func(url string) error) (email string, err error)`.

- [ ] **Step 1: Failing tests** (`open` is a fake browser that follows the local login URL to the fake authorize endpoint, which redirects to the callback):
  - `TestLoginNewRegistration`: authorize query has `client_id=dynamic_agent_client`, `agent_name_hint=HaythOS`, `ext_agent_host_id=urn:uuid:…`, exact scope/resource, `S256` challenge that matches the verifier sent to the token endpoint, redirect `http://127.0.0.1:<port>/auth/callback`; callback returns `client_id=oaiapp_test`; the vault record holds client id, host id, subject, tokens.
  - `TestLoginReauth`: existing record → `client_id=oaiapp_test`, `id_token_hint`, `login_hint`, no `agent_name_hint`, same host id; a different `sub` is rejected and the old record stays.
  - `TestLoginRejects` table: state mismatch; `error=access_denied`; missing `client_id` on new registration; missing `chatgpt.tokens.use.direct` scope → error, vault unchanged except host id.
  - `TestLoginHostIDStable`: two logins reuse the stored host id.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement**: listener `tcp4 127.0.0.1:0` started before `open`; the printed URL is `http://127.0.0.1:<port>/login/<random>` so the authorize URL (with `id_token_hint`) never hits a terminal or log; one-shot callback; code exchange `grant_type=authorization_code`; verify; persist.
- [ ] **Step 4:** PASS.
- [ ] **Step 5: Commit** `chatgpt: browser sign-in with dynamic registration and PKCE`

### Task 3: `Step` on the Responses API

**Files:**
- Create: `adapters/model/chatgpt/chatgpt.go`
- Test: `adapters/model/chatgpt/chatgpt_test.go`

**Interfaces:**
- Produces: `type Config struct { Model, BaseURL string; HTTP *http.Client; Session *Session }`, `func New(c Config) *Adapter`, `func (a *Adapter) Step(ctx, history, tools) (task.AssistantTurn, error)` (satisfies `app.Model`), `func (a *Adapter) Models(ctx) ([]ModelInfo, error)`, `var ErrUsageLimit`.

- [ ] **Step 1: Failing tests** (fake Responses server emitting SSE):
  - `TestStepRequestShape`: system → `instructions`; user/assistant/function_call (`namespace:"alfred"`, `name:"vmhealth__read"`)/function_call_output (`error: ` prefix) items; `tools` = one namespace with the function; `store:false`, `stream:true`; none of the forbidden fields; `Authorization: Bearer <access>`.
  - `TestStepParsesTextAndToolCalls`: `output_item.done` (message + function_call) + `response.completed` with usage → `Text`, `Calls[0].Name=="vmhealth.read"`, `Usage`.
  - `TestStepForeignNamespace`: function_call with `namespace:"other"` → error.
  - `TestStepUsageLimit`: `response.failed` with `subscription_sharing_usage_limit_exceeded` → `errors.Is(err, ErrUsageLimit)`, exactly one request; HTTP 429 body with the same code → same.
  - `TestStepNoCompleted`: stream ends early → error.
  - `TestStepCancel`: server blocks; cancel → returns within 2 s, `errors.Is(err, context.Canceled)`.
  - `TestStepErrorHasNoToken`: server echoes the Authorization header in a 400 body → error lacks the token.
  - `TestStepDefaultModel`: empty `Model` → first `visibility:"list"` slug from `GET /models` is used.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** PASS.
- [ ] **Step 5: Commit** `chatgpt: run a step on the Responses API with plan tokens`

### Task 4: Config kind, wiring, `haythosd auth chatgpt`, docs

**Files:**
- Modify: `adapters/config/config.go` (kind list), `adapters/config/config_test.go`
- Modify: `cmd/haythosd/main.go` (`case "chatgpt"` in `buildModels`, `auth chatgpt` branch), `cmd/haythosd/main_test.go`
- Modify: `README.md` (setup section + layout row), `config/haythos.yaml` (commented example only)

- [ ] **Step 1: Failing tests**: config with `kind: chatgpt` loads; `buildModels` with a `chatgpt` default and no vault entry skips the model with a warning (like a missing credential), and with an entry returns a model.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement**: `haythosd auth chatgpt` prints the local login URL, opens the browser best-effort (`open` / `xdg-open`), waits up to 10 min, prints "Signed in as <email>; stored as secret://chatgpt". README: setup, headless VPS = log in locally and copy the vault file + key (/self-hosted-vms), no device flow.
- [ ] **Step 4:** gate green.
- [ ] **Step 5: Commit** `chatgpt: wire kind chatgpt and haythosd auth chatgpt`

### Task 5: Live check (with the user)

- [ ] `haythosd auth chatgpt` in a real browser; `run-job vm-health` on a job/default pointing at a `kind: chatgpt` model returns a report. Confirm the live stream really tags function calls with `namespace` (see uncertain items).
