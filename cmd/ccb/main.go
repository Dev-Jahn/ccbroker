// Command ccb is the ccbroker client: it pulls credentials from the
// broker, keeps local destinations in sync, switches the active account, and
// reports quota status.
//
// Usage:
//
//	ccb pull        [-c agent.json]   # one-shot sync (+auto-rotate per policy)
//	ccb run         [-c agent.json]   # sync on an interval
//	ccb use <name>  [-c agent.json]   # switch the "@active" account and sync
//	ccb auto        [-c agent.json]   # switch to the least-utilized account and sync
//	ccb status      [-c agent.json]   # quota table for all accounts in scope
//	ccb statusline  [-c agent.json]   # one-line summary from cache (for Claude Code statusLine)
//	ccb statusline --all [-c agent.json]          # full per-account usage line
//	ccb statusline on|off [--settings <path>]     # install/remove the Claude Code statusLine
//	ccb statusline --install [--settings <path>]  # register as Claude Code statusLine (legacy)
//	ccb policy [manual|account|all] [-c agent.json]  # show or set the auto-rotation policy
//	ccb setup       [-c agent.json]   # interactive first-run wizard
//	ccb version                       # print the ccb version
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Dev-Jahn/ccbroker/internal/anthropic"
	"github.com/Dev-Jahn/ccbroker/internal/config"
)

const keychainService = "Claude Code-credentials"

// version is stamped at release time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ccb {pull|run|use <name>|auto|status|statusline|policy [value]|setup|version} [-c agent.json]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	cfgPath := defaultConfigPath()
	settingsPath := ""
	install := false
	all := false
	var positional []string
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "-c" || args[i] == "--config") && i+1 < len(args):
			cfgPath = args[i+1]
			i++
		case args[i] == "--install":
			install = true
		case args[i] == "--all":
			all = true
		case args[i] == "--settings" && i+1 < len(args):
			settingsPath = args[i+1]
			i++
		default:
			positional = append(positional, args[i])
		}
	}

	// on/off and --install edit settings.json (or a statusline script) directly
	// and need no agent.json, so they run before LoadAgent like the legacy path.
	if cmd == "statusline" {
		sub := ""
		if len(positional) > 0 {
			sub = positional[0]
		}
		switch {
		case install:
			if err := installStatusline(settingsPath); err != nil {
				fatal(err)
			}
			return
		case sub == "on":
			if err := statuslineOn(settingsPath); err != nil {
				fatal(err)
			}
			return
		case sub == "off":
			if err := statuslineOff(settingsPath); err != nil {
				fatal(err)
			}
			return
		}
	}

	// These run before LoadAgent: version needs no config, and setup/policy
	// manage the config file themselves (setup even creates it).
	switch cmd {
	case "version":
		fmt.Printf("ccb %s\n", version)
		return
	case "setup":
		if err := runSetup(cfgPath, os.Stdin, os.Stdout); err != nil {
			fatal(err)
		}
		return
	case "policy":
		if err := runPolicy(cfgPath, positional); err != nil {
			fatal(err)
		}
		return
	}

	cfg, err := config.LoadAgent(cfgPath)
	if err != nil {
		fatal(err)
	}

	if cmd == "statusline" {
		if all {
			printStatuslineAll(cfg)
		} else {
			printStatusline(cfg)
		}
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
		logf("agent started, interval=%s, targets=%d, policy=%s", iv, len(cfg.Targets), cfg.EffectivePolicy())
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

// runPolicy shows or sets the auto-rotation policy in agent.json. With no
// argument it prints the effective policy and where it comes from; with an
// argument it rewrites "autoPolicy" and drops the legacy "auto" key, leaving
// every other field untouched.
func runPolicy(cfgPath string, args []string) error {
	p := expandHome(cfgPath)
	if len(args) == 0 {
		cfg, err := config.LoadAgent(p)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("no config at %s (run `ccb setup`)", p)
			}
			return err
		}
		src := "(default)"
		switch {
		case cfg.AutoPolicy != "":
			src = "(from autoPolicy)"
		case cfg.Auto:
			src = `(from legacy "auto": true)`
		}
		fmt.Printf("policy: %s (threshold %g)\n", cfg.EffectivePolicy(), cfg.AutoThreshold)
		fmt.Println(src)
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: ccb policy [manual|account|all] [-c agent.json]")
	}
	val := args[0]
	switch val {
	case "manual", "account", "all":
	default:
		return fmt.Errorf("policy must be manual, account or all (got %q)", val)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no config at %s (run `ccb setup`)", p)
		}
		return err
	}
	// UseNumber so large integers round-trip exactly instead of degrading to
	// float64 (e.g. intervalSec 1800 must not become 1.8e+03) when rewritten.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	m := map[string]any{}
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("%s not valid JSON: %w", p, err)
	}
	m["autoPolicy"] = val
	delete(m, "auto")
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(p, append(out, '\n')); err != nil {
		return err
	}
	fmt.Printf("auto-rotation policy -> %s\n", val)
	return nil
}

