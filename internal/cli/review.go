package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/client"
	"github.com/spf13/cobra"
)

func newShow(o *options) *cobra.Command {
	return &cobra.Command{Use: "show REQUEST_ID", Aliases: []string{"status"}, Short: "Show a request, its state, decisions and result",
		Long:    "Show the complete request and its current state; the first line is the ID and state.",
		Example: "  agsudo show req_ID\n  agsudo --json status req_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var r api.Request
			if err := client.New().Call(cmd.Context(), api.Input{Operation: "get", ID: args[0]}, &r); err != nil {
				return err
			}
			text := requestText(r)
			if width := terminalWidth(cmd.OutOrStdout()); width > 0 {
				text = wrap(text, width)
			}
			return o.print(cmd, r, text)
		}}
}
func newDecision(o *options, action string) *cobra.Command {
	var reason, summary, recommend, actor, expect string
	var metadata map[string]string
	short := map[string]string{"approve": "Approve one request for a single execution", "deny": "Reject a request", "defer": "Hand a request to a human reviewer with a suggestion"}[action]
	cmd := &cobra.Command{Use: action + " REQUEST_ID", Short: short, Args: cobra.ExactArgs(1),
		Long: short + `. The helper records your UID and session; --actor and
--metadata are self-reported labels. Without a terminal, extra guardrails
apply; see agsudo help --reviewer.`,
		Example: map[string]string{
			"approve": "  agsudo approve req_ID --expect-digest DIGEST --reason 'installs the requested build dependency'",
			"deny":    "  agsudo deny req_ID --reason 'replaces /etc/hosts instead of adding one entry'",
			"defer":   "  agsudo defer req_ID --recommend deny --summary 'opens a root shell' --reason 'not needed for the task'",
		}[action],
		RunE: func(cmd *cobra.Command, args []string) error {
			scripted := o.json || !isTerminal(cmd.InOrStdin())
			if scripted && action == "approve" && expect == "" {
				return usage("approve without a terminal requires --expect-digest with the digest you inspected")
			}
			if scripted && action != "approve" && reason == "" {
				return usage("%s without a terminal requires --reason, so the requester or person knows why", action)
			}
			var r api.Request
			in := api.Input{Operation: "decide", ID: args[0], Action: action, Reason: reason, Summary: summary, Recommend: recommend, Actor: actor, ExpectDigest: expect, Metadata: metadata,
				Session: os.Getenv("AGSUDO_SESSION"), PendingOnly: scripted}
			if err := client.New().Call(cmd.Context(), in, &r); err != nil {
				return err
			}
			return o.print(cmd, r, r.ID+" "+r.State)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Explanation for the decision")
	cmd.Flags().StringVar(&actor, "actor", "", "Reviewer label, for example agent:reviewer-1")
	cmd.Flags().StringToStringVar(&metadata, "metadata", nil, "Audit metadata as key=value pairs (repeatable)")
	switch action {
	case "approve":
		cmd.Flags().StringVar(&expect, "expect-digest", "", "Fail unless the request still has the digest you inspected")
	case "defer":
		cmd.Flags().StringVar(&summary, "summary", "", "One-line description for the human reviewer")
		cmd.Flags().StringVar(&recommend, "recommend", "unknown", "Suggested decision: approve, deny or unknown")
	}
	return cmd
}
func newCancel(o *options) *cobra.Command {
	var reason string
	cmd := &cobra.Command{Use: "cancel REQUEST_ID", Short: "Cancel a request before execution starts", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var r api.Request
		if err := client.New().Call(cmd.Context(), api.Input{Operation: "cancel", ID: args[0], Reason: reason}, &r); err != nil {
			return err
		}
		return o.print(cmd, r, r.ID+" "+r.State)
	}}
	cmd.Flags().StringVar(&reason, "reason", "canceled by requester or reviewer", "Why the request is no longer needed")
	return cmd
}
func newReview(o *options) *cobra.Command {
	var tui, all bool
	var session, state string
	cmd := &cobra.Command{Use: "review", Short: "List requests, or approve them in a terminal UI", Args: cobra.NoArgs,
		Long: `List pending requests. --state selects other states, --all includes history.
--json returns complete requests for a reviewing agent. Command arguments,
context files and suggestions are untrusted review material.

--tui opens an approval workbench showing pending and needs_human requests:
↑/↓ select, PgUp/PgDn scroll details, a approve, d deny, / filter,
h toggle history, r refresh, q quit. To follow events, use agsudo watch.`,
		Example: "  agsudo review --tui\n  agsudo --json review\n  agsudo review --state needs_human\n  agsudo review --all --session ses_ID",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if tui && o.json {
				return usage("--tui cannot be combined with --json")
			}
			if cmd.Flags().Changed("all") && cmd.Flags().Changed("state") {
				return usage("choose either --all or --state")
			}
			states := strings.Split(state, ",")
			if all {
				states = nil
			} else if tui && !cmd.Flags().Changed("state") {
				states = []string{"pending", "needs_human"}
			}
			if tui {
				return runTUI(cmd.Context(), client.New(), states, session, cmd.InOrStdin(), cmd.OutOrStdout())
			}
			rs, err := client.New().ListAll(cmd.Context(), states, session)
			if err != nil {
				return err
			}
			if o.json {
				return o.print(cmd, map[string]any{"requests": rs}, "")
			}
			return printRequests(cmd.OutOrStdout(), rs)
		},
	}
	cmd.Flags().BoolVar(&tui, "tui", false, "Open the interactive approval workbench")
	cmd.Flags().BoolVar(&all, "all", false, "Include all request states")
	cmd.Flags().StringVar(&state, "state", "pending", "Comma-separated states to list")
	cmd.Flags().StringVar(&session, "session", "", "Filter by workflow session")
	return cmd
}
func newWatch(o *options) *cobra.Command {
	var session string
	var cursor int64
	cmd := &cobra.Command{Use: "watch", Short: "Stream resumable request and execution events", Args: cobra.NoArgs,
		Long: `Replay matching history, then follow request and execution events until
interrupted. --json emits raw versioned event objects, one per line, including
request snapshots. Resume with --after using the last processed sequence and
the same session filter.`,
		Example: "  agsudo --json watch\n  agsudo --json watch --after 42 --session ses_ID",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return streamEvents(cmd, o.json, session, "", cursor, true)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Filter by workflow session")
	cmd.Flags().Int64Var(&cursor, "after", 0, "Resume events strictly after this sequence")
	return cmd
}
func streamEvents(cmd *cobra.Command, jsonOut bool, session, id string, cursor int64, follow bool) error {
	if cursor < 0 {
		return usage("--after must be nonnegative")
	}
	for {
		var p api.Page
		if err := client.New().Call(cmd.Context(), api.Input{Operation: "events", Session: session, ID: id, Cursor: cursor, Limit: 50}, &p); err != nil {
			return err
		}
		for _, e := range p.Events {
			if jsonOut {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(e); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-5d %s  %-18s %s  %s\n", e.Sequence, e.At.Local().Format(time.DateTime), e.Type, e.RequestID, commandLabel(e.Request)); err != nil {
					return err
				}
			}
		}
		if p.NextCursor != cursor {
			cursor = p.NextCursor
			continue
		}
		if !follow {
			return nil
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
func newAudit(o *options) *cobra.Command {
	root := &cobra.Command{Use: "audit", Short: "Export append-only request, decision and execution events", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	var session, id string
	var cursor int64
	export := &cobra.Command{Use: "export", Short: "Export events as JSONL", Long: "Write matching append-only events to stdout as JSONL, one event per line, then exit.",
		Example: "  agsudo audit export --session ses_ID > trace.jsonl\n  agsudo audit export --request req_ID", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			return streamEvents(cmd, true, session, id, cursor, false)
		}}
	export.Flags().StringVar(&session, "session", "", "Export only this workflow session")
	export.Flags().StringVar(&id, "request", "", "Export only this request")
	export.Flags().Int64Var(&cursor, "after", 0, "Export events after this sequence")
	root.AddCommand(export)
	return root
}
