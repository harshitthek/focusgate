package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"focusgate/internal/app"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

//go:embed static/*
var staticFS embed.FS

type authTicket struct {
	user      *store.SessionUser
	expiresAt time.Time
}

// Server exposes the embedded PWA, REST control API, SSE stream, and webhook endpoint.
type Server struct {
	engine   *app.Engine
	mux      *http.ServeMux
	ticketMu sync.Mutex
	tickets  map[string]authTicket
	notifyMu sync.Mutex
}

// NewServer builds the HTTP handler tree with embedded PWA assets.
func NewServer(engine *app.Engine) (*Server, error) {
	s := &Server{
		engine:  engine,
		mux:     http.NewServeMux(),
		tickets: make(map[string]authTicket),
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))

	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("POST /api/webhook/{source}", s.handleWebhook)

	s.mux.HandleFunc("POST /api/auth/ticket", s.withAuth(s.handleCreateAuthTicket))
	s.mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("POST /api/auth/setup", s.handleAuthSetup)
	s.mux.HandleFunc("POST /api/auth/register", s.handleAuthRegister)
	s.mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	s.mux.HandleFunc("GET /api/auth/me", s.withAuth(s.handleAuthMe))
	s.mux.HandleFunc("POST /api/auth/change-passphrase", s.withAuth(s.handleAuthChangePassphrase))
	s.mux.HandleFunc("POST /api/classify", s.withAuth(s.handleClassifyProxy))

	s.mux.HandleFunc("GET /api/profile", s.withAuth(s.handleGetProfile))
	s.mux.HandleFunc("POST /api/github/token", s.withAuth(s.handleGitHubToken))
	s.mux.HandleFunc("POST /api/github/disconnect", s.withAuth(s.handleGitHubDisconnect))
	s.mux.HandleFunc("POST /api/github/settings", s.withAuth(s.handleGitHubSettings))
	s.mux.HandleFunc("POST /api/github/device/start", s.withAuth(s.handleGitHubDeviceStart))
	s.mux.HandleFunc("POST /api/github/device/poll", s.withAuth(s.handleGitHubDevicePoll))

	s.mux.HandleFunc("POST /api/email/connect", s.withAuth(s.handleEmailConnect))
	s.mux.HandleFunc("POST /api/email/disconnect", s.withAuth(s.handleEmailDisconnect))
	s.mux.HandleFunc("POST /api/email/sync", s.withAuth(s.handleEmailSync))
	s.mux.HandleFunc("POST /api/email/settings", s.withAuth(s.handleEmailSettings))

	s.mux.HandleFunc("POST /api/notify/settings", s.withAdmin(s.handleSaveNotifySettings))
	s.mux.HandleFunc("POST /api/notify/test-slack", s.withAdmin(s.handleTestSlack))
	s.mux.HandleFunc("POST /api/notify/test-matrix", s.withAdmin(s.handleTestMatrix))

	s.mux.HandleFunc("GET /api/status", s.withAuth(s.handleStatus))
	s.mux.HandleFunc("POST /api/mode", s.withAuth(s.handleSetMode))
	s.mux.HandleFunc("GET /api/items", s.withAuth(s.handleListItems))
	s.mux.HandleFunc("POST /api/items/bulk", s.withAuth(s.handleBulkUpdateItems))
	s.mux.HandleFunc("POST /api/items/{id}/status", s.withAuth(s.handleUpdateItemStatus))
	s.mux.HandleFunc("POST /api/sync", s.withAdmin(s.handleSync))
	s.mux.HandleFunc("POST /api/digest/flush", s.withAdmin(s.handleFlushDigest))
	s.mux.HandleFunc("POST /api/notify/test", s.withAdmin(s.handleTestNotify))
	s.mux.HandleFunc("POST /api/simulate", s.withAuth(s.handleSimulate))
	s.mux.HandleFunc("GET /api/events", s.withAuth(s.handleSSE))

	s.mux.HandleFunc("GET /api/version", s.handleVersion)
	s.mux.HandleFunc("GET /api/update/check", s.withAdmin(s.handleUpdateCheck))
	s.mux.HandleFunc("POST /api/update/apply", s.withAdmin(s.handleUpdateApply))
	s.mux.HandleFunc("GET /download/android", s.handleDownloadAndroid)

	s.mux.Handle("GET /", fileServer)
	return s, nil
}

