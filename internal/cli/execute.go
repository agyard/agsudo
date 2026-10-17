package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/client"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type executionFlags struct {
	user, reason, session        string
	files                        []string
	ttl                          time.Duration
	shell, login, nonInteractive bool
}

func (e *executionFlags) flags(f *pflag.FlagSet) {
	f.SetInterspersed(false)
	f.StringVarP(&e.user, "user", "u", "", "Target user; prefix a numeric UID with # (default root)")
	f.StringVar(&e.reason, "reason", "", "Why the command needs privileges, shown to the reviewer")
	f.StringVar(&e.session, "session", os.Getenv("AGSUDO_SESSION"), "Correlate requests within a workflow")
	f.StringArrayVar(&e.files, "context", nil, "Attach a UTF-8 file snapshot for the reviewer (repeatable)")
	f.DurationVar(&e.ttl, "ttl", 0, "Approval validity, e.g. 10m (default configured by administrator)")
	f.BoolVarP(&e.shell, "shell", "s", false, "Request the target user's interactive shell")
	f.BoolVarP(&e.login, "login", "i", false, "Request a login shell in the target user's home")
}

// create submits a request, or returns the matching one already submitted.
func (e executionFlags) create(ctx context.Context, args []string) (api.Request, error) {
	var r api.Request
	switch {
	case e.shell && e.login:
		return r, usage("choose either --shell or --login")
	case len(args) == 0 && !e.shell && !e.login:
		return r, usage("a command, -s/--shell or -i/--login is required")
	case e.ttl < 0 || (e.ttl > 0 && e.ttl < time.Second):
		return r, usage("ttl must be at least one second")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return r, err
	}
	in := api.Input{Operation: "create", Argv: args, Cwd: cwd, User: e.user, Reason: e.reason, Session: e.session, TTLSeconds: int64(e.ttl / time.Second), Shell: e.shell, Login: e.login, ReuseOnly: e.nonInteractive}
	// Snapshots are read with the caller's permissions, never by the helper.
	for _, name := range e.files {
		path, err := filepath.Abs(name)
		if err != nil {
			return r, err
		}
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return r, err
		}
		st, err := f.Stat()
		var b []byte
		if err == nil && !st.Mode().IsRegular() {
			err = usage("context must be a regular file: %s", name)
		} else if err == nil {
			b, err = io.ReadAll(io.LimitReader(f, api.MaxContextFile+1))
		}
		f.Close()
		if err != nil {
			return r, err
		}
		if len(b) > api.MaxContextFile || !utf8.Valid(b) {
			return r, usage("context must be UTF-8 text of at most 64 KiB: %s", name)
		}
		in.Context = append(in.Context, api.File{Path: path, Content: string(b)})
	}
	err = client.New().Call(ctx, in, &r)
	return r, err
}

// submit creates or reuses the request, then replaces this process with the
// helper, which waits for approval and runs it once. Like sudo -n,
// nonInteractive runs only an approved request and never asks.
func submit(ctx context.Context, e executionFlags, args []string, sudo bool) error {
	r, err := e.create(ctx, args)
	if err != nil {
		return err
	}
	if e.nonInteractive && r.Waiting() {
		return api.Err("pending", "%s is %s; -n does not wait. Once it is approved, run the same command again", r.ID, r.State)
	}
	return client.New().Exec(r.ID, sudo)
}

func newExecute(o *options, submitOnly bool) *cobra.Command {
	var e executionFlags
	cmd := &cobra.Command{Use: "exec [OPTIONS] -- COMMAND [ARGUMENTS...]", Short: "Wait for approval, then run a command once", Long: requesterHelp,
		Example: "  agsudo exec --reason 'install build dependencies' -- dnf install -y openssl-devel\n  agsudo exec -u postgres -- psql\n  agsudo exec -s",
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.json {
				return usage("exec preserves command output; use request or show with --json")
			}
			return submit(cmd.Context(), e, args, false)
		},
	}
	if submitOnly {
		cmd.Use, cmd.Short = "request [OPTIONS] -- COMMAND [ARGUMENTS...]", "Submit a request without waiting; prints its ID"
		cmd.Long = cmd.Short + ". Once it is approved, run the same command with sudo or agsudo exec;\nsee agsudo exec --help."
		cmd.Example = "  ID=$(agsudo request --reason 'restart the app' -- systemctl restart app.service)"
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			r, err := e.create(cmd.Context(), args)
			if err != nil {
				return err
			}
			return o.print(cmd, r, r.ID)
		}
	}
	e.flags(cmd.Flags())
	return cmd
}

