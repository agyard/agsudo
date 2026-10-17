package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/proc"
	"github.com/agyard/agsudo/internal/store"
	"golang.org/x/sys/unix"
)

func respond(data any, err error) int {
	r := api.Response{SchemaVersion: api.Version}
	if err != nil {
		var e *api.Error
		if !errors.As(err, &e) {
			e = &api.Error{Code: "internal", Message: err.Error()}
		}
		r.Error = e
	} else {
		r.Data, err = json.Marshal(data)
		if err != nil {
			r.Error = &api.Error{Code: "internal", Message: err.Error()}
		}
	}
	if err = json.NewEncoder(os.Stdout).Encode(r); err != nil {
		return 1
	}
	if r.Error != nil {
		return 1
	}
	return 0
}
func decode(r io.Reader) (api.Input, error) {
	b, err := io.ReadAll(io.LimitReader(r, api.MaxMessage+1))
	if err != nil {
		return api.Input{}, err
	}
	if len(b) > api.MaxMessage {
		return api.Input{}, api.Err("invalid_input", "protocol message exceeds 1 MiB")
	}
	var in api.Input
	// Reject both misspelled fields and additional trailing protocol messages.
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&in); err != nil {
		return in, api.Err("invalid_input", "invalid protocol JSON: %v", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return in, api.Err("invalid_input", "trailing protocol data")
	}
	if len(in.Actor) > 128 || len(in.Reason) > 8192 || len(in.Summary) > 8192 || len(in.Metadata) > 32 || in.Cursor < 0 || in.Limit < 0 {
		return in, api.Err("invalid_input", "invalid request fields")
	}
	for k, v := range in.Metadata {
		if len(k) > 128 || len(v) > 4096 {
			return in, api.Err("invalid_input", "metadata too large")
		}
	}
	if !api.ValidSession(in.Session) {
		return in, api.Err("invalid_input", "invalid session")
	}
	if in.Recommend != "" && in.Recommend != "approve" && in.Recommend != "deny" && in.Recommend != "unknown" {
		return in, api.Err("invalid_input", "recommend must be approve, deny or unknown")
	}
	return in, nil
}
func openStore(c Config) (*store.Store, error) {
	owner := uint32(0)
	if c.Dev {
		owner = uint32(os.Getuid())
	}
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", "state.db-journal", "state.db.lock"} {
		st, err := os.Lstat(filepath.Join(c.StateDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		x := st.Sys().(*syscall.Stat_t)
		if !st.Mode().IsRegular() || x.Uid != owner || x.Nlink != 1 || st.Mode().Perm()&0077 != 0 {
			return nil, api.Err("configuration", "unsafe database file: %s", name)
		}
	}
	return store.Open(filepath.Join(c.StateDir, "state.db"))
}

// Main is deliberately a small fixed protocol, not a general root command
// dispatcher. The public CLI and its UI libraries are absent from this binary.
// "rpc" reads one JSON request from stdin; "run ID" and "run-sudo ID" replace
// the CLI process, wait for approval and execute with the caller's terminal.
func Main(args []string) int {
	run := len(args) == 2 && (args[0] == "run" || args[0] == "run-sudo")
	if !run && (len(args) != 1 || args[0] != "rpc") {
		fmt.Fprintln(os.Stderr, "agsudo-helper is an internal program; use agsudo --help")
		return 2
	}
	ui := executionUI{"agsudo", "agsudo help --requester"}
	if args[0] == "run-sudo" {
		ui = executionUI{"sudo", "sudo --help"}
	}
	fail := func(err error) int {
		if run {
			ui.message("%v", err)
			return 1
		}
		return respond(nil, err)
	}
	umask := syscall.Umask(0077)
	c, err := loadConfig()
	if err != nil {
		return fail(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	os.Clearenv()
	if !c.Dev {
		if err = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			return fail(err)
		}
	}
	a, err := c.access(uid)
	if err != nil {
		return fail(err)
	}
	var in api.Input
	if run {
		in.ID = args[1]
	} else if in, err = decode(os.Stdin); err != nil {
		return fail(err)
	}
	if len(in.ID) > 128 {
		return fail(api.Err("invalid_input", "invalid request ID"))
	}
	s, err := openStore(c)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	if run {
		return runRequest(s, c, a, in.ID, umask, ui)
	}
	data, err := dispatch(context.Background(), s, c, a, gid, in)
	return respond(data, err)
}

func dispatch(ctx context.Context, s *store.Store, c Config, a store.Access, gid int, in api.Input) (any, error) {
	switch in.Operation {
	case "info":
		mode := "installed"
		if c.Dev {
			mode = "development (no elevation)"
		}
		return map[string]any{"mode": mode, "caller_uid": a.UID, "can_submit": c.canSubmit(a.UID), "can_review": a.Reviewer, "default_ttl_seconds": c.DefaultTTLSeconds, "max_ttl_seconds": c.MaxTTLSeconds}, nil
	case "create":
		if !c.canSubmit(a.UID) {
			return nil, api.Err("forbidden", "this user cannot submit requests")
		}
		if c.Dev && in.User == "" {
			in.User = fmt.Sprintf("#%d", a.UID)
		}
		spec, err := prepare(in, c, a.UID, gid)
		if err != nil {
			return nil, err
		}
		ttl := in.TTLSeconds
		if ttl == 0 {
			ttl = c.DefaultTTLSeconds
		}
		if ttl < 1 || ttl > c.MaxTTLSeconds {
			return nil, api.Err("invalid_input", "ttl must be between 1 and %d seconds", c.MaxTTLSeconds)
		}
		r := api.Request{ID: api.ID("req_"), OwnerUID: a.UID, OwnerGID: gid, Spec: spec, Detached: in.Detached, Submitter: proc.Identity(os.Getppid()), ExpiresAt: time.Now().UTC().Add(time.Duration(ttl) * time.Second)}
		return s.Create(ctx, r)
	case "get":
		return s.Get(ctx, in.ID, a)
	case "list":
		if err := store.ValidStates(in.States); err != nil {
			return nil, err
		}
		return s.List(ctx, a, in.States, in.Session, in.Cursor, in.Limit)
	case "decide":
		d := api.Decision{Action: in.Action, Actor: api.Actor{UID: a.UID, Label: in.Actor}, Reason: in.Reason, Summary: in.Summary, Recommend: in.Recommend, Metadata: in.Metadata}
		return s.Decide(ctx, in.ID, a, d, store.Check{Digest: in.ExpectDigest, Session: in.Session, PendingOnly: in.PendingOnly})
	case "cancel":
		return s.Cancel(ctx, in.ID, a, in.Reason)
	case "events":
		return s.Events(ctx, a, in.Session, in.ID, in.Cursor, in.Limit)
	default:
		return nil, api.Err("invalid_input", "unknown helper operation %q", in.Operation)
	}
}
