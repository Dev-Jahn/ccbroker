// Package server implements the broker: a background refresh manager, a health
// poller with a suspect/dead lifecycle, an account-routed offer/adopt endpoint,
// and a credential API (bearer + scope) plus a localhost-only admin API.
//
// Design invariants (v0.4, see v040-design.md):
//   - I1 quarantine: refresh tokens live only in the store; serving strips them
//     unless serveRefreshToken (rollout escape hatch).
//   - I2 single writer: only the broker calls the refresh endpoint; a fresh
//     /login enters via offer, never via replay.
//   - I3 liveness beats timestamps: adopt/overwrite decisions use broker-side
//     liveness verification, never cross-clock expiresAt comparison.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dev-Jahn/ccbroker/internal/anthropic"
	"github.com/Dev-Jahn/ccbroker/internal/config"
	"github.com/Dev-Jahn/ccbroker/internal/creds"
	"github.com/Dev-Jahn/ccbroker/internal/store"
)

const transientBackoff = 60 * time.Second

// retiredRing is the anti-rollback ring depth (design S2d/N-8).
const retiredRing = 8

type Server struct {
	cfg   *config.Server
	store *store.Store
	audit *log.Logger
	skew  time.Duration

	// now returns the current time in unix ms. Injectable so tests can step the
	// clock backward and prove gen stays monotonic (C-2).
	now func() int64

	mu        sync.Mutex
	inflight  map[string]*sync.Mutex // per-cred single-flight refresh/adopt
	backoff   map[string]time.Time   // per-cred next-allowed refresh after a transient failure
	deadProbe map[string]int64       // per-cred last dead re-probe (unix ms, in-memory; MINOR-4)

	// storeVer is a store-wide mutation counter bumped by every commit, so an
	// offer can detect ANY change (not just to the matched cred) between its
	// out-of-mutex verify (b) and the mutex re-check (S2e "since (b)"; MINOR-1).
	storeVer atomic.Int64

	genMu    sync.Mutex // the single mutex guarding the wake map (S3/M-4)
	genState map[string]*credState

	usageMu sync.Mutex
	usage   map[string]*usageEntry // per-cred latest quota snapshot

	probeMu sync.Mutex
	probe   map[string]*probeEntry // per-cred ≤probeTTL liveness probe cache (S2b)

	rlMu sync.Mutex
	rate map[string]*rateEntry // per-client offer rate limit (S2a)

	migrated atomic.Bool // S8: offers return migration_pending until set

	// Tunables (fields so tests can shrink them).
	confirmDelay      time.Duration // S4 confirm re-probe gap (default 10s)
	reclaimDelay      time.Duration // S4 last-resort reclaim delay (default 10min)
	deadProbeInterval time.Duration // S4 dead re-probe cadence (default 30min)
	probeTTL          time.Duration // S2b probe cache (default 30s)
	offerRate         int           // S2a offers per minute per client (default 6)
}

// credState is the wake state for one credential's long-poll (S3).
type credState struct {
	gen  int64
	wake chan struct{}
}

// probeEntry caches a liveness probe for one credential (S2b).
type probeEntry struct {
	live   bool
	at     int64 // unix ms
	forGen int64 // the gen this probe attests
}

// rateEntry is one client's fixed-window offer counter (S2a).
type rateEntry struct {
	count       int
	windowStart int64 // unix ms
}

type usageEntry struct {
	Usage        *anthropic.Usage
	OAuthAccount map[string]any // Claude Code .claude.json "oauthAccount" shape
	FetchedAt    int64          // unix ms
	LastError    string
}

