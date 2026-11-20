package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"focusgate/internal/config"
	"focusgate/internal/store"
)

// SlackPublisher delivers alerts to a Slack incoming webhook.
type SlackPublisher struct {
	cfg    config.SlackConfig
	client *http.Client
}

// NewSlackPublisher constructs a publisher for Slack webhooks.
func NewSlackPublisher(cfg config.SlackConfig) *SlackPublisher {
	return &SlackPublisher{
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
						return fmt.Errorf("slack redirect %d rejected", status)
					}
					if status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect {
						if len(via) > 0 && (req.URL.Host != via[0].URL.Host || req.URL.Scheme != "https") {
							return fmt.Errorf("slack redirect %d to %s rejected", status, req.URL.Host)
						}
					}
				}
				return nil
			},
		},
	}
}

// Publish delivers a formatted notification to the configured Slack webhook.
func (p *SlackPublisher) Publish(ctx context.Context, item store.StoredItem, publicURL string) error {
	webhookURL := strings.TrimSpace(p.cfg.WebhookURL)
	if webhookURL == "" {
		return errors.New("slack webhook URL is required")
	}

	rawURL := item.URL
	if rawURL == "" {
		rawURL = publicURL
	}
	clickURL := sanitizeSlackLinkURL(rawURL)

	mention := strings.TrimSpace(p.cfg.MentionHandle)
	mentionPrefix := ""
	if mention != "" {
		mentionPrefix = mention + " "
	}

	titleText := item.Title
	if titleText == "" {
		titleText = "Notification"
	}
	titleText = truncateBytesUTF8(titleText, 400)
	escapedTitle := escapeSlackMrkdwn(titleText)

	summaryText := fmt.Sprintf("%s[FocusGate] P%d: %s", mentionPrefix, item.Priority, escapedTitle)

	sourceType := escapeSlackMrkdwn(truncateBytesUTF8(item.SourceType, 50))
	prefixAndMeta := fmt.Sprintf("*[FocusGate]* %s\n`Priority P%d` · `%s`", mentionPrefix, item.Priority, sourceType)

	headerBlock := ""
	if clickURL != "" && (len(prefixAndMeta)+len(clickURL)+len(escapedTitle)+7) <= 2900 {
		headerBlock = fmt.Sprintf("*[FocusGate]* %s*<%s|%s>*\n`Priority P%d` · `%s`",
			mentionPrefix,
			clickURL,
			escapedTitle,
			item.Priority,
			sourceType,
		)
	} else {
		headerBlock = fmt.Sprintf("*[FocusGate]* %s*%s*\n`Priority P%d` · `%s`",
			mentionPrefix,
			escapedTitle,
			item.Priority,
			sourceType,
		)
	}
	if len(headerBlock) > 2900 {
		headerBlock = truncateEscapedSlackText(headerBlock, 2900)
	}

	bodySnippet := item.Body
	if len(bodySnippet) > 500 {
		bodySnippet = truncateBytesUTF8(bodySnippet, 500) + "..."
	}
	bodySnippet = escapeSlackMrkdwn(strings.TrimSpace(bodySnippet))

	var blocks []map[string]any
	blocks = append(blocks, map[string]any{
		"type": "section",
		"text": map[string]any{
			"type":     "mrkdwn",
			"text":     headerBlock,
			"verbatim": true,
		},
	})

	if bodySnippet != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type":     "mrkdwn",
				"text":     "> " + strings.ReplaceAll(bodySnippet, "\n", "\n> "),
				"verbatim": true,
			},
		})
	}

	payload := map[string]any{
		"text":   summaryText,
		"blocks": blocks,
	}

	return p.sendPayload(ctx, webhookURL, payload)
}

