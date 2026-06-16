package ebeco

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestUnwrapEnvelopeSuccess(t *testing.T) {
	body := []byte(`{"result":{"id":7,"temperatureSet":22.5},"success":true,"error":null}`)
	raw, err := unwrap(body)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	var dev Device
	if err := json.Unmarshal(raw, &dev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dev.ID != 7 || dev.TemperatureSet != 22.5 {
		t.Fatalf("got %+v", dev)
	}
}

func TestUnwrapEnvelopeFailure(t *testing.T) {
	body := []byte(`{"result":null,"success":false,"error":{"message":"nope"}}`)
	if _, err := unwrap(body); err == nil {
		t.Fatal("expected error for success:false")
	}
}

func TestUnwrapBareArray(t *testing.T) {
	// No ABP envelope: a bare array must pass through untouched.
	body := []byte(`[{"id":1},{"id":2}]`)
	raw, err := unwrap(body)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	var devs []Device
	if err := json.Unmarshal(raw, &devs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices", len(devs))
	}
}

func TestUnwrapBareObject(t *testing.T) {
	// A bare object with no "success" field is treated as the payload itself.
	body := []byte(`{"id":3,"temperatureSet":19}`)
	raw, err := unwrap(body)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	var dev Device
	if err := json.Unmarshal(raw, &dev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dev.ID != 3 {
		t.Fatalf("got %+v", dev)
	}
}

// TestAuthBackoff verifies that after an authentication failure a second
// immediate call is short-circuited by the backoff rather than hitting the
// token endpoint again.
func TestAuthBackoff(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"success":false,"error":{"message":"bad login"}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "user@example.com", "secret", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if err := c.Authenticate(ctx); err == nil {
		t.Fatal("expected authentication to fail")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", got)
	}

	// Within the backoff window the next attempt must not reach the server.
	if err := c.Authenticate(ctx); err == nil {
		t.Fatal("expected backoff error on immediate retry")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("token endpoint calls after backoff = %d, want 1 (must not hammer)", got)
	}
}

func TestTokenTTL(t *testing.T) {
	tests := []struct {
		in   int
		want int64 // seconds
	}{
		{in: 0, want: 60},
		{in: 60, want: 30},
		{in: 86400, want: 86400 - 300},
	}
	for _, tc := range tests {
		if got := int64(tokenTTL(tc.in).Seconds()); got != tc.want {
			t.Errorf("tokenTTL(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
