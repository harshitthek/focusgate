package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type genericWebhookPayload struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Message string `json:"message"`
	Text    string `json:"text"`
	Sender  string `json:"sender"`
	From    string `json:"from"`
	URL     string `json:"url"`
	Link    string `json:"link"`
	Tag     string `json:"tag"`
	Reason  string `json:"reason"`
}

// VerifyWebhookAuth checks either Bearer / X-Webhook-Secret or GitHub
// X-Hub-Signature-256 against the configured shared secret.
func VerifyWebhookAuth(secret, authHeader, tokenHeader, ghSignature string, body []byte) bool {
	if secret == "" {
		return true
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if authHeader != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(secret)) == 1 {
		return true
	}
	if tokenHeader != "" && subtle.ConstantTimeCompare([]byte(tokenHeader), []byte(secret)) == 1 {
		return true
	}
	if strings.HasPrefix(ghSignature, "sha256=") {
		sigHex := strings.TrimPrefix(ghSignature, "sha256=")
		sigBytes, err := hex.DecodeString(sigHex)
		if err != nil {
			return false
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := mac.Sum(nil)
		return hmac.Equal(sigBytes, expected)
	}
	return false
}

// ParseWebhook converts a JSON or plain-text webhook payload into an IncomingItem.
// When payloads omit an explicit ID, a minute-bucketed content hash prevents
// recurring alerts from being deduplicated forever while still ignoring immediate retries.
func ParseWebhook(sourceName, ghEvent, ghDelivery string, body []byte, maxSnippet int) (IncomingItem, error) {
	now := time.Now().UTC()
	if ghEvent != "" {
		if item, ok := tryParseGitHubWebhook(sourceName, ghEvent, ghDelivery, body, maxSnippet, now); ok {
			return item, nil
		}
	}

	var p genericWebhookPayload
	if err := json.Unmarshal(body, &p); err == nil && (p.Title != "" || p.Subject != "" || p.Body != "" || p.Message != "" || p.Text != "") {
		title := firstNonEmpty(p.Title, p.Subject, sourceName+" event")
		text := firstNonEmpty(p.Body, p.Message, p.Text)
		sender := firstNonEmpty(p.Sender, p.From, sourceName)
		link := firstNonEmpty(p.URL, p.Link)
		extID := strings.TrimSpace(p.ID)
		if extID == "" {
			extID = fallbackWebhookID(sourceName, body, now)
		} else {
			extID = fmt.Sprintf("wh:%s:%s", sourceName, extID)
		}
		item := IncomingItem{
			ExternalID:  extID,
			SourceType:  "webhook",
			SourceName:  sourceName,
			Title:       title,
			Body:        StripHTML(text),
			Sender:      sender,
			URL:         link,
			Tag:         p.Tag,
			Reason:      p.Reason,
			PublishedAt: now,
		}
		item.Normalize(maxSnippet)
		return item, nil
	}

	text := strings.TrimSpace(string(body))
	if text == "" {
		return IncomingItem{}, fmt.Errorf("empty webhook payload")
	}
	item := IncomingItem{
		ExternalID:  fallbackWebhookID(sourceName, body, now),
		SourceType:  "webhook",
		SourceName:  sourceName,
		Title:       fmt.Sprintf("Webhook: %s", sourceName),
		Body:        StripHTML(text),
		Sender:      sourceName,
		PublishedAt: now,
	}
	item.Normalize(maxSnippet)
	return item, nil
}

func tryParseGitHubWebhook(sourceName, event, deliveryID string, body []byte, maxSnippet int, now time.Time) (IncomingItem, bool) {
	var raw struct {
		Action string `json:"action"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Repository struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
		Issue struct {
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		} `json:"issue"`
		PullRequest struct {
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		} `json:"pull_request"`
		Comment struct {
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Repository.FullName == "" {
		return IncomingItem{}, false
	}
	title := firstNonEmpty(raw.PullRequest.Title, raw.Issue.Title, fmt.Sprintf("%s %s", event, raw.Action))
	text := firstNonEmpty(raw.Comment.Body, raw.PullRequest.Body, raw.Issue.Body, fmt.Sprintf("%s %s by %s", event, raw.Action, raw.Sender.Login))
	link := firstNonEmpty(raw.Comment.HTMLURL, raw.PullRequest.HTMLURL, raw.Issue.HTMLURL, raw.Repository.HTMLURL)
	extID := strings.TrimSpace(deliveryID)
	if extID != "" {
		extID = "gh-webhook:" + extID
	} else {
		extID = fallbackWebhookID(raw.Repository.FullName, body, now)
	}
	item := IncomingItem{
		ExternalID:  extID,
		SourceType:  "github",
		SourceName:  firstNonEmpty(raw.Repository.FullName, sourceName),
		Title:       fmt.Sprintf("[%s] %s", raw.Repository.FullName, title),
		Body:        StripHTML(text),
		Sender:      raw.Sender.Login,
		URL:         link,
		Tag:         event,
		Reason:      raw.Action,
		PublishedAt: now,
	}
	item.Normalize(maxSnippet)
	return item, true
}

func fallbackWebhookID(sourceName string, rawBody []byte, now time.Time) string {
	h := sha256.New()
	_, _ = h.Write([]byte(sourceName))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(now.UTC().Format("200601021504")))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write(rawBody)
	return "wh:" + hex.EncodeToString(h.Sum(nil)[:16])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
