package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/Dev-Jahn/ccbroker/internal/config"
)

// ---- overwrite gate (C-1 regression) ----

// TestOverwriteDecision is the C-1 regression: the gate writes only when the
// local refresh token is provably safe, and EVERY failure branch keeps it.
func TestOverwriteDecision(t *testing.T) {
	cases := []struct {
		name      string
		hasRT     bool
		adopted   bool
		reason    string
		probeLive bool
		want      bool
	}{
		{"no-local-RT propagates", false, false, "", false, true},
		{"no-local-RT propagates even on unknown", false, false, "unknown_account", false, true},
		{"adopted writes", true, true, "", false, true},
		{"already_current writes", true, false, "already_current", false, true},
		{"rollback writes", true, false, "rollback", false, true},
		{"offer_not_live + probeLive writes", true, false, "offer_not_live", true, true},
		{"offer_not_live + probe false keeps", true, false, "offer_not_live", false, false},
		{"unknown_account keeps (even if probe true)", true, false, "unknown_account", true, false},
		{"ambiguous_account keeps", true, false, "ambiguous_account", false, false},
		{"migration_pending keeps", true, false, "migration_pending", false, false},
		{"verify_unavailable keeps", true, false, "verify_unavailable", false, false},
		{"conflict keeps", true, false, "conflict", false, false},
		{"rate_limited keeps", true, false, "rate_limited", false, false},
		{"old_broker(404) keeps", true, false, "old_broker", false, false},
		{"offer_error keeps", true, false, "offer_error", false, false},
		// v0.4.1: profile lag on freshly issued tokens — must keep local even
		// with probeLive true (default-deny covers unknown reasons; assert it).
		{"fresh_unverified keeps (even if probe true)", true, false, "fresh_unverified", true, false},
	}
	for _, c := range cases {
		if got := overwriteDecision(c.hasRT, c.adopted, c.reason, c.probeLive); got != c.want {
			t.Errorf("%s: overwriteDecision(hasRT=%v adopted=%v reason=%q probe=%v)=%v want %v",
				c.name, c.hasRT, c.adopted, c.reason, c.probeLive, got, c.want)
		}
	}
}

func TestStripRefreshToken(t *testing.T) {
	in := map[string]any{"accessToken": "a", "refreshToken": "r", "expiresAt": float64(1)}
	out := stripRefreshToken(in)
	if _, ok := out["refreshToken"]; ok {
		t.Errorf("strip left refreshToken: %v", out)
	}
	if out["accessToken"] != "a" {
		t.Errorf("strip dropped accessToken")
	}
	if _, ok := in["refreshToken"]; !ok {
		t.Errorf("strip mutated its input")
	}
}

func TestParseOAuthBytes(t *testing.T) {
	full := []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r"}}`)
	if m := parseOAuthBytes(full); m == nil || m["accessToken"] != "a" {
		t.Errorf("full file: %v", m)
	}
	bare := []byte(`{"accessToken":"a"}`)
	if m := parseOAuthBytes(bare); m == nil || m["accessToken"] != "a" {
		t.Errorf("bare oauth: %v", m)
	}
	if m := parseOAuthBytes([]byte(`{"other":1}`)); m != nil {
		t.Errorf("non-credential json should be nil, got %v", m)
	}
}

// ---- fake broker for sync/longPoll integration ----

type fakeBroker struct {
	*httptest.Server
	offer     offerResult // canned offer response body
	offerHTTP int         // if !=0, override offer status
	env       credEnvelope
	getHTTP   int    // if !=0, override GET status
	getRaw    string // if set, GET returns this raw body (pre-v0.4 broker shape)
	getGen    int64  // X-Ccb-Gen for long-poll responses
}

