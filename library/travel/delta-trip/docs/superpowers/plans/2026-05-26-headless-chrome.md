# Remove Headed Chrome from delta-trip CLI — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the visible headed Chrome window with a three-tier strategy: direct HTTP first (~500ms), headless Chrome with stealth patches second (no window), headed Chrome only when `--headed` is explicitly passed.

**Architecture:** New `internal/delta/http.go` adds `TryHTTPTrip()` (direct API call via Chrome TLS impersonation) and file-based probe caching (24h TTL at `~/.cache/delta-trip-pp-cli/probe.json`). `scraper.go` gains a `launchHeadlessBrowser()` using `--headless=new` + `go-rod/stealth` patches, and `GetTrip()` gets a `headed bool` param that orchestrates the three tiers. CLI commands add a `--headed` flag to opt back into the visible window.

**Tech Stack:** Go, `github.com/go-rod/rod` (existing), `github.com/go-rod/stealth` (new), `github.com/enetx/surf` (existing, TLS impersonation), `net/http/httptest` (tests)

---

## File Map

| File | Action | Responsibility |
|------|--------|----------------|
| `internal/delta/http.go` | **Create** | `TryHTTPTrip`, `ErrAkamaiBlocked`, probe cache helpers, `newTripHTTPClient`, `isAkamaiChallenge` |
| `internal/delta/http_test.go` | **Create** | Unit tests for `TryHTTPTrip` (blocked on 403, blocked on HTML, success) |
| `internal/delta/scraper.go` | **Modify** | Add `stealth` import + `stealth.Inject`; split `launchBrowser` → `launchHeadlessBrowser` + `launchHeadedBrowser`; `GetTrip` adds `headed bool` and three-tier orchestration |
| `internal/delta/seatmap.go` | **Modify** | `GetSeatMap` adds `headed bool`; use `launchHeadlessBrowser` by default |
| `internal/cli/delta_trip.go` | **Modify** | `--headed` persistent flag on `trip` command; `fetchAndCacheTrip` adds `headed bool`; update progress messages; update all `newTrip*Cmd` constructors |
| `internal/cli/seatmap.go` | **Modify** | `--headed` flag on top-level `seatmap`; `newTripSeatMapCmd` accepts `*bool`; `seatMapRunE` takes `*bool`; update `GetSeatMap` call and progress message; fix `Long` description |
| `go.mod` / `go.sum` | **Modify** | Add `github.com/go-rod/stealth` |

---

### Task 1: Add go-rod/stealth dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Fetch the dependency**

```powershell
cd C:\Users\paulb\printing-press\library\delta-trip
go get github.com/go-rod/stealth@latest
```

Expected: `go.mod` now includes `github.com/go-rod/stealth`, `go.sum` updated, no errors.

- [ ] **Step 2: Verify existing build still passes**

```powershell
go build ./...
```

Expected: no errors.

---

### Task 2: Write failing tests for TryHTTPTrip

**Files:**
- Create: `internal/delta/http_test.go`

- [ ] **Step 1: Create the test file**

Create `internal/delta/http_test.go`:

