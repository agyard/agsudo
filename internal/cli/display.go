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
	"unicode/utf8"

	"github.com/agyard/agsudo/internal/api"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

var plainArgument = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// command renders argv as shell words. Arguments with invalid UTF-8 or any
// character that is not plainly printable (controls, format characters, line
// separators, non-ASCII spaces) use $'...' escapes, so nothing is hidden.
func command(r api.Request) string {
	words := make([]string, len(r.Spec.Argv))
	for i, s := range r.Spec.Argv {
		switch {
		case plainArgument.MatchString(s):
			words[i] = s
		case utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return !strconv.IsPrint(r) }):
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
	switch {
	case r.Spec.Login:
		return "login shell, authorizes everything in it: " + command(r)
	case r.Spec.Scope == "shell_session":
		return "shell, authorizes everything in it: " + command(r)
	}
	return command(r)
}

// prose renders caller text on one line with single spaces, so neither line
// breaks nor long runs of spaces can make it imitate other fields.
func prose(s string) string { return strings.Join(strings.Fields(api.Safe(s, false)), " ") }

// wrap breaks lines at spaces, never inside a command argument such as
// --no-preserve-root, and marks continuations with ↪, so wrapped untrusted
// text never starts a line the way a field does. Text must be sanitized.
func wrap(text string, width int) string {
	if width <= 0 {
		return text
	}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		body := strings.TrimLeft(line, " │")
		prefix, current := line[:len(line)-len(body)], ""
		next, used := prefix+"↪ ", ansi.StringWidth(prefix)
		for i, word := range strings.Split(body, " ") {
			w := ansi.StringWidth(word)
			switch {
			case i == 0:
				current, used = word, used+w
			case used+1+w > width:
				lines, prefix, current = append(lines, prefix+current), next, word
				used = ansi.StringWidth(next) + w
			default:
				current, used = current+" "+word, used+1+w
			}
			// Hard-wrap an overlong word in one pass.
			if head := ansi.Truncate(current, width-ansi.StringWidth(prefix), ""); used > width && head != "" {
				parts := strings.Split(ansi.Hardwrap(current[len(head):], max(width-ansi.StringWidth(next), 1), true), "\n")
				lines = append(lines, prefix+head)
				for _, part := range parts[:len(parts)-1] {
					lines = append(lines, next+part)
				}
				prefix, current = next, parts[len(parts)-1]
				used = ansi.StringWidth(next + current)
			}
		}
		lines = append(lines, prefix+current)
	}
	return strings.Join(lines, "\n")
}

func isTerminal(f any) bool {
	file, ok := f.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// terminalWidth is the width of out if it is a terminal, otherwise 0.
func terminalWidth(out io.Writer) int {
	if file, ok := out.(*os.File); ok {
		if cols, _, err := term.GetSize(int(file.Fd())); err == nil {
			return cols
		}
	}
	return 0
}

// elide fits s into width cells by cutting its middle, so both the start and
// the end of a command stay visible where a decision is confirmed.
func elide(s string, width int) string {
	if ansi.StringWidth(s) <= width || width < 9 {
		return ansi.Truncate(s, width, "…")
	}
	head := (width - 1) / 2
	return ansi.Truncate(s, head, "") + "…" + ansi.TruncateLeft(s, ansi.StringWidth(s)-(width-1-head), "")
}
func when(t time.Time) string { return t.Local().Format(time.DateTime + " MST") }

// approvalTitle is what a person confirms: any reviewer suggestion, then the
// command with its shell warning.
func approvalTitle(r api.Request) string {
	if d := r.Suggestion; d != nil && d.Recommend != "unknown" {
		return "[suggested: " + d.Recommend + "] " + commandLabel(r)
	}
	return commandLabel(r)
}

// requestText puts what a decision rests on first: the command, its reason,
// and earlier suggestions, decisions or requests for the same command.
func requestText(r api.Request) string {
	var b strings.Builder
	field := func(label, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%s: %s\n", label, api.Safe(value, false))
		}
	}
	fmt.Fprintf(&b, "%s  %s\n", r.ID, r.State)
	field("Command", command(r))
	if r.Spec.Scope == "shell_session" {
		field("Scope", "shell; everything it runs, typed or piped in, is authorized without review")
	}
	field("Reason (caller supplied)", prose(r.Spec.Reason))
	for _, d := range []*api.Decision{r.Suggestion, r.Decision} {
		if d == nil {
			continue
		}
		label, text := "Decision", d.Action
		if d.Action == "defer" {
			label, text = "Suggestion", d.Recommend+", deferred to a person"
		}
		text += fmt.Sprintf(" by UID %d", d.UID)
		if d.Session != "" {
			text += " in session " + d.Session
		}
		text += " at " + when(d.At)
		if d.Reason != "" {
			text += ": " + prose(d.Reason)
		}
		field(label, text)
	}
	if p := r.Prior; p != nil {
		field("Follows", strings.TrimSuffix(p.ID+", "+p.State+": "+prose(p.Reason), ": "))
	}
	field("Target", fmt.Sprintf("%s (UID %d)", r.Spec.Target.Name, r.Spec.Target.UID))
	field("Executable", r.Spec.Executable)
	field("Directory", r.Spec.Cwd)
	field("Requester", fmt.Sprintf("UID %d", r.OwnerUID))
	field("Session", r.Spec.Session)
	field("Created", when(r.CreatedAt))
	if left := time.Until(r.ExpiresAt).Round(time.Second); r.Unstarted() && left > 0 {
		field("Expires", when(r.ExpiresAt)+" (in "+left.String()+")")
	} else if r.Unstarted() {
		field("Expires", when(r.ExpiresAt)+" (expired)")
	}
	field("Digest", r.Digest)
	if r.ExitCode != nil {
		field("Exit status", strconv.Itoa(*r.ExitCode))
	}
	if r.State == "canceled" {
		field("Canceled", prose(r.Failure))
	} else {
		field("Failure", prose(r.Failure))
	}
	for _, f := range r.Spec.Context {
		content := "  │ " + strings.ReplaceAll(strings.TrimSuffix(api.Safe(f.Content, true), "\n"), "\n", "\n  │ ")
		fmt.Fprintf(&b, "\nContext (caller-supplied snapshot from submission): %s\n%s\n", api.Safe(f.Path, false), content)
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
		rows = append(rows, []string{r.ID, r.State, r.Spec.Target.Name, commandLabel(r), prose(r.Spec.Reason)})
	}
	var w [5]int
	for _, row := range rows {
		for i, cell := range row {
			row[i] = api.Safe(cell, false)
			w[i] = max(w[i], ansi.StringWidth(row[i]))
		}
	}
	width := terminalWidth(out)
	if width == 0 {
		width = math.MaxInt
	}
	// IDs, states and targets stay whole; the reason shrinks before the
	// command and is dropped in very narrow terminals.
	truncated := false
	if avail := width - w[0] - w[1] - w[2] - 6; w[3]+2+w[4] > avail {
		truncated = true
		if avail >= 21 {
			w[4] = max(avail-2-w[3], min(w[4], 12))
			w[3] = avail - 2 - w[4]
		} else {
			w[3], w[4] = max(avail, 7), 0
			for i := range rows {
				rows[i] = rows[i][:4]
			}
		}
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
