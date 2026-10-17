// Package store owns the durable request state machine. State changes and their
// audit records commit together; execution claims cannot be consumed twice.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/proc"
	_ "modernc.org/sqlite"
)

const schema = 2

type Access struct {
	UID      int
	Reviewer bool // Includes root.
}

func (a Access) sees(r api.Request) bool { return a.Reviewer || a.UID == r.OwnerUID }

// Owns reports whether a may execute r: only its requester, or root.
func (a Access) Owns(r api.Request) bool { return a.UID == r.OwnerUID || a.UID == 0 }

type Store struct {
	db    *sql.DB
	Now   func() time.Time
	Alive func(api.Process) bool
}

func Open(path string) (*Store, error) {
	if strings.ContainsRune(path, '?') {
		return nil, api.Err("configuration", "state path cannot contain '?'")
	}
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
	// IMMEDIATE transactions avoid the deferred-transaction upgrade race
	// between separate helper processes.
	db, err := sql.Open("sqlite", path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_pragma=temp_store(MEMORY)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err == nil && version == 0 {
		_, err = db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE requests (
 id TEXT PRIMARY KEY, owner_uid INTEGER NOT NULL, session TEXT NOT NULL, state TEXT NOT NULL,
 expires_at INTEGER NOT NULL, command_key TEXT NOT NULL, body BLOB NOT NULL
);
CREATE INDEX requests_state ON requests(state,expires_at);
CREATE INDEX requests_command ON requests(owner_uid,session,command_key);
CREATE TABLE events (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT, owner_uid INTEGER NOT NULL,
 session TEXT NOT NULL, request_id TEXT NOT NULL, body BLOB NOT NULL
);
CREATE INDEX events_owner ON events(owner_uid,sequence);
CREATE INDEX events_session ON events(session,sequence);
CREATE INDEX events_request ON events(request_id,sequence);
PRAGMA user_version=2;`)
	} else if err == nil && version != schema {
		err = api.Err("damaged_state", "state database %s is damaged; delete it", path)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, Now: func() time.Time { return time.Now().UTC() }, Alive: proc.Alive}, nil
}
func (s *Store) Close() error { return s.db.Close() }

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// scan reads rows of (key, JSON body).
func scan[T any](ctx context.Context, q querier, query string, args ...any) ([]T, []int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var items []T
	var keys []int64
	for rows.Next() {
		var key int64
		var b []byte
		var item T
		if err = rows.Scan(&key, &b); err != nil {
			return nil, nil, err
		}
		if err = json.Unmarshal(b, &item); err != nil {
			return nil, nil, err
		}
		items, keys = append(items, item), append(keys, key)
	}
	return items, keys, rows.Err()
}

// scope restricts a query to requests the caller may see, optionally in one session.
func scope(a Access, session string) (string, []any) {
	q, args := "", []any{}
	if !a.Reviewer {
		q, args = " AND owner_uid=?", append(args, a.UID)
	}
	if session != "" {
		q, args = q+" AND session=?", append(args, session)
	}
	return q, args
}

// commandKey identifies what would run: the spec without its explanations.
func commandKey(spec api.Spec) string {
	spec.Reason, spec.Context = "", nil
	return api.Digest(spec)
}

// Business errors returned by fn still commit housekeeping but authorize nothing.
func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // No-op after Commit.
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) save(ctx context.Context, tx *sql.Tx, r *api.Request, kind string, actor int) error {
	r.UpdatedAt = s.Now()
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO requests(id,owner_uid,session,state,expires_at,command_key,body) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET state=excluded.state,body=excluded.body`, r.ID, r.OwnerUID, r.Spec.Session, r.State, r.ExpiresAt.UnixNano(), commandKey(r.Spec), body)
	if err != nil {
		return err
	}
	if body, err = json.Marshal(api.Event{Type: kind, ActorUID: actor, Request: *r}); err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO events(owner_uid,session,request_id,body) VALUES(?,?,?,?)", r.OwnerUID, r.Spec.Session, r.ID, body)
	}
	return err
}

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

// refresh performs lifecycle housekeeping on demand, without a timer service.
// Only expired and running requests can need it.
func (s *Store) refresh(ctx context.Context, tx *sql.Tx, filter string, args ...any) error {
	rs, _, err := scan[api.Request](ctx, tx, "SELECT rowid,body FROM requests WHERE (state='running' OR state IN ('pending','needs_human','approved') AND expires_at<=?)"+filter, append([]any{s.Now().UnixNano()}, args...)...)
	for i := 0; err == nil && i < len(rs); i++ {
		if kind := s.normalize(&rs[i]); kind != "" {
			err = s.save(ctx, tx, &rs[i], kind, 0)
		}
	}
	return err
}

