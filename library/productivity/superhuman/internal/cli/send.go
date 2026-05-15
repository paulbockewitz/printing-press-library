// Copyright 2026 mvanhorn. Licensed under Apache-2.0. See LICENSE.

// send.go implements the headline `superhuman-pp-cli send` command — the
// 3-step real-email pipeline (userdata.writeMessage -> messages/send/log
// -> messages/send) that Superhuman's web client uses internally.
//
// Critical body-shape footgun (KD5 in plan 2026-05-14-002): the same email
// data must be marshaled TWO different ways across the three steps:
//
//   - DraftValue (step 1, writeMessage): from/to/cc/bcc are STRINGS like
//     "Matt Van Horn <user@example.com>".
//   - OutgoingMessage (step 3, messages/send): from/to/cc/bcc are OBJECTS
//     like {"email":"user@example.com","name":"Matt Van Horn"}.
//
// Mixing the two shapes returns 400 with cryptic detail messages from the
// backend. The buildDraftValue / buildOutgoingMessage functions in this
// file pin each shape with named struct types so the compiler enforces the
// distinction; do not collapse them into a single generic helper.
//
// Idempotency (KD4):
//   - writeMessage is idempotent (same draftId yields the same draft).
//   - send/log is NOT — duplicate analytics rows.
//   - send is NOT — duplicate deliveries.
//
// On failure after send/log, the CLI surfaces the failure cleanly and does
// not auto-retry steps 2 or 3.

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/printing-press-library/library/productivity/superhuman/internal/auth"
	"github.com/mvanhorn/printing-press-library/library/productivity/superhuman/internal/client"
	"github.com/mvanhorn/printing-press-library/library/productivity/superhuman/internal/cliutil"
	"github.com/mvanhorn/printing-press-library/library/productivity/superhuman/internal/config"
	"github.com/mvanhorn/printing-press-library/library/productivity/superhuman/internal/gmail"
)

// gmailAPIBaseURL is the Gmail API host. Tests override this to point at an
// httptest server. The trailing path (`/users/me/messages/send`) is appended
// at the call site.
var gmailAPIBaseURL = "https://gmail.googleapis.com/gmail/v1"

// gmailRefreshFn is the test seam for the 401-refresh path. Production wires
// it to auth.RefreshFromChromeCookies; tests override to inject a fresh
// CookieAuthResult without touching Chrome.
var gmailRefreshFn = func(ctx context.Context, email, googleID string) (*auth.CookieAuthResult, error) {
	return auth.RefreshFromChromeCookies(ctx, email, googleID)
}

// gmailAuthError is the typed error sendViaGmailAPI returns when the Gmail
// API responds with HTTP 401. The wrapper sendGmailWithRefresh uses errors.As
// to detect this case and gate the refresh-and-retry path. Other Gmail errors
// (429, 5xx, network) return as plain errors so the retry policy stays narrow.
type gmailAuthError struct {
	Status int
	Body   string
}

func (e *gmailAuthError) Error() string {
	return fmt.Sprintf("gmail api unauthorized (HTTP %d): %s", e.Status, e.Body)
}

// sendEndpointWriteMessage is the bundle's draft-persistence endpoint.
// Content-Type for this call is text/plain;charset=UTF-8 (matches the
// other /v3/userdata.* calls the bundle makes).
const sendEndpointWriteMessage = "/v3/userdata.writeMessage"

// sendEndpointLog is the analytics endpoint Superhuman hits between draft
// persistence and the actual send. Content-Type: application/json.
const sendEndpointLog = "/messages/send/log"

// sendEndpointSend is the wire-the-email-out endpoint. Content-Type:
// application/json. Returns 200 with the persisted message envelope on
// success.
const sendEndpointSend = "/messages/send"

// defaultTimeZone is the IANA zone the DraftValue records. We send the
// user's actual zone if the system reports one; otherwise the bundle's
// default (America/Los_Angeles) is a safe fallback.
const defaultTimeZone = "America/Los_Angeles"

// recipientHeader is the {name, value} shape Superhuman's OutgoingMessage
// expects for the `headers` field. An empty array is acceptable — but the
// field MUST be present (not omitted, not null).
type recipientHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// addressObject is the recipient/from shape used in OutgoingMessage. The
// Name field omitempty rule matches the bundle: "to" entries that are not
// in the user's contacts come through with email only.
type addressObject struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// draftFingerprint is the {to, cc, attachments} struct the bundle records
// inside DraftValue. The fields are concatenated recipient lists used by
// Superhuman's draft-deduplication logic.
type draftFingerprint struct {
	To          string `json:"to"`
	Cc          string `json:"cc"`
	Attachments string `json:"attachments"`
}

