package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Dev-Jahn/ccbroker/internal/creds"
	"github.com/Dev-Jahn/ccbroker/internal/store"
)

const farFuture = 8 * 60 * 60 * 1000 // 8h in ms

type offerResp struct {
	Adopted bool   `json:"adopted"`
	Name    string `json:"name"`
	Reason  string `json:"reason"`
	Account string `json:"account"`
	Gen     int64  `json:"gen"`
}

func doGet(srv *Server, token, name, rawquery string) *httptest.ResponseRecorder {
	u := "/v1/credentials/" + name
	if rawquery != "" {
		u += "?" + rawquery
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.handleGetCred(w, req)
	return w
}

func doOffer(srv *Server, token string, oauth map[string]any) (*httptest.ResponseRecorder, offerResp) {
	body, _ := json.Marshal(oauth)
	req := httptest.NewRequest(http.MethodPost, "/v1/creds/offer", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.handleOffer(w, req)
	var res offerResp
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w, res
}

func doPut(srv *Server, name string, oauth map[string]any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"claudeAiOauth": oauth})
	req := httptest.NewRequest(http.MethodPut, "/admin/creds/"+name, bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", "admin")
	w := httptest.NewRecorder()
	srv.handleAdmin(w, req)
	return w
}

func mustEnvelope(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", w.Code, w.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, w.Body.String())
	}
	return env
}

// ---- strip / envelope (S1) ----

func TestServeStripsRefreshToken(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-p", "p@x", "uuid-p")
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "personal", "p@x", "uuid-p", "acc-p", "rt-p", srv.now()+farFuture)

	// serveRefreshToken=false → stripped, envelope present.
	env := mustEnvelope(t, doGet(srv, tok1, "personal", ""))
	oauth := env["claudeAiOauth"].(map[string]any)
	if _, ok := oauth["refreshToken"]; ok {
		t.Errorf("served credential leaked refreshToken: %v", oauth)
	}
	if oauth["accessToken"] != "acc-p" {
		t.Errorf("accessToken=%v want acc-p", oauth["accessToken"])
	}
	if env["account"] != "p@x" {
		t.Errorf("envelope account=%v want p@x", env["account"])
	}
	if _, ok := env["gen"]; !ok {
		t.Errorf("envelope missing gen: %v", env)
	}

	// serveRefreshToken=true → refresh token retained (rollout escape hatch).
	srv.cfg.ServeRefreshToken = true
	env = mustEnvelope(t, doGet(srv, tok1, "personal", ""))
	oauth = env["claudeAiOauth"].(map[string]any)
	if oauth["refreshToken"] != "rt-p" {
		t.Errorf("serveRefreshToken=true should keep refreshToken, got %v", oauth["refreshToken"])
	}
}

// ---- gen counter (C-2) ----

func TestGenMonotonicBackwardClock(t *testing.T) {
	srv, clk := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)

	genOf := func() int64 {
		rec, _ := srv.store.Get("a")
		return rec.Gen
	}
	rec, _ := srv.store.Get("a")
	g0 := rec.Gen

	// Forward: two commits, gen strictly increases and tracks now.
	_ = srv.commit(cloneRecord(rec))
	g1 := genOf()
	if g1 <= g0 {
		t.Fatalf("gen did not increase: %d -> %d", g0, g1)
	}
	// Step the clock BACKWARD; gen must still strictly increase (C-2).
	clk.step(-1_000_000)
	rec, _ = srv.store.Get("a")
	_ = srv.commit(cloneRecord(rec))
	g2 := genOf()
	if g2 <= g1 {
		t.Fatalf("gen not monotonic under backward clock: %d -> %d", g1, g2)
	}
}

