package main

import (
	"context"
	"testing"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
)

func TestRouteNotifier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		replyTo   string
		ws, other int
	}{
		{"tui:x", 1, 0},
		{"123456", 0, 1},
		{"", 0, 1},
	}
	for _, tt := range tests {
		t.Run("replyTo="+tt.replyTo, func(t *testing.T) {
			t.Parallel()
			ws, other := &testfakes.Notifier{}, &testfakes.Notifier{}
			r := &routeNotifier{ws: ws, other: other}
			if err := r.Publish(context.Background(), app.Notice{Kind: app.NoticeMessage, ReplyTo: tt.replyTo}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if len(ws.Notices) != tt.ws || len(other.Notices) != tt.other {
				t.Fatalf("ws got %d, other got %d; want %d and %d", len(ws.Notices), len(other.Notices), tt.ws, tt.other)
			}
		})
	}
}