// draftValue is the JSON shape the bundle's userdata.writeMessage validator
// accepts. EVERY field below is required by the validator — omitting any of
// them returns 400 with no useful detail (the bundle does an aggregate
// schema check, not per-field).
//
// from/to/cc/bcc are STRINGS at this layer. Do not change.
type draftValue struct {
	ID                              string           `json:"id"`
	ThreadID                        string           `json:"threadId"`
	Action                          string           `json:"action"`
	Name                            *string          `json:"name"`
	From                            string           `json:"from"`
	To                              []string         `json:"to"`
	Cc                              []string         `json:"cc"`
	Bcc                             []string         `json:"bcc"`
	Subject                         string           `json:"subject"`
	Body                            string           `json:"body"`
	Snippet                         string           `json:"snippet"`
	InReplyToRfc822ID               *string          `json:"inReplyToRfc822Id"`
	LabelIDs                        []string         `json:"labelIds"`
	ClientCreatedAt                 string           `json:"clientCreatedAt"`
	Date                            string           `json:"date"`
	Fingerprint                     draftFingerprint `json:"fingerprint"`
	LastSessionID                   string           `json:"lastSessionId"`
	QuotedContent                   string           `json:"quotedContent"`
	QuotedContentInlined            bool             `json:"quotedContentInlined"`
	References                      []string         `json:"references"`
	Reminder                        *string          `json:"reminder"`
	Rfc822ID                        string           `json:"rfc822Id"`
	ScheduledFor                    *string          `json:"scheduledFor"`
	ScheduledReplyInterruptedAt     *string          `json:"scheduledReplyInterruptedAt"`
	SchemaVersion                   int              `json:"schemaVersion"`
	TotalComposeSeconds             int              `json:"totalComposeSeconds"`
	TimeZone                        string           `json:"timeZone"`
}

// outgoingMessage is the JSON shape the bundle's /messages/send validator
// accepts. from/to/cc/bcc are OBJECTS here, not strings — opposite of
// draftValue.
type outgoingMessage struct {
	Headers            []recipientHeader `json:"headers"`
	SuperhumanID       string            `json:"superhuman_id"`
	Rfc822ID           string            `json:"rfc822_id"`
	ThreadID           string            `json:"thread_id"`
	MessageID          string            `json:"message_id"`
	InReplyTo          *string           `json:"in_reply_to"`
	From               addressObject     `json:"from"`
	To                 []addressObject   `json:"to"`
	Cc                 []addressObject   `json:"cc"`
	Bcc                []addressObject   `json:"bcc"`
	Subject            string            `json:"subject"`
	HTMLBody           string            `json:"html_body"`
	Attachments        []any             `json:"attachments"`
	ScheduledFor       *string           `json:"scheduled_for"`
	AbortOnReply       bool              `json:"abort_on_reply"`
	CurrentMessageIDs  []string          `json:"current_message_ids"`
	MailMergeRecipients []any            `json:"mail_merge_recipients"`
}

// sendInputs captures the resolved user inputs for the send pipeline. Kept
// as a struct so buildDraftValue / buildOutgoingMessage take one argument
// instead of nine positional strings.
type sendInputs struct {
	FromEmail string
	FromName  string
	To        []string
	Cc        []string
	Bcc       []string
	Subject   string
	Body      string
	HTMLBody  bool

	DraftID      string
	Rfc822ID     string
	SuperhumanID string

	// now is the wall-clock the IDs are stamped against. Injected by tests so
	// the JSON output is byte-stable across runs. Production callers set this
	// to time.Now() inside the RunE.
	Now time.Time
}

