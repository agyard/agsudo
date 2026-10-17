package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/agyard/agsudo/internal/api"
	"golang.org/x/term"
)

func safe(s string, multiline bool) string {
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
func command(r api.Request) string {
	a := make([]string, len(r.Spec.Argv))
	for i, s := range r.Spec.Argv {
		a[i] = strconv.Quote(s)
	}
	return strings.Join(a, " ")
}
func requestText(r api.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\nRequester: UID %d, GID %d\nCommand: %s\nExecutable: %s\nDirectory: %s\nTarget: %s (UID %d)\nScope: %s\nSession: %s\nExpires: %s\nDigest: %s\n", r.ID, r.State, r.OwnerUID, r.OwnerGID, command(r), safe(r.Spec.Executable, false), safe(r.Spec.Cwd, false), safe(r.Spec.Target.Name, false), r.Spec.Target.UID, r.Spec.Scope, safe(r.Spec.Session, false), r.ExpiresAt.Local().Format("2006-01-02 15:04:05 MST"), r.Digest)
	if r.Detached {
		b.WriteString("Submission: detached (request)\n")
	} else {
		b.WriteString("Submission: foreground (exec / sudo)\n")
	}
	if r.Spec.Reason != "" {
		fmt.Fprintf(&b, "Purpose (caller supplied): %s\n", safe(r.Spec.Reason, true))
	}
	for _, d := range []*api.Decision{r.Suggestion, r.Decision} {
		if d != nil {
			fmt.Fprintf(&b, "\n%s by UID %d %s\nSummary: %s\nReason: %s\nRecommendation: %s\n", d.Action, d.Actor.UID, safe(d.Actor.Label, false), safe(d.Summary, true), safe(d.Reason, true), d.Recommend)
		}
	}
	if r.ExitCode != nil {
		fmt.Fprintf(&b, "Exit status: %d\n", *r.ExitCode)
	}
	if r.Failure != "" {
		fmt.Fprintf(&b, "Failure: %s\n", safe(r.Failure, true))
	}
	b.WriteString("\nEnvironment (sanitized):\n")
	for _, value := range r.Spec.Environment {
		fmt.Fprintf(&b, "  %s\n", safe(value, false))
	}
	for _, f := range r.Spec.Context {
		fmt.Fprintf(&b, "\nContext (caller supplied): %s\nSHA256: %s\n%s\n", safe(f.Path, false), f.SHA256, safe(f.Content, true))
	}
	return strings.TrimSpace(b.String())
}
func printRequests(out io.Writer, requests []api.Request) error {
	if len(requests) == 0 {
		_, err := fmt.Fprintln(out, "No matching requests.")
		return err
	}
	headers := []string{"REQUEST", "STATE", "TARGET", "COMMAND", "PURPOSE"}
	rows := make([][]string, 0, len(requests))
	for _, r := range requests {
		rows = append(rows, []string{r.ID, r.State, r.Spec.Target.Name, command(r), r.Spec.Reason})
	}
	for _, row := range rows {
		for i, s := range row {
			row[i] = safe(s, false)
		}
	}
	// Keep request IDs usable and states/targets unambiguous when narrowing.
	widths := [3]int{}
	for _, row := range append([][]string{headers}, rows...) {
		for i := range widths {
			widths[i] = max(widths[i], lipgloss.Width(row[i])+2)
		}
	}
	t := table.New().Headers(headers...).Rows(rows...).BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).BorderHeader(false).BorderColumn(false).Wrap(false).StyleFunc(func(_, col int) lipgloss.Style {
		s := lipgloss.NewStyle()
		if col < 4 {
			s = s.PaddingRight(2)
		}
		if col < len(widths) {
			s = s.Width(widths[col])
		}
		return s
	})
	text := t.String()
	if file, ok := out.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		minWidth := widths[0] + widths[1] + widths[2] + len("COMMAND") + 2 + len("PURPOSE")
		if width, _, err := term.GetSize(int(file.Fd())); err == nil && width >= minWidth && lipgloss.Width(text) > width {
			text = t.Width(width).String() + "\nUse agsudo show REQUEST_ID for full details."
		}
	}
	_, err := fmt.Fprintln(out, text)
	return err
}
