package raft

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HTTPNodeProvider implements NodeProvider by communicating with a remote
// Manager over HTTP, using the endpoints exposed by Manager.Handler().
//
// It satisfies §22 Step 2: the BalanceController can manage leaders across
// physical machines by pointing one HTTPNodeProvider at each machine's HTTP
// listener.
type HTTPNodeProvider struct {
	baseURL string       // trimmed, no trailing slash
	client  *http.Client // nil → http.DefaultClient
}

// NewHTTPNodeProvider returns an HTTPNodeProvider that talks to the Manager
// HTTP endpoint at baseURL. Pass nil for client to use http.DefaultClient.
func NewHTTPNodeProvider(baseURL string, client *http.Client) *HTTPNodeProvider {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPNodeProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
	}
}

// StatusAll implements NodeProvider by calling GET <baseURL>/status and
// decoding the JSON response. Returns nil on any error (the BalanceController
// skips nodes with empty status).
func (p *HTTPNodeProvider) StatusAll() []GroupStatus {
	resp, err := p.client.Get(p.baseURL + "/status")
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var statuses []GroupStatus
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		return nil
	}
	return statuses
}

// TransferGroupLeadership implements NodeProvider by calling
// POST <baseURL>/transfer with a JSON body.
func (p *HTTPNodeProvider) TransferGroupLeadership(ctx context.Context, groupID uint64, to NodeID) error {
	body, err := json.Marshal(transferBody{GroupID: groupID, To: to})
	if err != nil {
		return fmt.Errorf("HTTPNodeProvider: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/transfer", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("HTTPNodeProvider: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTPNodeProvider: POST /transfer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	// Read error body for a better error message.
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("HTTPNodeProvider: POST /transfer: status %d: %s",
		resp.StatusCode, strings.TrimSpace(string(errBody)))
}