func newFakeBroker(t *testing.T) *fakeBroker {
	fb := &fakeBroker{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/creds/offer", func(w http.ResponseWriter, r *http.Request) {
		if fb.offerHTTP != 0 {
			w.WriteHeader(fb.offerHTTP)
			return
		}
		_ = json.NewEncoder(w).Encode(fb.offer)
	})
	mux.HandleFunc("/v1/credentials/", func(w http.ResponseWriter, r *http.Request) {
		if fb.getGen != 0 {
			w.Header().Set("X-Ccb-Gen", strconv.FormatInt(fb.getGen, 10))
		}
		if fb.getHTTP != 0 {
			w.WriteHeader(fb.getHTTP)
			return
		}
		if fb.getRaw != "" {
			_, _ = w.Write([]byte(fb.getRaw))
			return
		}
		env := fb.env
		if r.URL.Query().Get("probe") == "1" {
			env.ProbeLive = fb.env.ProbeLive
		}
		_ = json.NewEncoder(w).Encode(env)
	})
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"credentials": []any{}})
	})
	fb.Server = httptest.NewServer(mux)
	t.Cleanup(fb.Close)
	return fb
}

func (fb *fakeBroker) agent(t *testing.T, target config.Target) (*config.Agent, *http.Client) {
	cfg := &config.Agent{BrokerURL: fb.URL, Token: "tok", Targets: []config.Target{target}, WatchWaitSec: 20}
	client, err := httpClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, client
}

func writeCredFile(t *testing.T, path string, oauth map[string]any) {
	t.Helper()
	b, _ := json.Marshal(credFile{ClaudeAiOauth: oauth})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCredFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return parseOAuthBytes(b)
}

// ---- sync target integration ----

func TestSyncTargetAdoptWritesStripped(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	writeCredFile(t, credPath, map[string]any{"accessToken": "local-acc", "refreshToken": "local-rt"})

	fb := newFakeBroker(t)
	fb.offer = offerResult{Adopted: true, Name: "personal", Gen: 5}
	fb.env = credEnvelope{ClaudeAiOauth: map[string]any{"accessToken": "broker-acc", "refreshToken": "broker-rt"}, Gen: 5, Account: "p@x"}
	target := config.Target{Cred: "personal", Type: "file", Path: credPath}
	cfg, client := fb.agent(t, target)

	if fail := syncTarget(cfg, client, target, map[string]usageRow{}); fail {
		t.Fatalf("adopt sync reported failure")
	}
	got := readCredFile(t, credPath)
	if got["accessToken"] != "broker-acc" {
		t.Errorf("token not written: %v", got)
	}
	if _, ok := got["refreshToken"]; ok {
		t.Errorf("refresh token not stripped on write (I1): %v", got)
	}
}

func TestSyncTargetNoLocalRTPropagates(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	// Local already stripped (steady state): no refresh token.
	writeCredFile(t, credPath, map[string]any{"accessToken": "old-acc"})

	fb := newFakeBroker(t)
	fb.env = credEnvelope{ClaudeAiOauth: map[string]any{"accessToken": "broker-acc", "refreshToken": "broker-rt"}, Gen: 9, Account: "p@x"}
	target := config.Target{Cred: "personal", Type: "file", Path: credPath}
	cfg, client := fb.agent(t, target)

	if fail := syncTarget(cfg, client, target, map[string]usageRow{}); fail {
		t.Fatalf("propagation reported failure")
	}
	got := readCredFile(t, credPath)
	if got["accessToken"] != "broker-acc" {
		t.Errorf("token not propagated: %v", got)
	}
	if _, ok := got["refreshToken"]; ok {
		t.Errorf("refresh token leaked on write: %v", got)
	}
}

