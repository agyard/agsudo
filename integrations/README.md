# Codex and Claude Code integrations

AG-sudo provides one integration per host, installed as user-level hooks:

```sh
agsudo integration install codex
agsudo integration install claude
agsudo integration uninstall codex
agsudo integration uninstall claude
```

Run these commands as a non-root user with AG-sudo installed at a stable path.
The installer records the executable's absolute path; reinstall the integration
after moving the binary. It creates missing configuration or merges into
existing configuration, preserving unrelated fields and hooks. Repeated
installation does not add duplicates. Uninstallation identifies handlers by
their AG-sudo marker and preserves other handlers in the same group.

Before the first change, the installer saves the original file alongside it as
`.agsudo.bak`. Later operations do not overwrite this backup. Invalid JSON,
unsupported hook structures, and symlinked configuration files produce an error
without changing the original file. Installation does not mean the host has
loaded or trusted the hook; restart the host and check its hook configuration.

## Codex

Configuration is installed at `$CODEX_HOME/hooks.json`, or
`~/.codex/hooks.json` when `CODEX_HOME` is unset.

`PreToolUse` matches the host's normalized `Bash` tool, including Codex's
`exec_command`. The hook prepends PATH and `AGSUDO_SESSION` settings to the
original command and preserves other tool arguments. It does not add a nested
shell or parse and replace sudo strings, so sudo calls made through PATH inside
scripts also use AG-sudo.

Codex requires `permissionDecision: allow` for input rewriting. This integration
therefore accepts only hook inputs with `permission_mode: bypassPermissions`
(Full Access). Other modes reject the Bash call to preserve the host's approval
expectations. The installer does not change the sandbox, approval policy, or
hook trust. Review and trust the installed hook through Codex `/hooks`.

Shells can still change PATH, define sudo functions, or invoke absolute paths;
the integration does not cover these cases. Ordinary Bash commands receive the
environment settings, but only actual AG-sudo calls create privilege audit
records.

## Claude Code

Configuration is installed at `$CLAUDE_CONFIG_DIR/settings.json`, or
`~/.claude/settings.json` when `CLAUDE_CONFIG_DIR` is unset.

The `SessionStart` and `CwdChanged` hooks append environment settings to the
host-provided `CLAUDE_ENV_FILE`, which Claude's Bash tool reads before running
commands. Reapplying the settings does not duplicate the same shim at the front
of PATH. The settings are reapplied after a working directory change. There is
no `PermissionRequest` auto-approval hook.

This integration supports local Bash execution on Linux. For remote tasks,
install AG-sudo on the machine that runs the commands; local configuration does
not install it remotely. Installing a hook does not lift a host sandbox's
restriction on setuid execution.

## Sessions and diagnostics

Both integrations export `AGSUDO_SESSION` and make `sudo` and `agsudo` available
through PATH. To list every request in the current session:

```sh
agsudo review --all --json --session "$AGSUDO_SESSION"
```

The internal entry point `agsudo integration hook codex|claude` reads host JSON
without using a model or network access. Hook initialization errors are reported
to the host. Codex may continue with the original command after a general hook
error, or when a hook is disabled or not yet trusted, so hooks are not a security
boundary against a hostile agent.

Documentation was checked against Codex CLI 0.162.1 and Claude Code 2.1.268.
Repository tests exercise official event formats, the installed hook commands,
and their generated shell code. They do not start paid model sessions or claim
coverage of every host version.

Official references: [Codex hooks](https://developers.openai.com/codex/hooks) and
[Claude Code hooks](https://code.claude.com/docs/en/hooks#persist-environment-variables).
