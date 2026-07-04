// Package server implements the broker: a background refresh manager plus a
// credential API (bearer + scope) and a localhost-only admin API.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ccbroker/internal/anthropic"
	"ccbroker/internal/config"
	"ccbroker/internal/creds"
	"ccbroker/internal/store"
)

const transientBackoff = 60 * time.Second

type Server struct {
	cfg   *config.Server
	store *store.Store
	audit *log.Logger
	skew  time.Duration

	mu       sync.Mutex
	inflight map[string]*sync.Mutex // per-cred single-flight refresh
	backoff  map[string]time.Time   // per-cred next-allowed-attempt after transient failure

	usageMu sync.Mutex
	usage   map[string]*usageEntry // per-cred latest quota snapshot
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
	return &Server{
		cfg:      cfg,
		store:    st,
		audit:    log.New(w, "", log.LstdFlags|log.LUTC),
		skew:     time.Duration(cfg.RefreshSkewSec) * time.Second,
		inflight: map[string]*sync.Mutex{},
		backoff:  map[string]time.Time{},
		usage:    map[string]*usageEntry{},
	}, nil
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
	return r.ExpiresAtMs()-time.Now().UnixMilli() <= s.skew.Milliseconds()
}

// ensureFresh refreshes the named credential if it is within the skew window.
// It is safe under concurrency (single-flight per credential). It returns the
// most current record even if a transient refresh failed.
func (s *Server) ensureFresh(ctx context.Context, name string) (*creds.Record, error) {
	rec, ok := s.store.Get(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	if rec.Dead || !s.needsRefresh(rec) {
		return rec, nil
	}
	m := s.credMutex(name)
	m.Lock()
	defer m.Unlock()

	// Re-check after acquiring the lock — another goroutine may have refreshed.
	rec, ok = s.store.Get(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	if rec.Dead || !s.needsRefresh(rec) {
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

// doRefresh performs the actual refresh. Caller must hold the per-cred mutex.
func (s *Server) doRefresh(ctx context.Context, rec *creds.Record) (*creds.Record, error) {
	res, err := anthropic.Refresh(ctx, rec.RefreshToken())
	if err != nil {
		var ae *anthropic.Err
		if errors.As(err, &ae) && ae.Permanent {
			dead := cloneRecord(rec)
			dead.Dead = true
			dead.LastError = ae.Error()
			dead.UpdatedAt = time.Now().UnixMilli()
			_ = s.store.Put(dead)
			s.audit.Printf("REFRESH name=%s result=DEAD err=%q", rec.Name, ae.Error())
			return dead, err
		}
		s.mu.Lock()
		s.backoff[rec.Name] = time.Now().Add(transientBackoff)
		s.mu.Unlock()
		s.audit.Printf("REFRESH name=%s result=TRANSIENT err=%v", rec.Name, err)
		return rec, err
	}

	oauth := cloneOAuth(rec.OAuth)
	oauth["accessToken"] = res.AccessToken
	if res.RefreshToken != "" {
		oauth["refreshToken"] = res.RefreshToken
	}
	oauth["expiresAt"] = float64(time.Now().Add(time.Duration(res.ExpiresIn) * time.Second).UnixMilli())
	if res.Scope != "" {
		oauth["scopes"] = toAnySlice(strings.Fields(res.Scope))
	}
	nr := &creds.Record{
		Name:      rec.Name,
		Account:   firstNonEmpty(res.Account.EmailAddress, rec.Account),
		OAuth:     oauth,
		UpdatedAt: time.Now().UnixMilli(),
	}
	if err := s.store.Put(nr); err != nil {
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
		if rec.Dead || !s.needsRefresh(rec) {
			continue
		}
		m := s.credMutex(rec.Name)
		if !m.TryLock() {
			continue // a request is already refreshing this cred
		}
		func() {
			defer m.Unlock()
			cur, ok := s.store.Get(rec.Name)
			if !ok || cur.Dead || !s.needsRefresh(cur) {
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

// ---- usage polling (quota, no message-quota cost) ----

// RunUsageLoop polls the OAuth usage endpoint for every live credential so
// clients can make quota-aware account choices. Polling reads utilization
// only; it never consumes message quota and never touches the refresh chain.
func (s *Server) RunUsageLoop(ctx context.Context) {
	iv := time.Duration(s.cfg.UsagePollSec) * time.Second
	t := time.NewTicker(iv)
	defer t.Stop()
	s.pollUsage(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollUsage(ctx)
		}
	}
}

func (s *Server) pollUsage(ctx context.Context) {
	for _, rec := range s.store.List() {
		if rec.Dead || rec.ExpiresAtMs() <= time.Now().UnixMilli() {
			continue // no valid access token to query with; refresh loop will fix
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		u, err := anthropic.FetchUsage(cctx, rec.AccessToken())
		acct, perr := anthropic.FetchProfile(cctx, rec.AccessToken(), time.Now().UnixMilli())
		cancel()
		s.usageMu.Lock()
		e := s.usage[rec.Name]
		if e == nil {
			e = &usageEntry{}
			s.usage[rec.Name] = e
		}
		if err != nil {
			e.LastError = err.Error()
		} else {
			e.Usage = u
			e.FetchedAt = time.Now().UnixMilli()
			e.LastError = ""
		}
		if perr == nil {
			e.OAuthAccount = acct
		}
		s.usageMu.Unlock()
		if err != nil {
			s.audit.Printf("USAGE name=%s result=ERR err=%v", rec.Name, err)
		}
		if perr != nil {
			s.audit.Printf("PROFILE name=%s result=ERR err=%v", rec.Name, perr)
		}
	}
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
	rec, ferr := s.ensureFresh(r.Context(), name)
	if rec == nil {
		if errors.Is(ferr, os.ErrNotExist) {
			http.Error(w, "no such credential", http.StatusNotFound)
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if rec.Dead {
		s.audit.Printf("GET name=%s client=%s ip=%s result=DEAD", name, client.Name, ip)
		http.Error(w, "credential needs re-auth", http.StatusConflict)
		return
	}
	// Never hand out an already-expired token: the client would then refresh it
	// itself and rotate the broker's refresh token out from under us.
	if rec.ExpiresAtMs() <= time.Now().UnixMilli() {
		s.audit.Printf("GET name=%s client=%s ip=%s result=EXPIRED_NO_REFRESH", name, client.Name, ip)
		http.Error(w, "token expired and refresh unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := rec.FileBytes()
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
	s.audit.Printf("GET name=%s client=%s ip=%s result=OK expiresAt=%d", name, client.Name, ip, rec.ExpiresAtMs())
}

// handleUsage returns quota snapshots for every credential the client's scope
// allows — enough for a client to pick the least-utilized account. It never
// includes tokens.
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
		rw := row{Name: rec.Name, Account: rec.Account, Dead: rec.Dead, ExpiresAt: rec.ExpiresAtMs()}
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
	// /admin/creds/{name} or /admin/creds/{name}/refresh
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
		ExpiresAt int64  `json:"expiresAt"`
		Dead      bool   `json:"dead"`
		UpdatedAt int64  `json:"updatedAt"`
		LastError string `json:"lastError,omitempty"`
	}
	var out []row
	for _, r := range s.store.List() {
		out = append(out, row{r.Name, r.Account, r.ExpiresAtMs(), r.Dead, r.UpdatedAt, r.LastError})
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

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
	rec := &creds.Record{Name: name, OAuth: oauth, UpdatedAt: time.Now().UnixMilli()}
	if err := s.store.Put(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit.Printf("ADMIN import name=%s expiresAt=%d", name, rec.ExpiresAtMs())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "expiresAt": rec.ExpiresAtMs()})
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
	rec, _ = s.store.Get(name)
	nr, err := s.doRefresh(r.Context(), rec)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "dead": nr != nil && nr.Dead})
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
	credMux.HandleFunc("/v1/usage", s.handleUsage)

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/admin/creds", s.handleAdmin)
	adminMux.HandleFunc("/admin/creds/", s.handleAdmin)

	credSrv := &http.Server{Addr: s.cfg.Listen, Handler: credMux, ReadHeaderTimeout: 10 * time.Second}
	adminSrv := &http.Server{Addr: s.cfg.AdminListen, Handler: adminMux, ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 2)
	go func() { errCh <- credSrv.ListenAndServe() }()
	go func() { errCh <- adminSrv.ListenAndServe() }()
	s.audit.Printf("START listen=%s admin=%s skew=%s", s.cfg.Listen, s.cfg.AdminListen, s.skew)

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
	return &creds.Record{
		Name: r.Name, Account: r.Account, OAuth: cloneOAuth(r.OAuth),
		UpdatedAt: r.UpdatedAt, Dead: r.Dead, LastError: r.LastError,
	}
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
