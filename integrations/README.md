# Codex and Claude Code integrations

AG-sudo provides one integration per host, installed as user-level hooks:

```sh
agsudo integration install codex     # or: claude
agsudo integration uninstall codex   # or: claude
```

Run these commands as a non-root user with AG-sudo installed at a stable path.
The installer records the executable's absolute path; reinstall the integration
after moving the binary. It creates missing configuration or merges into
existing configuration, preserving unrelated fields and hooks, and replaces the
file atomically. Repeated installation reports "Already installed" and changes
nothing. Uninstallation identifies handlers by their AG-sudo marker and
preserves other handlers in the same group. Invalid JSON, unsupported hook
structures, and symlinked configuration files produce an error without changing
anything.

Installing does not mean the host has loaded or trusted the hook. Restart the
host and run `sudo --version` in its shell; it prints `sudo (AG-sudo) VERSION`
when routing works.

## Codex

Configuration is installed at `$CODEX_HOME/hooks.json`, or
`~/.codex/hooks.json` when `CODEX_HOME` is unset.

`PreToolUse` matches the host's normalized `Bash` tool, including Codex's
`exec_command`. The hook prepends one line that sets PATH and `AGSUDO_SESSION`
to the original command and preserves other tool arguments. It does not add a
nested shell or rewrite sudo strings, so sudo calls made through PATH inside
scripts also use AG-sudo.

Codex requires `permissionDecision: allow` for input rewriting, so the hook
rewrites commands only when `permission_mode` is `bypassPermissions` (Full
Access). In other modes it leaves commands and Codex's approval policy
unchanged, and sudo is not routed. The installer does not change the sandbox,
approval policy, or hook trust. Review and trust the installed hook through
Codex `/hooks`.

## Claude Code

Configuration is installed at `$CLAUDE_CONFIG_DIR/settings.json`, or
`~/.claude/settings.json` when `CLAUDE_CONFIG_DIR` is unset.

The `SessionStart` and `CwdChanged` hooks append the same PATH and
`AGSUDO_SESSION` line to the host-provided `CLAUDE_ENV_FILE`, which Claude's
Bash tool reads before running commands. On `SessionStart`, the hook also prints
a short note that Claude adds to the model's context, so the agent knows that
sudo waits for approval and how to continue after a tool timeout. There is no
`PermissionRequest` auto-approval hook.

This integration supports local Bash execution on Linux. For remote tasks,
install AG-sudo on the machine that runs the commands. Installing a hook does
not lift a host sandbox's restriction on setuid execution.

## Sessions and limitations

Both integrations export `AGSUDO_SESSION` from the host session ID and make
`sudo` and `agsudo` available through PATH; `agsudo help --requester` shows how
an agent lists its session's requests. When a host times out a waiting sudo, the
request stays pending; once approved, running the same command again in the
session reuses it, including any input piped to it. Inside these sessions an
agent cannot approve its own requests with `agsudo approve`.

Shells can still change PATH, define sudo functions, or invoke absolute paths
such as `/usr/bin/sudo`; the integrations do not cover these cases. Codex may
continue with the original command after a hook error, or when a hook is disabled
or not yet trusted, so hooks are not a security boundary against a hostile agent.
The internal entry point `agsudo integration hook codex|claude` reads host JSON
without using a model or network access.

Documentation was checked against Codex CLI 0.162.1 and Claude Code 2.1.268.
Repository tests exercise official event formats, the installed hook commands,
and their generated shell code. They do not start paid model sessions or claim
coverage of every host version.

Official references: [Codex hooks](https://developers.openai.com/codex/hooks) and
[Claude Code hooks](https://code.claude.com/docs/en/hooks#persist-environment-variables).