// newSendCmd registers `superhuman-pp-cli send`. This is the headline
// workflow — see the package docstring for the 3-step pipeline.
func newSendCmd(flags *rootFlags) *cobra.Command {
	var (
		to        []string
		cc        []string
		bcc       []string
		subject   string
		body      string
		bodyFile  string
		bodyStdin bool
		from      string
		undo      time.Duration
		htmlBody  bool
	)

	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send a real email through Superhuman's backend (3-step pipeline + optional --undo)",
		Long: `Send a real email through Superhuman's backend.

The pipeline is the same three calls Superhuman's web client makes:
  1. POST /v3/userdata.writeMessage  (persist the draft on the server)
  2. POST /messages/send/log         (record the send for analytics)
  3. POST /messages/send             (fire the email out via Superhuman's MTAs)

Body source priority (highest first): --body-stdin > --body-file > --body.

The active account (set by 'auth use <email>') is the default sender; pass
--from <email> to override per call. Every invocation prints
"Sending as <from>" on stderr BEFORE firing so multi-account confusion is
visible at write time.

The --undo flag holds the send locally for the given duration, mirroring
Superhuman's UI undo window. The CLI process must stay foreground for the
duration — Ctrl-C or 'unsend' cancels.`,
		Example: `  echo "Hello!" | superhuman-pp-cli send --to alice@example.com --subject "test" --body-stdin
  superhuman-pp-cli send --to alice@example.com --subject "test" --body "Hi there"
  superhuman-pp-cli send --to alice@example.com --subject "test" --body-file ./draft.txt --undo 30s
  superhuman-pp-cli send --to alice@example.com --cc bob@example.com --subject "test" --body-stdin --from user@example.com`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2,4,5",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSend(cmd, flags, sendCmdArgs{
				To:        to,
				Cc:        cc,
				Bcc:       bcc,
				Subject:   subject,
				Body:      body,
				BodyFile:  bodyFile,
				BodyStdin: bodyStdin,
				From:      from,
				Undo:      undo,
				HTMLBody:  htmlBody,
			})
		},
	}
	cmd.Flags().StringSliceVar(&to, "to", nil, "recipient email (repeatable)")
	cmd.Flags().StringSliceVar(&cc, "cc", nil, "cc email (repeatable)")
	cmd.Flags().StringSliceVar(&bcc, "bcc", nil, "bcc email (repeatable)")
	cmd.Flags().StringVar(&subject, "subject", "", "subject line (required)")
	cmd.Flags().StringVar(&body, "body", "", "body text (or use --body-stdin / --body-file)")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read body from file")
	cmd.Flags().BoolVar(&bodyStdin, "body-stdin", false, "read body from stdin (best for Claude Code piping)")
	cmd.Flags().StringVar(&from, "from", "", "sender email (default: active account from 'auth use')")
	cmd.Flags().DurationVar(&undo, "undo", 0, "delay before sending; can cancel with 'unsend' or Ctrl-C")
	cmd.Flags().BoolVar(&htmlBody, "html", false, "treat body as raw HTML (default: wrap as plain-text <div>)")
	return cmd
}

// sendCmdArgs is a flat bag of CLI-parsed values handed to runSend. Pulled
// out so the RunE closure stays small and the test path can drive the same
// code with synthesized inputs.
type sendCmdArgs struct {
	To        []string
	Cc        []string
	Bcc       []string
	Subject   string
	Body      string
	BodyFile  string
	BodyStdin bool
	From      string
	Undo      time.Duration
	HTMLBody  bool
}

