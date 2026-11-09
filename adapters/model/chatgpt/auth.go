package chatgpt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultIssuer = "https://auth.openai.com"
	resource      = "https://api.openai.com/v1"
	scope         = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	planScope     = "chatgpt.tokens.use.direct"
	vaultName     = "chatgpt"
)

var ErrSignInRequired = errors.New("chatgpt sign-in required, run `haythosd auth chatgpt`")

// Store is the vault; *file.Vault satisfies it.
type Store interface {
	Resolve(handle string) (string, error)
	Set(name, value string) error
}

type AuthConfig struct {
	Issuer string
	HTTP   *http.Client
	Now    func() time.Time
}

// Session owns the ChatGPT credentials in the vault. One Session per process: mu serializes refreshes of the rotating refresh token.
type Session struct {
	store  Store
	issuer string
	http   *http.Client
	now    func() time.Time
	mu     sync.Mutex
}

type record struct {
	HostID       string    `json:"host_id"`
	ClientID     string    `json:"client_id,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	Email        string    `json:"email,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes,omitempty"`
}

type tokenResponse struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	ID      string `json:"id_token"`
	Type    string `json:"token_type"`
	Scope   string `json:"scope"`
	Expires int64  `json:"expires_in"`
}

func NewSession(store Store, cfg AuthConfig) *Session {
	if cfg.Issuer == "" {
		cfg.Issuer = defaultIssuer
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Session{store: store, issuer: strings.TrimRight(cfg.Issuer, "/"), http: cfg.HTTP, now: cfg.Now}
}

func (s *Session) load() (record, bool) {
	raw, err := s.store.Resolve("secret://" + vaultName)
	if err != nil {
		return record{}, false
	}
	var r record
	if json.Unmarshal([]byte(raw), &r) != nil {
		return record{}, false
	}
	return r, true
}

func (s *Session) save(r record) error {
	b, err := json.Marshal(r) //nolint:gosec // G117: the record goes only into the encrypted vault
	if err != nil {
		return fmt.Errorf("encode chatgpt credentials: %w", err)
	}
	if err := s.store.Set(vaultName, string(b)); err != nil {
		return fmt.Errorf("store chatgpt credentials: %w", err)
	}
	return nil
}

// AccessToken returns a valid access token, refreshing it first when less than a minute is left.
func (s *Session) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.load()
	if !ok || r.ClientID == "" || r.AccessToken == "" || r.RefreshToken == "" {
		return "", ErrSignInRequired
	}
	if s.now().Before(r.ExpiresAt.Add(-time.Minute)) {
		return r.AccessToken, nil
	}
	t, err := s.token(ctx, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {r.ClientID}, "refresh_token": {r.RefreshToken}, "resource": {resource},
	})
	if err != nil {
		return "", fmt.Errorf("refresh chatgpt token: %w", err)
	}
	if r, err = s.apply(ctx, r, t, ""); err != nil {
		return "", err
	}
	// The old refresh token is spent; the rotated one must be on disk before anything uses the new access token.
	if err := s.save(r); err != nil {
		return "", err
	}
	return r.AccessToken, nil
}

var (
	codeRE     = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)
	clientIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)
)

type callback struct {
	code, clientID string
	err            error
}

// Login runs the browser sign-in: dynamic registration on first use, reauthorization of the stored client after that.
// open receives a loopback URL that redirects to OpenAI, so the authorize URL (which may carry id_token_hint) is never printed.
func (s *Session) Login(ctx context.Context, open func(url string) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, _ := s.load()
	if r.HostID == "" {
		id, err := random(16)
		if err != nil {
			return "", err
		}
		id[6], id[8] = (id[6]&0x0f)|0x40, (id[8]&0x3f)|0x80
		r = record{HostID: fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])}
		if err := s.save(r); err != nil {
			return "", err
		}
	}
	var secrets [4]string
	for i := range secrets {
		b, err := random(32)
		if err != nil {
			return "", err
		}
		secrets[i] = base64.RawURLEncoding.EncodeToString(b)
	}
	state, nonce, verifier, entry := secrets[0], secrets[1], secrets[2], secrets[3]

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("start loopback callback: %w", err)
	}
	addr := ln.Addr().String()
	redirect := "http://" + addr + "/auth/callback"
	reauth := r.ClientID != ""
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id": {r.ClientID}, "ext_agent_host_id": {r.HostID}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope": {scope}, "resource": {resource}, "state": {state}, "nonce": {nonce},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
	}
	if reauth {
		if r.IDToken != "" {
			q.Set("id_token_hint", r.IDToken)
		}
		if r.Email != "" {
			q.Set("login_hint", r.Email)
		}
	} else {
		q.Set("client_id", "dynamic_agent_client")
		q.Set("agent_name_hint", "HaythOS")
	}

	done := make(chan callback, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/{entry}", func(w http.ResponseWriter, req *http.Request) {
		if req.Host != addr || subtle.ConstantTimeCompare([]byte(req.PathValue("entry")), []byte(entry)) != 1 {
			http.NotFound(w, req)
			return
		}
		http.Redirect(w, req, s.issuer+"/api/accounts/authorize?"+q.Encode(), http.StatusFound)
	})
	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if req.Host != addr {
			http.NotFound(w, req)
			return
		}
		sent := false
		once.Do(func() { sent = true; done <- parseCallback(req.URL.Query(), state, r.ClientID) })
		if !sent {
			http.Error(w, "sign-in attempt already used", http.StatusConflict)
			return
		}
		_, _ = io.WriteString(w, "HaythOS: you can close this tab and return to the terminal.")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	if err := open("http://" + addr + "/login/" + entry); err != nil {
		return "", fmt.Errorf("open sign-in page: %w", err)
	}
	var cb callback
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case cb = <-done:
	}
	if cb.err != nil {
		return "", cb.err
	}
	next := r
	next.ClientID = cb.clientID
	t, err := s.token(ctx, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {cb.clientID}, "code": {cb.code},
		"code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {resource},
	})
	if err != nil {
		return "", fmt.Errorf("exchange sign-in code: %w", err)
	}
	if next, err = s.apply(ctx, next, t, nonce); err != nil {
		return "", err
	}
	if err := s.save(next); err != nil {
		return "", err
	}
	return next.Email, nil
}

// parseCallback validates the loopback callback; stored is the issued client id on reauthorization, "" on first registration.
func parseCallback(p url.Values, state, stored string) callback {
	if len(p["state"]) != 1 || subtle.ConstantTimeCompare([]byte(p.Get("state")), []byte(state)) != 1 {
		return callback{err: errors.New("sign-in callback state did not match; run the login again")}
	}
	if p.Has("error") {
		return callback{err: errors.New("sign-in or ChatGPT plan access was declined")}
	}
	id := p.Get("client_id")
	if stored != "" && id == "" {
		id = stored
	}
	if len(p["client_id"]) > 1 || !clientIDRE.MatchString(id) || id == "dynamic_agent_client" || (stored != "" && id != stored) ||
		len(p["code"]) != 1 || p.Get("code") == "" {
		return callback{err: errors.New("sign-in callback registration is missing or mismatched")}
	}
	return callback{code: p.Get("code"), clientID: id}
}

func random(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("random: %w", err)
	}
	return b, nil
}

var terminalRefresh = []string{"invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused"}

// token posts to the token endpoint. The response body is never echoed into errors; only a sanitized error code is.
func (s *Session) token(ctx context.Context, form url.Values) (tokenResponse, error) {
	var t tokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer+"/api/accounts/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return t, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return t, ctx.Err()
		}
		return t, fmt.Errorf("token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return t, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		code := e.Error
		if !codeRE.MatchString(code) {
			code = "unknown"
		}
		if slices.Contains(terminalRefresh, code) || resp.StatusCode == http.StatusUnauthorized {
			return t, fmt.Errorf("%w (%s)", ErrSignInRequired, code)
		}
		return t, fmt.Errorf("token endpoint HTTP %d (%s)", resp.StatusCode, code)
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, errors.New("token endpoint returned invalid JSON")
	}
	return t, nil
}

// apply validates a token response and merges it into r. nonce is set only for an interactive login.
func (s *Session) apply(ctx context.Context, r record, t tokenResponse, nonce string) (record, error) {
	if t.Access == "" || !strings.EqualFold(t.Type, "Bearer") || t.Expires <= 0 {
		return r, errors.New("token endpoint returned an unusable access token")
	}
	r.AccessToken = t.Access
	r.ExpiresAt = s.now().Add(time.Duration(t.Expires) * time.Second)
	if t.Refresh != "" {
		r.RefreshToken = t.Refresh
	}
	if r.RefreshToken == "" {
		return r, errors.New("token endpoint returned no refresh token")
	}
	if t.Scope != "" {
		r.Scopes = strings.Fields(t.Scope)
	}
	if !slices.Contains(r.Scopes, planScope) {
		return r, fmt.Errorf("%w: ChatGPT plan usage was not granted", ErrSignInRequired)
	}
	switch {
	case t.ID != "":
		c, err := s.verifyIDToken(ctx, t.ID, r.ClientID, nonce)
		if err != nil {
			return r, err
		}
		if r.Subject != "" && c.Sub != r.Subject {
			return r, errors.New("signed in as a different ChatGPT account than the stored one")
		}
		r.Subject, r.Email, r.IDToken = c.Sub, c.Email, t.ID
	case nonce != "":
		return r, errors.New("token endpoint returned no ID token")
	}
	return r, nil
}

type idClaims struct {
	Iss   string          `json:"iss"`
	Sub   string          `json:"sub"`
	Aud   json.RawMessage `json:"aud"`
	Azp   string          `json:"azp"`
	Exp   int64           `json:"exp"`
	Nonce string          `json:"nonce"`
	Email string          `json:"email"`
}

var errBadIDToken = errors.New("chatgpt ID token could not be verified")

// verifyIDToken checks an RS256/ES256 JWT against the issuer's JWKS, then iss, aud, azp, exp, nonce and sub.
func (s *Session) verifyIDToken(ctx context.Context, raw, clientID, nonce string) (idClaims, error) {
	var c idClaims
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return c, errBadIDToken
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if decodeSegment(parts[0], &h) != nil || (h.Alg != "RS256" && h.Alg != "ES256") {
		return c, errBadIDToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return c, errBadIDToken
	}
	key, err := s.signingKey(ctx, h.Kid, h.Alg)
	if err != nil {
		return c, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch k := key.(type) {
	case *rsa.PublicKey:
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) != nil {
			return c, errBadIDToken
		}
	case *ecdsa.PublicKey:
		if len(sig) != 64 || !ecdsa.Verify(k, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			return c, errBadIDToken
		}
	}
	if decodeSegment(parts[1], &c) != nil {
		return c, errBadIDToken
	}
	var aud []string
	if json.Unmarshal(c.Aud, &aud) != nil {
		var one string
		if json.Unmarshal(c.Aud, &one) != nil {
			return c, errBadIDToken
		}
		aud = []string{one}
	}
	if c.Iss != s.issuer || c.Sub == "" || !slices.Contains(aud, clientID) || (c.Azp != "" && c.Azp != clientID) ||
		s.now().Unix() >= c.Exp || (nonce != "" && c.Nonce != nonce) {
		return c, errBadIDToken
	}
	return c, nil
}

func decodeSegment(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (s *Session) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("get %s: HTTP %d", endpoint, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", endpoint, err)
	}
	return nil
}

func (s *Session) signingKey(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	var d struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := s.getJSON(ctx, s.issuer+"/.well-known/openid-configuration", &d); err != nil {
		return nil, err
	}
	iss, _ := url.Parse(s.issuer)
	u, err := url.Parse(d.JWKS)
	if err != nil || u.Scheme != iss.Scheme || u.Host != iss.Host {
		return nil, errors.New("chatgpt JWKS endpoint is not on the issuer host")
	}
	var set struct {
		Keys []struct {
			Kty, Kid, Alg, N, E, Crv, X, Y string
		} `json:"keys"`
	}
	if err := s.getJSON(ctx, d.JWKS, &set); err != nil {
		return nil, err
	}
	for _, k := range set.Keys {
		if k.Kid != kid || (k.Alg != "" && k.Alg != alg) {
			continue
		}
		switch {
		case alg == "RS256" && k.Kty == "RSA":
			n, err1 := base64.RawURLEncoding.DecodeString(k.N)
			e, err2 := base64.RawURLEncoding.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(e) > 4 {
				return nil, errBadIDToken
			}
			return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
		case alg == "ES256" && k.Kty == "EC" && k.Crv == "P-256":
			x, err1 := base64.RawURLEncoding.DecodeString(k.X)
			y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
			if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
				return nil, errBadIDToken
			}
			pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), slices.Concat([]byte{4}, x, y))
			if err != nil {
				return nil, errBadIDToken
			}
			return pub, nil
		}
	}
	return nil, errBadIDToken
}