```go
package delta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// minimalTravelReservationsJSON is the smallest valid payload parseTravelReservations
// can parse without returning an error.
const minimalTravelReservationsJSON = `{
  "travelReservations": [{
    "reservation": {"tripType": "ONE_WAY"},
    "passengers": [],
    "trips": [{"segments": []}]
  }]
}`

func TestTryHTTPTrip_AkamaiBlocked_403(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	orig := tripAPIURL
	tripAPIURL = ts.URL
	defer func() { tripAPIURL = orig }()

	_, err := TryHTTPTrip(context.Background(), "ABC123", "JANE", "SMITH")
	if !errors.Is(err, ErrAkamaiBlocked) {
		t.Errorf("expected ErrAkamaiBlocked on 403, got %v", err)
	}
}

func TestTryHTTPTrip_AkamaiBlocked_HTMLBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html>challenge page</html>"))
	}))
	defer ts.Close()

	orig := tripAPIURL
	tripAPIURL = ts.URL
	defer func() { tripAPIURL = orig }()

	_, err := TryHTTPTrip(context.Background(), "ABC123", "JANE", "SMITH")
	if !errors.Is(err, ErrAkamaiBlocked) {
		t.Errorf("expected ErrAkamaiBlocked on HTML content-type, got %v", err)
	}
}

func TestTryHTTPTrip_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Chrome-like headers are present.
		if r.Header.Get("Origin") != "https://www.delta.com" {
			t.Errorf("expected Origin header, got %q", r.Header.Get("Origin"))
		}
		// Verify query params are uppercase.
		if r.URL.Query().Get("confirmationNo") != "ABC123" {
			t.Errorf("expected confirmationNo=ABC123, got %q", r.URL.Query().Get("confirmationNo"))
		}
		if r.URL.Query().Get("firstName") != "JANE" {
			t.Errorf("expected firstName=JANE, got %q", r.URL.Query().Get("firstName"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(minimalTravelReservationsJSON))
	}))
	defer ts.Close()

	orig := tripAPIURL
	tripAPIURL = ts.URL
	defer func() { tripAPIURL = orig }()

	trip, err := TryHTTPTrip(context.Background(), "abc123", "jane", "smith")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if trip == nil {
		t.Fatal("expected non-nil TripResult")
	}
	if trip.ConfirmationNumber != "ABC123" {
		t.Errorf("expected ConfirmationNumber ABC123, got %q", trip.ConfirmationNumber)
	}
}
```

- [ ] **Step 2: Run tests — verify they fail because http.go doesn't exist yet**

```powershell
go test ./internal/delta/ -run "TestTryHTTPTrip" -v
```

Expected: compile error — `TryHTTPTrip`, `ErrAkamaiBlocked`, `tripAPIURL` undefined. This is correct.

---

### Task 3: Implement `internal/delta/http.go`

**Files:**
- Create: `internal/delta/http.go`

- [ ] **Step 1: Create the file**

Create `internal/delta/http.go`:

```go
package delta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enetx/surf"
)

// ErrAkamaiBlocked is returned by TryHTTPTrip when the request is blocked
// by Akamai Bot Manager (HTTP 403 or HTML challenge page response).
var ErrAkamaiBlocked = errors.New("request blocked by Akamai bot protection")

// tripAPIURL is the Delta travelreservations API endpoint.
// Package-level var (not const) so tests can override it.
var tripAPIURL = "https://mytrips-api.delta.com/v1/mytrips/travelreservations"

const tripAPITimeout = 10 * time.Second

// TryHTTPTrip attempts to fetch trip data via direct HTTP to mytrips-api.delta.com,
// using the surf library to impersonate Chrome's TLS fingerprint.
//
// Returns ErrAkamaiBlocked if Akamai challenges the request.
// On success, parses the response via parseTravelReservations (defined in scraper.go).
//
// NOTE: This assumes the API accepts GET with query params. If inspection of
// the real XHR shows it uses POST+JSON body, change the method and body below.
func TryHTTPTrip(ctx context.Context, confirmationNo, firstName, lastName string) (*TripResult, error) {
	conf := strings.ToUpper(confirmationNo)
	first := strings.ToUpper(firstName)
	last := strings.ToUpper(lastName)

	httpClient := newTripHTTPClient(tripAPITimeout)

	req, err := http.NewRequestWithContext(ctx, "GET", tripAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building HTTP request: %w", err)
	}

	q := req.URL.Query()
	q.Set("confirmationNo", conf)
	q.Set("firstName", first)
	q.Set("lastName", last)
	req.URL.RawQuery = q.Encode()

	// Headers a real browser XHR sends to this sub-domain.
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://www.delta.com")
	req.Header.Set("Referer", "https://www.delta.com/my-trips/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if isAkamaiChallenge(resp) {
		return nil, ErrAkamaiBlocked
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP %d from travelreservations API", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	return parseTravelReservations(body, conf)
}

// newTripHTTPClient returns an *http.Client that impersonates Chrome's TLS fingerprint.
func newTripHTTPClient(timeout time.Duration) *http.Client {
	client := surf.NewClient().
		Builder().
		Impersonate().
		Chrome().
		Timeout(timeout).
		Build().
		Unwrap().
		Std()
	client.Timeout = timeout
	return client
}

// isAkamaiChallenge returns true when the response indicates an Akamai bot challenge.
// HTTP 403 is the definitive signal. A JSON API returning text/html is also a challenge.
func isAkamaiChallenge(resp *http.Response) bool {
	if resp.StatusCode == http.StatusForbidden {
		return true
	}
	return strings.Contains(resp.Header.Get("Content-Type"), "text/html")
}

// --- Probe cache ---
// Remembers whether the direct HTTP path works to avoid probing on every cold call.
// Stored at ~/.cache/delta-trip-pp-cli/probe.json with a 24h TTL.

const probeTTL = 24 * time.Hour

type probeCache struct {
	Status    string    `json:"status"`    // "ok" or "blocked"
	UpdatedAt time.Time `json:"updatedAt"`
}

// LoadProbeStatus reads the cached probe result.
// Returns "ok", "blocked", or "" if uncached/expired.
func LoadProbeStatus() string {
	data, err := os.ReadFile(probeFilePath())
	if err != nil {
		return ""
	}
	var pc probeCache
	if err := json.Unmarshal(data, &pc); err != nil {
		return ""
	}
	if time.Since(pc.UpdatedAt) > probeTTL {
		return ""
	}
	return pc.Status
}

// SaveProbeStatus writes the probe result to disk.
func SaveProbeStatus(status string) {
	path := probeFilePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	pc := probeCache{Status: status, UpdatedAt: time.Now()}
	data, _ := json.Marshal(pc)
	_ = os.WriteFile(path, data, 0o644)
}

func probeFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "delta-trip-pp-cli", "probe.json")
}
```

