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
	user, reason, session, id string
	files                     []string
	ttl                       time.Duration
	shell, login              bool
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
func (e executionFlags) input(args []string, detached bool) (api.Input, error) {
	if e.shell && e.login {
		return api.Input{}, api.Err("usage", "choose either --shell or --login")
	}
	if len(args) == 0 && !e.shell && !e.login {
		return api.Input{}, api.Err("usage", "a command, -s/--shell or -i/--login is required")
	}
	if e.ttl < 0 || (e.ttl > 0 && e.ttl < time.Second) {
		return api.Input{}, api.Err("usage", "ttl must be at least one second")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return api.Input{}, err
	}
	in := api.Input{Operation: "create", Argv: args, Cwd: cwd, User: e.user, Reason: e.reason, Session: e.session, Term: os.Getenv("TERM"), TTLSeconds: int64(e.ttl / time.Second), Detached: detached, Shell: e.shell, Login: e.login}
	// Snapshots are read with the caller's permissions; the helper enforces limits.
	for _, name := range e.files {
		path, err := filepath.Abs(name)
		if err != nil {
			return in, err
		}
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return in, err
		}
		st, err := f.Stat()
		var b []byte
		if err == nil && !st.Mode().IsRegular() {
			err = api.Err("usage", "context must be a regular file: %s", name)
		} else if err == nil {
			b, err = io.ReadAll(io.LimitReader(f, 64<<10+1))
		}
		f.Close()
		if err != nil {
			return in, err
		}
		if len(b) > 64<<10 || !utf8.Valid(b) {
			return in, api.Err("usage", "context must be UTF-8 text of at most 64 KiB: %s", name)
		}
		in.Context = append(in.Context, api.File{Path: path, Content: string(b)})
	}
	return in, nil
}

// submit creates the request unless one is being resumed, then replaces this
// process with the helper, which waits for approval and runs it once.
func submit(ctx context.Context, e executionFlags, args []string, sudo bool) error {
	c := client.New()
	if e.id == "" {
		in, err := e.input(args, false)
		if err != nil {
			return err
		}
		var r api.Request
		if err = c.Call(ctx, in, &r); err != nil {
			return err
		}
		e.id = r.ID
		if ctx.Err() != nil {
			_ = c.Call(context.Background(), api.Input{Operation: "cancel", ID: e.id, Reason: "interrupted while submitting"}, nil)
			return ctx.Err()
		}
	}
	return c.Exec(e.id, sudo)
}

func newExecute(o *options, detached bool) *cobra.Command {
	var e executionFlags
	name, short := "exec", "Wait for approval, then run a command once"
	if detached {
		name, short = "request", "Submit a request without waiting; prints its ID"
	}
	cmd := &cobra.Command{Use: name + " [OPTIONS] -- COMMAND [ARGUMENTS...]", Short: short,
		Long: short + `.

Options precede COMMAND; command arguments are passed verbatim. --context reads
bounded snapshots as the caller; they are not re-read at execution. Approving a
shell (-s/-i or a bare shell) authorizes everything run inside it. The target
environment is sanitized. Changing the executable after submission requires a
new request.`,
		Example: "  agsudo " + name + " --reason 'install build dependencies' -- dnf install -y openssl-devel\n  agsudo " + name + " -u postgres -- psql\n  agsudo " + name + " -s",
		RunE: func(cmd *cobra.Command, args []string) error {
			if detached {
				in, err := e.input(args, true)
				if err != nil {
					return usageError(cmd, err)
				}
				var r api.Request
				if err = o.client().Call(cmd.Context(), in, &r); err != nil {
					return err
				}
				return o.print(cmd, r, r.ID)
			}
			if o.json {
				return usage(cmd, "exec preserves command output; use request/status --json")
			}
			if cmd.Flags().Changed("request") {
				if e.id == "" {
					return usage(cmd, "--request requires a nonempty request ID")
				}
				if len(args) != 0 {
					return usage(cmd, "--request cannot replace any part of a submitted request")
				}
				for _, flag := range []string{"user", "context", "shell", "login", "reason", "session", "ttl"} {
					if cmd.Flags().Changed(flag) {
						return usage(cmd, "--request cannot be combined with --%s", flag)
					}
				}
			}
			return usageError(cmd, submit(cmd.Context(), e, args, false))
		},
	}
	e.flags(cmd.Flags())
	if !detached {
		cmd.Long += `

exec works like the sudo shim; see agsudo help --requester. Ctrl-C while
waiting cancels the request. If the wait ends any other way, the request stays
pending; running the same command again, or exec --request ID, resumes it.`
		cmd.Example += "\n  agsudo exec --request req_ID"
		cmd.Flags().StringVar(&e.id, "request", "", "Wait for and run this submitted request")
	}
	return cmd
}