// New constructs a Server and opens the audit log.
func New(cfg *config.Server, st *store.Store) (*Server, error) {
	var w io.Writer = os.Stdout
	if cfg.AuditLog != "" {
		f, err := os.OpenFile(cfg.AuditLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		w = f
	}
	s := &Server{
		cfg:               cfg,
		store:             st,
		audit:             log.New(w, "", log.LstdFlags|log.LUTC),
		skew:              time.Duration(cfg.RefreshSkewSec) * time.Second,
		now:               func() int64 { return time.Now().UnixMilli() },
		inflight:          map[string]*sync.Mutex{},
		backoff:           map[string]time.Time{},
		deadProbe:         map[string]int64{},
		genState:          map[string]*credState{},
		usage:             map[string]*usageEntry{},
		probe:             map[string]*probeEntry{},
		rate:              map[string]*rateEntry{},
		confirmDelay:      10 * time.Second,
		reclaimDelay:      10 * time.Minute,
		deadProbeInterval: 30 * time.Minute,
		probeTTL:          30 * time.Second,
		offerRate:         6,
	}
	// Seed the wake map from the persisted per-cred gen (N-5) so a long-poll can
	// resume with a pre-restart sinceGen.
	for _, rec := range st.List() {
		s.genState[rec.Name] = &credState{gen: rec.Gen, wake: make(chan struct{})}
	}
	return s, nil
}

func (s *Server) credMutex(name string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.inflight[name]
	if !ok {
		m = &sync.Mutex{}
		s.inflight[name] = m
	}
	return m
}

func (s *Server) needsRefresh(r *creds.Record) bool {
	return r.ExpiresAtMs()-s.now() <= s.skew.Milliseconds()
}

// ---- generation counter + long-poll (S3) ----

// nextGen computes the next generation from the stored value: strictly greater
// than both the stored gen and now, so it is monotonic under clock steps and
// distinct under sub-ms collisions.
func (s *Server) nextGen(storedGen int64) int64 {
	g := storedGen + 1
	if now := s.now(); now > g {
		g = now
	}
	return g
}

// commit is the single mutation primitive (S5/N-5): it assigns nr a freshly
// bumped gen, persists it atomically, then updates the wake map under the one
// genMu and closes+replaces the wake channel — no missed-wakeup window (M-4).
func (s *Server) commit(nr *creds.Record) error {
	stored, _ := s.store.Get(nr.Name)
	var prev int64
	if stored != nil {
		prev = stored.Gen
	}
	nr.Gen = s.nextGen(prev)
	nr.UpdatedAt = s.now()
	if err := s.store.Put(nr); err != nil {
		return err
	}
	s.storeVer.Add(1)
	s.genMu.Lock()
	st := s.genState[nr.Name]
	if st == nil {
		st = &credState{wake: make(chan struct{})}
		s.genState[nr.Name] = st
	}
	st.gen = nr.Gen
	close(st.wake)
	st.wake = make(chan struct{})
	s.genMu.Unlock()
	return nil
}

// seedGen sets the wake-map gen for a record without waking (startup only).
func (s *Server) seedGen(rec *creds.Record) {
	s.genMu.Lock()
	st := s.genState[rec.Name]
	if st == nil {
		st = &credState{wake: make(chan struct{})}
		s.genState[rec.Name] = st
	}
	st.gen = rec.Gen
	s.genMu.Unlock()
}

// waitForGen returns the current gen for name and, if it is not already newer
// than sinceGen, blocks until a wake, the deadline, or ctx cancel. The wake
// channel is captured before waiting, so a bump between the capture and the
// select is not missed (M-4b).
func (s *Server) waitForGen(ctx context.Context, name string, sinceGen int64, wait time.Duration) (int64, bool) {
	s.genMu.Lock()
	st := s.genState[name]
	if st == nil {
		st = &credState{wake: make(chan struct{})}
		s.genState[name] = st
	}
	cur := st.gen
	ch := st.wake
	s.genMu.Unlock()
	if cur > sinceGen {
		return cur, true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch:
		s.genMu.Lock()
		cur = st.gen
		s.genMu.Unlock()
		return cur, true
	case <-timer.C:
		return cur, false
	case <-ctx.Done():
		return cur, false
	}
}

// ---- refresh (S5) ----

// ensureFresh refreshes the named credential if it is healthy and within the
// skew window. Safe under concurrency (single-flight per credential). It returns
// the most current record even if a transient refresh failed. Suspect/dead creds
// are never refreshed on read (their RT must not be replayed — F2).
func (s *Server) ensureFresh(ctx context.Context, name string) (*creds.Record, error) {
	rec, ok := s.store.Get(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	if rec.Health() != creds.HealthOK || !s.needsRefresh(rec) {
		return rec, nil
	}
	m := s.credMutex(name)
	m.Lock()
	defer m.Unlock()

	rec, ok = s.store.Get(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	if rec.Health() != creds.HealthOK || !s.needsRefresh(rec) {
		return rec, nil
	}
	s.mu.Lock()
	nextOK := s.backoff[name]
	s.mu.Unlock()
	if time.Now().Before(nextOK) {
		return rec, errors.New("refresh in transient backoff")
	}
	return s.doRefresh(ctx, rec)
}

// applyRefresh builds the post-refresh record from res, retiring the previous
// access-hash into the anti-rollback ring (S5). Caller commits.
func (s *Server) applyRefresh(rec *creds.Record, res *anthropic.Result) *creds.Record {
	oauth := cloneOAuth(rec.OAuth)
	oldHash := sha256hex(rec.AccessToken())
	oauth["accessToken"] = res.AccessToken
	if res.RefreshToken != "" {
		oauth["refreshToken"] = res.RefreshToken
	}
	oauth["expiresAt"] = float64(s.now() + res.ExpiresIn*1000)
	if res.Scope != "" {
		oauth["scopes"] = toAnySlice(strings.Fields(res.Scope))
	}
	nr := cloneRecord(rec)
	nr.OAuth = oauth
	nr.Account = firstNonEmpty(res.Account.EmailAddress, rec.Account)
	if res.Account.UUID != "" {
		nr.AccountUUID = res.Account.UUID
	}
	nr.HealthState = creds.HealthOK
	nr.HealthSince = s.now()
	nr.LastError = ""
	nr.RetiredHashes = retireHash(rec.RetiredHashes, oldHash)
	return nr
}

// doRefresh performs the actual refresh. Caller must hold the per-cred mutex.
func (s *Server) doRefresh(ctx context.Context, rec *creds.Record) (*creds.Record, error) {
	res, err := anthropic.Refresh(ctx, rec.RefreshToken())
	if err != nil {
		var ae *anthropic.Err
		if errors.As(err, &ae) && ae.Permanent {
			dead := cloneRecord(rec)
			dead.HealthState = creds.HealthDead
			dead.HealthSince = s.now()
			dead.LastError = ae.Error()
			_ = s.commit(dead)
			s.audit.Printf("REFRESH name=%s result=DEAD err=%q", rec.Name, ae.Error())
			return dead, err
		}
		s.mu.Lock()
		s.backoff[rec.Name] = time.Now().Add(transientBackoff)
		s.mu.Unlock()
		s.audit.Printf("REFRESH name=%s result=TRANSIENT err=%v", rec.Name, err)
		return rec, err
	}
	nr := s.applyRefresh(rec, res)
	if err := s.commit(nr); err != nil {
		return rec, err
	}
	s.mu.Lock()
	delete(s.backoff, rec.Name)
	s.mu.Unlock()
	s.audit.Printf("REFRESH name=%s result=OK expires_in=%ds account=%s", rec.Name, res.ExpiresIn, nr.Account)
	return nr, nil
}

// RunRefreshLoop proactively refreshes credentials nearing expiry.
func (s *Server) RunRefreshLoop(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	s.refreshDue(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshDue(ctx)
		}
	}
}

func (s *Server) refreshDue(ctx context.Context) {
	for _, rec := range s.store.List() {
		// suspect/dead creds are skipped: the scheduled loop must not replay a
		// possibly-superseded refresh token either (S4).
		if rec.Health() != creds.HealthOK || !s.needsRefresh(rec) {
			continue
		}
		m := s.credMutex(rec.Name)
		if !m.TryLock() {
			continue // a request is already refreshing this cred
		}
		func() {
			defer m.Unlock()
			cur, ok := s.store.Get(rec.Name)
			if !ok || cur.Health() != creds.HealthOK || !s.needsRefresh(cur) {
				return
			}
			s.mu.Lock()
			nextOK := s.backoff[rec.Name]
			s.mu.Unlock()
			if time.Now().Before(nextOK) {
				return
			}
			cctx, cancel := context.WithTimeout(ctx, 35*time.Second)
			defer cancel()
			_, _ = s.doRefresh(cctx, cur)
		}()
	}
}

// ---- health poller + suspect/dead lifecycle (S4/S7) ----

// RunUsageLoop polls the usage+profile endpoints for every live credential
// (quota snapshots for clients, plus the health lifecycle). Polling reads
// utilization only; it never consumes message quota.
func (s *Server) RunUsageLoop(ctx context.Context) {
	iv := time.Duration(s.cfg.UsagePollSec) * time.Second
	t := time.NewTicker(iv)
	defer t.Stop()
	s.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.poll(ctx)
		}
	}
}