func TestGenDistinctSameMillisecond(t *testing.T) {
	srv, _ := newServer(t, newStore(t)) // clock is fixed (never steps)
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)
	rec, _ := srv.store.Get("a")
	_ = srv.commit(cloneRecord(rec))
	g1 := mustGen(t, srv, "a")
	rec, _ = srv.store.Get("a")
	_ = srv.commit(cloneRecord(rec))
	g2 := mustGen(t, srv, "a")
	if g1 == g2 {
		t.Fatalf("two same-ms mutations collided on gen %d", g1)
	}
}

func TestGenPersistsAcrossReload(t *testing.T) {
	st := newStore(t)
	srv, _ := newServer(t, st)
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)
	rec, _ := srv.store.Get("a")
	_ = srv.commit(cloneRecord(rec))
	want := mustGen(t, srv, "a")

	// Reopen the store from disk and re-seed a fresh server (N-5): gen survives
	// and the wake map resumes from it.
	st2, err := store.Open(st.Path(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv2, _ := newServer(t, st2)
	if got := mustGen(t, srv2, "a"); got != want {
		t.Errorf("gen after reload=%d want %d", got, want)
	}
	srv2.genMu.Lock()
	seeded := srv2.genState["a"].gen
	srv2.genMu.Unlock()
	if seeded != want {
		t.Errorf("wake map gen after restart=%d want %d", seeded, want)
	}
}

func mustGen(t *testing.T, srv *Server, name string) int64 {
	t.Helper()
	rec, ok := srv.store.Get(name)
	if !ok {
		t.Fatalf("no cred %q", name)
	}
	return rec.Gen
}

// ---- long-poll (S3, M-4) ----

func TestLongPollWakeAndTimeout(t *testing.T) {
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)
	cur := mustGen(t, srv, "a")

	// Timeout → not changed.
	if _, changed := srv.waitForGen(context.Background(), "a", cur, 20*time.Millisecond); changed {
		t.Errorf("waitForGen should time out with no bump")
	}

	// Wake on bump.
	done := make(chan bool, 1)
	go func() {
		_, changed := srv.waitForGen(context.Background(), "a", cur, 2*time.Second)
		done <- changed
	}()
	time.Sleep(20 * time.Millisecond)
	rec, _ := srv.store.Get("a")
	_ = srv.commit(cloneRecord(rec))
	select {
	case changed := <-done:
		if !changed {
			t.Errorf("waitForGen should wake on bump")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForGen did not wake")
	}
}

func TestLongPollNoMissedWakeRace(t *testing.T) {
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)

	var wg sync.WaitGroup
	// Committers + readers hammer the wake map concurrently (M-4 under -race).
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, _ := srv.store.Get("a")
			_ = srv.commit(cloneRecord(rec))
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.waitForGen(context.Background(), "a", 0, 50*time.Millisecond)
		}()
	}
	wg.Wait()
}

func TestGetLongPoll304AndChange(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-a", "a@x", "uuid-a")
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)
	cur := mustGen(t, srv, "a")

	// sinceGen==cur, waitSec=0 → immediate 304 carrying X-Ccb-Gen.
	w := doGet(srv, tok1, "a", "sinceGen="+strconv.FormatInt(cur, 10))
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", w.Code)
	}
	if w.Header().Get("X-Ccb-Gen") != strconv.FormatInt(cur, 10) {
		t.Errorf("304 X-Ccb-Gen=%q want %d", w.Header().Get("X-Ccb-Gen"), cur)
	}
	// sinceGen older than cur → immediate 200.
	w = doGet(srv, tok1, "a", "sinceGen=0")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for stale sinceGen, got %d", w.Code)
	}
}

// ---- offer decision table (S2) ----