// TestSyncTargetKeepsLocalRTOnFailure covers the C-1 branches where the offer
// path fails: the local refresh token must survive untouched.
func TestSyncTargetKeepsLocalRTOnFailure(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*fakeBroker)
	}{
		{"offer 500 (offer_error)", func(fb *fakeBroker) { fb.offerHTTP = http.StatusInternalServerError }},
		{"offer 404 (old broker)", func(fb *fakeBroker) {
			fb.offerHTTP = http.StatusNotFound
			fb.getRaw = `{"claudeAiOauth":{"accessToken":"broker-acc","refreshToken":"broker-rt"}}`
		}},
		{"offer 429 (rate limited)", func(fb *fakeBroker) { fb.offerHTTP = http.StatusTooManyRequests }},
		{"unknown_account", func(fb *fakeBroker) {
			fb.offer = offerResult{Reason: "unknown_account", Account: "z@x"}
		}},
		{"offer_not_live + probe false", func(fb *fakeBroker) {
			fb.offer = offerResult{Reason: "offer_not_live"}
			fb.env = credEnvelope{ClaudeAiOauth: map[string]any{"accessToken": "broker-acc"}, ProbeLive: false}
		}},
		{"GET 500 after offer error", func(fb *fakeBroker) {
			fb.offer = offerResult{Reason: "verify_unavailable"}
			fb.getHTTP = http.StatusInternalServerError
		}},
		{"GET 409 suspect even after adopt", func(fb *fakeBroker) {
			// Offer adopted (RT safe broker-side) but the GET 409s: still keep local
			// and retry next cycle rather than writing nothing/erroring.
			fb.offer = offerResult{Adopted: true, Name: "personal", Gen: 4}
			fb.getHTTP = http.StatusConflict
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			credPath := filepath.Join(dir, "creds.json")
			local := map[string]any{"accessToken": "local-acc", "refreshToken": "local-rt"}
			writeCredFile(t, credPath, local)

			fb := newFakeBroker(t)
			// A default healthy GET envelope unless a case overrides it.
			fb.env = credEnvelope{ClaudeAiOauth: map[string]any{"accessToken": "broker-acc", "refreshToken": "broker-rt"}, Gen: 3, Account: "p@x"}
			c.configure(fb)
			target := config.Target{Cred: "personal", Type: "file", Path: credPath}
			cfg, client := fb.agent(t, target)

			syncTarget(cfg, client, target, map[string]usageRow{})

			got := readCredFile(t, credPath)
			if got["accessToken"] != "local-acc" || got["refreshToken"] != "local-rt" {
				t.Errorf("local credential clobbered on failure branch %q: %v", c.name, got)
			}
		})
	}
}

// TestSyncTargetLocalReadErrorKeepsTarget is the MAJOR-1 regression: a local
// read FAILURE must not be treated as "no credential" and overwrite a target
// that actually holds a live refresh token we simply couldn't read.
func TestSyncTargetLocalReadErrorKeepsTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadable file not reproducible on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions; read error not reproducible")
	}
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	writeCredFile(t, credPath, map[string]any{"accessToken": "local-acc", "refreshToken": "local-rt"})
	if err := os.Chmod(credPath, 0o000); err != nil { // make it unreadable → read error
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(credPath, 0o600) })

	fb := newFakeBroker(t)
	fb.env = credEnvelope{ClaudeAiOauth: map[string]any{"accessToken": "broker-acc", "refreshToken": "broker-rt"}, Gen: 5, Account: "p@x"}
	target := config.Target{Cred: "personal", Type: "file", Path: credPath}
	cfg, client := fb.agent(t, target)

	if fail := syncTarget(cfg, client, target, map[string]usageRow{}); !fail {
		t.Errorf("read error should count as a failure (skip), not a success")
	}
	os.Chmod(credPath, 0o600)
	got := readCredFile(t, credPath)
	if got["accessToken"] != "local-acc" || got["refreshToken"] != "local-rt" {
		t.Errorf("target overwritten despite a local read error: %v", got)
	}
}

// TestWriteIfUnchanged is the MAJOR-2 TOCTOU regression: the write is aborted if
// the target changed since the gate read it — especially if a /login dropped a
// fresh refresh token there.
func TestWriteIfUnchanged(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	target := config.Target{Type: "file", Path: credPath}
	brokerBody, _ := json.Marshal(credFile{ClaudeAiOauth: map[string]any{"accessToken": "broker-acc"}})

	// Snapshot matches current → writes.
	snapshot := map[string]any{"accessToken": "old-acc"}
	writeCredFile(t, credPath, snapshot)
	if wrote, err := writeIfUnchanged(target, snapshot, brokerBody); err != nil || !wrote {
		t.Fatalf("unchanged target should write: wrote=%v err=%v", wrote, err)
	}
	if got := readCredFile(t, credPath); got["accessToken"] != "broker-acc" {
		t.Errorf("write did not land: %v", got)
	}

	// A concurrent /login drops a fresh RT after the snapshot → abort the write.
	snapshot = map[string]any{"accessToken": "a"} // gate saw no RT
	writeCredFile(t, credPath, map[string]any{"accessToken": "a", "refreshToken": "fresh-login-rt"})
	if wrote, err := writeIfUnchanged(target, snapshot, brokerBody); err != nil || wrote {
		t.Fatalf("changed target should abort: wrote=%v err=%v", wrote, err)
	}
	if got := readCredFile(t, credPath); got["refreshToken"] != "fresh-login-rt" {
		t.Errorf("aborted write clobbered the fresh /login RT: %v", got)
	}
}