// runSend is the verifiable RunE body. Each early-return is one statement
// so tests can pin error messages to the exact branch.
func runSend(cmd *cobra.Command, flags *rootFlags, a sendCmdArgs) error {
	// --- Validation ---
	if len(a.To) == 0 {
		return usageErr(fmt.Errorf("send: at least one --to recipient required"))
	}
	if a.Subject == "" {
		return usageErr(fmt.Errorf("send: --subject required"))
	}
	bodyText, err := resolveSendBody(cmd, a.Body, a.BodyFile, a.BodyStdin)
	if err != nil {
		return usageErr(err)
	}

	// --- Sender resolution (R5f) ---
	cfg, err := flags.loadConfig()
	if err != nil {
		return configErr(err)
	}
	fromEmail := a.From
	if fromEmail == "" {
		resolved, resolveErr := cfg.ResolveActiveEmail()
		if resolveErr != nil || resolved == "" {
			return usageErr(fmt.Errorf("send: no active account; pass --from <email> or run 'auth use <email>'"))
		}
		fromEmail = resolved
	}
	store := auth.NewStoreAt(cfg.TokenStorePath())
	acct, exists, err := store.Get(fromEmail)
	if err != nil {
		return configErr(fmt.Errorf("send: load token store: %w", err))
	}
	if !exists {
		return authErr(fmt.Errorf("send: account %s not in token store; run 'auth login --disk'", fromEmail))
	}

	// Pin --from onto flags.account so the *client.Client built below uses
	// the same account for auth headers + refresh as runSend uses for body
	// construction. Without this, the client's ResolveActiveEmail walk would
	// pick the on-disk pinned account (or LastUsedAt winner), surfacing
	// confusing "wrong account" errors during refresh — and silently sending
	// FROM the right user but with auth from the wrong account, which the
	// backend will 401 on the first request.
	flags.account = fromEmail
	cfg.ActiveEmail = fromEmail

	fromName := lookupAccountName(fromEmail)

	// --- ID generation (KD5: strict format pre-flight) ---
	now := time.Now()
	draftID := auth.NewDraftID()
	rfc822ID := auth.NewRFC822ID()
	superhumanID := auth.NewSuperhumanID()

	inputs := sendInputs{
		FromEmail:    fromEmail,
		FromName:     fromName,
		To:           a.To,
		Cc:           a.Cc,
		Bcc:          a.Bcc,
		Subject:      a.Subject,
		Body:         bodyText,
		HTMLBody:     a.HTMLBody,
		DraftID:      draftID,
		Rfc822ID:     rfc822ID,
		SuperhumanID: superhumanID,
		Now:          now,
	}

	// --- Sender announcement (every invocation, BEFORE firing) ---
	announceSender(cmd, fromEmail, fromName)

	// --- Dry run: print envelope + exit 0 ---
	if flags.dryRun {
		return printSendDryRun(cmd, inputs, acct.UserID)
	}

	// --- Verify mode short-circuit (per AGENTS.md) ---
	if cliutil.IsVerifyEnv() {
		fmt.Fprintf(cmd.OutOrStdout(), "would send: %q to %v\n", a.Subject, a.To)
		return nil
	}

	// --- Build the two body shapes ---
	dv := buildDraftValue(inputs)
	om := buildOutgoingMessage(inputs)

	// --- Step 1+2 (always fire) ---
	c, err := flags.newClient()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if err := step1WriteMessage(ctx, c, acct.UserID, draftID, dv); err != nil {
		return apiErr(fmt.Errorf("send step 1 (writeMessage): %w", err))
	}
	if err := step2SendLog(ctx, c, dv, om, superhumanID, draftID); err != nil {
		return apiErr(fmt.Errorf("send step 2 (send/log): %w", err))
	}

	// --- Step 3 (with optional --undo delay) ---
	//
	// We deliver via Gmail API (sendViaGmailAPI), not Superhuman's
	// /messages/send. Per the discovery during U4 ship-gate testing and
	// edwinhu's reference (src/draft-api.ts:797), /messages/send requires
	// browser-session state we can't replicate from CLI — it returns 400
	// {"code":400} (or 520) regardless of how closely we match the bundle.
	// Gmail API accepts the OAuth access_token captured at `auth login
	// --disk` time and delivers reliably. Steps 1+2 above still persist the
	// draft + record the analytics event in Superhuman so the email shows
	// up in Superhuman's Sent UI alongside web-sent mail.
	fromDisplay := formatAddressString(fromEmail, fromName)
	if a.Undo > 0 {
		return enqueueWithUndo(cmd, c, fromEmail, acct.UserID, acct.AccessToken, fromDisplay, store, inputs, om, a.Undo)
	}
	gmailID, err := sendGmailWithRefresh(ctx, cmd.ErrOrStderr(), store, fromEmail, acct.UserID, acct.AccessToken, fromDisplay, inputs)
	if err != nil {
		// 401-after-refresh path returns an authErr-shaped message; everything
		// else is a generic Gmail API failure. Both surface via apiErr today;
		// the authErr exit code is reserved for true auth setup gaps
		// (auth-login flow), not transient 401s during a send.
		return apiErr(fmt.Errorf("send step 3 (gmail api): %w", err))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Sent. send_at=%d, draftId=%s, gmailId=%s\n", now.Unix(), draftID, gmailID)
	return nil
}

// resolveSendBody picks the body source per the priority bodyStdin > bodyFile
// > body. Returns a usage error if none is supplied OR if more than one of
// the three is set (ambiguity is almost always a user mistake).
func resolveSendBody(cmd *cobra.Command, body, bodyFile string, bodyStdin bool) (string, error) {
	set := 0
	if bodyStdin {
		set++
	}
	if bodyFile != "" {
		set++
	}
	if body != "" {
		set++
	}
	if set == 0 {
		return "", fmt.Errorf("send: one of --body, --body-file, or --body-stdin required")
	}
	if set > 1 {
		return "", fmt.Errorf("send: pass exactly one of --body, --body-file, --body-stdin (got %d)", set)
	}
	switch {
	case bodyStdin:
		in := cmd.InOrStdin()
		data, err := io.ReadAll(in)
		if err != nil {
			return "", fmt.Errorf("send: read stdin: %w", err)
		}
		return string(data), nil
	case bodyFile != "":
		data, err := os.ReadFile(bodyFile)
		if err != nil {
			return "", fmt.Errorf("send: read body file %s: %w", bodyFile, err)
		}
		return string(data), nil
	default:
		return body, nil
	}
}

// lookupAccountName reads Chrome's localStorage to find the display name for
// the given email. The bundle stores names under "<email>:name" with the
// value JSON-quoted (e.g., `"Matt Van Horn"`). We strip the quotes before
// returning. Returns "" on any error — name is optional in the OutgoingMessage
// shape, and an empty string is a sane fallback.
func lookupAccountName(email string) string {
	dataDir, err := auth.ChromeDataDir()
	if err != nil {
		return ""
	}
	profileDir := dataDir + "/Default"
	kv, err := auth.ReadSuperhumanLocalStorage(profileDir)
	if err != nil {
		return ""
	}
	raw, ok := kv[email+":name"]
	if !ok {
		return ""
	}
	// The localStorage value is JSON-quoted; try to decode but fall back to
	// the raw string if it isn't valid JSON (defensive — some entries are
	// stored without quotes).
	var s string
	if jerr := json.Unmarshal([]byte(raw), &s); jerr == nil {
		return s
	}
	return strings.Trim(raw, `"`)
}

// announceSender writes the "Sending as ..." line to stderr BEFORE any HTTP
// call. Multi-account confusion is the highest-severity footgun in the plan,
// and this is the user's visible abort window before delivery starts.
func announceSender(cmd *cobra.Command, email, name string) {
	if name != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "Sending as %s (%s)\n", email, name)
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Sending as %s\n", email)
}