func (s *Server) poll(ctx context.Context) {
	for _, rec := range s.store.List() {
		s.pollOne(ctx, rec.Name)
	}
}

func (s *Server) pollOne(ctx context.Context, name string) {
	rec, ok := s.store.Get(name)
	if !ok {
		return
	}
	now := s.now()
	health := rec.Health()
	genAtProbe := rec.Gen // the record this poll's evidence pertains to (MAJOR-3)

	if health == creds.HealthDead {
		// dead is NOT terminal (M-3): re-probe every deadProbeInterval. The probe
		// cadence is tracked in memory so a FAILED probe neither commits nor wakes
		// long-pollers, and re-probes stay 30-min-spaced instead of every poll
		// (MINOR-4). A 200 clears dead (fluke self-heal).
		s.mu.Lock()
		last := s.deadProbe[name]
		s.mu.Unlock()
		if now-last < s.deadProbeInterval.Milliseconds() {
			return
		}
		s.mu.Lock()
		s.deadProbe[name] = now
		s.mu.Unlock()
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, perr := anthropic.FetchProfile(cctx, rec.AccessToken(), now)
		cancel()
		if perr == nil {
			s.transitionHealth(name, creds.HealthOK, "RECOVER", "dead-probe-200", genAtProbe)
		}
		return
	}

	// ok or suspect: usage snapshot + profile probe (identity + health).
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	u, uerr := anthropic.FetchUsage(cctx, rec.AccessToken())
	acct, perr := anthropic.FetchProfile(cctx, rec.AccessToken(), now)
	cancel()
	s.recordUsage(name, u, uerr, acct, perr)

	unexpired := rec.ExpiresAtMs() > now
	switch {
	case perr != nil && anthropic.AuthFailure(perr) && unexpired:
		// A single fluke 401 must not brick: only a CONFIRMED double-401 on an
		// unexpired token becomes suspect (S4). The health!=suspect guard is
		// evaluated FIRST so an already-suspect cred skips the confirm stall
		// (MINOR-10). transitionHealth's expectGen guard drops the transition if
		// the record was adopted/refreshed during the confirm window — the
		// evidence is then stale (MAJOR-3).
		if health != creds.HealthSuspect && s.confirmAuthFailure(ctx, rec.AccessToken()) {
			s.transitionHealth(name, creds.HealthSuspect, "SUSPECT", "double-401", genAtProbe)
		}
	case perr == nil && health == creds.HealthSuspect:
		// Recovered on its own (external re-auth) → ok.
		s.transitionHealth(name, creds.HealthOK, "RECOVER", "profile-200", genAtProbe)
	}

	// suspect reclaim: RECLAIM_DELAY passed with no offer → one last-resort
	// refresh attempt (covers the stable-RT/legacy-landmine case).
	if cur, ok := s.store.Get(name); ok && cur.Health() == creds.HealthSuspect &&
		now-cur.HealthSince >= s.reclaimDelay.Milliseconds() {
		s.reclaim(ctx, name)
	}
}

// confirmAuthFailure re-probes ≥confirmDelay after the first auth failure and
// reports whether the failure is confirmed (design: ≥10s later).
func (s *Server) confirmAuthFailure(ctx context.Context, token string) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(s.confirmDelay):
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, perr := anthropic.FetchProfile(cctx, token, s.now())
	cancel()
	return anthropic.AuthFailure(perr)
}

