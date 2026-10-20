package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/unofficialpku/disk/internal/anyshare"
)

func TestReadableAndExactSizeModes(t *testing.T) {
	for _, test := range []struct {
		n          int64
		human, raw string
	}{{0, "0 B", "0"}, {1536, "1.5 KiB", "1536"}, {12 << 30, "12 GiB", "12884901888"}, {2 << 40, "2.0 TiB", "2199023255552"}, {-1, "-", "-"}} {
		if got := (sizeFormat{}).value(test.n); got != test.human {
			t.Fatal(got)
		}
		if got := (sizeFormat{bytes: true}).value(test.n); got != test.raw {
			t.Fatal(got)
		}
	}
	entries := []anyshare.Entry{{ID: "gns://library/file", Name: "test.txt", Size: 1536}, {ID: "gns://library/folder", Name: "folder", Size: -1, Dir: true}}
	for _, raw := range []bool{false, true} {
		var out bytes.Buffer
		format := sizeFormat{bytes: raw}
		if err := printEntries(&out, entries, false, format); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), format.value(1536)) || strings.Contains(out.String(), "-1") {
			t.Fatal(out.String())
		}
		out.Reset()
		if err := printEntries(&out, entries, true, format); err != nil {
			t.Fatal(err)
		}
		var data []struct {
			Size int64 `json:"size"`
		}
		if err := json.Unmarshal(out.Bytes(), &data); err != nil || data[0].Size != 1536 || data[1].Size != -1 {
			t.Fatal("format flag changed JSON sizes", err)
		}
		out.Reset()
		var status bytes.Buffer
		roots := []anyshare.Entry{{ID: "gns://custom", Type: "custom_doc_lib", Name: "custom"}, {ID: "gns://public", Type: "department_doc_lib", Name: "public"}}
		if err := printLibraries(context.Background(), libraryFixture{}, roots, format, &out, &status); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), format.value(12<<30)+" / "+format.value(2<<40)) || strings.Contains(out.String(), format.value(200<<30)) {
			t.Fatal("library modes or RO quota are incorrect", out.String())
		}
	}
}

func TestShortFlagsAndHelpAreConsistent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	commands := [][]string{{"ls"}, {"upload"}, {"download"}, {"mount"}, {"tui"}, {"recover"}, {"quota"}, {"search"}}
	for _, path := range commands {
		out, stderr, err := testExecute(append(append([]string{}, path...), "-h")...)
		if err != nil || stderr != "" || !strings.Contains(out, "Usage:") || !strings.Contains(out, "-b, --bytes") {
			t.Fatalf("-h for %v: %v\n%s", path, err, out)
		}
		withBytes, stderr, err := testExecute(append(append([]string{}, path...), "-b", "--help")...)
		if err != nil || stderr != "" || withBytes != out {
			t.Fatalf("-b interfered with help for %v: %v", path, err)
		}
	}
	root := newRootCommand(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	for _, test := range []struct {
		path  []string
		flags map[string]string
	}{
		{[]string{"ls"}, map[string]string{"a": "account", "b": "bytes", "j": "json", "u": "username"}},
		{[]string{"upload"}, map[string]string{"a": "account", "b": "bytes", "f": "overwrite", "t": "io-timeout", "u": "username"}},
		{[]string{"download"}, map[string]string{"f": "overwrite", "t": "io-timeout"}},
		{[]string{"mount"}, map[string]string{"v": "verbose", "t": "io-timeout", "a": "account", "b": "bytes"}},
		{[]string{"quota"}, map[string]string{"a": "account", "b": "bytes", "t": "io-timeout"}},
		{[]string{"search"}, map[string]string{"j": "json", "b": "bytes"}},
		{[]string{"copy"}, map[string]string{"a": "account", "t": "io-timeout"}},
		{[]string{"account", "add"}, map[string]string{"u": "username"}},
		{[]string{"account", "ls"}, map[string]string{"j": "json"}},
		{[]string{"recover"}, map[string]string{"j": "json", "b": "bytes"}},
	} {
		cmd, _, err := root.Find(test.path)
		if err != nil {
			t.Fatal(err)
		}
		for short, long := range test.flags {
			flag := cmd.Flags().ShorthandLookup(short)
			if flag == nil || flag.Name != long {
				t.Fatalf("%v -%s does not map to --%s", test.path, short, long)
			}
		}
	}
}
