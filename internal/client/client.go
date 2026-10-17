package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/agyard/agsudo/internal/api"
)

const DefaultHelper = "/usr/local/libexec/agsudo-helper"

type Client struct{ Helper string }

func (c Client) Call(ctx context.Context, in api.Input, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.Helper, "rpc")
	cmd.Stdin = bytes.NewReader(b)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	var r api.Response
	if err = json.Unmarshal(stdout.Bytes(), &r); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return api.Err("helper_unavailable", "helper %s: %v %s (install AG-sudo or set AGSUDO_HELPER for development)", c.Helper, runErr, stderr.String())
	}
	if r.SchemaVersion != api.Version {
		return api.Err("protocol_version", "incompatible helper protocol version %d", r.SchemaVersion)
	}
	if r.Error != nil {
		return r.Error
	}
	if runErr != nil {
		return runErr
	}
	if out != nil {
		return json.Unmarshal(r.Data, out)
	}
	return nil
}

type ExitStatus struct{ Code int }

func (e *ExitStatus) Error() string { return fmt.Sprintf("command exited with status %d", e.Code) }

type RunOptions struct {
	NonInteractive bool
	Sudo           bool // Select presentation only; never changes authorization.
}

func (c Client) Run(ctx context.Context, id string, opts RunOptions, in io.Reader, out, errOut io.Writer) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	defer w.Close()
	mode := "run"
	if opts.Sudo {
		mode = "run-sudo"
	}
	cmd := exec.Command(c.Helper, mode)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errOut
	cmd.ExtraFiles = []*os.File{r}
	if err = cmd.Start(); err != nil {
		return err
	}
	r.Close()
	err = json.NewEncoder(w).Encode(api.Input{ID: id, NonInteractive: opts.NonInteractive})
	w.Close()
	if err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGINT)
		err = <-done
	}
	if err == nil {
		return nil
	}
	var e *exec.ExitError
	if errors.As(err, &e) {
		st := e.Sys().(syscall.WaitStatus)
		if st.Signaled() {
			return &ExitStatus{128 + int(st.Signal())}
		}
		return &ExitStatus{e.ExitCode()}
	}
	return err
}

func (c Client) ListAll(ctx context.Context, states []string, session string) ([]api.Request, error) {
	all := []api.Request{}
	var cursor int64
	for {
		var page api.List
		if err := c.Call(ctx, api.Input{Operation: "list", States: states, Session: session, Cursor: cursor, Limit: 200}, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Requests...)
		if page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return all, nil
}
