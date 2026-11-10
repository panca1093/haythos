package keychain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const value = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

type exitErr int

func (e exitErr) Error() string { return "exit status" }
func (e exitErr) ExitCode() int { return int(e) }

type call struct {
	Stdin string
	Argv  []string
}

type fakeRun struct {
	calls []call
	out   string
	err   error
}

func (f *fakeRun) run(_ context.Context, stdin, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{Stdin: stdin, Argv: append([]string{name}, args...)})
	return []byte(f.out), f.err
}

func TestArgvPerOS(t *testing.T) {
	t.Parallel()
	tests := []struct {
		goos    string
		get     []string
		set     call
		missing int
	}{
		{"darwin", []string{"security", "find-generic-password", "-s", "haythos", "-a", "vault-key", "-w"},
			call{Argv: []string{"security", "add-generic-password", "-U", "-s", "haythos", "-a", "vault-key", "-w", value}}, 44},
		{"linux", []string{"secret-tool", "lookup", "service", "haythos", "account", "vault-key"},
			call{Stdin: value, Argv: []string{"secret-tool", "store", "--label=haythos", "service", "haythos", "account", "vault-key"}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			t.Parallel()
			f := &fakeRun{out: value + "\n"}
			k := &Keychain{goos: tt.goos, run: f.run}
			got, err := k.Get(context.Background())
			if err != nil || got != value {
				t.Fatalf("get = %q, %v", got, err)
			}
			if diff := cmp.Diff(tt.get, f.calls[0].Argv); diff != "" {
				t.Fatalf("get argv (-want +got):\n%s", diff)
			}
			f.out = ""
			if err := k.Set(context.Background(), value); err != nil {
				t.Fatalf("set: %v", err)
			}
			if diff := cmp.Diff(tt.set, f.calls[1]); diff != "" {
				t.Fatalf("set call (-want +got):\n%s", diff)
			}

			f.err = exitErr(tt.missing)
			if _, err := k.Get(context.Background()); !errors.Is(err, ErrNotFound) {
				t.Fatalf("get of a missing entry = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestErrorsNeverCarryTheValue(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"darwin", "linux"} {
		f := &fakeRun{out: value, err: exitErr(2)}
		k := &Keychain{goos: goos, run: f.run}
		_, getErr := k.Get(context.Background())
		setErr := k.Set(context.Background(), value)
		for _, err := range []error{getErr, setErr} {
			if err == nil || errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), value) {
				t.Fatalf("%s: err = %v; want a failure that does not contain the key", goos, err)
			}
		}
	}
}

func TestUnsupportedOS(t *testing.T) {
	t.Parallel()
	f := &fakeRun{}
	k := &Keychain{goos: "windows", run: f.run}
	if _, err := k.Get(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get = %v, want ErrNotFound", err)
	}
	if err := k.Set(context.Background(), value); err == nil {
		t.Fatalf("set on an unsupported OS: want an error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("ran %v on an unsupported OS", f.calls)
	}
}
