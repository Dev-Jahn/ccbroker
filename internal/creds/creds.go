// Package creds models the Claude Code credential file and the broker's stored record.
package creds

import "encoding/json"

// Health states a credential can be in (design S4). The zero value "" means a
// pre-v0.4 record whose health is derived from the legacy Dead bool.
const (
	HealthOK      = "ok"
	HealthSuspect = "suspect"
	HealthDead    = "dead"
)

// File mirrors the on-disk ~/.claude/.credentials.json layout used by Claude Code.
type File struct {
	ClaudeAiOauth map[string]any `json:"claudeAiOauth"`
}

// Record is one managed credential in the broker store. OAuth holds the full
// claudeAiOauth object with full fidelity (unknown fields preserved), including
// the refresh token — which, per invariant I1, is stripped before serving.
type Record struct {
	Name        string         `json:"name"`
	Account     string         `json:"account,omitempty"`     // email (routing fallback)
	AccountUUID string         `json:"accountUuid,omitempty"` // primary offer-routing key (A4)
	OAuth       map[string]any `json:"oauth"`
	// Gen is a persisted strictly-increasing per-cred generation counter
	// (design S3/C-2): every mutation sets it to max(storedGen+1, nowUnixMilli),
	// so it survives restart, clock steps and sub-ms collisions, and long-pollers
	// wake on any change.
	Gen int64 `json:"gen,omitempty"`
	// HealthState is ok / suspect / dead (design S4). Empty on a pre-v0.4 record;
	// use Health() to read the normalized value.
	HealthState string `json:"healthState,omitempty"`
	HealthSince int64  `json:"healthSince,omitempty"` // unix ms of the last health transition
	// RetiredHashes is the anti-rollback ring: sha256-hex of the last ≤8 retired
	// access tokens (design S2d). A re-offered retired token is rejected.
	RetiredHashes []string `json:"retiredHashes,omitempty"`
	UpdatedAt     int64    `json:"updatedAt"`      // unix ms
	Dead          bool     `json:"dead,omitempty"` // legacy v0.3 field; migrated into HealthState at startup (S8)
	LastError     string   `json:"lastError,omitempty"`
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

// AccessToken returns the current access token.
func (r *Record) AccessToken() string { return str(r.OAuth, "accessToken") }

// RefreshToken returns the current refresh token.
func (r *Record) RefreshToken() string { return str(r.OAuth, "refreshToken") }

// Health returns the normalized health state, translating a pre-v0.4 record
// (empty HealthState) via its legacy Dead bool.
func (r *Record) Health() string {
	if r.HealthState != "" {
		return r.HealthState
	}
	if r.Dead {
		return HealthDead
	}
	return HealthOK
}

// ExpiresAtMs returns token expiry in unix ms (0 if unknown).
func (r *Record) ExpiresAtMs() int64 {
	if r.OAuth == nil {
		return 0
	}
	switch v := r.OAuth["expiresAt"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// ServedOAuth returns a copy of the claudeAiOauth object suitable for serving to
// clients. The refresh token is removed unless includeRefresh is set (the
// rollout compat escape hatch, config serveRefreshToken — design S1/M-7). Every
// other field (accessToken/expiresAt/scopes/subscriptionType/rateLimitTier/…)
// is preserved.
func (r *Record) ServedOAuth(includeRefresh bool) map[string]any {
	out := make(map[string]any, len(r.OAuth))
	for k, v := range r.OAuth {
		out[k] = v
	}
	if !includeRefresh {
		delete(out, "refreshToken")
	}
	return out
}
