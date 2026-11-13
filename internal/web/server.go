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

// Server exposes the embedded PWA, REST control API, SSE stream, and webhook endpoint.
type Server struct {
	engine   *app.Engine
	mux      *http.ServeMux
	ticketMu sync.Mutex
	tickets  map[string]time.Time
}

// NewServer builds the HTTP handler tree with embedded PWA assets.
func NewServer(engine *app.Engine) (*Server, error) {
	s := &Server{
		engine:  engine,
		mux:     http.NewServeMux(),
		tickets: make(map[string]time.Time),
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))

	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("POST /api/webhook/{source}", s.handleWebhook)

	s.mux.HandleFunc("POST /api/auth/ticket", s.withAuth(s.handleCreateAuthTicket))
	s.mux.HandleFunc("GET /api/status", s.withAuth(s.handleStatus))
	s.mux.HandleFunc("POST /api/mode", s.withAuth(s.handleSetMode))
	s.mux.HandleFunc("GET /api/items", s.withAuth(s.handleListItems))
	s.mux.HandleFunc("POST /api/items/bulk", s.withAuth(s.handleBulkUpdateItems))
	s.mux.HandleFunc("POST /api/items/{id}/status", s.withAuth(s.handleUpdateItemStatus))
	s.mux.HandleFunc("POST /api/sync", s.withAuth(s.handleSync))
	s.mux.HandleFunc("POST /api/digest/flush", s.withAuth(s.handleFlushDigest))
	s.mux.HandleFunc("POST /api/notify/test", s.withAuth(s.handleTestNotify))
	s.mux.HandleFunc("POST /api/simulate", s.withAuth(s.handleSimulate))
	s.mux.HandleFunc("GET /api/events", s.withAuth(s.handleSSE))

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
	if pub := s.engine.Config().Server.PublicURL; pub != "" {
		if u, err := url.Parse(pub); err == nil && strings.EqualFold(u.Host, r.Host) {
			return true
		}
	}
	return false
}

func (s *Server) hasValidAuth(r *http.Request) bool {
	expected := strings.TrimSpace(s.engine.Config().Server.AuthToken)
	if expected == "" {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if provided == "" {
		provided = strings.TrimSpace(r.Header.Get("X-FocusGate-Token"))
	}
	if provided == "" {
		if c, err := r.Cookie("focusgate_token"); err == nil {
			provided = strings.TrimSpace(c.Value)
		}
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1 {
		return true
	}

	if r.URL.Path == "/api/events" {
		if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
			if s.consumeTicket(ticket) {
				return true
			}
		}
	}
	return false
}

func (s *Server) createTicket() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	ticket := hex.EncodeToString(b)

	s.ticketMu.Lock()
	defer s.ticketMu.Unlock()
	now := time.Now()
	for k, exp := range s.tickets {
		if now.After(exp) {
			delete(s.tickets, k)
		}
	}
	s.tickets[ticket] = now.Add(60 * time.Second)
	return ticket
}

func (s *Server) consumeTicket(ticket string) bool {
	s.ticketMu.Lock()
	defer s.ticketMu.Unlock()
	exp, exists := s.tickets[ticket]
	if !exists {
		return false
	}
	delete(s.tickets, ticket)
	return time.Now().Before(exp)
}

func (s *Server) handleCreateAuthTicket(w http.ResponseWriter, _ *http.Request) {
	ticket := s.createTicket()
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

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expected := strings.TrimSpace(s.engine.Config().Server.AuthToken)
		if expected == "" {
			next(w, r)
			return
		}
		if !s.hasValidAuth(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.engine.Status(r.Context())
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
	if err := s.engine.SetMode(r.Context(), body.Mode); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.engine.Status(r.Context())
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
	items, err := s.engine.Store().ListItems(r.Context(), store.ItemFilter{
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
	if err := s.engine.Store().UpdateItemStatus(r.Context(), id, body.Status); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.engine.Status(r.Context())
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
	updated, err := s.engine.BulkUpdateStatus(r.Context(), body.IDs, body.FromStatus, body.Status)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.engine.Status(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "status": st})
}

func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
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
	item := ingest.IncomingItem{
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

	ch, unsub := s.engine.Subscribe()
	defer unsub()

	if st, err := s.engine.Status(r.Context()); err == nil {
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
