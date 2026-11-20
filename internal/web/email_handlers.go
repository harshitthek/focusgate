package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

// handleEmailConnect verifies and connects an email account via IMAP over TLS.
func (s *Server) handleEmailConnect(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx := r.Context()
	var body struct {
		Provider    string `json:"provider"`
		Host        string `json:"host"`
		Port        int    `json:"port"`
		Username    string `json:"username"`
		Password    string `json:"password"`
		Mailbox     string `json:"mailbox"`
		PollMinutes int    `json:"poll_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	username := strings.TrimSpace(body.Username)

	// Normalize app passwords (remove spaces) only for providers that use grouped app password codes
	password := body.Password
	if provider == "gmail" || provider == "icloud" {
		password = strings.ReplaceAll(password, " ", "")
		password = strings.TrimSpace(password)
	}

	if username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email address cannot be empty"})
		return
	}
	if password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password/app password cannot be empty"})
		return
	}

	host := strings.TrimSpace(body.Host)
	port := body.Port
	if port <= 0 {
		port = 993
	}

	switch provider {
	case "gmail":
		host = "imap.gmail.com"
		port = 993
	case "outlook", "hotmail", "office365":
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Microsoft Outlook/Hotmail has disabled password-based IMAP (Basic Auth) in favor of OAuth2. Please use Gmail, iCloud, or a custom IMAP provider.",
		})
		return
	case "icloud":
		host = "imap.mail.me.com"
		port = 993
	default:
		if host == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "IMAP host is required for custom provider"})
			return
		}
	}

	mailbox := strings.TrimSpace(body.Mailbox)
	if mailbox == "" {
		mailbox = "INBOX"
	}

	pollMin := body.PollMinutes
	if pollMin <= 0 {
		pollMin = 5
	}

	generation, err := s.engine.Store().BeginConnection(ctx, "email")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to start email authorization"})
		return
	}
	// Fast live credentials verification
	testCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	if err := ingest.TestIMAPConnection(testCtx, host, port, username, password); err != nil {
		errDesc := err.Error()
		if strings.Contains(strings.ToLower(errDesc), "authentication failed") || strings.Contains(strings.ToLower(errDesc), "login") {
			if provider == "gmail" {
				errDesc = "Gmail authentication failed. Please ensure you are using a 16-character Google App Password (from myaccount.google.com/apppasswords), NOT your normal Google account password."
			} else {
				errDesc = "Authentication failed. Please verify your email address and App Password."
			}
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errDesc})
		return
	}

	creds := store.EmailCredentials{
		Enabled:     true,
		Provider:    provider,
		Host:        host,
		Port:        port,
		Username:    username,
		Password:    password,
		Mailbox:     mailbox,
		PollMinutes: pollMin,
	}

	if err := s.engine.Store().SetEmailCredentialsIfCurrent(ctx, creds, generation); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save email credentials"})
		return
	}

	// Trigger immediate initial sync in background
	go func() {
		_, _ = s.engine.TriggerEmailSync(context.Background())
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "connected",
		"provider":     provider,
		"username":     username,
		"poll_minutes": pollMin,
	})
}

// handleEmailDisconnect disconnects and removes saved email credentials.
func (s *Server) handleEmailDisconnect(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx := r.Context()
	if err := s.engine.Store().ClearEmailCredentials(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to clear email credentials"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// handleEmailSync triggers an immediate manual polling pass.
func (s *Server) handleEmailSync(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx := r.Context()
	newCount, err := s.engine.TriggerEmailSync(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "synced",
		"new_items": newCount,
	})
}

// handleEmailSettings updates the email poll interval.
func (s *Server) handleEmailSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx := r.Context()
	var body struct {
		PollMinutes int `json:"poll_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}
	if body.PollMinutes <= 0 {
		body.PollMinutes = 5
	}
	if err := s.engine.Store().SetEmailPollMinutes(ctx, body.PollMinutes); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update poll interval"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "updated",
		"poll_minutes": body.PollMinutes,
	})
}
