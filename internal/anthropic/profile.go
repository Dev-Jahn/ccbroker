package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type rawProfile struct {
	Account struct {
		UUID      string `json:"uuid"`
		Email     string `json:"email"`
		CreatedAt string `json:"created_at"`
	} `json:"account"`
	Organization struct {
		UUID                    string          `json:"uuid"`
		Name                    string          `json:"name"`
		OrganizationType        string          `json:"organization_type"`
		BillingType             string          `json:"billing_type"`
		RateLimitTier           string          `json:"rate_limit_tier"`
		SeatTier                any             `json:"seat_tier"`
		HasExtraUsageEnabled    bool            `json:"has_extra_usage_enabled"`
		SubscriptionCreatedAt   string          `json:"subscription_created_at"`
		CcOnboardingFlags       json.RawMessage `json:"cc_onboarding_flags"`
		ClaudeCodeTrialEndsAt   any             `json:"claude_code_trial_ends_at"`
		ClaudeCodeTrialDuration any             `json:"claude_code_trial_duration_days"`
	} `json:"organization"`
}

// FetchProfile returns the account identity shaped exactly like Claude Code's
// `.claude.json` "oauthAccount" object, so a client can write it verbatim after
// switching accounts. profileFetchedAt is filled by the caller (the store has
// no clock dependency); pass nowMs. A non-2xx response is returned as a
// *StatusError so the caller can classify auth failures apart from transients.
func FetchProfile(ctx context.Context, accessToken string, nowMs int64) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, profileURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	var p rawProfile
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("profile decode: %w", err)
	}
	var flags any
	if len(p.Organization.CcOnboardingFlags) > 0 {
		_ = json.Unmarshal(p.Organization.CcOnboardingFlags, &flags)
	}
	if flags == nil {
		flags = map[string]any{}
	}
	return map[string]any{
		"accountUuid":                 p.Account.UUID,
		"emailAddress":                p.Account.Email,
		"organizationUuid":            p.Organization.UUID,
		"hasExtraUsageEnabled":        p.Organization.HasExtraUsageEnabled,
		"billingType":                 p.Organization.BillingType,
		"accountCreatedAt":            p.Account.CreatedAt,
		"subscriptionCreatedAt":       p.Organization.SubscriptionCreatedAt,
		"ccOnboardingFlags":           flags,
		"claudeCodeTrialEndsAt":       p.Organization.ClaudeCodeTrialEndsAt,
		"claudeCodeTrialDurationDays": p.Organization.ClaudeCodeTrialDuration,
		"seatTier":                    p.Organization.SeatTier,
		"profileFetchedAt":            nowMs,
		"organizationType":            p.Organization.OrganizationType,
		"organizationRateLimitTier":   p.Organization.RateLimitTier,
		"userRateLimitTier":           nil,
		"organizationName":            p.Organization.Name,
	}, nil
}