// TestWatchLockSingleInstance is the MAJOR-4 regression: the advisory lock admits
// exactly one live watcher; a second acquisition fails until the first releases.
func TestWatchLockSingleInstance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based single-instance guard is unix-only")
	}
	path := filepath.Join(t.TempDir(), "watch.pid")

	rel1, ok1, err1 := acquireWatchLock(path)
	if err1 != nil || !ok1 {
		t.Fatalf("first acquire: ok=%v err=%v", ok1, err1)
	}
	if !watchLockHeld(path) {
		t.Errorf("watchLockHeld should report the held lock")
	}
	if _, ok2, err2 := acquireWatchLock(path); err2 != nil || ok2 {
		t.Errorf("second acquire should fail while first is held: ok=%v err=%v", ok2, err2)
	}
	rel1()
	rel3, ok3, err3 := acquireWatchLock(path)
	if err3 != nil || !ok3 {
		t.Fatalf("acquire after release should succeed: ok=%v err=%v", ok3, err3)
	}
	rel3()
	if watchLockHeld(path) {
		t.Errorf("no watcher running → watchLockHeld should be false")
	}
}

// ---- long-poll status mapping (C2) ----

func TestLongPollMapping(t *testing.T) {
	fb := newFakeBroker(t)
	cfg, client := fb.agent(t, config.Target{Cred: "personal", Type: "file", Path: "/tmp/x"})

	// 200 → changed, gen from header.
	fb.getGen = 7
	fb.env = credEnvelope{Gen: 7}
	gen, changed, err := longPoll(context.Background(), cfg, client, "personal", 3, 0)
	if err != nil || !changed || gen != 7 {
		t.Errorf("200: gen=%d changed=%v err=%v want 7,true,nil", gen, changed, err)
	}

	// 304 → not changed, gen advanced from header.
	fb.getHTTP = http.StatusNotModified
	fb.getGen = 7
	gen, changed, err = longPoll(context.Background(), cfg, client, "personal", 7, 0)
	if err != nil || changed || gen != 7 {
		t.Errorf("304: gen=%d changed=%v err=%v want 7,false,nil", gen, changed, err)
	}

	// 409 (suspect) → not changed, no error, gen advances so no hot loop.
	fb.getHTTP = http.StatusConflict
	fb.getGen = 12
	gen, changed, err = longPoll(context.Background(), cfg, client, "personal", 7, 0)
	if err != nil || changed || gen != 12 {
		t.Errorf("409: gen=%d changed=%v err=%v want 12,false,nil", gen, changed, err)
	}

	// 500 → error (caller backs off).
	fb.getHTTP = http.StatusInternalServerError
	if _, _, err := longPoll(context.Background(), cfg, client, "personal", 7, 0); err == nil {
		t.Errorf("500 should be an error")
	}
}

func TestJitterBounds(t *testing.T) {
	base := 10 * time.Second
	for i := 0; i < 100; i++ {
		j := jitter(base)
		if j < base || j > base+base/2 {
			t.Fatalf("jitter out of bounds: %s (base %s)", j, base)
		}
	}
	if jitter(0) != 0 {
		t.Errorf("jitter(0) should be 0")
	}
}

func TestWatchClientTimeout(t *testing.T) {
	c, err := httpClientTimeout(&config.Agent{}, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 90*time.Second {
		t.Errorf("watch client timeout=%s want 90s", c.Timeout)
	}
}
