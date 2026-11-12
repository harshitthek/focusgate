package classify

import (
	"context"
	"fmt"
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
	GetDailyUsage(ctx context.Context, dayKey string) (int, error)
	SetDailyUsage(ctx context.Context, dayKey string, count int) error
}

// Router orchestrates Stage-1 deterministic rules and Stage-2 semantic judgment.
type Router struct {
	cfg      config.Config
	ts       *TypeSafeClient
	tracker  UsageTracker
	budgetMu sync.Mutex
	dayStamp int64
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
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	r.tracker = tracker
	if tracker != nil {
		now := time.Now()
		today := int64(now.Year()*10000 + int(now.Month())*100 + now.Day())
		dayKey := fmt.Sprintf("%08d", today)
		if count, err := tracker.GetDailyUsage(context.Background(), dayKey); err == nil && count > 0 {
			r.dayStamp = today
			r.dayCalls = count
		}
	}
}

// Classify performs semantic evaluation without applying focus-mode routing policies.
// It returns either the matching deterministic rule or computed SmartSignals.
func (r *Router) Classify(ctx context.Context, initialMode config.FocusMode, item ingest.IncomingItem) (RuleMatch, SmartSignals) {
	if match := MatchRules(r.cfg.Rules, item); match.Matched && match.Action != "smart" {
		return match, SmartSignals{}
	}

	var sig SmartSignals
	usedAPI := false
	if r.cfg.Classifier.Mode == "hybrid" && r.ts.Configured() && r.reserveDailyBudget(ctx) {
		evaluated, err := r.ts.Evaluate(ctx, r.cfg.Focus.UserContext, initialMode, item)
		if err == nil {
			sig = evaluated
			usedAPI = true
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
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	now := time.Now()
	today := int64(now.Year()*10000 + int(now.Month())*100 + now.Day())
	if r.dayStamp == today {
		used = r.dayCalls
	} else if r.tracker != nil {
		dayKey := fmt.Sprintf("%08d", today)
		if count, err := r.tracker.GetDailyUsage(context.Background(), dayKey); err == nil {
			r.dayStamp = today
			r.dayCalls = count
			used = count
		}
	}
	return used, r.cfg.Classifier.MaxDailyAPICalls
}

func (r *Router) reserveDailyBudget(ctx context.Context) bool {
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	now := time.Now()
	today := int64(now.Year()*10000 + int(now.Month())*100 + now.Day())
	if r.dayStamp != today {
		r.dayStamp = today
		r.dayCalls = 0
		if r.tracker != nil {
			dayKey := fmt.Sprintf("%08d", today)
			if count, err := r.tracker.GetDailyUsage(ctx, dayKey); err == nil {
				r.dayCalls = count
			}
		}
	}
	if r.dayCalls >= r.cfg.Classifier.MaxDailyAPICalls {
		return false
	}
	r.dayCalls++
	if r.tracker != nil {
		dayKey := fmt.Sprintf("%08d", today)
		_ = r.tracker.SetDailyUsage(ctx, dayKey, r.dayCalls)
	}
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
