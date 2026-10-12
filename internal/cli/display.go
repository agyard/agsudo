package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/thezzisu/agsudo/internal/api"
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
	fmt.Fprintf(&b, "%s  %s\nCommand: %s\nExecutable: %s\nDirectory: %s\nTarget: %s (UID %d)\nScope: %s\nSession: %s\nExpires: %s\nDigest: %s\n", r.ID, r.State, command(r), safe(r.Spec.Executable, false), safe(r.Spec.Cwd, false), safe(r.Spec.Target.Name, false), r.Spec.Target.UID, r.Spec.Scope, safe(r.Spec.Session, false), r.ExpiresAt.Local().Format("2006-01-02 15:04:05 MST"), r.Digest)
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
	rows := make([][]string, 0, len(requests))
	for _, r := range requests {
		rows = append(rows, []string{r.ID, r.State, r.Spec.Target.Name, command(r), r.Spec.Reason})
	}
	for _, row := range rows {
		for i, s := range row {
			row[i] = safe(s, false)
		}
	}
	t := table.New().Headers("REQUEST", "STATE", "TARGET", "COMMAND", "PURPOSE").Rows(rows...).BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).BorderHeader(false).BorderColumn(false).Wrap(false).StyleFunc(func(_, col int) lipgloss.Style {
		s := lipgloss.NewStyle()
		if col < 4 {
			s = s.PaddingRight(2)
		}
		return s
	})
	_, err := fmt.Fprintln(out, t.String())
	return err
}
