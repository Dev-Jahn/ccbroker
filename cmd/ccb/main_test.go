package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func TestRenderStatusShowsHealth(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active")
	if err := writeActive(active, "okk"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Agent{ActiveFile: active, AutoThreshold: 0.95}
	future := time.Now().UnixMilli() + 3_600_000
	rows := []usageRow{
		{Name: "sus", Health: "suspect", ExpiresAt: future},
		{Name: "ded", Health: "dead", ExpiresAt: future},
		{Name: "okk", Health: "ok", ExpiresAt: future},
	}
	out := captureStdout(t, func() { renderStatus(cfg, rows) })
	if !strings.Contains(out, "SUSPECT") {
		t.Errorf("ccb status should surface SUSPECT health:\n%s", out)
	}
	if !strings.Contains(out, "DEAD") {
		t.Errorf("ccb status should surface DEAD health:\n%s", out)
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
				FiveHour: &anthropic.Bucket{Utilization: 0.12}, // 12% → LOW
				// SevenDay resets 2h35m ahead → "↻2h35m".
				SevenDay: &anthropic.Bucket{Utilization: 0.71, ResetsAt: nowMs + 9_300_000}, // 71% → MID
				ScopedWeekly: map[string]anthropic.Bucket{
					"Fable":  {Utilization: 0.87}, // 87% → HIGH
					"Sonnet": {Utilization: 0.05}, // 5% → LOW
				},
			}},
			{Name: "bravo", Usage: &anthropic.Usage{
				// SevenDay reset already past → no ↻.
				SevenDay: &anthropic.Bucket{Utilization: 0.50, ResetsAt: nowMs - 60_000}, // 50% → MID
			}},
			{Name: "charlie", Dead: true},
		},
	}
	orig := append([]usageRow(nil), cache.Credentials...)
	line := renderStatuslineAll("bravo", cache, nowMs)

	// Active first, then the live account, then the dead one — regardless of
	// cache order (alpha comes first in the cache).
	ia, ib, ic := strings.Index(line, "alpha"), strings.Index(line, "bravo"), strings.Index(line, "charlie")
	if ia < 0 || ib < 0 || ic < 0 || !(ib < ia && ia < ic) {
		t.Fatalf("names missing or out of order (alpha=%d bravo=%d charlie=%d):\n%q", ia, ib, ic, line)
	}
	// The caller's slice must not be reordered.
	for i := range orig {
		if cache.Credentials[i].Name != orig[i].Name {
			t.Fatalf("cache.Credentials reordered: %v", cache.Credentials)
		}
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
	if !strings.Contains(line, slHIGH+"87%") {
		t.Errorf("HIGH color for 87%% missing:\n%q", line)
	}
	// Separators between the three creds.
	if n := strings.Count(line, slSEP); n != 2 {
		t.Errorf("separator count=%d want 2:\n%q", n, line)
	}
	// Reset countdown: only alpha's future SevenDay reset gets ↻; bravo's is
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

// slRow builds a live usageRow with the given 5h and 7d utilizations plus one
// weekly bucket per extra utilization ("Model0", "Model1", ...).
func slRow(name string, five, seven float64, weekly ...float64) usageRow {
	u := &anthropic.Usage{
		FiveHour: &anthropic.Bucket{Utilization: five},
		SevenDay: &anthropic.Bucket{Utilization: seven},
	}
	for i, w := range weekly {
		if u.ScopedWeekly == nil {
			u.ScopedWeekly = map[string]anthropic.Bucket{}
		}
		u.ScopedWeekly[fmt.Sprintf("Model%d", i)] = anthropic.Bucket{Utilization: w}
	}
	return usageRow{Name: name, Usage: u}
}

func TestStatuslineOrder(t *testing.T) {
	dead := usageRow{Name: "dd", Dead: true, Usage: &anthropic.Usage{
		FiveHour: &anthropic.Bucket{Utilization: 0.01}, // low usage must not save it
	}}
	noUsage := usageRow{Name: "nu"}
	cases := []struct {
		name   string
		active string
		rows   []usageRow
		want   []string
	}{
		{
			name: "weekly utilization outranks 7d and 5h",
			rows: []usageRow{slRow("weekly-hot", 0, 0, 0.9), slRow("account-hot", 0.99, 0.99, 0.1)},
			want: []string{"account-hot", "weekly-hot"},
		},
		{
			name: "the most constrained weekly bucket is the one compared",
			rows: []usageRow{slRow("a", 0, 0, 0.1, 0.8), slRow("b", 0, 0, 0.5, 0.5)},
			want: []string{"b", "a"},
		},
		{
			name: "no weekly buckets counts as 0",
			rows: []usageRow{slRow("has-weekly", 0, 0, 0.2), slRow("no-weekly", 0, 0.9)},
			want: []string{"no-weekly", "has-weekly"},
		},
		{
			name: "7d breaks a weekly tie",
			rows: []usageRow{slRow("a", 0.9, 0.4, 0.5), slRow("b", 0.1, 0.3, 0.5)},
			want: []string{"b", "a"},
		},
		{
			name: "5h breaks a weekly+7d tie",
			rows: []usageRow{slRow("a", 0.6, 0.3, 0.5), slRow("b", 0.2, 0.3, 0.5)},
			want: []string{"b", "a"},
		},
		{
			name: "name breaks a full tie",
			rows: []usageRow{slRow("zeta", 0.2, 0.3, 0.5), slRow("alpha", 0.2, 0.3, 0.5)},
			want: []string{"alpha", "zeta"},
		},
		{
			name: "dead and usage-less sort last, by name",
			rows: []usageRow{noUsage, dead, slRow("busy", 0.99, 0.99, 0.99)},
			want: []string{"busy", "dd", "nu"},
		},
		{
			name:   "active is leftmost regardless of usage",
			active: "busy",
			rows:   []usageRow{slRow("idle", 0, 0, 0), slRow("busy", 0.9, 0.9, 0.9)},
			want:   []string{"busy", "idle"},
		},
		{
			name:   "active goes first even when dead",
			active: "dd",
			rows:   []usageRow{slRow("idle", 0, 0, 0), dead, noUsage},
			want:   []string{"dd", "idle", "nu"},
		},
		{
			name:   "unknown active just sorts everything",
			active: "gone",
			rows:   []usageRow{slRow("b", 0, 0, 0.5), slRow("a", 0, 0, 0.2)},
			want:   []string{"a", "b"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := make([]string, len(c.rows))
			for i, r := range c.rows {
				before[i] = r.Name
			}
			got := make([]string, 0, len(c.rows))
			for _, r := range statuslineOrder(c.rows, c.active) {
				got = append(got, r.Name)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("order=%v want %v", got, c.want)
			}
			for i, r := range c.rows {
				if r.Name != before[i] {
					t.Fatalf("input slice mutated: %v want %v", c.rows, before)
				}
			}
		})
	}
}

func TestStatuslineCollapseAndCountdown(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	at := func(sec int64) int64 { return nowMs + sec*1000 }
	cases := []struct {
		name    string
		usage   *anthropic.Usage
		present []string
		absent  []string
	}{
		{
			name: "nothing maxed: every segment plus the 7d countdown",
			usage: &anthropic.Usage{
				FiveHour:     &anthropic.Bucket{Utilization: 0.10, ResetsAt: at(3600)},
				SevenDay:     &anthropic.Bucket{Utilization: 0.40, ResetsAt: at(3*86400 + 2*3600)},
				ScopedWeekly: map[string]anthropic.Bucket{"Fable": {Utilization: 0.20, ResetsAt: at(5 * 86400)}},
			},
			present: []string{"5h:", "10%", "7d:", "40%", "F:", "20%", "↻3d2h"},
		},
		{
			name: "0.996 displays as 100% so it collapses, and drives the countdown",
			usage: &anthropic.Usage{
				FiveHour:     &anthropic.Bucket{Utilization: 0.996, ResetsAt: at(45 * 60)},
				SevenDay:     &anthropic.Bucket{Utilization: 0.40, ResetsAt: at(10 * 60)}, // sooner but dropped
				ScopedWeekly: map[string]anthropic.Bucket{"Fable": {Utilization: 0.20}},
			},
			present: []string{"5h:", "100%", "↻45m"},
			absent:  []string{"7d:", "40%", "F:", "20%", "↻10m"},
		},
		{
			name: "several maxed windows are all kept, in order, earliest reset wins",
			usage: &anthropic.Usage{
				FiveHour:     &anthropic.Bucket{Utilization: 1.00, ResetsAt: at(2 * 3600)},
				SevenDay:     &anthropic.Bucket{Utilization: 0.50, ResetsAt: at(60)},
				ScopedWeekly: map[string]anthropic.Bucket{"Fable": {Utilization: 1.37, ResetsAt: at(30 * 60)}},
			},
			present: []string{"5h:", "100%", "F:", "137%", "↻30m"},
			absent:  []string{"7d:", "50%"},
		},
		{
			name: "collapsed with no future reset among the kept windows: no countdown",
			usage: &anthropic.Usage{
				FiveHour: &anthropic.Bucket{Utilization: 1.00, ResetsAt: nowMs - 1000},
				SevenDay: &anthropic.Bucket{Utilization: 0.50, ResetsAt: at(3 * 86400)},
			},
			present: []string{"5h:", "100%"},
			absent:  []string{"7d:", "50%", "↻"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cache := statusCache{FetchedAt: nowMs, Credentials: []usageRow{{Name: "x", Usage: c.usage}}}
			line := renderStatuslineAll("x", cache, nowMs)
			for _, s := range c.present {
				if !strings.Contains(line, s) {
					t.Errorf("missing %q:\n%q", s, line)
				}
			}
			for _, s := range c.absent {
				if strings.Contains(line, s) {
					t.Errorf("unexpected %q:\n%q", s, line)
				}
			}
		})
	}

	// Kept segments preserve their relative order (5h before the weekly one).
	segs, collapsed := statuslineCollapse(statuslineSegments(&anthropic.Usage{
		FiveHour:     &anthropic.Bucket{Utilization: 1.0},
		SevenDay:     &anthropic.Bucket{Utilization: 0.5},
		ScopedWeekly: map[string]anthropic.Bucket{"Fable": {Utilization: 1.2}},
	}))
	if !collapsed || len(segs) != 2 {
		t.Fatalf("collapsed=%v segs=%d want true/2", collapsed, len(segs))
	}
	if !strings.Contains(segs[0].text, "5h:") || !strings.Contains(segs[1].text, "F:") {
		t.Errorf("kept segments out of order: %q %q", segs[0].text, segs[1].text)
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

func TestHTTPClientTimeouts(t *testing.T) {
	client, err := httpClient(&config.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	// Overall request budget stays at 30s.
	if client.Timeout != 30*time.Second {
		t.Errorf("Client.Timeout=%s want 30s", client.Timeout)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T want *http.Transport", client.Transport)
	}
	// Fail-fast when the broker is unreachable: short dial + TLS-handshake timeouts.
	if tr.DialContext == nil {
		t.Error("DialContext not set (dial timeout would be unbounded)")
	}
	if tr.TLSHandshakeTimeout != 5*time.Second {
		t.Errorf("TLSHandshakeTimeout=%s want 5s", tr.TLSHandshakeTimeout)
	}
	// Empty proxyUrl keeps the stdlib default our custom Transport would
	// otherwise drop: proxy env vars are honored.
	if tr.Proxy == nil {
		t.Error("Proxy not set; want http.ProxyFromEnvironment")
	}
}

func TestHTTPClientProxyURL(t *testing.T) {
	client, err := httpClient(&config.Agent{ProxyURL: "socks5://localhost:1055"})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T want *http.Transport", client.Transport)
	}
	req, err := http.NewRequest(http.MethodGet, "http://broker.example.com/v1/usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := tr.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.String() != "socks5://localhost:1055" {
		t.Errorf("Proxy(req)=%v want socks5://localhost:1055", u)
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
