// Package api defines the protocol shared by the CLI and the helper. Both
// binaries are always installed together, so the protocol is unversioned.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const MaxMessage = 1 << 20
const MaxContextFile = 64 << 10
const PageSize = 200 // events per page

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string                   { return e.Message }
func Err(code, format string, args ...any) error { return &Error{code, fmt.Sprintf(format, args...)} }

type Identity struct {
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []uint32 `json:"groups"`
	Name   string   `json:"name"`
	Home   string   `json:"home"`
	Shell  string   `json:"shell"`
}

// File is a snapshot the caller read; the helper never reads it as root.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
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
	Scope            string   `json:"scope"` // command or shell_session
	Login            bool     `json:"login,omitempty"`
}

func Digest(v any) string  { b, _ := json.Marshal(v); return Hash(b) }
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func ID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // Never fails.
	return prefix + hex.EncodeToString(b)
}

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// ValidSession reports whether a session label is acceptable; empty means none.
func ValidSession(s string) bool { return s == "" || sessionPattern.MatchString(s) }

// Safe escapes control and format characters in untrusted text for terminals.
// Multiline text keeps newlines and tabs.
func Safe(s string, multiline bool) string {
	var b strings.Builder
	for _, r := range s {
		if multiline && (r == '\n' || r == '\t') {
			b.WriteRune(r)
		} else if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type Process struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
	Boot  string `json:"boot"`
}
type Decision struct {
	Action    string    `json:"action"`
	UID       int       `json:"uid"`
	Session   string    `json:"session,omitempty"` // The decider's own workflow session.
	At        time.Time `json:"at"`
	Reason    string    `json:"reason,omitempty"`
	Recommend string    `json:"recommend,omitempty"`
}

// Prior is the earlier request for the same command that a request replaces
// or answers, so reviewers see how that one ended.
type Prior struct {
	ID     string `json:"id"`
	State  string `json:"state"` // denied, or replaced
	Reason string `json:"reason,omitempty"`
}
type Request struct {
	ID         string    `json:"id"`
	OwnerUID   int       `json:"owner_uid"`
	Spec       Spec      `json:"spec"`
	Digest     string    `json:"digest"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Executor   Process   `json:"executor,omitzero"`
	CommandPID int       `json:"command_pid,omitempty"`
	Decision   *Decision `json:"decision,omitempty"`
	Suggestion *Decision `json:"suggestion,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Failure    string    `json:"failure,omitempty"`
	Prior      *Prior    `json:"prior,omitempty"`
}

func (r Request) Waiting() bool   { return r.State == "pending" || r.State == "needs_human" }
func (r Request) Unstarted() bool { return r.Waiting() || r.State == "approved" }

// Event records a state change with the request as it was afterwards.
type Event struct {
	Sequence int64   `json:"sequence"`
	Type     string  `json:"type"`
	ActorUID int     `json:"actor_uid"`
	Request  Request `json:"request"`
}
type Page struct {
	Events     []Event `json:"events"`
	NextCursor int64   `json:"next_cursor"`
}

// Input is an internal transport envelope. Identity is never accepted from it.
type Input struct {
	Operation    string   `json:"operation"`
	ID           string   `json:"id,omitempty"`
	Argv         []string `json:"argv,omitempty"`
	Cwd          string   `json:"cwd,omitempty"`
	User         string   `json:"user,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	Context      []File   `json:"context,omitempty"`
	Session      string   `json:"session,omitempty"`
	TTLSeconds   int64    `json:"ttl_seconds,omitempty"`
	Shell        bool     `json:"shell,omitempty"`
	Login        bool     `json:"login,omitempty"`
	ReuseOnly    bool     `json:"reuse_only,omitempty"`
	Action       string   `json:"action,omitempty"`
	Recommend    string   `json:"recommend,omitempty"`
	ExpectDigest string   `json:"expect_digest,omitempty"`
	PendingOnly  bool     `json:"pending_only,omitempty"`
	States       []string `json:"states,omitempty"`
	Cursor       int64    `json:"cursor,omitempty"`
}
type Response struct {
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}
