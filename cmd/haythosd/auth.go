package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/panca/haythos/adapters/vault/file"
)

type authTarget struct {
	secret string
	hint   string
}

var authTargets = map[string]authTarget{
	"claude":    {"claude", "Run `claude setup-token` in another terminal and paste the token it prints."},
	"anthropic": {"anthropic", "Paste an Anthropic API key."},
	"openai":    {"openai", "Paste an API key for your OpenAI-compatible endpoint."},
	"discord":   {"discord", "Paste the Discord bot token. owner_id and channel_id go in config/haythos.yaml."},
}

// runAuth stores one secret in the vault. The kernel never does OAuth; it only stores what the user pastes.
func runAuth(args []string, v *file.Vault, out io.Writer, readSecret func() (string, error)) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: haythosd auth <claude|anthropic|openai|discord>")
	}
	t, ok := authTargets[args[0]]
	if !ok {
		return fmt.Errorf("unknown auth target %q", args[0])
	}
	_, _ = fmt.Fprintln(out, t.hint)
	_, _ = fmt.Fprint(out, "Secret (input hidden): ")
	secret, err := readSecret()
	_, _ = fmt.Fprintln(out)
	if err != nil {
		return fmt.Errorf("read secret: %w", err)
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return fmt.Errorf("empty secret, nothing stored")
	}
	if err := v.Set(t.secret, secret); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Stored as secret://%s\n", t.secret)
	return nil
}

func stdinSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}