// reclaim is the single last-resort refresh for a suspect cred (S4). Caller must
// NOT hold the cred mutex.
func (s *Server) reclaim(ctx context.Context, name string) {
	m := s.credMutex(name)
	m.Lock()
	defer m.Unlock()
	rec, ok := s.store.Get(name)
	if !ok || rec.Health() != creds.HealthSuspect {
		return // an offer may have already adopted → ok
	}
	res, err := anthropic.Refresh(ctx, rec.RefreshToken())
	if err != nil {
		var ae *anthropic.Err
		if errors.As(err, &ae) && ae.Permanent {
			dead := cloneRecord(rec)
			dead.HealthState = creds.HealthDead
			dead.HealthSince = s.now()
			dead.LastError = ae.Error()
			_ = s.commit(dead)
			s.audit.Printf("DEAD_EARLY name=%s err=%q", name, ae.Error())
			return
		}
		s.audit.Printf("RECLAIM name=%s result=TRANSIENT err=%v", name, err)
		return
	}
	nr := s.applyRefresh(rec, res)
	_ = s.commit(nr)
	s.audit.Printf("RECLAIM name=%s result=OK account=%s", name, nr.Account)
}

// transitionHealth persists a health-state change (via commit, so GETs and
// long-pollers observe it) and audits it. A no-op if already in state. expectGen
// is the gen of the record the transition's evidence pertains to: if the record
// has since moved on (adopt/refresh/reclaim landed), the evidence is stale and
// the transition is dropped under the mutex (MAJOR-3). Caller must NOT hold the
// cred mutex.
func (s *Server) transitionHealth(name, state, verb, reason string, expectGen int64) bool {
	m := s.credMutex(name)
	m.Lock()
	defer m.Unlock()
	rec, ok := s.store.Get(name)
	if !ok || rec.Health() == state {
		return false
	}
	if rec.Gen != expectGen {
		s.audit.Printf("%s name=%s reason=%q result=STALE gen=%d!=%d", verb, name, reason, rec.Gen, expectGen)
		return false
	}
	nr := cloneRecord(rec)
	nr.HealthState = state
	nr.HealthSince = s.now()
	if state == creds.HealthOK {
		nr.LastError = ""
	}
	if err := s.commit(nr); err != nil {
		return false
	}
	s.audit.Printf("%s name=%s reason=%q", verb, name, reason)
	return true
}

func (s *Server) recordUsage(name string, u *anthropic.Usage, uerr error, acct map[string]any, perr error) {
	s.usageMu.Lock()
	e := s.usage[name]
	if e == nil {
		e = &usageEntry{}
		s.usage[name] = e
	}
	if uerr != nil {
		e.LastError = uerr.Error()
	} else {
		e.Usage = u
		e.FetchedAt = s.now()
		e.LastError = ""
	}
	if perr == nil {
		e.OAuthAccount = acct
	}
	s.usageMu.Unlock()
	if uerr != nil {
		s.audit.Printf("USAGE name=%s result=ERR err=%v", name, uerr)
	}
	if perr != nil {
		s.audit.Printf("PROFILE name=%s result=ERR err=%v", name, perr)
	}
}

// probeCred freshly verifies a record's stored access token via profile,
// caching the result ≤probeTTL per (name, gen) to bound load (S2b). Probes
// never run under the cred mutex.
func (s *Server) probeCred(ctx context.Context, rec *creds.Record) bool {
	now := s.now()
	s.probeMu.Lock()
	if e := s.probe[rec.Name]; e != nil && e.forGen == rec.Gen && now-e.at < s.probeTTL.Milliseconds() {
		live := e.live
		s.probeMu.Unlock()
		return live
	}
	s.probeMu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, perr := anthropic.FetchProfile(cctx, rec.AccessToken(), now)
	cancel()
	live := perr == nil // probe-endpoint-down ⇒ live=false

	s.probeMu.Lock()
	s.probe[rec.Name] = &probeEntry{live: live, at: now, forGen: rec.Gen}
	s.probeMu.Unlock()
	return live
}

// ---- startup migration (S8) ----

// Migrate populates Account/AccountUUID and gen for inherited v0.3 records,
// asserts account uniqueness, and marks offers ready. It must run before the
// first offer is served; on a duplicate account it refuses to start (returns an
// error) naming both records.
func (s *Server) Migrate(ctx context.Context) error {
	for _, rec := range s.store.List() {
		nr := cloneRecord(rec)
		changed := false
		if nr.HealthState == "" { // normalize legacy Dead → HealthState
			nr.HealthState = rec.Health()
			nr.HealthSince = s.now()
			changed = true
		}
		if nr.Gen == 0 {
			nr.Gen = s.nextGen(0)
			changed = true
		}
		if nr.Account == "" || nr.AccountUUID == "" {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			acct, perr := anthropic.FetchProfile(cctx, nr.AccessToken(), s.now())
			cancel()
			if perr != nil {
				// Unroutable for offers until re-imported (routing needs identity).
				s.audit.Printf("MIGRATE name=%s result=UNROUTABLE err=%v", nr.Name, perr)
			} else {
				nr.Account = firstNonEmpty(mstr(acct, "emailAddress"), nr.Account)
				nr.AccountUUID = firstNonEmpty(mstr(acct, "accountUuid"), nr.AccountUUID)
				changed = true
				s.audit.Printf("MIGRATE name=%s account=%s uuid=%s", nr.Name, nr.Account, nr.AccountUUID)
			}
		}
		if changed {
			nr.UpdatedAt = s.now()
			if err := s.store.Put(nr); err != nil {
				return err
			}
			s.seedGen(nr)
		}
	}
	if err := s.assertUnique(); err != nil {
		return err
	}
	s.migrated.Store(true)
	return nil
}

