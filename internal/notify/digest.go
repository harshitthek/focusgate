package notify

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

// Dispatcher fans out immediate notifications and scheduled digests to ntfy, Windows toast, Slack, and Matrix.
type Dispatcher struct {
	mu            sync.RWMutex
	cfg           config.Config
	ntfy          *NtfyPublisher
	slack         *SlackPublisher
	matrix        *MatrixPublisher
	deliveryStore *store.Store
}

// NewDispatcher constructs a Dispatcher from runtime configuration.
func NewDispatcher(cfg config.Config) *Dispatcher {
	return &Dispatcher{
		cfg:    cfg,
		ntfy:   NewNtfyPublisher(cfg.Notify.Ntfy),
		slack:  NewSlackPublisher(cfg.Notify.Slack),
		matrix: NewMatrixPublisher(cfg.Notify.Matrix),
	}
}

// UpdateNotifyConfig hot-reloads notification channel settings and mention filters.
func (d *Dispatcher) UpdateNotifyConfig(nc config.NotifyConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.Notify = nc
	d.ntfy = NewNtfyPublisher(nc.Ntfy)
	d.slack = NewSlackPublisher(nc.Slack)
	d.matrix = NewMatrixPublisher(nc.Matrix)
}

// NotifyConfig returns the active notification configuration.
func (d *Dispatcher) NotifyConfig() config.NotifyConfig {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg.Notify
}

// SetDeliveryStore enables durable receipts shared by daemon and CLI dispatchers.
func (d *Dispatcher) SetDeliveryStore(st *store.Store) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deliveryStore = st
}

type outboundChannel struct {
	key    string
	push   func(context.Context, store.StoredItem) error
	digest func(context.Context, []store.StoredItem) error
}

func (d *Dispatcher) channels() (config.Config, []outboundChannel) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	cfg := d.cfg
	ntfy, slack, matrix := d.ntfy, d.slack, d.matrix
	var channels []outboundChannel
	if cfg.Notify.Ntfy.Enabled {
		channels = append(channels, outboundChannel{
			key: fmt.Sprintf("ntfy:%x", sha256.Sum256([]byte(cfg.Notify.Ntfy.Server+"/"+cfg.Notify.Ntfy.Topic))),
			push: func(ctx context.Context, item store.StoredItem) error {
				click := item.URL
				if click == "" {
					click = cfg.Server.PublicURL
				}
				return ntfy.Publish(ctx, Message{
					Title: item.Title, Body: pushBody(item), Priority: item.Priority,
					Tags: tagForItem(item), ClickURL: click,
				})
			},
			digest: func(ctx context.Context, items []store.StoredItem) error {
				title, body := digestText(items)
				return ntfy.Publish(ctx, Message{Title: title, Body: body, Priority: 2, Tags: "newspaper", ClickURL: cfg.Server.PublicURL})
			},
		})
	}
	if cfg.Notify.DesktopWindows.Enabled {
		channels = append(channels, outboundChannel{
			key: "desktop",
			push: func(ctx context.Context, item store.StoredItem) error {
				return SendDesktopToast(ctx, item.Title, pushBody(item))
			},
			digest: func(ctx context.Context, items []store.StoredItem) error {
				return SendDesktopToast(ctx, fmt.Sprintf("FocusGate Digest (%d items)", len(items)), fmt.Sprintf("%d queued updates ready to review", len(items)))
			},
		})
	}
	if cfg.Notify.Slack.Enabled {
		channels = append(channels, outboundChannel{
			key: fmt.Sprintf("slack:%x", sha256.Sum256([]byte(cfg.Notify.Slack.WebhookURL))),
			push: func(ctx context.Context, item store.StoredItem) error {
				return slack.Publish(ctx, item, cfg.Server.PublicURL)
			},
			digest: func(ctx context.Context, items []store.StoredItem) error {
				title, body := digestText(items)
				return slack.PublishDigest(ctx, title, body, cfg.Server.PublicURL)
			},
		})
	}
	if cfg.Notify.Matrix.Enabled {
		channels = append(channels, outboundChannel{
			key: fmt.Sprintf("matrix:%x", sha256.Sum256([]byte(cfg.Notify.Matrix.WebhookURL))),
			push: func(ctx context.Context, item store.StoredItem) error {
				return matrix.Publish(ctx, item, cfg.Server.PublicURL)
			},
			digest: func(ctx context.Context, items []store.StoredItem) error {
				title, body := digestText(items)
				return matrix.PublishDigest(ctx, title, body, cfg.Server.PublicURL)
			},
		})
	}
	return cfg, channels
}

func pushBody(item store.StoredItem) string {
	body := item.Body
	if body == "" {
		body = item.DecisionReason
	}
	if item.Sender != "" && !strings.Contains(body, item.Sender) {
		body = fmt.Sprintf("[%s] %s", item.Sender, body)
	}
	return body
}

