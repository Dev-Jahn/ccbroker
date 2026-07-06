package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Dev-Jahn/ccbroker/internal/config"
)

const policyMenu = "Auto-rotation policy — when should ccb switch the active account?\n" +
	"  1) manual  — never switch automatically; you switch with `ccb use <name>`\n" +
	"  2) account — switch when the account-wide 5-hour or 7-day window reaches the threshold\n" +
	"  3) all     — like account, but ALSO switch when any per-model weekly bucket\n" +
	"               (e.g. a top-tier model you rely on) reaches the threshold\n"

// runSetup is the interactive first-run wizard: it collects broker connection
// details, a sync target, interval and rotation policy, writes agent.json,
// offers a test pull, and offers to install a per-user scheduler. in/out are
// injected so the flow is testable. It returns non-nil only on write errors;
// a failed test pull is reported but leaves the written config in place.
func runSetup(cfgPath string, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	// One scanner drives every prompt. ask prints prompt and returns the
	// trimmed line; on EOF it returns "" and sets eof so required-field loops
	// abort instead of re-prompting forever (optional prompts treat "" as the
	// bracketed default either way).
	eof := false
	ask := func(prompt string) string {
		fmt.Fprint(out, prompt)
		if !sc.Scan() {
			eof = true
			return ""
		}
		return strings.TrimSpace(sc.Text())
	}

	p := expandHome(cfgPath)
	fmt.Fprintf(out, "ccbroker client setup — writes %s\n", p)

	if _, err := os.Stat(p); err == nil {
		if !yes(ask("agent.json already exists — overwrite? [y/N]: "), false) {
			fmt.Fprintln(out, "aborted; existing config left untouched")
			return nil
		}
	}

	var brokerURL string
	for {
		brokerURL = ask("Broker URL (e.g. https://broker.example.com): ")
		if strings.HasPrefix(brokerURL, "http://") || strings.HasPrefix(brokerURL, "https://") {
			break
		}
		if eof {
			return fmt.Errorf("aborted: unexpected end of input")
		}
		fmt.Fprintln(out, "  must start with http:// or https://")
	}

	var token string
	for {
		token = ask("Client token: ")
		if token != "" {
			break
		}
		if eof {
			return fmt.Errorf("aborted: unexpected end of input")
		}
		fmt.Fprintln(out, "  required")
	}

	caCert := ask("Custom CA certificate path (empty = system CAs) []: ")

	clientCert := ask("Client certificate for mTLS (empty = none) []: ")
	clientKey := ""
	if clientCert != "" {
		for {
			clientKey = ask("Client key path: ")
			if clientKey != "" {
				break
			}
			if eof {
				return fmt.Errorf("aborted: unexpected end of input")
			}
			fmt.Fprintln(out, "  required")
		}
	}

	proxyURL := ""
	for {
		proxyURL = ask("Proxy URL (empty = none/env, e.g. socks5://localhost:1055) []: ")
		if proxyURL == "" {
			break
		}
		if u, err := url.Parse(proxyURL); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "socks5" || u.Scheme == "socks5h") {
			break
		}
		fmt.Fprintln(out, "  must be http(s)://, socks5:// or socks5h://")
	}

	// @active follows `ccb use`/auto-rotation, so one target serves every account.
	target := config.Target{Cred: "@active"}
	useFile := true
	if runtime.GOOS == "darwin" {
		for {
			fmt.Fprintln(out, "Sync target:")
			fmt.Fprintln(out, "  1) macOS Keychain (default)")
			fmt.Fprintln(out, "  2) credentials file")
			switch ask("Choice [1]: ") {
			case "", "1":
				useFile = false
			case "2":
				useFile = true
			default:
				fmt.Fprintln(out, "  choose 1 or 2")
				continue
			}
			break
		}
	}
	if useFile {
		path := ask("Credentials file path [~/.claude/.credentials.json]: ")
		if path == "" {
			path = "~/.claude/.credentials.json"
		}
		target.Type = "file"
		target.Path = path
	} else {
		target.Type = "keychain"
	}

	interval := int64(1800)
	for {
		s := ask("Sync interval seconds [1800]: ")
		if s == "" {
			break
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			interval = n
			break
		}
		fmt.Fprintln(out, "  enter a positive integer")
	}

	fmt.Fprint(out, policyMenu)
	policy := "manual"
	for {
		switch ask("Choice [1]: ") {
		case "", "1":
			policy = "manual"
		case "2":
			policy = "account"
		case "3":
			policy = "all"
		default:
			fmt.Fprintln(out, "  choose 1, 2 or 3")
			continue
		}
		break
	}
	threshold := 0.95
	if policy != "manual" {
		for {
			s := ask("Rotation threshold 0..1 [0.95]: ")
			if s == "" {
				break
			}
			if t, err := strconv.ParseFloat(s, 64); err == nil && t > 0 && t <= 2 {
				threshold = t
				break
			}
			fmt.Fprintln(out, "  enter a number greater than 0 and at most 2")
		}
	}
	fmt.Fprintln(out, "You can change this later with: ccb policy <manual|account|all> (or /ccb-policy in the Claude Code plugin).")

	cfg := config.Agent{
		BrokerURL:      brokerURL,
		Token:          token,
		IntervalSec:    interval,
		Targets:        []config.Target{target},
		CACertPath:     caCert,
		ClientCertPath: clientCert,
		ClientKeyPath:  clientKey,
		ProxyURL:       proxyURL,
		AutoPolicy:     policy,
		AutoThreshold:  threshold,
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(p, append(b, '\n')); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s\n", p)

	// A failed test pull is reported, not fatal: the config is already written.
	if yes(ask("Test the connection with a pull now? [Y/n]: "), true) {
		if loaded, err := config.LoadAgent(p); err != nil {
			fmt.Fprintf(out, "test skipped: %v\n", err)
		} else if client, err := httpClient(loaded); err != nil {
			fmt.Fprintf(out, "test skipped: %v\n", err)
		} else if n := syncCycle(loaded, client, false); n > 0 {
			fmt.Fprintf(out, "test pull reported %d failure(s) — check the broker URL/token; config was still written\n", n)
		} else {
			fmt.Fprintln(out, "test pull OK")
		}
	}

	offerScheduler(p, interval, ask, out)
	return nil
}

