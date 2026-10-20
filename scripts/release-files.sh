#!/bin/sh
set -eu

# Generate completions and copy dependency licenses without modifying their text.
# Keep the helper tool outside the application module and pin its version.
task_support="$(pwd)/.local/release-support"
rm -rf licenses completions
mkdir -p "$task_support/bin" licenses completions
GOBIN="$task_support/bin" go install github.com/google/go-licenses/v2@v2.0.1

for task_os in linux darwin windows; do
    GOOS="$task_os" CGO_ENABLED=0 "$task_support/bin/go-licenses" save \
        ./cmd/pkudisk --ignore github.com/unofficialpku/disk \
        --save_path="$task_support/licenses-$task_os" --force
    cp -fR "$task_support/licenses-$task_os/." licenses/
done
cp "$(go env GOROOT)/LICENSE" licenses/Go-LICENSE

CGO_ENABLED=0 go build -trimpath -o "$task_support/pkudisk" ./cmd/pkudisk
for task_shell in bash zsh fish powershell; do
    "$task_support/pkudisk" completion "$task_shell" \
        > "completions/pkudisk.$task_shell"
done
