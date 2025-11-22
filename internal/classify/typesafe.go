package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

// SmartSignals holds calibrated probabilities returned either by TypeSafe System One
// or by the zero-API heuristic fallback.
type SmartSignals struct {
	Engine            string  `json:"engine"` // "typesafe" | "heuristic"
	Model             string  `json:"model,omitempty"`
	ActionableProb    float64 `json:"actionable_prob"`
	TimeSensitiveProb float64 `json:"time_sensitive_prob"`
	Category          string  `json:"category"`
	CategoryProb      float64 `json:"category_prob"`
	CategoryConf      float64 `json:"category_confidence"`
	UrgencyScore      float64 `json:"urgency_score"`      // 0..3
	UrgencyConf       float64 `json:"urgency_confidence"` // 0..1
	InputTokens       int     `json:"input_tokens,omitempty"`
	OutputTokens      int     `json:"output_tokens,omitempty"`
}

// TypeSafeClient calls POST /v1/systemone with zero local model RAM.
type TypeSafeClient struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// NewTypeSafeClient builds a reusable HTTP client for TypeSafe System One.
func NewTypeSafeClient(cfg config.ClassifierConfig) *TypeSafeClient {
	return &TypeSafeClient{
		BaseURL: strings.TrimRight(cfg.TypeSafeBaseURL, "/"),
		APIKey:  strings.TrimSpace(cfg.TypeSafeAPIKey),
		Model:   cfg.Model,
		HTTPClient: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
	}
}

func (c *TypeSafeClient) Configured() bool {
	return c != nil && c.APIKey != ""
}

type systemOneRequest struct {
	Model     string                 `json:"model"`
	State     map[string]any         `json:"state"`
	Questions map[string]interface{} `json:"questions"`
}

type systemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type noulAnswer struct {
	Type string  `json:"type"`
	Noul float64 `json:"noul"`
}

type choiceAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type scoreAnswer struct {
	Type       string  `json:"type"`
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
}