func newWrap(o *options) *cobra.Command {
	var session string
	cmd := &cobra.Command{Use: "wrap [OPTIONS] -- COMMAND [ARGUMENTS...]", Short: "Run an agent with sudo routed through AG-sudo", Args: cobra.MinimumNArgs(1),
		Long: `Run COMMAND with a sudo shim first in PATH and AGSUDO_SESSION set, so its sudo
calls are approved through AG-sudo and grouped in one session. The command
replaces the wrapper, keeping its terminal, process lifetime and exit status.
Only sudo found through PATH is covered; /usr/bin/sudo and shell overrides are
not. Ordinary commands are not traced.`,
		Example: "  agsudo wrap -- your-agent\n  agsudo wrap -- bash",
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.json {
				return usage("wrap preserves the command's output and does not support --json")
			}
			if session == "" {
				session = api.ID("ses_")
			}
			if !api.ValidSession(session) {
				return usage("session label %q (from --session or AGSUDO_SESSION) must be 1-128 letters, digits or _.:-", session)
			}
			dir, err := ensureShim()
			if err != nil {
				return err
			}
			helper, err := exec.LookPath(client.New().Helper)
			if err == nil {
				helper, err = filepath.Abs(helper)
			}
			if err != nil {
				return err
			}
			// Resolve the wrapped command through the new PATH too, so wrapping
			// sudo itself is covered as well as sudo calls in its descendants.
			os.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			os.Setenv("AGSUDO_SESSION", session)
			os.Setenv("AGSUDO_HELPER", helper)
			path, err := exec.LookPath(args[0])
			if err != nil {
				return err
			}
			return syscall.Exec(path, args, os.Environ())
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&session, "session", os.Getenv("AGSUDO_SESSION"), "Session label (default: inherited or new)")
	return cmd
}

// selfPath is the resolved path of this binary, for shims and host hooks.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	return exe, err
}

func ensureShim() (string, error) {
	exe, err := selfPath()
	if err != nil {
		return "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "agsudo", "shim-"+api.Hash([]byte(exe))[:16])
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, ":\n\r") {
		return "", api.Err("configuration", "cache path cannot be used as a PATH entry")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	for _, name := range []string{"sudo", "agsudo"} {
		shim := filepath.Join(dir, name)
		if err = os.Symlink(exe, shim); err != nil {
			target, e := os.Readlink(shim)
			if e != nil || target != exe {
				return "", api.Err("shim_conflict", "unexpected file at %s", shim)
			}
		}
	}
	return dir, nil
}

// sudo compatibility uses pflag, with option parsing stopping at COMMAND.
func sudo(ctx context.Context, args []string, out, errOut io.Writer) int {
	f := pflag.NewFlagSet("sudo", pflag.ContinueOnError)
	f.SetOutput(io.Discard)
	var e executionFlags
	e.flags(f)
	var help, version, home, reset bool
	var preserve string
	f.BoolVarP(&help, "help", "h", false, "")
	f.BoolVarP(&version, "version", "V", false, "")
	f.BoolVarP(&home, "set-home", "H", false, "")
	f.BoolVarP(&e.nonInteractive, "non-interactive", "n", false, "")
	f.BoolVarP(&reset, "reset-timestamp", "k", false, "")
	f.StringVarP(&preserve, "preserve-env", "E", "", "")
	f.Lookup("preserve-env").NoOptDefVal = "all"
	err := f.Parse(args)
	switch {
	case err != nil:
		err = usage("%v", err)
	case f.Changed("preserve-env"):
		err = usage("the environment is always sanitized; pass variables with: sudo env NAME=VALUE COMMAND")
	case help:
		fmt.Fprintln(out, requesterHelp)
	case version:
		fmt.Fprintln(out, "sudo (AG-sudo)", Version)
	case reset && len(f.Args()) == 0 && !e.shell && !e.login:
		// There are no cached credentials to forget.
	default:
		err = submit(ctx, e, f.Args(), true)
	}
	var usageErr *api.Error
	if errors.As(err, &usageErr) && usageErr.Code == "usage" {
		usageErr.Message += "; see 'sudo --help'"
	}
	return report(err, false, errOut, "sudo")
}
