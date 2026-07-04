package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ProfileURL reports the account/organization identity for an OAuth access
// token. Free to call (no message-quota cost).
const ProfileURL = "https://api.anthropic.com/api/oauth/profile"

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
// no clock dependency); pass nowMs.
func FetchProfile(ctx context.Context, accessToken string, nowMs int64) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ProfileURL, nil)
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
		return nil, fmt.Errorf("profile http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