// Evaluate sends one parallel System One request asking 4 orthogonal questions
// (actionability, time sensitivity, category, urgency) about the item.
func (c *TypeSafeClient) Evaluate(ctx context.Context, userContext string, mode config.FocusMode, item ingest.IncomingItem) (SmartSignals, error) {
	if !c.Configured() {
		return SmartSignals{}, fmt.Errorf("typesafe api key not configured")
	}

	reqBody := systemOneRequest{
		Model: c.Model,
		State: map[string]any{
			"user_context": userContext,
			"focus_mode":   string(mode),
			"item": map[string]any{
				"source_type": item.SourceType,
				"source_name": item.SourceName,
				"sender":      item.Sender,
				"title":       item.Title,
				"body":        ingest.TruncateRunes(item.Body, 900),
				"reason":      item.Reason,
				"tag":         item.Tag,
			},
		},
		Questions: map[string]interface{}{
			"is_actionable": map[string]any{
				"type":         "noul",
				"instructions": "Does `item` require direct action, a response, or a decision from the user described in `user_context`, rather than being passive reading, newsletter content, or automated status noise?",
				"criteria": map[string]string{
					"true":  "A person or system directly asks for review, reply, fix, security action, or personal attention.",
					"false": "Passive reading, release notes, newsletters, marketing, or routine CI logs.",
				},
			},
			"is_time_sensitive": map[string]any{
				"type":         "noul",
				"instructions": "Would delaying reading `item` by 4 hours cause real harm, missed deadlines, broken production systems, security risk, or time-critical consequences?",
			},
			"category": map[string]any{
				"type":         "choice",
				"instructions": "Which category best describes `item`?",
				"criteria": map[string]string{
					"security_or_outage":       "Production outage, service downtime, vulnerability, or urgent account security warning.",
					"direct_person_or_review":  "Direct message from a human, code review request, assignment, or @mention needing response.",
					"work_or_dev_update":       "Project update, issue comment, build status, or release relevant to work without immediate crisis.",
					"news_or_reading":          "Blog post, article, RSS feed story, or general tech news for reading later.",
					"promo_or_noise":           "Promotional email, spam, marketing digest, or automated boilerplate noise.",
				},
			},
			"urgency": map[string]any{
				"type":         "score",
				"instructions": "Rate the urgency of `item` for the user in `user_context`.",
				"criteria": []string{
					"Low-value promo, marketing, or irrelevant noise that can be archived.",
					"Useful reading or non-urgent update suitable for a scheduled digest.",
					"Actionable work or direct update worth notifying during normal focus mode.",
					"Critical outage, security alert, or urgent human blocker that should break through deep work.",
				},
			},
		},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return SmartSignals{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/systemone", bytes.NewReader(payload))
	if err != nil {
		return SmartSignals{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return SmartSignals{}, fmt.Errorf("typesafe systemone request: %w", err)
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return SmartSignals{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SmartSignals{}, fmt.Errorf("typesafe API HTTP %d: %s", resp.StatusCode, ingest.TruncateRunes(string(rawResp), 240))
	}

	var decoded systemOneResponse
	if err := json.Unmarshal(rawResp, &decoded); err != nil {
		return SmartSignals{}, fmt.Errorf("decode typesafe response: %w", err)
	}

	var actionable noulAnswer
	var timeSens noulAnswer
	var cat choiceAnswer
	var urg scoreAnswer
	if err := json.Unmarshal(decoded.Answers["is_actionable"], &actionable); err != nil {
		return SmartSignals{}, fmt.Errorf("decode is_actionable: %w", err)
	}
	if err := json.Unmarshal(decoded.Answers["is_time_sensitive"], &timeSens); err != nil {
		return SmartSignals{}, fmt.Errorf("decode is_time_sensitive: %w", err)
	}
	if err := json.Unmarshal(decoded.Answers["category"], &cat); err != nil {
		return SmartSignals{}, fmt.Errorf("decode category: %w", err)
	}
	if err := json.Unmarshal(decoded.Answers["urgency"], &urg); err != nil {
		return SmartSignals{}, fmt.Errorf("decode urgency: %w", err)
	}

	return SmartSignals{
		Engine:            "typesafe",
		Model:             decoded.Model,
		ActionableProb:    actionable.Noul,
		TimeSensitiveProb: timeSens.Noul,
		Category:          cat.Choice,
		CategoryProb:      cat.Probabilities[cat.Choice],
		CategoryConf:      cat.Confidence,
		UrgencyScore:      urg.Score,
		UrgencyConf:       urg.Confidence,
		InputTokens:       decoded.Usage.InputTokens,
		OutputTokens:      decoded.Usage.OutputTokens,
	}, nil
}

var (
	crisisKeywordRE = regexp.MustCompile(`(?i)\b(outage|sev1|production down|security alert|breach|unauthorized)\b|\bcve-`)
	urgentKeywordRE = regexp.MustCompile(`(?i)\b(urgent|asap|deadline|otp|verification code|invoice due)\b`)
	promoKeywordRE  = regexp.MustCompile(`(?i)\b(unsubscribe|sale|discount|newsletter|webinar)\b`)
)

// HeuristicEvaluate provides a deterministic zero-RAM, zero-network fallback
// when TypeSafe API is unconfigured, disabled, or daily quota is reached.
func HeuristicEvaluate(item ingest.IncomingItem) SmartSignals {
	text := item.Title + " " + item.Body + " " + item.Reason
	sig := SmartSignals{
		Engine:            "heuristic",
		ActionableProb:    0.35,
		TimeSensitiveProb: 0.20,
		Category:          "work_or_dev_update",
		CategoryProb:      0.60,
		CategoryConf:      0.50,
		UrgencyScore:      1.0,
		UrgencyConf:       0.55,
	}

	switch {
	case crisisKeywordRE.MatchString(text):
		sig.Category = "security_or_outage"
		sig.CategoryProb = 0.90
		sig.CategoryConf = 0.85
		sig.ActionableProb = 0.92
		sig.TimeSensitiveProb = 0.92
		sig.UrgencyScore = 2.85
		sig.UrgencyConf = 0.85
	case item.SourceType == "github" && hasReason(item.Reason, "review_requested", "mention", "assign"):
		sig.Category = "direct_person_or_review"
		sig.CategoryProb = 0.85
		sig.CategoryConf = 0.80
		sig.ActionableProb = 0.85
		sig.TimeSensitiveProb = 0.45
		sig.UrgencyScore = 2.0
		sig.UrgencyConf = 0.75
	case urgentKeywordRE.MatchString(text):
		sig.Category = "direct_person_or_review"
		sig.CategoryProb = 0.78
		sig.CategoryConf = 0.72
		sig.ActionableProb = 0.82
		sig.TimeSensitiveProb = 0.78
		sig.UrgencyScore = 2.4
		sig.UrgencyConf = 0.70
	case item.SourceType == "rss":
		sig.Category = "news_or_reading"
		sig.CategoryProb = 0.85
		sig.CategoryConf = 0.80
		sig.ActionableProb = 0.10
		sig.TimeSensitiveProb = 0.05
		sig.UrgencyScore = 0.9
		sig.UrgencyConf = 0.80
	case promoKeywordRE.MatchString(text):
		sig.Category = "promo_or_noise"
		sig.CategoryProb = 0.88
		sig.CategoryConf = 0.82
		sig.ActionableProb = 0.05
		sig.TimeSensitiveProb = 0.02
		sig.UrgencyScore = 0.2
		sig.UrgencyConf = 0.80
	}
	return sig
}

func hasReason(actual string, targets ...string) bool {
	actual = strings.TrimSpace(strings.ToLower(actual))
	for _, t := range targets {
		if actual == t {
			return true
		}
	}
	return false
}
