package app_test

import (
	"context"
	"testing"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
)

func TestThreadModesPersistAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db := t.TempDir() + "/haythos.db"
	a := replyApp(t, db, &testfakes.Notifier{})

	for _, step := range []func() error{
		func() error { return a.ActivateThread(ctx, "t1") },
		func() error { return a.ActivateThread(ctx, "t1") },
		func() error { return a.SetThreadMentionOnly(ctx, "t1", true) },
		func() error { return a.SetThreadMentionOnly(ctx, "t1", false) },
		func() error { return a.SetThreadMentionOnly(ctx, "t1", true) },
		func() error { return a.SetThreadMentionOnly(ctx, "t2", true) },
		func() error { return a.ActivateThread(ctx, "t3") },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}

	got, err := replyApp(t, db, &testfakes.Notifier{}).ThreadModes(ctx)
	if err != nil {
		t.Fatalf("modes: %v", err)
	}
	want := map[string]app.ThreadMode{
		"t1": {Active: true, MentionOnly: true},
		"t2": {MentionOnly: true},
		"t3": {Active: true},
	}
	if len(got) != len(want) {
		t.Fatalf("modes = %+v, want %+v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("mode[%s] = %+v, want %+v", id, got[id], w)
		}
	}
}
