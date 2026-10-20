//go:build (linux && !android) || (darwin && !ios)

package gui

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDesktopLogPathUsesStateOrNativeLogs(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	for _, value := range []string{state, "", "relative-state"} {
		t.Setenv("XDG_STATE_HOME", value)
		path, err := desktopLogPath()
		if err != nil {
			t.Fatal(err)
		}
		expected := filepath.Join(homeDir, ".local", "state", "pkudisk", "logs", "desktop.log")
		if runtime.GOOS == "darwin" {
			expected = filepath.Join(homeDir, "Library", "Logs", "pkudisk", "desktop.log")
		} else if filepath.IsAbs(value) {
			expected = filepath.Join(value, "pkudisk", "logs", "desktop.log")
		}
		if path != expected {
			t.Fatalf("XDG_STATE_HOME=%q: %q, want %q", value, path, expected)
		}
	}
}