// formatAddressString returns the "Name <email>" form expected by DraftValue's
// string-shaped recipient fields. Name-less senders/recipients collapse to
// the bare email (no angle brackets) so the bundle's parser doesn't choke on
// a literal "<email>" with empty name.
func formatAddressString(email, name string) string {
	if name == "" {
		return email
	}
	return fmt.Sprintf("%s <%s>", name, email)
}

// joinAddresses concatenates the input emails with commas. Used for the
// fingerprint.to / fingerprint.cc fields, which the bundle records as
// comma-joined recipient strings (not arrays).
func joinAddresses(emails []string) string {
	return strings.Join(emails, ",")
}

// buildDraftValue produces the JSON shape /v3/userdata.writeMessage accepts.
// Critical: from/to/cc/bcc are STRINGS here.
func buildDraftValue(in sendInputs) draftValue {
	iso := in.Now.UTC().Format("2006-01-02T15:04:05.000Z")
	toStrings := make([]string, len(in.To))
	for i, e := range in.To {
		toStrings[i] = e
	}
	ccStrings := make([]string, len(in.Cc))
	for i, e := range in.Cc {
		ccStrings[i] = e
	}
	bccStrings := make([]string, len(in.Bcc))
	for i, e := range in.Bcc {
		bccStrings[i] = e
	}
	dv := draftValue{
		ID:                              in.DraftID,
		ThreadID:                        in.DraftID,
		Action:                          "compose",
		Name:                            nil,
		From:                            formatAddressString(in.FromEmail, in.FromName),
		To:                              toStrings,
		Cc:                              ccStrings,
		Bcc:                             bccStrings,
		Subject:                         in.Subject,
		Body:                            renderBody(in.Body, in.HTMLBody),
		Snippet:                         "",
		InReplyToRfc822ID:               nil,
		LabelIDs:                        []string{"DRAFT"},
		ClientCreatedAt:                 iso,
		Date:                            iso,
		Fingerprint: draftFingerprint{
			To:          joinAddresses(in.To),
			Cc:          joinAddresses(in.Cc),
			Attachments: "",
		},
		LastSessionID:               in.SuperhumanID,
		QuotedContent:               "",
		QuotedContentInlined:        false,
		References:                  []string{},
		Reminder:                    nil,
		Rfc822ID:                    in.Rfc822ID,
		ScheduledFor:                nil,
		ScheduledReplyInterruptedAt: nil,
		SchemaVersion:               3,
		TotalComposeSeconds:         0,
		TimeZone:                    defaultTimeZone,
	}
	return dv
}

// senderDisplayName returns the value used in OutgoingMessage.from.name.
// Per edwinhu's reference (src/draft-api.ts:669):
//
//	const fromName = userInfo.displayName || userInfo.email.split("@")[0];
//
// The backend rejects from.name="" (HTTP 400 code-400 with no detail), so a
// non-empty name is mandatory. Email-prefix fallback matches the bundle.
func senderDisplayName(email, name string) string {
	if name != "" {
		return name
	}
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i]
	}
	return email
}

// outgoingMessageHeaders builds the meta-headers array that toJsonRequest()
// emits per the bundle (and edwinhu's TS reference). The backend rejects an
// empty `headers` array — these four entries are the minimum-viable set.
// X-Superhuman-Thread-ID is conditional: it's only included when the thread
// id is a draft id (every "new email" case in our v1 send flow).
func outgoingMessageHeaders(superhumanID, draftID, threadID string) []recipientHeader {
	const xMailer = "Superhuman Web (" + auth.SuperhumanBackendVersion + ")"
	hdrs := []recipientHeader{
		{Name: "X-Mailer", Value: xMailer},
		{Name: "X-Superhuman-ID", Value: superhumanID},
		{Name: "X-Superhuman-Draft-ID", Value: draftID},
	}
	if strings.HasPrefix(threadID, "draft") {
		hdrs = append(hdrs, recipientHeader{Name: "X-Superhuman-Thread-ID", Value: threadID})
	}
	return hdrs
}