// Create submits a request. With reuseOnly it returns an existing request or
// fails, so sudo -n never asks for a new approval.
func (s *Store) Create(ctx context.Context, r api.Request, reuseOnly bool) (api.Request, error) {
	r.State, r.CreatedAt, r.Digest = "pending", s.Now(), api.Digest(r.Spec)
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.refresh(ctx, tx, " AND owner_uid=?", r.OwnerUID); err != nil {
			return err
		}
		// Running the same command again in the same session attaches to its
		// unused request or gets its recent denial again. Only a new reason or
		// context asks anew: it replaces a pending request or answers a denial,
		// while approvals and deferrals stay.
		matches, _, err := scan[api.Request](ctx, tx, "SELECT rowid,body FROM requests WHERE owner_uid=? AND session=? AND command_key=? AND (state IN ('pending','needs_human','approved') OR state='denied' AND expires_at>?) ORDER BY rowid DESC",
			r.OwnerUID, r.Spec.Session, commandKey(r.Spec), s.Now().UnixNano())
		if err != nil {
			return err
		}
		var replaced, denied *api.Request
		explains := !reuseOnly && (r.Spec.Reason != "" || len(r.Spec.Context) > 0)
		for _, old := range matches {
			same := old.Digest == r.Digest || !explains
			switch {
			case old.Unstarted() && (old.State != "pending" || same), old.State == "denied" && same:
				r = old
				return nil
			case old.State == "pending":
				replaced = &old
			case old.State == "denied" && denied == nil:
				denied = &old
			}
		}
		if reuseOnly {
			return api.Err("pending", "approval required; run the command without -n to ask for it")
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM requests WHERE owner_uid=? AND state IN ('pending','needs_human','approved')", r.OwnerUID).Scan(&count); err != nil {
			return err
		}
		if replaced != nil {
			count--
			r.Prior = &api.Prior{ID: replaced.ID, State: "replaced"}
			replaced.State, replaced.Failure = "canceled", "replaced by "+r.ID
			if err = s.save(ctx, tx, replaced, "request.canceled", r.OwnerUID); err != nil {
				return err
			}
		}
		if denied != nil {
			r.Prior = &api.Prior{ID: denied.ID, State: "denied", Reason: denied.Decision.Reason}
		}
		if count >= 256 {
			return api.Err("queue_full", "at most 256 unconsumed requests per user; cancel stale requests")
		}
		return s.save(ctx, tx, &r, "request.created", r.OwnerUID)
	})
	return r, err
}

func (s *Store) load(ctx context.Context, q querier, id string, a Access) (api.Request, error) {
	rs, _, err := scan[api.Request](ctx, q, "SELECT rowid,body FROM requests WHERE id=?", id)
	if err == nil && (len(rs) == 0 || !a.sees(rs[0])) {
		err = api.Err("not_found", "request %s not found", id)
	}
	if err != nil {
		return api.Request{}, err
	}
	return rs[0], nil
}

// Get reads without the write lock unless housekeeping is due.
func (s *Store) Get(ctx context.Context, id string, a Access) (api.Request, error) {
	r, err := s.load(ctx, s.db, id, a)
	if probe := r; err != nil || s.normalize(&probe) == "" {
		return r, err
	}
	return s.change(ctx, id, a, a.UID, nil)
}

