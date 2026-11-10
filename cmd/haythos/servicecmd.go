package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/panca/haythos/adapters/service"
	"github.com/panca/haythos/adapters/vault/file"
	"github.com/panca/haythos/adapters/vault/keychain"
)

type keyStore interface {
	Get(ctx context.Context) (string, error)
	Set(ctx context.Context, value string) error
}

type serviceDeps struct {
	kc        keyStore
	getenv    func(string) string
	in        io.Reader
	out       io.Writer
	stop      func(ctx context.Context) error
	installer interface {
		Install(ctx context.Context) error
		Uninstall(ctx context.Context) error
	}
}

func runService(ctx context.Context, args []string, d *serviceDeps) error {
	if len(args) != 1 || (args[0] != "install" && args[0] != "uninstall") {
		return errors.New("usage: haythos service install|uninstall")
	}
	if args[0] == "uninstall" {
		if err := d.installer.Uninstall(ctx); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(d.out, "service removed; the vault key stays in the OS keychain")
		return nil
	}
	if err := ensureKeychainKey(ctx, d); err != nil {
		return err
	}
	if err := d.stop(ctx); err != nil {
		return fmt.Errorf("stop the running kernel first: %w", err)
	}
	if err := d.installer.Install(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(d.out, "service installed; it starts now and at every login")
	return nil
}

// ensureKeychainKey makes sure the service, which gets no environment, can read the vault key.
func ensureKeychainKey(ctx context.Context, d *serviceDeps) error {
	env := d.getenv(file.KeyEnv)
	stored, err := d.kc.Get(ctx)
	switch {
	case err == nil:
		if env != "" && strings.TrimSpace(env) != stored {
			_, _ = fmt.Fprintf(d.out, "warning: %s differs from the key in the OS keychain; the service uses the keychain key\n", file.KeyEnv)
		}
		return nil
	case !errors.Is(err, keychain.ErrNotFound):
		return fmt.Errorf("read the OS keychain: %w", err)
	case env == "":
		return fmt.Errorf("no vault key: set %s and run `haythos service install` again", file.KeyEnv)
	}
	if _, err := file.KeyFromEnv(d.getenv); err != nil {
		return err
	}
	_, _ = fmt.Fprint(d.out, "Store the vault key in the OS keychain so the service can start without it? [y/N] ")
	line, _ := bufio.NewReader(d.in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("aborted: the service cannot read the vault key without the OS keychain; run `haythos serve` yourself instead")
	}
	if err := d.kc.Set(ctx, strings.TrimSpace(env)); err != nil {
		return fmt.Errorf("store the vault key: %w", err)
	}
	_, _ = fmt.Fprintln(d.out, "vault key stored in the OS keychain")
	return nil
}

func newInstaller(home paths, args []string) (*service.Installer, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find own binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, fmt.Errorf("resolve own binary: %w", err)
	}
	if strings.Contains(exe, "go-build") {
		return nil, errors.New("install from a built binary (go install ./cmd/haythos), not from go run")
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("find working directory: %w", err)
	}
	if err := os.MkdirAll(home.Logs, 0o700); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	return &service.Installer{
		Home: userHome, ExecPath: exe, GOOS: runtime.GOOS, WorkDir: wd,
		LogPath: filepath.Join(home.Logs, "haythos.log"), Args: args, UID: os.Getuid(),
		Run: func(ctx context.Context, name string, args ...string) error {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // G204: fixed launchctl/systemctl invocations
			if err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		WriteFile: func(path string, b []byte, mode os.FileMode) error {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			return os.WriteFile(path, b, mode)
		},
		Remove: os.Remove,
	}, nil
}

// serveArgs are the flags this process got, made absolute, so a detached kernel reads the same files.
func serveArgs(cfgPath, policyPath, jobsDir string) ([]string, error) {
	out := []string{}
	for _, f := range []struct{ flag, path string }{{"-config", cfgPath}, {"-policy", policyPath}, {"-jobs", jobsDir}} {
		abs, err := filepath.Abs(f.path)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", f.path, err)
		}
		out = append(out, f.flag, abs)
	}
	return append(out, "serve"), nil
}
