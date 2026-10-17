package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/agyard/agsudo/internal/api"
)

const defaultHelper = "/usr/local/libexec/agsudo-helper"

type Client struct{ Helper string }

// New uses AGSUDO_HELPER, for development builds, or the installed helper.
func New() Client {
	if helper := os.Getenv("AGSUDO_HELPER"); helper != "" {
		return Client{helper}
	}
	return Client{defaultHelper}
}

func (c Client) unavailable(err error, stderr string) error {
	message := fmt.Sprint(err)
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		message += ": " + stderr
	}
	return api.Err("helper_unavailable", "helper %s: %s (install AG-sudo or set AGSUDO_HELPER for development)", c.Helper, message)
}

// Call runs one helper operation and decodes its result into out.
func (c Client) Call(ctx context.Context, in api.Input, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.Helper, "rpc")
	cmd.Stdin = bytes.NewReader(b)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	r := api.Response{Data: out}
	if err = json.Unmarshal(stdout.Bytes(), &r); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return c.unavailable(runErr, stderr.String())
	}
	if r.Error != nil {
		return r.Error
	}
	return runErr
}

// Exec replaces the CLI with the helper, which waits for approval and runs the
// command, so the terminal, signals and exit status need no relaying. It only
// returns on failure.
func (c Client) Exec(id string, sudo bool) error {
	mode := "run"
	if sudo {
		mode = "run-sudo"
	}
	path, err := exec.LookPath(c.Helper)
	if err == nil {
		// Ignored dispositions survive exec: until the helper installs its
		// handlers, an early Ctrl-C cannot kill it and strand the request.
		signal.Ignore(syscall.SIGINT, syscall.SIGHUP)
		err = syscall.Exec(path, []string{path, mode, id}, os.Environ())
	}
	return c.unavailable(err, "")
}
