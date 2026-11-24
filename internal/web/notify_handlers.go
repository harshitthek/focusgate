package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"focusgate/internal/config"
	"focusgate/internal/notify"
	"focusgate/internal/store"
)

func validateWebhookURL(raw string, serviceName string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid %s webhook URL: %w", serviceName, err)
	}
	if u.User != nil {
		return fmt.Errorf("%s webhook URL cannot contain embedded credentials", serviceName)
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return fmt.Errorf("%s webhook URL must specify a host", serviceName)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "http" {
		if hostname != "127.0.0.1" && hostname != "localhost" && hostname != "::1" && hostname != "[::1]" {
			return fmt.Errorf("%s webhook URL must use https:// (http:// is only allowed for loopback)", serviceName)
		}
	} else if scheme != "https" {
		return fmt.Errorf("%s webhook URL must use https://", serviceName)
	}
	return nil
}

// handleSaveNotifySettings saves Slack, Matrix, and mention filtering preferences to SQLite.
func (s *Server) handleSaveNotifySettings(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	var body struct {
		SlackEnabled     *bool   `json:"slack_enabled"`
		SlackWebhookURL  *string `json:"slack_webhook_url"`
		SlackMention     *string `json:"slack_mention"`
		MatrixEnabled    *bool   `json:"matrix_enabled"`
		MatrixWebhookURL *string `json:"matrix_webhook_url"`
		MatrixMention    *string `json:"matrix_mention"`
		OnlyMentions     *bool   `json:"only_mentions"`
		MentionHandles   *string `json:"mention_handles"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()

	ctx := r.Context()
	current, err := s.engine.Store().GetNotifySettings(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("Failed to load current settings: %v", err)})
		return
	}
	var toSave store.NotifySettings = current

	if body.SlackEnabled != nil {
		toSave.SlackEnabled = *body.SlackEnabled
	}
	if body.SlackWebhookURL != nil {
		slackURL := strings.TrimSpace(*body.SlackWebhookURL)
		if slackURL == maskWebhookURL(current.SlackWebhookURL) && current.SlackWebhookURL != "" {
			slackURL = current.SlackWebhookURL
		}
		if strings.HasPrefix(strings.ToLower(slackURL), "https://...") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "redacted webhook preview is stale; reload settings"})
			return
		}
		if err := validateWebhookURL(slackURL, "Slack"); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		toSave.SlackWebhookURL = slackURL
	}
	if body.SlackMention != nil {
		toSave.SlackMention = strings.TrimSpace(*body.SlackMention)
	}

	if body.MatrixEnabled != nil {
		toSave.MatrixEnabled = *body.MatrixEnabled
	}
	if body.MatrixWebhookURL != nil {
		matrixURL := strings.TrimSpace(*body.MatrixWebhookURL)
		if matrixURL == maskWebhookURL(current.MatrixWebhookURL) && current.MatrixWebhookURL != "" {
			matrixURL = current.MatrixWebhookURL
		}
		if strings.HasPrefix(strings.ToLower(matrixURL), "https://...") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "redacted webhook preview is stale; reload settings"})
			return
		}
		if err := validateWebhookURL(matrixURL, "Matrix"); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		toSave.MatrixWebhookURL = matrixURL
	}
	if body.MatrixMention != nil {
		toSave.MatrixMention = strings.TrimSpace(*body.MatrixMention)
	}

	if body.OnlyMentions != nil {
		toSave.OnlyMentions = *body.OnlyMentions
	}
	if body.MentionHandles != nil {
		toSave.MentionHandles = strings.TrimSpace(*body.MentionHandles)
	}

	if err := s.engine.Store().SetNotifySettings(ctx, toSave); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("Failed to save settings: %v", err)})
		return
	}

	if _, err := s.engine.ReloadNotifySettings(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("Settings were persisted to database but failed to apply to active runtime: %v. Please retry saving.", err),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"slack_enabled":   toSave.SlackEnabled,
		"slack_preview":   maskWebhookURL(toSave.SlackWebhookURL),
		"slack_mention":   toSave.SlackMention,
		"matrix_enabled":  toSave.MatrixEnabled,
		"matrix_preview":  maskWebhookURL(toSave.MatrixWebhookURL),
		"matrix_mention":  toSave.MatrixMention,
		"only_mentions":   toSave.OnlyMentions,
		"mention_handles": toSave.MentionHandles,
	})
}

// handleTestSlack sends a test message to the provided or saved Slack webhook.
func (s *Server) handleTestSlack(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	var body struct {
		WebhookURL    *string `json:"webhook_url"`
		MentionHandle *string `json:"mention_handle"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
			return
		}
	}

	ctx := r.Context()
	saved, _ := s.engine.Store().GetNotifySettings(ctx)

	targetURL := ""
	if body.WebhookURL != nil {
		targetURL = strings.TrimSpace(*body.WebhookURL)
	}

	mention := ""
	if body.MentionHandle != nil {
		mention = strings.TrimSpace(*body.MentionHandle)
	} else {
		mention = saved.SlackMention
	}

	if targetURL == "" || (saved.SlackWebhookURL != "" && targetURL == maskWebhookURL(saved.SlackWebhookURL)) {
		if saved.SlackWebhookURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Please enter a Slack webhook URL first"})
			return
		}
		targetURL = saved.SlackWebhookURL
	}

	if err := validateWebhookURL(targetURL, "Slack"); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	pub := notify.NewSlackPublisher(config.SlackConfig{
		Enabled:       true,
		WebhookURL:    targetURL,
		MentionHandle: mention,
	})

	if err := pub.SendTest(ctx, targetURL, mention); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Slack test delivery failed: %v", err)})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Slack test alert sent successfully!",
	})
}

// handleTestMatrix sends a test message to the provided or saved Element / Matrix endpoint.
func (s *Server) handleTestMatrix(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	var body struct {
		WebhookURL    *string `json:"webhook_url"`
		MentionHandle *string `json:"mention_handle"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
			return
		}
	}

	ctx := r.Context()
	saved, _ := s.engine.Store().GetNotifySettings(ctx)

	targetURL := ""
	if body.WebhookURL != nil {
		targetURL = strings.TrimSpace(*body.WebhookURL)
	}

	mention := ""
	if body.MentionHandle != nil {
		mention = strings.TrimSpace(*body.MentionHandle)
	} else {
		mention = saved.MatrixMention
	}

	if targetURL == "" || (saved.MatrixWebhookURL != "" && targetURL == maskWebhookURL(saved.MatrixWebhookURL)) {
		if saved.MatrixWebhookURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Please enter a Matrix webhook or room URL first"})
			return
		}
		targetURL = saved.MatrixWebhookURL
	}

	if err := validateWebhookURL(targetURL, "Matrix"); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	pub := notify.NewMatrixPublisher(config.MatrixConfig{
		Enabled:       true,
		WebhookURL:    targetURL,
		MentionHandle: mention,
	})

	if err := pub.SendTest(ctx, targetURL, mention); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Matrix test delivery failed: %v", err)})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Element / Matrix test alert sent successfully!",
	})
}

func maskWebhookURL(url string) string {
	url = strings.TrimSpace(url)
	if len(url) <= 12 {
		return url
	}
	return url[:8] + "..." + url[len(url)-6:]
}