// deliver skips completed destinations and claims pending sends across processes.
// A crash after an external send and before its receipt can still cause a retry.
func (d *Dispatcher) deliver(ctx context.Context, key string, items []store.StoredItem, send func([]store.StoredItem) error) (result error) {
	d.mu.RLock()
	st := d.deliveryStore
	d.mu.RUnlock()
	if st == nil {
		return send(items)
	}
	type claimedItem struct {
		id    int64
		lease string
	}
	var claims []claimedItem
	defer func() {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		success := result == nil
		for _, claim := range claims {
			if err := st.FinishDelivery(finishCtx, claim.id, key, claim.lease, success); err != nil {
				result = errors.Join(result, err)
			}
		}
	}()
	var pending []store.StoredItem
	for _, item := range items {
		lease, done, err := st.ClaimDelivery(ctx, item.ID, key)
		if err != nil {
			return err
		}
		if !done {
			claims = append(claims, claimedItem{id: item.ID, lease: lease})
			pending = append(pending, item)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	return send(pending)
}

// PushItem persists completion independently for each enabled destination.
func (d *Dispatcher) PushItem(ctx context.Context, item store.StoredItem) error {
	cfg, channels := d.channels()
	if cfg.Notify.OnlyMentions && item.Priority < 5 && !IsMention(item, cfg.Notify.MentionHandles) {
		return nil
	}
	var sendErrors []error
	for _, channel := range channels {
		err := d.deliver(ctx, channel.key, []store.StoredItem{item}, func(_ []store.StoredItem) error {
			return channel.push(ctx, item)
		})
		if err != nil {
			sendErrors = append(sendErrors, err)
		}
	}
	return errors.Join(sendErrors...)
}

// SendDigest completes only after every enabled destination succeeds.
// Completed item/destination receipts prevent resending partial successes.
func (d *Dispatcher) SendDigest(ctx context.Context, items []store.StoredItem) (bool, error) {
	if len(items) == 0 {
		return false, nil
	}
	cfg, channels := d.channels()
	if cfg.Notify.OnlyMentions {
		var filtered []store.StoredItem
		for _, item := range items {
			if item.Priority >= 5 || IsMention(item, cfg.Notify.MentionHandles) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	if len(items) == 0 {
		return true, nil
	}
	var sendErrors []error
	for _, channel := range channels {
		err := d.deliver(ctx, channel.key, items, func(pending []store.StoredItem) error {
			return channel.digest(ctx, pending)
		})
		if err != nil {
			sendErrors = append(sendErrors, err)
		}
	}
	err := errors.Join(sendErrors...)
	return err == nil, err
}

func digestText(items []store.StoredItem) (string, string) {
	var body strings.Builder
	for index, item := range items {
		if index >= 10 {
			fmt.Fprintf(&body, "+ %d more items in FocusGate PWA", len(items)-index)
			break
		}
		prefix := "•"
		if item.Uncertain {
			prefix += " [review]"
		}
		fmt.Fprintf(&body, "%s [%s] %s\n", prefix, item.SourceName, ingest.TruncateRunes(item.Title, 80))
	}
	return fmt.Sprintf("FocusGate Digest (%d items)", len(items)), strings.TrimSpace(body.String())
}

// SendTestAlert dispatches a diagnostic notification to all enabled delivery channels
// and returns the list of channel names that succeeded alongside the first error, if any.
func (d *Dispatcher) SendTestAlert(ctx context.Context) ([]string, error) {
	d.mu.RLock()
	cfg := d.cfg
	ntfyPub := d.ntfy
	slackPub := d.slack
	matrixPub := d.matrix
	d.mu.RUnlock()

	var sent []string
	var firstErr error

	title := "FocusGate Test Alert"
	body := "Notification channel is healthy and reachable."

	if cfg.Notify.Ntfy.Enabled && ntfyPub != nil {
		if err := ntfyPub.Publish(ctx, Message{
			Title:    title,
			Body:     body,
			Priority: 3,
			Tags:     "white_check_mark",
			ClickURL: cfg.Server.PublicURL,
		}); err != nil {
			firstErr = err
		} else {
			sent = append(sent, "ntfy")
		}
	}

	if cfg.Notify.DesktopWindows.Enabled {
		if err := SendDesktopToast(ctx, title, body); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			sent = append(sent, "desktop")
		}
	}

	if cfg.Notify.Slack.Enabled && slackPub != nil {
		if err := slackPub.SendTest(ctx, cfg.Notify.Slack.WebhookURL, cfg.Notify.Slack.MentionHandle); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			sent = append(sent, "slack")
		}
	}

	if cfg.Notify.Matrix.Enabled && matrixPub != nil {
		if err := matrixPub.SendTest(ctx, cfg.Notify.Matrix.WebhookURL, cfg.Notify.Matrix.MentionHandle); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			sent = append(sent, "matrix")
		}
	}

	return sent, firstErr
}

func tagForItem(item store.StoredItem) string {
	if item.Priority >= 5 {
		return "rotating_light"
	}
	switch item.SourceType {
	case "github":
		return "octocat"
	case "imap":
		return "email"
	case "rss":
		return "newspaper"
	default:
		return "bell"
	}
}
