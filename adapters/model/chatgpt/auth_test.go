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
