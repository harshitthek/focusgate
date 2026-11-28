// Package classify_test tests the hybrid rule and ML classifier.
package classify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
		Body:       "Manage preferences or unsubscribe from optional messages.",
	})
	if urgent.Category != "direct_person_or_review" || urgent.UrgencyScore < 2.0 {
		t.Fatalf("expected OTP to score urgent, got %+v", urgent)
	}
}

type classifierTransportFunc func(*http.Request) (*http.Response, error)

func (fn classifierTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestTypeSafeRejectsUnsafeInitialEndpointBeforeSending(t *testing.T) {
	for _, raw := range []string{"http://external.test", "http://localhost.example", "http://192.0.2.1", "https://user:password@example.test", "https://example.test/?token=secret", "https://example.test/#fragment", ""} {
		client := NewTypeSafeClient(config.ClassifierConfig{TypeSafeBaseURL: raw, TypeSafeAPIKey: "test-key"})
		client.HTTPClient.Transport = classifierTransportFunc(func(*http.Request) (*http.Response, error) {
			t.Errorf("unsafe classifier URL %q reached the transport", raw)
			return nil, errors.New("unexpected request")
		})
		if _, err := client.Evaluate(context.Background(), "", config.ModeNormal, ingest.IncomingItem{}); err == nil {
			t.Errorf("classifier accepted %q", raw)
		}
	}
	cases := map[string]string{
		"http://localhost:9000/mock": "http://127.0.0.1:9000/mock/v1/systemone",
		"http://[::1]:9000":          "http://[::1]:9000/v1/systemone",
		"https://external.test/api":  "https://external.test/api/v1/systemone",
	}
	for base, want := range cases {
		got, err := (&TypeSafeClient{BaseURL: base}).endpoint()
		if err != nil || got != want {
			t.Errorf("endpoint(%q) = %q, %v; want %q", base, got, err, want)
		}
	}
}

func TestClassifierHTTPErrorOmitsProviderResponse(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private reflected token: "+r.Header.Get("Authorization"), http.StatusBadGateway)
	}))
	defer mock.Close()
	client := NewTypeSafeClient(config.ClassifierConfig{TypeSafeBaseURL: mock.URL, TypeSafeAPIKey: "test-key"})
	_, err := client.Evaluate(context.Background(), "", config.ModeNormal, ingest.IncomingItem{})
	if err == nil || err.Error() != "typesafe API HTTP 502" {
		t.Fatalf("unexpected classifier HTTP error: %v", err)
	}
}

func TestHeuristicCrisisPrecedenceAndIMAPConfCap(t *testing.T) {
	// Crisis words remain reviewable even when the same message contains marketing words.
	saleCrisis := HeuristicEvaluate(ingest.IncomingItem{
		SourceType: "imap",
		Title:      "Massive security sale: protect against breach",
		Body:       "Get 50% discount on anti-outage software.",
	})
	if saleCrisis.Category != "security_or_outage" || saleCrisis.UrgencyScore < 2.0 {
		t.Fatalf("expected crisis precedence over promo keywords, got category=%q urgency=%.2f", saleCrisis.Category, saleCrisis.UrgencyScore)
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

func (m *memoryUsageTracker) GetUserDailyUsage(_ context.Context, dayKey string, _ int64) (int, error) {
	return m.counts[dayKey], nil
}

func (m *memoryUsageTracker) ReserveDailyUsage(_ context.Context, dayKey string, _ int64, limit, _ int) (bool, error) {
	if m.counts[dayKey] >= limit {
		return false, nil
	}
	m.counts[dayKey]++
	return true, nil
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

func TestTypeSafeRedirectOriginsAndPlainPromo(t *testing.T) {
	var destinationHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationHits.Add(1); w.WriteHeader(http.StatusOK) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/ok", http.StatusTemporaryRedirect)
		case "/cross":
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	client := NewTypeSafeClient(config.ClassifierConfig{TimeoutSeconds: 1}).HTTPClient
	response, err := client.Get(server.URL + "/same")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response, err := client.Get(server.URL + "/cross"); err == nil {
		response.Body.Close()
		t.Fatal("cross-origin redirect allowed")
	}
	if destinationHits.Load() != 0 {
		t.Fatal("redirect reached a different origin")
	}
	initial, _ := http.NewRequest(http.MethodPost, "https://example.test/api", nil)
	downgrade, _ := http.NewRequest(http.MethodPost, "http://example.test/api", nil)
	if err := client.CheckRedirect(downgrade, []*http.Request{initial}); err == nil {
		t.Fatal("HTTPS downgrade allowed")
	}
	plainPromo := HeuristicEvaluate(ingest.IncomingItem{SourceType: "imap", Title: "Newsletter sale", Body: "Discount on books"})
	if plainPromo.Category != "promo_or_noise" {
		t.Fatalf("plain promotion changed category: %+v", plainPromo)
	}
}

func TestTypeSafeCategoryProbabilityDistinguishesNullAndZero(t *testing.T) {
	for _, tc := range []struct {
		wire  string
		valid bool
	}{{"null", false}, {"0", true}} {
		t.Run(tc.wire, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"model":"jev","answers":{
                    "is_actionable":{"type":"noul","noul":0},
                    "is_time_sensitive":{"type":"noul","noul":0},
                    "category":{"type":"choice","choice":"news_or_reading","confidence":1,"probabilities":{"news_or_reading":` + tc.wire + `}},
                    "urgency":{"type":"score","score":0,"confidence":1}}}`))
			}))
			defer upstream.Close()
			client := NewTypeSafeClient(config.ClassifierConfig{TypeSafeBaseURL: upstream.URL, TypeSafeAPIKey: "key"})
			actual, err := client.Evaluate(context.Background(), "", config.ModeNormal, ingest.IncomingItem{Title: "article"})
			if (err == nil) != tc.valid || err == nil && actual.CategoryProb != 0 {
				t.Fatalf("probability %s decoded as %+v with error %v", tc.wire, actual, err)
			}
		})
	}
}
