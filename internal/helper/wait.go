package helper

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/store"
)

const approvalNoticeDelay = 2 * time.Second

// Presentation is chosen by the entry point. Decisions always use the same
// wait loop; no child output is intercepted or rewritten.
type executionUI struct {
	sudo bool
	out  io.Writer
}

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
	fmt.Fprintf(u.out, "%s: %s\n", prefix, api.Safe(fmt.Sprintf(format, args...), false))
}

// waitForApproval returns true once the request is approved. Otherwise it
// reports why the command did not start and returns the exit status.
func waitForApproval(ctx context.Context, s *store.Store, a store.Access, id string, signals <-chan os.Signal, ui executionUI) (int, bool) {
	notice := time.After(approvalNoticeDelay)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		r, err := s.Get(ctx, id, a)
		if err != nil {
			ui.message("%v", err)
			return 1, false
		}
		if a.UID != r.OwnerUID && a.UID != 0 {
			ui.message("only the requester can execute %s", id)
			return 1, false
		}
		switch r.State {
		case "approved":
			return 0, true
		case "pending", "needs_human":
		case "denied":
			reason := "; command was not started"
			if r.Decision != nil && r.Decision.Reason != "" {
				reason = ": " + r.Decision.Reason
			}
			ui.message("%s was denied%s", id, reason)
			return 1, false
		case "expired", "canceled":
			ui.message("%s was %s; command was not started", id, r.State)
			return 1, false
		default:
			ui.message("%s is %s and cannot run again", id, r.State)
			return 1, false
		}
		select {
		case sig := <-signals:
			// A keyboard interrupt means "never mind", as at a sudo password
			// prompt. Timeouts, hangups and kills leave the request resumable.
			if sig == syscall.SIGINT || sig == syscall.SIGQUIT {
				if _, err := s.Cancel(ctx, id, a, "interrupted while waiting for approval"); err != nil {
					ui.message("cancel %s: %v", id, err)
				} else {
					ui.message("canceled %s; command was not started", id)
				}
			} else {
				ui.message("stopped waiting; resume with: agsudo exec --request %s", id)
			}
			return 128 + int(sig.(syscall.Signal)), false
		case <-notice:
			ui.message("waiting for approval of %s; see '%s'", id, ui.help())
		case <-tick.C:
		}
	}
}
