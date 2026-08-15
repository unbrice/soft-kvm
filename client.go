// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// client.go: HTTP client for the coordinator service (SPEC §5.3, §7, §11.2).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnauthorized is returned when the server rejects the X-Display-Token.
var ErrUnauthorized = errors.New("invalid display token")

// ErrNoLiveAgent is returned by Claim when the server reports the target host
// has no live agent and the claim was not forced.
var ErrNoLiveAgent = errors.New("no live agent")

// Client speaks the coordinator HTTP API.
type Client struct {
	token  string
	client *http.Client
}

// NewClient returns a client that authenticates with token and trusts exactly
// the TLS identity derived from it (SPEC §9).
func NewClient(token string) (*Client, error) {
	tlsCfg, err := clientTLSConfig(token)
	if err != nil {
		return nil, fmt.Errorf("derive TLS identity: %w", err)
	}
	return &Client{
		token:  token,
		client: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	}, nil
}

// Claim POSTs /claim/<id> (?force=true when force). It returns whether the
// server's owner changed. 401 becomes ErrUnauthorized; a 400 whose body reports
// no live agent becomes ErrNoLiveAgent (SPEC §5.3: activate needs --force).
// Any other non-2xx returns an error that includes the response body.
func (c *Client) Claim(ctx context.Context, base, id string, force bool) (bool, error) {
	url := "https://" + base + "/claim/" + id
	if force {
		url += "?force=true"
	}
	status, body, err := c.request(ctx, http.MethodPost, url, 5*time.Second)
	if err != nil {
		return false, err
	}
	if status == http.StatusUnauthorized {
		return false, ErrUnauthorized
	}
	if status == http.StatusBadRequest {
		if strings.Contains(string(body), "no live agent for") {
			return false, ErrNoLiveAgent
		}
		return false, httpError(http.MethodPost, "/claim/"+id, status, body)
	}
	if status != http.StatusOK {
		return false, httpError(http.MethodPost, "/claim/"+id, status, body)
	}

	var r struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return false, fmt.Errorf("decode claim response: %w", err)
	}
	return r.Changed, nil
}

// State GETs /state and returns the current server state.
func (c *Client) State(ctx context.Context, base string) (*ServerState, error) {
	url := "https://" + base + "/state"
	status, body, err := c.request(ctx, http.MethodGet, url, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if status != http.StatusOK {
		return nil, httpError(http.MethodGet, "/state", status, body)
	}

	var state ServerState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, fmt.Errorf("decode state response: %w", err)
	}
	return &state, nil
}

// Wait GETs /wait?epoch=N&id=me. It returns woke=true on a 200 wake and
// woke=false on a 204 timeout. The caller supplies ctx; the internal timeout is
// 60 s to match the long-poll (the server times out at 50 s).
func (c *Client) Wait(ctx context.Context, base string, epoch int64, id string) (bool, error) {
	u := fmt.Sprintf("https://%s/wait?epoch=%d&id=%s", base, epoch, url.QueryEscape(id))
	status, body, err := c.request(ctx, http.MethodGet, u, 60*time.Second)
	if err != nil {
		return false, err
	}
	if status == http.StatusUnauthorized {
		return false, ErrUnauthorized
	}
	if status == http.StatusNoContent {
		return false, nil
	}
	if status != http.StatusOK {
		return false, httpError(http.MethodGet, "/wait", status, body)
	}
	return true, nil
}

// request performs one authenticated request with a per-call timeout. It
// returns the status, the full response body, and any transport-level error.
func (c *Client) request(ctx context.Context, method, url string, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Display-Token", c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// httpError builds a non-2xx error that includes the response body.
func httpError(method, path string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("%s %s: %d %s", method, path, status, msg)
}
