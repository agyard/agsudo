package cli

import (
	"bufio"
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
	return &cobra.Command{Use: "show REQUEST_ID", Short: "Show a request, its state, decisions and result",
		Long:    "Show the complete request and its current state; the first line is the ID and state.",
		Example: "  agsudo show req_ID\n  agsudo --json show req_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var r api.Request
			if err := client.New().Call(cmd.Context(), api.Input{Operation: "get", ID: args[0]}, &r); err != nil {
				return err
			}
			return o.print(cmd, r, wrap(requestText(r), terminalWidth(cmd.OutOrStdout())))
		}}
}
func newDecision(o *options, action string) *cobra.Command {
	var reason, recommend, expect string
	short := map[string]string{"approve": "Approve one request for a single execution", "deny": "Reject a request", "defer": "Hand a request to a person with a suggestion"}[action]
	cmd := &cobra.Command{Use: action + " REQUEST_ID", Short: short, Args: cobra.ExactArgs(1),
		Long: short + `. The helper records your UID and session.
Without a terminal, guardrails apply; see agsudo review --help.`,
		Example: map[string]string{
			"approve": "  agsudo approve req_ID --expect-digest DIGEST --reason 'installs the requested build dependency'",
			"deny":    "  agsudo deny req_ID --reason 'replaces /etc/hosts instead of adding one entry'",
			"defer":   "  agsudo defer req_ID --recommend deny --reason 'opens a root shell the task does not need'",
		}[action],
		RunE: func(cmd *cobra.Command, args []string) error {
			scripted := o.json || !isTerminal(cmd.InOrStdin())
			if scripted && action == "approve" && expect == "" {
				return usage("approve without a terminal requires --expect-digest with the digest you inspected")
			}
			if scripted && action != "approve" && reason == "" {
				return usage("%s without a terminal requires --reason, so the requester or person knows why", action)
			}
			if action == "approve" && !scripted && expect == "" {
				digest, err := confirmApproval(cmd, args[0])
				if err != nil {
					return err
				}
				expect = digest
			}
			var r api.Request
			in := api.Input{Operation: "decide", ID: args[0], Action: action, Reason: reason, Recommend: recommend, ExpectDigest: expect, Session: os.Getenv("AGSUDO_SESSION"), PendingOnly: scripted}
			if err := client.New().Call(cmd.Context(), in, &r); err != nil {
				return err
			}
			return o.print(cmd, r, r.ID+" "+r.State)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Explanation for the requester or the person")
	switch action {
	case "approve":
		cmd.Long += "\nAt a terminal, approve shows the request and asks for confirmation."
		cmd.Flags().StringVar(&expect, "expect-digest", "", "Fail unless the request still has the digest you inspected")
	case "defer":
		cmd.Flags().StringVar(&recommend, "recommend", "unknown", "Suggested decision: approve, deny or unknown")
	}
	return cmd
}

// confirmApproval shows a person the request before approving it and returns
// the digest of what they saw.
func confirmApproval(cmd *cobra.Command, id string) (string, error) {
	var r api.Request
	if err := client.New().Call(cmd.Context(), api.Input{Operation: "get", ID: id}, &r); err != nil {
		return "", err
	}
	width, prompt := terminalWidth(cmd.OutOrStdout()), "Approve "+approvalTitle(r)
	if width > 0 {
		prompt = elide(prompt, width-len(r.ID)-12)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n%s (%s)? [y/N] ", wrap(requestText(r), width), prompt, r.ID)
	answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if !strings.EqualFold(strings.TrimSpace(answer), "y") {
		return "", api.Err("aborted", "%s was not approved", r.ID)
	}
	return r.Digest, nil
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

// reviewStates is what a list shows: everything, what awaits a person, or
// what a script may decide.
func reviewStates(all, people bool) []string {
	switch {
	case all:
		return nil
	case people:
		return []string{"pending", "needs_human"}
	}
	return []string{"pending"}
}
func newReview(o *options) *cobra.Command {
	var tui, all bool
	var session string
	cmd := &cobra.Command{Use: "review", Short: "List requests, or approve them in a terminal UI", Args: cobra.NoArgs, Long: reviewerHelp,
		Example: "  agsudo review --tui\n  agsudo --json review\n  agsudo review --all --session ses_ID",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if tui && o.json {
				return usage("--tui cannot be combined with --json")
			}
			states := reviewStates(all, tui || !o.json && isTerminal(cmd.InOrStdin()))
			if tui {
				return runTUI(cmd.Context(), client.New(), states, session, cmd.InOrStdin(), cmd.OutOrStdout())
			}
			var rs []api.Request
			if err := client.New().Call(cmd.Context(), api.Input{Operation: "list", States: states, Session: session}, &rs); err != nil {
				return err
			}
			if o.json {
				return o.print(cmd, rs, "")
			}
			return printRequests(cmd.OutOrStdout(), rs)
		},
	}
	cmd.Flags().BoolVar(&tui, "tui", false, "Open the interactive approval workbench")
	cmd.Flags().BoolVar(&all, "all", false, "Include all request states")
	cmd.Flags().StringVar(&session, "session", "", "Filter by workflow session")
	return cmd
}
func newEvents(o *options) *cobra.Command {
	var session, id string
	var cursor int64
	var follow bool
	cmd := &cobra.Command{Use: "events", Short: "Print the audit trail of requests, decisions and executions", Args: cobra.NoArgs,
		Long: `Print recorded events after a sequence, oldest first; --follow keeps waiting
for new ones. With --json, each line is an event with the request as it was
afterwards. Resume with --after and the last sequence you processed.`,
		Example: "  agsudo --json events --session ses_ID > trace.jsonl\n  agsudo events --follow --after 42",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cursor < 0 {
				return usage("--after must be a sequence number")
			}
			out := json.NewEncoder(cmd.OutOrStdout())
			for {
				var p api.Page
				if err := client.New().Call(cmd.Context(), api.Input{Operation: "events", Session: session, ID: id, Cursor: cursor}, &p); err != nil {
					return err
				}
				for _, e := range p.Events {
					var err error
					if o.json {
						err = out.Encode(e)
					} else {
						_, err = fmt.Fprintf(cmd.OutOrStdout(), "%-5d %s  %-18s %s  %s\n", e.Sequence, e.Request.UpdatedAt.Local().Format(time.DateTime), e.Type, e.Request.ID, commandLabel(e.Request))
					}
					if err != nil {
						return err
					}
				}
				cursor = p.NextCursor
				if len(p.Events) == api.PageSize {
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
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep waiting for new events")
	cmd.Flags().Int64Var(&cursor, "after", 0, "Print events after this sequence")
	cmd.Flags().StringVar(&session, "session", "", "Only this workflow session")
	cmd.Flags().StringVar(&id, "request", "", "Only this request")
	return cmd
}
