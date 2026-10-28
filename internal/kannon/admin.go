package kannon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Admin reads from Kannon's Admin and Stats APIs, for the delivery panel on the admin pages.
// It only ever reads, but Kannon has no read-only credential: both APIs answer to the one
// admin token.
type Admin struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Count is one aggregated counter: how many Deliveries of a type, in the hour starting at At.
// It names no recipient.
type Count struct {
	Type  string
	At    time.Time
	Count int64
}

// AggregatedStats returns the Domain's counters between from and to.
func (a *Admin) AggregatedStats(ctx context.Context, domain string, from, to time.Time) ([]Count, error) {
	var res struct {
		Stats []struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Count     string    `json:"count"` // int64, which protojson writes as a string
		} `json:"stats"`
	}
	err := a.call(ctx, "/kannon.stats.apiv2.StatsApiV2/GetAggregatedStats", map[string]any{
		"domain": domain, "fromDate": from.UTC().Format(time.RFC3339), "toDate": to.UTC().Format(time.RFC3339),
	}, &res)
	if err != nil {
		return nil, err
	}
	out := make([]Count, 0, len(res.Stats))
	for _, s := range res.Stats {
		n, err := strconv.ParseInt(s.Count, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("kannon: count %q: %w", s.Count, err)
		}
		out = append(out, Count{Type: s.Type, At: s.Timestamp, Count: n})
	}
	return out, nil
}

// Tracking is a Domain's Tracking Policy, as Kannon names the modes (TRACKING_MODE_OFF, ...).
type Tracking struct {
	Opens string `json:"opens"`
	Links string `json:"links"`
}

// Off reports whether neither opens nor links are tracked.
func (t Tracking) Off() bool {
	return t.Opens == "TRACKING_MODE_OFF" && t.Links == "TRACKING_MODE_OFF"
}

func (a *Admin) DomainTracking(ctx context.Context, domain string) (Tracking, error) {
	var res struct {
		Domain struct {
			Tracking Tracking `json:"tracking"`
		} `json:"domain"`
	}
	err := a.call(ctx, "/pkg.kannon.admin.apiv1.Api/GetDomain", map[string]string{"domain": domain}, &res)
	return res.Domain.Tracking, err
}

func (a *Admin) call(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.URL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kannon-Admin-Token", a.Token)
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return &RefusedError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return json.Unmarshal(data, out)
}
