package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxGitHubResponseBytes = 2 << 20 // 2 MiB guard

type ghNotification struct {
	ID         string `json:"id"`
	Reason     string `json:"reason"`
	Unread     bool   `json:"unread"`
	UpdatedAt  string `json:"updated_at"`
	Subject    struct {
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
// If-Modified-Since so unchanged polls return 304 and do minimal work.
func FetchGitHubNotifications(ctx context.Context, client *http.Client, token string, participatingOnly bool, lastModified string, maxSnippet int) (items []IncomingItem, nextLastModified string, err error) {
	if strings.TrimSpace(token) == "" {
		return nil, lastModified, fmt.Errorf("github token is empty")
	}
	endpoint := "https://api.github.com/notifications?per_page=40"
	if participatingOnly {
		endpoint += "&participating=true"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, lastModified, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "FocusGate/1.0")
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, lastModified, fmt.Errorf("github notifications request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, lastModified, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, lastModified, fmt.Errorf("github notifications HTTP %d", resp.StatusCode)
	}

	nextLastModified = resp.Header.Get("Last-Modified")
	if nextLastModified == "" {
		nextLastModified = lastModified
	}

	var notes []ghNotification
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGitHubResponseBytes)).Decode(&notes); err != nil {
		return nil, lastModified, fmt.Errorf("decode github notifications: %w", err)
	}

	items = make([]IncomingItem, 0, len(notes))
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
	return items, nextLastModified, nil
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

// SleepInterval parses GitHub's X-Poll-Interval header if needed.
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