- [ ] **Step 2: Run the tests — verify they pass**

```powershell
go test ./internal/delta/ -run "TestTryHTTPTrip" -v
```

Expected:
```
--- PASS: TestTryHTTPTrip_AkamaiBlocked_403 (0.00s)
--- PASS: TestTryHTTPTrip_AkamaiBlocked_HTMLBody (0.00s)
--- PASS: TestTryHTTPTrip_Success (0.00s)
PASS
```

- [ ] **Step 3: Commit**

```powershell
git add internal/delta/http.go internal/delta/http_test.go
git commit -m "feat(delta): TryHTTPTrip direct API client with ErrAkamaiBlocked and probe cache"
```

---

### Task 4: Update scraper.go — stealth patches + headless/headed launchers + GetTrip tiers

**Files:**
- Modify: `internal/delta/scraper.go`

- [ ] **Step 1: Add stealth to the import block**

In `internal/delta/scraper.go`, the current import block is:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)
```

Replace with:

```go
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)
```

- [ ] **Step 2: Replace applyStealthScripts to include stealth.Inject**

Find the existing `applyStealthScripts` function:

```go
func applyStealthScripts(page *rod.Page) error {
	_, err := page.EvalOnNewDocument(`() => {
		Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
		Object.defineProperty(navigator, 'plugins', { get: () => [1,2,3] });
		Object.defineProperty(navigator, 'languages', { get: () => ['en-US','en'] });
		window.chrome = { runtime: {} };
	}`)
	return err
}
```

Replace with:

```go
func applyStealthScripts(page *rod.Page) error {
	// go-rod/stealth patches ~20 anti-detection signals including WebGL vendor
	// strings, plugin arrays, iframe contentWindow access, and more.
	if err := stealth.Inject(page); err != nil {
		return fmt.Errorf("stealth.Inject: %w", err)
	}
	// Belt-and-suspenders: ensure chrome.runtime exists in addition to stealth's patches.
	_, err := page.EvalOnNewDocument(`() => {
		window.chrome = window.chrome || { runtime: {} };
	}`)
	return err
}
```

- [ ] **Step 3: Replace launchBrowser with launchHeadlessBrowser + launchHeadedBrowser**

Find the existing `launchBrowser` function:

```go
func launchBrowser() (*rod.Browser, func(), error) {
	l := launcher.New().
		Headless(false).
		Leakless(false). // avoid leakless helper binary (AV false-positive on Windows)
		Set("disable-blink-features", "AutomationControlled").
		Set("disable-infobars", "").
		Set("window-size", "1920,1080").
		Set("start-maximized", "").
		Delete("enable-automation")

	if path, ok := launcher.LookPath(); ok {
		l = l.Bin(path)
	}

	u, err := l.Launch()
	if err != nil {
		return nil, nil, fmt.Errorf("launch: %w", err)
	}

	browser := rod.New().ControlURL(u).MustConnect()
	cleanup := func() {
		browser.MustClose()
		l.Cleanup()
	}
	return browser, cleanup, nil
}
```

Replace with:

```go
// launchHeadlessBrowser starts Chrome in --headless=new mode.
// --headless=new (Chrome 112+) uses the same renderer as headed Chrome,
// making it significantly harder for WAFs to detect than --headless (old).
// Works in Docker, CI, and SSH sessions — no display server required.
func launchHeadlessBrowser() (*rod.Browser, func(), error) {
	l := launcher.New().
		Headless(false).             // disable --headless (old headless flag)
		Set("headless", "new").      // enable --headless=new
		Leakless(false).
		Set("disable-blink-features", "AutomationControlled").
		Set("disable-infobars", "").
		Set("window-size", "1920,1080").
		Delete("enable-automation")

	if path, ok := launcher.LookPath(); ok {
		l = l.Bin(path)
	}

	u, err := l.Launch()
	if err != nil {
		return nil, nil, fmt.Errorf("headless browser launch: %w", err)
	}

	browser := rod.New().ControlURL(u).MustConnect()
	cleanup := func() {
		browser.MustClose()
		l.Cleanup()
	}
	return browser, cleanup, nil
}