// usageError adds command help to CLI-side validation errors.
func usageError(cmd *cobra.Command, err error) error {
	var e *api.Error
	if errors.As(err, &e) && e.Code == "usage" {
		return usage(cmd, "%s", e.Message)
	}
	return err
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
				return usage(cmd, "wrap preserves the command's output and does not support --json")
			}
			if session == "" {
				session = api.ID("ses_")
			}
			if !api.ValidSession(session) {
				return usage(cmd, "session label %q (from --session or AGSUDO_SESSION) must be 1-128 letters, digits or _.:-", session)
			}
			dir, err := ensureShim()
			if err != nil {
				return err
			}
			// Resolve the wrapped command through the new PATH too, so wrapping
			// sudo itself is covered as well as sudo calls in its descendants.
			wrappedPath := dir + ":" + os.Getenv("PATH")
			if err = os.Setenv("PATH", wrappedPath); err != nil {
				return err
			}
			path, err := exec.LookPath(args[0])
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
			env := os.Environ()
			env = setEnv(env, "AGSUDO_SESSION", session)
			env = setEnv(env, "AGSUDO_HELPER", helper)
			return syscall.Exec(path, args, env)
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&session, "session", os.Getenv("AGSUDO_SESSION"), "Session label (default: inherited or new)")
	return cmd
}

func ensureShim() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
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

func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, v := range env {
		if !strings.HasPrefix(v, key+"=") {
			out = append(out, v)
		}
	}
	return append(out, key+"="+value)
}

// sudo compatibility uses pflag, with option parsing stopping at COMMAND.
func sudo(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	f := pflag.NewFlagSet("sudo", pflag.ContinueOnError)
	f.SetOutput(io.Discard)
	var e executionFlags
	e.flags(f)
	var help, version, home, nonInteractive, reset bool
	f.BoolVarP(&help, "help", "h", false, "")
	f.BoolVarP(&version, "version", "V", false, "")
	f.BoolVarP(&home, "set-home", "H", false, "")
	f.BoolVarP(&nonInteractive, "non-interactive", "n", false, "")
	f.BoolVarP(&reset, "reset-timestamp", "k", false, "")
	if err := f.Parse(args); err != nil {
		return report(api.Err("usage", "%v; see 'sudo --help'", err), false, errOut, "sudo")
	}
	switch {
	case help:
		fmt.Fprintln(out, requesterHelp)
		return 0
	case version:
		fmt.Fprintln(out, "sudo (AG-sudo)", Version)
		return 0
	case reset && len(f.Args()) == 0 && !e.shell && !e.login:
		return 0 // There are no cached credentials to forget.
	case nonInteractive:
		fmt.Fprintln(errOut, "sudo: approval required; -n does not wait (submit with 'agsudo request', see 'sudo --help')")
		return 1
	}
	err := submit(ctx, e, f.Args(), true)
	var usageErr *api.Error
	if errors.As(err, &usageErr) && usageErr.Code == "usage" {
		err = api.Err("usage", "%s; see 'sudo --help'", usageErr.Message)
	}
	return report(err, false, errOut, "sudo")
}