// Handler returns the root http.Handler protected against cross-origin browser
// requests and DNS rebinding on unauthenticated loopback endpoints.
func (s *Server) Handler() http.Handler {
	csrfProtected := http.NewCrossOriginProtection().Handler(s.mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowHost(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden host"})
			return
		}
		csrfProtected.ServeHTTP(w, r)
	})
}

func (s *Server) allowHost(r *http.Request) bool {
	if isLoopbackHostHeader(r.Host) {
		return true
	}
	// When UI auth is configured, allow remote requests through so withAuth can
	// challenge them with 401, avoiding ticket-consuming authentication in Host validation.
	if strings.TrimSpace(s.engine.Config().Server.AuthToken) != "" {
		return true
	}
	if req, err := s.engine.Store().IsAuthSetupRequired(r.Context()); err == nil && !req {
		return true
	}
	if pub := s.engine.Config().Server.PublicURL; pub != "" {
		if u, err := url.Parse(pub); err == nil && strings.EqualFold(u.Host, r.Host) {
			return true
		}
	}
	return false
}

type contextKey string

const userContextKey contextKey = "focusgate_user"

func userFromContext(ctx context.Context) *store.SessionUser {
	if u, ok := ctx.Value(userContextKey).(*store.SessionUser); ok {
		return u
	}
	return nil
}

func (s *Server) createTicket(user *store.SessionUser) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	ticket := hex.EncodeToString(b)

	s.ticketMu.Lock()
	defer s.ticketMu.Unlock()
	now := time.Now()
	for k, item := range s.tickets {
		if now.After(item.expiresAt) {
			delete(s.tickets, k)
		}
	}
	s.tickets[ticket] = authTicket{
		user:      user,
		expiresAt: now.Add(60 * time.Second),
	}
	return ticket
}

func (s *Server) consumeTicket(ticket string) *store.SessionUser {
	s.ticketMu.Lock()
	defer s.ticketMu.Unlock()
	item, exists := s.tickets[ticket]
	if !exists {
		return nil
	}
	delete(s.tickets, ticket)
	if time.Now().After(item.expiresAt) {
		return nil
	}
	if item.user != nil {
		return item.user
	}
	return &store.SessionUser{UserID: 1, Username: "admin", Role: "admin"}
}

func (s *Server) handleCreateAuthTicket(w http.ResponseWriter, r *http.Request) {
	ticket := s.createTicket(userFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "expires_in": 60})
}

func isLoopbackHostHeader(rawHost string) bool {
	host := strings.TrimSpace(rawHost)
	if host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackClient(r *http.Request) bool {
	if r.RemoteAddr != "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		host = strings.Trim(strings.TrimSpace(host), "[]")
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return isLoopbackHostHeader(r.Host)
}

func (s *Server) authenticateRequest(r *http.Request) *store.SessionUser {
	ctx := r.Context()

	// 1. Check SSE one-time ticket first (for /api/events, explicit ticket takes precedence over stale session cookie)
	if r.URL.Path == "/api/events" {
		if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
			return s.consumeTicket(ticket)
		}
	}

	provided := s.extractToken(r)

	// 2. Check persistent session token in SQLite (returns specific user)
	if provided != "" {
		if su, err := s.engine.Store().ValidateSessionUser(ctx, provided); err == nil && su != nil {
			return su
		}
	}

	// 3. Check legacy or explicit config auth_token (maps to admin)
	expected := strings.TrimSpace(s.engine.Config().Server.AuthToken)
	if expected != "" && provided != "" {
		if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1 {
			return &store.SessionUser{UserID: 1, Username: "admin", Role: "admin"}
		}
	}

	// 4. Initial setup grace on loopback: only if no auth token is in config and setup is required (no user and no master passphrase exists yet)
	if expected == "" {
		if setupReq, err := s.engine.Store().IsAuthSetupRequired(ctx); err == nil && setupReq {
			if isLoopbackClient(r) {
				return &store.SessionUser{UserID: 1, Username: "admin", Role: "admin"}
			}
		}
	}

	return nil
}

