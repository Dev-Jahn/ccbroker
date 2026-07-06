package creds

import "testing"

func TestServedOAuthStrip(t *testing.T) {
	r := &Record{OAuth: map[string]any{
		"accessToken": "a", "refreshToken": "r", "expiresAt": float64(1), "subscriptionType": "pro",
	}}
	// Default: refresh token stripped, everything else preserved (invariant I1).
	got := r.ServedOAuth(false)
	if _, ok := got["refreshToken"]; ok {
		t.Errorf("ServedOAuth(false) leaked refreshToken: %v", got)
	}
	if got["accessToken"] != "a" || got["subscriptionType"] != "pro" {
		t.Errorf("ServedOAuth(false) dropped non-secret fields: %v", got)
	}
	// The source map must not be mutated.
	if _, ok := r.OAuth["refreshToken"]; !ok {
		t.Errorf("ServedOAuth mutated the stored record")
	}
	// Escape hatch keeps the refresh token.
	if r.ServedOAuth(true)["refreshToken"] != "r" {
		t.Errorf("ServedOAuth(true) should retain refreshToken")
	}
}

func TestHealthNormalization(t *testing.T) {
	cases := []struct {
		state string
		dead  bool
		want  string
	}{
		{"", false, HealthOK},       // legacy record, alive
		{"", true, HealthDead},      // legacy record, dead bool
		{HealthOK, false, HealthOK}, // explicit ok wins over dead bool
		{HealthSuspect, false, HealthSuspect},
		{HealthDead, false, HealthDead},
	}
	for _, c := range cases {
		r := &Record{HealthState: c.state, Dead: c.dead}
		if got := r.Health(); got != c.want {
			t.Errorf("Health(state=%q dead=%v)=%q want %q", c.state, c.dead, got, c.want)
		}
	}
}
