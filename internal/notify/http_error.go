package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// Transport errors can embed the complete secret webhook URL or topic.
func privateRequestError(channel string, err error, response ...*http.Response) error {
	if len(response) > 0 && response[0] != nil && response[0].StatusCode >= 300 && response[0].StatusCode < 400 {
		return errors.New(channel + " redirect rejected")
	}
	if errors.Is(err, context.Canceled) {
		return errors.New(channel + " request canceled")
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return errors.New(channel + " request timed out")
	}
	return errors.New(channel + " request failed")
}
