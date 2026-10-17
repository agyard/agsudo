package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agyard/agsudo/internal/api"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

var plainArgument = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// command renders argv as shell words. Arguments with control or format
// characters use $'...' escapes, so terminals never interpret them.
func command(r api.Request) string {
	words := make([]string, len(r.Spec.Argv))
	for i, s := range r.Spec.Argv {
		switch {
		case plainArgument.MatchString(s):
			words[i] = s
		case api.Safe(s, false) == s:
			words[i] = shellQuote(s)
		default:
			q := strconv.Quote(s)
			words[i] = "$'" + strings.ReplaceAll(q[1:len(q)-1], "'", `\'`) + "'"
		}
	}
	return strings.Join(words, " ")
}

// commandLabel marks shell sessions, whose approval covers everything run in them.
func commandLabel(r api.Request) string {
	if r.Spec.Scope == "shell_session" {
		return "shell: " + command(r)
	}
	return command(r)
}
func when(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05 MST") }
func requestText(r api.Request) string {
	var b strings.Builder
	field := func(label, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%s: %s\n", label, value)
		}
	}
	fmt.Fprintf(&b, "%s  %s\n", r.ID, r.State)
	field("Requester", fmt.Sprintf("UID %d, GID %d", r.OwnerUID, r.OwnerGID))
	field("Command", command(r))
	if r.Spec.Scope == "shell_session" {
		field("Scope", "interactive shell; commands run inside it are not reviewed")
	}
	field("Executable", api.Safe(r.Spec.Executable, false))
	field("Directory", api.Safe(r.Spec.Cwd, false))
	field("Target", fmt.Sprintf("%s (UID %d)", api.Safe(r.Spec.Target.Name, false), r.Spec.Target.UID))
	field("Session", api.Safe(r.Spec.Session, false))
	field("Created", when(r.CreatedAt))
	if r.Unstarted() {
		field("Expires", when(r.ExpiresAt)+" (in "+time.Until(r.ExpiresAt).Round(time.Second).String()+")")
	}
	field("Digest", r.Digest)
	if r.Detached {
		field("Submission", "agsudo request; runs only after agsudo exec --request")
	} else {
		field("Submission", "sudo or agsudo exec; starts on approval if still waiting")
	}
	field("Reason (caller supplied)", api.Safe(r.Spec.Reason, true))
	for _, d := range []*api.Decision{r.Suggestion, r.Decision} {
		if d != nil {
			fmt.Fprintf(&b, "\n%s by UID %d %s at %s\n", d.Action, d.Actor.UID, api.Safe(d.Actor.Label, false), when(d.At))
			field("Summary", api.Safe(d.Summary, true))
			field("Reason", api.Safe(d.Reason, true))
			if d.Action == "defer" {
				field("Recommendation", d.Recommend)
			}
		}
	}
	if r.ExitCode != nil {
		field("Exit status", strconv.Itoa(*r.ExitCode))
	}
	field(map[bool]string{true: "Canceled", false: "Failure"}[r.State == "canceled"], api.Safe(r.Failure, true))
	b.WriteString("\nEnvironment (sanitized):\n")
	for _, value := range r.Spec.Environment {
		fmt.Fprintf(&b, "  %s\n", api.Safe(value, false))
	}
	for _, f := range r.Spec.Context {
		fmt.Fprintf(&b, "\nContext (caller-supplied snapshot from submission): %s\nSHA256: %s\n%s\n", api.Safe(f.Path, false), f.SHA256, api.Safe(f.Content, true))
	}
	return strings.TrimSpace(b.String())
}
func printRequests(out io.Writer, requests []api.Request) error {
	if len(requests) == 0 {
		_, err := fmt.Fprintln(out, "No matching requests.")
		return err
	}
	rows := [][]string{{"REQUEST", "STATE", "TARGET", "COMMAND", "REASON"}}
	for _, r := range requests {
		rows = append(rows, []string{r.ID, r.State, api.Safe(r.Spec.Target.Name, false), commandLabel(r), api.Safe(r.Spec.Reason, false)})
	}
	var w [5]int
	for _, row := range rows {
		for i, cell := range row {
			w[i] = max(w[i], ansi.StringWidth(cell))
		}
	}
	width := math.MaxInt
	if file, ok := out.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		if cols, _, err := term.GetSize(int(file.Fd())); err == nil {
			width = cols
		}
	}
	// IDs, states and targets stay whole; the reason shrinks before the command.
	truncated := false
	if avail := width - w[0] - w[1] - w[2] - 6; w[3]+2+w[4] > avail && avail >= 21 {
		w[4] = max(avail-2-w[3], min(w[4], 12))
		w[3] = avail - 2 - w[4]
		truncated = true
	}
	var b strings.Builder
	for _, row := range rows {
		var line strings.Builder
		for i, cell := range row {
			cell = ansi.Truncate(cell, w[i], "…")
			line.WriteString(cell + strings.Repeat(" ", w[i]-ansi.StringWidth(cell)+2))
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
	}
	if truncated {
		b.WriteString("Use agsudo show REQUEST_ID for full details.\n")
	}
	_, err := io.WriteString(out, b.String())
	return err
}
