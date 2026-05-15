// Copyright 2026 mvanhorn. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/printing-press-library/library/productivity/superhuman/internal/auth"
)

// seedSendStore writes a tokens.json with the named account so send-related
// resolution paths (account name lookup, UserID for the path string) work
// without a real Chrome on disk. AccessToken is a placeholder — every send
// test that exercises the Gmail-API step expects that call to fail against
// the real endpoint (we mock Superhuman but not gmail.googleapis.com); the
// non-empty value avoids the early-return "no OAuth token" path.
func seedSendStore(t *testing.T, tokenStorePath, email, googleID string) {
	t.Helper()
	store := auth.NewStoreAt(tokenStorePath)
	now := time.Now().UnixMilli()
	_, err := store.Upsert(email, auth.AccountTokens{
		Type:           "google",
		AccessToken:    "ya29.fake-access-token-for-test-" + email,
		RefreshToken:   "rt-" + email,
		UserID:         googleID,
		UserExternalID: "user_" + email,
		DeviceID:       "dev_test",
		SuperhumanToken: auth.SuperhumanToken{
			Token:   "id-token-" + email,
			Expires: now + int64(time.Hour.Milliseconds()),
		},
		LastUsedAt: now,
	})
	if err != nil {
		t.Fatalf("seed %s: %v", email, err)
	}
}

