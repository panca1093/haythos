package file

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestRoundTripEncryptedAtRest(t *testing.T) {
	path := t.TempDir() + "/vault.enc"
	v, err := Open(path, key(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("github", "ghp_supersecret"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("ghp_supersecret")) {
		t.Error("secret stored in plaintext")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	v2, err := Open(path, key(1))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v2.Resolve("secret://github"); err != nil || got != "ghp_supersecret" {
		t.Errorf("got %q err=%v", got, err)
	}
	if _, err := v2.Resolve("secret://nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestWrongKeyFails(t *testing.T) {
	path := t.TempDir() + "/vault.enc"
	v, _ := Open(path, key(1))
	_ = v.Set("a", "b")
	if _, err := Open(path, key(2)); err == nil {
		t.Error("want error with wrong key")
	}
}

func TestKeyFromEnv(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := KeyFromEnv(get(nil)); err == nil {
		t.Error("want error when unset")
	}
	if _, err := KeyFromEnv(get(map[string]string{KeyEnv: "c2hvcnQ="})); err == nil {
		t.Error("want error for short key")
	}
	k, err := KeyFromEnv(get(map[string]string{KeyEnv: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}))
	if err != nil || !bytes.Equal(k, key(1)) {
		t.Errorf("k=%v err=%v", k, err)
	}
}

func TestSetFromTwoInstancesKeepsBoth(t *testing.T) {
	path := t.TempDir() + "/vault.enc"
	a, _ := Open(path, key(1))
	b, _ := Open(path, key(1))
	if err := a.Set("one", "1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Set("two", "2"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []*Vault{a, b} {
		for name, want := range map[string]string{"one": "1", "two": "2"} {
			if got, err := v.Resolve("secret://" + name); err != nil || got != want {
				t.Errorf("%s = %q err=%v, want %q", name, got, err, want)
			}
		}
	}
}
