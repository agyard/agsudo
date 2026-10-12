package helper

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/thezzisu/agsudo/internal/api"
	"github.com/thezzisu/agsudo/internal/proc"
	"github.com/thezzisu/agsudo/internal/store"
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
	return "agsudo help requester"
}
func (u executionUI) message(message string) {
	prefix := "agsudo"
	if u.sudo {
		prefix = "sudo"
	}
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '\uFFFD'
		}
		return r
	}, message)
	fmt.Fprintf(u.out, "%s: %s\n", prefix, message)
}

type interrupted struct{ signal syscall.Signal }

func (e *interrupted) Error() string { return "approval canceled; command was not started" }

func approvalStateError(state string) error {
	switch state {
	case "denied":
		return api.Err(state, "permission denied; command was not started")
	case "expired":
		return api.Err(state, "approval expired; command was not started")
	case "canceled":
		return api.Err(state, "approval canceled; command was not started")
	default:
		return api.Err(state, "request is %s; cannot execute again", strconv.Quote(state))
	}
}

func waitForApproval(ctx context.Context, s *store.Store, a store.Access, in api.Input, parent api.Process, signals <-chan os.Signal, ui executionUI) error {
	started := time.Now()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	announced := false
	cancel := func(reason string) {
		if _, err := s.Cancel(ctx, in.ID, a, reason); err != nil {
			ui.message("cancellation: " + err.Error())
		}
	}
	for {
		if !proc.Alive(parent) {
			cancel("execution client disconnected")
			return approvalStateError("canceled")
		}
		select {
		case sig := <-signals:
			cancel("interrupted while waiting for approval")
			return &interrupted{sig.(syscall.Signal)}
		default:
		}
		r, err := s.Get(ctx, in.ID, a)
		if err != nil {
			return err
		}
		if a.UID != r.OwnerUID && a.UID != 0 {
			return api.Err("forbidden", "only the requester can execute this request")
		}
		if r.State == "approved" {
			return nil
		}
		if !r.Waiting() {
			return approvalStateError(r.State)
		}
		if in.NonInteractive {
			cancel("non-interactive execution requires an existing approval")
			return api.Err("pending", "approval required; see '%s'", ui.help())
		}
		// Check after fetching the state: a running command must never produce
		// a pending-approval hint just because it takes longer than two seconds.
		if !announced && time.Since(started) >= approvalNoticeDelay {
			ui.message("approval pending; see '" + ui.help() + "'")
			announced = true
		}
		select {
		case sig := <-signals:
			cancel("interrupted while waiting for approval")
			return &interrupted{sig.(syscall.Signal)}
		case <-ctx.Done():
			cancel("client stopped waiting")
			return approvalStateError("canceled")
		case <-tick.C:
		}
	}
}
