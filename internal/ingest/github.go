// Package ingest provides polling and webhook ingestion drivers for FocusGate.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxGitHubResponseBytes = 2 << 20 // 2 MiB guard
	maxGitHubPages         = 3
)

type ghNotification struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	Unread    bool   `json:"unread"`
	UpdatedAt string `json:"updated_at"`
	Subject   struct {
		Title string `json:"title"`
		Type  string `json:"type"`
		URL   string `json:"url"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
}

// FetchGitHubNotifications polls GitHub's /notifications endpoint using
// If-Modified-Since on the first page or resumes from a saved next-page cursor,
// following bounded rel="next" links.
func FetchGitHubNotifications(ctx context.Context, client *http.Client, token string, participatingOnly bool, lastModified string, nextCursor string, maxSnippet int) (items []IncomingItem, nextLastModified string, continuationCursor string, err error) {
	if strings.TrimSpace(token) == "" {
		return nil, lastModified, nextCursor, fmt.Errorf("github token is empty")
	}
	endpoint := "https://api.github.com/notifications?per_page=40"
	if participatingOnly {
		endpoint += "&participating=true"
	}

	nextLastModified = lastModified
	baseParsed, _ := url.Parse(endpoint)

	currentURL := endpoint
	isResumedCursor := false
	pendingLM := ""
	rawCursor := nextCursor
	if strings.Contains(nextCursor, "||") {
		parts := strings.SplitN(nextCursor, "||", 2)
		rawCursor = parts[0]
		pendingLM = parts[1]
	}
	if sanitizedCursor := sanitizeNextGitHubLink(rawCursor, baseParsed, participatingOnly); sanitizedCursor != "" {
		currentURL = sanitizedCursor
		isResumedCursor = true
	}

	for page := 0; page < maxGitHubPages && currentURL != ""; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, currentURL, nil)
		if err != nil {
			return nil, lastModified, nextCursor, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "FocusGate/1.0")
		if page == 0 && !isResumedCursor && lastModified != "" {
			req.Header.Set("If-Modified-Since", lastModified)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, lastModified, nextCursor, fmt.Errorf("github notifications request: %w", err)
		}

		if page == 0 && !isResumedCursor && resp.StatusCode == http.StatusNotModified {
			resp.Body.Close()
			return nil, lastModified, "", nil
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, lastModified, nextCursor, fmt.Errorf("github notifications HTTP %d", resp.StatusCode)
		}
		if page == 0 && !isResumedCursor {
			if lm := resp.Header.Get("Last-Modified"); lm != "" {
				pendingLM = lm
			}
		}

		var notes []ghNotification
		decErr := json.NewDecoder(io.LimitReader(resp.Body, maxGitHubResponseBytes)).Decode(&notes)
		linkHdr := resp.Header.Get("Link")
		resp.Body.Close()
		if decErr != nil {
			return nil, lastModified, nextCursor, fmt.Errorf("decode github notifications: %w", decErr)
		}

		for _, n := range notes {
			if !n.Unread {
				continue
			}
			webURL := apiURLToWebURL(n.Subject.URL, n.Repository.HTMLURL)
			item := IncomingItem{
				ExternalID:  "gh:" + n.ID + ":" + n.UpdatedAt,
				SourceType:  "github",
				SourceName:  n.Repository.FullName,
				Title:       fmt.Sprintf("[%s] %s", n.Repository.FullName, n.Subject.Title),
				Body:        fmt.Sprintf("%s on %s (%s)", n.Reason, n.Subject.Type, n.Repository.FullName),
				Sender:      n.Repository.FullName,
				URL:         webURL,
				Tag:         n.Subject.Type,
				Reason:      n.Reason,
				PublishedAt: parseAnyTime(n.UpdatedAt),
			}
			item.Normalize(maxSnippet)
			items = append(items, item)
		}

		currentURL = sanitizeNextGitHubLink(parseNextLink(linkHdr), baseParsed, participatingOnly)
	}

	continuationCursor = currentURL
	if continuationCursor != "" {
		// When pagination halts before reaching the last page, preserve pending LastModified
		// in cursor payload and retain previous LastModified so watermark advances only after
		// the entire unread backlog has been consumed.
		if pendingLM != "" {
			continuationCursor = currentURL + "||" + pendingLM
		}
		nextLastModified = lastModified
	} else if pendingLM != "" {
		nextLastModified = pendingLM
	}
	return items, nextLastModified, continuationCursor, nil
}

func parseNextLink(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		pieces := strings.Split(strings.TrimSpace(part), ";")
		if len(pieces) < 2 {
			continue
		}
		rawURL := strings.Trim(strings.TrimSpace(pieces[0]), "<>")
		for _, param := range pieces[1:] {
			if strings.TrimSpace(param) == `rel="next"` {
				return rawURL
			}
		}
	}
	return ""
}

func sanitizeNextGitHubLink(nextRaw string, baseParsed *url.URL, participatingOnly bool) string {
	if nextRaw == "" || baseParsed == nil {
		return ""
	}
	nextParsed, err := baseParsed.Parse(nextRaw)
	if err != nil || nextParsed.Scheme != baseParsed.Scheme || !strings.EqualFold(nextParsed.Host, baseParsed.Host) {
		return ""
	}
	if participatingOnly {
		q := nextParsed.Query()
		if q.Get("participating") != "true" {
			q.Set("participating", "true")
			nextParsed.RawQuery = q.Encode()
		}
	}
	return nextParsed.String()
}

// apiURLToWebURL converts GitHub API subject URLs into clickable browser URLs
// without extra API round trips.
func apiURLToWebURL(apiURL, repoHTMLURL string) string {
	if apiURL == "" {
		return repoHTMLURL
	}
	replacer := strings.NewReplacer(
		"https://api.github.com/repos/", "https://github.com/",
		"/pulls/", "/pull/",
		"/commits/", "/commit/",
	)
	converted := replacer.Replace(apiURL)
	if strings.HasPrefix(converted, "https://github.com/") {
		return converted
	}
	return repoHTMLURL
}

// ParseGitHubPollWait parses GitHub's X-Poll-Interval header if needed.
func ParseGitHubPollWait(header string, fallback time.Duration) time.Duration {
	if header == "" {
		return fallback
	}
	var secs int
	if _, err := fmt.Sscanf(header, "%d", &secs); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return fallback
}
