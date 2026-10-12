//go:build linux

package helper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/thezzisu/agsudo/internal/api"
	"github.com/thezzisu/agsudo/internal/proc"
	"github.com/thezzisu/agsudo/internal/store"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func authorizationExit(err error) int {
	var interruption *interrupted
	if errors.As(err, &interruption) {
		return 128 + int(interruption.signal)
	}
	var e *api.Error
	if errors.As(err, &e) {
		switch e.Code {
		case "canceled":
			return 126
		case "expired":
			return 124
		case "denied", "pending", "needs_human", "forbidden":
			return 127
		}
	}
	return 125
}
func runRequest(s *store.Store, c Config, a store.Access, in api.Input, ui executionUI) int {
	// Linux parent-death signals refer to the creating thread. Keep that thread
	// alive for the entire monitored execution, even while waiting in Go code.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx := context.Background()
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	parent := proc.Identity(os.Getppid())
	self := proc.Identity(os.Getpid())
	if parent.PID == 0 || self.PID == 0 {
		ui.message("cannot establish process identity")
		return 125
	}
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	if err := waitForApproval(ctx, s, a, in, parent, signals, ui); err != nil {
		ui.message(err.Error())
		return authorizationExit(err)
	}
	// Cancellation and consumption compete in one transaction. After consumption
	// the grant cannot be reused, including after a crash or failed exec.
	r, err := s.ClaimExecution(ctx, in.ID, a, self)
	if err != nil {
		ui.message(err.Error())
		return authorizationExit(err)
	}
	finish := func(code int, failure string) int {
		if _, e := s.Finish(ctx, r.ID, a, self, code, failure); e != nil {
			ui.message("could not persist execution result: " + e.Error())
			return 125
		}
		return code
	}
	f, digest, err := executableFile(r.Spec.Executable)
	if err != nil {
		ui.message("open executable: " + err.Error())
		return finish(125, "open executable: "+err.Error())
	}
	defer f.Close()
	if digest != r.Spec.ExecutableSHA256 {
		ui.message("executable changed after submission; submit a new request")
		return finish(125, "executable changed after submission")
	}
	if !proc.Alive(parent) {
		return finish(126, "execution client disconnected before start")
	}
	select {
	case sig := <-signals:
		return finish(128+int(sig.(syscall.Signal)), "interrupted before start")
	default:
	}
	// ELF programs run from the verified inode. Shebang scripts keep their path
	// for $0 / interpreter resource lookup; their hash check is best effort,
	// not protection against an adversary concurrently replacing script paths.
	cmd := exec.Command("/proc/self/fd/3")
	var header [2]byte
	if n, _ := f.ReadAt(header[:], 0); n == 2 && string(header[:]) == "#!" {
		cmd.Path = r.Spec.Executable
	}
	cmd.Args = append([]string(nil), r.Spec.Argv...)
	if r.Spec.Login {
		cmd.Args[0] = "-" + filepath.Base(r.Spec.Executable)
	}
	cmd.Dir = r.Spec.Cwd
	cmd.Env = append(append([]string(nil), r.Spec.Environment...), "AGSUDO_REQUEST_ID="+r.ID)
	cmd.ExtraFiles = []*os.File{f}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	tty, ttyErr := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	hasTTY := ttyErr == nil
	if hasTTY {
		defer tty.Close()
		oldGroup, _ := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
		oldState, _ := term.GetState(int(tty.Fd()))
		defer func() {
			signal.Ignore(syscall.SIGTTOU)
			if oldGroup > 0 {
				_ = unix.IoctlSetPointerInt(int(tty.Fd()), unix.TIOCSPGRP, oldGroup)
			}
			if oldState != nil {
				_ = term.Restore(int(tty.Fd()), oldState)
			}
			signal.Reset(syscall.SIGTTOU)
		}()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: !hasTTY, Pdeathsig: syscall.SIGKILL}
	if !c.Dev {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: r.Spec.Target.UID, Gid: r.Spec.Target.GID, Groups: r.Spec.Target.Groups}
	}
	if err = cmd.Start(); err != nil {
		ui.message(err.Error())
		return finish(127, err.Error())
	}
	forward := func(sig syscall.Signal) {
		if !hasTTY {
			_ = syscall.Kill(-cmd.Process.Pid, sig)
			return
		}
		fg, e := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
		if e == nil && fg > 0 && fg != syscall.Getpgrp() {
			_ = syscall.Kill(-fg, sig)
		}
		_ = cmd.Process.Signal(sig)
	}
	if _, err = s.Started(ctx, r.ID, a, self, cmd.Process.Pid); err != nil {
		forward(syscall.SIGKILL)
		_ = cmd.Wait()
		return finish(125, "persist start: "+err.Error())
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var killAt time.Time
	for {
		select {
		case err = <-done:
			code := 0
			if err != nil {
				var e *exec.ExitError
				if !errors.As(err, &e) {
					return finish(125, err.Error())
				}
				st := e.Sys().(syscall.WaitStatus)
				if st.Signaled() {
					code = 128 + int(st.Signal())
				} else {
					code = e.ExitCode()
				}
			}
			return finish(code, "")
		case sig := <-signals:
			forward(sig.(syscall.Signal))
		case <-tick.C:
			if !proc.Alive(parent) && killAt.IsZero() {
				forward(syscall.SIGHUP)
				killAt = time.Now().Add(2 * time.Second)
			}
			if !killAt.IsZero() && time.Now().After(killAt) {
				forward(syscall.SIGKILL)
			}
		}
	}
}
