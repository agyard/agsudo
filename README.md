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
- **Human or agent review:** use a terminal UI or a JSON CLI. No built-in
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
binaries together; the installer keeps the configuration and state directory.
It does not modify sudoers.

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
command runs as root. The UI also supports `d` to deny, `/` to filter, `h` for
history, PgUp/PgDn to scroll details, and `q` to quit. You can inspect the
requester, target, arguments, context snapshots, review suggestions, and any
earlier denial of the same command before deciding.

### Connect your agent

Install the integration for your host, then restart it:

```sh
agsudo integration install codex
# Or:
agsudo integration install claude
```

**The Codex integration routes sudo only in Full Access mode and requires trust
for the hook in `/hooks`.** The installer merges user-level hook settings and
preserves unrelated settings. It does not change host permissions or hook trust.
Check routing with `sudo --version` in the host. To remove an integration, use
`agsudo integration uninstall codex` or `agsudo integration uninstall claude`.

For other agents, wrap the process:

```sh
agsudo wrap -- your-agent
# Or try an interactive shell:
agsudo wrap -- bash
```

Inside the session, `sudo` calls resolved through PATH use AG-sudo. Approval waits
are initially silent; after about two seconds, one hint names the request:

```text
sudo: waiting for approval of req_3f9c2a71d04b8e65; see 'sudo --help'
```

Ctrl-C cancels the request. If an agent's tool call times out or is killed
instead, the request stays pending; once it is approved, running the same
command again reuses the request and runs it. See the
[integration guide](integrations/README.md) for hook behavior, tested host
versions, and limitations.

## Agent workflows

The guides are part of the command help:

```sh
agsudo exec --help     # requesting agents; the same guide as sudo --help
agsudo review --help   # reviewing agents and the approval workbench
```

A requester can submit without waiting, then run the same command once it is
approved:

```sh
ID=$(agsudo request --reason 'Restart the application' -- systemctl restart example.service)
agsudo show "$ID"
agsudo exec -- systemctl restart example.service
```

A reviewing agent uses JSON:

```sh
agsudo --json review
agsudo --json approve req_ID --expect-digest DIGEST --reason 'Within the requested scope'
agsudo --json defer req_ID --recommend unknown \
  --reason 'Restarts a service; confirm that interrupting active users is acceptable'
```

`defer` moves a request to `needs_human` for a person. Decisions made without a
terminal get guardrails against agent mistakes; they are not a boundary against
a hostile agent. `agsudo --json events --session ses_ID > trace.jsonl` exports
the audit trail and `agsudo events --follow` follows it. Events include
requests, context, suggestions, decisions, and execution results, but not
ordinary commands, keystrokes, or output. See the [reviewer guide](docs/reviewer.md)
for an example review policy and the [protocol](docs/protocol.md) for the JSON,
states, and exit codes.

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
  individually reviewed or audited. The execution environment is sanitized and,
  as with sudo, the umask is the caller's combined with `0022`.

The sudo shim supports `-u USER`, `-n`, `-k`, `-H`, `-s`/`-i` without command
arguments, `--`, help, and version. Other sudo options fail explicitly. AG-sudo does not
implement PAM, LDAP/SSSD NSS, or full sudo compatibility. See the
[architecture](docs/architecture.md) for the trust boundaries.

## Development

```sh
git clone https://github.com/agyard/agsudo.git
cd agsudo
make build        # Production CLI and helper in bin/
make check        # gofmt, go mod tidy, go vet
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
