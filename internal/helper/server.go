package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/store"
	"golang.org/x/sys/unix"
)

func respond(data any, err error) int {
	r := api.Response{Data: data}
	if err != nil {
		var e *api.Error
		if !errors.As(err, &e) {
			e = &api.Error{Code: "internal", Message: err.Error()}
		}
		r = api.Response{Error: e}
	}
	if json.NewEncoder(os.Stdout).Encode(r) != nil || r.Error != nil {
		return 1
	}
	return 0
}
func decode(r io.Reader) (api.Input, error) {
	var in api.Input
	if err := decodeOne(r, api.MaxMessage, &in); err != nil {
		return in, api.Err("invalid_input", "invalid protocol message: %v", err)
	}
	if len(in.Reason) > 8192 || in.Cursor < 0 || !api.ValidSession(in.Session) {
		return in, api.Err("invalid_input", "invalid reason, cursor or session")
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
	// Catch signals first, so even an immediate Ctrl-C cancels the request.
	signals := make(chan os.Signal, 8)
	if run {
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	}
	ui := executionUI{"agsudo", "agsudo exec --help"}
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
	uid, term := os.Getuid(), os.Getenv("TERM")
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
		return runRequest(s, c, a, in.ID, umask, term, signals, ui)
	}
	data, err := dispatch(context.Background(), s, c, a, in)
	return respond(data, err)
}

func dispatch(ctx context.Context, s *store.Store, c Config, a store.Access, in api.Input) (any, error) {
	switch in.Operation {
	case "info":
		mode := "installed"
		if c.Dev {
			mode = "development (no elevation)"
		}
		return map[string]any{"mode": mode, "caller_uid": a.UID, "can_submit": c.canSubmit(a.UID), "can_review": a.Reviewer}, nil
	case "create":
		if !c.canSubmit(a.UID) {
			return nil, api.Err("forbidden", "this user cannot submit requests")
		}
		if c.Dev && in.User == "" {
			in.User = fmt.Sprintf("#%d", a.UID)
		}
		spec, err := prepare(in, c, a.UID)
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
		r := api.Request{ID: api.ID("req_"), OwnerUID: a.UID, Spec: spec, ExpiresAt: time.Now().UTC().Add(time.Duration(ttl) * time.Second)}
		return s.Create(ctx, r, in.ReuseOnly)
	case "get":
		return s.Get(ctx, in.ID, a)
	case "list":
		return s.List(ctx, a, in.States, in.Session)
	case "decide":
		d := api.Decision{Action: in.Action, Reason: in.Reason, Recommend: in.Recommend}
		return s.Decide(ctx, in.ID, a, d, store.Check{Digest: in.ExpectDigest, Session: in.Session, PendingOnly: in.PendingOnly})
	case "cancel":
		return s.Cancel(ctx, in.ID, a, in.Reason)
	case "events":
		return s.Events(ctx, a, in.Session, in.ID, in.Cursor)
	default:
		return nil, api.Err("invalid_input", "unknown helper operation %q", in.Operation)
	}
}
