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
type executionUI struct{ prefix, help string }

func (u executionUI) message(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", u.prefix, api.Safe(fmt.Sprintf(format, args...), false))
}

// interrupt ends a wait. A keyboard interrupt means "never mind", as at a sudo
// password prompt; timeouts, hangups and kills leave the request to be run again.
func interrupt(ctx context.Context, s *store.Store, a store.Access, r api.Request, sig os.Signal, ui executionUI) int {
	switch {
	case sig == syscall.SIGINT || sig == syscall.SIGQUIT:
		if _, err := s.Cancel(ctx, r.ID, a, "interrupted while waiting for approval"); err != nil {
			ui.message("cancel %s: %v", r.ID, err)
		} else {
			ui.message("canceled %s; command was not started", r.ID)
		}
	case r.State == "approved":
		ui.message("stopped before starting %s; it is still approved, so run the same command again to run it.", r.ID)
	default:
		ui.message("stopped waiting; %s stays pending until it expires. Once it is approved (agsudo review --tui), run the same command again.", r.ID)
	}
	return 128 + int(sig.(syscall.Signal))
}

// waitForApproval returns the approved request with status 0. Otherwise it
// reports why the command did not start and returns the exit status.
func waitForApproval(ctx context.Context, s *store.Store, a store.Access, id string, signals <-chan os.Signal, ui executionUI) (api.Request, int) {
	notice := time.After(approvalNoticeDelay)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		r, err := s.Get(ctx, id, a)
		problem := ""
		switch {
		case err != nil:
			problem = err.Error()
		case a.UID != r.OwnerUID && a.UID != 0:
			problem = "only the requester can execute " + id
		case r.State == "approved":
			return r, 0
		case r.State == "denied" && r.Decision != nil && r.Decision.Reason != "":
			problem = id + " was denied: " + r.Decision.Reason
		case r.State == "denied", r.State == "expired", r.State == "canceled":
			problem = id + " is " + r.State + "; command was not started"
		case r.State == "running":
			problem = id + " is already running in another process"
		case !r.Waiting():
			problem = id + " has already run (" + r.State + "); run the command again to submit a new request"
		}
		if problem != "" {
			ui.message("%s", problem)
			return r, 1
		}
		select {
		case sig := <-signals:
			return r, interrupt(ctx, s, a, r, sig, ui)
		case <-notice:
			ui.message("waiting for approval of %s; see '%s'", id, ui.help)
		case <-tick.C:
		}
	}
}
