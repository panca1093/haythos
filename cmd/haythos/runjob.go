package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/panca/haythos/adapters/channel/ws"
	"github.com/panca/haythos/app"
)

// refuseDelegatedInProcess: a delegated job asks for approvals, and this process has no channel to deliver them or tick to expire them.
func refuseDelegatedInProcess(jobs []app.Job, name string) error {
	for _, j := range jobs {
		if j.Name == name && j.Runtime != "" {
			return fmt.Errorf("job %q is delegated to a runtime and needs a running kernel for its approvals: start `haythos serve` and run it again", name)
		}
	}
	return nil
}

const runJobDialWait = 2 * time.Second

// runJobRemote asks a running kernel to start the job, so its approvals reach the job's channels; handled is false when no kernel answers.
// The kernel acknowledges nothing, so an error frame within settle is the only sign of a refusal.
func runJobRemote(ctx context.Context, p paths, name string, out io.Writer, settle time.Duration) (handled bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, runJobDialWait)
	defer cancel()
	c, err := ws.Dial(dialCtx, p.Socket)
	if err != nil {
		return false, nil //nolint:nilerr // no kernel on the socket means run the job in this process
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Hello(dialCtx, "run-job", "", binaryVersion()); err != nil {
		return true, fmt.Errorf("greet kernel: %w", err)
	}
	if err := c.Send(ctx, ws.RunJob{Name: name}); err != nil {
		return true, err
	}
	waitCtx, stop := context.WithTimeout(ctx, settle)
	defer stop()
	f, err := c.Next(waitCtx)
	switch {
	case err == nil && f.Type == ws.TypeError:
		return true, fmt.Errorf("kernel could not run job %q: %s", name, f.Message)
	case err != nil && !errors.Is(err, context.DeadlineExceeded):
		return true, err
	}
	_, _ = fmt.Fprintf(out, "started %s on the running kernel; follow it on the job's channel\n", name)
	return true, nil
}
