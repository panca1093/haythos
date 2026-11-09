package chatgpt

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/vault/file"
)

var testNow = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

type fakeAuth struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu        sync.Mutex
	forms     []url.Values
	issued    int
	tokenErr  string
	sub       string
	scope     string
	authorize func(w http.ResponseWriter, r *http.Request)
	nonce     string
}

func newFakeAuth(t *testing.T) *fakeAuth {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAuth{t: t, key: key, sub: "user-1", scope: scope}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.forms = append(f.forms, r.PostForm)
		if f.tokenErr != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":%q,"echo":%q}`, f.tokenErr, r.PostForm.Get("refresh_token"))
			return
		}
		f.issued++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("at-%d", f.issued),
			"refresh_token": fmt.Sprintf("rt-%d", f.issued),
			"id_token":      f.idToken(r.PostForm.Get("client_id"), f.nonce),
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         f.scope,
		})
	})
	mux.HandleFunc("/api/accounts/authorize", func(w http.ResponseWriter, r *http.Request) { f.authorize(w, r) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAuth) claims(aud, nonce string) map[string]any {
	c := map[string]any{"iss": f.srv.URL, "sub": f.sub, "aud": aud, "exp": testNow.Add(time.Hour).Unix(), "iat": testNow.Unix(), "email": "me@example.test"}
	if nonce != "" {
		c["nonce"] = nonce
	}
	return c
}

func (f *fakeAuth) idToken(aud, nonce string) string {
	return signRS256(f.t, f.key, "k1", f.claims(aud, nonce))
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *fakeAuth) tokenCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forms)
}

