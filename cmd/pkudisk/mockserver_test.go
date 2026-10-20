package main

import (
	"testing"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare/mockserver"
)

func commandFixture(t *testing.T) *mockserver.Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := mockserver.New("fixture")
	t.Cleanup(func() {
		server.Close()
		for _, failure := range server.Failures() {
			t.Error(failure)
		}
	})
	store, err := accounts.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(server.Account("fixture"), true, false); err != nil {
		t.Fatal(err)
	}
	return server
}

func mutationCount(server *mockserver.Server, action string) int {
	count := 0
	for _, event := range server.Mutations() {
		if event == action {
			count++
		}
	}
	return count
}
