package service

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "rewrite golden files")

var serveArgs = []string{"-config", "/srv/haythos/config/haythos.yaml", "serve"}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Fatalf("%s (-want +got):\n%s", name, diff)
	}
	for _, banned := range []string{"HAYTHOS_VAULT_KEY", "EnvironmentVariables", "Environment="} {
		if strings.Contains(string(got), banned) {
			t.Fatalf("%s contains %q", name, banned)
		}
	}
}

func TestLaunchdPlistGolden(t *testing.T) {
	t.Parallel()
	golden(t, "haythos.plist.golden", LaunchdPlist(Label, "/opt/haythos/bin/haythos", "/srv/haythos", "/home/me/.haythos/logs/haythos.log", serveArgs))
}

func TestSystemdUnitGolden(t *testing.T) {
	t.Parallel()
	golden(t, "haythos.service.golden", SystemdUnit("/opt/haythos/bin/haythos", "/srv/haythos", "/home/me/.haythos/logs/haythos.log", serveArgs))
}

func TestPlistEscapesXML(t *testing.T) {
	t.Parallel()
	got := string(LaunchdPlist(Label, "/opt/a&b/haythos", "/srv/<x>", "/l.log", []string{"serve"}))
	if !strings.Contains(got, "/opt/a&amp;b/haythos") || !strings.Contains(got, "/srv/&lt;x&gt;") {
		t.Fatalf("plist not escaped:\n%s", got)
	}
}

func TestUnitQuotesArgs(t *testing.T) {
	t.Parallel()
	got := string(SystemdUnit("/opt/my apps/haythos", "/srv", "/l.log", []string{"-config", `/srv/100%"x".yaml`, "serve"}))
	if !strings.Contains(got, `ExecStart="/opt/my apps/haythos" -config "/srv/100%%\"x\".yaml" serve`) {
		t.Fatalf("ExecStart not quoted:\n%s", got)
	}
}

type recorder struct {
	runs    []string
	written map[string]string
	removed []string
}

func (r *recorder) installer(goos string) *Installer {
	r.written = map[string]string{}
	return &Installer{
		Home: "/home/me", ExecPath: "/opt/haythos/bin/haythos", GOOS: goos,
		WorkDir: "/srv/haythos", LogPath: "/home/me/.haythos/logs/haythos.log", Args: []string{"serve"}, UID: 501,
		Run: func(_ context.Context, name string, args ...string) error {
			r.runs = append(r.runs, strings.Join(append([]string{name}, args...), " "))
			return nil
		},
		WriteFile: func(path string, b []byte, mode os.FileMode) error {
			r.written[path] = mode.String()
			return nil
		},
		Remove: func(path string) error {
			r.removed = append(r.removed, path)
			return nil
		},
	}
}

func TestInstallerPerOS(t *testing.T) {
	t.Parallel()
	plist := "/home/me/Library/LaunchAgents/" + Label + ".plist"
	unit := "/home/me/.config/systemd/user/haythos.service"
	tests := []struct {
		goos               string
		file               string
		install, uninstall []string
	}{
		{"darwin", plist,
			[]string{"launchctl bootout gui/501/" + Label, "launchctl bootstrap gui/501 " + plist},
			[]string{"launchctl bootout gui/501/" + Label}},
		{"linux", unit,
			[]string{"systemctl --user daemon-reload", "systemctl --user enable --now haythos"},
			[]string{"systemctl --user disable --now haythos", "systemctl --user daemon-reload"}},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			t.Parallel()
			r := &recorder{}
			in := r.installer(tt.goos)
			if err := in.Install(context.Background()); err != nil {
				t.Fatalf("install: %v", err)
			}
			if diff := cmp.Diff(map[string]string{tt.file: "-rw-r--r--"}, r.written); diff != "" {
				t.Fatalf("written (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.install, r.runs); diff != "" {
				t.Fatalf("install runs (-want +got):\n%s", diff)
			}
			r.runs = nil
			if err := in.Uninstall(context.Background()); err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			if diff := cmp.Diff(tt.uninstall, r.runs); diff != "" {
				t.Fatalf("uninstall runs (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]string{tt.file}, r.removed); diff != "" {
				t.Fatalf("removed (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInstallerUnsupportedOS(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	if err := r.installer("windows").Install(context.Background()); err == nil {
		t.Fatalf("install on windows: want an error")
	}
	if len(r.runs) != 0 || len(r.written) != 0 {
		t.Fatalf("did something on an unsupported OS: %v %v", r.runs, r.written)
	}
}
