# Design: Remove Headed Chrome Requirement from delta-trip CLI

**Date:** 2026-05-26  
**Status:** Approved

## Context

The delta-trip CLI currently requires a **visible headed Chrome window** to fetch trip data from delta.com. Chrome is launched via go-rod, a form is filled in via JavaScript (Shadow DOM traversal), and the `mytrips-api.delta.com/v1/mytrips/travelreservations` XHR is intercepted and parsed.

This blocks usage in:
- CI/CD pipelines and Docker containers
- SSH sessions and remote servers
- Any environment without a display
- Users who don't have Chrome installed

It also pops a visible browser window on every cold lookup (first call or post-TTL), which is jarring and slow (~10–15s round-trip).

The goal is to eliminate the visible window and make the CLI work in headless environments, while preserving full functionality and providing a `--headed` escape hatch.

---

## Constraints

- Delta's site is protected by **Akamai Bot Manager v3**, which operates in two phases:
  - Phase 1: TLS/HTTP fingerprinting (before JS runs) — already handled by `surf` in the existing HTTP client
  - Phase 2: `sensor_data` POST from `sensor.js` — requires a real browser with GPU, WebRTC, AudioContext; cannot be fully mocked
- The existing HTTP client uses `github.com/enetx/surf` for Chrome-like TLS fingerprinting
- The existing go-rod browser automation uses headed Chrome specifically because Akamai historically blocked headless Chrome
- `go-rod/stealth` patches exist to suppress headless detection signals; Chrome's `--headless=new` mode (112+) shares the same renderer as headed and is significantly harder to detect

---

## Approach: HTTP probe → headless Chrome fallback → headed escape hatch

Three-tier strategy, attempted in order on each cold lookup:

**Tier 1 — Direct HTTP** (~500ms if it works)  
Try a direct call to `mytrips-api.delta.com/v1/mytrips/travelreservations` using `surf` for TLS impersonation. Set all Chrome-like request headers. If Akamai's phase-1 check passes (possible for this sub-domain, especially for personal/low-volume use), return the result immediately with no browser overhead.

**Tier 2 — Headless Chrome with stealth** (~10–15s)  
If Tier 1 returns an Akamai challenge, launch Chrome in `--headless=new` mode with `go-rod/stealth` patches applied. No visible window. Works in any environment where Chrome or Chromium is installed, including Docker and CI. The stealth patches suppress `navigator.webdriver`, fix WebGL vendor strings, patch `navigator.plugins`, `navigator.languages`, and other signals Akamai's sensor.js reads.

**Tier 3 — Headed Chrome** (`--headed` flag only)  
Current behavior, preserved as an explicit opt-in for debugging or if Tier 2 gets blocked. Never taken automatically.

**Probe caching**  
The result of the Tier 1 probe (succeeded / blocked) is stored in the existing SQLite store under key `transport-probe` with a 24h TTL. On subsequent calls:
- If the last probe succeeded → skip probe, go directly to direct HTTP
- If the last probe failed → skip probe, go directly to headless Chrome
This eliminates extra latency on repeat calls.

---

## Changes

### New file: `internal/delta/http.go`
Direct HTTP client for the travelreservations API.

- `TryHTTPTrip(ctx, confirmationNo, firstName, lastName) (*TripResult, error)` — makes a GET (or POST, to be determined by inspecting the actual XHR during development) to `mytrips-api.delta.com/v1/mytrips/travelreservations` with Chrome-like headers using an `*http.Client` from `surf`
- Returns `ErrAkamaiBlocked` sentinel (typed error, not a string) when the response is an Akamai challenge page (HTTP 403, or body containing Akamai challenge markers)
- Returns `*TripResult` on success, parsed from the same JSON schema already handled by `parseTravelReservations()` in `scraper.go`
- Reuses `parseTravelReservations()` — no duplication of parsing logic

### Modified: `internal/delta/scraper.go`
- `applyStealthScripts()`: add `go-rod/stealth` package call (`stealth.Inject(page)`) in addition to the existing manual patches — stealth covers ~20 additional detection vectors
- `launchBrowser()`: switch to `--headless=new` by default; add new `launchHeadedBrowser()` that preserves the current `Headless(false)` launcher config
- `GetTrip(ctx, conf, first, last string) (*TripResult, error)`: gains a `headed bool` parameter; orchestrates Tier 1 → Tier 2 → Tier 3 (only if `headed`)

### Modified: `internal/delta/seatmap.go`
- `GetSeatMap()`: same `headed bool` parameter added; same tier logic (HTTP probe is unlikely to work for the seat map page but the headless path applies)

### Modified: `internal/cli/delta_trip.go`
- Add `--headed` persistent flag to the `trip` command group
- Pass `headed` down through `fetchAndCacheTrip()` → `delta.GetTrip()`
- Update the progress message: "Fetching trip from delta.com (this opens a browser window)..." only prints when `headed=true`. Headless path prints nothing (or a brief "Fetching..." with no browser mention)
- Add probe-cache read/write calls around `TryHTTPTrip`

### Modified: `internal/cli/seatmap.go`
- Same `--headed` flag wired to `GetSeatMap()`

### Modified: `go.mod` / `go.sum`
- Add `github.com/go-rod/stealth`

---

## Error handling

- If Tier 1 returns `ErrAkamaiBlocked`: log at debug level, proceed to Tier 2 silently
- If Tier 2 (headless) fails with a Chrome-not-found error: return a clear message: "Chrome not found. Install Chrome or run with --headed to use a headed browser."
- If Tier 2 fails for any other reason and `--headed` is not set: return the error (don't silently try Tier 3 — the user should opt in to headed mode explicitly)
- `--headed` always bypasses Tier 1 and Tier 2, goes directly to Tier 3

---

## Testing

1. **Unit test** `TryHTTPTrip` with a mock HTTP server returning (a) valid travelreservations JSON, (b) Akamai challenge response — verify `ErrAkamaiBlocked` is returned for case (b)
2. **Build test**: `go build ./...` passes
3. **Manual smoke test**: `delta-trip trip show <real-confirmation> <first> <last>` — verify no Chrome window appears, correct trip data returned
4. **Headed regression**: `delta-trip trip show <conf> <first> <last> --headed` — verify Chrome window still appears and works
5. **No-Chrome environment**: run `delta-trip trip show` in a Docker container with no Chrome installed — verify error message is clear (not a panic or obscure go-rod error)

---

## What success looks like

| Scenario | Before | After |
|---|---|---|
| Personal laptop, cold lookup | Chrome window pops up | No visible window |
| CI/Docker with Chrome | Fails (no display) | Works headlessly |
| CI/Docker without Chrome | Fails | Clear error message |
| HTTP probe succeeds | N/A | Sub-second result, zero Chrome |
| Debug / WAF blocked | Only option | `--headed` flag |
| Repeat lookup (within TTL) | Instant (SQLite cache) | Instant (unchanged) |
