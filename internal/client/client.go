package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/agyard/agsudo/internal/api"
)

const DefaultHelper = "/usr/local/libexec/agsudo-helper"

type Client struct{ Helper string }

// New uses AGSUDO_HELPER, for development builds, or the installed helper.
func New() Client {
	if helper := os.Getenv("AGSUDO_HELPER"); helper != "" {
		return Client{helper}
	}
	return Client{DefaultHelper}
}

func (c Client) unavailable(err error, stderr string) error {
	message := fmt.Sprint(err)
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		message += ": " + stderr
	}
	return api.Err("helper_unavailable", "helper %s: %s (install AG-sudo or set AGSUDO_HELPER for development)", c.Helper, message)
}

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
		return c.unavailable(runErr, stderr.String())
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
		err = syscall.Exec(path, []string{path, mode, id}, os.Environ())
	}
	return c.unavailable(err, "")
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