// yes interprets a [Y/n] or [y/N] answer; empty input takes def.
func yes(ans string, def bool) bool {
	switch strings.ToLower(ans) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

// offerScheduler installs the watch daemon as the DEFAULT (design C3): a
// long-poll daemon that syncs on every rotation, plus a periodic `ccb sync`
// watchdog that bounds the outage if the daemon dies. Unit-file contents come
// from the pure builders below; only command execution lives here. cfgPath must
// already be absolute.
func offerScheduler(cfgPath string, interval int64, ask func(string) string, out io.Writer) {
	exe, err := resolveExe()
	if err != nil {
		fmt.Fprintf(out, "scheduler skipped: %v\n", err)
		return
	}
	logPath := expandHome("~/.config/ccbroker/agent.log")
	outageBound := func() {
		fmt.Fprintf(out, "If the watch daemon stops, the sync watchdog bounds any outage to ≤%ds (one sync interval).\n", interval)
	}
	switch runtime.GOOS {
	case "darwin":
		if !yes(ask("Install a launchd watch daemon (+ periodic sync watchdog)? [Y/n]: "), true) {
			fmt.Fprintln(out, "skipped; run `ccb watch` under your own supervisor or re-run `ccb setup`.")
			return
		}
		watch := expandHome("~/Library/LaunchAgents/com.ccbroker.watch.plist")
		sync := expandHome("~/Library/LaunchAgents/com.ccbroker.sync.plist")
		if err := writeFile(watch, []byte(launchdWatchPlist(exe, cfgPath, logPath))); err != nil {
			fmt.Fprintf(out, "scheduler failed: %v\n", err)
			return
		}
		if err := writeFile(sync, []byte(launchdSyncPlist(exe, cfgPath, logPath, interval))); err != nil {
			fmt.Fprintf(out, "scheduler failed: %v\n", err)
			return
		}
		for _, p := range []string{watch, sync} {
			exec.Command("launchctl", "unload", p).Run() // ignore: not loaded yet
			if err := exec.Command("launchctl", "load", "-w", p).Run(); err != nil {
				fmt.Fprintf(out, "launchctl load failed: %v\n", err)
				return
			}
		}
		fmt.Fprintf(out, "launchd watch daemon + sync watchdog installed. Undo with:\n  launchctl unload %s %s && rm %s %s\n", watch, sync, watch, sync)
		outageBound()
	case "linux":
		unitDir := expandHome("~/.config/systemd/user")
		if err := os.MkdirAll(unitDir, 0o700); err != nil || exec.Command("systemctl", "--user", "daemon-reload").Run() != nil {
			printCronFallback(out, exe, cfgPath, interval)
			return
		}
		if !yes(ask("Install a systemd-user watch service (Restart=always) + periodic sync watchdog? [Y/n]: "), true) {
			fmt.Fprintln(out, "skipped; run `ccb watch` under your own supervisor or re-run `ccb setup`.")
			return
		}
		service := systemdWatchService(exe, cfgPath)
		syncSvc, syncTimer := systemdSyncUnits(exe, cfgPath, interval)
		for name, body := range map[string]string{
			"ccb-watch.service": service,
			"ccb-sync.service":  syncSvc,
			"ccb-sync.timer":    syncTimer,
		} {
			if err := writeFile(filepath.Join(unitDir, name), []byte(body)); err != nil {
				fmt.Fprintf(out, "scheduler failed: %v\n", err)
				return
			}
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		if err := exec.Command("systemctl", "--user", "enable", "--now", "ccb-watch.service").Run(); err != nil {
			fmt.Fprintf(out, "systemctl enable ccb-watch failed: %v\n", err)
			return
		}
		if err := exec.Command("systemctl", "--user", "enable", "--now", "ccb-sync.timer").Run(); err != nil {
			fmt.Fprintf(out, "systemctl enable ccb-sync.timer failed: %v\n", err)
			return
		}
		fmt.Fprintln(out, "systemd-user watch service + sync watchdog installed. To run while logged out: sudo loginctl enable-linger $USER")
		outageBound()
	default:
		printCronFallback(out, exe, cfgPath, interval)
	}
}

// printCronFallback prints the crontab lines for a host with no launchd/systemd:
// a */5 ensure-alive wrapper that restarts the watch daemon, plus the periodic
// sync watchdog (design C3).
func printCronFallback(out io.Writer, exe, cfgPath string, interval int64) {
	ensureAlive, sync := cronLines(exe, cfgPath, interval)
	fmt.Fprintln(out, "No supported service manager detected. Run `ccb watch` under your process supervisor,")
	fmt.Fprintln(out, "or add these crontab lines (crontab -e) — the first keeps the watch daemon alive, the second is the sync watchdog:")
	fmt.Fprintf(out, "  %s\n  %s\n", ensureAlive, sync)
	fmt.Fprintf(out, "If the watch daemon stops, the sync watchdog bounds any outage to ≤%ds (one sync interval).\n", interval)
}

// resolveExe returns the absolute, symlink-resolved path of the running binary
// so scheduler units invoke a stable ccb path.
func resolveExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// xmlEscaper escapes the metacharacters that would make the launchd plist
// malformed when a path contains &, <, or > (e.g. a "Foo & Bar" directory).
var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// launchdWatchPlist builds the KeepAlive LaunchAgent that runs the `ccb watch`
// daemon (design C3). Pure: no I/O, for testability.
func launchdWatchPlist(exe, cfgPath, logPath string) string {
	exe = xmlEscaper.Replace(exe)
	cfgPath = xmlEscaper.Replace(cfgPath)
	logPath = xmlEscaper.Replace(logPath)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.ccbroker.watch</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>watch</string>
		<string>-c</string>
		<string>%s</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, exe, cfgPath, logPath, logPath)
}

// launchdSyncPlist builds the StartInterval LaunchAgent that runs `ccb sync`
// every interval seconds as the watchdog fallback. Pure: no I/O, for testability.
func launchdSyncPlist(exe, cfgPath, logPath string, interval int64) string {
	exe = xmlEscaper.Replace(exe)
	cfgPath = xmlEscaper.Replace(cfgPath)
	logPath = xmlEscaper.Replace(logPath)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.ccbroker.sync</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>sync</string>
		<string>-c</string>
		<string>%s</string>
	</array>
	<key>StartInterval</key>
	<integer>%d</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, exe, cfgPath, interval, logPath, logPath)
}