func TestOfferDecisionTable(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	// Two managed creds.
	f.addAccess("acc-p0", "p@x", "uuid-p")
	f.addAccess("acc-w0", "w@x", "uuid-w")
	srv, _ := newServer(t, newStore(t))
	srv.offerRate = 100 // this test makes many offers in one window
	putCred(t, srv, "personal", "p@x", "uuid-p", "acc-p0", "rt-p0", srv.now()+farFuture)
	putCred(t, srv, "work", "w@x", "uuid-w", "acc-w0", "rt-w0", srv.now()+farFuture)
	if err := srv.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	// not-live: an unknown/dead access token.
	_, res := doOffer(srv, tok1, oauthBody("dead-token", "rt-x", srv.now()+farFuture))
	if res.Reason != "offer_not_live" {
		t.Errorf("dead token: reason=%q want offer_not_live", res.Reason)
	}

	// verify_unavailable: profile endpoint 500.
	f.setProfileStatus(500)
	_, res = doOffer(srv, tok1, oauthBody("acc-p0", "rt-p0", srv.now()+farFuture))
	if res.Reason != "verify_unavailable" {
		t.Errorf("profile 500: reason=%q want verify_unavailable", res.Reason)
	}
	f.setProfileStatus(0)

	// unknown_account: a live token for an unmanaged account.
	f.addAccess("acc-z", "z@x", "uuid-z")
	_, res = doOffer(srv, tok1, oauthBody("acc-z", "rt-z", srv.now()+farFuture))
	if res.Reason != "unknown_account" {
		t.Errorf("unmanaged account: reason=%q want unknown_account", res.Reason)
	}
	if res.Account != "z@x" {
		t.Errorf("unknown_account should carry email, got %q", res.Account)
	}

	// already_current: offering the currently-stored token.
	_, res = doOffer(srv, tok1, oauthBody("acc-p0", "rt-p0", srv.now()+farFuture))
	if res.Adopted || res.Reason != "already_current" {
		t.Errorf("same token: adopted=%v reason=%q want already_current", res.Adopted, res.Reason)
	}

	// happy-path adopt into the active-ish cred: a new live token for p@x.
	f.addAccess("acc-p1", "p@x", "uuid-p")
	w, res := doOffer(srv, tok1, oauthBody("acc-p1", "rt-p1", srv.now()+farFuture))
	if w.Code != http.StatusOK || !res.Adopted || res.Name != "personal" {
		t.Fatalf("adopt: code=%d adopted=%v name=%q reason=%q", w.Code, res.Adopted, res.Name, res.Reason)
	}
	rec, _ := srv.store.Get("personal")
	if rec.AccessToken() != "acc-p1" {
		t.Errorf("after adopt store token=%q want acc-p1", rec.AccessToken())
	}
	if rec.RefreshToken() != "rt-p1" {
		t.Errorf("after adopt store RT not stored (quarantine): %q", rec.RefreshToken())
	}

	// rollback: re-offer the now-retired acc-p0 (still made live again).
	f.addAccess("acc-p0", "p@x", "uuid-p")
	_, res = doOffer(srv, tok1, oauthBody("acc-p0", "rt-p0", srv.now()+farFuture))
	if res.Reason != "rollback" {
		t.Errorf("retired token: reason=%q want rollback", res.Reason)
	}

	// adopt into a NON-active cred (work) still works (store-wide routing).
	f.addAccess("acc-w1", "w@x", "uuid-w")
	_, res = doOffer(srv, tok1, oauthBody("acc-w1", "rt-w1", srv.now()+farFuture))
	if !res.Adopted || res.Name != "work" {
		t.Errorf("adopt into work: adopted=%v name=%q", res.Adopted, res.Name)
	}
}

