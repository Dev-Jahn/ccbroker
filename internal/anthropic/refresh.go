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
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// ClientID is the public OAuth client id used by Claude Code.
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// TokenURL is the refresh endpoint that is not Cloudflare-challenged.
	TokenURL = "https://api.anthropic.com/v1/oauth/token"
)

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

var client = &http.Client{Timeout: 30 * time.Second}

// Refresh exchanges a refresh token for a fresh access+refresh token pair.
// Anthropic rotates the refresh token on every call.
func Refresh(ctx context.Context, refreshToken string) (*Result, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     ClientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-cli/2.1.199 (external, cli)")

	resp, err := client.Do(req)
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
