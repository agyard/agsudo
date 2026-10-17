#!/bin/sh
set -eu

# Packaging only: collect the licenses required to distribute both Go binaries.
agsudo_tools="$(pwd)/.local/release-tools"
mkdir -p "$agsudo_tools" licenses completions
GOBIN="$agsudo_tools" go install github.com/google/go-licenses/v2@v2.0.1
CGO_ENABLED=0 "$agsudo_tools/go-licenses" save ./cmd/... \
    --ignore github.com/agyard/agsudo --save_path=licenses --force
cp "$(go env GOROOT)/LICENSE" licenses/Go-LICENSE
CGO_ENABLED=0 go build -trimpath -o "$agsudo_tools/agsudo" ./cmd/agsudo
for agsudo_shell in bash zsh fish; do
    "$agsudo_tools/agsudo" completion "$agsudo_shell" > "completions/agsudo.$agsudo_shell"
done
