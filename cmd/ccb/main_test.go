package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dev-Jahn/ccbroker/internal/anthropic"
	"github.com/Dev-Jahn/ccbroker/internal/config"
)

func TestUsageMetric(t *testing.T) {
	// nil Usage scores 0 under every policy.
	if got := usageMetric(nil, "account"); got != 0 {
		t.Errorf("nil account: got %v want 0", got)
	}
	if got := usageMetric(nil, "all"); got != 0 {
		t.Errorf("nil all: got %v want 0", got)
	}

	u := &anthropic.Usage{
		FiveHour:     &anthropic.Bucket{Utilization: 0.2},
		SevenDay:     &anthropic.Bucket{Utilization: 0.3},
		ScopedWeekly: map[string]anthropic.Bucket{"Fable": {Utilization: 0.9}},
	}
	if got := usageMetric(u, "account"); got != 0.3 {
		t.Errorf("account: got %v want 0.3", got)
	}
	if got := usageMetric(u, "all"); got != 0.9 {
		t.Errorf("all: got %v want 0.9", got)
	}
}

func TestAutoSelectPolicy(t *testing.T) {
	active := filepath.Join(t.TempDir(), "active")
	cfg := &config.Agent{ActiveFile: active, AutoThreshold: 0.95}
	future := time.Now().UnixMilli() + 3_600_000
	rows := []usageRow{
		{Name: "A", ExpiresAt: future, Usage: &anthropic.Usage{
			FiveHour:     &anthropic.Bucket{Utilization: 0.2},
			SevenDay:     &anthropic.Bucket{Utilization: 0.3},
			ScopedWeekly: map[string]anthropic.Bucket{"Fable": {Utilization: 1.0}},
		}},
		{Name: "B", ExpiresAt: future, Usage: &anthropic.Usage{
			FiveHour: &anthropic.Bucket{Utilization: 0.5},
			SevenDay: &anthropic.Bucket{Utilization: 0.5},
		}},
	}

	if err := writeActive(active, "A"); err != nil {
		t.Fatal(err)
	}

	// account policy: A's account-wide windows are under threshold → kept.
	if err := autoSelect(cfg, rows, "account"); err != nil {
		t.Fatal(err)
	}
	if got := readActive(cfg); got != "A" {
		t.Errorf("account policy: active=%q want A", got)
	}

	// all policy: A's per-model weekly bucket is maxed → switch to B (lower).
	if err := autoSelect(cfg, rows, "all"); err != nil {
		t.Fatal(err)
	}
	if got := readActive(cfg); got != "B" {
		t.Errorf("all policy: active=%q want B", got)
	}
}

func TestRunPolicySet(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.json")
	orig := `{
  "brokerUrl": "https://b",
  "token": "t",
  "intervalSec": 1800,
  "auto": true,
  "x": 1
}
`
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPolicy(p, []string{"all"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// intervalSec must round-trip as an integer, not degrade to 1.8e+03.
	if !strings.Contains(string(b), `"intervalSec": 1800`) {
		t.Errorf("intervalSec not preserved as int:\n%s", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["autoPolicy"] != "all" {
		t.Errorf("autoPolicy=%v want all", m["autoPolicy"])
	}
	if _, ok := m["auto"]; ok {
		t.Errorf("legacy auto key not removed")
	}
	if m["x"] != float64(1) {
		t.Errorf("x=%v want 1 (unknown field must survive)", m["x"])
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode=%o want 600", perm)
	}
}
