// Copyright 2026 mvanhorn. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mvanhorn/printing-press-library/library/productivity/superhuman/internal/gmail"
)

// inboxFakeServer scripts an httptest server that serves the Gmail inbox
// listing endpoint plus optionally returns a 401 on the first hit to drive
// the refresh path.
type inboxFakeServer struct {
	srv          *httptest.Server
	failFirst401 bool
	requestN     int
	lastQuery    url.Values
}

func newInboxFakeServer(t *testing.T, threadJSON string, failFirst401 bool) *inboxFakeServer {
	t.Helper()
	f := &inboxFakeServer{failFirst401: failFirst401}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requestN++
		f.lastQuery = r.URL.Query()
		if f.failFirst401 && f.requestN == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/users/me/threads") {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(threadJSON))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// withGmailBaseURL points the gmail package BaseURL at a test server for
// the duration of the test, restoring on cleanup.
func withGmailBaseURL(t *testing.T, url string) {
	t.Helper()
	orig := gmail.BaseURL
	gmail.BaseURL = url
	t.Cleanup(func() { gmail.BaseURL = orig })
}

// TestThreadsListInbox_HappyPath_JSON verifies the headline path: --json
// returns the envelope with threads + nextPageToken + result_size_estimate.
func TestThreadsListInbox_HappyPath_JSON(t *testing.T) {
	srv := newInboxFakeServer(t, `{
		"threads": [
			{"id": "t1", "historyId": "h1"},
			{"id": "t2", "historyId": "h2"}
		],
		"nextPageToken": "tok-next",
		"resultSizeEstimate": 42
	}`, false)
	withGmailBaseURL(t, srv.srv.URL)

	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "gid-001")
	writeConfigPointingAt(t, configPath, "http://unused", "user@example.com")

	stdout, _, err := executeCmd(t, "--config", configPath, "--json", "threads", "list", "--type", "inbox", "--limit", "25")
	if err != nil {
		t.Fatalf("threads list --type inbox --json: %v", err)
	}

	var envelope map[string]any
	if jerr := json.Unmarshal([]byte(stdout), &envelope); jerr != nil {
		t.Fatalf("parse JSON envelope: %v\n%s", jerr, stdout)
	}
	if envelope["action"] != "threads.list" {
		t.Fatalf("action = %v want threads.list", envelope["action"])
	}
	if envelope["type"] != "inbox" {
		t.Fatalf("type = %v want inbox", envelope["type"])
	}
	if envelope["next_page_token"] != "tok-next" {
		t.Fatalf("next_page_token = %v want tok-next", envelope["next_page_token"])
	}
	threads, ok := envelope["threads"].([]any)
	if !ok || len(threads) != 2 {
		t.Fatalf("threads = %v want 2-entry slice", envelope["threads"])
	}
	if got := srv.lastQuery.Get("labelIds"); got != "INBOX" {
		t.Fatalf("labelIds query = %q want INBOX", got)
	}
	if got := srv.lastQuery.Get("maxResults"); got != "25" {
		t.Fatalf("maxResults query = %q want 25", got)
	}
}

// TestThreadsListInbox_PageTokenContinues verifies the --page-token flag
// is forwarded to Gmail as pageToken.
func TestThreadsListInbox_PageTokenContinues(t *testing.T) {
	srv := newInboxFakeServer(t, `{"threads":[],"nextPageToken":""}`, false)
	withGmailBaseURL(t, srv.srv.URL)

	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "gid-001")
	writeConfigPointingAt(t, configPath, "http://unused", "user@example.com")

	_, _, err := executeCmd(t, "--config", configPath, "--json", "threads", "list", "--type", "inbox", "--page-token", "tok-2")
	if err != nil {
		t.Fatalf("threads list --type inbox --page-token: %v", err)
	}
	if got := srv.lastQuery.Get("pageToken"); got != "tok-2" {
		t.Fatalf("pageToken query = %q want tok-2", got)
	}
}

// TestThreadsListInbox_NoActiveAccount surfaces a usable error when the
// store has no account.
func TestThreadsListInbox_NoActiveAccount(t *testing.T) {
	configPath, _ := withConfigPath(t)
	writeConfigPointingAt(t, configPath, "http://unused", "")

	_, _, err := executeCmd(t, "--config", configPath, "threads", "list", "--type", "inbox")
	if err == nil {
		t.Fatalf("expected error when no active account, got nil")
	}
	if !strings.Contains(err.Error(), "active account") && !strings.Contains(err.Error(), "auth login") {
		t.Fatalf("error %q does not hint at auth login", err.Error())
	}
}

// TestThreadsListInbox_EmptyInbox_HumanFriendly ensures an empty inbox
// prints a friendly message rather than an empty table.
func TestThreadsListInbox_EmptyInbox_HumanFriendly(t *testing.T) {
	srv := newInboxFakeServer(t, `{"threads":[]}`, false)
	withGmailBaseURL(t, srv.srv.URL)

	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "gid-001")
	writeConfigPointingAt(t, configPath, "http://unused", "user@example.com")

	// --plain forces the human path even when stdout is a non-TTY buffer
	// (the default executeCmd target). Without it the non-TTY default falls
	// back to the JSON envelope, which empty-but-machine-readable.
	stdout, _, err := executeCmd(t, "--config", configPath, "--plain", "threads", "list", "--type", "inbox")
	if err != nil {
		t.Fatalf("threads list --type inbox (empty): %v", err)
	}
	if !strings.Contains(stdout, "No inbox threads") {
		t.Fatalf("empty inbox should print 'No inbox threads', got: %s", stdout)
	}
}

// TestThreadsListInbox_RegressionDraftStillUsesSuperhumanBackend ensures
// the inbox branch doesn't accidentally redirect the system-list types
// (which must continue to hit /v3/userdata.getThreads).
func TestThreadsListInbox_RegressionDraftStillUsesSuperhumanBackend(t *testing.T) {
	gmailHit := false
	gmailSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gmailHit = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer gmailSrv.Close()
	withGmailBaseURL(t, gmailSrv.URL)

	// Superhuman backend mock returns an empty list quickly.
	shSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v3/userdata.getThreads") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.Error(w, "wrong path", http.StatusNotFound)
	}))
	defer shSrv.Close()

	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "gid-001")
	writeConfigPointingAt(t, configPath, shSrv.URL, "user@example.com")

	_, _, err := executeCmd(t, "--config", configPath, "threads", "list", "--type", "draft")
	if err != nil {
		t.Fatalf("threads list --type draft: %v", err)
	}
	if gmailHit {
		t.Fatalf("--type draft must not call gmail.googleapis.com path")
	}
}

// TestThreadsListInbox_UnsupportedType keeps the validation contract: a
// non-allowed --type still errors with the supported list.
func TestThreadsListInbox_UnsupportedType(t *testing.T) {
	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "gid-001")
	writeConfigPointingAt(t, configPath, "http://unused", "user@example.com")

	_, _, err := executeCmd(t, "--config", configPath, "threads", "list", "--type", "bogus")
	if err == nil {
		t.Fatalf("expected error for --type bogus, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported --type") {
		t.Fatalf("error %q missing 'unsupported --type'", err.Error())
	}
	// New "inbox" type must surface in the listed alternatives.
	if !strings.Contains(err.Error(), "inbox") {
		t.Fatalf("error %q should mention inbox as a valid type", err.Error())
	}
}