// PublishDigest delivers a digest summary to Slack with full block formatting.
func (p *SlackPublisher) PublishDigest(ctx context.Context, title, body, publicURL string) error {
	webhookURL := strings.TrimSpace(p.cfg.WebhookURL)
	if webhookURL == "" {
		return errors.New("slack webhook URL is required")
	}

	mention := strings.TrimSpace(p.cfg.MentionHandle)
	mentionPrefix := ""
	if mention != "" {
		mentionPrefix = mention + " "
	}

	clickURL := sanitizeSlackLinkURL(publicURL)
	if clickURL == "" {
		clickURL = "http://127.0.0.1:8787"
	}

	escapedTitle := escapeSlackMrkdwn(title)
	summaryText := fmt.Sprintf("%s[FocusGate] %s", mentionPrefix, escapedTitle)
	prefix := fmt.Sprintf("*[FocusGate]* %s", mentionPrefix)
	headerBlock := ""
	if clickURL != "" && (len(prefix)+len(clickURL)+len(escapedTitle)+7) <= 2900 {
		headerBlock = fmt.Sprintf("*[FocusGate]* %s*<%s|%s>*",
			mentionPrefix,
			clickURL,
			escapedTitle,
		)
	} else {
		headerBlock = fmt.Sprintf("*[FocusGate]* %s*%s*",
			mentionPrefix,
			escapedTitle,
		)
	}
	if len(headerBlock) > 2900 {
		headerBlock = truncateEscapedSlackText(headerBlock, 2900)
	}

	escapedBody := escapeSlackMrkdwn(strings.TrimSpace(body))
	bodySnippet := truncateEscapedSlackText(escapedBody, 2800)

	var blocks []map[string]any
	blocks = append(blocks, map[string]any{
		"type": "section",
		"text": map[string]any{
			"type":     "mrkdwn",
			"text":     headerBlock,
			"verbatim": true,
		},
	})

	if bodySnippet != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type":     "mrkdwn",
				"text":     bodySnippet,
				"verbatim": true,
			},
		})
	}

	payload := map[string]any{
		"text":   summaryText,
		"blocks": blocks,
	}

	return p.sendPayload(ctx, webhookURL, payload)
}

// SendTest sends an immediate verification alert to the given Slack webhook URL.
func (p *SlackPublisher) SendTest(ctx context.Context, webhookURL, mention string) error {
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return errors.New("webhook URL is required")
	}

	mentionPrefix := ""
	if mention = strings.TrimSpace(mention); mention != "" {
		mentionPrefix = mention + " "
	}

	payload := map[string]any{
		"text": fmt.Sprintf("%s[FocusGate] Test alert: your Slack incoming webhook integration is verified and operational!", mentionPrefix),
		"blocks": []map[string]any{
			{
				"type": "section",
				"text": map[string]any{
					"type":     "mrkdwn",
					"text":     fmt.Sprintf("*[FocusGate]* %sSlack notification channel successfully connected! :bell:", mentionPrefix),
					"verbatim": true,
				},
			},
		},
	}

	return p.sendPayload(ctx, webhookURL, payload)
}

func (p *SlackPublisher) sendPayload(ctx context.Context, webhookURL string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(data))
	if err != nil {
		return privateRequestError("slack", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "FocusGate")

	resp, err := p.client.Do(req)
	if err != nil {
		return privateRequestError("slack", err, resp)
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook HTTP %d", resp.StatusCode)
	}

	return nil
}

func escapeSlackMrkdwn(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// truncateEscapedSlackText truncates an already mrkdwn-escaped string to at most maxLen
// bytes, preserving complete escape entities and reserving space for the truncation marker.
func truncateEscapedSlackText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	const ellipsis = "\n..."
	target := maxLen - len(ellipsis)
	if target <= 0 {
		return ellipsis
	}
	truncated := truncateBytesUTF8(s, target)
	// If truncated ends inside an unclosed escape sequence like &amp; &lt; &gt;, back up before the '&'
	if lastAmp := strings.LastIndex(truncated, "&"); lastAmp != -1 {
		if !strings.Contains(truncated[lastAmp:], ";") {
			truncated = truncated[:lastAmp]
		}
	}
	return truncated + ellipsis
}

// sanitizeSlackLinkURL validates and encodes a URL to ensure it cannot break out of
// the Slack mrkdwn link format `<URL|Label>`.
func sanitizeSlackLinkURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	cleaned := strings.ReplaceAll(raw, "<", "%3C")
	cleaned = strings.ReplaceAll(cleaned, ">", "%3E")
	cleaned = strings.ReplaceAll(cleaned, "|", "%7C")
	cleaned = strings.ReplaceAll(cleaned, "\"", "%22")
	return cleaned
}
