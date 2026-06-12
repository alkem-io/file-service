// Package nats implements port.AuthPort via NATS request-reply on the
// auth.evaluate subject — the legacy auth transport, kept for environments
// that have not switched to the h2c client in the sibling authhttp package.
// Connection resilience (reconnect/backoff) is handled in internal/resilience.
package nats

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/alkem-io/file-service/internal/domain/model"
)

type evaluateRequest struct {
	Pattern string       `json:"pattern"`
	Data    evaluateData `json:"data"`
}

type evaluateData struct {
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

// AuthClient implements port.AuthPort via NATS request-reply to auth.evaluate.
type AuthClient struct {
	Conn    *nats.Conn
	Subject string
}

// CheckPrivilege implements port.AuthPort: a request-reply round trip on the
// configured subject, bounded by ctx. A degraded auth service (error payload
// with retryAfterMs) returns both a denying AuthResult and a non-nil error so
// callers fail closed without mistaking the outage for a policy denial.
func (c *AuthClient) CheckPrivilege(ctx context.Context, actorID, privilege, authorizationPolicyID string) (model.AuthResult, error) {
	if c.Conn == nil {
		return model.AuthResult{}, fmt.Errorf("NATS connection is nil")
	}
	if c.Subject == "" {
		return model.AuthResult{}, fmt.Errorf("NATS subject is empty")
	}

	req := evaluateRequest{
		Pattern: "evaluate",
		Data: evaluateData{
			ActorID:               actorID,
			Privilege:             privilege,
			AuthorizationPolicyID: authorizationPolicyID,
		},
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return model.AuthResult{}, fmt.Errorf("marshal auth request: %w", err)
	}

	msg, err := c.Conn.RequestWithContext(ctx, c.Subject, payload)
	if err != nil {
		return model.AuthResult{}, fmt.Errorf("nats auth request: %w", err)
	}

	var resp evaluateResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return model.AuthResult{}, fmt.Errorf("unmarshal auth response: %w", err)
	}

	if resp.Error != nil && resp.Error.RetryAfterMs > 0 {
		return model.AuthResult{Allowed: false, Reason: resp.Reason},
			fmt.Errorf("auth service degraded: %s (retry after %dms)", resp.Error.Code, resp.Error.RetryAfterMs)
	}

	return model.AuthResult{
		Allowed: resp.Allowed,
		Reason:  resp.Reason,
	}, nil
}