func TestOfferAdoptClearsSuspectAndDead(t *testing.T) {
	for _, state := range []string{creds.HealthSuspect, creds.HealthDead} {
		f := newFake("rotating")
		f.use(t)
		f.addAccess("acc-p1", "p@x", "uuid-p")
		srv, _ := newServer(t, newStore(t))
		putCred(t, srv, "personal", "p@x", "uuid-p", "acc-old", "rt-old", srv.now()+farFuture)
		// Force the cred into the bad state.
		rec, _ := srv.store.Get("personal")
		bad := cloneRecord(rec)
		bad.HealthState = state
		bad.HealthSince = srv.now()
		_ = srv.commit(bad)
		if err := srv.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Same-cycle GET before adopt → 409.
		if w := doGet(srv, tok1, "personal", ""); w.Code != http.StatusConflict {
			t.Fatalf("[%s] GET before adopt=%d want 409", state, w.Code)
		}
		// Offer a fresh live token → adopt clears the bad state (N-2).
		_, res := doOffer(srv, tok1, oauthBody("acc-p1", "rt-p1", srv.now()+farFuture))
		if !res.Adopted {
			t.Fatalf("[%s] offer not adopted: %+v", state, res)
		}
		if got := mustHealth(t, srv, "personal"); got != creds.HealthOK {
			t.Errorf("[%s] health after adopt=%q want ok", state, got)
		}
		// Same-cycle GET after adopt → 200 (no longer 409).
		if w := doGet(srv, tok1, "personal", ""); w.Code != http.StatusOK {
			t.Errorf("[%s] GET after adopt=%d want 200", state, w.Code)
		}
	}
}

func TestOfferRateLimit(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	srv, _ := newServer(t, newStore(t))
	srv.offerRate = 2
	if err := srv.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := oauthBody("dead", "rt", srv.now()+farFuture)
	for i := 0; i < 2; i++ {
		if w, _ := doOffer(srv, tok1, body); w.Code == http.StatusTooManyRequests {
			t.Fatalf("offer %d unexpectedly rate-limited", i)
		}
	}
	w, _ := doOffer(srv, tok1, body)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("3rd offer code=%d want 429", w.Code)
	}
}

func TestOfferMigrationPending(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-p", "p@x", "uuid-p")
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "personal", "p@x", "uuid-p", "acc-p", "rt-p", srv.now()+farFuture)
	// Do NOT Migrate → offers should report migration_pending.
	_, res := doOffer(srv, tok1, oauthBody("acc-p", "rt-p", srv.now()+farFuture))
	if res.Reason != "migration_pending" {
		t.Errorf("reason=%q want migration_pending", res.Reason)
	}
}

// ---- routing (A3/A4) ----

func TestRoutingUUIDPrimaryEmailFallback(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	srv, _ := newServer(t, newStore(t))
	// "personal" exercises uuid-primary routing; "work" exercises email fallback.
	putCred(t, srv, "personal", "p@x", "uuid-p", "acc-p0", "rt-p0", srv.now()+farFuture)
	putCred(t, srv, "work", "w@x", "uuid-w", "acc-w0", "rt-w0", srv.now()+farFuture)
	if err := srv.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	// UUID matches even when the offered email differs (uuid is primary, A4).
	f.addAccess("acc-p-newmail", "changed@x", "uuid-p")
	_, res := doOffer(srv, tok1, oauthBody("acc-p-newmail", "rt-p-new", srv.now()+farFuture))
	if !res.Adopted || res.Name != "personal" {
		t.Errorf("uuid-primary routing failed: %+v", res)
	}

	// Email fallback when the profile carries no uuid.
	f.addAccess("acc-w-nouuid", "w@x", "")
	_, res = doOffer(srv, tok1, oauthBody("acc-w-nouuid", "rt-w-nouuid", srv.now()+farFuture))
	if !res.Adopted || res.Name != "work" {
		t.Errorf("email-fallback routing failed: %+v", res)
	}
}

func TestRoutingStoreWideAdoptButScopedGetForbidden(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-w0", "w@x", "uuid-w")
	f.addAccess("acc-w1", "w@x", "uuid-w")
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "personal", "p@x", "uuid-p", "acc-p0", "rt-p0", srv.now()+farFuture)
	putCred(t, srv, "work", "w@x", "uuid-w", "acc-w0", "rt-w0", srv.now()+farFuture)
	if err := srv.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	// c2's scope is ["work"] only, yet it can offer any managed account — adoption
	// is store-wide (liveness + identity + anti-rollback are the security).
	_, res := doOffer(srv, tok2, oauthBody("acc-w1", "rt-w1", srv.now()+farFuture))
	if !res.Adopted || res.Name != "work" {
		t.Fatalf("store-wide offer via scoped client failed: %+v", res)
	}
	// But c2 GETting personal (out of scope) is still 403.
	if w := doGet(srv, tok2, "personal", ""); w.Code != http.StatusForbidden {
		t.Errorf("scoped GET of personal by c2=%d want 403", w.Code)
	}
}