func (s *Store) change(ctx context.Context, id string, a Access, actor int, fn func(*api.Request) (string, error)) (api.Request, error) {
	var r api.Request
	var business error
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		if r, err = s.load(ctx, tx, id, a); err != nil {
			return err
		}
		if kind := s.normalize(&r); kind != "" {
			if err = s.save(ctx, tx, &r, kind, 0); err != nil {
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
			return s.save(ctx, tx, &r, kind, actor)
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	return r, business
}

// Check binds a decision to what the reviewer inspected and who decides.
// These are guardrails against mistakes, not authentication.
type Check struct {
	Digest      string // digest of the inspected request, if given
	Session     string // the decider's own workflow session, if any
	PendingOnly bool   // leave needs_human requests to people
}

func (s *Store) Decide(ctx context.Context, id string, a Access, d api.Decision, check Check) (api.Request, error) {
	if !a.Reviewer {
		return api.Request{}, api.Err("forbidden", "this user is not an approver")
	}
	d.UID, d.At, d.Session = a.UID, s.Now(), check.Session
	return s.change(ctx, id, a, a.UID, func(r *api.Request) (string, error) {
		// A repeat of the same final decision is safe and adds no duplicate event.
		repeat := r.Decision != nil && r.Decision.Action == d.Action && r.Decision.UID == a.UID && (r.State == "approved" || r.State == "denied")
		switch {
		case !r.Waiting() && !repeat:
			state := r.State
			if r.Failure != "" {
				state += " (" + r.Failure + ")"
			}
			return "", api.Err("conflict", "cannot %s %s: it is %s", d.Action, r.ID, state)
		case check.Digest != "" && check.Digest != r.Digest:
			return "", api.Err("conflict", "--expect-digest does not match %s; inspect it again with agsudo show", r.ID)
		case repeat:
			return "", nil
		case check.PendingOnly && r.State == "needs_human":
			return "", api.Err("conflict", "%s was deferred to a person; decide it in agsudo review --tui", r.ID)
		case d.Action == "approve" && check.Session != "" && check.Session == r.Spec.Session:
			return "", api.Err("forbidden", "%s belongs to your own session; ask the user or a reviewer to approve it", r.ID)
		}
		switch d.Action {
		case "approve":
			r.State, r.Decision = "approved", &d
			return "request.approved", nil
		case "deny":
			r.State, r.Decision = "denied", &d
			return "request.denied", nil
		case "defer":
			r.State, r.Suggestion = "needs_human", &d
			return "request.deferred", nil
		}
		return "", api.Err("invalid_input", "unknown decision %q", d.Action)
	})
}
func (s *Store) Cancel(ctx context.Context, id string, a Access, reason string) (api.Request, error) {
	return s.change(ctx, id, a, a.UID, func(r *api.Request) (string, error) {
		if r.State == "canceled" {
			return "", nil
		}
		if !r.Unstarted() {
			return "", api.Err("conflict", "cannot cancel %s: it is %s", r.ID, r.State)
		}
		r.State, r.Failure = "canceled", reason
		return "request.canceled", nil
	})
}
func (s *Store) ClaimExecution(ctx context.Context, id string, a Access, p api.Process) (api.Request, error) {
	return s.change(ctx, id, a, a.UID, func(r *api.Request) (string, error) {
		if !a.Owns(*r) {
			return "", api.Err("forbidden", "only the requester can execute %s", r.ID)
		}
		if r.State != "approved" {
			return "", api.Err(r.State, "%s is %s", r.ID, r.State)
		}
		r.State, r.Executor = "running", p
		return "execution.claimed", nil
	})
}
func (s *Store) Started(ctx context.Context, id string, a Access, p api.Process, pid int) (api.Request, error) {
	return s.executionUpdate(ctx, id, a, p, func(r *api.Request) string { r.CommandPID = pid; return "execution.started" })
}
func (s *Store) Finish(ctx context.Context, id string, a Access, p api.Process, code int, failure string) (api.Request, error) {
	return s.executionUpdate(ctx, id, a, p, func(r *api.Request) string {
		r.ExitCode, r.Failure = &code, failure
		if failure != "" {
			r.State = "failed"
			return "execution.failed"
		}
		r.State = "exited"
		return "execution.exited"
	})
}
func (s *Store) executionUpdate(ctx context.Context, id string, a Access, p api.Process, fn func(*api.Request) string) (api.Request, error) {
	return s.change(ctx, id, a, a.UID, func(r *api.Request) (string, error) {
		if r.State != "running" || r.Executor != p {
			return "", api.Err("conflict", "execution ownership lost")
		}
		return fn(r), nil
	})
}

// List returns the requests the caller may see, oldest first; no states means all.
func (s *Store) List(ctx context.Context, a Access, states []string, session string) ([]api.Request, error) {
	filter, args := scope(a, session)
	var rs []api.Request
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.refresh(ctx, tx, filter, args...); err != nil {
			return err
		}
		q := "SELECT rowid,body FROM requests WHERE 1" + filter
		if len(states) > 0 {
			q += " AND state IN (?" + strings.Repeat(",?", len(states)-1) + ")"
			for _, state := range states {
				args = append(args, state)
			}
		}
		var err error
		rs, _, err = scan[api.Request](ctx, tx, q+" ORDER BY rowid", args...)
		return err
	})
	if rs == nil {
		rs = []api.Request{}
	}
	return rs, err
}

// Events refreshes first, so expiry and lost executions appear without a daemon.
func (s *Store) Events(ctx context.Context, a Access, session, id string, cursor int64) (api.Page, error) {
	filter, args := scope(a, session)
	p := api.Page{Events: []api.Event{}, NextCursor: cursor}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.refresh(ctx, tx, filter, args...); err != nil {
			return err
		}
		q := "SELECT sequence,body FROM events WHERE sequence>?" + filter
		args := append([]any{cursor}, args...)
		if id != "" {
			q, args = q+" AND request_id=?", append(args, id)
		}
		es, keys, err := scan[api.Event](ctx, tx, q+" ORDER BY sequence LIMIT ?", append(args, api.PageSize)...)
		for i := range es {
			es[i].Sequence, p.NextCursor = keys[i], keys[i]
			p.Events = append(p.Events, es[i])
		}
		return err
	})
	return p, err
}