func openVault(t *testing.T, path string) *file.Vault {
	t.Helper()
	v, err := file.Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func newTestSession(f *fakeAuth, store Store) *Session {
	return NewSession(store, AuthConfig{Issuer: f.srv.URL, HTTP: f.srv.Client(), Now: func() time.Time { return testNow }})
}

func seed(t *testing.T, store Store, expiresIn time.Duration) {
	t.Helper()
	b, _ := json.Marshal(record{
		HostID: "urn:uuid:00000000-0000-4000-8000-000000000000", ClientID: "oaiapp_test", Subject: "user-1",
		AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: testNow.Add(expiresIn), Scopes: strings.Fields(scope),
	})
	if err := store.Set(vaultName, string(b)); err != nil {
		t.Fatal(err)
	}
}

func loadRecord(t *testing.T, store Store) record {
	t.Helper()
	raw, err := store.Resolve("secret://" + vaultName)
	if err != nil {
		t.Fatal(err)
	}
	var r record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAccessTokenFresh(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	v := openVault(t, t.TempDir()+"/v.enc")
	seed(t, v, 30*time.Minute)
	tok, err := newTestSession(f, v).AccessToken(context.Background())
	if err != nil || tok != "at-old" || f.tokenCalls() != 0 {
		t.Fatalf("tok=%q err=%v calls=%d", tok, err, f.tokenCalls())
	}
}

func TestAccessTokenRefreshRotates(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	path := t.TempDir() + "/v.enc"
	v := openVault(t, path)
	seed(t, v, 30*time.Second)
	tok, err := newTestSession(f, v).AccessToken(context.Background())
	if err != nil || tok != "at-1" {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	form := f.forms[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("client_id") != "oaiapp_test" || form.Get("refresh_token") != "rt-old" || form.Get("resource") != resource || form.Has("scope") {
		t.Errorf("refresh form = %v", form)
	}
	r := loadRecord(t, openVault(t, path))
	if r.AccessToken != "at-1" || r.RefreshToken != "rt-1" || !r.ExpiresAt.Equal(testNow.Add(time.Hour)) || r.Subject != "user-1" {
		t.Errorf("persisted = %+v", r)
	}
}

type failingSet struct{ Store }

func (failingSet) Set(string, string) error { return errors.New("disk full") }

func TestAccessTokenRefreshPersistFails(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	v := openVault(t, t.TempDir()+"/v.enc")
	seed(t, v, 0)
	tok, err := newTestSession(f, failingSet{v}).AccessToken(context.Background())
	if err == nil || tok != "" {
		t.Fatalf("tok=%q err=%v, want error and no token", tok, err)
	}
}

func TestAccessTokenConcurrentRefreshOnce(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	v := openVault(t, t.TempDir()+"/v.enc")
	seed(t, v, 0)
	s := newTestSession(f, v)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if tok, err := s.AccessToken(context.Background()); err != nil || tok != "at-1" {
				t.Errorf("tok=%q err=%v", tok, err)
			}
		})
	}
	wg.Wait()
	if n := f.tokenCalls(); n != 1 {
		t.Errorf("token endpoint calls = %d, want 1", n)
	}
}

func TestAccessTokenRefreshInvalidGrant(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	f.tokenErr = "invalid_grant"
	v := openVault(t, t.TempDir()+"/v.enc")
	seed(t, v, 0)
	_, err := newTestSession(f, v).AccessToken(context.Background())
	if !errors.Is(err, ErrSignInRequired) {
		t.Fatalf("err = %v, want ErrSignInRequired", err)
	}
	if strings.Contains(err.Error(), "rt-old") || strings.Contains(err.Error(), "at-old") {
		t.Errorf("error leaks a token: %v", err)
	}
}

func TestAccessTokenMissing(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	_, err := newTestSession(f, openVault(t, t.TempDir()+"/v.enc")).AccessToken(context.Background())
	if !errors.Is(err, ErrSignInRequired) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyIDToken(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	s := newTestSession(f, nil)
	mod := func(k string, v any) string {
		c := f.claims("oaiapp_test", "n1")
		c[k] = v
		return signRS256(t, f.key, "k1", c)
	}
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + "."
	cases := []struct {
		name string
		tok  string
		ok   bool
	}{
		{"valid", f.idToken("oaiapp_test", "n1"), true},
		{"aud list", mod("aud", []string{"x", "oaiapp_test"}), true},
		{"bad signature", signRS256(t, other, "k1", f.claims("oaiapp_test", "n1")), false},
		{"wrong iss", mod("iss", "https://evil.example"), false},
		{"wrong aud", mod("aud", "oaiapp_other"), false},
		{"expired", mod("exp", testNow.Add(-time.Minute).Unix()), false},
		{"wrong nonce", mod("nonce", "n2"), false},
		{"alg none", none, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.verifyIDToken(context.Background(), c.tok, "oaiapp_test", "n1")
			if (err == nil) != c.ok {
				t.Errorf("err = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

// onAuthorize makes the fake authorize endpoint redirect to redirect_uri with params; "STATE" is replaced by the real state.
func (f *fakeAuth) onAuthorize(params url.Values, seen *url.Values) {
	f.authorize = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		*seen = q
		f.mu.Lock()
		f.nonce = q.Get("nonce")
		f.mu.Unlock()
		out := url.Values{}
		for k, vs := range params {
			for _, v := range vs {
				out.Add(k, strings.ReplaceAll(v, "STATE", q.Get("state")))
			}
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+out.Encode(), http.StatusFound)
	}
}

func browser(t *testing.T) func(string) error {
	return func(u string) error {
		if !strings.HasPrefix(u, "http://127.0.0.1:") {
			t.Errorf("login url = %q, want loopback", u)
		}
		resp, err := http.Get(u) //nolint:noctx // test browser
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
}

func okCallback() url.Values {
	return url.Values{"code": {"c1"}, "state": {"STATE"}, "client_id": {"oaiapp_test"}, "scope": {scope}}
}

func TestLoginNewRegistration(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	var q url.Values
	f.onAuthorize(okCallback(), &q)
	path := t.TempDir() + "/v.enc"
	email, err := newTestSession(f, openVault(t, path)).Login(context.Background(), browser(t))
	if err != nil || email != "me@example.test" {
		t.Fatalf("email=%q err=%v", email, err)
	}
	if q.Get("client_id") != "dynamic_agent_client" || q.Get("agent_name_hint") != "HaythOS" || q.Get("scope") != scope ||
		q.Get("resource") != resource || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		!strings.HasPrefix(q.Get("ext_agent_host_id"), "urn:uuid:") || q.Has("id_token_hint") {
		t.Errorf("authorize query = %v", q)
	}
	redirect, _ := url.Parse(q.Get("redirect_uri"))
	if redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Path != "/auth/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	form := f.forms[0]
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if form.Get("grant_type") != "authorization_code" || form.Get("client_id") != "oaiapp_test" || form.Get("code") != "c1" ||
		form.Get("redirect_uri") != q.Get("redirect_uri") || form.Get("resource") != resource ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
		t.Errorf("token form = %v", form)
	}
	r := loadRecord(t, openVault(t, path))
	if r.ClientID != "oaiapp_test" || r.HostID != q.Get("ext_agent_host_id") || r.Subject != "user-1" || r.AccessToken != "at-1" || r.RefreshToken != "rt-1" || r.IDToken == "" {
		t.Errorf("record = %+v", r)
	}
}

func TestLoginReauth(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	v := openVault(t, t.TempDir()+"/v.enc")
	seed(t, v, 0)
	old := loadRecord(t, v)
	old.IDToken, old.Email = "old-id", "me@example.test"
	b, _ := json.Marshal(old)
	_ = v.Set(vaultName, string(b))

	var q url.Values
	cb := okCallback()
	cb.Del("client_id")
	f.onAuthorize(cb, &q)
	if _, err := newTestSession(f, v).Login(context.Background(), browser(t)); err != nil {
		t.Fatal(err)
	}
	if q.Get("client_id") != "oaiapp_test" || q.Get("id_token_hint") != "old-id" || q.Get("login_hint") != "me@example.test" ||
		q.Has("agent_name_hint") || q.Get("ext_agent_host_id") != old.HostID {
		t.Errorf("authorize query = %v", q)
	}

	f.mu.Lock()
	f.sub = "someone-else"
	f.mu.Unlock()
	before := loadRecord(t, v)
	if _, err := newTestSession(f, v).Login(context.Background(), browser(t)); err == nil {
		t.Fatal("login as a different account succeeded")
	}
	if after := loadRecord(t, v); after.AccessToken != before.AccessToken {
		t.Errorf("record changed after rejected reauth")
	}
}

func TestLoginRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		cb    url.Values
		scope string
	}{
		"state mismatch":  {cb: url.Values{"code": {"c1"}, "state": {"nope"}, "client_id": {"oaiapp_test"}}},
		"access denied":   {cb: url.Values{"error": {"access_denied"}, "state": {"STATE"}}},
		"no client id":    {cb: url.Values{"code": {"c1"}, "state": {"STATE"}}},
		"dynamic as id":   {cb: url.Values{"code": {"c1"}, "state": {"STATE"}, "client_id": {"dynamic_agent_client"}}},
		"two client ids":  {cb: url.Values{"code": {"c1"}, "state": {"STATE"}, "client_id": {"oaiapp_test", "oaiapp_x"}}},
		"no plan scope":   {cb: okCallback(), scope: "openid profile email offline_access"},
		"bad client char": {cb: url.Values{"code": {"c1"}, "state": {"STATE"}, "client_id": {"oaiapp/../x"}}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeAuth(t)
			if c.scope != "" {
				f.scope = c.scope
			}
			var q url.Values
			f.onAuthorize(c.cb, &q)
			v := openVault(t, t.TempDir()+"/v.enc")
			if _, err := newTestSession(f, v).Login(context.Background(), browser(t)); err == nil {
				t.Fatal("login succeeded")
			}
			if r := loadRecord(t, v); r.ClientID != "" || r.AccessToken != "" || r.HostID == "" {
				t.Errorf("record = %+v, want only the host id", r)
			}
		})
	}
}

func TestLoginHostIDStable(t *testing.T) {
	t.Parallel()
	f := newFakeAuth(t)
	var q1, q2 url.Values
	v := openVault(t, t.TempDir()+"/v.enc")
	f.onAuthorize(url.Values{"error": {"access_denied"}, "state": {"STATE"}}, &q1)
	_, _ = newTestSession(f, v).Login(context.Background(), browser(t))
	f.onAuthorize(okCallback(), &q2)
	if _, err := newTestSession(f, v).Login(context.Background(), browser(t)); err != nil {
		t.Fatal(err)
	}
	if q1.Get("ext_agent_host_id") == "" || q1.Get("ext_agent_host_id") != q2.Get("ext_agent_host_id") {
		t.Errorf("host ids %q vs %q", q1.Get("ext_agent_host_id"), q2.Get("ext_agent_host_id"))
	}
}
