package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAgent(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAgentInvalidAutoPolicy(t *testing.T) {
	p := writeAgent(t, `{"brokerUrl":"https://b","token":"t","autoPolicy":"weird"}`)
	if _, err := LoadAgent(p); err == nil {
		t.Fatal("expected error for invalid autoPolicy")
	}
}

func TestLoadAgentValidAutoPolicy(t *testing.T) {
	for _, pol := range []string{"", "manual", "account", "all"} {
		p := writeAgent(t, `{"brokerUrl":"https://b","token":"t","autoPolicy":"`+pol+`"}`)
		if _, err := LoadAgent(p); err != nil {
			t.Fatalf("autoPolicy %q: unexpected error %v", pol, err)
		}
	}
}

func TestLoadServerDefaultRefreshSkew(t *testing.T) {
	p := filepath.Join(t.TempDir(), "server.json")
	// refreshSkewSec absent → must default to 3600 (comfortably above the
	// default 1800s agent pull interval so rotated tokens land while valid).
	if err := os.WriteFile(p, []byte(`{"storePath":"/x/store.json","keyPath":"/x/key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadServer(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.RefreshSkewSec != 3600 {
		t.Errorf("RefreshSkewSec default = %d, want 3600", c.RefreshSkewSec)
	}
}

func TestEffectivePolicy(t *testing.T) {
	cases := []struct {
		policy string
		auto   bool
		want   string
	}{
		{"all", true, "all"},        // explicit autoPolicy wins over legacy auto:true
		{"manual", false, "manual"}, // explicit manual honored
		{"", true, "account"},       // legacy auto:true → account
		{"", false, "manual"},       // neither set → manual
	}
	for _, c := range cases {
		a := &Agent{AutoPolicy: c.policy, Auto: c.auto}
		if got := a.EffectivePolicy(); got != c.want {
			t.Errorf("AutoPolicy=%q Auto=%v: got %q want %q", c.policy, c.auto, got, c.want)
		}
	}
}
