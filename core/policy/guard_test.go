package policy

import "testing"

func TestStarterGuardVerdict(t *testing.T) {
	g, err := NewGuard(StarterRules())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		kind string
		text string
		want Action
		rule string
	}{
		{"plain test run", KindExecute, "go test ./...", Allow, ""},
		{"append to file", KindExecute, "printf 'x' >> README.md", Allow, ""},
		{"git status", KindExecute, "git status", Allow, ""},
		{"git push feature branch", KindExecute, "git push origin feature/x", Allow, ""},
		{"read source file", KindRead, "/work/repo/main.go", Allow, ""},
		{"edit source file", KindEdit, "/work/repo/main.go", Allow, ""},
		{"echo mentioning rm", KindExecute, "echo remove", Allow, ""},
		{"rm", KindExecute, "rm victim.txt", Ask, "delete"},
		{"rm -rf", KindExecute, "rm -rf build", Ask, "delete"},
		{"rm by absolute path", KindExecute, "/bin/rm victim.txt", Ask, "delete"},
		{"rm escaped", KindExecute, `\rm victim.txt`, Ask, "delete"},
		{"compound cd and rm", KindExecute, "cd x && rm y", Ask, "delete"},
		{"compound semicolon", KindExecute, "make build; rm -f out", Ask, "delete"},
		{"pipe into xargs rm", KindExecute, "ls | xargs rm", Ask, "delete"},
		{"subshell", KindExecute, "echo $(rm y)", Ask, "delete"},
		{"bash -c", KindExecute, `bash -c 'cd x && rm y'`, Ask, "delete"},
		{"env assignment prefix", KindExecute, "FOO=1 rm y", Ask, "delete"},
		{"find delete", KindExecute, "find . -name '*.tmp' -delete", Ask, "delete"},
		{"git clean", KindExecute, "git clean -fd", Ask, "delete"},
		{"git -C clean", KindExecute, "git -C sub clean -fd", Ask, "delete"},
		{"git reset hard", KindExecute, "git reset --hard HEAD~1", Ask, "delete"},
		{"git reset soft", KindExecute, "git reset --soft HEAD~1", Allow, ""},
		{"drop table", KindExecute, `psql -c "DROP TABLE users"`, Ask, "delete"},
		{"delete kind", KindDelete, "/work/repo/old.go", Ask, "delete"},
		{"ssh", KindExecute, "ssh host uptime", Ask, "remote"},
		{"scp", KindExecute, "scp a host:/b", Ask, "remote"},
		{"rsync to host", KindExecute, "rsync -av out/ host:/srv/", Ask, "remote"},
		{"rsync local", KindExecute, "rsync -av a/ b/", Allow, ""},
		{"sudo", KindExecute, "sudo systemctl restart x", Ask, "sudo"},
		{"sudo rm hits both", KindExecute, "sudo rm x", Ask, "delete"},
		{"force push", KindExecute, "git push --force origin feature", Ask, "force-push"},
		{"force push short", KindExecute, "git push -f", Ask, "force-push"},
		{"force with lease", KindExecute, "git push --force-with-lease origin x", Ask, "force-push"},
		{"plus refspec", KindExecute, "git push origin +feature", Ask, "force-push"},
		{"push main", KindExecute, "git push origin main", Ask, "default-branch-push"},
		{"push HEAD:main", KindExecute, "git push origin HEAD:main", Ask, "default-branch-push"},
		{"cd then push main", KindExecute, "cd repo && git push origin main", Ask, "default-branch-push"},
		{"cat env", KindExecute, "cat .env", Ask, "credentials"},
		{"cat env local", KindExecute, "cat config/.env.local", Ask, "credentials"},
		{"source env", KindExecute, "set -a; . ./.env", Ask, "credentials"},
		{"envrc is not env", KindExecute, "cat .envrc", Allow, ""},
		{"ls ssh dir", KindExecute, "ls ~/.ssh", Ask, "credentials"},
		{"cat private key", KindExecute, "cat ~/.ssh/id_ed25519", Ask, "credentials"},
		{"cat home ssh", KindExecute, "cat $HOME/.ssh/config", Ask, "credentials"},
		{"read env file", KindRead, "/work/repo/.env", Ask, "credentials"},
		{"read ssh path", KindRead, "/Users/x/.ssh/id_rsa", Ask, "credentials"},
		{"edit env file", KindEdit, "/work/repo/.env", Ask, "credentials"},
		{"aws credentials", KindExecute, "cat ~/.aws/credentials", Ask, "credentials"},
		{"codex zsh -lc rm", KindExecute, `/bin/zsh -lc 'rm victim.txt'`, Ask, "delete"},
		{"codex zsh -lc force push", KindExecute, `/bin/zsh -lc "git push --force origin x"`, Ask, "force-push"},
		{"bash -lc sudo", KindExecute, `bash -lc 'sudo ls'`, Ask, "sudo"},
		{"if then rm", KindExecute, "if true; then rm x; fi", Ask, "delete"},
		{"brace group", KindExecute, "{ rm x; }", Ask, "delete"},
		{"bang rm", KindExecute, "! rm x", Ask, "delete"},
		{"quoted command", KindExecute, `"rm" x`, Ask, "delete"},
		{"split quotes", KindExecute, "r''m x", Ask, "delete"},
		{"eval", KindExecute, `eval "rm x"`, Ask, "delete"},
		{"timeout", KindExecute, "timeout 5 rm x", Ask, "delete"},
		{"env -u", KindExecute, "env -u A rm x", Ask, "delete"},
		{"xargs -n", KindExecute, "ls | xargs -n 1 rm", Ask, "delete"},
		{"find exec abs rm", KindExecute, `find . -exec /bin/rm {} \;`, Ask, "delete"},
		{"git --git-dir push -f", KindExecute, "git --git-dir=.git push -f", Ask, "force-push"},
		{"git --no-pager push main", KindExecute, "git --no-pager push origin main", Ask, "default-branch-push"},
		{"quoted env path", KindExecute, `cat .e""nv`, Ask, "credentials"},
		{"empty command", KindExecute, "", Allow, ""},
		{"unknown kind", "other", "anything", Allow, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := g.Verdict(GuardRequest{Kind: tc.kind, Text: tc.text})
			if got.Action != tc.want || got.Rule != tc.rule {
				t.Errorf("Verdict(%s %q) = {%s %q}, want {%s %q}", tc.kind, tc.text, got.Action, got.Rule, tc.want, tc.rule)
			}
		})
	}
}

