// Package ebeco is a small client for the Ebeco Connect API
// (https://ebecoconnect.com/swagger). It handles token authentication with
// automatic refresh, unwraps the ABP/ASP.NET-Zero response envelope, and
// exposes the device read/update calls this app needs.
package ebeco

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Authentication backoff bounds: after a failed token fetch we wait at least
// authBackoffBase, doubling up to authBackoffMax, so a sustained outage or bad
// credentials can't hammer the token endpoint on every API call.
const (
	authBackoffBase = 15 * time.Second
	authBackoffMax  = 10 * time.Minute
)

// Device mirrors the fields of UserDeviceDto that we care about.
type Device struct {
	ID               int     `json:"id"`
	DisplayName      string  `json:"displayName"`
	PowerOn          bool    `json:"powerOn"`
	SelectedProgram  string  `json:"selectedProgram"`
	ProgramState     string  `json:"programState"`
	TemperatureSet   float64 `json:"temperatureSet"`   // the target we control
	TemperatureFloor float64 `json:"temperatureFloor"` // floor sensor reading
	TemperatureRoom  float64 `json:"temperatureRoom"`  // room sensor reading
	RelayOn          bool    `json:"relayOn"`
	HasError         bool    `json:"hasError"`
	ErrorMessage     string  `json:"errorMessage"`
}

// UpdateInput is the UserDeviceInput body for UpdateUserDevice. Pointer fields
// are omitted when nil so we only send what we intend to change.
type UpdateInput struct {
	ID              int      `json:"id"`
	PowerOn         *bool    `json:"powerOn,omitempty"`
	SelectedProgram *string  `json:"selectedProgram,omitempty"`
	TemperatureSet  *float64 `json:"temperatureSet,omitempty"`
}

// Client is a concurrency-safe Ebeco Connect API client.
type Client struct {
	baseURL  string
	email    string
	password string
	tenantID int
	http     *http.Client
	log      *slog.Logger

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	// Auth backoff state, all guarded by mu.
	authBackoff     time.Duration
	nextAuthAttempt time.Time
	lastAuthErr     error
}

// New returns a client for the given base URL (e.g. https://ebecoconnect.com).
// tenantID is sent as the ABP "Abp.TenantId" header (Ebeco accounts live in the
// default tenant, id 1); pass 0 to omit it. A nil logger falls back to the slog
// default.
func New(baseURL, email, password string, tenantID int, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		email:       email,
		password:    password,
		tenantID:    tenantID,
		http:        &http.Client{Timeout: 20 * time.Second},
		log:         log,
		authBackoff: authBackoffBase,
	}
}

// Authenticate forces an initial token fetch so startup fails fast on bad
// credentials.
func (c *Client) Authenticate(ctx context.Context) error {
	_, err := c.ensureToken(ctx)
	return err
}

// GetDevice fetches a single device by id.
func (c *Client) GetDevice(ctx context.Context, id int) (Device, error) {
	var dev Device
	path := fmt.Sprintf("/api/services/app/Devices/GetUserDeviceById?id=%d", id)
	if err := c.do(ctx, http.MethodGet, path, nil, &dev); err != nil {
		return Device{}, err
	}
	return dev, nil
}

// GetDevices fetches all devices on the account.
func (c *Client) GetDevices(ctx context.Context) ([]Device, error) {
	var devs []Device
	if err := c.do(ctx, http.MethodGet, "/api/services/app/Devices/GetUserDevices", nil, &devs); err != nil {
		return nil, err
	}
	return devs, nil
}

// UpdateDevice applies an UpdateInput via UpdateUserDevice.
func (c *Client) UpdateDevice(ctx context.Context, in UpdateInput) error {
	return c.do(ctx, http.MethodPut, "/api/services/app/Devices/UpdateUserDevice", in, nil)
}

// --- internals ---

type authRequest struct {
	UserNameOrEmailAddress string `json:"userNameOrEmailAddress"`
	Password               string `json:"password"`
	RememberClient         bool   `json:"rememberClient"`
}

type authResult struct {
	AccessToken                   string `json:"accessToken"`
	ExpireInSeconds               int    `json:"expireInSeconds"`
	UserID                        int64  `json:"userId"`
	RequiresTwoFactorVerification bool   `json:"requiresTwoFactorVerification"`
	ShouldResetPassword           bool   `json:"shouldResetPassword"`
}

