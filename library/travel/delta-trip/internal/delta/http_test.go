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
		if r.URL.Query().Get("confirmationNo") != "ABC123" {
			t.Errorf("expected confirmationNo=ABC123, got %q", r.URL.Query().Get("confirmationNo"))
		}
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

// TestTryHTTPTrip_AkamaiBlocked_HTMLBody tests that a 200 response with text/html
// Content-Type is treated as an Akamai challenge page. Akamai serves challenge
// pages with HTTP 200 and text/html even on API sub-domains.
func TestTryHTTPTrip_AkamaiBlocked_HTMLBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("confirmationNo") != "ABC123" {
			t.Errorf("expected confirmationNo=ABC123, got %q", r.URL.Query().Get("confirmationNo"))
		}
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
