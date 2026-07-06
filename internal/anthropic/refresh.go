// Package anthropic performs the OAuth refresh_token grant against Anthropic.
//
// The refresh endpoint on console.anthropic.com sits behind a Cloudflare
// managed challenge that blocks programmatic clients; api.anthropic.com does
// not, so we use that host (verified working with the Claude Code public
// client id).
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ClientID is the public OAuth client id used by Claude Code.
const ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// Endpoint is the Anthropic host serving the OAuth endpoints. It is a var (not
// a const) so integration tests can point every call at an httptest fake
// upstream; production always uses api.anthropic.com, which — unlike the
// documented console.anthropic.com host — is not behind a Cloudflare challenge.
var Endpoint = "https://api.anthropic.com"

// HTTPClient performs every OAuth request. Overridable in tests.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

func tokenURL() string   { return Endpoint + "/v1/oauth/token" }
func profileURL() string { return Endpoint + "/api/oauth/profile" }
func usageURL() string   { return Endpoint + "/api/oauth/usage" }

// StatusError is a non-2xx HTTP response from a free OAuth endpoint
// (profile/usage). It carries the status code so callers can classify auth
// failures (401 / 403-revoked) apart from transient ones (429/5xx/network) —
// only the former count toward a credential's health (design S7).
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, truncate(e.Body, 200))
}

// Status returns the HTTP status carried by err, or 0 for a network-level
// error (no response at all).
func Status(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// Revoked reports whether err is a 403 whose body indicates the OAuth token was
// revoked. Claude Code treats this like a 401 (design CC8/F1), attributing it to
// another process having refreshed (and thereby rotated) the token.
func Revoked(err error) bool {
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusForbidden {
		return strings.Contains(strings.ToLower(se.Body), "revoked")
	}
	return false
}

// AuthFailure reports whether err means the access token is no longer valid: a
// 401, or a 403 whose body says "revoked". Transient statuses (429/5xx) and
// network errors are not auth failures and must never affect health (S7).
func AuthFailure(err error) bool {
	if err == nil {
		return false
	}
	return Status(err) == http.StatusUnauthorized || Revoked(err)
}

// Result is the successful refresh response.
type Result struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	Account      struct {
		EmailAddress string `json:"email_address"`
		UUID         string `json:"uuid"`
	} `json:"account"`
}

// Err distinguishes permanent failures (bad/revoked refresh token → needs
// re-auth) from transient ones (429, 5xx, network) that should be retried.
type Err struct {
	Status    int
	Code      string
	Body      string
	Permanent bool
}

func (e *Err) Error() string {
	return fmt.Sprintf("refresh http=%d code=%q permanent=%v body=%s",
		e.Status, e.Code, e.Permanent, truncate(e.Body, 300))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Refresh exchanges a refresh token for a fresh access+refresh token pair.
// Anthropic rotates the refresh token on every call.
func Refresh(ctx context.Context, refreshToken string) (*Result, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     ClientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-cli/2.1.199 (external, cli)")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, &Err{Status: 0, Body: err.Error(), Permanent: false}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusOK {
		var r Result
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, &Err{Status: resp.StatusCode, Body: "bad json: " + err.Error()}
		}
		if r.AccessToken == "" {
			return nil, &Err{Status: resp.StatusCode, Body: "no access_token in response"}
		}
		return &r, nil
	}

	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &e)
	// Only treat explicit auth-invalidation as permanent; everything else
	// (including bare 400/429/5xx) is retried so a fluke doesn't kill a cred.
	permanent := e.Error == "invalid_grant" || e.Error == "invalid_client" || e.Error == "unauthorized_client"
	return nil, &Err{Status: resp.StatusCode, Code: e.Error, Body: string(raw), Permanent: permanent}
}
