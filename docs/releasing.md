# Releasing

Release archives contain the production CLI and helper for Linux amd64/arm64,
installation script, documentation, Bash/Zsh/Fish completions, project and
third-party licenses, plus a separate `checksums.txt`.

## Prepare a version

1. Update `docs/releases/vX.Y.Z.md`. Keep it about the released behavior,
   installation and material limitations.
2. Run `make check test integration`. Tests use disposable state and a Docker
   container. `AGSUDO_TEST_IMAGE` can select an existing Debian-compatible image
   when registry access is unavailable.
3. Rehearse with GoReleaser v2.18.3:

   ```sh
   goreleaser check
   goreleaser release --snapshot --clean
   ```

   Extract the native archive into an empty directory and run the installer
   tests against the extracted package:

   ```sh
   AGSUDO_TEST_PACKAGE=/absolute/path/to/extracted-package python3 tests/privileged.py
   ```

4. Commit and push to `main`. Wait for CI on that commit before tagging.
5. Publish an annotated semantic version tag:

   ```sh
   git tag -a vX.Y.Z -m 'Release vX.Y.Z'
   git push origin vX.Y.Z
   ```

Never reuse a published tag. Corrections get a new version.

## Workflows

CI uses one Linux runner: source formatting and dependency consistency, vet,
race tests, CLI/PTY tests, real setuid and installation tests, then an arm64
cross-build. The native binaries are already built for integration tests.

Release requires a successful `main` push CI run for the tag's commit. It
packages with GoReleaser, verifies the native version, and publishes archives
and checksums through `gh release create`. It does not rerun CI or replace an
existing release. Prerelease tags containing `-` do not become `latest`.

The workflow uses Go 1.26.x, `CGO_ENABLED=0`, `-trimpath`, and injects the release
tag into the CLI version. Normal source builds report `dev` unless built with
`make build VERSION=vX.Y.Z`. Both binaries must be upgraded together.

`scripts/release-files.sh` uses pinned go-licenses v2.0.1 to collect dependency
licenses and generate completions; it is packaging work, not another CI check.
`LICENSE` is the unchanged Apache-2.0 text, with project attribution in `NOTICE`.
