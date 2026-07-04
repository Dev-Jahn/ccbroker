package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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

// offerScheduler offers to install a per-user scheduler that runs `ccb pull`
// on the sync interval. Unit-file contents come from the pure builders below;
// only command execution lives here. cfgPath must already be absolute.
func offerScheduler(cfgPath string, interval int64, ask func(string) string, out io.Writer) {
	exe, err := resolveExe()
	if err != nil {
		fmt.Fprintf(out, "scheduler skipped: %v\n", err)
		return
	}
	switch runtime.GOOS {
	case "darwin":
		if !yes(ask(fmt.Sprintf("Install a launchd agent to run 'ccb pull' every %ds? [Y/n]: ", interval)), true) {
			return
		}
		plist := expandHome("~/Library/LaunchAgents/com.ccbroker.pull.plist")
		logPath := expandHome("~/.config/ccbroker/agent.log")
		if err := writeFile(plist, []byte(launchdPlist(exe, cfgPath, logPath, interval))); err != nil {
			fmt.Fprintf(out, "scheduler failed: %v\n", err)
			return
		}
		exec.Command("launchctl", "unload", plist).Run() // ignore: not loaded yet
		if err := exec.Command("launchctl", "load", "-w", plist).Run(); err != nil {
			fmt.Fprintf(out, "launchctl load failed: %v\n", err)
			return
		}
		fmt.Fprintf(out, "launchd agent installed. Undo with: launchctl unload %s && rm %s\n", plist, plist)
	case "linux":
		unitDir := expandHome("~/.config/systemd/user")
		if err := os.MkdirAll(unitDir, 0o700); err != nil || exec.Command("systemctl", "--user", "daemon-reload").Run() != nil {
			printCrontab(out, exe, cfgPath, interval)
			return
		}
		if !yes(ask(fmt.Sprintf("Install a systemd user timer to run 'ccb pull' every %ds? [Y/n]: ", interval)), true) {
			return
		}
		service, timer := systemdUnits(exe, cfgPath, interval)
		if err := writeFile(filepath.Join(unitDir, "ccb-pull.service"), []byte(service)); err != nil {
			fmt.Fprintf(out, "scheduler failed: %v\n", err)
			return
		}
		if err := writeFile(filepath.Join(unitDir, "ccb-pull.timer"), []byte(timer)); err != nil {
			fmt.Fprintf(out, "scheduler failed: %v\n", err)
			return
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		if err := exec.Command("systemctl", "--user", "enable", "--now", "ccb-pull.timer").Run(); err != nil {
			fmt.Fprintf(out, "systemctl enable failed: %v\n", err)
			return
		}
		fmt.Fprintln(out, "systemd user timer installed. To run while logged out: sudo loginctl enable-linger $USER")
	default:
		printCrontab(out, exe, cfgPath, interval)
	}
}

func printCrontab(out io.Writer, exe, cfgPath string, interval int64) {
	mins := interval / 60
	if mins < 1 {
		mins = 1
	}
	fmt.Fprintln(out, "No supported scheduler detected. Add this crontab line (crontab -e) to pull periodically:")
	fmt.Fprintf(out, "  */%d * * * * %s pull -c %s\n", mins, exe, cfgPath)
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

// launchdPlist builds the LaunchAgent plist that runs `ccb pull` every interval
// seconds. Pure: no I/O, for testability.
func launchdPlist(exe, cfgPath, logPath string, interval int64) string {
	// Paths land inside <string> elements, so XML-escape them first.
	exe = xmlEscaper.Replace(exe)
	cfgPath = xmlEscaper.Replace(cfgPath)
	logPath = xmlEscaper.Replace(logPath)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.ccbroker.pull</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>pull</string>
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

// systemdUnits builds the ccb-pull.service (oneshot) and ccb-pull.timer that
// run `ccb pull` every interval seconds. Pure: no I/O, for testability.
func systemdUnits(exe, cfgPath string, interval int64) (service, timer string) {
	service = fmt.Sprintf(`[Unit]
Description=ccbroker credential pull

[Service]
Type=oneshot
ExecStart=%s pull -c %s
StandardOutput=append:%%h/.config/ccbroker/agent.log
StandardError=append:%%h/.config/ccbroker/agent.log
`, systemdQuote(exe), systemdQuote(cfgPath))
	timer = fmt.Sprintf(`[Unit]
Description=ccbroker credential pull timer

[Timer]
OnBootSec=15
OnUnitActiveSec=%d
Persistent=true

[Install]
WantedBy=timers.target
`, interval)
	return service, timer
}
