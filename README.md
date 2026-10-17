# AG-sudo

[![CI](https://github.com/agyard/agsudo/actions/workflows/ci.yml/badge.svg)](https://github.com/agyard/agsudo/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/agyard/agsudo)](https://github.com/agyard/agsudo/releases/latest)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Platform: Linux](https://img.shields.io/badge/platform-Linux-blue)](#requirements)

**Privilege approval for agent workflows on Linux.**

Let agents work with ordinary user permissions while a person or a separate
reviewing agent approves privilege requests. AG-sudo routes `sudo` calls through
an approval workflow, then runs each approved request at most once.

- **Familiar execution:** a `sudo` PATH shim preserves command streams, terminal
  interaction, signals, and exit status.
- **Human or agent review:** use a terminal UI or a versioned JSON CLI. No built-in
  model, model credentials, or agent loop is required.
- **Asynchronous requests:** submit, inspect, approve, defer, or cancel requests
  while the requesting agent continues other work.
- **Daemonless operation:** an unprivileged CLI and an on-demand setuid helper
  use SQLite for request state and audit events.
- **Research-ready traces:** export privilege requests, context, decisions, and
  execution results as JSONL.

[Get started](#quick-start) · [Agent workflows](#agent-workflows) ·
[Security model](#security-model) · [Documentation](#documentation)

## Requirements

- Linux with local user accounts. Release archives support **amd64** and **arm64**.
- Root access for installation and an environment that permits setuid execution.
  `nosuid` mounts, `no_new_privs`, or a host sandbox can prevent elevation.
- **Go 1.26.9+** only when building from source. Release binaries are static and
  do not require Go.

## Quick start

### Install

Download the archive for your architecture and `checksums.txt` from
[the latest release](https://github.com/agyard/agsudo/releases/latest) into an
empty directory. For amd64, verify and extract it with:

```sh
sha256sum --ignore-missing --check checksums.txt &&
  mkdir agsudo-package &&
  tar -xzf agsudo_*_linux_amd64.tar.gz -C agsudo-package
```

Use `linux_arm64` in the archive name for arm64. After verification succeeds,
run these commands from your **ordinary user shell**:

```sh
cd agsudo-package
sudo ./scripts/install.sh "$(id -u)"
agsudo doctor
```

`id -u` is evaluated before elevation, so the installer grants submission and
review access to your ordinary account. It installs the CLI at
`/usr/local/bin/agsudo` and the root-owned setuid helper at
`/usr/local/libexec/agsudo-helper`. Configuration lives in
`/etc/agsudo/config.json`; state lives in `/var/lib/agsudo`.

To upgrade, unpack a new release and run its installer again. Upgrade both
binaries together; existing configuration and state are preserved. The installer
does not modify sudoers.

### Try an approval

In one terminal, open the approval UI:

```sh
agsudo review --tui
```

In another terminal, submit a command:

```sh
agsudo exec --reason 'Verify privilege approval' -- id
```

Select the request in the UI, press `a`, and press Enter to confirm. The waiting
command runs as root. The UI also supports `d` to deny, `s` to defer to human
review, `/` to filter, Tab to switch panes, `t` for events, `h` for history, and
`q` to quit. You can inspect the requester, target, arguments, environment,
context snapshots, and review suggestions before deciding.

### Connect your agent

Install the integration for your host, then restart it:

```sh
agsudo integration install codex
# Or:
agsudo integration install claude
```

**The Codex integration requires Full Access and trust for the hook in `/hooks`.**
The installer merges user-level hook settings, preserves unrelated settings, and
backs up the original configuration. It does not change host permissions or hook
trust. To remove an integration, use `agsudo integration uninstall codex` or
`agsudo integration uninstall claude`.

For other agents, wrap the process:

```sh
agsudo wrap -- your-agent
# Or try an interactive shell:
agsudo wrap -- bash
```

Inside the session, `sudo` calls resolved through PATH use AG-sudo. Approval waits
are initially silent; after about two seconds, one hint appears:

```text
sudo: approval pending; see 'sudo --help'
```

`sudo --help` points to the requester and reviewer guides. See the
[integration guide](integrations/README.md) for hook behavior, tested host
versions, and limitations.

## Agent workflows

Use `agsudo COMMAND --help` or `agsudo help COMMAND` for command reference.
The role guides describe how to work with the CLI:

```sh
agsudo help --requester
agsudo help --reviewer
```

### Submit without blocking

`exec` waits for approval and runs a command. `request` only submits it and
returns an ID; approval alone does not start a detached request.

```sh
agsudo --json request --reason 'Restart the application' -- systemctl restart example.service
agsudo --json status req_ID
agsudo exec --request req_ID
# If the request is no longer needed:
agsudo cancel req_ID --reason 'Task changed'
```

Replace `req_ID` with the returned `data.id`. Put `--json` before the subcommand
so argument errors also use JSON. `exec` preserves the command's output and does
not accept `--json`. Submission options must precede the command; arguments
after the command are passed verbatim. Use repeatable `--context FILE` options
to attach bounded UTF-8 snapshots read with the caller's permissions.

A request is immutable, expires from the time of submission, and can execute
only once. Ctrl-C while waiting cancels the attempt; after launch it reaches the
command. `cancel` only affects requests that have not started.

### Review from another agent

```sh
agsudo --json review
agsudo --json show req_ID
agsudo --json approve req_ID --expect-digest DIGEST --reason 'Within the requested scope'
# If human judgment is needed:
agsudo --json defer req_ID --recommend unknown \
  --summary 'Restart the application service' \
  --reason 'Confirm that interrupting active users is acceptable'
```

Replace `DIGEST` with the digest of the request you inspected. `defer` moves the
request to `needs_human`, removing it from the default agent queue while keeping
it visible in the TUI and `review --state needs_human`. A waiting requester may
start immediately after approval. Reviewers must treat arguments, attached files,
and suggestions as untrusted evidence. See the [reviewer guide](docs/reviewer.md)
for integration advice.

### Follow and export events

```sh
agsudo --json watch --after 42
agsudo audit export --session ses_ID > trace.jsonl
```

Events include privilege requests, context, suggestions, decisions, execution
start/end, and exit status. Use the last processed sequence with `--after` to
resume a stream. Ordinary commands, terminal keystrokes, and command output are
not recorded. Traces may contain sensitive arguments or attached file contents;
local root can modify the database. See the [protocol](docs/protocol.md) for
schemas, states, and exit codes.

## Security model

AG-sudo provides a collaboration boundary for privilege approval. It is not
isolation against a hostile agent running under the same UID.

- `allowed_uids` in `/etc/agsudo/config.json` can submit and execute their own
  requests; `reviewer_uids` can inspect and decide all requests. Root can manage
  all requests. Reviewers can authorize arbitrary root operations, including
  root shells; processes sharing a reviewer's UID can call the same approval API.
- The PATH shim does not intercept `/usr/bin/sudo`, shell aliases/functions, or
  other elevation mechanisms. Host hooks do not remove these limitations.
- Executables are hashed at submission and checked again before execution. This
  does not freeze scripts' dependencies, configuration, or external state.
- A shell approval covers the entire shell session; commands inside it are not
  individually reviewed or audited. The execution environment is sanitized and
  the default umask is `0077`.

The sudo shim supports `-u USER`, `-n`, `-H`, `-s`/`-i` without command arguments,
`--`, help, and version. Other sudo options fail explicitly. AG-sudo does not
implement PAM, LDAP/SSSD NSS, or full sudo compatibility. See the
[architecture](docs/architecture.md) for the trust boundaries.

## Development

```sh
git clone https://github.com/agyard/agsudo.git
cd agsudo
make build        # Production CLI and helper in bin/
make check        # go vet
make test         # Unit tests with the race detector
make integration  # CLI, real terminal, and containerized setuid/install tests
```

Integration tests require Python 3, Bash, and Docker. They use disposable state
and containers and do not install AG-sudo on the host. Set `AGSUDO_TEST_IMAGE` to
a local Debian-compatible image when registry access is unavailable. Terminal
and elevation tests run on Linux amd64; arm64 is cross-compiled.

To try the workflow without root or installation:

```sh
make demo
export AGSUDO_HELPER="$PWD/bin/agsudo-helper-dev"
export AGSUDO_DEV_STATE="$PWD/.agsudo-dev"
bin/agsudo exec -- id
# In another terminal, set the same variables and run bin/agsudo review --tui.
```

The development helper only runs commands as the current UID and refuses root
or setuid execution. Production builds do not include the development state
path override.

## Documentation

| Guide | Contents |
| --- | --- |
| [Host integrations](integrations/README.md) | Codex and Claude Code hooks, setup, and troubleshooting |
| [Reviewer agents](docs/reviewer.md) | Review policy, decisions, and event handling |
| [CLI protocol](docs/protocol.md) | JSON/NDJSON, lifecycle, errors, and limits |
| [Architecture](docs/architecture.md) | Helper, state storage, and privilege boundaries |
| [Releasing](docs/releasing.md) | Packaging, validation, and release workflow |
| [Release notes](https://github.com/agyard/agsudo/releases) | Changes and downloads |

## Contributing

[Open an issue](https://github.com/agyard/agsudo/issues) for a bug or a proposed
change. Include the version, Linux distribution, reproduction steps, and relevant
redacted diagnostics. Pull requests should keep the CLI small, document behavior
changes, and pass the checks above.

## License

[Apache License 2.0](LICENSE). Release archives include third-party license notices.
