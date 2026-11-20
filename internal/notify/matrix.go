package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"focusgate/internal/config"
	"focusgate/internal/store"
)

// MatrixPublisher delivers alerts to Element / Matrix rooms or webhooks.
type MatrixPublisher struct {
	cfg    config.MatrixConfig
	client *http.Client
}

// NewMatrixPublisher constructs a publisher for Element / Matrix endpoints.
func NewMatrixPublisher(cfg config.MatrixConfig) *MatrixPublisher {
	return &MatrixPublisher{
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				if req.Response != nil {
					status := req.Response.StatusCode
					if status == http.StatusMovedPermanently ||
						status == http.StatusFound ||
						status == http.StatusSeeOther {
						return fmt.Errorf("matrix redirect %d rejected", status)
					}
					if status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect {
						if len(via) > 0 && (req.URL.Host != via[0].URL.Host || req.URL.Scheme != "https") {
							return fmt.Errorf("matrix redirect %d to %s rejected", status, req.URL.Host)
						}
					}
				}
				return nil
			},
		},
	}
}

// Publish delivers a formatted notification to the configured Matrix endpoint.
func (p *MatrixPublisher) Publish(ctx context.Context, item store.StoredItem, publicURL string) error {
	webhookURL := strings.TrimSpace(p.cfg.WebhookURL)
	if webhookURL == "" {
		return errors.New("matrix webhook or room URL is required")
	}

	clickURL := item.URL
	if clickURL == "" {
		clickURL = publicURL
	}

	mention := strings.TrimSpace(p.cfg.MentionHandle)
	mentionPrefix := ""
	if mention != "" {
		mentionPrefix = mention + " "
	}

	titleText := item.Title
	if titleText == "" {
		titleText = "Notification"
	}

	plainBody := fmt.Sprintf("%s[FocusGate] P%d: %s (%s)", mentionPrefix, item.Priority, titleText, clickURL)
	var snippet string
	if item.Body != "" {
		snippet = item.Body
		if len(snippet) > 400 {
			snippet = truncateBytesUTF8(snippet, 400) + "..."
		}
		plainBody += "\n" + snippet
	}

	htmlBody := fmt.Sprintf(
		"<p><strong>[FocusGate]</strong> %s<a href=\"%s\"><strong>%s</strong></a><br>"+
			"Priority: <strong>P%d</strong> · Source: <em>%s</em></p>",
		html.EscapeString(mentionPrefix),
		html.EscapeString(clickURL),
		html.EscapeString(titleText),
		item.Priority,
		html.EscapeString(item.SourceType),
	)
	if snippet != "" {
		htmlBody += fmt.Sprintf("<blockquote>%s</blockquote>", html.EscapeString(snippet))
	}

	payload := map[string]any{
		"m.mentions":     matrixMentions(mention),
		"msgtype":        "m.text",
		"body":           plainBody,
		"text":           plainBody,
		"format":         "org.matrix.custom.html",
		"formatted_body": htmlBody,
	}

	return p.sendPayload(ctx, webhookURL, payload)
}

// PublishDigest delivers a digest summary to Element / Matrix.
func (p *MatrixPublisher) PublishDigest(ctx context.Context, title, body, publicURL string) error {
	webhookURL := strings.TrimSpace(p.cfg.WebhookURL)
	if webhookURL == "" {
		return errors.New("matrix webhook or room URL is required")
	}

	mention := strings.TrimSpace(p.cfg.MentionHandle)
	mentionPrefix := ""
	if mention != "" {
		mentionPrefix = mention + " "
	}

	clickURL := publicURL
	if clickURL == "" {
		clickURL = "http://127.0.0.1:8787"
	}

	plainBody := fmt.Sprintf("%s[FocusGate] %s (%s)\n\n%s", mentionPrefix, title, clickURL, body)

	lines := strings.Split(body, "\n")
	var htmlList strings.Builder
	htmlList.WriteString("<ul>")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			htmlList.WriteString("<li>")
			htmlList.WriteString(html.EscapeString(l))
			htmlList.WriteString("</li>")
		}
	}
	htmlList.WriteString("</ul>")

	htmlBody := fmt.Sprintf(
		"<p><strong>[FocusGate]</strong> %s<a href=\"%s\"><strong>%s</strong></a></p>%s",
		html.EscapeString(mentionPrefix),
		html.EscapeString(clickURL),
		html.EscapeString(title),
		htmlList.String(),
	)

	payload := map[string]any{
		"m.mentions":     matrixMentions(mention),
		"msgtype":        "m.text",
		"body":           plainBody,
		"text":           plainBody,
		"format":         "org.matrix.custom.html",
		"formatted_body": htmlBody,
	}

	return p.sendPayload(ctx, webhookURL, payload)
}

// SendTest sends an immediate verification alert to the given Matrix endpoint.
func (p *MatrixPublisher) SendTest(ctx context.Context, webhookURL, mention string) error {
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return errors.New("webhook URL is required")
	}

	mentionPrefix := ""
	if mention = strings.TrimSpace(mention); mention != "" {
		mentionPrefix = mention + " "
	}

	plainBody := fmt.Sprintf("%s[FocusGate] Test alert: your Element / Matrix notification channel is verified and operational!", mentionPrefix)
	htmlBody := fmt.Sprintf("<p><strong>[FocusGate]</strong> %sElement / Matrix notification channel successfully connected! 🟢</p>", html.EscapeString(mentionPrefix))

	payload := map[string]any{
		"m.mentions":     matrixMentions(mention),
		"msgtype":        "m.text",
		"body":           plainBody,
		"text":           plainBody,
		"format":         "org.matrix.custom.html",
		"formatted_body": htmlBody,
	}

	return p.sendPayload(ctx, webhookURL, payload)
}

func (p *MatrixPublisher) sendPayload(ctx context.Context, webhookURL string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal matrix payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(data))
	if err != nil {
		return privateRequestError("matrix", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "FocusGate")

	resp, err := p.client.Do(req)
	if err != nil {
		return privateRequestError("matrix", err, resp)
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("matrix webhook HTTP %d", resp.StatusCode)
	}

	return nil
}

// matrixMentions permits only the configured user to be notified intentionally.
func matrixMentions(handle string) map[string]any {
	mentions := map[string]any{"room": false, "user_ids": []string{}}
	handle = strings.TrimSpace(handle)
	if strings.HasPrefix(handle, "@") && strings.Contains(handle, ":") && !strings.ContainsAny(handle, " \t\r\n") {
		mentions["user_ids"] = []string{handle}
	}
	return mentions
}