// syncCycle refreshes the quota cache, optionally auto-rotates the active
// account, and syncs every target. forceAuto rotates even when cfg.Auto is
// off (the `auto` subcommand). Returns the number of failures.
func syncCycle(cfg *config.Agent, client *http.Client, forceAuto bool) int {
	rows, err := fetchUsageRows(cfg, client)
	if err != nil {
		logf("usage fetch failed: %v", err)
	} else {
		pol := cfg.EffectivePolicy()
		if forceAuto && pol == "manual" {
			pol = "account" // explicit `ccb auto` keeps its historical account-wide metric
		}
		if pol != "manual" {
			if err := autoSelect(cfg, rows, pol); err != nil {
				logf("auto-select: %v", err)
			}
		}
		writeStatusCache(cfg, rows)
	}
	return syncAll(cfg, client, rows)
}

// usageMetric is the utilization score a rotation policy compares against
// AutoThreshold: the "all" policy includes model-scoped weekly buckets, every
// other policy looks at the account-wide 5h/7d windows only. A nil Usage
// (missing data or a fetch error) scores 0.
func usageMetric(u *anthropic.Usage, policy string) float64 {
	if u == nil {
		return 0
	}
	if policy == "all" {
		return u.MaxUtilizationAll()
	}
	return u.MaxUtilization()
}

// autoSelect keeps the current active account while it is alive and under
// AutoThreshold; otherwise it switches to the least-utilized eligible one.
// policy selects the utilization metric (see usageMetric).
func autoSelect(cfg *config.Agent, rows []usageRow, policy string) error {
	now := time.Now().UnixMilli()
	eligible := func(r usageRow) bool { return !r.Dead && r.ExpiresAt > now }
	score := func(r usageRow) float64 { return usageMetric(r.Usage, policy) }

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

// syncAll syncs every target and returns the number of failures. rows carries
// the broker's per-cred oauthAccount so the credential's identity (which Claude
// Code shows in /status) follows the account, keeping /status and the token in
// agreement after a switch.
func syncAll(cfg *config.Agent, client *http.Client, rows []usageRow) int {
	byName := make(map[string]usageRow, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}
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
		if row, ok := byName[name]; ok && row.OAuthAccount != nil {
			if cj := claudeJSONForTarget(t); cj != "" {
				switch changed, err := syncIdentity(cj, row.OAuthAccount); {
				case err != nil:
					logf("cred=%s identity WARN %v", name, err)
				case changed:
					logf("cred=%s identity -> %s updated", name, cj)
				}
			}
		}
	}
	return fails
}

// claudeJSONForTarget returns the .claude.json Claude Code actually reads for a
// target, or "" if not applicable. Layout quirk: the DEFAULT config dir
// (~/.claude, i.e. CLAUDE_CONFIG_DIR unset) keeps its .claude.json at HOME
// (~/.claude.json), while a custom CLAUDE_CONFIG_DIR colocates it as a sibling
// of the credential file. The keychain target is macOS's default dir → HOME.
func claudeJSONForTarget(t config.Target) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	defaultJSON := filepath.Join(home, ".claude.json")
	switch t.Type {
	case "file":
		p := expandHome(t.Path)
		if p == "" {
			return ""
		}
		dir := filepath.Dir(p)
		if dir == filepath.Join(home, ".claude") {
			return defaultJSON
		}
		return filepath.Join(dir, ".claude.json")
	case "keychain":
		return defaultJSON
	}
	return ""
}

