package mount

import (
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/vfs"
)

func TestMountByteSizes(t *testing.T) {
	for text, want := range map[string]int64{
		"0": 0, "5368709121": (5 << 30) + 1, "8MiB": 8 << 20,
		"1.5GiB": 3 << 29, "5MB": 5000000, "9223372036854775807": 9223372036854775807,
	} {
		var got int64
		if err := (ByteSize{&got}).Set(text); err != nil || got != want {
			t.Fatalf("size %s = %d, want %d; error %v", text, got, want, err)
		}
	}
	for _, text := range []string{"-1", "invalid", "8MiiB", "9223372036854775808", "18446744073709551615"} {
		var value int64
		if err := (ByteSize{&value}).Set(text); err == nil {
			t.Fatalf("invalid size %s accepted", text)
		}
	}
}

func TestOptionsPreserveExplicitZeroAndReadPolicy(t *testing.T) {
	zero := 0
	opts, err := ParseOptions(Input{Point: "/mount", CacheLimit: "0", WholeFile: "0", ReadAhead: "0", TTLSeconds: &zero, RefreshSeconds: &zero, BlockSize: "8MiB", PartSize: "32MiB", Concurrency: 2, TimeoutSeconds: 1200})
	if err != nil {
		t.Fatal(err)
	}
	if opts.CacheLimit != 0 || opts.TTL != 0 || opts.RefreshInterval != 0 || opts.Read.WholeFile != 0 || opts.Read.ReadAhead != 0 || opts.Read.BlockSize != 8<<20 || opts.PartSize != 32<<20 || opts.Read.Concurrency != 2 || opts.Timeout != 20*time.Minute {
		t.Fatal("GUI mount options lost explicit values")
	}
	defaults, err := ParseOptions(Input{Point: "/mount"})
	if err != nil || defaults.Read != vfs.DefaultReadOptions() || defaults.CacheLimit != 10<<30 {
		t.Fatal("GUI differs from CLI mount defaults", err)
	}
	for _, input := range []Input{{BlockSize: "0"}, {PartSize: "1MiB"}, {Concurrency: 65}, {ReadAhead: "513MiB"}, {CacheLimit: "-1"}, {TimeoutSeconds: -1}} {
		if _, err := ParseOptions(input); err == nil {
			t.Fatal("invalid GUI mount options accepted", input)
		}
	}
}
