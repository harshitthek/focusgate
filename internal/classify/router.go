package classify

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

// Decision is the final focus-aware routing outcome stored and acted upon.
type Decision struct {
	Route      string        `json:"route"` // "push_now" | "queue_digest" | "archive"
	Priority   int           `json:"priority"`
	Confidence float64       `json:"confidence"`
	Uncertain  bool          `json:"uncertain"`
	Source     string        `json:"source"` // "rule" | "typesafe" | "heuristic"
	RuleName   string        `json:"rule_name,omitempty"`
	Reason     string        `json:"reason"`
	Signals    *SmartSignals `json:"signals,omitempty"`
}

// UsageTracker persists daily TypeSafe API call counts across daemon restarts.
type UsageTracker interface {
	GetUserDailyUsage(ctx context.Context, dayKey string, userID int64) (int, error)
	ReserveDailyUsage(ctx context.Context, dayKey string, userID int64, globalLimit, userLimit int) (bool, error)
}

// Router orchestrates Stage-1 deterministic rules and Stage-2 semantic judgment.
type Router struct {
	cfg      config.Config
	ts       *TypeSafeClient
	tracker  UsageTracker
	budgetMu sync.Mutex
	dayStamp string
	dayCalls int
}

// NewRouter constructs a focus-aware classification router.
func NewRouter(cfg config.Config) *Router {
	return &Router{
		cfg: cfg,
		ts:  NewTypeSafeClient(cfg.Classifier),
	}
}

// SetUsageTracker registers a persistent storage backend for tracking daily API usage.
func (r *Router) SetUsageTracker(tracker UsageTracker) {
	r.tracker = tracker
}

// Classify performs semantic evaluation without applying focus-mode routing policies.
// It returns either the matching deterministic rule or computed SmartSignals.
func (r *Router) Classify(ctx context.Context, initialMode config.FocusMode, item ingest.IncomingItem) (RuleMatch, SmartSignals) {
	if match := MatchRules(r.cfg.Rules, item); match.Matched && match.Action != "smart" {
		return match, SmartSignals{}
	}

	var sig SmartSignals
	usedAPI := false
	if r.cfg.Classifier.Mode == "hybrid" && r.ts.Configured() && r.reserveDailyBudget(ctx, item.UserID) {
		evaluated, err := r.ts.Evaluate(ctx, r.cfg.Focus.UserContext, initialMode, item)
		if err == nil {
			sig = evaluated
			usedAPI = true
		} else {
			log.Printf("typesafe evaluate error for item %q, using heuristic fallback: %v", item.ExternalID, err)
		}
	}
	if !usedAPI {
		sig = HeuristicEvaluate(item)
	}
	return RuleMatch{}, sig
}

// Route applies the given focus mode to the classification results.
func (r *Router) Route(mode config.FocusMode, match RuleMatch, sig SmartSignals) Decision {
	if match.Matched && match.Action != "smart" {
		return applyModeToRule(mode, match)
	}
	return applyFocusPolicy(mode, sig)
}

// Evaluate classifies and routes an IncomingItem under the active FocusMode.
func (r *Router) Evaluate(ctx context.Context, mode config.FocusMode, item ingest.IncomingItem) Decision {
	match, sig := r.Classify(ctx, mode, item)
	return r.Route(mode, match, sig)
}

// DailyAPIUsage returns today's consumed TypeSafe API call count and configured daily cap.
func (r *Router) DailyAPIUsage() (used int, limit int) {
	return r.DailyUserAPIUsage(context.Background(), 1)
}

// DailyUserAPIUsage reports the current account's TypeSafe allowance.
func (r *Router) DailyUserAPIUsage(ctx context.Context, userID int64) (used int, limit int) {
	limit = r.cfg.Classifier.MaxDailyAPICalls
	if userID > 1 {
		limit = min(limit, r.cfg.Classifier.MaxDailyAPICallsPerUser)
	}
	today := time.Now().Format("20060102")
	if r.tracker != nil {
		used, _ = r.tracker.GetUserDailyUsage(ctx, today, userID)
		return used, limit
	}
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	if r.dayStamp == today {
		used = r.dayCalls
	}
	return used, limit
}

func (r *Router) reserveDailyBudget(ctx context.Context, userID int64) bool {
	today := time.Now().Format("20060102")
	limit := r.cfg.Classifier.MaxDailyAPICalls
	if r.tracker != nil {
		ok, err := r.tracker.ReserveDailyUsage(ctx, today, userID, limit, r.cfg.Classifier.MaxDailyAPICallsPerUser)
		if err != nil {
			log.Printf("quota tracker error: %v, falling back to heuristics", err)
		}
		return ok && err == nil
	}
	if userID > 1 {
		return false
	}
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	if r.dayStamp != today {
		r.dayStamp = today
		r.dayCalls = 0
	}
	if r.dayCalls >= limit {
		return false
	}
	r.dayCalls++
	return true
}