// assertUnique refuses duplicate Account (email) or AccountUUID across records.
func (s *Server) assertUnique() error {
	byEmail := map[string]string{}
	byUUID := map[string]string{}
	for _, rec := range s.store.List() {
		if rec.Account != "" {
			if other, ok := byEmail[rec.Account]; ok {
				return fmt.Errorf("duplicate account email %q on creds %q and %q", rec.Account, other, rec.Name)
			}
			byEmail[rec.Account] = rec.Name
		}
		if rec.AccountUUID != "" {
			if other, ok := byUUID[rec.AccountUUID]; ok {
				return fmt.Errorf("duplicate account uuid %q on creds %q and %q", rec.AccountUUID, other, rec.Name)
			}
			byUUID[rec.AccountUUID] = rec.Name
		}
	}
	return nil
}

// routeByAccount routes an offered account to exactly one cred: by uuid first
// (primary, A4), then email (fallback). "" reason means match found.
func (s *Server) routeByAccount(uuid, email string) (*creds.Record, string) {
	var uuidMatches, emailMatches []*creds.Record
	for _, rec := range s.store.List() {
		if uuid != "" && rec.AccountUUID == uuid {
			uuidMatches = append(uuidMatches, rec)
		}
		if email != "" && rec.Account == email {
			emailMatches = append(emailMatches, rec)
		}
	}
	if len(uuidMatches) == 1 {
		return uuidMatches[0], ""
	}
	if len(uuidMatches) > 1 {
		return nil, "ambiguous_account"
	}
	if len(emailMatches) == 1 {
		return emailMatches[0], ""
	}
	if len(emailMatches) > 1 {
		return nil, "ambiguous_account"
	}
	return nil, "unknown_account"
}

// rateLimitOffer applies a fixed-window per-client offer limit (S2a).
func (s *Server) rateLimitOffer(client string) bool {
	now := s.now()
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	e := s.rate[client]
	if e == nil || now-e.windowStart >= 60_000 {
		s.rate[client] = &rateEntry{count: 1, windowStart: now}
		return true
	}
	if e.count >= s.offerRate {
		return false
	}
	e.count++
	return true
}

// ---- credential API (bearer + scope) ----

func (s *Server) authClient(r *http.Request) (*config.Client, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, p) {
		return nil, false
	}
	tok := strings.TrimSpace(h[len(p):])
	if tok == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(tok))
	got := hex.EncodeToString(sum[:])
	for i := range s.cfg.Clients {
		c := &s.cfg.Clients[i]
		want := strings.ToLower(strings.TrimSpace(c.TokenSHA256))
		if len(want) == len(got) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return c, true
		}
	}
	return nil, false
}

func scopeAllows(c *config.Client, name string) bool {
	for _, sc := range c.Scopes {
		if sc == "*" || sc == name {
			return true
		}
	}
	return false
}