// launchHeadedBrowser starts Chrome in headed (visible window) mode.
// Used only when --headed flag is passed.
func launchHeadedBrowser() (*rod.Browser, func(), error) {
	l := launcher.New().
		Headless(false).
		Leakless(false).
		Set("disable-blink-features", "AutomationControlled").
		Set("disable-infobars", "").
		Set("window-size", "1920,1080").
		Set("start-maximized", "").
		Delete("enable-automation")

	if path, ok := launcher.LookPath(); ok {
		l = l.Bin(path)
	}

	u, err := l.Launch()
	if err != nil {
		return nil, nil, fmt.Errorf("headed browser launch: %w", err)
	}

	browser := rod.New().ControlURL(u).MustConnect()
	cleanup := func() {
		browser.MustClose()
		l.Cleanup()
	}
	return browser, cleanup, nil
}
```

- [ ] **Step 4: Update GetTrip signature and add three-tier orchestration**

Find the start of `GetTrip`:

```go
func GetTrip(ctx context.Context, confirmationNo, firstName, lastName string) (*TripResult, error) {
	conf := strings.ToUpper(confirmationNo)
	first := strings.ToUpper(firstName)
	last := strings.ToUpper(lastName)

	browser, cleanup, err := launchBrowser()
```

Replace only this opening block (keep everything from the browser page setup onward unchanged):

```go
// GetTrip fetches trip data from delta.com using a three-tier strategy:
//   - Tier 1 (headed=false): direct HTTP via TryHTTPTrip — instant if Akamai allows it
//   - Tier 2 (headed=false): headless Chrome with go-rod/stealth patches, no visible window
//   - Tier 3 (headed=true):  headed Chrome (visible window), only when --headed is passed
func GetTrip(ctx context.Context, confirmationNo, firstName, lastName string, headed bool) (*TripResult, error) {
	conf := strings.ToUpper(confirmationNo)
	first := strings.ToUpper(firstName)
	last := strings.ToUpper(lastName)

	// Tier 1: direct HTTP (skipped if --headed or probe cache says "blocked").
	if !headed {
		probeStatus := LoadProbeStatus()
		if probeStatus != "blocked" {
			trip, httpErr := TryHTTPTrip(ctx, conf, first, last)
			if httpErr == nil {
				SaveProbeStatus("ok")
				return trip, nil
			}
			if errors.Is(httpErr, ErrAkamaiBlocked) {
				SaveProbeStatus("blocked")
				// Fall through to browser.
			}
			// Non-Akamai errors (network timeout, parse failure): don't cache result,
			// fall through to browser so the user still gets data.
		}
	}

	// Tier 2 / Tier 3: browser.
	launchFn := launchHeadlessBrowser
	if headed {
		launchFn = launchHeadedBrowser
	}

	browser, cleanup, err := launchFn()
```

- [ ] **Step 5: Build to verify compile errors are only in the CLI layer (expected)**

```powershell
go build ./internal/delta/...
```

Expected: no errors in the delta package. Errors in `internal/cli/...` are expected because `GetTrip` and `GetSeatMap` callers haven't been updated yet.

- [ ] **Step 6: Commit**

```powershell
git add internal/delta/scraper.go
git commit -m "feat(delta): headless Chrome + go-rod/stealth + three-tier GetTrip orchestration"
```

---

### Task 5: Update seatmap.go (delta package) — headless launcher + headed param

**Files:**
- Modify: `internal/delta/seatmap.go`

- [ ] **Step 1: Update GetSeatMap signature**

In `internal/delta/seatmap.go`, find:

```go
func GetSeatMap(ctx context.Context, confirmationNo, firstName, lastName string, flightIndex int) (*SeatMapResult, error) {
```

Replace with:

```go
func GetSeatMap(ctx context.Context, confirmationNo, firstName, lastName string, flightIndex int, headed bool) (*SeatMapResult, error) {
```

- [ ] **Step 2: Replace launchBrowser() call with the tier-aware launcher**

In `seatmap.go`, find:

```go
	browser, cleanup, err := launchBrowser()
```

Replace with:

```go
	launchFn := launchHeadlessBrowser
	if headed {
		launchFn = launchHeadedBrowser
	}
	browser, cleanup, err := launchFn()
```

- [ ] **Step 3: Build delta package**

```powershell
go build ./internal/delta/...
```

Expected: no errors. CLI errors still expected.

- [ ] **Step 4: Commit**

```powershell
git add internal/delta/seatmap.go
git commit -m "feat(delta): GetSeatMap supports headless Chrome via headed bool param"
```

---

### Task 6: Wire --headed flag in delta_trip.go

**Files:**
- Modify: `internal/cli/delta_trip.go`

- [ ] **Step 1: Add errors import (needed for errors.Is in fetchAndCacheTrip)**

`delta_trip.go` already imports `"errors"` — no change needed. Verify by checking the import block at the top of the file.

- [ ] **Step 2: Update fetchAndCacheTrip signature and body**

Find:

```go
func fetchAndCacheTrip(ctx context.Context, conf, first, last string, flags *rootFlags, noCache bool) (*delta.TripResult, error) {
```

Replace with:

```go
func fetchAndCacheTrip(ctx context.Context, conf, first, last string, flags *rootFlags, noCache bool, headed bool) (*delta.TripResult, error) {
```

Find the browser-scrape section:

```go
	// Browser scrape.
	fmt.Fprintf(os.Stderr, "Fetching trip %s from delta.com (this opens a browser window)...\n", conf)
	timeout := flags.timeout
	if timeout < 60*time.Second {
		timeout = 60 * time.Second
	}
	scrapeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	trip, err := delta.GetTrip(scrapeCtx, conf, first, last)
```

Replace with:

```go
	// Browser scrape (headless by default, headed if --headed).
	if headed {
		fmt.Fprintf(os.Stderr, "Fetching trip %s from delta.com (opening browser window)...\n", conf)
	} else {
		fmt.Fprintf(os.Stderr, "Fetching trip %s from delta.com...\n", conf)
	}
	timeout := flags.timeout
	if timeout < 60*time.Second {
		timeout = 60 * time.Second
	}
	scrapeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	trip, err := delta.GetTrip(scrapeCtx, conf, first, last, headed)
```

- [ ] **Step 3: Add --headed persistent flag to newTripCmd and pass to all subcommands**

Find `newTripCmd`:

```go
func newTripCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trip",
		Short: "Manage Delta trips by confirmation number",
		Long:  "Look up and display Delta Air Lines trip details using a confirmation number (no login required).",
	}
	cmd.AddCommand(newTripShowCmd(flags))
	cmd.AddCommand(newTripFlightsCmd(flags))
	cmd.AddCommand(newCheckinStatusCmd(flags))
	cmd.AddCommand(newTripSeatMapCmd(flags))
	cmd.AddCommand(newLayoverRiskCmd(flags))
	return cmd
}
```

Replace with:

```go
func newTripCmd(flags *rootFlags) *cobra.Command {
	var flagHeaded bool
	cmd := &cobra.Command{
		Use:   "trip",
		Short: "Manage Delta trips by confirmation number",
		Long:  "Look up and display Delta Air Lines trip details using a confirmation number (no login required).",
	}
	cmd.PersistentFlags().BoolVar(&flagHeaded, "headed", false, "Use a visible Chrome window (default: headless; use if bot detection blocks headless)")
	cmd.AddCommand(newTripShowCmd(flags, &flagHeaded))
	cmd.AddCommand(newTripFlightsCmd(flags, &flagHeaded))
	cmd.AddCommand(newCheckinStatusCmd(flags, &flagHeaded))
	cmd.AddCommand(newTripSeatMapCmd(flags, &flagHeaded))
	cmd.AddCommand(newLayoverRiskCmd(flags, &flagHeaded))
	return cmd
}
```

- [ ] **Step 4: Update newTripShowCmd**

Find:

```go
func newTripShowCmd(flags *rootFlags) *cobra.Command {
```

Replace with:

```go
func newTripShowCmd(flags *rootFlags, flagHeaded *bool) *cobra.Command {
```

Find its `fetchAndCacheTrip` call:

```go
			trip, err := fetchAndCacheTrip(cmd.Context(), conf, first, last, flags, flagNoCache)
```

Replace with:

```go
			trip, err := fetchAndCacheTrip(cmd.Context(), conf, first, last, flags, flagNoCache, *flagHeaded)
```

- [ ] **Step 5: Update newTripFlightsCmd**

Find:

```go
func newTripFlightsCmd(flags *rootFlags) *cobra.Command {
```

Replace with:

```go
func newTripFlightsCmd(flags *rootFlags, flagHeaded *bool) *cobra.Command {
```

Find its `fetchAndCacheTrip` call:

```go
			trip, err := fetchAndCacheTrip(cmd.Context(), conf, first, last, flags, flagNoCache)
```

Replace with:

```go
			trip, err := fetchAndCacheTrip(cmd.Context(), conf, first, last, flags, flagNoCache, *flagHeaded)
```

- [ ] **Step 6: Update newCheckinStatusCmd**

Find:

```go
func newCheckinStatusCmd(flags *rootFlags) *cobra.Command {
```

Replace with:

```go
func newCheckinStatusCmd(flags *rootFlags, flagHeaded *bool) *cobra.Command {
```

Find its `fetchAndCacheTrip` call:

```go
			trip, err := fetchAndCacheTrip(cmd.Context(), conf, first, last, flags, flagNoCache)
```

Replace with:

```go
			trip, err := fetchAndCacheTrip(cmd.Context(), conf, first, last, flags, flagNoCache, *flagHeaded)
```

- [ ] **Step 7: Update newLayoverRiskCmd**

Find:

```go
func newLayoverRiskCmd(flags *rootFlags) *cobra.Command {
```

Replace with:

```go
func newLayoverRiskCmd(flags *rootFlags, flagHeaded *bool) *cobra.Command {
```

Find its `fetchAndCacheTrip` call:

```go
			trip, err := fetchAndCacheTrip(cmd.Context(), conf, first, last, flags, flagNoCache)
```

Replace with:

```go
			trip, err := fetchAndCacheTrip(cmd.Context(), conf, first, last, flags, flagNoCache, *flagHeaded)
```

- [ ] **Step 8: Update newTripSeatMapCmd signature (will be fully updated in Task 7)**

Find:

```go
func newTripSeatMapCmd(flags *rootFlags) *cobra.Command {
```

Replace with:

```go
func newTripSeatMapCmd(flags *rootFlags, flagHeaded *bool) *cobra.Command {
```

- [ ] **Step 9: Build — only seatmap.go CLI errors should remain**

```powershell
go build ./internal/cli/...
```

Expected: only errors about `delta.GetSeatMap` wrong argument count in `seatmap.go`. Fixed in Task 7.

- [ ] **Step 10: Commit**

```powershell
git add internal/cli/delta_trip.go
git commit -m "feat(cli): --headed flag for trip commands; headless Chrome by default"
```

---

### Task 7: Wire --headed flag in cli/seatmap.go

**Files:**
- Modify: `internal/cli/seatmap.go`

- [ ] **Step 1: Update seatMapRunE to take *bool for headed**

Find:

```go
func seatMapRunE(flags *rootFlags, flagFlight *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if dryRunOK(flags) {
			return nil
		}

		conf := strings.ToUpper(args[0])
		first := strings.ToUpper(args[1])
		last := strings.ToUpper(args[2])

		fmt.Fprintf(os.Stderr, "Fetching seat map for %s flight %d (opens a browser window)...\n", conf, *flagFlight)

		timeout := flags.timeout
		if timeout < 150*time.Second {
			timeout = 150 * time.Second
		}
		scrapeCtx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()

		seatMap, err := delta.GetSeatMap(scrapeCtx, conf, first, last, *flagFlight)
```

Replace with:

```go
func seatMapRunE(flags *rootFlags, flagFlight *int, flagHeaded *bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if dryRunOK(flags) {
			return nil
		}

		conf := strings.ToUpper(args[0])
		first := strings.ToUpper(args[1])
		last := strings.ToUpper(args[2])

		if *flagHeaded {
			fmt.Fprintf(os.Stderr, "Fetching seat map for %s flight %d (opening browser window)...\n", conf, *flagFlight)
		} else {
			fmt.Fprintf(os.Stderr, "Fetching seat map for %s flight %d...\n", conf, *flagFlight)
		}

		timeout := flags.timeout
		if timeout < 150*time.Second {
			timeout = 150 * time.Second
		}
		scrapeCtx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()

		seatMap, err := delta.GetSeatMap(scrapeCtx, conf, first, last, *flagFlight, *flagHeaded)
```

- [ ] **Step 2: Update newSeatMapCmd (top-level seatmap command)**

Find:

```go
func newSeatMapCmd(flags *rootFlags) *cobra.Command {
	var flagFlight int
	cmd := &cobra.Command{
		// ...
		RunE:        seatMapRunE(flags, &flagFlight),
	}
	cmd.Flags().IntVar(&flagFlight, "flight", 1, "Flight within the itinerary to map (1-based, default 1)")
	return cmd
}
```

Replace with:

```go
func newSeatMapCmd(flags *rootFlags) *cobra.Command {
	var flagFlight int
	var flagHeaded bool
	cmd := &cobra.Command{
		Use:   "seatmap <confirmation> <first-name> <last-name>",
		Short: "Show full seat availability map for a flight",
		Long: `Show every seat on a flight with availability status.

Fetches seat availability from delta.com for the specified flight using headless
Chrome by default (no visible window). Pass --headed to use a visible window
if headless mode is blocked.
Subsequent calls within the 4-hour trip cache are faster (flight metadata
is reused; only the seat map page is re-fetched).`,
		Example: strings.TrimRight(`
  delta-trip-pp-cli seatmap ABC123 JANE SMITH
  delta-trip-pp-cli seatmap ABC123 JANE SMITH --flight 2
  delta-trip-pp-cli seatmap ABC123 JANE SMITH --json
  delta-trip-pp-cli seatmap ABC123 JANE SMITH --json --select cabins,availableSeats`, "\n"),
		Args:        cobra.ExactArgs(3),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE:        seatMapRunE(flags, &flagFlight, &flagHeaded),
	}
	cmd.Flags().IntVar(&flagFlight, "flight", 1, "Flight within the itinerary to map (1-based, default 1)")
	cmd.Flags().BoolVar(&flagHeaded, "headed", false, "Use a visible Chrome window (default: headless; use if bot detection blocks headless)")
	return cmd
}
```

- [ ] **Step 3: Update newTripSeatMapCmd to use the parent's flagHeaded pointer**

Find:

```go
func newTripSeatMapCmd(flags *rootFlags, flagHeaded *bool) *cobra.Command {
	var flagFlight int
	cmd := &cobra.Command{
		// ...
		RunE:        seatMapRunE(flags, &flagFlight),
	}
	cmd.Flags().IntVar(&flagFlight, "flight", 1, "Flight within the itinerary to map (1-based, default 1)")
	return cmd
}
```

Replace with:

```go
func newTripSeatMapCmd(flags *rootFlags, flagHeaded *bool) *cobra.Command {
	var flagFlight int
	cmd := &cobra.Command{
		Use:   "seatmap <confirmation> <first-name> <last-name>",
		Short: "Show full seat availability map for a flight",
		Example: strings.TrimRight(`
  delta-trip-pp-cli trip seatmap ABC123 JANE SMITH
  delta-trip-pp-cli trip seatmap ABC123 JANE SMITH --flight 2`, "\n"),
		Args:        cobra.ExactArgs(3),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE:        seatMapRunE(flags, &flagFlight, flagHeaded),
	}
	cmd.Flags().IntVar(&flagFlight, "flight", 1, "Flight within the itinerary to map (1-based, default 1)")
	return cmd
}
```

- [ ] **Step 4: Build everything — should be clean**

```powershell
go build ./...
```

Expected: no errors.

- [ ] **Step 5: Run all tests**

```powershell
go test ./...
```

Expected: all tests pass including the three `TestTryHTTPTrip_*` tests.

- [ ] **Step 6: Commit**

```powershell
git add internal/cli/seatmap.go
git commit -m "feat(cli): --headed flag for seatmap; headless Chrome by default; update Long description"
```

---

### Task 8: Build final binary and smoke test

**Files:** none (verification only)

- [ ] **Step 1: Build the Windows binary**

```powershell
go build -o delta-trip.exe ./cmd/delta-trip-pp-cli
```

Expected: `delta-trip.exe` produced, no errors.

- [ ] **Step 2: Verify --headed flag appears in help**

```powershell
.\delta-trip.exe trip --help
```

Expected output includes:
```
      --headed   Use a visible Chrome window (default: headless; use if bot detection blocks headless)
```

- [ ] **Step 3: Verify --headed flag appears on seatmap help**

```powershell
.\delta-trip.exe seatmap --help
```

Expected output includes:
```
      --headed   Use a visible Chrome window (default: headless; use if bot detection blocks headless)
```

- [ ] **Step 4: Smoke test — live lookup without --headed**

Run with a real confirmation number and observe: no Chrome window appears. Progress message says `"Fetching trip ... from delta.com..."` (no "browser window" mention).

```powershell
.\delta-trip.exe trip show <CONF> <FIRSTNAME> <LASTNAME>
```

One of three outcomes:
- **HTTP succeeds**: result prints in ~1s, probe.json written with `"ok"`
- **HTTP blocked, headless works**: result prints in ~12s, no visible window, probe.json written with `"blocked"`
- **Both blocked**: error returned — run `--headed` as fallback (that's the escape hatch)

- [ ] **Step 5: Regression test — --headed still works**

```powershell
.\delta-trip.exe trip show <CONF> <FIRSTNAME> <LASTNAME> --headed
```

Expected: Chrome window appears, `"Fetching trip ... from delta.com (opening browser window)..."` in stderr, correct trip data printed.

---

## Implementation Notes

**If the API uses POST instead of GET:** Inspect the XHR using `--headed` mode + browser DevTools, or check the captured `apiBody` content in a debug run. If it's POST+JSON, change `TryHTTPTrip` to:
```go
body := fmt.Sprintf(`{"confirmationNo":%q,"firstName":%q,"lastName":%q}`, conf, first, last)
req, err = http.NewRequestWithContext(ctx, "POST", tripAPIURL, strings.NewReader(body))
req.Header.Set("Content-Type", "application/json")
```

**If headless Chrome is also blocked:** `GetTrip` returns an error with go-rod's launch error message. Add `--headed` to the user-facing error: in `fetchAndCacheTrip`, wrap the error:
```go
return nil, fmt.Errorf("fetching trip from delta.com: %w\nIf headless Chrome is blocked, try adding --headed", err)
```

**Chrome not installed:** `launcher.LookPath()` returns `false` and go-rod tries to auto-download Chromium. This can be slow on first run. This behavior is inherited from the existing code.
