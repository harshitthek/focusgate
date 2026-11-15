package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"focusgate/internal/app"
	"focusgate/internal/config"
	"focusgate/internal/store"
	"focusgate/internal/web"
)

func newNotifyRequest(method, url string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, url, body)
	req.RemoteAddr = "127.0.0.1:1234"
	return req
}

func TestNotifyHandlers_SettingsAndTestEndpoints(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer st.Close()

	cfg := config.Default()
	engine, err := app.NewEngine(ctx, cfg, st)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}

	server, err := web.NewServer(engine)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	// 1. Mock Slack & Matrix receiver servers
	slackHits := 0
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		slackHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer slackSrv.Close()

	matrixHits := 0
	matrixSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		matrixHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"event_id":"$123"}`))
	}))
	defer matrixSrv.Close()

	// 2. Save Settings via POST /api/notify/settings
	savePayload := map[string]any{
		"slack_enabled":      true,
		"slack_webhook_url":  slackSrv.URL,
		"slack_mention":      "<@U12345>",
		"matrix_enabled":     true,
		"matrix_webhook_url": matrixSrv.URL,
		"matrix_mention":     "@user:matrix.org",
		"only_mentions":      true,
		"mention_handles":    "@user, myname",
	}
	bodyBytes, _ := json.Marshal(savePayload)
	req := newNotifyRequest(http.MethodPost, "http://127.0.0.1:8787/api/notify/settings", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from save settings, got %d: %s", rec.Code, rec.Body.String())
	}

	var saveResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &saveResp); err != nil {
		t.Fatalf("unmarshal save response failed: %v", err)
	}
	if saveResp["only_mentions"] != true {
		t.Errorf("expected only_mentions true, got %v", saveResp["only_mentions"])
	}

	// 3. Test Slack alert via POST /api/notify/test-slack
	testReq := newNotifyRequest(http.MethodPost, "http://127.0.0.1:8787/api/notify/test-slack", strings.NewReader(`{}`))
	testRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(testRec, testReq)

	if testRec.Code != http.StatusOK {
		t.Fatalf("expected 200 from test slack, got %d: %s", testRec.Code, testRec.Body.String())
	}
	if slackHits != 1 {
		t.Errorf("expected 1 hit on mock slack server, got %d", slackHits)
	}

	// 4. Test Matrix alert via POST /api/notify/test-matrix
	mTestReq := newNotifyRequest(http.MethodPost, "http://127.0.0.1:8787/api/notify/test-matrix", strings.NewReader(`{}`))
	mTestRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(mTestRec, mTestReq)

	if mTestRec.Code != http.StatusOK {
		t.Fatalf("expected 200 from test matrix, got %d: %s", mTestRec.Code, mTestRec.Body.String())
	}
	if matrixHits != 1 {
		t.Errorf("expected 1 hit on mock matrix server, got %d", matrixHits)
	}

	// 5. Test URL validation: credentials should be rejected
	credPayload := map[string]any{
		"slack_webhook_url": "https://user:pass@hooks.slack.com/services/123",
	}
	cb, _ := json.Marshal(credPayload)
	cReq := newNotifyRequest(http.MethodPost, "http://127.0.0.1:8787/api/notify/settings", bytes.NewReader(cb))
	cRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(cRec, cReq)
	if cRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for credentials URL, got %d", cRec.Code)
	}

	// 6. Test URL validation: non-loopback HTTP should be rejected
	insecurePayload := map[string]any{
		"slack_webhook_url": "http://evil.example.com/webhook",
	}
	ib, _ := json.Marshal(insecurePayload)
	iReq := newNotifyRequest(http.MethodPost, "http://127.0.0.1:8787/api/notify/settings", bytes.NewReader(ib))
	iRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(iRec, iReq)
	if iRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for insecure non-loopback HTTP, got %d", iRec.Code)
	}

	// 7. Test clearing webhook URL with explicit empty string
	emptyURLPayload := map[string]any{
		"slack_webhook_url": "",
	}
	eb, _ := json.Marshal(emptyURLPayload)
	eReq := newNotifyRequest(http.MethodPost, "http://127.0.0.1:8787/api/notify/settings", bytes.NewReader(eb))
	eRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(eRec, eReq)
	if eRec.Code != http.StatusOK {
		t.Errorf("expected 200 when clearing webhook URL, got %d", eRec.Code)
	}
	updated, _ := engine.Store().GetNotifySettings(ctx)
	if updated.SlackWebhookURL != "" {
		t.Errorf("expected cleared Slack webhook URL, got %q", updated.SlackWebhookURL)
	}

	// 8. Test malformed JSON bodies on test endpoints
	badSlackReq := newNotifyRequest(http.MethodPost, "http://127.0.0.1:8787/api/notify/test-slack", strings.NewReader("not-json"))
	badSlackRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(badSlackRec, badSlackReq)
	if badSlackRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed JSON body in test-slack, got %d", badSlackRec.Code)
	}

	badMatrixReq := newNotifyRequest(http.MethodPost, "http://127.0.0.1:8787/api/notify/test-matrix", strings.NewReader("not-json"))
	badMatrixRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(badMatrixRec, badMatrixReq)
	if badMatrixRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed JSON body in test-matrix, got %d", badMatrixRec.Code)
	}

	// 5. Verify GET /api/profile includes slack, matrix, and notifications
	profReq := newNotifyRequest(http.MethodGet, "http://127.0.0.1:8787/api/profile", nil)
	profRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(profRec, profReq)

	if profRec.Code != http.StatusOK {
		t.Fatalf("expected 200 from get profile, got %d", profRec.Code)
	}
	var prof map[string]any
	_ = json.Unmarshal(profRec.Body.Bytes(), &prof)
	if prof["slack"] == nil || prof["matrix"] == nil || prof["notifications"] == nil {
		t.Errorf("expected slack, matrix, and notifications in profile, got: %+v", prof)
	}
}