// syncIdentity rewrites .claude.json's "oauthAccount" to match acct when the
// recorded email differs (a no-op otherwise), so /status reflects the account
// whose token is currently in place. All other keys are preserved.
func syncIdentity(path string, acct map[string]any) (bool, error) {
	email, _ := acct["emailAddress"].(string)
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		// UseNumber so large integers (timestamps, counters) round-trip exactly
		// instead of degrading to float64 when this big file is rewritten.
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			return false, fmt.Errorf("%s not valid JSON: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if cur, ok := m["oauthAccount"].(map[string]any); ok {
		if e, _ := cur["emailAddress"].(string); e == email {
			return false, nil // already this account
		}
	}
	if _, ok := m["hasCompletedOnboarding"]; !ok {
		m["hasCompletedOnboarding"] = true
	}
	m["oauthAccount"] = acct
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return false, err
	}
	return true, writeFile(path, append(b, '\n'))
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
	OAuthAccount   map[string]any   `json:"oauthAccount,omitempty"`
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
	// "manual" has no rotation metric of its own; show the account-wide warning.
	displayPolicy := cfg.EffectivePolicy()
	if displayPolicy == "manual" {
		displayPolicy = "account"
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
			if usageMetric(r.Usage, displayPolicy) >= cfg.AutoThreshold {
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

// ---- statusline --all (full multi-account line, no network) ----

// ANSI codes for the --all line. Foreground colors are 256-color (38;5;N).
const (
	slRST  = "\x1b[0m"
	slSEP  = "\x1b[38;5;240m │ \x1b[0m" // dim separator with its surrounding spaces
	slACT  = "\x1b[1;38;5;117m"         // active account name (bold bright blue)
	slDIM  = "\x1b[38;5;245m"           // inactive names and segment labels
	slHIGH = "\x1b[38;5;210m"           // utilization >= 80% (and the dead ✗)
	slMID  = "\x1b[38;5;221m"           // utilization >= 50%
	slLOW  = "\x1b[38;5;114m"           // utilization < 50%
)

// printStatuslineAll renders the full multi-account usage line (see
// renderStatuslineAll) from the cache. Like printStatusline, a missing or
// unparsable cache falls back to the plain active name.
func printStatuslineAll(cfg *config.Agent) {
	active := readActive(cfg)
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
	fmt.Println(renderStatuslineAll(active, cache, time.Now().UnixMilli()))
}

// renderStatuslineAll builds the full one-line status from a cache snapshot:
// every credential in cache order joined by SEP, the active one marked ⛁ and
// bright, dead ones prefixed ✗, each followed by 5h/7d and per-model weekly
// utilization segments. Pure (no I/O) so it is testable; nowMs drives the
// " ~stale" suffix.
func renderStatuslineAll(active string, cache statusCache, nowMs int64) string {
	parts := make([]string, 0, len(cache.Credentials))
	for _, r := range cache.Credentials {
		var b strings.Builder
		if r.Dead {
			b.WriteString(slHIGH + "✗ " + slRST)
		}
		if r.Name == active {
			b.WriteString(slACT + "⛁ " + r.Name + slRST)
		} else {
			b.WriteString(slDIM + r.Name + slRST)
		}
		for _, seg := range statuslineSegments(r.Usage) {
			b.WriteString(" " + seg)
		}
		parts = append(parts, b.String())
	}
	line := strings.Join(parts, slSEP)
	if nowMs-cache.FetchedAt > 90*60*1000 {
		line += slDIM + " ~stale" + slRST
	}
	return line
}

// statuslineSegments renders the "5h:", "7d:" and per-model weekly segments for
// one credential, in that order; weekly buckets are sorted by model display
// name. A nil Usage yields no segments.
func statuslineSegments(u *anthropic.Usage) []string {
	if u == nil {
		return nil
	}
	var segs []string
	if u.FiveHour != nil {
		segs = append(segs, statuslineSegment("5h:", u.FiveHour.Utilization))
	}
	if u.SevenDay != nil {
		segs = append(segs, statuslineSegment("7d:", u.SevenDay.Utilization))
	}
	models := make([]string, 0, len(u.ScopedWeekly))
	for m := range u.ScopedWeekly {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		segs = append(segs, statuslineSegment(modelLabel(m), u.ScopedWeekly[m].Utilization))
	}
	return segs
}

// statuslineSegment formats one "<label><pct>%" segment: label in DIM, the
// percentage colored by utilization (>=80 HIGH, >=50 MID, else LOW).
func statuslineSegment(label string, util float64) string {
	p := int(math.Round(util * 100))
	color := slLOW
	switch {
	case p >= 80:
		color = slHIGH
	case p >= 50:
		color = slMID
	}
	return slDIM + label + color + fmt.Sprintf("%d%%", p) + slRST
}

// modelLabel abbreviates a model display name to its first rune uppercased plus
// ":" (e.g. "Fable" → "F:").
func modelLabel(model string) string {
	for _, r := range model {
		return string(unicode.ToUpper(r)) + ":"
	}
	return ":"
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

// ---- statusline on|off (idempotent install/remove) ----

// The marker block wraps the ccbroker line inside an existing statusline script
// so `off` can find and remove exactly what `on` added. Bytes are exact.
const (
	markerBegin = "# >>> ccbroker statusline >>>"
	markerBody  = "command -v ccb >/dev/null 2>&1 && ccb statusline --all"
	markerEnd   = "# <<< ccbroker statusline <<<"
)

// statuslineBlock is the marker block with a trailing newline.
func statuslineBlock() string {
	return markerBegin + "\n" + markerBody + "\n" + markerEnd + "\n"
}

// settingsPathOrDefault resolves the settings path, defaulting to the standard
// Claude Code location. A symlinked settings.json is resolved to its target so
// an atomic write updates the real file instead of replacing the symlink (which
// would break dotfile managers like stow/chezmoi).
func settingsPathOrDefault(settingsPath string) string {
	if settingsPath == "" {
		settingsPath = "~/.claude/settings.json"
	}
	return resolveSymlink(expandHome(settingsPath))
}

// loadSettings reads settings.json into a map, decoding with UseNumber so large
// integers round-trip exactly. A missing file yields an empty map; invalid JSON
// is an error (the file is left untouched).
func loadSettings(p string) (map[string]any, error) {
	m := map[string]any{}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON, not touching it: %w", p, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s is not valid JSON, not touching it: trailing data after the top-level object", p)
	}
	return m, nil
}

// writeSettings atomically writes m to p as pretty JSON with 0644 perms
// (settings.json is not a secret).
func writeSettings(p string, m map[string]any) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileMode(p, append(b, '\n'), 0o644)
}