func (s *Server) handleGetCred(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v1/credentials/")
	ip := clientIP(r)
	client, ok := s.authClient(r)
	if !ok {
		s.audit.Printf("GET name=%s client=? ip=%s result=UNAUTHORIZED", name, ip)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if name == "" || strings.ContainsAny(name, "/?") {
		http.Error(w, "bad credential name", http.StatusBadRequest)
		return
	}
	if !scopeAllows(client, name) {
		s.audit.Printf("GET name=%s client=%s ip=%s result=FORBIDDEN", name, client.Name, ip)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	q := r.URL.Query()
	probe := q.Get("probe") == "1"
	sinceGen, _ := strconv.ParseInt(q.Get("sinceGen"), 10, 64)
	waitSec, _ := strconv.ParseInt(q.Get("waitSec"), 10, 64)
	if waitSec > 25 { // clamp (m-2)
		waitSec = 25
	}

	// Long-poll: block until a newer gen or the deadline (→ 304).
	if q.Has("sinceGen") || waitSec > 0 {
		if _, ok := s.store.Get(name); !ok {
			http.Error(w, "no such credential", http.StatusNotFound)
			return
		}
		if gen, changed := s.waitForGen(r.Context(), name, sinceGen, time.Duration(waitSec)*time.Second); !changed {
			w.Header().Set("X-Ccb-Gen", strconv.FormatInt(gen, 10))
			w.WriteHeader(http.StatusNotModified)
			s.audit.Printf("GET name=%s client=%s ip=%s result=NOT_MODIFIED sinceGen=%d", name, client.Name, ip, sinceGen)
			return
		}
	}

	rec, ferr := s.ensureFresh(r.Context(), name)
	if rec == nil {
		if errors.Is(ferr, os.ErrNotExist) {
			http.Error(w, "no such credential", http.StatusNotFound)
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}

	// Capture ONE snapshot for both the probe and the response body, so probeLive
	// attests exactly the token served (S2b). The gen is advertised on every
	// response (incl. 304/409/503) so a long-polling client can advance past a
	// suspect-state change without a hot loop.
	snap := rec
	w.Header().Set("X-Ccb-Gen", strconv.FormatInt(snap.Gen, 10))
	health := snap.Health()
	probeLive := false
	if probe {
		probeLive = s.probeCred(r.Context(), snap)
		// Probe-first self-heal: a 200 on a suspect/dead cred clears it and serves
		// normally (the token was just verified live). Guarded by snap.Gen so a
		// stale probe (the record moved on) does not resurrect it (MAJOR-3).
		if probeLive && health != creds.HealthOK {
			s.transitionHealth(name, creds.HealthOK, "RECOVER", "probe-self-heal", snap.Gen)
			health = creds.HealthOK
		}
	}

	if health != creds.HealthOK {
		s.audit.Printf("GET name=%s client=%s ip=%s result=UNHEALTHY state=%s", name, client.Name, ip, health)
		http.Error(w, "credential unhealthy (needs re-auth)", http.StatusConflict)
		return
	}

	// Never hand out an already-expired token.
	if snap.ExpiresAtMs() <= s.now() {
		s.audit.Printf("GET name=%s client=%s ip=%s result=EXPIRED_NO_REFRESH", name, client.Name, ip)
		http.Error(w, "token expired and refresh unavailable", http.StatusServiceUnavailable)
		return
	}

	env := map[string]any{
		"claudeAiOauth": snap.ServedOAuth(s.cfg.ServeRefreshToken),
		"gen":           snap.Gen,
		"account":       snap.Account,
	}
	if probe {
		env["probeLive"] = probeLive
	}
	// Bound the write after a long-poll wake without a server-wide WriteTimeout
	// that would kill idle long-polls (m-4).
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
	writeJSON(w, http.StatusOK, env)
	s.audit.Printf("GET name=%s client=%s ip=%s result=OK gen=%d expiresAt=%d probe=%v",
		name, client.Name, ip, snap.Gen, snap.ExpiresAtMs(), probe)
}

// freshUnverifiedWindow is how long after apparent issuance a profile 401/403
// on an offered token is treated as transient rather than definitive. Measured
// 2026-07-06: /api/oauth/profile returned 401 for several minutes after a token
// was issued (propagation lag) while /v1/messages already accepted it.
const freshUnverifiedWindow = 15 * time.Minute

// oauthApparentAge estimates how long ago the offered token was issued, using
// the standard 8h access-token lifetime (expires_in has been a constant 28800s).
// ok=false when the age cannot be computed sensibly (missing/absurd expiresAt);
// callers must then fall back to the definitive classification.
func oauthApparentAge(oauth map[string]any, nowMs int64) (time.Duration, bool) {
	rec := creds.Record{OAuth: oauth}
	exp := rec.ExpiresAtMs()
	if exp <= 0 {
		return 0, false
	}
	age := time.Duration(nowMs-(exp-(8*time.Hour).Milliseconds())) * time.Millisecond
	if age < 0 || age > 9*time.Hour {
		return 0, false
	}
	return age, true
}

// handleOffer is the account-routed offer/adopt endpoint (S2). The client hands
// in a full credential (WITH refresh token) from a fresh /login; the broker
// verifies liveness, routes by account, checks anti-rollback, and adopts.
func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	client, ok := s.authClient(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.rateLimitOffer(client.Name) {
		s.audit.Printf("OFFER client=%s ip=%s result=RATELIMITED", client.Name, ip)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	oauth, err := parseOAuth(raw) // requires refreshToken
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	accessToken := mstr(oauth, "accessToken")
	if accessToken == "" {
		http.Error(w, "missing accessToken", http.StatusBadRequest)
		return
	}
	offHash := sha256hex(accessToken)

	respond := func(adopted bool, name, reason, account string, gen int64) {
		writeJSON(w, http.StatusOK, map[string]any{"adopted": adopted, "name": name, "reason": reason, "account": account, "gen": gen})
	}

	for attempt := 0; attempt < 2; attempt++ {
		// Snapshot the store version BEFORE the out-of-mutex verify so the mutex
		// re-check can detect ANY store mutation since (b), not just to the matched
		// cred (S2e "since (b)"; MINOR-1).
		verAtB := s.storeVer.Load()

		// (b) verify offered token live — OUTSIDE any cred mutex.
		cctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		acct, perr := anthropic.FetchProfile(cctx, accessToken, s.now())
		cancel()
		if perr != nil {
			if anthropic.AuthFailure(perr) {
				// A freshly issued token can 401 on profile for minutes after
				// issuance while inference already accepts it. offer_not_live is
				// definitive to the client's overwrite gate, so a fresh token gets
				// a retryable classification instead (clients keep local and
				// re-offer next cycle; a genuinely dead-but-fresh-looking token
				// converges to offer_not_live once the window passes).
				if age, ok := oauthApparentAge(oauth, s.now()); ok && age < freshUnverifiedWindow {
					s.audit.Printf("OFFER client=%s ip=%s result=fresh_unverified age=%s", client.Name, ip, age.Round(time.Second))
					respond(false, "", "fresh_unverified", "", 0)
					return
				}
				s.audit.Printf("OFFER client=%s ip=%s result=offer_not_live", client.Name, ip)
				respond(false, "", "offer_not_live", "", 0)
				return
			}
			s.audit.Printf("OFFER client=%s ip=%s result=verify_unavailable err=%v", client.Name, ip, perr)
			respond(false, "", "verify_unavailable", "", 0)
			return
		}
		email := mstr(acct, "emailAddress")
		uuid := mstr(acct, "accountUuid")
		if !s.migrated.Load() {
			respond(false, "", "migration_pending", email, 0)
			return
		}

		// (c) route across the full store.
		match, rreason := s.routeByAccount(uuid, email)
		if rreason != "" {
			s.audit.Printf("OFFER client=%s ip=%s account=%s result=%s", client.Name, ip, email, rreason)
			respond(false, "", rreason, email, 0)
			return
		}
		capturedGen := match.Gen

		// (d) anti-rollback ring.
		if containsStr(match.RetiredHashes, offHash) {
			s.audit.Printf("OFFER client=%s ip=%s name=%s result=rollback", client.Name, ip, match.Name)
			respond(false, match.Name, "rollback", email, capturedGen)
			return
		}

		// (e) commit under the matched cred's mutex.
		m := s.credMutex(match.Name)
		m.Lock()
		cur, ok := s.store.Get(match.Name)
		if !ok {
			m.Unlock()
			respond(false, "", "unknown_account", email, 0)
			return
		}
		if cur.Gen != capturedGen || s.storeVer.Load() != verAtB {
			m.Unlock()
			if attempt == 0 {
				continue // store changed since verify+route; redo from (b) once
			}
			s.audit.Printf("OFFER client=%s ip=%s name=%s result=conflict", client.Name, ip, match.Name)
			respond(false, match.Name, "conflict", email, cur.Gen)
			return
		}
		if sha256hex(cur.AccessToken()) == offHash {
			gen := cur.Gen
			m.Unlock()
			respond(false, match.Name, "already_current", email, gen)
			return
		}
		// ADOPT — live-verified in (b), identity matched in (c), not a rollback (d).
		nr := cloneRecord(cur)
		nr.RetiredHashes = retireHash(cur.RetiredHashes, sha256hex(cur.AccessToken()))
		nr.OAuth = cloneOAuth(oauth) // full, WITH refresh token
		nr.Account = firstNonEmpty(email, cur.Account)
		nr.AccountUUID = firstNonEmpty(uuid, cur.AccountUUID)
		nr.HealthState = creds.HealthOK // clears BOTH suspect and dead (N-2)
		nr.HealthSince = s.now()
		nr.LastError = ""
		if err := s.commit(nr); err != nil {
			m.Unlock()
			http.Error(w, "commit error", http.StatusInternalServerError)
			return
		}
		gen := nr.Gen
		m.Unlock()
		s.audit.Printf("ADOPT name=%s client=%s gen=%d account=%s", match.Name, client.Name, gen, nr.Account)
		respond(true, match.Name, "", nr.Account, gen)
		return
	}
}

// handleUsage returns quota snapshots for every credential the client's scope
// allows. It never includes tokens.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	client, ok := s.authClient(r)
	if !ok {
		s.audit.Printf("USAGE-API client=? ip=%s result=UNAUTHORIZED", ip)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	type row struct {
		Name           string           `json:"name"`
		Account        string           `json:"account,omitempty"`
		Dead           bool             `json:"dead,omitempty"`
		Health         string           `json:"health,omitempty"`
		ExpiresAt      int64            `json:"expiresAt"`
		Usage          *anthropic.Usage `json:"usage,omitempty"`
		OAuthAccount   map[string]any   `json:"oauthAccount,omitempty"`
		UsageFetchedAt int64            `json:"usageFetchedAt,omitempty"`
		UsageError     string           `json:"usageError,omitempty"`
	}
	out := []row{}
	for _, rec := range s.store.List() {
		if !scopeAllows(client, rec.Name) {
			continue
		}
		rw := row{Name: rec.Name, Account: rec.Account, Dead: rec.Health() == creds.HealthDead,
			Health: rec.Health(), ExpiresAt: rec.ExpiresAtMs()}
		s.usageMu.Lock()
		if e := s.usage[rec.Name]; e != nil {
			rw.Usage = e.Usage
			rw.OAuthAccount = e.OAuthAccount
			rw.UsageFetchedAt = e.FetchedAt
			rw.UsageError = e.LastError
		}
		s.usageMu.Unlock()
		out = append(out, rw)
	}
	s.audit.Printf("USAGE-API client=%s ip=%s result=OK creds=%d", client.Name, ip, len(out))
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

// ---- admin API (localhost, admin token) ----

func (s *Server) adminAuth(r *http.Request) bool {
	t := r.Header.Get("X-Admin-Token")
	if s.cfg.AdminToken == "" || t == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.cfg.AdminToken)) == 1
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/creds")
	rest = strings.Trim(rest, "/")

	if rest == "" { // /admin/creds
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.adminList(w)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	if len(parts) == 2 && parts[1] == "refresh" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.adminRefresh(w, r, name)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.adminPut(w, r, name)
	case http.MethodDelete:
		if err := s.store.Delete(name); err != nil {
			http.Error(w, "no such credential", http.StatusNotFound)
			return
		}
		s.audit.Printf("ADMIN delete name=%s", name)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) adminList(w http.ResponseWriter) {
	type row struct {
		Name      string `json:"name"`
		Account   string `json:"account"`
		AccountID string `json:"accountUuid,omitempty"`
		Gen       int64  `json:"gen"`
		Health    string `json:"health"`
		ExpiresAt int64  `json:"expiresAt"`
		Dead      bool   `json:"dead"`
		UpdatedAt int64  `json:"updatedAt"`
		LastError string `json:"lastError,omitempty"`
	}
	var out []row
	for _, r := range s.store.List() {
		out = append(out, row{r.Name, r.Account, r.AccountUUID, r.Gen, r.Health(),
			r.ExpiresAtMs(), r.Health() == creds.HealthDead, r.UpdatedAt, r.LastError})
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

// adminPut imports/replaces a credential (S6): it verifies the token live via
// profile, sets Account/AccountUUID, enforces account uniqueness, and rejects
// the import when profile verification is unavailable (no Account-empty records
// can exist, so offer routing always has an identity to match).
func (s *Server) adminPut(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || strings.ContainsAny(name, "/?") {
		http.Error(w, "bad credential name", http.StatusBadRequest)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	oauth, err := parseOAuth(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	accessToken := mstr(oauth, "accessToken")
	if accessToken == "" {
		http.Error(w, "missing accessToken", http.StatusBadRequest)
		return
	}
	cctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	acct, perr := anthropic.FetchProfile(cctx, accessToken, s.now())
	cancel()
	if perr != nil {
		s.audit.Printf("ADMIN import name=%s result=REJECTED_PROFILE_DOWN err=%v", name, perr)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false,
			"error": "profile verification unavailable; retry when the endpoint is healthy: " + perr.Error()})
		return
	}
	email := mstr(acct, "emailAddress")
	uuid := mstr(acct, "accountUuid")
	for _, rec := range s.store.List() {
		if rec.Name == name {
			continue
		}
		if (email != "" && rec.Account == email) || (uuid != "" && rec.AccountUUID == uuid) {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false,
				"error": fmt.Sprintf("account %q already managed by cred %q", email, rec.Name)})
			return
		}
	}
	m := s.credMutex(name)
	m.Lock()
	defer m.Unlock()
	nr := &creds.Record{Name: name, Account: email, AccountUUID: uuid, OAuth: oauth,
		HealthState: creds.HealthOK, HealthSince: s.now()}
	if cur, ok := s.store.Get(name); ok {
		// Retire the replaced access-token hash into the ring so an old copy can no
		// longer be re-offered as if fresh (MINOR-3); preserves the prior ring.
		nr.RetiredHashes = retireHash(cur.RetiredHashes, sha256hex(cur.AccessToken()))
	}
	if err := s.commit(nr); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit.Printf("ADMIN import name=%s account=%s gen=%d expiresAt=%d", name, email, nr.Gen, nr.ExpiresAtMs())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "account": email, "gen": nr.Gen, "expiresAt": nr.ExpiresAtMs()})
}

func (s *Server) adminRefresh(w http.ResponseWriter, r *http.Request, name string) {
	rec, ok := s.store.Get(name)
	if !ok {
		http.Error(w, "no such credential", http.StatusNotFound)
		return
	}
	m := s.credMutex(name)
	m.Lock()
	defer m.Unlock()
	rec, ok = s.store.Get(name)
	if !ok {
		// Deleted between the first read and acquiring the mutex (MINOR-2): avoid
		// a nil deref in doRefresh.
		http.Error(w, "no such credential", http.StatusNotFound)
		return
	}
	nr, err := s.doRefresh(r.Context(), rec)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "dead": nr != nil && nr.Health() == creds.HealthDead})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "expiresAt": nr.ExpiresAtMs(), "account": nr.Account})
}

