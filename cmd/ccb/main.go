// Command ccb is the cc-cred-broker client: it pulls credentials from the
// broker, keeps local destinations in sync, switches the active account, and
// reports quota status.
//
// Usage:
//
//	ccb pull        [-c agent.json]   # one-shot sync (+auto-rotate if "auto": true)
//	ccb run         [-c agent.json]   # sync on an interval
//	ccb use <name>  [-c agent.json]   # switch the "@active" account and sync
//	ccb auto        [-c agent.json]   # switch to the least-utilized account and sync
//	ccb status      [-c agent.json]   # quota table for all accounts in scope
//	ccb statusline  [-c agent.json]   # one-line summary from cache (for Claude Code statusLine)
//	ccb statusline --install [--settings <path>]  # register as Claude Code statusLine
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"ccbroker/internal/anthropic"
	"ccbroker/internal/config"
)

const keychainService = "Claude Code-credentials"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ccb {pull|run|use <name>|auto|status|statusline} [-c agent.json]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	cfgPath := defaultConfigPath()
	settingsPath := ""
	install := false
	var positional []string
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "-c" || args[i] == "--config") && i+1 < len(args):
			cfgPath = args[i+1]
			i++
		case args[i] == "--install":
			install = true
		case args[i] == "--settings" && i+1 < len(args):
			settingsPath = args[i+1]
			i++
		default:
			positional = append(positional, args[i])
		}
	}

	if cmd == "statusline" && install {
		if err := installStatusline(settingsPath); err != nil {
			fatal(err)
		}
		return
	}

	cfg, err := config.LoadAgent(cfgPath)
	if err != nil {
		fatal(err)
	}

	if cmd == "statusline" {
		printStatusline(cfg)
		return
	}

	client, err := httpClient(cfg)
	if err != nil {
		fatal(err)
	}

	switch cmd {
	case "pull":
		if n := syncCycle(cfg, client, false); n > 0 {
			os.Exit(1)
		}
	case "run":
		iv := time.Duration(cfg.IntervalSec) * time.Second
		logf("agent started, interval=%s, targets=%d, auto=%v", iv, len(cfg.Targets), cfg.Auto)
		for {
			syncCycle(cfg, client, false)
			time.Sleep(iv)
		}
	case "use":
		if len(positional) != 1 {
			fatal(fmt.Errorf("usage: ccb use <cred-name> [-c agent.json]"))
		}
		if err := writeActive(cfg.ActiveFile, positional[0]); err != nil {
			fatal(err)
		}
		logf("active account -> %s", positional[0])
		if n := syncCycle(cfg, client, false); n > 0 {
			os.Exit(1)
		}
	case "auto":
		if n := syncCycle(cfg, client, true); n > 0 {
			os.Exit(1)
		}
	case "status":
		rows, err := fetchUsageRows(cfg, client)
		if err != nil {
			fatal(err)
		}
		writeStatusCache(cfg, rows)
		renderStatus(cfg, rows)
	default:
		fatal(fmt.Errorf("unknown command %q", cmd))
	}
}

func defaultConfigPath() string {
	return expandHome("~/.config/ccbroker/agent.json")
}

// syncCycle refreshes the quota cache, optionally auto-rotates the active
// account, and syncs every target. forceAuto rotates even when cfg.Auto is
// off (the `auto` subcommand). Returns the number of failures.
func syncCycle(cfg *config.Agent, client *http.Client, forceAuto bool) int {
	rows, err := fetchUsageRows(cfg, client)
	if err != nil {
		logf("usage fetch failed: %v", err)
	} else {
		if cfg.Auto || forceAuto {
			if err := autoSelect(cfg, rows); err != nil {
				logf("auto-select: %v", err)
			}
		}
		writeStatusCache(cfg, rows)
	}
	return syncAll(cfg, client)
}

// autoSelect keeps the current active account while it is alive and under
// AutoThreshold; otherwise it switches to the least-utilized eligible one.
func autoSelect(cfg *config.Agent, rows []usageRow) error {
	now := time.Now().UnixMilli()
	eligible := func(r usageRow) bool { return !r.Dead && r.ExpiresAt > now }
	score := func(r usageRow) float64 {
		if r.Usage == nil {
			return 0
		}
		return r.Usage.MaxUtilization()
	}

	active := readActive(cfg)
	for _, r := range rows {
		if r.Name == active && eligible(r) && score(r) < cfg.AutoThreshold {
			return nil // current account is fine; don't thrash
		}
	}

	best := ""
	bestScore := 0.0
	for _, r := range rows {
		if !eligible(r) {
			continue
		}
		s := score(r)
		if best == "" || s < bestScore || (s == bestScore && r.Name < best) {
			best, bestScore = r.Name, s
		}
	}
	if best == "" {
		return fmt.Errorf("no eligible account (all dead or expired)")
	}
	if best == active {
		return nil
	}
	if err := writeActive(cfg.ActiveFile, best); err != nil {
		return err
	}
	logf("auto-switched active account: %s -> %s (%.0f%% utilized)", active, best, bestScore*100)
	return nil
}