// statuslineInterp holds interpreter tokens skipped when locating the program or
// script inside a statusLine command string.
var statuslineInterp = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "env": true}

// isInterpToken reports whether tok names an interpreter, by bare name or by
// absolute path (basename), so leading interpreter tokens can be skipped.
func isInterpToken(tok string) bool {
	return statuslineInterp[tok] || statuslineInterp[filepath.Base(tok)]
}

// isCcbStatuslineCommand reports whether a statusLine command string is a ccb
// statusline invocation (ours to manage). A substring match on this binary's
// resolved absolute path is unambiguous; otherwise `ccb` must be the actual
// program invoked (after skipping interpreter tokens) followed by the statusline
// subcommand — not merely a substring of a larger word (e.g. "myccb") or
// embedded inside a composite command — so `on`/`off` never rewrite or delete a
// foreign or wrapper statusLine.
func isCcbStatuslineCommand(cmd, exe string) bool {
	if strings.Contains(cmd, exe+" statusline") {
		return true
	}
	fields := strings.Fields(cmd)
	i := 0
	for i < len(fields) && isInterpToken(fields[i]) {
		i++
	}
	return i+1 < len(fields) && filepath.Base(fields[i]) == "ccb" && fields[i+1] == "statusline"
}

// locateScript finds the statusline script inside a command string: it skips
// leading interpreter tokens (bash/sh/zsh/dash/env, by name or absolute path)
// and returns the first remaining token that ~-expands to an existing regular
// file. Returns "" if none is found.
//
// The command is split on whitespace and does not honor shell quoting, so a
// script whose path contains spaces (e.g. bash "/Users/me/My Scripts/x.sh") is
// not located; such a script must be integrated with the marker block manually.
func locateScript(cmd string) string {
	skipping := true
	for _, tok := range strings.Fields(cmd) {
		if skipping && isInterpToken(tok) {
			continue
		}
		skipping = false
		p := expandHome(tok)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// blockBounds locates the marker block in content, returning the byte range
// [start, end) covering the block including its trailing newline.
func blockBounds(content string) (start, end int, ok bool) {
	bi := strings.Index(content, markerBegin)
	if bi < 0 {
		return 0, 0, false
	}
	mi := strings.Index(content[bi:], markerEnd)
	if mi < 0 {
		return 0, 0, false
	}
	end = bi + mi + len(markerEnd)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return bi, end, true
}

// blockUpsert returns content with the marker block present: it replaces an
// existing block in place (so re-running is byte-identical) or appends
// "\n" + block at EOF.
func blockUpsert(content string) string {
	block := statuslineBlock()
	if s, e, ok := blockBounds(content); ok {
		return content[:s] + block + content[e:]
	}
	return content + "\n" + block
}

// blockRemove returns content with the marker block removed; ok is false if no
// block was present. The single newline blockUpsert inserts before an appended
// block is stripped only when the block was at EOF (the shape `on` produces), so
// any content following the block keeps its line separator instead of being
// glued onto the preceding line.
func blockRemove(content string) (string, bool) {
	s, e, ok := blockBounds(content)
	if !ok {
		return content, false
	}
	pre, post := content[:s], content[e:]
	if post == "" {
		pre = strings.TrimSuffix(pre, "\n")
	}
	return pre + post, true
}

// statuslineOn installs (or idempotently updates) the ccbroker `statusline
// --all` line. With no existing statusLine it sets one; if the existing
// statusLine is a ccb command it normalizes it; if it is an existing statusline
// script it edits in the marker block; otherwise it errors with the block to
// paste manually.
func statuslineOn(settingsPath string) error {
	p := settingsPathOrDefault(settingsPath)
	m, err := loadSettings(p)
	if err != nil {
		return err
	}
	exe, err := resolveExe()
	if err != nil {
		return err
	}
	target := exe + " statusline --all"

	v, exists := m["statusLine"]
	if !exists {
		m["statusLine"] = map[string]any{"type": "command", "command": target}
		if err := writeSettings(p, m); err != nil {
			return err
		}
		fmt.Printf("statusLine installed in %s -> %s\n", p, target)
		return nil
	}

	sl, _ := v.(map[string]any)
	cmd := ""
	if sl != nil {
		cmd, _ = sl["command"].(string)
	}

	if isCcbStatuslineCommand(cmd, exe) {
		if cmd == target {
			fmt.Printf("statusLine already set in %s -> %s\n", p, target)
			return nil
		}
		sl["command"] = target
		if _, ok := sl["type"]; !ok {
			sl["type"] = "command"
		}
		if err := writeSettings(p, m); err != nil {
			return err
		}
		fmt.Printf("statusLine updated in %s -> %s\n", p, target)
		return nil
	}

	if script := locateScript(cmd); script != "" {
		script = resolveSymlink(script)
		fi, err := os.Stat(script)
		if err != nil {
			return err
		}
		old, err := os.ReadFile(script)
		if err != nil {
			return err
		}
		updated := blockUpsert(string(old))
		if updated == string(old) {
			fmt.Printf("ccbroker statusline block already present in %s\n", script)
			return nil
		}
		if err := writeFileMode(script, []byte(updated), fi.Mode().Perm()); err != nil {
			return err
		}
		fmt.Printf("ccbroker statusline block added to %s\n", script)
		return nil
	}

	return fmt.Errorf("%s has a statusLine command (%q) that is neither a ccb command nor a locatable script; add this block to your statusline script manually:\n\n%s", p, cmd, statuslineBlock())
}

// statuslineOff removes what statuslineOn added: a ccb statusLine command is
// deleted from settings.json; a marker block is removed from the referenced
// script (perms preserved); anything else is a no-op.
func statuslineOff(settingsPath string) error {
	p := settingsPathOrDefault(settingsPath)
	m, err := loadSettings(p)
	if err != nil {
		return err
	}
	v, exists := m["statusLine"]
	if !exists {
		fmt.Println("statusLine: nothing to remove")
		return nil
	}
	sl, _ := v.(map[string]any)
	cmd := ""
	if sl != nil {
		cmd, _ = sl["command"].(string)
	}
	exe, err := resolveExe()
	if err != nil {
		return err
	}

	if isCcbStatuslineCommand(cmd, exe) {
		delete(m, "statusLine")
		if err := writeSettings(p, m); err != nil {
			return err
		}
		fmt.Printf("statusLine removed from %s\n", p)
		return nil
	}

	if script := locateScript(cmd); script != "" {
		script = resolveSymlink(script)
		fi, err := os.Stat(script)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(script)
		if err != nil {
			return err
		}
		if updated, removed := blockRemove(string(b)); removed {
			if err := writeFileMode(script, []byte(updated), fi.Mode().Perm()); err != nil {
				return err
			}
			fmt.Printf("ccbroker statusline block removed from %s\n", script)
			return nil
		}
	}

	fmt.Println("statusLine: nothing to remove")
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
	return writeFileMode(path, body, 0o600)
}

// resolveSymlink returns the real path a symlink points at (following the whole
// chain) so a caller's atomic rename writes through to the target rather than
// replacing the link itself. Non-symlinks and missing paths are returned
// unchanged.
func resolveSymlink(path string) string {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			return real
		}
	}
	return path
}

// writeFileMode atomically writes body to path with perm: it writes a temp file
// in the same directory, chmods it, and renames over path so readers never see
// a partial file. writeFile is the 0600 shorthand.
func writeFileMode(path string, body []byte, perm os.FileMode) error {
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
	if err := tmp.Chmod(perm); err != nil {
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