func (s *Server) hasValidAuth(r *http.Request) bool {
	return s.authenticateRequest(r) != nil
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := s.authenticateRequest(r)
		if user == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey, user)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) withAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
			return
		}
		next(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	userID := int64(1)
	if u := userFromContext(r.Context()); u != nil {
		userID = u.UserID
	}
	st, err := s.engine.StatusUser(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode config.FocusMode `json:"mode"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	switch body.Mode {
	case config.ModeNormal, config.ModeDeepWork, config.ModeDND:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported focus mode"})
		return
	}

	u := userFromContext(r.Context())
	userID := int64(1)
	if u != nil && u.UserID > 0 {
		userID = u.UserID
	}

	if err := s.engine.Store().SetUserSetting(r.Context(), userID, "focus_mode", string(body.Mode)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save user mode"})
		return
	}
	if userID <= 1 || (u != nil && u.Role == "admin") {
		if err := s.engine.SetMode(r.Context(), body.Mode); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}

	st, _ := s.engine.StatusUser(r.Context(), userID)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	source := r.URL.Query().Get("source")
	query := r.URL.Query().Get("q")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	userID := int64(1)
	if u := userFromContext(r.Context()); u != nil {
		userID = u.UserID
	}

	items, err := s.engine.Store().ListItems(r.Context(), store.ItemFilter{
		UserID: userID,
		Status: status,
		Source: source,
		Query:  query,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if items == nil {
		items = []store.StoredItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
}

func validItemStatus(status string) bool {
	switch status {
	case "read", "archived", "queued", "digested":
		return true
	default:
		return false
	}
}

func (s *Server) handleUpdateItemStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid item id"})
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !validItemStatus(body.Status) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported status"})
		return
	}
	u := userFromContext(r.Context())
	userID := int64(0)
	if u != nil {
		userID = u.UserID
	}
	if err := s.engine.Store().UpdateUserItemStatus(r.Context(), userID, id, body.Status); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.engine.StatusUser(r.Context(), userID)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleBulkUpdateItems(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs        []int64 `json:"ids"`
		FromStatus string  `json:"from_status"`
		Status     string  `json:"status"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !validItemStatus(body.Status) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported status"})
		return
	}
	u := userFromContext(r.Context())
	userID := int64(0)
	if u != nil {
		userID = u.UserID
	}
	updated, err := s.engine.BulkUpdateUserStatus(r.Context(), userID, body.IDs, body.FromStatus, body.Status)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.engine.StatusUser(r.Context(), userID)
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "status": st})
}