func TestRoutingAmbiguous(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-dup", "dup@x", "uuid-dup")
	srv, _ := newServer(t, newStore(t))
	// Two creds share an account (defensive; migration would forbid it, so we
	// bypass Migrate's uniqueness and just mark ready).
	putCred(t, srv, "one", "dup@x", "uuid-dup", "acc-1", "rt-1", srv.now()+farFuture)
	putCred(t, srv, "two", "dup@x", "uuid-dup", "acc-2", "rt-2", srv.now()+farFuture)
	srv.migrated.Store(true)
	_, res := doOffer(srv, tok1, oauthBody("acc-dup", "rt-dup", srv.now()+farFuture))
	if res.Reason != "ambiguous_account" {
		t.Errorf("reason=%q want ambiguous_account", res.Reason)
	}
}

// ---- probed GET (S2b) ----

func TestProbedGET(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-a", "a@x", "uuid-a")
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)

	// probeLive true.
	env := mustEnvelope(t, doGet(srv, tok1, "a", "probe=1"))
	if env["probeLive"] != true {
		t.Errorf("probeLive=%v want true", env["probeLive"])
	}
	// Snapshot identity: the token the probe attested is exactly the token served
	// (both come from the single captured snapshot).
	oauth := env["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != f.lastProbedToken() {
		t.Errorf("probe attested %q but body served %q (snapshot mismatch)", f.lastProbedToken(), oauth["accessToken"])
	}
	hits1 := f.hits()
	// Cache ≤30s: a second probe within TTL does not hit the endpoint again.
	_ = mustEnvelope(t, doGet(srv, tok1, "a", "probe=1"))
	if f.hits() != hits1 {
		t.Errorf("probe cache miss: hits went %d -> %d within TTL", hits1, f.hits())
	}

	// probe-endpoint-down ⇒ probeLive=false (a healthy cred still serves 200).
	f.setProfileStatus(500)
	// Advance clock past the probe TTL so the cache is bypassed.
	srv.now = func() int64 { return 1_700_000_000_000 + 40_000 }
	env = mustEnvelope(t, doGet(srv, tok1, "a", "probe=1"))
	if env["probeLive"] != false {
		t.Errorf("probe endpoint down: probeLive=%v want false", env["probeLive"])
	}
}

func TestProbeSelfHeal(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-a", "a@x", "uuid-a") // token is actually live
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)
	// Mark suspect despite the token being live.
	rec, _ := srv.store.Get("a")
	bad := cloneRecord(rec)
	bad.HealthState = creds.HealthSuspect
	bad.HealthSince = srv.now()
	_ = srv.commit(bad)

	// A plain GET 409s; a probed GET self-heals (probe 200) and serves.
	if w := doGet(srv, tok1, "a", ""); w.Code != http.StatusConflict {
		t.Fatalf("plain GET on suspect=%d want 409", w.Code)
	}
	env := mustEnvelope(t, doGet(srv, tok1, "a", "probe=1"))
	if env["probeLive"] != true {
		t.Errorf("self-heal probe should be live: %v", env["probeLive"])
	}
	if got := mustHealth(t, srv, "a"); got != creds.HealthOK {
		t.Errorf("health after self-heal=%q want ok", got)
	}
}

// ---- health classification + suspect lifecycle (S4/S7) ----

