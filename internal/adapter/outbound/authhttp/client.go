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
	"time"

	gobreaker "github.com/sony/gobreaker/v2"
	"go.uber.org/zap"
	"golang.org/x/net/http2"

	"github.com/alkem-io/file-service-go/internal/domain/model"
)

type evaluateRequest struct {
	ActorID               string `json:"actorId"`
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
// Uses a persistent multiplexed TCP connection with circuit breaker protection.
type Client struct {
	httpClient *http.Client
	baseURL    string
	breaker    *gobreaker.CircuitBreaker[model.AuthResult]
	logger     *zap.Logger
}

// New creates an h2c auth client with circuit breaker.
func New(baseURL string, breaker *gobreaker.CircuitBreaker[model.AuthResult], logger *zap.Logger) *Client {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Client{
		httpClient: newH2CClient(),
		baseURL:    baseURL,
		breaker:    breaker,
		logger:     logger,
	}
}

func (c *Client) CheckPrivilege(ctx context.Context, actorID, privilege, authorizationPolicyID string) (model.AuthResult, error) {
	if c.httpClient == nil {
		return model.AuthResult{}, fmt.Errorf("h2c client is nil")
	}

	if c.breaker == nil {
		return c.doRequest(ctx, actorID, privilege, authorizationPolicyID)
	}

	return c.breaker.Execute(func() (model.AuthResult, error) {
		return c.doRequest(ctx, actorID, privilege, authorizationPolicyID)
	})
}

func (c *Client) doRequest(ctx context.Context, actorID, privilege, authorizationPolicyID string) (model.AuthResult, error) {
	reqBody := evaluateRequest{
		ActorID:               actorID,
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
		c.logger.Warn("h2c auth request failed", zap.Error(err))
		return model.AuthResult{}, fmt.Errorf("h2c auth request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	const maxResponseBody = 64 * 1024 // 64KB — auth responses are small JSON
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
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

// newH2CClient creates an HTTP/2 cleartext client with persistent connection multiplexing.
// http2.Transport automatically re-establishes the TCP connection if it drops.
func newH2CClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second, // fallback for requests without context deadline
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}
