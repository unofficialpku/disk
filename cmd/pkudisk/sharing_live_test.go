package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare"
)

// Sharing acceptance mutates only a fresh fixture in a personal library and
// grants access only to another saved account on the same server. No existing
// files/ACLs or arbitrary recipients are changed; no notifications are sent.
func TestLiveSharing(t *testing.T) {
	if os.Getenv("PKUDISK_TEST_LIVE_SHARING") != "1" {
		t.Skip("set PKUDISK_TEST_LIVE_SHARING=1 with two saved accounts to test temporary sharing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := accounts.Default()
	if err != nil {
		t.Fatal(err)
	}
	config, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	owner := os.Getenv("PKUDISK_TEST_ACCOUNT")
	if owner == "" {
		owner = config.Default
	}
	ownerRecord, ok := config.Find(owner)
	if !ok {
		t.Fatal("saved owner account missing")
	}
	var recipientAccount string
	for _, record := range config.Accounts {
		if record.Name != owner && strings.TrimRight(record.Server, "/") == strings.TrimRight(ownerRecord.Server, "/") {
			recipientAccount = record.Name
			break
		}
	}
	if recipientAccount == "" {
		t.Fatal("sharing acceptance needs another saved account on the same server")
	}
	ownerSession, server, _, err := authenticate(ctx, authFlags{account: owner, nonInteractive: true, output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer ownerSession.Close()
	client, err := anyshare.New(server, ownerSession)
	if err != nil {
		t.Fatal(err)
	}
	recipientSession, _, _, err := authenticate(ctx, authFlags{account: recipientAccount, nonInteractive: true, output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer recipientSession.Close()
	recipientClient, err := anyshare.New(server, recipientSession)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, "POST", server+"/api/eacp/v1/user/get", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+recipientSession.AccessToken())
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("read recipient identity: transport failed (%T)", err)
	}
	var recipient struct{ UserID, Name string }
	err = json.NewDecoder(response.Body).Decode(&recipient)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || recipient.UserID == "" {
		t.Fatal("recipient account omitted a usable user ID")
	}
	matched, err := client.SearchRecipients(ctx, recipient.Name, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range matched {
		found = found || candidate.ID == recipient.UserID
	}
	if !found {
		t.Fatal("recipient search omitted the authenticated target account")
	}
	roots, err := client.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var library anyshare.Entry
	for _, root := range roots {
		if root.Type == "user_doc_lib" {
			library = root
			break
		}
	}
	if library.ID == "" {
		t.Fatal("a writable personal library is required")
	}
	folder, err := client.Mkdir(ctx, library.ID, fmt.Sprintf("pkudisk-sharing-e2e-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := client.Delete(clean, folder); err != nil {
			t.Error("temporary sharing fixture cleanup:", err)
		} else {
			t.Log("temporary sharing fixture removed")
		}
	}()
	payload := []byte("temporary sharing acceptance\n")
	file, err := client.Upload(ctx, folder.ID, "test.txt", nil, bytes.NewReader(payload), int64(len(payload)), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		args = append(args, "-a", owner, "--non-interactive", "-t", "30s")
		var out, stderr bytes.Buffer
		if binary := os.Getenv("PKUDISK_TEST_BINARY"); binary != "" {
			command := exec.CommandContext(ctx, binary, args...)
			command.Stdout, command.Stderr = &out, &stderr
			err = command.Run()
		} else {
			err = execute(ctx, args, strings.NewReader(""), &out, &stderr)
		}
		if err != nil {
			t.Fatalf("sharing command %s: %v (%s)", strings.Join(args[:3], " "), err, stderr.String())
		}
		if !strings.Contains(stderr.String(), "Using account") || strings.Contains(out.String(), "Using account") {
			t.Fatal("sharing mixed account announcements into result output")
		}
		return out.String()
	}
	var links []anyshare.ShareLink
	if json.Unmarshal([]byte(run("share", "link", file.ID, "-j")), &links) != nil || len(links) != 1 {
		t.Fatal("internal link response malformed")
	}
	first := links[0].ID
	if json.Unmarshal([]byte(run("share", "link", file.ID, "-j")), &links) != nil || len(links) != 1 || links[0].ID != first {
		t.Fatal("internal link was not reused")
	}
	can, err := recipientClient.Can(ctx, file.ID, "download")
	if err != nil || can {
		t.Fatal("inner link alone granted download", err)
	}
	run("share", "add", file.ID, "--user", recipient.UserID, "--permission", "preview,download", "--expires-in", "1h", "-j")
	can, err = recipientClient.Can(ctx, file.ID, "download")
	if err != nil || !can {
		t.Fatal("recipient could not download after granting access", err)
	}
	var downloaded bytes.Buffer
	if err := recipientClient.Download(ctx, file, &downloaded); err != nil || !bytes.Equal(payload, downloaded.Bytes()) {
		t.Fatal("recipient shared-file download failed", err)
	}
	var acl struct {
		anyshare.ShareACL
		Links []anyshare.ShareLink `json:"links"`
	}
	if json.Unmarshal([]byte(run("share", "ls", file.ID, "-j")), &acl) != nil || len(acl.Permissions) == 0 || len(acl.Links) != 1 {
		t.Fatal("populated sharing listing malformed")
	}
	run("share", "remove", file.ID, "--user", recipient.UserID)
	can, err = recipientClient.Can(ctx, file.ID, "download")
	if err != nil || can {
		t.Fatal("recipient retained download after revoking direct access", err)
	}
	t.Log("internal link reuse, actual recipient download and selective revocation passed")
	if json.Unmarshal([]byte(run("link", "add", file.ID, "--code", "Test-code123", "--expires-in", "1h", "--permission", "download", "-j")), &links) != nil || len(links) != 1 {
		t.Fatal("external link creation response malformed")
	}
	externalID := links[0].ID
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		client.RemoveExternalLink(clean, externalID)
	}()
	portalRequest, err := http.NewRequestWithContext(ctx, "GET", links[0].URL, nil)
	if err != nil {
		t.Fatal("share URL is invalid")
	}
	portalResponse, err := http.DefaultClient.Do(portalRequest)
	if err != nil {
		t.Fatalf("open anonymous share URL: transport failed (%T)", err)
	}
	portalHTML, err := io.ReadAll(io.LimitReader(portalResponse.Body, 1<<20))
	portalResponse.Body.Close()
	if err != nil || portalResponse.StatusCode != 200 || !bytes.Contains(portalHTML, []byte("__anyshare")) {
		t.Fatal("external share URL did not open the AnyShare web application")
	}
	if json.Unmarshal([]byte(run("link", "ls", file.ID, "-j")), &links) != nil || len(links) != 1 || links[0].ID != externalID || links[0].Code != "Test-code123" || links[0].ExpiresAt == nil {
		t.Fatal("external code/expiry were not retained")
	}
	run("link", "remove", externalID)
	if out := run("link", "ls", file.ID, "-j"); strings.TrimSpace(out) != "[]" {
		t.Fatal("external link was not revoked")
	}
	if json.Unmarshal([]byte(run("link", "add", folder.ID, "--code", "Inbox-code123", "--expires-in", "1h", "--permission", "create,modify", "--limit", "3", "-j")), &links) != nil || len(links) != 1 || links[0].LimitedTimes != 3 {
		t.Fatal("folder upload link response malformed")
	}
	run("link", "remove", links[0].ID)
	t.Log("external file/folder links, pickup codes, validity/visit limits and revocation passed")
}
