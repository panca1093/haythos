package main

import (
	"context"
	"strings"

	"github.com/panca/haythos/adapters/channel/ws"
	"github.com/panca/haythos/app"
)

// routeNotifier sends each notice to the adapter that owns its origin.
type routeNotifier struct {
	ws, other app.Notifier
}

func (r *routeNotifier) Publish(ctx context.Context, n app.Notice) error {
	if strings.HasPrefix(n.ReplyTo, ws.Prefix) {
		return r.ws.Publish(ctx, n)
	}
	return r.other.Publish(ctx, n)
}
