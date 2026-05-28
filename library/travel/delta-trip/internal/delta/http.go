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

	enetxhttp "github.com/enetx/http"
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
// The ResponseHeaderTimeout is explicitly set to match the overall timeout; surf's default
// is 10s at the transport level and would silently override the client Timeout otherwise.
func newTripHTTPClient(timeout time.Duration) *http.Client {
	surfClient := surf.NewClient().
		Builder().
		Impersonate().
		Chrome().
		Timeout(timeout).
		Build().
		Unwrap()
	// Surf's transport sets ResponseHeaderTimeout to its package default (10s),
	// which caps how long we wait for the first response byte independent of
	// client.Timeout. Override so the timeout parameter is respected end-to-end.
	if t, ok := surfClient.GetTransport().(*enetxhttp.Transport); ok {
		t.ResponseHeaderTimeout = timeout
	}
	client := surfClient.Std()
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
	Status    string    `json:"status"` // "ok" or "blocked"
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
