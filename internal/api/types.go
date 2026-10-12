// Package api defines the versioned protocol shared by the CLI and helper.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const Version = 1
const MaxMessage = 1024 * 1024

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string                   { return e.Message }
func Err(code, format string, args ...any) error { return &Error{code, fmt.Sprintf(format, args...)} }

type Actor struct {
	UID   int    `json:"uid"`
	Label string `json:"label,omitempty"` // Caller-supplied attribution, not authentication.
}
type Identity struct {
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []uint32 `json:"groups"`
	Name   string   `json:"name"`
	Home   string   `json:"home"`
	Shell  string   `json:"shell"`
}
type File struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
	Source  string `json:"source"` // Always caller-supplied, never a privileged read.
}
type Spec struct {
	Argv             []string `json:"argv"`
	Executable       string   `json:"executable"`
	ExecutableSHA256 string   `json:"executable_sha256"`
	Cwd              string   `json:"cwd"`
	Target           Identity `json:"target"`
	Reason           string   `json:"reason,omitempty"`
	Context          []File   `json:"context,omitempty"`
	Session          string   `json:"session,omitempty"`
	Environment      []string `json:"environment"`
	Scope            string   `json:"scope"` // command or shell_session
	Login            bool     `json:"login,omitempty"`
}

func Digest(v any) string  { b, _ := json.Marshal(v); return Hash(b) }
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func ID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

type Process struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
	Boot  string `json:"boot"`
}
type Decision struct {
	Action    string            `json:"action"`
	Actor     Actor             `json:"actor"`
	At        time.Time         `json:"at"`
	Reason    string            `json:"reason,omitempty"`
	Summary   string            `json:"summary,omitempty"`
	Recommend string            `json:"recommend,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}
type Request struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	OwnerUID      int       `json:"owner_uid"`
	OwnerGID      int       `json:"owner_gid"`
	Spec          Spec      `json:"spec"`
	Digest        string    `json:"digest"`
	State         string    `json:"state"`
	Revision      int       `json:"revision"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Detached      bool      `json:"detached"`
	Submitter     Process   `json:"submitter"`
	Executor      Process   `json:"executor,omitempty"`
	CommandPID    int       `json:"command_pid,omitempty"`
	Decision      *Decision `json:"decision,omitempty"`
	Suggestion    *Decision `json:"suggestion,omitempty"`
	ExitCode      *int      `json:"exit_code,omitempty"`
	Failure       string    `json:"failure,omitempty"`
}

func (r Request) Waiting() bool   { return r.State == "pending" || r.State == "needs_human" }
func (r Request) Unstarted() bool { return r.Waiting() || r.State == "approved" }

type Event struct {
	SchemaVersion int       `json:"schema_version"`
	Sequence      int64     `json:"sequence"`
	At            time.Time `json:"at"`
	Type          string    `json:"type"`
	RequestID     string    `json:"request_id"`
	OwnerUID      int       `json:"owner_uid"`
	Session       string    `json:"session,omitempty"`
	Actor         Actor     `json:"actor"`
	Request       Request   `json:"request"`
}
type Page struct {
	Events     []Event `json:"events"`
	NextCursor int64   `json:"next_cursor"`
}
type List struct {
	Requests   []Request `json:"requests"`
	NextCursor int64     `json:"next_cursor"`
}

// Input is an internal transport envelope. Identity is never accepted from it.
type Input struct {
	Operation      string            `json:"operation"`
	ID             string            `json:"id,omitempty"`
	Argv           []string          `json:"argv,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	User           string            `json:"user,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	Context        []File            `json:"context,omitempty"`
	Session        string            `json:"session,omitempty"`
	Term           string            `json:"term,omitempty"`
	TTLSeconds     int64             `json:"ttl_seconds,omitempty"`
	Detached       bool              `json:"detached,omitempty"`
	Shell          bool              `json:"shell,omitempty"`
	Login          bool              `json:"login,omitempty"`
	NonInteractive bool              `json:"non_interactive,omitempty"`
	Action         string            `json:"action,omitempty"`
	Actor          string            `json:"actor,omitempty"`
	Summary        string            `json:"summary,omitempty"`
	Recommend      string            `json:"recommend,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	ExpectDigest   string            `json:"expect_digest,omitempty"`
	States         []string          `json:"states,omitempty"`
	Cursor         int64             `json:"cursor,omitempty"`
	Limit          int               `json:"limit,omitempty"`
}
type Response struct {
	SchemaVersion int             `json:"schema_version"`
	Data          json.RawMessage `json:"data,omitempty"`
	Error         *Error          `json:"error,omitempty"`
}
