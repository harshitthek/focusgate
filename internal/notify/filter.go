package notify

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"focusgate/internal/store"
)

// IsMention checks whether a stored notification item represents an explicit
// mention, review request, or assignment directed to the user, or matches any
// of the user's configured mention handles.
func IsMention(item store.StoredItem, handles []string) bool {
	// 1. Direct GitHub triage signals
	if strings.EqualFold(item.SourceType, "github") {
		reason := strings.ToLower(strings.TrimSpace(item.Reason))
		if reason == "mention" || reason == "team_mention" || reason == "review_requested" || reason == "assign" {
			return true
		}
	}

	if len(handles) == 0 {
		return false
	}

	lowerTitle := strings.ToLower(item.Title)
	lowerBody := strings.ToLower(item.Body)
	lowerSender := strings.ToLower(item.Sender)

	for _, h := range handles {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		hBare := strings.TrimPrefix(h, "@")

		// Check full handle with word boundaries
		if hasWordBoundary(lowerTitle, h) || hasWordBoundary(lowerBody, h) || hasWordBoundary(lowerSender, h) {
			return true
		}

		// Also check with leading @ if provided without it, or bare name if provided with it
		if hBare != "" && hBare != h {
			if hasWordBoundary(lowerTitle, hBare) || hasWordBoundary(lowerBody, hBare) || hasWordBoundary(lowerSender, hBare) {
				return true
			}
		} else if hBare != "" {
			withAt := "@" + hBare
			if hasWordBoundary(lowerTitle, withAt) || hasWordBoundary(lowerBody, withAt) || hasWordBoundary(lowerSender, withAt) {
				return true
			}
		}
	}

	return false
}

func hasWordBoundary(text, target string) bool {
	if text == "" || target == "" {
		return false
	}
	start := 0
	targetLen := len(target)
	isAtPrefixed := strings.HasPrefix(target, "@")

	for {
		idx := strings.Index(text[start:], target)
		if idx < 0 {
			return false
		}
		realIdx := start + idx
		start = realIdx + 1

		// Check left boundary
		leftValid := true
		if realIdx > 0 {
			rPrev, _ := utf8.DecodeLastRuneInString(text[:realIdx])
			if isWordRune(rPrev) || (!isAtPrefixed && rPrev == '@') {
				leftValid = false
			}
		}

		if !leftValid {
			continue
		}

		// Check right boundary
		rightValid := true
		endIdx := realIdx + targetLen
		if endIdx < len(text) {
			rNext, _ := utf8.DecodeRuneInString(text[endIdx:])
			if isWordRune(rNext) {
				rightValid = false
			}
		}

		if rightValid {
			return true
		}
	}
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

// ParseMentionHandles splits a comma-, semicolon-, or whitespace-separated string into cleaned handles.
func ParseMentionHandles(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	var res []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			res = append(res, trimmed)
		}
	}
	return res
}
