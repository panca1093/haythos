package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	Label    = "io.github.panca1093.haythos"
	unitName = "haythos"
)

// LaunchdPlist is a user agent that keeps `haythos serve` running. It carries no environment: the vault key comes from the keychain.
func LaunchdPlist(label, execPath, workDir, logPath string, args []string) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + esc(label) + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range append([]string{execPath}, args...) {
		b.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>WorkingDirectory</key>
	<string>` + esc(workDir) + `</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + esc(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + esc(logPath) + `</string>
</dict>
</plist>
`)
	return b.Bytes()
}

// SystemdUnit is a user unit for `haythos serve`, restarted when it fails.
func SystemdUnit(execPath, workDir, logPath string, args []string) []byte {
	argv := make([]string, 0, len(args)+1)
	for _, a := range append([]string{execPath}, args...) {
		argv = append(argv, unitQuote(a))
	}
	return []byte(`[Unit]
Description=HaythOS kernel

[Service]
ExecStart=` + strings.Join(argv, " ") + `
WorkingDirectory=` + unitQuote(workDir) + `
Restart=on-failure
StandardOutput=append:` + strings.ReplaceAll(logPath, "%", "%%") + `
StandardError=append:` + strings.ReplaceAll(logPath, "%", "%%") + `

[Install]
WantedBy=default.target
`)
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func unitQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Installer registers the kernel as a launchd agent (macOS) or a systemd user service (Linux).
type Installer struct {
	// Home is the user's home directory.
	Home, ExecPath, GOOS string
	WorkDir, LogPath     string
	// Args follow ExecPath, ending with "serve".
	Args      []string
	UID       int
	Run       func(ctx context.Context, name string, args ...string) error
	WriteFile func(path string, b []byte, mode os.FileMode) error
	Remove    func(path string) error
}

func (in *Installer) Install(ctx context.Context) error {
	switch in.GOOS {
	case "darwin":
		plist := in.plistPath()
		if err := in.WriteFile(plist, LaunchdPlist(Label, in.ExecPath, in.WorkDir, in.LogPath, in.Args), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", plist, err)
		}
		_ = in.Run(ctx, "launchctl", "bootout", in.domain()+"/"+Label)
		return in.run(ctx, "launchctl", "bootstrap", in.domain(), plist)
	case "linux":
		unit := in.unitPath()
		if err := in.WriteFile(unit, SystemdUnit(in.ExecPath, in.WorkDir, in.LogPath, in.Args), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", unit, err)
		}
		if err := in.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		return in.run(ctx, "systemctl", "--user", "enable", "--now", unitName)
	}
	return fmt.Errorf("service install is not supported on %s", in.GOOS)
}

func (in *Installer) Uninstall(ctx context.Context) error {
	switch in.GOOS {
	case "darwin":
		_ = in.Run(ctx, "launchctl", "bootout", in.domain()+"/"+Label)
		return in.remove(in.plistPath())
	case "linux":
		_ = in.Run(ctx, "systemctl", "--user", "disable", "--now", unitName)
		if err := in.remove(in.unitPath()); err != nil {
			return err
		}
		return in.run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return fmt.Errorf("service uninstall is not supported on %s", in.GOOS)
}

func (in *Installer) plistPath() string {
	return filepath.Join(in.Home, "Library", "LaunchAgents", Label+".plist")
}

func (in *Installer) unitPath() string {
	return filepath.Join(in.Home, ".config", "systemd", "user", unitName+".service")
}

func (in *Installer) domain() string { return "gui/" + strconv.Itoa(in.UID) }

func (in *Installer) run(ctx context.Context, name string, args ...string) error {
	if err := in.Run(ctx, name, args...); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (in *Installer) remove(path string) error {
	if err := in.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
