package keychain

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

const (
	service = "haythos"
	account = "vault-key"
)

var ErrNotFound = errors.New("no vault key in the OS keychain")

// Keychain stores the vault key in the macOS keychain or the Secret Service (secret-tool) on Linux.
type Keychain struct {
	goos string
	run  func(ctx context.Context, stdin, name string, args ...string) ([]byte, error)
}

func New() *Keychain { return &Keychain{goos: runtime.GOOS, run: runCmd} }

func (k *Keychain) Get(ctx context.Context) (string, error) {
	var name string
	var args []string
	missing := 0
	switch k.goos {
	case "darwin":
		name, args, missing = "security", []string{"find-generic-password", "-s", service, "-a", account, "-w"}, 44
	case "linux":
		name, args, missing = "secret-tool", []string{"lookup", "service", service, "account", account}, 1
	default:
		return "", ErrNotFound
	}
	out, err := k.run(ctx, "", name, args...)
	value := strings.TrimRight(string(out), "\r\n")
	if err != nil {
		var ee interface{ ExitCode() int }
		if errors.As(err, &ee) && ee.ExitCode() == missing && value == "" {
			return "", ErrNotFound
		}
		return "", failed(name, err)
	}
	if value == "" {
		return "", ErrNotFound
	}
	return value, nil
}

func (k *Keychain) Set(ctx context.Context, value string) error {
	var err error
	switch k.goos {
	case "darwin":
		quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
		_, err = k.run(ctx, "add-generic-password -U -s "+service+" -a "+account+" -w "+quoted+"\n", "security", "-i")
		if err != nil {
			return failed("security", err)
		}
	case "linux":
		_, err = k.run(ctx, value, "secret-tool", "store", "--label="+service, "service", service, "account", account)
		if err != nil {
			return failed("secret-tool", err)
		}
	default:
		return fmt.Errorf("no supported OS keychain on %s", k.goos)
	}
	return nil
}

// failed never wraps err text from a command that saw the key in its argv or stdin, only the exit code.
func failed(name string, err error) error {
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		return fmt.Errorf("keychain: %s exited with status %d", name, ee.ExitCode())
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("keychain: %s is not installed", name)
	}
	return fmt.Errorf("keychain: %s could not run", name)
}

func runCmd(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: name and args are fixed keychain commands chosen above
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd.Output()
}