// syncAll syncs every target and returns the number of failures.
func syncAll(cfg *config.Agent, client *http.Client) int {
	fails := 0
	for _, t := range cfg.Targets {
		name, err := resolveCred(cfg, t.Cred)
		if err != nil {
			logf("target=%s SKIP %v", t.Type, err)
			fails++
			continue
		}
		body, err := fetchCred(cfg, client, name)
		if err != nil {
			logf("cred=%s FETCH_FAIL %v", name, err)
			fails++
			continue
		}
		if err := writeTarget(t, body); err != nil {
			logf("cred=%s target=%s WRITE_FAIL %v", name, t.Type, err)
			fails++
			continue
		}
		logf("cred=%s target=%s -> %s OK", name, t.Type, t.Path)
	}
	return fails
}

// resolveCred maps the special name "@active" to the account named in the
// activeFile (written by `use`/`auto`), so one target can follow switches.
func resolveCred(cfg *config.Agent, cred string) (string, error) {
	if cred != "@active" {
		return cred, nil
	}
	name := readActive(cfg)
	if name == "" {
		return "", fmt.Errorf("@active unresolved (run `ccb use <name>`)")
	}
	return name, nil
}

func readActive(cfg *config.Agent) string {
	b, err := os.ReadFile(expandHome(cfg.ActiveFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeActive records name as the current "@active" account.
func writeActive(path, name string) error {
	p := expandHome(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(name+"\n"), 0o600)
}

// ---- broker API ----

func fetchCred(cfg *config.Agent, client *http.Client, name string) ([]byte, error) {
	return brokerGet(cfg, client, "/v1/credentials/"+name)
}

// usageRow mirrors one entry of the broker's /v1/usage response.
type usageRow struct {
	Name           string           `json:"name"`
	Account        string           `json:"account,omitempty"`
	Dead           bool             `json:"dead,omitempty"`
	ExpiresAt      int64            `json:"expiresAt"`
	Usage          *anthropic.Usage `json:"usage,omitempty"`
	UsageFetchedAt int64            `json:"usageFetchedAt,omitempty"`
	UsageError     string           `json:"usageError,omitempty"`
}

func fetchUsageRows(cfg *config.Agent, client *http.Client) ([]usageRow, error) {
	body, err := brokerGet(cfg, client, "/v1/usage")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Credentials []usageRow `json:"credentials"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("usage decode: %w", err)
	}
	sort.Slice(resp.Credentials, func(i, j int) bool { return resp.Credentials[i].Name < resp.Credentials[j].Name })
	return resp.Credentials, nil
}

func brokerGet(cfg *config.Agent, client *http.Client, path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, cfg.BrokerURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// ---- status rendering & cache ----

// statusCache is what `statusline` reads: the last /v1/usage snapshot.
type statusCache struct {
	FetchedAt   int64      `json:"fetchedAt"`
	Credentials []usageRow `json:"credentials"`
}

func statusCachePath(cfg *config.Agent) string {
	return filepath.Join(filepath.Dir(expandHome(cfg.ActiveFile)), "status.json")
}

func writeStatusCache(cfg *config.Agent, rows []usageRow) {
	b, err := json.Marshal(statusCache{FetchedAt: time.Now().UnixMilli(), Credentials: rows})
	if err != nil {
		return
	}
	_ = writeFile(statusCachePath(cfg), b)
}

func pct(b *anthropic.Bucket) string {
	if b == nil {
		return " n/a"
	}
	return fmt.Sprintf("%3.0f%%", b.Utilization*100)
}

func bar(b *anthropic.Bucket) string {
	if b == nil {
		return strings.Repeat("·", 10)
	}
	n := int(b.Utilization*10 + 0.5)
	if n < 0 {
		n = 0
	}
	if n > 10 {
		n = 10
	}
	return strings.Repeat("▓", n) + strings.Repeat("░", 10-n)
}

func renderStatus(cfg *config.Agent, rows []usageRow) {
	active := readActive(cfg)
	now := time.Now().UnixMilli()
	fmt.Printf("  %-10s %-26s %-16s %-16s %s\n", "NAME", "ACCOUNT", "5H", "7D", "STATE")
	for _, r := range rows {
		mark := " "
		if r.Name == active {
			mark = "*"
		}
		state := "ok"
		switch {
		case r.Dead:
			state = "DEAD (re-auth needed)"
		case r.ExpiresAt <= now:
			state = "token expired"
		case r.UsageError != "":
			state = "usage-err"
		}
		var u5, u7 *anthropic.Bucket
		var scoped []string
		if r.Usage != nil {
			u5, u7 = r.Usage.FiveHour, r.Usage.SevenDay
			for model, b := range r.Usage.ScopedWeekly {
				scoped = append(scoped, fmt.Sprintf("%s:%.0f%%", strings.ToLower(model), b.Utilization*100))
			}
			sort.Strings(scoped)
		}
		if len(scoped) > 0 {
			state += "  [7d " + strings.Join(scoped, " ") + "]"
		}
		fmt.Printf("%s %-10s %-26s %s %s %s %s %s\n",
			mark, r.Name, r.Account, bar(u5), pct(u5), bar(u7), pct(u7), state)
	}
}

// ---- statusline (fast path: cache + active file only, no network) ----

func printStatusline(cfg *config.Agent) {
	active := readActive(cfg)
	if active == "" {
		fmt.Println("ccb: no active account")
		return
	}
	b, err := os.ReadFile(statusCachePath(cfg))
	if err != nil {
		fmt.Println(active)
		return
	}
	var cache statusCache
	if err := json.Unmarshal(b, &cache); err != nil {
		fmt.Println(active)
		return
	}
	for _, r := range cache.Credentials {
		if r.Name != active {
			continue
		}
		line := active
		if r.Usage != nil {
			if r.Usage.FiveHour != nil {
				line += fmt.Sprintf(" 5h:%.0f%%", r.Usage.FiveHour.Utilization*100)
			}
			if r.Usage.SevenDay != nil {
				line += fmt.Sprintf(" 7d:%.0f%%", r.Usage.SevenDay.Utilization*100)
			}
			if r.Usage.MaxUtilization() >= cfg.AutoThreshold {
				line = "⚠ " + line
			}
		}
		if r.Dead {
			line = "✗ " + line + " (dead)"
		}
		if time.Now().UnixMilli()-cache.FetchedAt > 90*60*1000 {
			line += " ~stale"
		}
		fmt.Println(line)
		return
	}
	fmt.Println(active)
}

// installStatusline registers `ccb statusline` as the Claude Code statusLine
// command in settings.json (default ~/.claude/settings.json), preserving all
// other settings.
func installStatusline(settingsPath string) error {
	if settingsPath == "" {
		settingsPath = "~/.claude/settings.json"
	}
	p := expandHome(settingsPath)
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s is not valid JSON, not touching it: %w", p, err)
		}
	}
	if existing, ok := m["statusLine"]; ok {
		return fmt.Errorf("%s already has a statusLine (%v) — integrate manually by calling `ccb statusline` from it", p, existing)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	m["statusLine"] = map[string]any{"type": "command", "command": exe + " statusline"}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("statusLine installed in %s -> %s statusline\n", p, exe)
	return nil
}

// ---- target writers ----

func writeTarget(t config.Target, body []byte) error {
	switch t.Type {
	case "file":
		return writeFile(expandHome(t.Path), body)
	case "keychain":
		return writeKeychain(body)
	default:
		return fmt.Errorf("unknown target type %q", t.Type)
	}
}

// writeFile atomically writes body to path with 0600 perms.
func writeFile(path string, body []byte) error {
	if path == "" {
		return fmt.Errorf("file target requires a path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ccb-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// writeKeychain stores body as the "Claude Code-credentials" generic password,
// reusing the account of any existing item so Claude Code finds it.
func writeKeychain(body []byte) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("keychain target is only supported on macOS")
	}
	acct := keychainAccount()
	args := []string{"add-generic-password", "-U", "-s", keychainService, "-a", acct, "-w", string(body)}
	cmd := exec.Command("security", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("security add-generic-password: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

var acctRe = regexp.MustCompile(`"acct"<blob>="(.*)"`)

// keychainAccount returns the account of the existing Claude Code keychain item,
// falling back to $USER.
func keychainAccount() string {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService).CombinedOutput()
	if err == nil {
		if m := acctRe.FindSubmatch(out); m != nil {
			return string(m[1])
		}
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "claude"
}

// ---- plumbing ----

func httpClient(cfg *config.Agent) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Insecure {
		tlsCfg.InsecureSkipVerify = true
	}
	if cfg.CACertPath != "" {
		pem, err := os.ReadFile(expandHome(cfg.CACertPath))
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certs parsed from %s", cfg.CACertPath)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.ClientCertPath != "" || cfg.ClientKeyPath != "" {
		if cfg.ClientCertPath == "" || cfg.ClientKeyPath == "" {
			return nil, fmt.Errorf("clientCertPath and clientKeyPath must both be set for mTLS")
		}
		cert, err := tls.LoadX509KeyPair(expandHome(cfg.ClientCertPath), expandHome(cfg.ClientKeyPath))
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, time.Now().UTC().Format("2006-01-02T15:04:05Z ")+format+"\n", a...)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ccb:", err)
	os.Exit(1)
}