func TestHealthTransientNeverSuspect(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		f := newFake("rotating")
		f.use(t)
		f.addAccess("acc-a", "a@x", "uuid-a")
		srv, _ := newServer(t, newStore(t))
		putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)
		f.setProfileStatus(status)
		srv.pollOne(context.Background(), "a")
		if got := mustHealth(t, srv, "a"); got != creds.HealthOK {
			t.Errorf("profile %d must not affect health, got %q", status, got)
		}
	}
}

func TestSuspectFlukeVsConfirmed(t *testing.T) {
	// Single fluke 401 (confirm passes) → stays ok.
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-a", "a@x", "uuid-a")
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)
	f.setProfileSeq(401) // main probe 401; confirm re-probe falls through to live 200
	srv.pollOne(context.Background(), "a")
	if got := mustHealth(t, srv, "a"); got != creds.HealthOK {
		t.Errorf("single fluke bricked cred: health=%q want ok", got)
	}

	// Confirmed double-401 → suspect → GET 409.
	f.setProfileSeq(401, 401)
	srv.pollOne(context.Background(), "a")
	if got := mustHealth(t, srv, "a"); got != creds.HealthSuspect {
		t.Fatalf("double-401 health=%q want suspect", got)
	}
	if w := doGet(srv, tok1, "a", ""); w.Code != http.StatusConflict {
		t.Errorf("suspect GET=%d want 409", w.Code)
	}
}

// TestSuspectStaleEvidenceDropped is the MAJOR-3 regression: if an adopt lands
// during the confirm-re-probe window, the (now stale) suspect transition is
// dropped — the cred was proven live by the offer seconds ago.
func TestSuspectStaleEvidenceDropped(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	// "acc-dead" is NOT live → both the main probe and the confirm re-probe 401.
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-dead", "rt-a", srv.now()+farFuture)

	var once sync.Once
	f.setOnProfileCall(func(n int) {
		if n == 2 { // the confirm re-probe — simulate an adopt landing mid-window
			once.Do(func() {
				rec, _ := srv.store.Get("a")
				nr := cloneRecord(rec)
				nr.OAuth["accessToken"] = "acc-fresh"
				nr.HealthState = creds.HealthOK
				_ = srv.commit(nr) // bumps gen; evidence for the 401 is now stale
			})
		}
	})
	srv.pollOne(context.Background(), "a")
	if got := mustHealth(t, srv, "a"); got != creds.HealthOK {
		t.Errorf("stale-evidence suspect not dropped: health=%q want ok", got)
	}
}

func TestReclaimSuccessAndDead(t *testing.T) {
	// Suspect with a still-valid RT → reclaim refreshes → ok (stable-RT case).
	f := newFake("stable")
	f.use(t)
	f.addRT("rt-a", "a@x", "uuid-a") // RT valid, but access token is dead (not live)
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-dead", "rt-a", srv.now()+farFuture)
	suspectNow(t, srv, "a")
	srv.pollOne(context.Background(), "a") // past reclaimDelay (HealthSince backdated)
	if got := mustHealth(t, srv, "a"); got != creds.HealthOK {
		t.Errorf("reclaim should recover: health=%q want ok", got)
	}

	// Suspect with an invalid RT → reclaim invalid_grant → dead.
	f2 := newFake("rotating")
	f2.use(t)
	srv2, _ := newServer(t, newStore(t))
	putCred(t, srv2, "b", "b@x", "uuid-b", "acc-b", "rt-invalid", srv2.now()+farFuture)
	suspectNow(t, srv2, "b")
	srv2.pollOne(context.Background(), "b")
	if got := mustHealth(t, srv2, "b"); got != creds.HealthDead {
		t.Errorf("reclaim invalid_grant should be dead: health=%q", got)
	}
}

