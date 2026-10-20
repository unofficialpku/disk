package main

import (
	mountcore "github.com/unofficialpku/disk/internal/mount"
	"testing"
)

func TestMountReadFlags(t *testing.T) {
	command := newRemoteCommand("mount")
	if err := command.ParseFlags([]string{"--read-whole-file", "32MiB", "--read-block-size", "1MiB", "--read-ahead", "64MiB", "--read-concurrency", "2", "--cache-limit", "2GiB"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int64{"read-whole-file": 32 << 20, "read-block-size": 1 << 20, "read-ahead": 64 << 20, "cache-limit": 2 << 30} {
		value := command.Flags().Lookup(name).Value.(mountcore.ByteSize)
		if *value.Value != want {
			t.Fatalf("--%s parsed as %d", name, *value.Value)
		}
	}
	if value, err := command.Flags().GetInt("read-concurrency"); err != nil || value != 2 {
		t.Fatal("read concurrency was not parsed", value, err)
	}
}
