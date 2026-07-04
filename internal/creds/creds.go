// Package creds models the Claude Code credential file and the broker's stored record.
package creds

import "encoding/json"

// File mirrors the on-disk ~/.claude/.credentials.json layout used by Claude Code.
type File struct {
	ClaudeAiOauth map[string]any `json:"claudeAiOauth"`
}

// Record is one managed credential in the broker store. OAuth holds the full
// claudeAiOauth object with full fidelity (unknown fields preserved).
type Record struct {
	Name      string         `json:"name"`
	Account   string         `json:"account,omitempty"`
	OAuth     map[string]any `json:"oauth"`
	UpdatedAt int64          `json:"updatedAt"` // unix ms
	Dead      bool           `json:"dead,omitempty"`
	LastError string         `json:"lastError,omitempty"`
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

// FileBytes renders the record as a .credentials.json payload. Compact,
// single-line JSON matching Claude Code's native format (a multiline value in
// the macOS keychain would also display as hex in `security -w`).
func (r *Record) FileBytes() ([]byte, error) {
	return json.Marshal(File{ClaudeAiOauth: r.OAuth})
}
