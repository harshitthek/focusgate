/* Package androidbridge exposes the on-device engine to Android via gomobile. */
package androidbridge

import (
	"encoding/json"

	"focusgate/internal/phone"
)

type Client struct {
	engine *phone.Engine
	dir    string
}

func Open(dir, issuer, subject string) (*Client, error) {
	profile := ""
	if issuer != "" || subject != "" {
		var err error
		profile, err = phone.AccountKey(issuer, subject)
		if err != nil {
			return nil, err
		}
	}
	e, err := phone.Open(dir, profile)
	if err != nil {
		return nil, err
	}
	return &Client{e, dir}, nil
}

func (c *Client) Close() error                     { return c.engine.Close() }
func (c *Client) StopNetwork()                     { c.engine.Suspend() }
func (c *Client) BeginNetwork(id string) error     { return c.engine.BeginNetwork(id) }
func (c *Client) EndNetwork(id string)             { c.engine.EndNetwork(id) }
func (c *Client) Mark(id string, status int) error { return c.engine.Mark(id, status) }
func (c *Client) Refresh(id, assessmentURL, token string) error {
	return c.engine.Refresh(id, assessmentURL, token)
}
func (c *Client) Sync(id, syncURL, token string) error { return c.engine.Sync(id, syncURL, token) }
func (c *Client) ImportLocal() error                   { return c.engine.ImportLocal(c.dir) }
func (c *Client) ImportLegacy(path string) error       { return c.engine.ImportLegacy(path) }
func (c *Client) DismissConflict(id string) error      { return c.engine.DismissConflict(id) }

func (c *Client) State(archive bool, offset int) (string, error) {
	state, err := c.engine.State(archive, offset)
	return result(state, err)
}

func (c *Client) ReadSettings() (string, error) {
	s, err := c.engine.Settings()
	return result(s, err)
}

func (c *Client) Digest(offset int) (string, error) {
	items, err := c.engine.Digest(offset)
	return result(items, err)
}

func (c *Client) Notices(zone string, offsetSeconds int) (string, error) {
	notices, err := c.engine.Notices(zone, offsetSeconds)
	return result(notices, err)
}

func (c *Client) Settings(raw string) error {
	var s phone.Settings
	if err := phone.Decode(raw, &s); err != nil {
		return err
	}
	return c.engine.SetSettings(s)
}

func (c *Client) SaveFeed(raw string) error {
	var f phone.Feed
	if err := phone.Decode(raw, &f); err != nil {
		return err
	}
	return c.engine.SaveFeed(f)
}

func (c *Client) Acknowledge(raw string) error {
	var n phone.Notice
	if err := phone.Decode(raw, &n); err != nil {
		return err
	}
	return c.engine.Acknowledge(n)
}

func result(value any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(value)
	return string(raw), err
}
