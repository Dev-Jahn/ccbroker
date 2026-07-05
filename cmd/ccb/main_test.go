package main

import (
	"encoding/json"
	"fmt"
	"io"
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

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestRenderStatuslineAll(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	cache := statusCache{
		FetchedAt: nowMs - 2*60*60*1000, // 2h ago → stale (>90min)
		Credentials: []usageRow{
			{Name: "alpha", Usage: &anthropic.Usage{
				// FiveHour resets 2h35m ahead → "↻2h35m".
				FiveHour: &anthropic.Bucket{Utilization: 0.12, ResetsAt: nowMs + 9_300_000}, // 12% → LOW
				SevenDay: &anthropic.Bucket{Utilization: 0.71},                              // 71% → MID
				ScopedWeekly: map[string]anthropic.Bucket{
					"Fable":  {Utilization: 1.37}, // 137% overage → HIGH
					"Sonnet": {Utilization: 0.05}, // 5% → LOW
				},
			}},
			{Name: "bravo", Usage: &anthropic.Usage{
				// FiveHour reset already past → no ↻.
				FiveHour: &anthropic.Bucket{Utilization: 0.50, ResetsAt: nowMs - 60_000}, // 50% → MID
			}},
			{Name: "charlie", Dead: true},
		},
	}
	line := renderStatuslineAll("bravo", cache, nowMs)

	// Names present in cache order.
	ia, ib, ic := strings.Index(line, "alpha"), strings.Index(line, "bravo"), strings.Index(line, "charlie")
	if ia < 0 || ib < 0 || ic < 0 || !(ia < ib && ib < ic) {
		t.Fatalf("names missing or out of order (alpha=%d bravo=%d charlie=%d):\n%q", ia, ib, ic, line)
	}
	// ⛁ marks the active account only; ✗ marks the dead one only.
	if n := strings.Count(line, "⛁"); n != 1 {
		t.Errorf("⛁ count=%d want 1", n)
	}
	if !strings.Contains(line, slACT+"⛁ bravo"+slRST) {
		t.Errorf("active bravo not rendered with ⛁ + ACT:\n%q", line)
	}
	if !strings.Contains(line, slDIM+"alpha"+slRST) {
		t.Errorf("inactive alpha not rendered in DIM:\n%q", line)
	}
	if n := strings.Count(line, "✗"); n != 1 {
		t.Errorf("✗ count=%d want 1", n)
	}
	if !strings.Contains(line, slHIGH+"✗ "+slRST) {
		t.Errorf("dead prefix not rendered in HIGH:\n%q", line)
	}
	// Segment labels: 5h/7d and model first-letters, weekly sorted by name.
	if !strings.Contains(line, "5h:") || !strings.Contains(line, "7d:") {
		t.Errorf("5h:/7d: labels missing:\n%q", line)
	}
	if iF, iS := strings.Index(line, "F:"), strings.Index(line, "S:"); iF < 0 || iS < 0 || iF > iS {
		t.Errorf("model labels missing or unsorted (F:=%d S:=%d):\n%q", iF, iS, line)
	}
	// Three threshold color classes.
	if !strings.Contains(line, slLOW+"12%") {
		t.Errorf("LOW color for 12%% missing:\n%q", line)
	}
	if !strings.Contains(line, slMID+"71%") {
		t.Errorf("MID color for 71%% missing:\n%q", line)
	}
	if !strings.Contains(line, slHIGH+"137%") {
		t.Errorf("HIGH color for 137%% missing:\n%q", line)
	}
	// Separators between the three creds.
	if n := strings.Count(line, slSEP); n != 2 {
		t.Errorf("separator count=%d want 2:\n%q", n, line)
	}
	// Reset countdown: only alpha's future FiveHour reset gets ↻; bravo's is
	// in the past and charlie has no usage, so exactly one ↻ appears.
	if n := strings.Count(line, "↻"); n != 1 {
		t.Errorf("↻ count=%d want 1:\n%q", n, line)
	}
	if !strings.Contains(line, slREM+"↻2h35m"+slRST) {
		t.Errorf("alpha reset countdown ↻2h35m missing:\n%q", line)
	}
	// Stale suffix (fetched 2h ago).
	if !strings.Contains(line, "~stale") {
		t.Errorf("~stale suffix missing:\n%q", line)
	}

	// Fresh cache → no ~stale.
	fresh := statusCache{FetchedAt: nowMs, Credentials: cache.Credentials}
	if strings.Contains(renderStatuslineAll("bravo", fresh, nowMs), "~stale") {
		t.Errorf("fresh cache should not be marked ~stale")
	}
}

func TestFmtRemain(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{95 * 60, "1h35m"},         // 95 min
		{3*86400 + 2*3600, "3d2h"}, // 3d2h exactly
		{45, "0m"},                 // under a minute floors to 0m
		{26 * 3600, "1d2h"},        // 26h rolls into 1d2h
	}
	for _, c := range cases {
		if got := fmtRemain(c.sec); got != c.want {
			t.Errorf("fmtRemain(%d)=%q want %q", c.sec, got, c.want)
		}
	}
}