// buildOutgoingMessage produces the JSON shape /messages/send accepts.
// Critical: from/to/cc/bcc are OBJECTS here.
func buildOutgoingMessage(in sendInputs) outgoingMessage {
	toAddrs := make([]addressObject, len(in.To))
	for i, e := range in.To {
		toAddrs[i] = addressObject{Email: e}
	}
	ccAddrs := make([]addressObject, len(in.Cc))
	for i, e := range in.Cc {
		ccAddrs[i] = addressObject{Email: e}
	}
	bccAddrs := make([]addressObject, len(in.Bcc))
	for i, e := range in.Bcc {
		bccAddrs[i] = addressObject{Email: e}
	}
	om := outgoingMessage{
		Headers:      outgoingMessageHeaders(in.SuperhumanID, in.DraftID, in.DraftID),
		SuperhumanID: in.SuperhumanID,
		Rfc822ID:     in.Rfc822ID,
		ThreadID:     in.DraftID,
		MessageID:    in.DraftID,
		InReplyTo:    nil,
		From:         addressObject{Email: in.FromEmail, Name: senderDisplayName(in.FromEmail, in.FromName)},
		To:           toAddrs,
		Cc:           ccAddrs,
		Bcc:          bccAddrs,
		Subject:      in.Subject,
		HTMLBody:     renderBody(in.Body, in.HTMLBody),
		Attachments:  []any{},
		ScheduledFor: nil,
		AbortOnReply: false,
		CurrentMessageIDs:   []string{in.DraftID},
		MailMergeRecipients: []any{},
	}
	return om
}

// renderBody wraps plain-text bodies in <div> so Superhuman's HTML renderer
// preserves line breaks and renders consistently with the web UI's "plain
// text" mode. HTML bodies pass through untouched.
func renderBody(body string, asHTML bool) string {
	if asHTML {
		return body
	}
	// Normalize CRLF/CR to LF so the <br> substitution is uniform.
	normalized := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	withBreaks := strings.ReplaceAll(normalized, "\n", "<br>")
	return "<div>" + withBreaks + "</div>"
}

// isMultiRecipient reports whether the OutgoingMessage has >1 recipient
// across to+cc+bcc. The /messages/send body field is_multi_recipient pins
// this for Superhuman's analytics + delivery routing.
func isMultiRecipient(om outgoingMessage) bool {
	return len(om.To)+len(om.Cc)+len(om.Bcc) > 1
}

// jsonHeaderOverride returns the headerOverrides map that swaps the client's
// default text/plain content-type for application/json on the two steps
// that need it (send/log + send). Step 1 (writeMessage) uses the client's
// default text/plain so we don't pass any override there.
func jsonHeaderOverride() map[string]string {
	return map[string]string{"Content-Type": "application/json"}
}

// step1WriteMessage persists the draft on Superhuman's servers so subsequent
// reads (and the analytics endpoint) can find it. Returns nil on 2xx.
//
// The path "users/<google-id>/threads/<draftId>/messages/<draftId>/draft" is
// what the bundle uses — google-id is the active account's UserID captured
// at `auth login --disk` time.
func step1WriteMessage(ctx context.Context, c *client.Client, googleID, draftID string, dv draftValue) error {
	body := map[string]any{
		"doNotReturnValues": true,
		"writes": []map[string]any{
			{
				"path":  fmt.Sprintf("users/%s/threads/%s/messages/%s/draft", googleID, draftID, draftID),
				"value": dv,
			},
		},
	}
	// writeMessage uses the same text/plain default the bundle ships, so no
	// header override is needed here.
	_, _, err := c.Post(sendEndpointWriteMessage, body)
	if err != nil {
		return err
	}
	return nil
}

