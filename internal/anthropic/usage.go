package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UsageURL reports subscription quota utilization for an OAuth access token
// WITHOUT consuming message quota, which makes it safe to poll.
const UsageURL = "https://api.anthropic.com/api/oauth/usage"

// Bucket is one normalized quota window.
type Bucket struct {
	Utilization float64 `json:"utilization"`        // 0-1 (may exceed 1 in overage)
	ResetsAt    int64   `json:"resetsAt,omitempty"` // unix ms, 0 if unknown
}

// Usage is the normalized quota picture for one account.
type Usage struct {
	FiveHour     *Bucket           `json:"fiveHour,omitempty"`
	SevenDay     *Bucket           `json:"sevenDay,omitempty"`
	ScopedWeekly map[string]Bucket `json:"scopedWeekly,omitempty"` // per-model weekly limits, keyed by display name
}

// MaxUtilization returns the highest utilization across the account-wide
// windows (5h/7d). Model-scoped weekly buckets are excluded: a spent
// per-model bucket does not block other models.
func (u *Usage) MaxUtilization() float64 {
	max := 0.0
	if u.FiveHour != nil && u.FiveHour.Utilization > max {
		max = u.FiveHour.Utilization
	}
	if u.SevenDay != nil && u.SevenDay.Utilization > max {
		max = u.SevenDay.Utilization
	}
	return max
}

// MaxUtilizationAll returns the highest utilization across every window,
// including model-scoped weekly buckets (used by the "all" rotation policy).
func (u *Usage) MaxUtilizationAll() float64 {
	max := u.MaxUtilization()
	for _, b := range u.ScopedWeekly {
		if b.Utilization > max {
			max = b.Utilization
		}
	}
	return max
}

// rawBucket tolerates the field spellings the endpoint has used.
type rawBucket struct {
	Utilization    *float64        `json:"utilization"`
	UsedPercentage *float64        `json:"used_percentage"`
	ResetsAt       json.RawMessage `json:"resets_at"`
}

type rawLimit struct {
	Group   string          `json:"group"`
	Percent *float64        `json:"percent"`
	ResetAt json.RawMessage `json:"resets_at"`
	Scope   struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type rawUsage struct {
	FiveHour *rawBucket `json:"five_hour"`
	SevenDay *rawBucket `json:"seven_day"`
	Limits   []rawLimit `json:"limits"`
}

// FetchUsage queries the usage endpoint with an access token.
func FetchUsage(ctx context.Context, accessToken string) (*Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var raw rawUsage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("usage decode: %w", err)
	}
	u := &Usage{
		FiveHour: normalizeBucket(raw.FiveHour),
		SevenDay: normalizeBucket(raw.SevenDay),
	}
	for _, l := range raw.Limits {
		if l.Group != "weekly" || l.Percent == nil || l.Scope.Model.DisplayName == "" {
			continue
		}
		if u.ScopedWeekly == nil {
			u.ScopedWeekly = map[string]Bucket{}
		}
		u.ScopedWeekly[l.Scope.Model.DisplayName] = Bucket{
			Utilization: *l.Percent / 100,
			ResetsAt:    parseResetAt(l.ResetAt),
		}
	}
	return u, nil
}

// normalizeBucket converts a raw bucket (percent 0-100) to Utilization 0-1.
func normalizeBucket(b *rawBucket) *Bucket {
	if b == nil {
		return nil
	}
	pct := b.UsedPercentage
	if pct == nil {
		pct = b.Utilization
	}
	if pct == nil {
		return nil
	}
	return &Bucket{Utilization: *pct / 100, ResetsAt: parseResetAt(b.ResetsAt)}
}

// parseResetAt normalizes resets_at, which may be a unix timestamp in seconds
// or milliseconds (number or numeric string) or an RFC3339 string, to unix ms.
func parseResetAt(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return secOrMsToMs(n)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return secOrMsToMs(n)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func secOrMsToMs(n float64) int64 {
	if n < 1e12 { // plausibly seconds
		return int64(n * 1000)
	}
	return int64(n)
}