func TestPrintStatuslineAllCacheMissing(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active")
	if err := writeActive(active, "personal"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Agent{ActiveFile: active} // no status.json in dir → fallback
	out := captureStdout(t, func() { printStatuslineAll(cfg) })
	if strings.TrimSpace(out) != "personal" {
		t.Errorf("cache-missing fallback: got %q want personal", out)
	}
}

func TestStatuslineOnInstallAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	// A big integer must survive the UseNumber round-trip as an integer.
	orig := "{\n  \"theme\": \"dark\",\n  \"big\": 123456789012345\n}\n"
	if err := os.WriteFile(settings, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := resolveExe()
	if err != nil {
		t.Fatal(err)
	}
	target := exe + " statusline --all"

	captureStdout(t, func() {
		if err := statuslineOn(settings); err != nil {
			t.Fatal(err)
		}
	})
	b1, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b1), "123456789012345") {
		t.Errorf("big int not preserved as integer:\n%s", b1)
	}
	var m map[string]any
	if err := json.Unmarshal(b1, &m); err != nil {
		t.Fatal(err)
	}
	sl, ok := m["statusLine"].(map[string]any)
	if !ok {
		t.Fatalf("statusLine missing or not an object: %v", m["statusLine"])
	}
	if sl["command"] != target {
		t.Errorf("command=%v want %q", sl["command"], target)
	}
	if sl["type"] != "command" {
		t.Errorf("type=%v want command", sl["type"])
	}
	if m["theme"] != "dark" {
		t.Errorf("theme not preserved: %v", m["theme"])
	}

	// Running again must be byte-identical.
	captureStdout(t, func() {
		if err := statuslineOn(settings); err != nil {
			t.Fatal(err)
		}
	})
	b2, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Errorf("second on not byte-identical:\n%s\n---\n%s", b1, b2)
	}
}

func TestStatuslineOnOffScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "statusline.sh")
	scriptContent := "#!/bin/bash\necho custom statusline\n"
	if err := os.WriteFile(script, []byte(scriptContent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o755); err != nil { // guarantee 0755 despite umask
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "settings.json")
	settingsContent := fmt.Sprintf("{\n  \"statusLine\": {\n    \"type\": \"command\",\n    \"command\": \"bash %s\"\n  }\n}\n", script)
	if err := os.WriteFile(settings, []byte(settingsContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// on appends the block exactly once, preserving script perms and settings.
	captureStdout(t, func() {
		if err := statuslineOn(settings); err != nil {
			t.Fatal(err)
		}
	})
	afterOn, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(afterOn), markerBegin) || !strings.Contains(string(afterOn), markerBody) || !strings.Contains(string(afterOn), markerEnd) {
		t.Errorf("marker block not present after on:\n%s", afterOn)
	}
	if n := strings.Count(string(afterOn), markerBegin); n != 1 {
		t.Errorf("marker block count=%d want 1:\n%s", n, afterOn)
	}
	if fi, err := os.Stat(script); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("script perm=%o want 755 after on", perm)
	}
	if s, err := os.ReadFile(settings); err != nil {
		t.Fatal(err)
	} else if string(s) != settingsContent {
		t.Errorf("settings.json must be untouched by script edit:\n%s", s)
	}

	// on again → byte-identical script.
	captureStdout(t, func() {
		if err := statuslineOn(settings); err != nil {
			t.Fatal(err)
		}
	})
	afterOn2, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterOn) != string(afterOn2) {
		t.Errorf("second on not byte-identical:\n%q\n---\n%q", afterOn, afterOn2)
	}

	// off removes the block exactly, restoring pre-on bytes and perms.
	captureStdout(t, func() {
		if err := statuslineOff(settings); err != nil {
			t.Fatal(err)
		}
	})
	afterOff, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterOff) != scriptContent {
		t.Errorf("off did not restore pre-on content:\ngot  %q\nwant %q", afterOff, scriptContent)
	}
	if fi, err := os.Stat(script); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("script perm=%o want 755 after off", perm)
	}

	// off again → no-op, script unchanged.
	captureStdout(t, func() {
		if err := statuslineOff(settings); err != nil {
			t.Fatal(err)
		}
	})
	afterOff2, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterOff2) != scriptContent {
		t.Errorf("second off changed the script:\n%q", afterOff2)
	}
}

func TestStatuslineOnUnlocatable(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	content := "{\n  \"statusLine\": {\n    \"type\": \"command\",\n    \"command\": \"some-missing-binary --flag\"\n  }\n}\n"
	if err := os.WriteFile(settings, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statuslineOn(settings); err == nil {
		t.Fatal("expected error for a statusLine command with no locatable script")
	}
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != content {
		t.Errorf("settings.json must be untouched on error:\n%s", b)
	}
}
