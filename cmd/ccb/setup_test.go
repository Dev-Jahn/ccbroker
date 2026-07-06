package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dev-Jahn/ccbroker/internal/config"
)

func TestRunSetupWritesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // isolate any ~ expansion (scheduler paths) into temp
	p := filepath.Join(dir, "agent.json")

	var in strings.Builder
	in.WriteString("https://broker.example.com\n") // broker url
	in.WriteString("tok-abc\n")                    // client token
	in.WriteString("\n")                           // CA cert (empty default)
	in.WriteString("\n")                           // client cert (empty default)
	in.WriteString("\n")                           // proxy URL (empty = none/env)
	if runtime.GOOS == "darwin" {
		in.WriteString("2\n") // target menu (darwin only): credentials file
	}
	in.WriteString("\n")  // credentials file path (default)
	in.WriteString("\n")  // interval (default 1800)
	in.WriteString("3\n") // policy: all
	in.WriteString("\n")  // threshold (default 0.95)
	in.WriteString("n\n") // test pull? no
	in.WriteString("n\n") // scheduler? no

	var out strings.Builder
	if err := runSetup(p, strings.NewReader(in.String()), &out); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Agent
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.BrokerURL != "https://broker.example.com" {
		t.Errorf("brokerUrl=%q", cfg.BrokerURL)
	}
	if cfg.Token != "tok-abc" {
		t.Errorf("token=%q", cfg.Token)
	}
	if cfg.IntervalSec != 1800 {
		t.Errorf("intervalSec=%d want 1800", cfg.IntervalSec)
	}
	if cfg.AutoPolicy != "all" {
		t.Errorf("autoPolicy=%q want all", cfg.AutoPolicy)
	}
	if cfg.AutoThreshold != 0.95 {
		t.Errorf("autoThreshold=%v want 0.95", cfg.AutoThreshold)
	}
	if cfg.ProxyURL != "" {
		t.Errorf("proxyUrl=%q want empty (prompt skipped)", cfg.ProxyURL)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].Type != "file" || cfg.Targets[0].Cred != "@active" {
		t.Errorf("targets=%+v", cfg.Targets)
	}
	if cfg.Targets[0].Path != "~/.claude/.credentials.json" {
		t.Errorf("target path=%q want default", cfg.Targets[0].Path)
	}
	// setup writes the modern autoPolicy, never the legacy auto bool.
	if strings.Contains(string(raw), `"auto"`) {
		t.Errorf("legacy auto key written:\n%s", raw)
	}
}

func TestLaunchdWatchPlist(t *testing.T) {
	got := launchdWatchPlist("/opt/ccb", "/home/me/agent.json", "/var/log/ccb.log")
	for _, want := range []string{
		"<string>com.ccbroker.watch</string>",
		"<string>watch</string>",
		"<key>KeepAlive</key>",
		"<true/>",
		"<string>/opt/ccb</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("watch plist missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "StartInterval") {
		t.Errorf("watch daemon must not use StartInterval:\n%s", got)
	}
}

func TestLaunchdSyncPlist(t *testing.T) {
	got := launchdSyncPlist("/opt/ccb", "/home/me/agent.json", "/var/log/ccb.log", 1800)
	for _, want := range []string{
		"<string>com.ccbroker.sync</string>",
		"<string>sync</string>",
		"<key>StartInterval</key>",
		"<integer>1800</integer>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sync plist missing %q:\n%s", want, got)
		}
	}
}

func TestSystemdWatchService(t *testing.T) {
	got := systemdWatchService("/opt/ccb", "/home/me/agent.json")
	for _, want := range []string{
		`ExecStart="/opt/ccb" watch -c "/home/me/agent.json"`,
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("watch service missing %q:\n%s", want, got)
		}
	}
}

func TestSystemdSyncUnits(t *testing.T) {
	service, timer := systemdSyncUnits("/opt/ccb", "/home/me/agent.json", 1800)
	if !strings.Contains(service, `ExecStart="/opt/ccb" sync -c "/home/me/agent.json"`) {
		t.Errorf("sync service ExecStart wrong:\n%s", service)
	}
	if !strings.Contains(service, "Type=oneshot") {
		t.Errorf("sync service should be oneshot:\n%s", service)
	}
	if !strings.Contains(timer, "OnUnitActiveSec=1800") {
		t.Errorf("sync timer interval wrong:\n%s", timer)
	}
}

func TestCronLines(t *testing.T) {
	ensureAlive, sync := cronLines("/opt/ccb", "/home/me/agent.json", 1800)
	if ensureAlive != "*/5 * * * * /opt/ccb ensure-alive -c /home/me/agent.json" {
		t.Errorf("ensure-alive cron line wrong: %q", ensureAlive)
	}
	if sync != "*/30 * * * * /opt/ccb sync -c /home/me/agent.json" {
		t.Errorf("sync cron line wrong: %q", sync)
	}
	// MINOR-7: interval > 3599s must clamp the minute field to ≤59 (»/60 is an
	// invalid crontab minute).
	if _, sync := cronLines("/opt/ccb", "/c", 7200); !strings.Contains(sync, "*/59 ") {
		t.Errorf("interval 7200 should clamp minute to 59, got %q", sync)
	}
}

func TestRunSetupOverwriteRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	p := filepath.Join(dir, "agent.json")
	sentinel := `{"keep":"me"}` + "\n"
	if err := os.WriteFile(p, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := runSetup(p, strings.NewReader("n\n"), &out); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != sentinel {
		t.Errorf("file modified on overwrite refusal: %q", raw)
	}
}
