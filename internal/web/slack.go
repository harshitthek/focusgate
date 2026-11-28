package web

import (
	"context"
	"io"
	"net/http"
	"time"

	"focusgate/internal/ingest"
)

func (s *Server) handleSlackEvent(w http.ResponseWriter, r *http.Request) {
	cfg := s.engine.Config()
	slack := cfg.Sources.Slack
	if !slack.Enabled || slack.SigningSecret == "" || slack.WorkspaceID == "" {
		http.NotFound(w, r)
		return
	}
	now := time.Now().UTC()
	if err := http.NewResponseController(w).SetReadDeadline(now.Add(2 * time.Second)); err == nil {
		defer http.NewResponseController(w).SetReadDeadline(time.Time{})
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid Slack request body", http.StatusBadRequest)
		return
	}
	if !ingest.VerifySlackEvent(slack.SigningSecret, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), body, now) {
		http.Error(w, "invalid Slack signature", http.StatusUnauthorized)
		return
	}
	item, challenge, err := ingest.ParseSlackEvent(body, slack.WorkspaceID, slack.MentionUserID, now, cfg.Storage.MaxSnippetChars)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if challenge != "" {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": challenge})
		return
	}
	if item.ExternalID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// The server operator owns the configured workspace. Commit before acknowledging so Slack can retry failures.
	ctx, cancel := context.WithDeadline(r.Context(), now.Add(2500*time.Millisecond))
	defer cancel()
	item.UserID = 1
	if _, _, err := s.engine.ProcessItem(ctx, item); err != nil {
		http.Error(w, "Slack event could not be saved", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
