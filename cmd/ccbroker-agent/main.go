// Command ccbroker-agent pulls credentials from the broker and writes them to
// local destinations (a .credentials.json file, or the macOS Keychain) so that
// Claude Code / CCS always see a freshly-refreshed token.
//
// Usage:
//
//	ccbroker-agent pull -c agent.json          # one-shot sync
//	ccbroker-agent run  -c agent.json          # sync on an interval
//	ccbroker-agent use <name> -c agent.json    # switch the "@active" account and sync
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"ccbroker/internal/config"
)

const keychainService = "Claude Code-credentials"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ccbroker-agent {pull|run|use <name>} -c agent.json")
		os.Exit(2)
	}
	cmd := os.Args[1]
	cfgPath := ""
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		if (args[i] == "-c" || args[i] == "--config") && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		}
	}
	if cfgPath == "" {
		fatal(fmt.Errorf("requires -c <agent.json>"))
	}
	cfg, err := config.LoadAgent(cfgPath)
	if err != nil {
		fatal(err)
	}
	client, err := httpClient(cfg)
	if err != nil {
		fatal(err)
	}

	switch cmd {
	case "pull":
		if n := syncAll(cfg, client); n > 0 {
			os.Exit(1)
		}
	case "use":
		name := ""
		for i := 0; i < len(args); i++ {
			if (args[i] == "-c" || args[i] == "--config") && i+1 < len(args) {
				i++
				continue
			}
			name = args[i]
		}
		if name == "" {
			fatal(fmt.Errorf("usage: ccbroker-agent use <cred-name> -c agent.json"))
		}
		if err := writeActive(cfg.ActiveFile, name); err != nil {
			fatal(err)
		}
		logf("active account -> %s", name)
		if n := syncAll(cfg, client); n > 0 {
			os.Exit(1)
		}
	case "run":
		iv := time.Duration(cfg.IntervalSec) * time.Second
		logf("agent started, interval=%s, targets=%d", iv, len(cfg.Targets))
		for {
			syncAll(cfg, client)
			time.Sleep(iv)
		}
	default:
		fatal(fmt.Errorf("unknown command %q", cmd))
	}
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
// activeFile (written by `use`), so one target can follow account switches.
func resolveCred(cfg *config.Agent, cred string) (string, error) {
	if cred != "@active" {
		return cred, nil
	}
	b, err := os.ReadFile(expandHome(cfg.ActiveFile))
	if err != nil {
		return "", fmt.Errorf("@active unresolved (run `ccbroker-agent use <name>`): %w", err)
	}
	name := strings.TrimSpace(string(b))
	if name == "" {
		return "", fmt.Errorf("@active unresolved: %s is empty", cfg.ActiveFile)
	}
	return name, nil
}

// writeActive records name as the current "@active" account.
func writeActive(path, name string) error {
	p := expandHome(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(name+"\n"), 0o600)
}

func fetchCred(cfg *config.Agent, client *http.Client, name string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, cfg.BrokerURL+"/v1/credentials/"+name, nil)
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
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
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
	fmt.Fprintln(os.Stderr, "ccbroker-agent:", err)
	os.Exit(1)
}
