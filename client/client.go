// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// client.go: HTTP client for the coordinator service (SPEC §5.3, §7, §11.2).

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/unbrice/soft-kvm/identity"
	"github.com/unbrice/soft-kvm/state"
)

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
	tlsCfg, err := identity.ClientTLSConfig(token)
	if err != nil {
		return nil, fmt.Errorf("derive TLS identity: %w", err)
	}
	return &Client{
		token:  token,
		client: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	}, nil
}

// ErrNotOwner is returned by Take when the server refuses it: the sender is
// not the owner, or the activation it took under is no longer the current one
// (SPEC §4.3).
var ErrNotOwner = errors.New("not the owner under this activation")

// Claim POSTs /claim/<id> (?force=true when force, ?retake=true when retake).
// It returns whether the server's owner or activation changed. A 400 whose
// body reports no live agent becomes ErrNoLiveAgent (SPEC §5.3: activate needs
// --force). Any other non-2xx returns an error that includes the response
// body.
func (c *Client) Claim(ctx context.Context, base, id string, force, retake bool) (bool, error) {
	u := "https://" + base + "/claim/" + id
	var q []string
	if force {
		q = append(q, "force=true")
	}
	if retake {
		q = append(q, "retake=true")
	}
	if len(q) > 0 {
		u += "?" + strings.Join(q, "&")
	}
	status, body, err := c.request(ctx, http.MethodPost, u, nil, 5*time.Second)
	if err != nil {
		return false, err
	}
	if status == http.StatusBadRequest && strings.Contains(string(body), "no live agent for") {
		return false, ErrNoLiveAgent
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

// Take POSTs /take/<id>: the owner, under activation, asks that the named
// screens and devices come to it. It returns ErrNotOwner on a 409.
func (c *Client) Take(ctx context.Context, base, id string, activation int64, takes []state.Take) error {
	if takes == nil {
		takes = []state.Take{}
	}
	return c.postJSON(ctx, "/take/"+id, base, struct {
		Activation int64        `json:"activation"`
		Takes      []state.Take `json:"takes"`
	}{activation, takes})
}

// Arrived POSTs /arrived/<id>: the holder reports that the devices it took
// are linked to it. The server ignores any arrival that no longer matches a
// hold.
func (c *Client) Arrived(ctx context.Context, base, id string, arrivals ...state.Arrival) error {
	if arrivals == nil {
		arrivals = []state.Arrival{}
	}
	return c.postJSON(ctx, "/arrived/"+id, base, struct {
		Arrivals []state.Arrival `json:"arrivals"`
	}{arrivals})
}

// postJSON POSTs v to path and maps the status: 409 is ErrNotOwner, any other
// non-200 an error carrying the body.
func (c *Client) postJSON(ctx context.Context, path, base string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	status, body, err := c.request(ctx, http.MethodPost, "https://"+base+path, payload, 5*time.Second)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusConflict:
		return ErrNotOwner
	}
	return httpError(http.MethodPost, path, status, body)
}

// State GETs /state and returns the current server state.
func (c *Client) State(ctx context.Context, base string) (*state.ServerState, error) {
	url := "https://" + base + "/state"
	status, body, err := c.request(ctx, http.MethodGet, url, nil, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, httpError(http.MethodGet, "/state", status, body)
	}

	var st state.ServerState
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, fmt.Errorf("decode state response: %w", err)
	}
	return &st, nil
}

// WaitClientTimeout is the internal timeout for GET /wait. It must be larger
// than the server's long-poll timeout so the server fires first.
const WaitClientTimeout = 60 * time.Second

// Wait GETs /wait?epoch=N&id=me. It returns woke=true on a 200 wake and
// woke=false on a 204 timeout. The caller supplies ctx; the internal timeout is
// WaitClientTimeout so the server fires first.
func (c *Client) Wait(ctx context.Context, base string, epoch int64, id string) (bool, error) {
	u := fmt.Sprintf("https://%s/wait?epoch=%d&id=%s", base, epoch, url.QueryEscape(id))
	status, body, err := c.request(ctx, http.MethodGet, u, nil, WaitClientTimeout)
	if err != nil {
		return false, err
	}
	if status == http.StatusNoContent {
		return false, nil
	}
	if status != http.StatusOK {
		return false, httpError(http.MethodGet, "/wait", status, body)
	}
	return true, nil
}

// request performs one request with a per-call timeout; a non-nil payload is
// sent as a JSON body. It returns the status, the full response body, and any
// transport-level error. TLS handshake failures are wrapped with a hint that
// SOFTKVM_TOKEN may differ.
func (c *Client) request(ctx context.Context, method, url string, payload []byte, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "tls:") {
			return 0, nil, fmt.Errorf("%w (SOFTKVM_TOKEN mismatch?)", err)
		}
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
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