func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	sent, err := s.engine.SendTestAlert(r.Context())
	if sent == nil {
		sent = []string{}
	}
	resp := map[string]any{"sent": sent}
	if err != nil {
		resp["error"] = err.Error()
		if len(sent) == 0 {
			writeJSON(w, http.StatusBadGateway, resp)
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	added, err := s.engine.SyncAll(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.engine.Status(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "status": st})
}

func (s *Server) handleFlushDigest(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	count, err := s.engine.FlushDigest(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.engine.Status(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"flushed": count, "status": st})
}

func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode       config.FocusMode `json:"mode"`
		SourceType string           `json:"source_type"`
		SourceName string           `json:"source_name"`
		Sender     string           `json:"sender"`
		Title      string           `json:"title"`
		Body       string           `json:"body"`
		Reason     string           `json:"reason"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(body.Title) == "" && strings.TrimSpace(body.Body) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title or body required"})
		return
	}
	if body.SourceType == "" {
		body.SourceType = "webhook"
	}
	if body.SourceName == "" {
		body.SourceName = "simulator"
	}
	targetUserID := int64(1)
	if u := userFromContext(r.Context()); u != nil && u.UserID > 0 {
		targetUserID = u.UserID
	}
	item := ingest.IncomingItem{
		UserID:     targetUserID,
		SourceType: body.SourceType,
		SourceName: body.SourceName,
		Sender:     body.Sender,
		Title:      body.Title,
		Body:       body.Body,
		Reason:     body.Reason,
	}
	dec := s.engine.SimulateItem(r.Context(), body.Mode, item)
	writeJSON(w, http.StatusOK, dec)
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	cfg := s.engine.Config()
	if !cfg.Sources.Webhook.Enabled {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "webhooks disabled"})
		return
	}
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, 512<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	if !ingest.VerifyWebhookAuth(
		cfg.Sources.Webhook.Secret,
		r.Header.Get("Authorization"),
		r.Header.Get("X-Webhook-Secret"),
		r.Header.Get("X-Hub-Signature-256"),
		rawBody,
	) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid webhook signature or secret"})
		return
	}

	source := r.PathValue("source")
	if source == "" {
		source = "webhook"
	}
	item, err := ingest.ParseWebhook(source, r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery"), rawBody, cfg.Storage.MaxSnippetChars)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	webhookUserID := int64(1)
	if u := userFromContext(r.Context()); u != nil && u.UserID > 0 {
		webhookUserID = u.UserID
	}
	item.UserID = webhookUserID
	stored, inserted, err := s.engine.ProcessItem(r.Context(), item)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"inserted": inserted,
		"item":     stored,
	})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	u := userFromContext(r.Context())
	userID := int64(1)
	if u != nil && u.UserID > 0 {
		userID = u.UserID
	}

	ch, unsub := s.engine.SubscribeUser(userID)
	defer unsub()
	if st, err := s.engine.StatusUser(r.Context(), userID); err == nil {
		if raw, mErr := json.Marshal(app.Event{Type: "status", Data: st}); mErr == nil {
			_, _ = w.Write([]byte("event: status\ndata: " + string(raw) + "\n\n"))
			flusher.Flush()
		}
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, open := <-ch:
			if !open {
				return
			}
			if userID <= 0 {
				continue
			}
			strFrame := string(frame)
			if strings.HasPrefix(strFrame, "event: item\n") {
				allowed := false
				for _, line := range strings.Split(strFrame, "\n") {
					if strings.HasPrefix(line, "data: ") {
						payload := strings.TrimPrefix(line, "data: ")
						var itemWrapper struct {
							UserID int64 `json:"user_id"`
							Data   struct {
								UserID int64 `json:"user_id"`
								Item   struct {
									UserID int64 `json:"user_id"`
								} `json:"item"`
							} `json:"data"`
							Item struct {
								UserID int64 `json:"user_id"`
							} `json:"item"`
						}
						if err := json.Unmarshal([]byte(payload), &itemWrapper); err == nil {
							owner := itemWrapper.Data.UserID
							if owner <= 0 {
								owner = itemWrapper.Data.Item.UserID
							}
							if owner <= 0 {
								owner = itemWrapper.Item.UserID
							}
							if owner <= 0 {
								owner = itemWrapper.UserID
							}
							if owner == userID || (owner <= 0 && userID == 1) {
								allowed = true
							}
						}
						break
					}
				}
				if !allowed {
					continue // drop item frame if ownership cannot be proven to belong to this user
				}
			} else if strings.HasPrefix(strFrame, "event: status\n") {
				userSt, err := s.engine.StatusUser(r.Context(), userID)
				if err != nil {
					continue
				}
				userRaw, mErr := json.Marshal(app.Event{Type: "status", Data: userSt})
				if mErr != nil {
					continue
				}
				frame = []byte("event: status\ndata: " + string(userRaw) + "\n\n")
			} else if strings.HasPrefix(strFrame, "event: digest\n") {
				// Server-configured digest events belong exclusively to user 1 (administrator).
				// Suppress foreign digest events for other users to prevent cross-user leakage.
				if userID > 1 {
					continue
				}
			}
			_, _ = w.Write(frame)
			flusher.Flush()
		case <-keepalive.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

func decodeJSON(r io.Reader, dst any) error {
	return json.NewDecoder(io.LimitReader(r, 256<<10)).Decode(dst)
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

// handleDownloadAndroid serves the native Android APK directly if cached on the server,
// or redirects the client to the latest GitHub Release APK artifact.
func (s *Server) handleDownloadAndroid(w http.ResponseWriter, r *http.Request) {
	candidates := []string{
		"/var/lib/focusgate/focusgate.apk",
		"./focusgate.apk",
		"./dist/focusgate.apk",
	}
	for _, path := range candidates {
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			w.Header().Set("Content-Type", "application/vnd.android.package-archive")
			w.Header().Set("Content-Disposition", "attachment; filename=\"focusgate.apk\"")
			http.ServeFile(w, r, path)
			return
		}
	}

	http.Redirect(w, r, "https://github.com/harshitthek/focusgate/releases/latest/download/focusgate.apk", http.StatusFound)
}