func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	now := time.Now()
	if now.Before(c.nextAuthAttempt) {
		// A recent authentication failed; serve the cached error fast instead of
		// hammering the token endpoint on every API call.
		return "", fmt.Errorf("authenticate: backing off until %s after prior failure: %w",
			c.nextAuthAttempt.Format(time.RFC3339), c.lastAuthErr)
	}

	if err := c.authenticateLocked(ctx); err != nil {
		c.lastAuthErr = err
		c.nextAuthAttempt = now.Add(c.authBackoff)
		c.log.Warn("ebeco authentication failed; backing off",
			"err", err,
			"backoff", c.authBackoff.String(),
			"retry_after", c.nextAuthAttempt.Format(time.RFC3339))
		c.authBackoff = min(c.authBackoff*2, authBackoffMax)
		return "", err
	}

	// Success: clear the backoff and report (debug) the new validity window.
	c.authBackoff = authBackoffBase
	c.nextAuthAttempt = time.Time{}
	c.lastAuthErr = nil
	c.log.Debug("ebeco authenticated", "token_valid_until", c.tokenExpiry.Format(time.RFC3339))
	return c.token, nil
}

func (c *Client) authenticateLocked(ctx context.Context) error {
	req := authRequest{
		UserNameOrEmailAddress: c.email,
		Password:               c.password,
		RememberClient:         true, // ask for the longest-lived token
	}
	body, status, err := c.raw(ctx, http.MethodPost, "/api/TokenAuth", req, "")
	if err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}
	if status < 200 || status >= 300 {
		return apiError("authenticate", status, body)
	}
	result, err := unwrap(body)
	if err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}
	var res authResult
	if err := json.Unmarshal(result, &res); err != nil {
		return fmt.Errorf("authenticate: decoding result: %w", err)
	}
	if res.RequiresTwoFactorVerification {
		return fmt.Errorf("authenticate: account requires two-factor verification, which is not supported")
	}
	if res.AccessToken == "" {
		return fmt.Errorf("authenticate: empty access token in response")
	}

	c.token = res.AccessToken
	c.tokenExpiry = time.Now().Add(tokenTTL(res.ExpireInSeconds))
	return nil
}

// tokenTTL returns how long to trust a token, keeping a safety margin before
// the server-stated expiry.
func tokenTTL(expireInSeconds int) time.Duration {
	ttl := time.Duration(expireInSeconds) * time.Second
	switch {
	case ttl <= 0:
		return time.Minute // defensive fallback for a missing/zero value
	case ttl > 10*time.Minute:
		return ttl - 5*time.Minute
	default:
		return ttl / 2
	}
}

// do performs an authenticated request, transparently re-authenticating once on
// a 401, then unwraps the envelope into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	token, err := c.ensureToken(ctx)
	if err != nil {
		return err
	}

	respBody, status, err := c.raw(ctx, method, path, body, token)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		// Token rejected — drop it, re-auth and retry exactly once.
		c.log.Info("ebeco token rejected (401); re-authenticating", "method", method, "path", path)
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		if token, err = c.ensureToken(ctx); err != nil {
			return err
		}
		if respBody, status, err = c.raw(ctx, method, path, body, token); err != nil {
			return err
		}
	}
	if status < 200 || status >= 300 {
		return apiError(fmt.Sprintf("ebeco %s %s", method, path), status, respBody)
	}

	result, err := unwrap(respBody)
	if err != nil {
		return fmt.Errorf("ebeco %s %s: %w", method, path, err)
	}
	if out != nil {
		if err := json.Unmarshal(result, out); err != nil {
			return fmt.Errorf("ebeco %s %s: decoding result: %w", method, path, err)
		}
	}
	return nil
}

// raw issues a single HTTP request and returns the body bytes and status.
func (c *Client) raw(ctx context.Context, method, path string, body any, token string) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.tenantID != 0 {
		req.Header.Set("Abp.TenantId", strconv.Itoa(c.tenantID))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// unwrap returns the inner payload of an ABP response envelope
// ({"result":…,"success":…}). If the body is not such an envelope (e.g. a bare
// object or array), it is returned unchanged. A success:false envelope is
// surfaced as an error.
func unwrap(body []byte) (json.RawMessage, error) {
	var env struct {
		Result  json.RawMessage `json:"result"`
		Success *bool           `json:"success"`
		Error   *struct {
			Message string `json:"message"`
			Details string `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		// Not a JSON object — treat the whole body as the payload.
		return body, nil
	}
	if env.Success == nil {
		// No ABP envelope present.
		return body, nil
	}
	if !*env.Success {
		if env.Error != nil && env.Error.Message != "" {
			if env.Error.Details != "" {
				return nil, fmt.Errorf("api error: %s (%s)", env.Error.Message, env.Error.Details)
			}
			return nil, fmt.Errorf("api error: %s", env.Error.Message)
		}
		return nil, fmt.Errorf("api request reported failure")
	}
	return env.Result, nil
}

// apiError builds an error for a non-2xx response, preferring the ABP envelope's
// error message when the body carries one.
func apiError(prefix string, status int, body []byte) error {
	if _, err := unwrap(body); err != nil {
		return fmt.Errorf("%s: http %d: %w", prefix, status, err)
	}
	return fmt.Errorf("%s: http %d: %s", prefix, status, truncate(body))
}

func truncate(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