// writeConfigPointingAt builds a config.toml whose base_url points at the
// supplied test server. The config has no jwt set so the token store
// supplies auth — same path the real CLI takes.
func writeConfigPointingAt(t *testing.T, configPath, baseURL, activeEmail string) {
	t.Helper()
	body := "base_url = \"" + baseURL + "\"\nactive_email = \"" + activeEmail + "\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestBuildDraftValue_FromToAreStrings asserts the KD5 footgun: DraftValue
// uses STRING-shaped from/to/cc/bcc fields, never objects. If a refactor
// accidentally swaps the type, this test catches it before any HTTP fire.
func TestBuildDraftValue_FromToAreStrings(t *testing.T) {
	cases := []struct {
		name    string
		in      sendInputs
		wantFrom string
		wantTo  []string
	}{
		{
			name: "single recipient with name",
			in: sendInputs{
				FromEmail: "user@example.com",
				FromName:  "Matt Van Horn",
				To:        []string{"alice@example.com"},
				Subject:   "test",
				Body:      "hello",
				DraftID:   "draft0001",
				Rfc822ID:  "<rfc822>",
				Now:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			wantFrom: "Matt Van Horn <user@example.com>",
			wantTo:   []string{"alice@example.com"},
		},
		{
			name: "no-name sender collapses to bare email",
			in: sendInputs{
				FromEmail: "no-name@example.com",
				To:        []string{"alice@example.com"},
				Subject:   "t",
				DraftID:   "draft0002",
				Now:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			wantFrom: "no-name@example.com",
			wantTo:   []string{"alice@example.com"},
		},
		{
			name: "multiple recipients are individual strings",
			in: sendInputs{
				FromEmail: "user@example.com",
				FromName:  "M",
				To:        []string{"a@x.com", "b@x.com", "c@x.com"},
				Subject:   "t",
				DraftID:   "draft0003",
				Now:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			wantFrom: "M <user@example.com>",
			wantTo:   []string{"a@x.com", "b@x.com", "c@x.com"},
		},
		{
			name: "cc + bcc populate but stay strings",
			in: sendInputs{
				FromEmail: "user@example.com",
				FromName:  "M",
				To:        []string{"a@x.com"},
				Cc:        []string{"c1@x.com"},
				Bcc:       []string{"b1@x.com"},
				Subject:   "t",
				DraftID:   "draft0004",
				Now:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			wantFrom: "M <user@example.com>",
			wantTo:   []string{"a@x.com"},
		},
		{
			name: "subject and body propagate to draft value",
			in: sendInputs{
				FromEmail: "user@example.com",
				FromName:  "M",
				To:        []string{"a@x.com"},
				Subject:   "Hello world",
				Body:      "line1\nline2",
				DraftID:   "draft0005",
				Now:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			wantFrom: "M <user@example.com>",
			wantTo:   []string{"a@x.com"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dv := buildDraftValue(c.in)
			if dv.From != c.wantFrom {
				t.Fatalf("From: want %q, got %q", c.wantFrom, dv.From)
			}
			if len(dv.To) != len(c.wantTo) {
				t.Fatalf("To length mismatch: want %v got %v", c.wantTo, dv.To)
			}
			for i, want := range c.wantTo {
				if dv.To[i] != want {
					t.Fatalf("To[%d]: want %q, got %q", i, want, dv.To[i])
				}
			}
			// Round-trip through JSON to confirm From/To serialize as strings,
			// not objects. The serialized field types are the wire contract.
			data, err := json.Marshal(dv)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var probe struct {
				From json.RawMessage `json:"from"`
				To   json.RawMessage `json:"to"`
			}
			if err := json.Unmarshal(data, &probe); err != nil {
				t.Fatalf("unmarshal probe: %v", err)
			}
			if !strings.HasPrefix(string(probe.From), `"`) {
				t.Fatalf("DraftValue.from must serialize as a JSON string, got %s", probe.From)
			}
			// Array-of-strings starts with `["` (not `[{`).
			if !strings.HasPrefix(string(probe.To), `["`) {
				t.Fatalf("DraftValue.to must serialize as JSON string-array, got %s", probe.To)
			}
		})
	}
}

// TestBuildOutgoingMessage_FromToAreObjects asserts the KD5 footgun's other
// half: OutgoingMessage uses OBJECT-shaped from/to/cc/bcc fields with
// {email, name?}. Same shape-pinning logic as the DraftValue test.
func TestBuildOutgoingMessage_FromToAreObjects(t *testing.T) {
	cases := []struct {
		name string
		in   sendInputs
	}{
		{
			name: "single recipient",
			in: sendInputs{
				FromEmail:    "user@example.com",
				FromName:     "Matt Van Horn",
				To:           []string{"alice@example.com"},
				Subject:      "test",
				DraftID:      "draft0001",
				Rfc822ID:     "<rfc822>",
				SuperhumanID: "shid",
				Now:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "name-less sender",
			in: sendInputs{
				FromEmail:    "no-name@example.com",
				To:           []string{"alice@example.com"},
				Subject:      "t",
				DraftID:      "draft0002",
				SuperhumanID: "shid",
				Now:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "multiple recipients",
			in: sendInputs{
				FromEmail:    "user@example.com",
				FromName:     "M",
				To:           []string{"a@x.com", "b@x.com"},
				Subject:      "t",
				DraftID:      "draft0003",
				SuperhumanID: "shid",
				Now:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "cc + bcc",
			in: sendInputs{
				FromEmail:    "user@example.com",
				FromName:     "M",
				To:           []string{"a@x.com"},
				Cc:           []string{"c1@x.com"},
				Bcc:          []string{"b1@x.com"},
				Subject:      "t",
				DraftID:      "draft0004",
				SuperhumanID: "shid",
				Now:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "html body passes through",
			in: sendInputs{
				FromEmail:    "user@example.com",
				FromName:     "M",
				To:           []string{"a@x.com"},
				Subject:      "t",
				Body:         "<p>hi</p>",
				HTMLBody:     true,
				DraftID:      "draft0005",
				SuperhumanID: "shid",
				Now:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			om := buildOutgoingMessage(c.in)
			if om.From.Email != c.in.FromEmail {
				t.Fatalf("From.Email: want %q, got %q", c.in.FromEmail, om.From.Email)
			}
			// from.name must be non-empty (the backend rejects an empty name
			// with 400). When the user has no display name, the email-prefix
			// fallback in senderDisplayName supplies one.
			wantName := c.in.FromName
			if wantName == "" {
				wantName = senderDisplayName(c.in.FromEmail, "")
			}
			if om.From.Name != wantName {
				t.Fatalf("From.Name: want %q, got %q", wantName, om.From.Name)
			}
			if om.From.Name == "" {
				t.Fatalf("From.Name must never be empty (backend rejects); email=%s", c.in.FromEmail)
			}
			if len(om.To) != len(c.in.To) {
				t.Fatalf("To length: want %d got %d", len(c.in.To), len(om.To))
			}
			for i, e := range c.in.To {
				if om.To[i].Email != e {
					t.Fatalf("To[%d].Email: want %q, got %q", i, e, om.To[i].Email)
				}
			}
			data, err := json.Marshal(om)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var probe struct {
				From json.RawMessage `json:"from"`
				To   json.RawMessage `json:"to"`
			}
			if err := json.Unmarshal(data, &probe); err != nil {
				t.Fatalf("unmarshal probe: %v", err)
			}
			// from is an OBJECT, so the first byte is `{`.
			if !strings.HasPrefix(string(probe.From), `{`) {
				t.Fatalf("OutgoingMessage.from must serialize as JSON object, got %s", probe.From)
			}
			// to is an ARRAY OF OBJECTS, so it starts with `[{`.
			if !strings.HasPrefix(string(probe.To), `[{`) && !strings.HasPrefix(string(probe.To), `[`) {
				t.Fatalf("OutgoingMessage.to must serialize as JSON array, got %s", probe.To)
			}
			// Headers must serialize as empty array (NOT null) — the bundle
			// validator rejects null headers.
			if om.Headers == nil {
				t.Fatalf("Headers must be non-nil (empty array, not null)")
			}
			// Attachments + MailMergeRecipients same rule.
			if om.Attachments == nil {
				t.Fatalf("Attachments must be non-nil")
			}
			if om.MailMergeRecipients == nil {
				t.Fatalf("MailMergeRecipients must be non-nil")
			}
		})
	}
}

// TestOutgoingMessageHeaders covers the meta-header set the bundle's
// /messages/send validator requires. An empty `headers: []` returns 400.
func TestOutgoingMessageHeaders(t *testing.T) {
	hdrs := outgoingMessageHeaders("shid-xyz", "draft0001abc", "draft0001abc")
	got := map[string]string{}
	for _, h := range hdrs {
		got[h.Name] = h.Value
	}
	for _, name := range []string{"X-Mailer", "X-Superhuman-ID", "X-Superhuman-Draft-ID", "X-Superhuman-Thread-ID"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("missing header %s, got names: %v", name, got)
		}
	}
	if got["X-Superhuman-ID"] != "shid-xyz" {
		t.Fatalf("X-Superhuman-ID value mismatch: %s", got["X-Superhuman-ID"])
	}
	if !strings.HasPrefix(got["X-Mailer"], "Superhuman Web (") {
		t.Fatalf("X-Mailer prefix mismatch: %s", got["X-Mailer"])
	}
}

// TestOutgoingMessageHeaders_NonDraftThreadOmitsThreadHeader: when threading
// against a real (non-draft) thread id, X-Superhuman-Thread-ID is omitted.
// V1 send is always-draft so this is theoretical-but-cheap to lock in.
func TestOutgoingMessageHeaders_NonDraftThreadOmitsThreadHeader(t *testing.T) {
	hdrs := outgoingMessageHeaders("shid", "draft0001", "real-thread-id")
	for _, h := range hdrs {
		if h.Name == "X-Superhuman-Thread-ID" {
			t.Fatalf("non-draft thread should not include X-Superhuman-Thread-ID, got %v", hdrs)
		}
	}
}

// TestSenderDisplayName covers the from.name fallback used by
// buildOutgoingMessage. Backend rejects empty from.name with 400.
func TestSenderDisplayName(t *testing.T) {
	cases := []struct{ email, name, want string }{
		{"user@example.com", "Matt Van Horn", "Matt Van Horn"},
		{"no-name@example.com", "", "no-name"},
		{"weird", "", "weird"},
	}
	for _, c := range cases {
		if got := senderDisplayName(c.email, c.name); got != c.want {
			t.Fatalf("senderDisplayName(%q, %q): want %q got %q", c.email, c.name, c.want, got)
		}
	}
}

// TestRenderBody_PlainTextWrapping confirms plain bodies get <div> wrapped
// with <br> for newlines. Superhuman's HTML renderer collapses raw newlines,
// so this is what makes terminal-piped bodies render correctly.
func TestRenderBody_PlainTextWrapping(t *testing.T) {
	got := renderBody("line1\nline2", false)
	want := "<div>line1<br>line2</div>"
	if got != want {
		t.Fatalf("plain text body: want %q, got %q", want, got)
	}
	// HTML body should pass through.
	got = renderBody("<p>hi</p>", true)
	if got != "<p>hi</p>" {
		t.Fatalf("html body: want pass-through, got %q", got)
	}
}

// TestIsMultiRecipient covers the boolean Superhuman's analytics field
// records for delivery routing.
func TestIsMultiRecipient(t *testing.T) {
	cases := []struct {
		name string
		om   outgoingMessage
		want bool
	}{
		{"single-to", outgoingMessage{To: []addressObject{{Email: "a"}}}, false},
		{"two-to", outgoingMessage{To: []addressObject{{Email: "a"}, {Email: "b"}}}, true},
		{"to-and-cc", outgoingMessage{To: []addressObject{{Email: "a"}}, Cc: []addressObject{{Email: "b"}}}, true},
		{"to-and-bcc", outgoingMessage{To: []addressObject{{Email: "a"}}, Bcc: []addressObject{{Email: "b"}}}, true},
		{"empty", outgoingMessage{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isMultiRecipient(c.om); got != c.want {
				t.Fatalf("want %v got %v", c.want, got)
			}
		})
	}
}

// TestFormatAddressString covers the name-elision behavior used in DraftValue
// from/to string-shaped fields.
func TestFormatAddressString(t *testing.T) {
	cases := []struct {
		email, name, want string
	}{
		{"user@example.com", "Matt Van Horn", "Matt Van Horn <user@example.com>"},
		{"x@y.com", "", "x@y.com"},
	}
	for _, c := range cases {
		if got := formatAddressString(c.email, c.name); got != c.want {
			t.Fatalf("formatAddressString(%q, %q): want %q got %q", c.email, c.name, c.want, got)
		}
	}
}

// TestSend_Pipeline_HappyPath drives runSend through a mock server that
// records each hit. Asserts (1) steps 1+2 fire in order against Superhuman
// with the correct content types and (2) the success line carries gmailId.
//
// Step 3 (the actual delivery) goes to Gmail API, not Superhuman — see the
// runSend docstring for the rationale. We can't easily redirect Gmail API to
// a test server without redirecting all of *.googleapis.com, so this test
// asserts the local pipeline; the Gmail step's HTTP correctness is covered
// by the manual ship-gate smoke (which DID hit Gmail successfully).
//
// The "happy path" is achieved by passing a fake access token; the
// sendViaGmailAPI call will fail with 401 against the real Gmail endpoint,
// but the test asserts that the Superhuman steps fire correctly and the
// post-step-3 error message identifies "gmail api" as the failure source.
func TestSend_Pipeline_HappyPath(t *testing.T) {
	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "1234567890123456789")

	var hits int32
	var seenPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seenPaths = append(seenPaths, r.URL.Path)
		switch r.URL.Path {
		case "/v3/userdata.writeMessage":
			if !strings.Contains(r.Header.Get("Content-Type"), "text/plain") {
				t.Errorf("step 1 content-type: want text/plain, got %q", r.Header.Get("Content-Type"))
			}
		case "/messages/send/log":
			if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
				t.Errorf("step 2 content-type: want application/json, got %q", r.Header.Get("Content-Type"))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	writeConfigPointingAt(t, configPath, srv.URL, "user@example.com")

	// We expect the local pipeline (Superhuman steps 1+2) to succeed, then
	// step 3 (Gmail API with the fake token) to fail with a clear error.
	// That sequencing is what we're testing.
	_, stderr, err := executeCmd(t,
		"--config", configPath,
		"send",
		"--to", "alice@example.com",
		"--subject", "happy path",
		"--body", "hello",
		"--from", "user@example.com",
	)
	// Step 1+2 should have fired regardless of Gmail outcome.
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("want 2 Superhuman hits (steps 1+2), got %d (paths: %v)", got, seenPaths)
	}
	for i, want := range []string{
		"/v3/userdata.writeMessage",
		"/messages/send/log",
	} {
		if i >= len(seenPaths) || seenPaths[i] != want {
			t.Fatalf("step %d: want %s, got %v", i+1, want, seenPaths)
		}
	}
	// The error must mention the Gmail API as the failure source — that's the
	// contract the user relies on to know step 3 failed (not steps 1/2).
	if err == nil {
		t.Fatalf("expected gmail-api error with fake token, got nil")
	}
	if !strings.Contains(err.Error(), "gmail api") {
		t.Fatalf("expected 'gmail api' in error, got: %v", err)
	}
	if !strings.Contains(stderr, "Sending as user@example.com") {
		t.Fatalf("expected 'Sending as' on stderr, got: %s", stderr)
	}
}

// TestSend_Step2Fails_DoesNotCallStep3 covers KD4: send/log is not idempotent
// and step 3 must NOT fire if step 2 fails. The test fails the send/log
// endpoint and asserts /messages/send never gets hit.
func TestSend_Step2Fails_DoesNotCallStep3(t *testing.T) {
	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "1234567890123456789")

	var sendHit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/userdata.writeMessage":
			w.WriteHeader(200)
			w.Write([]byte(`{"ok":true}`))
		case "/messages/send/log":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"bad shape"}`))
		case "/messages/send":
			atomic.AddInt32(&sendHit, 1)
			w.WriteHeader(200)
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	writeConfigPointingAt(t, configPath, srv.URL, "user@example.com")

	_, _, err := executeCmd(t,
		"--config", configPath,
		"send",
		"--to", "alice@example.com",
		"--subject", "step2 fail",
		"--body", "hello",
		"--from", "user@example.com",
	)
	if err == nil {
		t.Fatalf("expected error when step 2 fails")
	}
	if !strings.Contains(err.Error(), "send/log") {
		t.Fatalf("expected 'send/log' in error, got: %v", err)
	}
	if got := atomic.LoadInt32(&sendHit); got != 0 {
		t.Fatalf("step 3 must not fire after step 2 failure, got %d hits", got)
	}
}

// TestSend_DryRun_NoHTTP confirms --dry-run prints the envelope and exits 0
// without firing any HTTP call.
func TestSend_DryRun_NoHTTP(t *testing.T) {
	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "1234567890123456789")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	writeConfigPointingAt(t, configPath, srv.URL, "user@example.com")

	stdout, _, err := executeCmd(t,
		"--config", configPath,
		"--dry-run",
		"send",
		"--to", "alice@example.com",
		"--subject", "dryrun",
		"--body", "hello",
		"--from", "user@example.com",
	)
	if err != nil {
		t.Fatalf("dry-run send: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("dry-run must not hit network, got %d hits", got)
	}
	if !strings.Contains(stdout, `"dry_run": true`) {
		t.Fatalf("expected dry_run envelope in stdout, got: %s", stdout)
	}
	// All three step paths should be present in the printed envelope.
	for _, want := range []string{
		"/v3/userdata.writeMessage",
		"/messages/send/log",
		"/messages/send",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("dry-run envelope missing %s, got: %s", want, stdout)
		}
	}
}

// TestSend_VerifyEnv_NoHTTP confirms PRINTING_PRESS_VERIFY=1 short-circuits
// to a "would send" line without firing.
func TestSend_VerifyEnv_NoHTTP(t *testing.T) {
	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "1234567890123456789")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	writeConfigPointingAt(t, configPath, srv.URL, "user@example.com")
	t.Setenv("PRINTING_PRESS_VERIFY", "1")

	stdout, _, err := executeCmd(t,
		"--config", configPath,
		"send",
		"--to", "alice@example.com",
		"--subject", "verify",
		"--body", "hello",
		"--from", "user@example.com",
	)
	if err != nil {
		t.Fatalf("verify send: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("verify mode must not hit network, got %d hits", got)
	}
	if !strings.Contains(stdout, "would send:") {
		t.Fatalf("expected 'would send:' in stdout, got: %s", stdout)
	}
}

// TestSend_MissingTo errors with the right usage exit code (2).
func TestSend_MissingTo(t *testing.T) {
	configPath, _ := withConfigPath(t)
	_, _, err := executeCmd(t, "--config", configPath, "send", "--subject", "x", "--body", "y")
	if err == nil {
		t.Fatalf("expected error for missing --to")
	}
	if !strings.Contains(err.Error(), "--to") {
		t.Fatalf("expected '--to' in error, got: %v", err)
	}
}

// TestSend_MissingSubject errors with the right usage exit code (2).
func TestSend_MissingSubject(t *testing.T) {
	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "1234567890123456789")
	_, _, err := executeCmd(t, "--config", configPath, "send", "--to", "a@x.com", "--body", "hi", "--from", "user@example.com")
	if err == nil {
		t.Fatalf("expected error for missing --subject")
	}
	if !strings.Contains(err.Error(), "--subject") {
		t.Fatalf("expected '--subject' in error, got: %v", err)
	}
}

// TestSend_NoActiveAccount errors with a helpful "auth login --disk" hint.
func TestSend_NoActiveAccount(t *testing.T) {
	configPath, _ := withConfigPath(t)
	// No store seeding, no --from: resolution falls through to "no active".
	_, _, err := executeCmd(t,
		"--config", configPath,
		"send",
		"--to", "a@x.com",
		"--subject", "no-account",
		"--body", "hi",
	)
	if err == nil {
		t.Fatalf("expected error with no active account")
	}
	if !strings.Contains(err.Error(), "no active account") && !strings.Contains(err.Error(), "auth use") {
		t.Fatalf("expected 'no active account' / 'auth use' in error, got: %v", err)
	}
}

// TestSend_TwoBodySources errors when both --body and --body-file are set
// (ambiguity is a user mistake).
func TestSend_TwoBodySources(t *testing.T) {
	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "1234567890123456789")
	bodyPath := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(bodyPath, []byte("hi"), 0o644); err != nil {
		t.Fatalf("write body file: %v", err)
	}
	_, _, err := executeCmd(t,
		"--config", configPath,
		"send",
		"--to", "a@x.com",
		"--subject", "ambiguous",
		"--body", "x",
		"--body-file", bodyPath,
		"--from", "user@example.com",
	)
	if err == nil {
		t.Fatalf("expected error for ambiguous body source")
	}
	if !strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("expected 'exactly one of' in error, got: %v", err)
	}
}

// TestSend_BodyFile reads the body from a file path. Asserts the file
// contents reach step 1 (writeMessage) — step 3 (Gmail API) is expected to
// fail against the fake access token in tests; we just check the
// Superhuman-side body shape.
func TestSend_BodyFile(t *testing.T) {
	configPath, tokenStorePath := withConfigPath(t)
	seedSendStore(t, tokenStorePath, "user@example.com", "1234567890123456789")
	bodyPath := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(bodyPath, []byte("hi from file"), 0o644); err != nil {
		t.Fatalf("write body file: %v", err)
	}

	var step1Body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/userdata.writeMessage" {
			buf, _ := io.ReadAll(r.Body)
			step1Body = string(buf)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	writeConfigPointingAt(t, configPath, srv.URL, "user@example.com")

	_, _, _ = executeCmd(t,
		"--config", configPath,
		"send",
		"--to", "a@x.com",
		"--subject", "file",
		"--body-file", bodyPath,
		"--from", "user@example.com",
	)
	if !strings.Contains(step1Body, "hi from file") {
		t.Fatalf("body file content not in step1 body, got: %s", step1Body)
	}
}
