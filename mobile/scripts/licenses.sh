#!/bin/sh
# Collect verbatim licenses and resolved dependency metadata for embedding inside the release APK.
set -eu
mobile_dir=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
project_dir=$(CDPATH= cd -- "$mobile_dir/.." && pwd)
output="$mobile_dir/build/licenses"
tools_dir="$mobile_dir/build/license-tools"
mkdir -p "$output/go" "$output/android" "$output/web" "$tools_dir"
cd "$mobile_dir"

GOBIN="$tools_dir" go install github.com/google/go-licenses/v2@v2.0.1
# Android's pure-Go dependency graph is sufficient for license collection; the
# generated JNI binding runtime has its own x/mobile license copied below.
GOOS=android GOARCH=arm64 CGO_ENABLED=0 "$tools_dir/go-licenses" save ./bridge \
    --ignore github.com/unofficialpku/disk --save_path="$output/go" --force
GOOS=android GOARCH=arm64 CGO_ENABLED=0 "$tools_dir/go-licenses" report ./bridge \
    --ignore github.com/unofficialpku/disk > "$output/go/dependencies.csv"
cp "$(go env GOROOT)/LICENSE" "$output/go/Go-LICENSE"
cp "$(go list -m -f '{{.Dir}}' golang.org/x/mobile)/LICENSE" "$output/go/gomobile-LICENSE"
go list -m golang.org/x/mobile > "$output/go/gomobile-version.txt"
cp "$project_dir/LICENSE" "$project_dir/NOTICE" "$output/"
cp "$project_dir/LICENSE" "$output/android/Apache-2.0-LICENSE.txt"
cp "$mobile_dir/licenses/"* "$output/android/"
cp "$project_dir/internal/gui/web/THIRD_PARTY_LICENSES.txt" \
    "$project_dir/internal/gui/web/NUXT_UI_STARTER_LICENSE.txt" \
    "$project_dir/internal/gui/web/LUCIDE_LICENSE.txt" "$output/web/"
cp "$project_dir/internal/gui/web/material-icons/LICENSE.txt" "$output/web/MATERIAL_ICON_THEME_LICENSE.txt"

if [ "${1:-}" = "--sources-only" ]; then exit 0; fi

cd "$mobile_dir/android"
./gradlew --console=plain -q :app:exportRuntimeLicenses
printf 'Android release licenses: %s\n' "$output"
