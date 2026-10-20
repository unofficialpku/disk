package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLaunchArgs(t *testing.T) {
	for _, test := range []struct {
		args    []string
		desktop bool
		want    []string
	}{
		{nil, false, nil}, {nil, true, []string{"gui"}},
		{[]string{"--help"}, true, []string{"--help"}},
		{[]string{"ls", "--json"}, true, []string{"ls", "--json"}},
		{[]string{"tui"}, true, []string{"tui"}},
		{[]string{"gui"}, false, []string{"gui"}},
		{[]string{"__complete", ""}, true, []string{"__complete", ""}},
	} {
		if got := launchArgs(test.args, test.desktop); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%v desktop=%v: %v", test.args, test.desktop, got)
		}
	}
}

func TestGUIHelpCompletionAndEmptyInvocationAreOffline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, args := range [][]string{nil, {"gui", "--help"}, {"gui", "stop", "--help"}, {"gui", "stop"}, {"help", "gui"}, {"completion", "bash"}, {"__complete", ""}} {
		out, stderr, err := testExecute(args...)
		if err != nil || out == "" || strings.Contains(stderr, "GUI running") {
			t.Fatalf("%v: %v", args, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "pkudisk")); !os.IsNotExist(err) {
			t.Fatal("offline command created GUI/account state")
		}
	}
	for _, args := range [][]string{{"gui", "unexpected"}, {"gui", "--account", "work"}} {
		_, _, err := testExecute(args...)
		if exitCode(err) != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
