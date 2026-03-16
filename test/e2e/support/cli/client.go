package cli

import (
	"context"
	"os/exec"
)

type Option func(*Client)

type Client struct {
	binaryPath string
	endpoint   string
	namespace  string
}

func New(binaryPath string, opts ...Option) *Client {
	client := &Client{binaryPath: binaryPath}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

func WithEndpoint(endpoint string) Option {
	return func(client *Client) {
		client.endpoint = endpoint
	}
}

func WithNamespace(namespace string) Option {
	return func(client *Client) {
		client.namespace = namespace
	}
}

func (c *Client) Command(ctx context.Context, args ...string) *exec.Cmd {
	commandArgs := make([]string, 0, len(args)+4)
	if c.endpoint != "" {
		commandArgs = append(commandArgs, "--endpoint", c.endpoint)
	}
	if c.namespace != "" {
		commandArgs = append(commandArgs, "--namespace", c.namespace)
	}
	commandArgs = append(commandArgs, args...)
	return exec.CommandContext(ctx, c.binaryPath, commandArgs...)
}
