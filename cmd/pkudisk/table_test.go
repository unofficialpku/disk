package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/unofficialpku/disk/internal/anyshare"
)

func TestListingsAlignWideNames(t *testing.T) {
	var output bytes.Buffer
	entries := []anyshare.Entry{
		{Name: "12TB_文档库", Type: "custom_doc_lib", Size: -1, ID: "gns://custom"},
		{Name: "北大网盘公共资源", Type: "department_doc_lib", Size: -1, ID: "gns://public"},
		{Name: "🖼️ Photos é", Type: "user_doc_lib", Size: 12345, ID: "gns://user"},
	}
	if err := printEntries(&output, entries, false, sizeFormat{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatal(output.String())
	}
	expected := ansi.StringWidth(lines[0][:strings.Index(lines[0], "ID")])
	for _, line := range lines[1:] {
		at := strings.Index(line, "gns://")
		if at < 0 || ansi.StringWidth(line[:at]) != expected {
			t.Fatal("columns use character counts instead of display widths", output.String())
		}
	}
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "\t") {
		t.Fatal("listing contains ANSI codes or tab stops")
	}
}

func TestSharedTableAlignsNumericColumnsAndSanitizesCells(t *testing.T) {
	var output bytes.Buffer
	if err := printTable(&output, []string{"NAME", "BYTES", "ID"}, [][]string{{"中文", "1", "first"}, {"emoji😀", "1000000", "second"}, {"line\n\x1b[31m", "-1", "third"}}, 1); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 4 || strings.Contains(output.String(), "\x1b") {
		t.Fatal("control characters broke the table")
	}
	edge := -1
	for i, cell := range []string{"1", "1000000", "-1"} {
		line := lines[i+1]
		at := strings.LastIndex(line, cell)
		width := ansi.StringWidth(line[:at+len(cell)])
		if edge < 0 {
			edge = width
		}
		if width != edge {
			t.Fatal("numeric column is not right aligned", output.String())
		}
	}
}

type libraryFixture struct{}

func (libraryFixture) LibraryQuota(_ context.Context, id string) (anyshare.Quota, error) {
	switch id {
	case "gns://custom":
		return anyshare.Quota{Used: 12 << 30, Allocated: 2 << 40}, nil
	case "gns://public":
		return anyshare.Quota{Used: 1 << 30, Allocated: 200 << 30}, nil
	default:
		return anyshare.Quota{}, errors.New("quota unavailable")
	}
}
func (libraryFixture) Writable(_ context.Context, id string) (bool, error) {
	return id == "gns://custom", nil
}

func TestLibraryListingDisplaysUsageAndAccess(t *testing.T) {
	var output, status bytes.Buffer
	entries := []anyshare.Entry{{Name: "12TB_文档库", Type: "custom_doc_lib", ID: "gns://custom", Dir: true, Size: -1}, {Name: "北大网盘公共资源", Type: "department_doc_lib", ID: "gns://public", Dir: true, Size: -1}, {Name: "unknown", ID: "gns://unknown", Dir: true, Size: -1}}
	if err := printLibraries(context.Background(), libraryFixture{}, entries, sizeFormat{}, &output, &status); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "USED") || !strings.Contains(text, "12 GiB / 2.0 TiB (0.6%)") || !strings.Contains(text, "1.0 GiB") || strings.Contains(text, "200 GiB") || strings.Contains(text, "(0.5%)") || !strings.Contains(text, "RW") || !strings.Contains(text, "RO") || !strings.Contains(text, "unavailable") || strings.Contains(text, "-1") {
		t.Fatal(text)
	}
	if !strings.Contains(status.String(), "quota unavailable") {
		t.Fatal("missing per-library error status")
	}
}

func TestFolderSizeMarkerOnlyChangesTextOutput(t *testing.T) {
	entry := anyshare.Entry{ID: "gns://lib/folder", Name: "中文目录", Dir: true, Size: -1}
	var output bytes.Buffer
	if err := printEntries(&output, []anyshare.Entry{entry}, false, sizeFormat{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "-1") || !strings.Contains(output.String(), "folder") {
		t.Fatal(output.String())
	}
	output.Reset()
	if err := printEntries(&output, []anyshare.Entry{entry}, true, sizeFormat{bytes: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"size": -1`) {
		t.Fatal("JSON folder size changed", output.String())
	}
}