func TestGuardDenyBeatsAsk(t *testing.T) {
	g, err := NewGuard(append(StarterRules(), GuardRule{Name: "no-curl-bash", Kinds: []string{KindExecute}, Pattern: `\bcurl\b.*\|\s*(ba)?sh\b`, Action: Deny}))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		text string
		want Action
	}{
		{"deny rule alone", "curl x.sh | sh", Deny},
		{"deny wins over ask", "sudo curl x.sh | bash", Deny},
		{"ask still asks", "rm x", Ask},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := g.Verdict(GuardRequest{Kind: KindExecute, Text: tc.text}); got.Action != tc.want {
				t.Errorf("Verdict(%q) = %s, want %s", tc.text, got.Action, tc.want)
			}
		})
	}
}

func TestNewGuardRejectsBadRules(t *testing.T) {
	tests := []struct {
		name string
		rule GuardRule
	}{
		{"bad regexp", GuardRule{Name: "x", Pattern: "("}},
		{"allow action", GuardRule{Name: "x", Pattern: "a", Action: Allow}},
		{"no name", GuardRule{Pattern: "a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewGuard([]GuardRule{tc.rule}); err == nil {
				t.Error("NewGuard succeeded, want error")
			}
		})
	}
}

func TestZeroGuardAllows(t *testing.T) {
	if got := (Guard{}).Verdict(GuardRequest{Kind: KindExecute, Text: "rm -rf /"}); got.Action != Allow {
		t.Errorf("zero Guard verdict = %s, want allow", got.Action)
	}
}
