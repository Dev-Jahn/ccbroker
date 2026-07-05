// Package config defines and loads the broker daemon and agent configuration.
package config

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Client is an authorized machine that may read a scoped set of credentials.
type Client struct {
	Name        string   `json:"name"`
	TokenSHA256 string   `json:"tokenSha256"` // hex sha256 of the bearer token
	Scopes      []string `json:"scopes"`      // cred names, or ["*"]
}

// Server is the daemon configuration.
type Server struct {
	Listen         string   `json:"listen"`      // credential API, e.g. ":8787"
	AdminListen    string   `json:"adminListen"` // admin API, e.g. "127.0.0.1:8788"
	AdminToken     string   `json:"adminToken"`  // bearer for the admin API
	StorePath      string   `json:"storePath"`
	KeyPath        string   `json:"keyPath"` // 32-byte master key, hex-encoded
	AuditLog       string   `json:"auditLog"`
	RefreshSkewSec int64    `json:"refreshSkewSec"` // refresh N sec before expiry; keep well above agents' intervalSec (default 3600)
	UsagePollSec   int64    `json:"usagePollSec"`   // poll quota usage every N sec
	Clients        []Client `json:"clients"`
}

// LoadServer reads and validates a server config file.
func LoadServer(path string) (*Server, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Server
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = ":8787"
	}
	if c.AdminListen == "" {
		c.AdminListen = "127.0.0.1:8788"
	}
	if c.RefreshSkewSec == 0 {
		c.RefreshSkewSec = 3600
	}
	if c.UsagePollSec == 0 {
		c.UsagePollSec = 300
	}
	if c.StorePath == "" || c.KeyPath == "" {
		return nil, fmt.Errorf("storePath and keyPath are required")
	}
	return &c, nil
}

// Target is one local destination the agent keeps in sync.
type Target struct {
	Cred string `json:"cred"`           // broker credential name
	Type string `json:"type"`           // "file" | "keychain"
	Path string `json:"path,omitempty"` // for type=file
}

// Agent is the client agent configuration.
type Agent struct {
	BrokerURL   string   `json:"brokerUrl"`
	Token       string   `json:"token"`
	IntervalSec int64    `json:"intervalSec"`
	Targets     []Target `json:"targets"`
	CACertPath  string   `json:"caCertPath,omitempty"`
	Insecure    bool     `json:"insecure,omitempty"`
	// ClientCertPath/ClientKeyPath present a TLS client certificate for mTLS,
	// e.g. when the broker sits behind a reverse proxy that verifies client certs.
	ClientCertPath string `json:"clientCertPath,omitempty"`
	ClientKeyPath  string `json:"clientKeyPath,omitempty"`
	// ProxyURL is an explicit proxy for reaching the broker, e.g.
	// "socks5://localhost:1055" for a tailscaled running with
	// --tun=userspace-networking; empty honors the standard proxy env vars.
	ProxyURL string `json:"proxyUrl,omitempty"`
	// ActiveFile holds the credential name that "@active" targets resolve to;
	// written by `ccb use <name>`.
	ActiveFile string `json:"activeFile,omitempty"`
	// Auto makes pull/run auto-switch the active account to the least-utilized
	// one whenever the current account reaches AutoThreshold.
	Auto          bool    `json:"auto,omitempty"`
	AutoThreshold float64 `json:"autoThreshold,omitempty"` // default 0.95
	// AutoPolicy selects which quota windows drive auto-rotation:
	//   "manual"  — pull/run never rotate (ccb auto still rotates, account metric)
	//   "account" — rotate when max(5h, 7d) reaches AutoThreshold
	//   "all"     — additionally rotate when any model-scoped weekly bucket reaches it
	// Empty falls back to the legacy Auto bool (true→account, false→manual).
	AutoPolicy string `json:"autoPolicy,omitempty"`
}

// EffectivePolicy resolves AutoPolicy, falling back to the legacy Auto bool.
func (a *Agent) EffectivePolicy() string {
	if a.AutoPolicy != "" {
		return a.AutoPolicy
	}
	if a.Auto {
		return "account"
	}
	return "manual"
}

// LoadAgent reads and validates an agent config file.
func LoadAgent(path string) (*Agent, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Agent
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.BrokerURL == "" || c.Token == "" {
		return nil, fmt.Errorf("brokerUrl and token are required")
	}
	switch c.AutoPolicy {
	case "", "manual", "account", "all":
	default:
		return nil, fmt.Errorf("autoPolicy must be manual, account or all (got %q)", c.AutoPolicy)
	}
	if c.ProxyURL != "" {
		u, err := url.Parse(c.ProxyURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return nil, fmt.Errorf("proxyUrl must be http(s)://, socks5:// or socks5h:// (got %q)", c.ProxyURL)
		}
	}
	if c.IntervalSec == 0 {
		c.IntervalSec = 1800
	}
	if c.ActiveFile == "" {
		c.ActiveFile = "~/.config/ccbroker/active"
	}
	if c.AutoThreshold == 0 {
		c.AutoThreshold = 0.95
	}
	c.BrokerURL = strings.TrimRight(c.BrokerURL, "/")
	return &c, nil
}

// LoadKey reads a 32-byte master key from a hex-encoded key file.
func LoadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("key file must be hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}
