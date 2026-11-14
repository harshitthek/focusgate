// Package classify_test tests the hybrid rule and ML classifier.
package classify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

func TestRouter_RulesAndFocusModes(t *testing.T) {
	cfg := config.Default()
	cfg.Classifier.Mode = "rules_only"
	cfg.Rules = []config.RuleConfig{
		{Name: "Crisis", Contains: "production down", Action: "push_now", Priority: 5},
		{Name: "PR Review", SourceType: "github", GitHubReason: "review_requested", Action: "push_now", Priority: 4},
		{Name: "Newsletter", SourceType: "rss", Action: "queue_digest", Priority: 2},
	}

	router := NewRouter(cfg)
	ctx := context.Background()

	// Priority 5 breaks through Deep Work.
	crisis := ingest.IncomingItem{SourceType: "webhook", Title: "ALERT: production down"}
	dec := router.Evaluate(ctx, config.ModeDeepWork, crisis)
	if dec.Route != "push_now" || dec.Priority != 5 {
		t.Fatalf("expected crisis to push_now in deep_work, got %+v", dec)
	}

	// Priority 4 PR review pushes in normal mode, but queues for digest in deep_work.
	pr := ingest.IncomingItem{SourceType: "github", Reason: "review_requested", Title: "Fix cache"}
	if d := router.Evaluate(ctx, config.ModeNormal, pr); d.Route != "push_now" {
		t.Fatalf("expected PR review to push in normal mode, got %+v", d)
	}
	if d := router.Evaluate(ctx, config.ModeDeepWork, pr); d.Route != "queue_digest" {
		t.Fatalf("expected PR review to defer in deep_work, got %+v", d)
	}

	// RSS newsletter queues cleanly.
	rss := ingest.IncomingItem{SourceType: "rss", Title: "Weekly Roundup"}
	if d := router.Evaluate(ctx, config.ModeNormal, rss); d.Route != "queue_digest" {
		t.Fatalf("expected RSS to queue_digest, got %+v", d)
	}
}

func TestRouter_TypeSafeSystemOneIntegration(t *testing.T) {
	mockTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-1.14.0",
			"answers": map[string]any{
				"is_actionable":     map[string]any{"type": "noul", "noul": 0.91},
				"is_time_sensitive": map[string]any{"type": "noul", "noul": 0.88},
				"category": map[string]any{
					"type":       "choice",
					"choice":     "security_or_outage",
					"confidence": 0.86,
					"probabilities": map[string]float64{
						"security_or_outage": 0.89,
						"work_or_dev_update": 0.11,
					},
				},
				"urgency": map[string]any{
					"type":       "score",
					"score":      2.75,
					"confidence": 0.84,
				},
			},
			"usage": map[string]int{"input_tokens": 110, "output_tokens": 16},
		})
	}))
	defer mockTS.Close()

	cfg := config.Default()
	cfg.Classifier.Mode = "hybrid"
	cfg.Classifier.TypeSafeAPIKey = "test-key"
	cfg.Classifier.TypeSafeBaseURL = mockTS.URL

	router := NewRouter(cfg)
	dec := router.Evaluate(context.Background(), config.ModeDeepWork, ingest.IncomingItem{
		SourceType: "imap",
		SourceName: "Alerts",
		Title:      "Database primary unreachable",
		Body:       "Health check failing in ap-south-1",
	})
	if dec.Route != "push_now" || dec.Source != "typesafe" || dec.Priority != 5 {
		t.Fatalf("expected TypeSafe deep_work breakthrough push_now, got %+v", dec)
	}
}

func TestHeuristicWordBoundaries(t *testing.T) {
	wholesale := HeuristicEvaluate(ingest.IncomingItem{
		SourceType: "imap",
		Title:      "Quarterly wholesale inventory report",
		Body:       "Updated unit counts for Q4.",
	})
	if wholesale.Category == "promo_or_noise" {
		t.Fatalf("word-boundary check failed: 'wholesale' matched 'sale': %+v", wholesale)
	}

	urgent := HeuristicEvaluate(ingest.IncomingItem{
		SourceType: "imap",
		Title:      "Your OTP code expires in 2 minutes",
	})
	if urgent.Category != "direct_person_or_review" || urgent.UrgencyScore < 2.0 {
		t.Fatalf("expected OTP to score urgent, got %+v", urgent)
	}
}

func TestHeuristicPromoPrecedenceAndIMAPConfCap(t *testing.T) {
	// Promo keywords must take precedence over crisis keywords
	saleCrisis := HeuristicEvaluate(ingest.IncomingItem{
		SourceType: "imap",
		Title:      "Massive security sale: protect against breach",
		Body:       "Get 50% discount on anti-outage software.",
	})
	if saleCrisis.Category != "promo_or_noise" || saleCrisis.UrgencyScore > 0.5 {
		t.Fatalf("expected promo precedence over crisis keywords, got category=%q urgency=%.2f", saleCrisis.Category, saleCrisis.UrgencyScore)
	}

	// IMAP non-promo crisis message has UrgencyConf capped below 0.55
	oktaBreach := HeuristicEvaluate(ingest.IncomingItem{
		SourceType: "imap",
		Title:      "Security alert: unauthorized access breach detected",
		Body:       "Details on the recent incident.",
	})
	if oktaBreach.Category != "security_or_outage" {
		t.Fatalf("expected security_or_outage, got %q", oktaBreach.Category)
	}
	if oktaBreach.UrgencyConf >= 0.55 {
		t.Fatalf("expected IMAP crisis UrgencyConf < 0.55, got %.2f", oktaBreach.UrgencyConf)
	}
}

type memoryUsageTracker struct {
	counts map[string]int
}

func (m *memoryUsageTracker) GetDailyUsage(_ context.Context, dayKey string) (int, error) {
	return m.counts[dayKey], nil
}

func (m *memoryUsageTracker) SetDailyUsage(_ context.Context, dayKey string, count int) error {
	if m.counts == nil {
		m.counts = make(map[string]int)
	}
	m.counts[dayKey] = count
	return nil
}

func TestRouter_TypeSafeInvalidPayloadFallsBackToHeuristic(t *testing.T) {
	mockTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return invalid score > 3.0
		w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {
				"is_actionable": {"type": "noul", "noul": 0.85},
				"is_time_sensitive": {"type": "noul", "noul": 0.75},
				"category": {
					"type": "choice",
					"choice": "security_or_outage",
					"confidence": 0.90,
					"probabilities": {"security_or_outage": 0.90}
				},
				"urgency": {"type": "score", "score": 9.9, "confidence": 0.80}
			}
		}`))
	}))
	defer mockTS.Close()

	cfg := config.Default()
	cfg.Classifier.Mode = "hybrid"
	cfg.Classifier.TypeSafeAPIKey = "test-key"
	cfg.Classifier.TypeSafeBaseURL = mockTS.URL

	tracker := &memoryUsageTracker{counts: make(map[string]int)}
	router := NewRouter(cfg)
	router.SetUsageTracker(tracker)

	dec := router.Evaluate(context.Background(), config.ModeNormal, ingest.IncomingItem{
		SourceType: "imap",
		Title:      "Simple message",
		Body:       "hello",
	})
	// Invalid score must cause fallback to heuristic
	if dec.Source != "heuristic" {
		t.Fatalf("expected heuristic fallback for invalid score, got %q", dec.Source)
	}
	used, _ := router.DailyAPIUsage()
	if used != 1 {
		t.Fatalf("expected daily usage counter 1, got %d", used)
	}
}