// parseOAuth accepts either a full .credentials.json ({"claudeAiOauth":{...}})
// or a bare oauth object, and requires a refreshToken.
func parseOAuth(raw []byte) (map[string]any, error) {
	var f creds.File
	if err := json.Unmarshal(raw, &f); err == nil && f.ClaudeAiOauth != nil {
		return validateOAuth(f.ClaudeAiOauth)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("invalid json")
	}
	return validateOAuth(m)
}

func validateOAuth(m map[string]any) (map[string]any, error) {
	if m == nil {
		return nil, errors.New("empty oauth object")
	}
	if _, ok := m["refreshToken"].(string); !ok {
		return nil, errors.New("missing refreshToken")
	}
	return m, nil
}

// ---- HTTP servers ----

// Serve starts the credential and admin listeners and blocks until ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	credMux := http.NewServeMux()
	credMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	credMux.HandleFunc("/v1/credentials/", s.handleGetCred)
	credMux.HandleFunc("/v1/creds/offer", s.handleOffer)
	credMux.HandleFunc("/v1/usage", s.handleUsage)

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/admin/creds", s.handleAdmin)
	adminMux.HandleFunc("/admin/creds/", s.handleAdmin)

	// Only ReadHeaderTimeout is set: no ReadTimeout/WriteTimeout that could kill
	// an idle long-poll (S3/m-4). Per-request write deadlines are set after wake.
	credSrv := &http.Server{Addr: s.cfg.Listen, Handler: credMux, ReadHeaderTimeout: 10 * time.Second}
	adminSrv := &http.Server{Addr: s.cfg.AdminListen, Handler: adminMux, ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 2)
	go func() { errCh <- credSrv.ListenAndServe() }()
	go func() { errCh <- adminSrv.ListenAndServe() }()
	s.audit.Printf("START listen=%s admin=%s skew=%s serveRefreshToken=%v", s.cfg.Listen, s.cfg.AdminListen, s.skew, s.cfg.ServeRefreshToken)

	select {
	case <-ctx.Done():
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = credSrv.Shutdown(sh)
		_ = adminSrv.Shutdown(sh)
		return nil
	case err := <-errCh:
		return err
	}
}

// ---- helpers ----

func cloneOAuth(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneRecord(r *creds.Record) *creds.Record {
	var ring []string
	if len(r.RetiredHashes) > 0 {
		ring = append(ring, r.RetiredHashes...)
	}
	return &creds.Record{
		Name: r.Name, Account: r.Account, AccountUUID: r.AccountUUID,
		OAuth: cloneOAuth(r.OAuth), Gen: r.Gen,
		HealthState: r.HealthState, HealthSince: r.HealthSince, RetiredHashes: ring,
		UpdatedAt: r.UpdatedAt, Dead: r.Dead, LastError: r.LastError,
	}
}

// retireHash appends h to the anti-rollback ring, de-duplicating and keeping the
// last retiredRing entries (S2d).
func retireHash(ring []string, h string) []string {
	if h == "" {
		return ring
	}
	out := make([]string, 0, len(ring)+1)
	for _, x := range ring {
		if x != h {
			out = append(out, x)
		}
	}
	out = append(out, h)
	if len(out) > retiredRing {
		out = out[len(out)-retiredRing:]
	}
	return out
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func mstr(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
