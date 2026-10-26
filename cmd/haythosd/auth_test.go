package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/panca/haythos/adapters/vault/file"
)

func TestAuthStoresTrimmedSecretAndNeverEchoesIt(t *testing.T) {
	v, err := file.Open(t.TempDir()+"/v.enc", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAuth([]string{"claude"}, v, &out, func() (string, error) { return "  tok-123\n", nil }); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.Resolve("secret://claude"); got != "tok-123" {
		t.Errorf("stored %q", got)
	}
	if strings.Contains(out.String(), "tok-123") {
		t.Error("output echoes the secret")
	}
}

func TestAuthRejectsBadInput(t *testing.T) {
	v, _ := file.Open(t.TempDir()+"/v.enc", bytes.Repeat([]byte{7}, 32))
	read := func() (string, error) { return " \n", nil }
	var out bytes.Buffer
	if runAuth([]string{"nope"}, v, &out, read) == nil {
		t.Error("unknown target accepted")
	}
	if runAuth([]string{"openai"}, v, &out, read) == nil {
		t.Error("empty secret accepted")
	}
	if runAuth(nil, v, &out, read) == nil {
		t.Error("missing target accepted")
	}
}
