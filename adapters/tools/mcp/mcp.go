package mcp

import (
	"context"
	"errors"

	"github.com/panca/haythos/core/task"
)

type Config struct {
	Name    string
	Command string
	Args    []string
	Env     []string
	Trusted bool
}

type Client struct{}

func Start(context.Context, Config) (*Client, error) { return nil, errors.New("not implemented") }

func (c *Client) List([]string) []task.ToolSpec { return nil }

func (c *Client) Call(context.Context, task.ToolCall) (task.ToolResult, error) {
	return task.ToolResult{}, errors.New("not implemented")
}

func (c *Client) Exists(context.Context, string, string, string) (bool, error) { return false, nil }

func (c *Client) Close() error { return nil }
