package helper

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/store"
)

const approvalNoticeDelay = 2 * time.Second

// Presentation is chosen by the entry point. Decisions always use the same
// wait loop; no child output is intercepted or rewritten.
type executionUI struct{ sudo bool }

func (u executionUI) help() string {
	if u.sudo {
		return "sudo --help"
	}
	return "agsudo help --requester"
}
func (u executionUI) message(format string, args ...any) {
	prefix := "agsudo"
	if u.sudo {
		prefix = "sudo"
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", prefix, api.Safe(fmt.Sprintf(format, args...), false))
}

// interrupt ends a wait. A keyboard interrupt means "never mind", as at a sudo
// password prompt; timeouts, hangups and kills leave the request to be run again.
func interrupt(ctx context.Context, s *store.Store, a store.Access, id string, sig os.Signal, ui executionUI) int {
	if sig == syscall.SIGINT || sig == syscall.SIGQUIT {
		if _, err := s.Cancel(ctx, id, a, "interrupted while waiting for approval"); err != nil {
			ui.message("cancel %s: %v", id, err)
		} else {
			ui.message("canceled %s; command was not started", id)
		}
	} else {
		ui.message("stopped waiting; %s stays pending until it expires. Once it is approved, run the same command again", id)
	}
	return 128 + int(sig.(syscall.Signal))
}

// waitForApproval returns the approved request. Otherwise it reports why the
// command did not start and returns the exit status.
func waitForApproval(ctx context.Context, s *store.Store, a store.Access, id string, signals <-chan os.Signal, ui executionUI) (api.Request, int, bool) {
	notice := time.After(approvalNoticeDelay)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		r, err := s.Get(ctx, id, a)
		if err != nil {
			ui.message("%v", err)
			return r, 1, false
		}
		if a.UID != r.OwnerUID && a.UID != 0 {
			ui.message("only the requester can execute %s", id)
			return r, 1, false
		}
		switch r.State {
		case "approved":
			return r, 0, true
		case "pending", "needs_human":
		case "denied":
			reason := "; command was not started"
			if r.Decision != nil && r.Decision.Reason != "" {
				reason = ": " + r.Decision.Reason
			}
			ui.message("%s was denied%s", id, reason)
			return r, 1, false
		case "expired":
			ui.message("%s expired; command was not started", id)
			return r, 1, false
		case "canceled":
			ui.message("%s was canceled; command was not started", id)
			return r, 1, false
		default:
			ui.message("%s has already run (%s); run the command again to submit a new request", id, r.State)
			return r, 1, false
		}
		select {
		case sig := <-signals:
			return r, interrupt(ctx, s, a, id, sig, ui), false
		case <-notice:
			ui.message("waiting for approval of %s; see '%s'", id, ui.help())
		case <-tick.C:
		}
	}
}
