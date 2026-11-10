package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/panca/haythos/adapters/vault/keychain"
)

const testKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

type fakeKeyStore struct {
	value string
	sets  []string
}

func (k *fakeKeyStore) Get(context.Context) (string, error) {
	if k.value == "" {
		return "", keychain.ErrNotFound
	}
	return k.value, nil
}

func (k *fakeKeyStore) Set(_ context.Context, v string) error {
	k.sets = append(k.sets, v)
	k.value = v
	return nil
}

type fakeInstaller struct{ steps *[]string }

func (f fakeInstaller) Install(context.Context) error {
	*f.steps = append(*f.steps, "install")
	return nil
}

func (f fakeInstaller) Uninstall(context.Context) error {
	*f.steps = append(*f.steps, "uninstall")
	return nil
}

func serviceRig(kc *fakeKeyStore, env, answer string) (*serviceDeps, *[]string, *bytes.Buffer) {
	var steps []string
	out := &bytes.Buffer{}
	return &serviceDeps{
		kc:        kc,
		getenv:    func(k string) string { return map[string]string{"HAYTHOS_VAULT_KEY": env}[k] },
		in:        strings.NewReader(answer),
		out:       out,
		stop:      func(context.Context) error { steps = append(steps, "stop"); return nil },
		installer: fakeInstaller{&steps},
	}, &steps, out
}

func TestServiceInstallStoresKeyOnYes(t *testing.T) {
	t.Parallel()
	kc := &fakeKeyStore{}
	d, steps, out := serviceRig(kc, testKey, "y\n")
	if err := runService(context.Background(), []string{"install"}, d); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(kc.sets) != 1 || kc.sets[0] != testKey {
		t.Fatalf("keychain sets = %d, want the env key stored once", len(kc.sets))
	}
	if got := strings.Join(*steps, ","); got != "stop,install" {
		t.Fatalf("steps = %s, want stop,install", got)
	}
	if !strings.Contains(out.String(), "Store the vault key in the OS keychain so the service can start without it? [y/N]") {
		t.Fatalf("no prompt in %q", out.String())
	}
	if strings.Contains(out.String(), testKey) {
		t.Fatalf("output contains the key")
	}
}

func TestServiceInstallAbortsOnNo(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"n\n", "\n", ""} {
		kc := &fakeKeyStore{}
		d, steps, _ := serviceRig(kc, testKey, answer)
		err := runService(context.Background(), []string{"install"}, d)
		if err == nil || len(kc.sets) != 0 || len(*steps) != 0 {
			t.Fatalf("answer %q: err = %v, sets = %d, steps = %v; want abort with nothing done", answer, err, len(kc.sets), *steps)
		}
		if strings.Contains(err.Error(), testKey) {
			t.Fatalf("error contains the key")
		}
	}
}

func TestServiceInstallKeyAlreadyInKeychain(t *testing.T) {
	t.Parallel()
	kc := &fakeKeyStore{value: testKey}
	d, steps, out := serviceRig(kc, "", "")
	if err := runService(context.Background(), []string{"install"}, d); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(kc.sets) != 0 || strings.Contains(out.String(), "[y/N]") || strings.Join(*steps, ",") != "stop,install" {
		t.Fatalf("sets = %d, out = %q, steps = %v", len(kc.sets), out.String(), *steps)
	}
}

func TestServiceInstallNoKeyAnywhere(t *testing.T) {
	t.Parallel()
	d, steps, _ := serviceRig(&fakeKeyStore{}, "", "")
	err := runService(context.Background(), []string{"install"}, d)
	if err == nil || !strings.Contains(err.Error(), "HAYTHOS_VAULT_KEY") || len(*steps) != 0 {
		t.Fatalf("err = %v, steps = %v; want an error naming HAYTHOS_VAULT_KEY", err, *steps)
	}
}

func TestServiceUninstall(t *testing.T) {
	t.Parallel()
	d, steps, _ := serviceRig(&fakeKeyStore{}, "", "")
	if err := runService(context.Background(), []string{"uninstall"}, d); err != nil || strings.Join(*steps, ",") != "uninstall" {
		t.Fatalf("err = %v, steps = %v", err, *steps)
	}
	if err := runService(context.Background(), []string{"bogus"}, d); err == nil {
		t.Fatalf("unknown service action: want an error")
	}
}
