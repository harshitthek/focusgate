package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var slackID = regexp.MustCompile(`^[A-Z][A-Z0-9]{2,63}$`)
var slackEventID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{2,63}$`)
var slackMessageTime = regexp.MustCompile(`^[0-9]{1,15}\.[0-9]{6}$`)

// VerifySlackEvent authenticates the raw request and rejects stale deliveries.
func VerifySlackEvent(secret, timestamp, signature string, body []byte, now time.Time) bool {
	value, err := strconv.ParseInt(timestamp, 10, 64)
	if secret == "" || err != nil || value < now.Unix()-300 || value > now.Unix()+300 {
		return false
	}
	digest, ok := strings.CutPrefix(signature, "v0=")
	if !ok || len(digest) != sha256.Size*2 {
		return false
	}
	received, err := hex.DecodeString(digest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return hmac.Equal(received, mac.Sum(nil))
}

// ParseSlackEvent handles URL verification and mentions delivered to the installed app.
func ParseSlackEvent(body []byte, workspace, mentionUser string, now time.Time, maxSnippet int) (IncomingItem, string, error) {
	var event struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		TeamID    string `json:"team_id"`
		EventID   string `json:"event_id"`
		EventTime int64  `json:"event_time"`
		Event     struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			BotID   string `json:"bot_id"`
			User    string `json:"user"`
			Text    string `json:"text"`
			Channel string `json:"channel"`
			TS      string `json:"ts"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return IncomingItem{}, "", errors.New("invalid Slack event")
	}
	if event.Type == "url_verification" {
		if event.Challenge == "" || len(event.Challenge) > 256 {
			return IncomingItem{}, "", errors.New("invalid Slack challenge")
		}
		return IncomingItem{}, event.Challenge, nil
	}
	if event.Type != "event_callback" || event.Event.BotID != "" ||
		event.Event.Subtype != "" || event.TeamID != workspace {
		return IncomingItem{}, "", nil
	}
	personalMention := event.Event.Type == "message" && slackID.MatchString(mentionUser) &&
		strings.Contains(event.Event.Text, "<@"+mentionUser+">")
	if event.Event.Type != "app_mention" && !personalMention {
		return IncomingItem{}, "", nil
	}
	if !slackEventID.MatchString(event.EventID) || !slackID.MatchString(event.Event.Channel) ||
		!slackID.MatchString(event.Event.User) || strings.TrimSpace(event.Event.Text) == "" {
		return IncomingItem{}, "", errors.New("invalid Slack mention")
	}
	link := ""
	itemKey := event.EventID
	if slackMessageTime.MatchString(event.Event.TS) {
		itemKey = event.Event.Channel + ":" + event.Event.TS
		link = "https://app.slack.com/archives/" + event.Event.Channel + "/p" + strings.ReplaceAll(event.Event.TS, ".", "")
	}
	published := now
	if event.EventTime > 0 && event.EventTime <= now.Add(5*time.Minute).Unix() {
		published = time.Unix(event.EventTime, 0).UTC()
	}
	item := IncomingItem{
		ExternalID:  "slack:" + workspace + ":" + itemKey,
		SourceType:  "slack",
		SourceName:  event.Event.Channel,
		Title:       "Slack mention",
		Body:        event.Event.Text,
		Sender:      event.Event.User,
		URL:         link,
		Reason:      "mention",
		PublishedAt: published,
	}
	item.Normalize(maxSnippet)
	return item, "", nil
}
