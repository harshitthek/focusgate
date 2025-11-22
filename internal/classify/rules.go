package classify

import (
	"strings"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

// RuleMatch captures the result of Stage-1 deterministic rule evaluation.
type RuleMatch struct {
	Matched  bool
	RuleName string
	Action   string
	Priority int
}

// MatchRules evaluates configured rules in order and returns the first match.
func MatchRules(rules []config.RuleConfig, item ingest.IncomingItem) RuleMatch {
	haystack := strings.ToLower(item.Title + "\n" + item.Body + "\n" + item.Sender)
	for _, r := range rules {
		if !ruleMatches(r, item, haystack) {
			continue
		}
		prio := r.Priority
		if prio == 0 {
			prio = defaultPriorityForAction(r.Action)
		}
		return RuleMatch{
			Matched:  true,
			RuleName: r.Name,
			Action:   r.Action,
			Priority: prio,
		}
	}
	return RuleMatch{}
}

func ruleMatches(r config.RuleConfig, item ingest.IncomingItem, lowerHaystack string) bool {
	if r.SourceType != "" && !strings.EqualFold(r.SourceType, item.SourceType) {
		return false
	}
	if r.SourceName != "" && !strings.Contains(strings.ToLower(item.SourceName), strings.ToLower(r.SourceName)) {
		return false
	}
	if r.SenderMatch != "" && !strings.Contains(strings.ToLower(item.Sender), strings.ToLower(r.SenderMatch)) {
		return false
	}
	if r.Contains != "" && !strings.Contains(lowerHaystack, strings.ToLower(r.Contains)) {
		return false
	}
	if r.GitHubReason != "" && !matchesAnyCSV(item.Reason, r.GitHubReason) {
		return false
	}
	if r.CompiledRegex != nil {
		full := item.Title + "\n" + item.Body + "\n" + item.Sender
		if !r.CompiledRegex.MatchString(full) {
			return false
		}
	}
	return true
}

func matchesAnyCSV(actual, csvList string) bool {
	actual = strings.TrimSpace(strings.ToLower(actual))
	if actual == "" {
		return false
	}
	for _, part := range strings.Split(csvList, ",") {
		if strings.TrimSpace(strings.ToLower(part)) == actual {
			return true
		}
	}
	return false
}

func defaultPriorityForAction(action string) int {
	switch action {
	case "push_now":
		return 4
	case "queue_digest":
		return 2
	default:
		return 1
	}
}
