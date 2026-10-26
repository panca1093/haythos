package file

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

const KeyEnv = "HAYTHOS_VAULT_KEY"

var ErrNotFound = errors.New("secret not found")

type Vault struct {
	path string
	gcm  cipher.AEAD
	mu   sync.Mutex
	data map[string]string
}

// KeyFromEnv reads a base64 32-byte key from KeyEnv.
// TODO: uncertain — OS keyring is not implemented, only the env var; verify before prod
func KeyFromEnv(getenv func(string) string) ([]byte, error) {
	raw := getenv(KeyEnv)
	if raw == "" {
		return nil, fmt.Errorf("%s is not set (generate one with: openssl rand -base64 32)", KeyEnv)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be base64 of exactly 32 bytes", KeyEnv)
	}
	return key, nil
}

func Open(path string, key []byte) (*Vault, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault cipher: %w", err)
	}
	v := &Vault{path: path, gcm: gcm, data: map[string]string{}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return v, nil
	case err != nil:
		return nil, fmt.Errorf("read vault: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("vault file is truncated")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return nil, errors.New("vault file cannot be decrypted (wrong key or corrupt file)")
	}
	if err := json.Unmarshal(plain, &v.data); err != nil {
		return nil, fmt.Errorf("decode vault: %w", err)
	}
	return v, nil
}

// Resolve accepts a handle such as secret://github.
func (v *Vault) Resolve(handle string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	val, ok := v.data[strings.TrimPrefix(handle, "secret://")]
	if !ok {
		return "", fmt.Errorf("%s: %w", handle, ErrNotFound)
	}
	return val, nil
}

func (v *Vault) Has(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.data[name]
	return ok
}

func (v *Vault) Set(name, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.data[name] = value
	plain, err := json.Marshal(v.data)
	if err != nil {
		return fmt.Errorf("encode vault: %w", err)
	}
	nonce := make([]byte, v.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("vault nonce: %w", err)
	}
	tmp := v.path + ".tmp"
	if err := os.WriteFile(tmp, v.gcm.Seal(nonce, nonce, plain, nil), 0o600); err != nil {
		return fmt.Errorf("write vault: %w", err)
	}
	if err := os.Rename(tmp, v.path); err != nil {
		return fmt.Errorf("replace vault: %w", err)
	}
	return nil
}
