package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// Client is one connection to the kernel socket.
type Client struct {
	ws *websocket.Conn
}

type Welcome struct {
	Version   string
	PID       int
	StartedAt time.Time
	Channels  []string
}

func Dial(ctx context.Context, socketPath string) (*Client, error) {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}}}
	c, resp, err := websocket.Dial(ctx, "ws://haythos/", &websocket.DialOptions{HTTPClient: hc})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("dial kernel at %s: %w", socketPath, err)
	}
	c.SetReadLimit(readLimit)
	return &Client{ws: c}, nil
}

func (c *Client) Hello(ctx context.Context, client, instance, version string) (Welcome, error) {
	if err := c.Send(ctx, Hello{Client: client, Instance: instance, Version: version}); err != nil {
		return Welcome{}, err
	}
	f, err := c.Next(ctx)
	if err != nil {
		return Welcome{}, err
	}
	if f.Type != TypeWelcome {
		return Welcome{}, fmt.Errorf("kernel answered hello with %q: %s", f.Type, f.Message)
	}
	return Welcome{Version: f.Version, PID: f.PID, StartedAt: f.StartedAt, Channels: f.Channels}, nil
}

func (c *Client) Send(ctx context.Context, frame any) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("send frame: %w", err)
	}
	return nil
}

// Next blocks for the next server frame; a cancelled ctx closes the connection.
func (c *Client) Next(ctx context.Context) (Frame, error) {
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		return Frame{}, fmt.Errorf("read frame: %w", err)
	}
	var f Frame
	if err := json.Unmarshal(b, &f); err != nil {
		return Frame{}, fmt.Errorf("decode frame: %w", err)
	}
	return f, nil
}

func (c *Client) Close() error {
	if err := c.ws.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		return fmt.Errorf("close kernel connection: %w", err)
	}
	return nil
}
