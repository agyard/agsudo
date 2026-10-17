package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agyard/agsudo/internal/api"
	"github.com/spf13/cobra"
)

func newShow(o *options) *cobra.Command {
	return &cobra.Command{Use: "show REQUEST_ID", Aliases: []string{"status"}, Short: "Inspect a request, its context, suggestion and execution result",
		Long:    "Show the complete request and its current state. The status alias returns the same data, before or after execution.",
		Example: "  agsudo show req_ID\n  agsudo status req_ID --json", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var r api.Request
			if err := o.client().Call(cmd.Context(), api.Input{Operation: "get", ID: args[0]}, &r); err != nil {
				return err
			}
			return o.print(cmd, r, requestText(r))
		}}
}
func newDecision(o *options, action string) *cobra.Command {
	var reason, summary, recommend, actor, expect string
	var metadata map[string]string
	cmd := &cobra.Command{Use: action + " REQUEST_ID", Short: map[string]string{"approve": "Approve exactly one request", "deny": "Reject a request", "defer": "Leave a suggestion and move a request to human review"}[action], Args: cobra.ExactArgs(1),
		Long: `Decisions are explicit actions and work for human and Agent reviewers.
--expect-digest binds the decision to a previously inspected request. A deferred
request is excluded from the default Agent queue; TUI includes human review.
Actor labels and metadata are caller supplied; the helper records the real UID.`,
		Example: "  agsudo " + action + " req_ID --reason 'reviewed command and context' --json",
		RunE: func(cmd *cobra.Command, args []string) error {
			var r api.Request
			in := api.Input{Operation: "decide", ID: args[0], Action: action, Reason: reason, Summary: summary, Recommend: recommend, Actor: actor, ExpectDigest: expect, Metadata: metadata}
			if err := o.client().Call(cmd.Context(), in, &r); err != nil {
				return err
			}
			return o.print(cmd, r, r.ID+" "+r.State)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Explanation for the decision")
	cmd.Flags().StringVar(&summary, "summary", "", "Short description for the human reviewer")
	cmd.Flags().StringVar(&actor, "actor", "", "Reviewer label (for example agent:reviewer-1)")
	cmd.Flags().StringVar(&expect, "expect-digest", "", "Require the digest returned by show/request")
	cmd.Flags().StringToStringVar(&metadata, "metadata", nil, "Research metadata as key=value pairs (repeatable)")
	if action == "defer" {
		cmd.Flags().StringVar(&recommend, "recommend", "unknown", "Suggested decision: approve, deny or unknown")
	}
	return cmd
}
func newCancel(o *options) *cobra.Command {
	var reason string
	cmd := &cobra.Command{Use: "cancel REQUEST_ID", Short: "Cancel a request before execution starts", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var r api.Request
		if err := o.client().Call(cmd.Context(), api.Input{Operation: "cancel", ID: args[0], Reason: reason}, &r); err != nil {
			return err
		}
		return o.print(cmd, r, r.ID+" "+r.State)
	}}
	cmd.Flags().StringVar(&reason, "reason", "canceled by requester or reviewer", "Cancellation reason")
	return cmd
}
func newReview(o *options) *cobra.Command {
	var tui, watch, all bool
	var session, state string
	var cursor int64
	cmd := &cobra.Command{Use: "review", Short: "List requests or open the interactive approval workbench", Args: cobra.NoArgs,
		Long: `List pending requests, or open --tui for pending and needs_human requests.
Use --state to select states, or --all to include history. --json returns the
complete requests for a reviewing Agent. Command arguments, context files and
suggestions are untrusted review material.

For resumable events, use agsudo watch --json. The equivalent review --watch
form is supported; --after applies only to event streams.`,
		Example: "  agsudo review --tui\n  agsudo review --json\n  agsudo review --state needs_human\n  agsudo review --all --session ses_ID",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if tui && (watch || o.json) {
				return usage(cmd, "--tui cannot be combined with --watch or --json")
			}
			if cmd.Flags().Changed("all") && cmd.Flags().Changed("state") {
				return usage(cmd, "choose either --all or --state")
			}
			if cursor < 0 {
				return usage(cmd, "--after must be nonnegative")
			}
			if watch {
				if cmd.Flags().Changed("all") || cmd.Flags().Changed("state") {
					return usage(cmd, "--state and --all apply to lists, not event streams")
				}
				return streamEvents(cmd, o, session, "", cursor, true)
			}
			if cmd.Flags().Changed("after") {
				return usage(cmd, "--after requires --watch; use agsudo watch --after SEQUENCE")
			}
			states := strings.Split(state, ",")
			if all {
				states = nil
			}
			if tui {
				if !cmd.Flags().Changed("state") && !all {
					states = []string{"pending", "needs_human"}
				}
				return runTUI(cmd.Context(), o.client(), states, session, cmd.InOrStdin(), cmd.OutOrStdout())
			}
			rs, err := o.client().ListAll(cmd.Context(), states, session)
			if err != nil {
				return err
			}
			if o.json {
				return o.print(cmd, map[string]any{"requests": rs}, "")
			}
			return printRequests(cmd.OutOrStdout(), rs)
		},
	}
	cmd.Flags().BoolVar(&tui, "tui", false, "Open the Bubble Tea approval workbench")
	cmd.Flags().BoolVar(&watch, "watch", false, "Stream events instead of listing requests (same as agsudo watch)")
	cmd.Flags().BoolVar(&all, "all", false, "Include all request states")
	cmd.Flags().StringVar(&state, "state", "pending", "Comma-separated states to list")
	cmd.Flags().StringVar(&session, "session", "", "Filter by workflow session")
	cmd.Flags().Int64Var(&cursor, "after", 0, "Resume events strictly after this sequence")
	return cmd
}
func newWatch(o *options) *cobra.Command {
	var session string
	var cursor int64
	cmd := &cobra.Command{Use: "watch", Short: "Stream resumable request and execution events", Args: cobra.NoArgs,
		Long: `Replay matching history, then follow request and execution events until
interrupted. Output is concise text; --json emits raw versioned event objects,
one per line, including request snapshots. Resume with --after using the last
processed sequence and the same session filter. For an approval queue, use
agsudo review --json.`,
		Example: "  agsudo watch --json\n  agsudo watch --json --after 42 --session ses_ID",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cursor < 0 {
				return usage(cmd, "--after must be nonnegative")
			}
			return streamEvents(cmd, o, session, "", cursor, true)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Filter by workflow session")
	cmd.Flags().Int64Var(&cursor, "after", 0, "Resume events strictly after this sequence")
	return cmd
}
func streamEvents(cmd *cobra.Command, o *options, session, id string, cursor int64, follow bool) error {
	for {
		var p api.Page
		if err := o.client().Call(cmd.Context(), api.Input{Operation: "events", Session: session, ID: id, Cursor: cursor, Limit: 50}, &p); err != nil {
			return err
		}
		for _, e := range p.Events {
			if o.json {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(e); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d  %s  %s  %s  %s\n", e.Sequence, e.At.Format(time.RFC3339), e.Type, e.RequestID, command(e.Request)); err != nil {
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
	var session, id, format string
	var cursor int64
	export := &cobra.Command{Use: "export", Short: "Export research records as JSONL", Long: "Write matching append-only events to stdout as raw JSONL, then exit. Redirect stdout to save a trace.",
		Example: "  agsudo audit export --session ses_ID > trace.jsonl\n  agsudo audit export --request req_ID", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "jsonl" {
				return usage(cmd, "supported format: jsonl")
			}
			if cursor < 0 {
				return usage(cmd, "--after must be nonnegative")
			}
			copy := *o
			copy.json = true
			return streamEvents(cmd, &copy, session, id, cursor, false)
		}}
	export.Flags().StringVar(&session, "session", "", "Export only this workflow session")
	export.Flags().StringVar(&id, "request", "", "Export only this request")
	export.Flags().StringVar(&format, "format", "jsonl", "Export format")
	_ = export.Flags().MarkHidden("format")
	export.Flags().Int64Var(&cursor, "after", 0, "Export events after this sequence")
	root.AddCommand(export)
	return root
}