// step2SendLog records the "draft_ready" analytics event. Content-Type is
// application/json (not text/plain) — this differs from step 1 and is the
// KD7 footgun in the plan.
func step2SendLog(ctx context.Context, c *client.Client, dv draftValue, om outgoingMessage, superhumanID, draftID string) error {
	body := map[string]any{
		"action":           "draft_ready",
		"draft":            dv,
		"superhuman_id":    superhumanID,
		"draft_message_id": draftID,
		"draft_thread_id":  draftID,
		"client_sent_at":   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	_, _, err := c.PostWithHeaders(sendEndpointLog, body, jsonHeaderOverride())
	return err
}

// step3Send actually fires the email. Content-Type: application/json. No
// retry on failure — duplicate sends would deliver twice.
//
// is_multi_recipient: true matches edwinhu's reference (src/draft-api.ts:707
// "app always sends true"). The backend uses this as a routing hint, not a
// recipient count; sending it as false on a single-recipient mail returns
// 400. Mismatched with the doc-block on isMultiRecipient below — that helper
// remains in case a future analytics field needs the literal count.
//
// LIMITATION (discovered during U4 ship-gate testing): Superhuman's
// /messages/send endpoint requires browser-session state we can't replicate
// from CLI (per edwinhu's reference, src/draft-api.ts:797: "Bypasses
// Superhuman's messages/send endpoint which requires browser session
// cookies and returns 520 from CLI contexts"). Our smoke runs returned 400
// with body {"code":400} — same class of failure. The CLI therefore uses
// Gmail API directly for the actual delivery (sendViaGmailAPI below); this
// step3Send remains in the codebase as the canonical implementation of the
// Superhuman shape, gated behind a (non-default) opt-in flag a future patch
// can wire up if Superhuman publishes a CLI-compatible alternative.
func step3Send(ctx context.Context, c *client.Client, om outgoingMessage) error {
	body := map[string]any{
		"version":            3,
		"outgoing_message":   om,
		"delay":              0,
		"is_multi_recipient": true,
	}
	_, _, err := c.PostWithHeaders(sendEndpointSend, body, jsonHeaderOverride())
	return err
}

// sendViaGmailAPI is the actual-delivery path. Builds an RFC822 message and
// POSTs it to https://gmail.googleapis.com/gmail/v1/users/me/messages/send
// using the OAuth access token captured at `auth login --disk` time.
// Matches edwinhu's sendViaGmailApi reference (src/draft-api.ts:800).
//
// Why this works when Superhuman's endpoint doesn't: Superhuman's web app
// ALSO uses Gmail API under the hood for Gmail accounts (the /messages/send
// wrapper is just an analytics + reply-detection layer). The Gmail API
// accepts our OAuth access token directly because we obtained it from the
// same Firebase auth flow Superhuman uses — same audience, same scopes.
//
// Returns the Gmail message id on success, or an error describing the
// failure. The CLI logs the message id to stdout so the user has a tracking
// handle if delivery fails downstream.
func sendViaGmailAPI(ctx context.Context, accessToken, fromDisplay string, in sendInputs) (string, error) {
	if accessToken == "" {
		return "", fmt.Errorf("sendViaGmailAPI: no OAuth access token; re-run 'auth login --disk' to capture")
	}

	headerLines := []string{
		"MIME-Version: 1.0",
		"From: " + fromDisplay,
		"To: " + strings.Join(in.To, ", "),
	}
	if len(in.Cc) > 0 {
		headerLines = append(headerLines, "Cc: "+strings.Join(in.Cc, ", "))
	}
	if len(in.Bcc) > 0 {
		headerLines = append(headerLines, "Bcc: "+strings.Join(in.Bcc, ", "))
	}
	headerLines = append(headerLines,
		"Subject: "+in.Subject,
		"Content-Type: text/html; charset=utf-8",
		"",
		renderBody(in.Body, in.HTMLBody),
	)
	raw := strings.Join(headerLines, "\r\n")

	// base64url WITHOUT padding (Gmail API rejects "=" padding).
	encoded := base64.URLEncoding.EncodeToString([]byte(raw))
	encoded = strings.TrimRight(encoded, "=")

	payload, err := json.Marshal(map[string]any{"raw": encoded})
	if err != nil {
		return "", fmt.Errorf("sendViaGmailAPI: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		gmailAPIBaseURL+"/users/me/messages/send",
		bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("sendViaGmailAPI: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sendViaGmailAPI: do request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return "", &gmailAuthError{Status: resp.StatusCode, Body: string(respBody)}
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("sendViaGmailAPI: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var data struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return "", fmt.Errorf("sendViaGmailAPI: parse response: %w", err)
	}
	return data.ID, nil
}

// sendGmailWithRefresh fires the send via the shared internal/gmail
// passthrough so the 401-refresh policy is implemented once for every
// consumer (per plan 2026-05-14-003 KD7). The wrapper:
//
//  1. Builds the same RFC822 + base64url payload sendViaGmailAPI builds.
//  2. Hands the request to gmail.Client.DoWithRefresh, which fires once,
//     refreshes on 401 via the seam, persists fresh tokens, and retries
//     once.
//  3. Parses the response and returns the Gmail message id.
//
// gmailRefreshFn is wired onto the Client's Refresh field so existing
// tests in send_refresh_test.go can drive the seam at the send.go level
// without reaching into the gmail package.
func sendGmailWithRefresh(
	ctx context.Context,
	stderr io.Writer,
	store *auth.Store,
	email, googleID string,
	accessToken, fromDisplay string,
	in sendInputs,
) (string, error) {
	// Build the request body identical to sendViaGmailAPI.
	if accessToken == "" {
		return "", fmt.Errorf("sendGmailWithRefresh: no OAuth access token; re-run 'auth login --disk' to capture")
	}

	headerLines := []string{
		"MIME-Version: 1.0",
		"From: " + fromDisplay,
		"To: " + strings.Join(in.To, ", "),
	}
	if len(in.Cc) > 0 {
		headerLines = append(headerLines, "Cc: "+strings.Join(in.Cc, ", "))
	}
	if len(in.Bcc) > 0 {
		headerLines = append(headerLines, "Bcc: "+strings.Join(in.Bcc, ", "))
	}
	headerLines = append(headerLines,
		"Subject: "+in.Subject,
		"Content-Type: text/html; charset=utf-8",
		"",
		renderBody(in.Body, in.HTMLBody),
	)
	raw := strings.Join(headerLines, "\r\n")
	encoded := base64.URLEncoding.EncodeToString([]byte(raw))
	encoded = strings.TrimRight(encoded, "=")

	payload, err := json.Marshal(map[string]any{"raw": encoded})
	if err != nil {
		return "", fmt.Errorf("sendGmailWithRefresh: marshal payload: %w", err)
	}

	// Construct a per-call Client wired to the existing refresh seam. The
	// Client's BaseURL comes from gmail.BaseURL, which tests can override.
	// gmailAPIBaseURL stays as the send-only seam so existing send_refresh
	// tests that swap it keep working — we propagate it into gmail.BaseURL
	// only when callers point us at a non-default host (which today only
	// happens in tests).
	if gmailAPIBaseURL != "" {
		gmail.BaseURL = gmailAPIBaseURL
	}
	gc := gmail.New(store, email, googleID, accessToken)
	gc.Refresh = gmailRefreshFn
	gc.Stderr = stderr

	req, err := http.NewRequest(http.MethodPost, gmail.BaseURL+"/users/me/messages/send", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("sendGmailWithRefresh: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	respBody, derr := gc.DoWithRefresh(ctx, req, payload)
	if derr != nil {
		return "", normalizeGmailErr(derr)
	}

	var data struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return "", fmt.Errorf("sendGmailWithRefresh: parse response: %w", err)
	}
	return data.ID, nil
}

// normalizeGmailErr maps gmail package errors onto the human-readable
// messages the existing send tests assert against. gmail.AuthError already
// surfaces "auth login --disk" so we forward it; APIError carries the
// status so we surface "HTTP <n>" to match the prior sendViaGmailAPI shape.
func normalizeGmailErr(err error) error {
	var auth *gmail.AuthError
	if errors.As(err, &auth) {
		// Preserve the specific reason language so test substring asserts
		// continue to match ("refresh failed", "still unauthorized after
		// refresh", "no access token").
		if auth.Inner != nil {
			return fmt.Errorf("gmail 401 + %s: %w (run 'auth login --disk' to re-auth)", auth.Reason, auth.Inner)
		}
		return fmt.Errorf("gmail %s (run 'auth login --disk' to re-auth)", auth.Reason)
	}
	var api *gmail.APIError
	if errors.As(err, &api) {
		return fmt.Errorf("sendViaGmailAPI: HTTP %d: %s", api.Status, api.Body)
	}
	return err
}

// printSendDryRun emits the full envelope of all three steps to stderr so
// the user can inspect what would be sent without firing. Exits 0.
func printSendDryRun(cmd *cobra.Command, in sendInputs, googleID string) error {
	dv := buildDraftValue(in)
	om := buildOutgoingMessage(in)

	step1 := map[string]any{
		"path":   sendEndpointWriteMessage,
		"method": "POST",
		"body": map[string]any{
			"doNotReturnValues": true,
			"writes": []map[string]any{
				{
					"path":  fmt.Sprintf("users/%s/threads/%s/messages/%s/draft", googleID, in.DraftID, in.DraftID),
					"value": dv,
				},
			},
		},
		"content_type": "text/plain;charset=UTF-8",
	}
	step2 := map[string]any{
		"path":   sendEndpointLog,
		"method": "POST",
		"body": map[string]any{
			"action":           "draft_ready",
			"draft":            dv,
			"superhuman_id":    in.SuperhumanID,
			"draft_message_id": in.DraftID,
			"draft_thread_id":  in.DraftID,
		},
		"content_type": "application/json",
	}
	step3 := map[string]any{
		"path":   sendEndpointSend,
		"method": "POST",
		"body": map[string]any{
			"version":            3,
			"outgoing_message":   om,
			"delay":              0,
			"is_multi_recipient": true,
		},
		"content_type": "application/json",
	}
	envelope := map[string]any{
		"dry_run":      true,
		"draft_id":     in.DraftID,
		"rfc822_id":    in.Rfc822ID,
		"superhuman_id": in.SuperhumanID,
		"step1":        step1,
		"step2":        step2,
		"step3":        step3,
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(envelope)
}

// Compile-time guarantee that the *config.Config helpers used here exist
// (cheap reminder if config.go drops them).
var _ = (*config.Config)(nil).TokenStorePath
