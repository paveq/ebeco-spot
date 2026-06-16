// Package spothinta fetches heating on/off schedules from the spot-hinta.fi
// PlanAhead endpoint, mirroring the request built by the Shelly script.
package spothinta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.spot-hinta.fi"

// Period is one entry of the PlanAhead response: a period start (epoch ms) and
// whether heating should be on during it.
type Period struct {
	EpochMs int64 `json:"epochMs"`
	Result  bool  `json:"result"`
}

// Params are the PlanAhead query parameters.
type Params struct {
	Region             string
	PriorityHours      []int
	PriceModifier      int
	RanksAllowed       []string
	RankDuration       int
	PriceAlwaysAllowed int
	MaxPrice           int
}

// Client calls the spot-hinta.fi API.
type Client struct {
	baseURL string
	http    *http.Client
	log     *slog.Logger
}

// New returns a client targeting the public spot-hinta.fi API. A nil logger
// disables logging.
func New(log *slog.Logger) *Client {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{baseURL: defaultBaseURL, http: &http.Client{Timeout: 20 * time.Second}, log: log}
}

// PlanAhead returns the heating schedule. The returned slice is in the order
// the API provides it (callers sort as needed).
func (c *Client) PlanAhead(ctx context.Context, p Params) ([]Period, error) {
	// Built by hand so the comma-separated lists stay as literal commas, exactly
	// like the example URL.
	u := fmt.Sprintf("%s/PlanAhead?region=%s&priorityHours=%s&priceModifier=%d&ranksAllowed=%s&rankDuration=%d&priceAlwaysAllowed=%d&maxPrice=%d",
		c.baseURL,
		url.QueryEscape(p.Region),
		joinInts(p.PriorityHours),
		p.PriceModifier,
		strings.Join(p.RanksAllowed, ","),
		p.RankDuration,
		p.PriceAlwaysAllowed,
		p.MaxPrice,
	)
	c.log.Debug("spot-hinta PlanAhead request", "url", u)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("spot-hinta PlanAhead: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("spot-hinta PlanAhead: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spot-hinta PlanAhead: http %d: %s", resp.StatusCode, truncate(body))
	}

	var periods []Period
	if err := json.Unmarshal(body, &periods); err != nil {
		return nil, fmt.Errorf("spot-hinta PlanAhead: decoding %q: %w", truncate(body), err)
	}
	return periods, nil
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

func truncate(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
