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

	"github.com/agyard/agsudo/internal/proc"
	"github.com/agyard/agsudo/internal/store"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// runRequest replaces the CLI process, so the caller's terminal, signals and
// exit status reach this monitor directly. Like sudo, it exits 1 when the
// command does not start, otherwise with the command's status or 128 + signal.
func runRequest(s *store.Store, c Config, a store.Access, id string, umask int, ui executionUI) int {
	// Linux parent-death signals refer to the creating thread. Keep that thread
	// alive for the entire monitored execution, even while waiting in Go code.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx := context.Background()
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	self := proc.Identity(os.Getpid())
	if self.PID == 0 {
		ui.message("cannot establish process identity")
		return 1
	}
	r, code, ok := waitForApproval(ctx, s, a, id, signals, ui)
	if !ok {
		return code
	}
	// Hash before claiming, so a signal meanwhile still leaves the grant unused.
	f, digest, openErr := executableFile(r.Spec.Executable)
	if f != nil {
		defer f.Close()
	}
	select {
	case sig := <-signals:
		return interrupt(ctx, s, a, id, sig, ui)
	default:
	}
	// Cancellation and consumption compete in one transaction. After consumption
	// the grant cannot be reused, including after a crash or failed exec.
	r, err := s.ClaimExecution(ctx, id, a, self)
	if err != nil {
		ui.message("%v", err)
		return 1
	}
	finish := func(code int, failure string) int {
		if failure != "" {
			ui.message("%s", failure)
		}
		if _, e := s.Finish(ctx, r.ID, a, self, code, failure); e != nil {
			ui.message("could not persist execution result: %v", e)
			return 1
		}
		return code
	}
	if openErr != nil {
		return finish(1, "open executable: "+openErr.Error())
	}
	if digest != r.Spec.ExecutableSHA256 {
		return finish(1, "executable changed after submission; submit a new request")
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
	// With a terminal, the command stays in the caller's job, so job control
	// and keyboard signals reach both. The terminal is reclaimed afterwards
	// from the command's finished jobs, never from another live job.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	hasTTY := err == nil
	if hasTTY {
		defer tty.Close()
		state, _ := term.GetState(int(tty.Fd()))
		defer func() {
			pgrp, e := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
			if e != nil || (pgrp != syscall.Getpgrp() && syscall.Kill(-pgrp, 0) != syscall.ESRCH) {
				return
			}
			signal.Ignore(syscall.SIGTTOU)
			_ = unix.IoctlSetPointerInt(int(tty.Fd()), unix.TIOCSPGRP, syscall.Getpgrp())
			if state != nil {
				_ = term.Restore(int(tty.Fd()), state)
			}
			signal.Reset(syscall.SIGTTOU)
		}()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: !hasTTY, Pdeathsig: syscall.SIGKILL}
	if !c.Dev {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: r.Spec.Target.UID, Gid: r.Spec.Target.GID, Groups: r.Spec.Target.Groups}
	}
	// Like sudo, the command gets the caller's umask, but at least 0022.
	syscall.Umask(umask | 0022)
	err = cmd.Start()
	syscall.Umask(0077)
	if err != nil {
		return finish(1, err.Error())
	}
	// Like sudo, signal only the command; an interactive shell passes hangups
	// on to its own jobs. Keyboard signals already reached the shared job.
	forward := func(sig syscall.Signal) {
		if !hasTTY {
			_ = syscall.Kill(-cmd.Process.Pid, sig)
		} else if sig != syscall.SIGINT && sig != syscall.SIGQUIT {
			_ = cmd.Process.Signal(sig)
		}
	}
	if _, err = s.Started(ctx, r.ID, a, self, cmd.Process.Pid); err != nil {
		forward(syscall.SIGKILL)
		_ = cmd.Wait()
		return finish(1, "persist start: "+err.Error())
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case err = <-done:
			var e *exec.ExitError
			switch {
			case err == nil:
				return finish(0, "")
			case !errors.As(err, &e):
				return finish(1, err.Error())
			case e.Sys().(syscall.WaitStatus).Signaled():
				return finish(128+int(e.Sys().(syscall.WaitStatus).Signal()), "")
			default:
				return finish(e.ExitCode(), "")
			}
		case sig := <-signals:
			forward(sig.(syscall.Signal))
		}
	}
}
