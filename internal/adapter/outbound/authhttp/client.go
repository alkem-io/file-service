package authhttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"golang.org/x/net/http2"

	"github.com/alkem-io/file-service-go/internal/domain/model"
)

type evaluateRequest struct {
	AgentID               string `json:"agentId"`
	Privilege             string `json:"privilege"`
	AuthorizationPolicyID string `json:"authorizationPolicyId"`
}

type evaluateResponse struct {
	Allowed bool          `json:"allowed"`
	Reason  string        `json:"reason"`
	Error   *errorDetails `json:"error,omitempty"`
}

type errorDetails struct {
	Code         string `json:"code"`
	Dependency   string `json:"dependency,omitempty"`
	RetryAfterMs int    `json:"retryAfterMs,omitempty"`
}

// Client implements port.AuthPort via h2c HTTP/2 to the authorization-evaluation-service.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// New creates an h2c auth client for the given base URL (e.g., "http://auth-service:6060").
func New(baseURL string) *Client {
	return &Client{
		httpClient: newH2CClient(),
		baseURL:    baseURL,
	}
}

func (c *Client) CheckPrivilege(ctx context.Context, agentID, privilege, authorizationPolicyID string) (model.AuthResult, error) {
	if c.httpClient == nil {
		return model.AuthResult{}, fmt.Errorf("h2c client is nil")
	}

	reqBody := evaluateRequest{
		AgentID:               agentID,
		Privilege:             privilege,
		AuthorizationPolicyID: authorizationPolicyID,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return model.AuthResult{}, fmt.Errorf("marshal auth request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/internal/auth/evaluate",
		bytes.NewReader(payload))
	if err != nil {
		return model.AuthResult{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return model.AuthResult{}, fmt.Errorf("h2c auth request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return model.AuthResult{}, fmt.Errorf("read auth response: %w", err)
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		var evalResp evaluateResponse
		if jsonErr := json.Unmarshal(body, &evalResp); jsonErr == nil && evalResp.Error != nil {
			return model.AuthResult{Allowed: false, Reason: evalResp.Reason},
				fmt.Errorf("auth service degraded: %s (retry after %dms)", evalResp.Error.Code, evalResp.Error.RetryAfterMs)
		}
		return model.AuthResult{}, fmt.Errorf("auth service unavailable (HTTP %d)", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return model.AuthResult{}, fmt.Errorf("auth service error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var evalResp evaluateResponse
	if err := json.Unmarshal(body, &evalResp); err != nil {
		return model.AuthResult{}, fmt.Errorf("unmarshal auth response: %w", err)
	}

	return model.AuthResult{
		Allowed: evalResp.Allowed,
		Reason:  evalResp.Reason,
	}, nil
}

// newH2CClient creates an HTTP/2 cleartext client with connection multiplexing.
func newH2CClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}
