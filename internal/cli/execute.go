package cli

import (
	"context"
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
	user, reason, session, id    string
	files                        []string
	ttl                          time.Duration
	shell, login, nonInteractive bool
}

func (e *executionFlags) flags(f *pflag.FlagSet) {
	f.SetInterspersed(false)
	f.StringVarP(&e.user, "user", "u", "", "Target user; prefix a numeric UID with # (default root)")
	f.StringVar(&e.reason, "reason", os.Getenv("AGSUDO_REASON"), "Purpose of the privilege request")
	f.StringVar(&e.session, "session", os.Getenv("AGSUDO_SESSION"), "Correlate requests within a workflow")
	f.StringArrayVar(&e.files, "context", nil, "Attach a UTF-8 file snapshot (repeatable, read as the caller)")
	f.DurationVar(&e.ttl, "ttl", 0, "Approval validity, e.g. 10m (default configured by administrator)")
	f.BoolVarP(&e.shell, "shell", "s", false, "Request the target user's interactive shell")
	f.BoolVarP(&e.login, "login", "i", false, "Request a login shell in the target user's home")
}
func (e executionFlags) input(args []string, detached bool) (api.Input, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return api.Input{}, err
	}
	if e.ttl < 0 || (e.ttl > 0 && e.ttl < time.Second) {
		return api.Input{}, api.Err("usage", "ttl must be at least one second")
	}
	in := api.Input{Operation: "create", Argv: args, Cwd: cwd, User: e.user, Reason: e.reason, Session: e.session, Term: os.Getenv("TERM"), TTLSeconds: int64(e.ttl / time.Second), Detached: detached, Shell: e.shell, Login: e.login}
	if len(e.files) > 8 {
		return in, api.Err("usage", "at most 8 context files")
	}
	total := 0
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
		if err != nil {
			f.Close()
			return in, err
		}
		if !st.Mode().IsRegular() {
			f.Close()
			return in, api.Err("usage", "context must be a regular file: %s", name)
		}
		b, err := io.ReadAll(io.LimitReader(f, 65537))
		f.Close()
		if err != nil {
			return in, err
		}
		total += len(b)
		if len(b) > 65536 || total > 256<<10 || !utf8.Valid(b) {
			return in, api.Err("usage", "context requires UTF-8 files <= 64 KiB, <= 256 KiB total")
		}
		in.Context = append(in.Context, api.File{Path: path, SHA256: api.Hash(b), Content: string(b), Source: "caller_supplied"})
	}
	return in, nil
}
func newExecute(o *options, detached bool) *cobra.Command {
	var e executionFlags
	name, short := "exec", "Wait for approval and execute once with inherited terminal and streams"
	if detached {
		name, short = "request", "Submit an immutable request without waiting or executing"
	}
	cmd := &cobra.Command{Use: name + " [OPTIONS] -- COMMAND [ARGUMENTS...]", Short: short,
		Long: short + `.

Options precede COMMAND; command arguments are passed verbatim. --context reads
bounded snapshots as the caller. Shell sessions authorize all work inside that
shell; they do not provide per-command review. Target environment is sanitized.
An approved detached request only executes when resumed with exec --request.
Changing an executable after submission requires a new request.`,
		Example: "  agsudo " + name + " --reason 'install build dependencies' -- apt-get install -y libssl-dev\n  agsudo " + name + " -u postgres -- psql\n  agsudo " + name + " -s",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !detached && o.json {
				return usage(cmd, "exec preserves command output; use request/status --json")
			}
			if e.shell && e.login {
				return usage(cmd, "choose either --shell or --login")
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
			} else {
				if len(args) == 0 && !e.shell && !e.login {
					return usage(cmd, "a command or --shell/--login is required")
				}
				in, err := e.input(args, detached)
				if err != nil {
					return err
				}
				var r api.Request
				if err = o.client().Call(cmd.Context(), in, &r); err != nil {
					return err
				}
				e.id = r.ID
				if detached {
					return o.print(cmd, r, r.ID)
				}
			}
			if cmd.Context().Err() != nil {
				_ = o.client().Call(context.Background(), api.Input{Operation: "cancel", ID: e.id, Reason: "client interrupted before waiting"}, nil)
				return cmd.Context().Err()
			}
			return o.client().Run(cmd.Context(), e.id, client.RunOptions{
				NonInteractive: e.nonInteractive,
			}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	e.flags(cmd.Flags())
	if !detached {
		cmd.Long += `

Ctrl-C while waiting cancels the request; after launch it reaches the command.
exec -n cancels an unapproved request instead of waiting.
Before execution: 124 expired, 125 setup failure, 126 canceled, 127 unauthorized.
After launch: the command's exit code, or 128 + signal.`
		cmd.Example += "\n  agsudo exec --request req_ID"
		cmd.Flags().StringVar(&e.id, "request", "", "Resume exactly this request without changing it")
		cmd.Flags().BoolVarP(&e.nonInteractive, "non-interactive", "n", false, "Do not wait; cancel if approval is not already present")
	}
	return cmd
}

func newWrap(o *options) *cobra.Command {
	var session, reason string
	cmd := &cobra.Command{Use: "wrap [OPTIONS] -- COMMAND [ARGUMENTS...]", Short: "Run an Agent with sudo routed through AG-sudo", Args: cobra.MinimumNArgs(1),
		Long: `Create a workflow session and prepend a sudo shim to PATH. The command replaces
the wrapper, preserving its terminal, process lifetime, and exit status. Only sudo
lookups through PATH are intercepted; absolute /usr/bin/sudo and shell overrides
remain outside this mechanism. No ordinary command tracing is performed.`,
		Example: "  agsudo wrap -- your-agent\n  agsudo wrap --reason 'prepare development environment' -- bash",
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.json {
				return usage(cmd, "wrap preserves the command's output and does not support --json")
			}
			dir, err := ensureShim()
			if err != nil {
				return err
			}
			if session == "" {
				session = api.ID("ses_")
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
			helper, err := filepath.Abs(o.helper)
			if err != nil {
				return err
			}
			env := os.Environ()
			env = setEnv(env, "PATH", wrappedPath)
			env = setEnv(env, "AGSUDO_SESSION", session)
			env = setEnv(env, "AGSUDO_HELPER", helper)
			env = setEnv(env, "AGSUDO_REASON", reason)
			return syscall.Exec(path, args, env)
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&session, "session", "", "Use an existing session label")
	cmd.Flags().StringVar(&reason, "reason", "", "Default purpose for this session's requests")
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
	f.SetOutput(errOut)
	var e executionFlags
	e.flags(f)
	var help, version, home bool
	f.BoolVarP(&help, "help", "h", false, "Show supported options")
	f.BoolVarP(&version, "version", "V", false, "Show AG-sudo version")
	f.BoolVarP(&home, "set-home", "H", false, "Set target HOME (always enabled)")
	f.BoolVarP(&e.nonInteractive, "non-interactive", "n", false, "Fail instead of waiting for approval")
	if err := f.Parse(args); err != nil {
		fmt.Fprintln(errOut, "sudo:", err)
		return 2
	}
	if help {
		fmt.Fprintln(out, `sudo (AG-sudo): sudo [-u USER] [-n] [-H] [-s|-i] [--] COMMAND [ARGS...]
Unsupported sudo options fail explicitly.

AG-sudo workflow:
  This sudo may wait for approval; no password is requested.
  Async requests, status, cancellation:
    agsudo help --requester
  Review requests from a person or another Agent:
    agsudo help --reviewer
  Human workbench:
    agsudo review --tui`)
		return 0
	}
	if version {
		fmt.Fprintln(out, "sudo (AG-sudo)", Version)
		return 0
	}
	if (e.shell && e.login) || (len(f.Args()) == 0 && !e.shell && !e.login) {
		fmt.Fprintln(errOut, "sudo: specify a command, -s, or -i; use sudo --help")
		return 2
	}
	helper := os.Getenv("AGSUDO_HELPER")
	if helper == "" {
		helper = client.DefaultHelper
	}
	c := client.Client{Helper: helper}
	req, err := e.input(f.Args(), false)
	if err != nil {
		return reportAs(err, false, errOut, "sudo")
	}
	var r api.Request
	if err = c.Call(ctx, req, &r); err != nil {
		return reportAs(err, false, errOut, "sudo")
	}
	if ctx.Err() != nil {
		_ = c.Call(context.Background(), api.Input{Operation: "cancel", ID: r.ID, Reason: "client interrupted before waiting"}, nil)
		return 130
	}
	return reportAs(c.Run(ctx, r.ID, client.RunOptions{
		NonInteractive: e.nonInteractive,
		Sudo:           true,
	}, in, out, errOut), false, errOut, "sudo")
}
