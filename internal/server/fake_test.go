package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Dev-Jahn/ccbroker/internal/anthropic"
	"github.com/Dev-Jahn/ccbroker/internal/config"
	"github.com/Dev-Jahn/ccbroker/internal/creds"
	"github.com/Dev-Jahn/ccbroker/internal/store"
)

// fakeUpstream is an httptest stand-in for api.anthropic.com implementing the
// profile, usage and refresh endpoints. It can model BOTH rotation-semantics the
// design must be safe under: "rotating" (rotating RT + reuse-revocation) and
// "stable" (stable RT + access-revocation-on-rotate).
type fakeUpstream struct {
	mu            sync.Mutex
	live          map[string]acct // access token -> account (profile/usage 200)
	rt            map[string]acct // refresh token -> account
	rtLive        map[string]bool // refresh token still valid
	model         string          // "rotating" | "stable"
	seq           int
	profileSeq    []int // per-call forced profile statuses (popped); 200 = normal
	profileStatus int   // persistent forced profile status when profileSeq empty
	profileHits   int
	lastToken     string      // bearer token of the most recent profile call
	onProfileCall func(n int) // test hook fired on each profile call (n = call number)
	server        *httptest.Server
}

type acct struct{ email, uuid string }

func newFake(model string) *fakeUpstream {
	f := &fakeUpstream{
		live: map[string]acct{}, rt: map[string]acct{}, rtLive: map[string]bool{}, model: model,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/oauth/profile", f.handleProfile)
	mux.HandleFunc("/api/oauth/usage", f.handleUsage)
	mux.HandleFunc("/v1/oauth/token", f.handleToken)
	f.server = httptest.NewServer(mux)
	return f
}

// use points the anthropic package at this fake for the duration of the test.
func (f *fakeUpstream) use(t *testing.T) {
	t.Helper()
	oldEnd, oldClient := anthropic.Endpoint, anthropic.HTTPClient
	anthropic.Endpoint = f.server.URL
	anthropic.HTTPClient = f.server.Client()
	t.Cleanup(func() {
		anthropic.Endpoint = oldEnd
		anthropic.HTTPClient = oldClient
		f.server.Close()
	})
}

func bearerTok(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(p) {
		return h[len(p):]
	}
	return ""
}

func (f *fakeUpstream) handleProfile(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profileHits++
	f.lastToken = bearerTok(r)
	if f.onProfileCall != nil {
		f.onProfileCall(f.profileHits)
	}
	status := 0
	if len(f.profileSeq) > 0 {
		status = f.profileSeq[0]
		f.profileSeq = f.profileSeq[1:]
	} else if f.profileStatus != 0 {
		status = f.profileStatus
	}
	if status != 0 && status != http.StatusOK {
		body := `{"error":"unavailable"}`
		if status == http.StatusForbidden {
			body = `{"error":"OAuth token has been revoked"}`
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
		return
	}
	a, ok := f.live[bearerTok(r)]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"unauthorized"}`)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":      map[string]any{"uuid": a.uuid, "email": a.email},
		"organization": map[string]any{"uuid": "org-" + a.uuid, "name": "Org", "rate_limit_tier": "tier"},
	})
}

func (f *fakeUpstream) handleUsage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[bearerTok(r)]; !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"five_hour": map[string]any{"utilization": 10},
		"seven_day": map[string]any{"utilization": 20},
	})
}

func (f *fakeUpstream) handleToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !f.rtLive[body.RefreshToken] {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"revoked or superseded"}`)
		return
	}
	a := f.rt[body.RefreshToken]
	f.seq++
	newAccess := tokenName("acc", a.uuid, f.seq)
	// Rotation revokes the previous access token(s) for this account (F1).
	for tok, ac := range f.live {
		if ac.uuid == a.uuid {
			delete(f.live, tok)
		}
	}
	f.live[newAccess] = a
	resp := map[string]any{
		"access_token": newAccess, "expires_in": 28800, "token_type": "Bearer",
		"scope":   "user:inference",
		"account": map[string]any{"email_address": a.email, "uuid": a.uuid},
	}
	if f.model == "rotating" {
		f.rtLive[body.RefreshToken] = false // reuse-revocation
		newRT := tokenName("rt", a.uuid, f.seq)
		f.rt[newRT] = a
		f.rtLive[newRT] = true
		resp["refresh_token"] = newRT
	}
	writeJSON(w, http.StatusOK, resp)
}

