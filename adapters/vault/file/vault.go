package file

import (
	"context"
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
	"syscall"
)

const KeyEnv = "HAYTHOS_VAULT_KEY"

var ErrNotFound = errors.New("secret not found")

type Vault struct {
	path string
	gcm  cipher.AEAD
	mu   sync.Mutex
}

// KeyFromEnv reads a base64 32-byte key from KeyEnv.
func KeyFromEnv(getenv func(string) string) ([]byte, error) {
	raw := getenv(KeyEnv)
	if raw == "" {
		return nil, fmt.Errorf("%s is not set (generate one with: openssl rand -base64 32)", KeyEnv)
	}
	return decodeKey(raw, KeyEnv)
}

// Key reads the vault key from KeyEnv when set, else from the OS keychain.
func Key(ctx context.Context, getenv func(string) string, kc interface {
	Get(ctx context.Context) (string, error)
}) ([]byte, error) {
	if getenv(KeyEnv) != "" {
		return KeyFromEnv(getenv)
	}
	raw, err := kc.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("no vault key: %s is not set and the OS keychain has none (%w); generate one with: openssl rand -base64 32", KeyEnv, err)
	}
	return decodeKey(raw, "the vault key in the OS keychain")
}

func decodeKey(raw, source string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be base64 of exactly 32 bytes", source)
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
	v := &Vault{path: path, gcm: gcm}
	if _, err := v.read(); err != nil {
		return nil, err
	}
	return v, nil
}

// read decrypts the file on every call so several processes sharing it see each other's writes; Set replaces the file atomically.
func (v *Vault) read() (map[string]string, error) {
	data := map[string]string{}
	raw, err := os.ReadFile(v.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return data, nil
	case err != nil:
		return nil, fmt.Errorf("read vault: %w", err)
	}
	if len(raw) < v.gcm.NonceSize() {
		return nil, errors.New("vault file is truncated")
	}
	plain, err := v.gcm.Open(nil, raw[:v.gcm.NonceSize()], raw[v.gcm.NonceSize():], nil)
	if err != nil {
		return nil, errors.New("vault file cannot be decrypted (wrong key or corrupt file)")
	}
	if err := json.Unmarshal(plain, &data); err != nil {
		return nil, fmt.Errorf("decode vault: %w", err)
	}
	return data, nil
}

// Resolve accepts a handle such as secret://github.
func (v *Vault) Resolve(handle string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	data, err := v.read()
	if err != nil {
		return "", err
	}
	val, ok := data[strings.TrimPrefix(handle, "secret://")]
	if !ok {
		return "", fmt.Errorf("%s: %w", handle, ErrNotFound)
	}
	return val, nil
}

func (v *Vault) Has(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	data, err := v.read()
	if err != nil {
		return false
	}
	_, ok := data[name]
	return ok
}

func (v *Vault) Set(name, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	lock, err := os.OpenFile(v.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open vault lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock vault: %w", err)
	}
	data, err := v.read()
	if err != nil {
		return err
	}
	data[name] = value
	plain, err := json.Marshal(data)
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