func TestDeadSelfHeals(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-a", "a@x", "uuid-a") // token now live again
	srv, _ := newServer(t, newStore(t))
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a", "rt-a", srv.now()+farFuture)
	// Mark dead with a backdated HealthSince so the 30-min re-probe is due.
	rec, _ := srv.store.Get("a")
	dead := cloneRecord(rec)
	dead.HealthState = creds.HealthDead
	dead.HealthSince = srv.now() - 10_000_000
	_ = srv.commit(dead)
	srv.pollOne(context.Background(), "a")
	if got := mustHealth(t, srv, "a"); got != creds.HealthOK {
		t.Errorf("dead cred should self-heal on 200 probe: health=%q", got)
	}
}

// suspectNow marks a cred suspect with a backdated HealthSince so a reclaim is
// due on the next poll.
func suspectNow(t *testing.T, srv *Server, name string) {
	t.Helper()
	rec, _ := srv.store.Get(name)
	bad := cloneRecord(rec)
	bad.HealthState = creds.HealthSuspect
	bad.HealthSince = srv.now() - 10_000_000
	if err := srv.commit(bad); err != nil {
		t.Fatal(err)
	}
}

func mustHealth(t *testing.T, srv *Server, name string) string {
	t.Helper()
	rec, ok := srv.store.Get(name)
	if !ok {
		t.Fatalf("no cred %q", name)
	}
	return rec.Health()
}

// ---- import (S6) ----

func TestImportVerifiesAndUniqueness(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-a", "a@x", "uuid-a")
	srv, _ := newServer(t, newStore(t))

	// Happy import: profile 200 → account recorded.
	if w := doPut(srv, "personal", oauthBody("acc-a", "rt-a", srv.now()+farFuture)); w.Code != http.StatusOK {
		t.Fatalf("import code=%d body=%s", w.Code, w.Body.String())
	}
	rec, _ := srv.store.Get("personal")
	if rec.Account != "a@x" || rec.AccountUUID != "uuid-a" {
		t.Errorf("import did not record account: %+v", rec)
	}
	if rec.Gen == 0 {
		t.Errorf("import did not initialize gen")
	}

	// Uniqueness: importing the same account under a different name → 409.
	f.addAccess("acc-a2", "a@x", "uuid-a")
	if w := doPut(srv, "dupe", oauthBody("acc-a2", "rt-a2", srv.now()+farFuture)); w.Code != http.StatusConflict {
		t.Errorf("duplicate-account import code=%d want 409", w.Code)
	}

	// Reject when profile is unavailable (N-3).
	f.setProfileStatus(500)
	if w := doPut(srv, "other", oauthBody("acc-a", "rt-a", srv.now()+farFuture)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("import with profile down code=%d want 503", w.Code)
	}
}

// TestImportRetiresReplacedHash is the MINOR-3 regression: re-importing a cred
// retires the replaced access-token hash, so an old copy can no longer be
// re-offered as fresh.
func TestImportRetiresReplacedHash(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-1", "a@x", "uuid-a")
	f.addAccess("acc-2", "a@x", "uuid-a")
	srv, _ := newServer(t, newStore(t))
	if w := doPut(srv, "a", oauthBody("acc-1", "rt-1", srv.now()+farFuture)); w.Code != http.StatusOK {
		t.Fatalf("import 1: %d", w.Code)
	}
	if w := doPut(srv, "a", oauthBody("acc-2", "rt-2", srv.now()+farFuture)); w.Code != http.StatusOK {
		t.Fatalf("import 2 (replace): %d %s", w.Code, w.Body.String())
	}
	srv.migrated.Store(true)
	// Re-offer the retired acc-1 (still live) → rollback, not adopt.
	_, res := doOffer(srv, tok1, oauthBody("acc-1", "rt-1", srv.now()+farFuture))
	if res.Reason != "rollback" {
		t.Errorf("re-offer of import-replaced token: reason=%q want rollback", res.Reason)
	}
}

// ---- migration (S8) ----