func applyModeToRule(mode config.FocusMode, match RuleMatch) Decision {
	route := match.Action
	prio := match.Priority
	reason := fmt.Sprintf("Matched rule %q (%s)", match.RuleName, match.Action)

	if route == "push_now" {
		switch mode {
		case config.ModeDeepWork:
			if prio < 5 {
				route = "queue_digest"
				reason = fmt.Sprintf("Held rule %q for digest during deep_work (priority %d < 5)", match.RuleName, prio)
			}
		case config.ModeDND:
			if prio < 5 {
				route = "queue_digest"
				reason = fmt.Sprintf("Held rule %q for digest during dnd", match.RuleName)
			}
		}
	}

	return Decision{
		Route:      route,
		Priority:   prio,
		Confidence: 1.0,
		Source:     "rule",
		RuleName:   match.RuleName,
		Reason:     reason,
	}
}

// applyFocusPolicy keeps focus policy purely in deterministic code over calibrated
// probabilities, matching TypeSafe's confidence-gating patterns.
func applyFocusPolicy(mode config.FocusMode, sig SmartSignals) Decision {
	conf := math.Min(sig.UrgencyConf, sig.CategoryConf)
	uncertain := conf < 0.45

	if sig.Category == "promo_or_noise" && sig.CategoryProb >= 0.78 && sig.ActionableProb < 0.30 {
		return Decision{
			Route:      "archive",
			Priority:   1,
			Confidence: sig.CategoryProb,
			Source:     sig.Engine,
			Reason:     fmt.Sprintf("Archived noise (%s p=%.2f, actionable=%.2f)", sig.Category, sig.CategoryProb, sig.ActionableProb),
			Signals:    &sig,
		}
	}

	switch mode {
	case config.ModeDND:
		// Only severe outages/security threats with clear certainty pierce DND.
		if sig.Category == "security_or_outage" && sig.CategoryProb >= 0.75 && sig.UrgencyScore >= 2.3 && sig.UrgencyConf >= 0.55 {
			return Decision{
				Route:      "push_now",
				Priority:   5,
				Confidence: conf,
				Source:     sig.Engine,
				Reason:     fmt.Sprintf("DND breakthrough: %s (score=%.2f, p=%.2f)", sig.Category, sig.UrgencyScore, sig.CategoryProb),
				Signals:    &sig,
			}
		}
		return Decision{
			Route:      "queue_digest",
			Priority:   2,
			Confidence: conf,
			Uncertain:  uncertain,
			Source:     sig.Engine,
			Reason:     fmt.Sprintf("Queued during dnd (%s, urgency=%.2f)", sig.Category, sig.UrgencyScore),
			Signals:    &sig,
		}

	case config.ModeDeepWork:
		isCrisis := sig.Category == "security_or_outage" && sig.CategoryProb >= 0.72
		isUrgentBreakthrough := sig.UrgencyScore >= 2.35 && sig.TimeSensitiveProb >= 0.72 && sig.UrgencyConf >= 0.55
		if (isCrisis && sig.UrgencyScore >= 2.0) || isUrgentBreakthrough {
			return Decision{
				Route:      "push_now",
				Priority:   5,
				Confidence: conf,
				Source:     sig.Engine,
				Reason:     fmt.Sprintf("Deep work breakthrough: %s (urgency=%.2f, time_sens=%.2f)", sig.Category, sig.UrgencyScore, sig.TimeSensitiveProb),
				Signals:    &sig,
			}
		}
		return Decision{
			Route:      "queue_digest",
			Priority:   scoreToPriority(sig.UrgencyScore),
			Confidence: conf,
			Uncertain:  uncertain,
			Source:     sig.Engine,
			Reason:     fmt.Sprintf("Deferred for digest in deep_work (%s, urgency=%.2f, conf=%.2f)", sig.Category, sig.UrgencyScore, conf),
			Signals:    &sig,
		}

	default: // ModeNormal
		shouldPush := sig.UrgencyScore >= 1.75 ||
			(sig.ActionableProb >= 0.70 && sig.Category != "news_or_reading" && sig.UrgencyScore >= 1.35)
		if shouldPush && !uncertain {
			return Decision{
				Route:      "push_now",
				Priority:   scoreToPriority(sig.UrgencyScore),
				Confidence: conf,
				Source:     sig.Engine,
				Reason:     fmt.Sprintf("Pushed in normal mode (%s, urgency=%.2f, actionable=%.2f)", sig.Category, sig.UrgencyScore, sig.ActionableProb),
				Signals:    &sig,
			}
		}
		if sig.UrgencyScore < 0.55 && sig.ActionableProb < 0.25 && sig.Category != "news_or_reading" {
			return Decision{
				Route:      "archive",
				Priority:   1,
				Confidence: conf,
				Source:     sig.Engine,
				Reason:     fmt.Sprintf("Low signal archived (%s, urgency=%.2f)", sig.Category, sig.UrgencyScore),
				Signals:    &sig,
			}
		}
		return Decision{
			Route:      "queue_digest",
			Priority:   scoreToPriority(sig.UrgencyScore),
			Confidence: conf,
			Uncertain:  uncertain,
			Source:     sig.Engine,
			Reason:     fmt.Sprintf("Queued for digest (%s, urgency=%.2f, conf=%.2f)", sig.Category, sig.UrgencyScore, conf),
			Signals:    &sig,
		}
	}
}

func scoreToPriority(score float64) int {
	switch {
	case score >= 2.5:
		return 5
	case score >= 1.9:
		return 4
	case score >= 1.2:
		return 3
	case score >= 0.6:
		return 2
	default:
		return 1
	}
}
