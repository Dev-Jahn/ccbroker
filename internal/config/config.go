// Package config defines and loads the broker daemon and agent configuration.
package config

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	Listen         string   `json:"listen"`         // credential API, e.g. ":8787"
	AdminListen    string   `json:"adminListen"`    // admin API, e.g. "127.0.0.1:8788"
	AdminToken     string   `json:"adminToken"`     // bearer for the admin API
	StorePath      string   `json:"storePath"`
	KeyPath        string   `json:"keyPath"` // 32-byte master key, hex-encoded
	AuditLog       string   `json:"auditLog"`
	RefreshSkewSec int64    `json:"refreshSkewSec"` // refresh when within N sec of expiry
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
		c.RefreshSkewSec = 600
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
	if c.IntervalSec == 0 {
		c.IntervalSec = 1800
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
