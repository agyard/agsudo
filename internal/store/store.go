// Package store owns the durable request state machine. State changes and their
// audit records commit together; execution claims cannot be consumed twice.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/proc"
	_ "modernc.org/sqlite"
)

type Access struct {
	UID      int
	Reviewer bool
}

func (a Access) sees(r api.Request) bool { return a.UID == 0 || a.Reviewer || a.UID == r.OwnerUID }

type Store struct {
	db    *sql.DB
	Now   func() time.Time
	Alive func(api.Process) bool
}

func Open(path string) (*Store, error) {
	// WAL conversion and schema creation cannot safely race another first
	// connection. flock covers initialization only; SQLite owns normal locking.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL; PRAGMA temp_store=MEMORY;"); err != nil {
		db.Close()
		return nil, err
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 1 {
		db.Close()
		return nil, api.Err("schema_version", "database schema %d is newer than supported schema 1", version)
	}
	if version == 0 {
		_, err = db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS requests (
 id TEXT NOT NULL UNIQUE, owner_uid INTEGER NOT NULL, session TEXT NOT NULL,
 state TEXT NOT NULL, body BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS requests_owner_state ON requests(owner_uid,state);
CREATE TABLE IF NOT EXISTS events (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT, owner_uid INTEGER NOT NULL,
 session TEXT NOT NULL, request_id TEXT NOT NULL, body BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS events_owner ON events(owner_uid,sequence);
CREATE INDEX IF NOT EXISTS events_session ON events(session,sequence);
PRAGMA user_version=1;`)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, Now: func() time.Time { return time.Now().UTC() }, Alive: proc.Alive}, nil
}
func (s *Store) Close() error { return s.db.Close() }

// scope restricts a query to requests the caller may see, optionally in one session.
func scope(a Access, session string) (string, []any) {
	q, args := "", []any{}
	if !a.Reviewer && a.UID != 0 {
		q, args = " AND owner_uid=?", append(args, a.UID)
	}
	if session != "" {
		q, args = q+" AND session=?", append(args, session)
	}
	return q, args
}

func scan(ctx context.Context, c *sql.Conn, q string, args ...any) ([]api.Request, int64, error) {
	rows, err := c.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var rs []api.Request
	var last int64
	for rows.Next() {
		var b []byte
		var r api.Request
		if err = rows.Scan(&last, &b); err != nil {
			return nil, 0, err
		}
		if err = json.Unmarshal(b, &r); err != nil {
			return nil, 0, err
		}
		rs = append(rs, r)
	}
	return rs, last, rows.Err()
}

// refresh performs lifecycle housekeeping on demand, without a timer service.
func (s *Store) refresh(ctx context.Context, c *sql.Conn, filter string, args ...any) error {
	rs, _, err := scan(ctx, c, "SELECT rowid,body FROM requests WHERE state IN ('pending','needs_human','approved','running')"+filter, args...)
	for i := 0; err == nil && i < len(rs); i++ {
		if kind := s.normalize(&rs[i]); kind != "" {
			err = s.save(ctx, c, &rs[i], kind, lifecycle)
		}
	}
	return err
}

// BEGIN IMMEDIATE avoids the deferred-transaction upgrade race between separate
// helper processes. Business errors can commit housekeeping without authorizing.
func (s *Store) transaction(ctx context.Context, fn func(*sql.Conn) error) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err = c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer c.ExecContext(context.Background(), "ROLLBACK") // no-op after COMMIT
	if err = fn(c); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "COMMIT")
	return err
}

func (s *Store) save(ctx context.Context, c *sql.Conn, r *api.Request, kind string, actor api.Actor) error {
	r.Revision++
	r.UpdatedAt = s.Now()
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, `INSERT INTO requests(id,owner_uid,session,state,body) VALUES(?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET state=excluded.state,body=excluded.body`, r.ID, r.OwnerUID, r.Spec.Session, r.State, body)
	if err != nil {
		return err
	}
	event := api.Event{SchemaVersion: api.Version, At: r.UpdatedAt, Type: kind, RequestID: r.ID, OwnerUID: r.OwnerUID, Session: r.Spec.Session, Actor: actor, Request: *r}
	body, err = json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "INSERT INTO events(owner_uid,session,request_id,body) VALUES(?,?,?,?)", r.OwnerUID, r.Spec.Session, r.ID, body)
	return err
}

func (s *Store) Create(ctx context.Context, r api.Request) (api.Request, error) {
	r.SchemaVersion, r.State, r.CreatedAt = api.Version, "pending", s.Now()
	r.Digest = api.Digest(r.Spec)
	err := s.transaction(ctx, func(c *sql.Conn) error {
		if err := s.refresh(ctx, c, " AND owner_uid=?", r.OwnerUID); err != nil {
			return err
		}
		var count int
		if err := c.QueryRowContext(ctx, "SELECT count(*) FROM requests WHERE owner_uid=? AND state IN ('pending','needs_human','approved')", r.OwnerUID).Scan(&count); err != nil {
			return err
		}
		if count >= 256 {
			return api.Err("queue_full", "at most 256 unconsumed requests per user; list and cancel stale requests")
		}
		return s.save(ctx, c, &r, "request.created", api.Actor{UID: r.OwnerUID})
	})
	return r, err
}

var lifecycle = api.Actor{UID: 0, Label: "lifecycle"}

func (s *Store) normalize(r *api.Request) string {
	if r.Unstarted() && !s.Now().Before(r.ExpiresAt) {
		r.State = "expired"
		return "request.expired"
	}
	if r.State == "running" && !s.Alive(r.Executor) {
		r.State = "unknown"
		r.Failure = "execution monitor disappeared; execution will never be retried"
		return "execution.lost"
	}
	return ""
}

func (s *Store) change(ctx context.Context, id string, a Access, actor api.Actor, fn func(*api.Request) (string, error)) (api.Request, error) {
	var r api.Request
	var business error
	err := s.transaction(ctx, func(c *sql.Conn) error {
		var b []byte
		if err := c.QueryRowContext(ctx, "SELECT body FROM requests WHERE id=?", id).Scan(&b); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return api.Err("not_found", "request %s not found", id)
			}
			return err
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if !a.sees(r) {
			return api.Err("not_found", "request %s not found", id)
		}
		if kind := s.normalize(&r); kind != "" {
			if err := s.save(ctx, c, &r, kind, lifecycle); err != nil {
				return err
			}
		}
		if fn == nil {
			return nil
		}
		kind, err := fn(&r)
		if err != nil {
			business = err
			return nil
		}
		if kind != "" {
			return s.save(ctx, c, &r, kind, actor)
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	return r, business
}
func (s *Store) Get(ctx context.Context, id string, a Access) (api.Request, error) {
	return s.change(ctx, id, a, api.Actor{}, nil)
}

func (s *Store) Decide(ctx context.Context, id string, a Access, d api.Decision, expect string) (api.Request, error) {
	if !a.Reviewer && a.UID != 0 {
		return api.Request{}, api.Err("forbidden", "this user is not an approver")
	}
	d.Actor.UID = a.UID
	d.At = s.Now()
	return s.change(ctx, id, a, d.Actor, func(r *api.Request) (string, error) {
		// A repeat of the same final decision is safe and adds no duplicate event.
		repeat := r.Decision != nil && r.Decision.Action == d.Action && r.Decision.Actor.UID == a.UID && (r.State == "approved" || r.State == "denied")
		if !r.Waiting() && !repeat {
			return "", api.Err("conflict", "cannot %s request in state %s", d.Action, r.State)
		}
		if expect != "" && expect != r.Digest {
			return "", api.Err("conflict", "--expect-digest does not match %s; inspect it again with agsudo show", r.ID)
		}
		if repeat {
			return "", nil
		}
		switch d.Action {
		case "approve":
			r.State = "approved"
			r.Decision = &d
			return "request.approved", nil
		case "deny":
			r.State = "denied"
			r.Decision = &d
			return "request.denied", nil
		case "defer":
			r.State = "needs_human"
			r.Suggestion = &d
			return "request.deferred", nil
		default:
			return "", api.Err("invalid_input", "unknown decision %q", d.Action)
		}
	})
}
func (s *Store) Cancel(ctx context.Context, id string, a Access, reason string) (api.Request, error) {
	return s.change(ctx, id, a, api.Actor{UID: a.UID}, func(r *api.Request) (string, error) {
		if r.State == "canceled" {
			return "", nil
		}
		if !r.Unstarted() {
			return "", api.Err("conflict", "cannot cancel request in state %s; cancel only applies before execution", r.State)
		}
		r.State = "canceled"
		r.Failure = reason
		return "request.canceled", nil
	})
}
func (s *Store) ClaimExecution(ctx context.Context, id string, a Access, p api.Process) (api.Request, error) {
	return s.change(ctx, id, a, api.Actor{UID: a.UID}, func(r *api.Request) (string, error) {
		if a.UID != r.OwnerUID && a.UID != 0 {
			return "", api.Err("forbidden", "only the requester can execute this request")
		}
		if r.State != "approved" {
			return "", api.Err(r.State, "request is %s", r.State)
		}
		r.State = "running"
		r.Executor = p
		return "execution.claimed", nil
	})
}
func (s *Store) Started(ctx context.Context, id string, a Access, p api.Process, pid int) (api.Request, error) {
	return s.executionUpdate(ctx, id, a, p, func(r *api.Request) string { r.CommandPID = pid; return "execution.started" })
}
func (s *Store) Finish(ctx context.Context, id string, a Access, p api.Process, code int, failure string) (api.Request, error) {
	return s.executionUpdate(ctx, id, a, p, func(r *api.Request) string {
		r.ExitCode = &code
		r.Failure = failure
		if failure != "" {
			r.State = "failed"
			return "execution.failed"
		}
		r.State = "exited"
		return "execution.exited"
	})
}
func (s *Store) executionUpdate(ctx context.Context, id string, a Access, p api.Process, fn func(*api.Request) string) (api.Request, error) {
	return s.change(ctx, id, a, api.Actor{UID: a.UID}, func(r *api.Request) (string, error) {
		if r.State != "running" || r.Executor != p {
			return "", api.Err("conflict", "execution ownership lost")
		}
		return fn(r), nil
	})
}

func bounded(n int) int {
	if n <= 0 {
		return 50
	}
	return min(n, 200)
}
func (s *Store) List(ctx context.Context, a Access, states []string, session string, cursor int64, limit int) (api.List, error) {
	filter, args := scope(a, session)
	page := api.List{Requests: []api.Request{}, NextCursor: cursor}
	err := s.transaction(ctx, func(c *sql.Conn) error {
		if err := s.refresh(ctx, c, filter, args...); err != nil {
			return err
		}
		q := "SELECT rowid,body FROM requests WHERE rowid>?" + filter
		args := append([]any{cursor}, args...)
		if len(states) > 0 {
			q += " AND state IN (" + strings.TrimSuffix(strings.Repeat("?,", len(states)), ",") + ")"
			for _, state := range states {
				args = append(args, state)
			}
		}
		rs, last, err := scan(ctx, c, q+" ORDER BY rowid LIMIT ?", append(args, bounded(limit))...)
		if len(rs) > 0 {
			page.Requests, page.NextCursor = rs, last
		}
		return err
	})
	return page, err
}

// Events refreshes first, so expiry and lost executions appear without a daemon.
func (s *Store) Events(ctx context.Context, a Access, session, id string, cursor int64, limit int) (api.Page, error) {
	filter, args := scope(a, session)
	p := api.Page{Events: []api.Event{}, NextCursor: cursor}
	err := s.transaction(ctx, func(c *sql.Conn) error {
		if err := s.refresh(ctx, c, filter, args...); err != nil {
			return err
		}
		q := "SELECT sequence,body FROM events WHERE sequence>?" + filter
		args := append([]any{cursor}, args...)
		if id != "" {
			q, args = q+" AND request_id=?", append(args, id)
		}
		rows, err := c.QueryContext(ctx, q+" ORDER BY sequence LIMIT ?", append(args, bounded(limit))...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e api.Event
			var b []byte
			if err = rows.Scan(&p.NextCursor, &b); err != nil {
				return err
			}
			if err = json.Unmarshal(b, &e); err != nil {
				return err
			}
			e.Sequence = p.NextCursor
			p.Events = append(p.Events, e)
		}
		return rows.Err()
	})
	return p, err
}

func ValidStates(states []string) error {
	allowed := strings.Fields("pending needs_human approved running exited denied canceled expired failed unknown")
	for _, s := range states {
		if !slices.Contains(allowed, s) {
			return api.Err("invalid_input", "unknown state %q (choose %s)", s, fmt.Sprint(allowed))
		}
	}
	return nil
}
