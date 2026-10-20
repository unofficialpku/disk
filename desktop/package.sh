#!/bin/sh
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 3 ]; then
    printf 'Usage: sh desktop/package.sh BINARY [OUTPUT-DIRECTORY] [linux|darwin]\n' >&2
    exit 2
fi
task_binary="$(CDPATH= cd -- "$(dirname -- "$1")" && pwd)/$(basename -- "$1")"
if [ ! -f "$task_binary" ] || [ ! -x "$task_binary" ]; then
    printf 'An executable binary is required.\n' >&2
    exit 1
fi
# Desktop entry values cannot contain newlines. Other reserved characters are
# escaped below, including field-code percent signs and shell metacharacters.
case "$task_binary" in
    *'
'*|*"$(printf '\r')"*) printf 'The binary path cannot contain line breaks.\n' >&2; exit 1 ;;
esac
task_output="${2:-./bin/desktop}"
task_platform="${3:-$(uname -s)}"
task_resources="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
mkdir -p "$task_output"
task_output="$(CDPATH= cd -- "$task_output" && pwd)"

case "$task_platform" in
    Darwin|darwin)
        task_app="$task_output/PKUDisk.app"
        if [ -e "$task_app" ] || [ -L "$task_app" ]; then
            printf 'Application bundle already exists: %s\n' "$task_app" >&2
            exit 1
        fi
        mkdir -p "$task_app/Contents/MacOS"
        mkdir -p "$task_app/Contents/Resources"
        cp "$task_resources/icon.icns" "$task_app/Contents/Resources/icon.icns"
        cp "$task_binary" "$task_app/Contents/MacOS/pkudisk"
        cp "$task_resources/Info.plist" "$task_app/Contents/Info.plist"
        cp "$task_resources/launch" "$task_app/Contents/MacOS/launch"
        chmod 755 "$task_app/Contents/MacOS/launch" "$task_app/Contents/MacOS/pkudisk"
        printf 'Created %s\n' "$task_app"
        ;;
    Linux|linux)
        task_entry="$task_output/pkudisk.desktop"
        if [ -e "$task_entry" ] || [ -L "$task_entry" ]; then
            printf 'Desktop entry already exists: %s\n' "$task_entry" >&2
            exit 1
        fi
        # Desktop files unescape string values before parsing quoted Exec
        # arguments, so backslashes for Exec quoting must themselves be escaped.
        task_exec=$(printf '%s' "$task_binary" | sed 's/\\/\\\\\\\\/g; s/"/\\\\"/g; s/`/\\\\`/g; s/\$/\\\\$/g; s/%/%%/g')
        cp "$task_resources/icon.png" "$task_output/pkudisk.png"
        task_icon=$(printf '%s' "$task_output/pkudisk.png" | sed 's/\\/\\\\/g')
        cat > "$task_entry" <<EOF
[Desktop Entry]
Type=Application
Name=PKUDisk
Comment=Open the PKUDisk desktop interface
Exec=/usr/bin/env PKUDISK_DESKTOP_LAUNCH=1 "$task_exec" gui
Icon=$task_icon
Terminal=false
Categories=Network;FileTransfer;
EOF
        chmod 755 "$task_entry"
        printf 'Created %s\n' "$task_entry"
        ;;
    *) printf 'Choose linux or darwin; Windows uses the executable directly.\n' >&2; exit 2 ;;
esac