func TestMigratePopulatesAccounts(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addAccess("acc-legacy", "legacy@x", "uuid-legacy")
	st := newStore(t)
	// A pre-v0.4 record: no Account/AccountUUID, no gen, legacy Dead=false.
	_ = st.Put(&creds.Record{Name: "legacy", OAuth: map[string]any{
		"accessToken": "acc-legacy", "refreshToken": "rt-legacy", "expiresAt": float64(1_700_000_000_000 + farFuture),
	}})
	srv, _ := newServer(t, st)
	if err := srv.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := srv.store.Get("legacy")
	if rec.Account != "legacy@x" || rec.AccountUUID != "uuid-legacy" {
		t.Errorf("migration did not populate identity: %+v", rec)
	}
	if rec.Gen == 0 {
		t.Errorf("migration did not assign gen")
	}
	if !srv.migrated.Load() {
		t.Errorf("migration did not mark ready")
	}
}

func TestMigrateDeadTokenStaysUnroutable(t *testing.T) {
	f := newFake("rotating")
	f.use(t) // acc-dead is NOT registered live → profile 401
	st := newStore(t)
	_ = st.Put(&creds.Record{Name: "legacy", OAuth: map[string]any{
		"accessToken": "acc-dead", "refreshToken": "rt-dead", "expiresAt": float64(1_700_000_000_000 + farFuture),
	}})
	srv, _ := newServer(t, st)
	if err := srv.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := srv.store.Get("legacy")
	if rec.Account != "" || rec.AccountUUID != "" {
		t.Errorf("dead-token record should stay unroutable, got %+v", rec)
	}
	if !srv.migrated.Load() {
		t.Errorf("migration should still complete for the rest of the store")
	}
}

func TestMigrateDuplicateRefusesStart(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	st := newStore(t)
	_ = st.Put(&creds.Record{Name: "a", Account: "dup@x", AccountUUID: "uuid-dup", Gen: 1, HealthState: creds.HealthOK,
		OAuth: map[string]any{"accessToken": "acc-a", "refreshToken": "rt-a", "expiresAt": float64(1)}})
	_ = st.Put(&creds.Record{Name: "b", Account: "dup@x", AccountUUID: "uuid-dup", Gen: 1, HealthState: creds.HealthOK,
		OAuth: map[string]any{"accessToken": "acc-b", "refreshToken": "rt-b", "expiresAt": float64(1)}})
	srv, _ := newServer(t, st)
	if err := srv.Migrate(context.Background()); err == nil {
		t.Fatal("expected Migrate to refuse start on duplicate account")
	}
}

// ---- concurrency (S2/M-4 under -race) ----

func TestConcurrentOfferAndRefresh(t *testing.T) {
	f := newFake("rotating")
	f.use(t)
	f.addRT("rt-a", "a@x", "uuid-a")
	f.addAccess("acc-a0", "a@x", "uuid-a")
	for i := 1; i <= 12; i++ {
		f.addAccess("live-"+itoa(i), "a@x", "uuid-a")
		f.addRT("rt-off-"+itoa(i), "a@x", "uuid-a") // adopted RTs stay valid for a later refresh
	}
	srv, _ := newServer(t, newStore(t))
	// Expiry just inside the skew so the refresh loop path is exercised too.
	putCred(t, srv, "a", "a@x", "uuid-a", "acc-a0", "rt-a", srv.now()+1000)
	if err := srv.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 1; i <= 12; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			doOffer(srv, tok1, oauthBody("live-"+itoa(i), "rt-off-"+itoa(i), srv.now()+farFuture))
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := srv.credMutex("a")
			m.Lock()
			cur, _ := srv.store.Get("a")
			if srv.needsRefresh(cur) {
				_, _ = srv.doRefresh(context.Background(), cur)
			}
			m.Unlock()
		}()
	}
	wg.Wait()
	// Store must remain consistent and healthy.
	if got := mustHealth(t, srv, "a"); got != creds.HealthOK {
		t.Errorf("after concurrency health=%q want ok", got)
	}
	if rec, _ := srv.store.Get("a"); rec.RefreshToken() == "" {
		t.Errorf("cred lost its refresh token under concurrency")
	}
}