func tokenName(kind, uuid string, seq int) string {
	return kind + "-" + uuid + "-" + itoa(seq)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (f *fakeUpstream) addAccess(access, email, uuid string) {
	f.mu.Lock()
	f.live[access] = acct{email, uuid}
	f.mu.Unlock()
}

func (f *fakeUpstream) addRT(rt, email, uuid string) {
	f.mu.Lock()
	f.rt[rt] = acct{email, uuid}
	f.rtLive[rt] = true
	f.mu.Unlock()
}

func (f *fakeUpstream) setProfileStatus(s int) {
	f.mu.Lock()
	f.profileStatus = s
	f.mu.Unlock()
}

func (f *fakeUpstream) setProfileSeq(seq ...int) {
	f.mu.Lock()
	f.profileSeq = seq
	f.mu.Unlock()
}

func (f *fakeUpstream) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profileHits
}

func (f *fakeUpstream) lastProbedToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastToken
}

func (f *fakeUpstream) setOnProfileCall(fn func(n int)) {
	f.mu.Lock()
	f.onProfileCall = fn
	f.mu.Unlock()
}

// ---- server test harness ----

const (
	tok1 = "client-token-1"
	tok2 = "client-token-2"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.enc"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// testClock is an injectable, steppable clock.
type testClock struct{ ms atomic.Int64 }

func (c *testClock) now() int64   { return c.ms.Load() }
func (c *testClock) step(d int64) { c.ms.Add(d) }

func newServer(t *testing.T, st *store.Store) (*Server, *testClock) {
	t.Helper()
	cfg := &config.Server{
		Listen: ":0", AdminListen: "127.0.0.1:0", AdminToken: "admin",
		RefreshSkewSec: 3600, UsagePollSec: 300,
		Clients: []config.Client{
			{Name: "c1", TokenSHA256: sha256hex(tok1), Scopes: []string{"*"}},
			{Name: "c2", TokenSHA256: sha256hex(tok2), Scopes: []string{"work"}},
		},
	}
	srv, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	clk := &testClock{}
	clk.ms.Store(1_700_000_000_000)
	srv.now = clk.now
	// Shrink tunables so the lifecycle tests run in milliseconds.
	srv.confirmDelay = 2 * 1_000_000      // 2ms
	srv.reclaimDelay = 5 * 1_000_000      // 5ms wall (gated by clock via HealthSince)
	srv.deadProbeInterval = 5 * 1_000_000 // 5ms
	srv.offerRate = 6
	return srv, clk
}

// putCred stores a healthy credential directly (bypassing offer/import).
func putCred(t *testing.T, srv *Server, name, email, uuid, access, rt string, expiresAt int64) {
	t.Helper()
	rec := &creds.Record{
		Name: name, Account: email, AccountUUID: uuid, Gen: 1,
		HealthState: creds.HealthOK, HealthSince: srv.now(),
		OAuth: map[string]any{
			"accessToken": access, "refreshToken": rt,
			"expiresAt": float64(expiresAt), "scopes": []any{"user:inference"},
		},
	}
	if err := srv.store.Put(rec); err != nil {
		t.Fatal(err)
	}
	srv.seedGen(rec)
}

func oauthBody(access, rt string, expiresAt int64) map[string]any {
	return map[string]any{
		"accessToken": access, "refreshToken": rt,
		"expiresAt": float64(expiresAt), "scopes": []any{"user:inference"},
	}
}