// systemdQuote wraps a path for a systemd ExecStart= argument so a space in the
// path does not split it into extra arguments. Backslashes and double quotes are
// escaped per systemd's command-line unquoting rules.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// systemdWatchService builds the ccb-watch.service (Restart=always) that runs
// the `ccb watch` daemon. Pure: no I/O, for testability.
func systemdWatchService(exe, cfgPath string) string {
	return fmt.Sprintf(`[Unit]
Description=ccbroker credential watch daemon
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s watch -c %s
Restart=always
RestartSec=5
StandardOutput=append:%%h/.config/ccbroker/agent.log
StandardError=append:%%h/.config/ccbroker/agent.log

[Install]
WantedBy=default.target
`, systemdQuote(exe), systemdQuote(cfgPath))
}

// systemdSyncUnits builds the ccb-sync.service (oneshot) and ccb-sync.timer that
// run `ccb sync` every interval seconds as the watchdog fallback. Pure: no I/O.
func systemdSyncUnits(exe, cfgPath string, interval int64) (service, timer string) {
	service = fmt.Sprintf(`[Unit]
Description=ccbroker credential sync watchdog

[Service]
Type=oneshot
ExecStart=%s sync -c %s
StandardOutput=append:%%h/.config/ccbroker/agent.log
StandardError=append:%%h/.config/ccbroker/agent.log
`, systemdQuote(exe), systemdQuote(cfgPath))
	timer = fmt.Sprintf(`[Unit]
Description=ccbroker credential sync watchdog timer

[Timer]
OnBootSec=60
OnUnitActiveSec=%d
Persistent=true

[Install]
WantedBy=timers.target
`, interval)
	return service, timer
}

// cronLines builds the crontab fallback: a */5 ensure-alive wrapper (restarts
// the watch daemon) and the periodic sync watchdog. Pure: no I/O.
func cronLines(exe, cfgPath string, interval int64) (ensureAlive, sync string) {
	mins := interval / 60
	if mins < 1 {
		mins = 1
	}
	if mins > 59 {
		mins = 59 // the cron minute field is 0-59; */60 is invalid (MINOR-7)
	}
	ensureAlive = fmt.Sprintf("*/5 * * * * %s ensure-alive -c %s", exe, cfgPath)
	sync = fmt.Sprintf("*/%d * * * * %s sync -c %s", mins, exe, cfgPath)
	return ensureAlive, sync
}
