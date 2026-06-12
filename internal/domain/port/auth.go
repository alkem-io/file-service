// Package port defines the outbound interfaces the domain core depends on —
// document persistence, blob storage, authorization, and image processing.
// Adapters under internal/adapter implement them; the domain service only
// ever sees these contracts, which is what keeps the hexagon's dependencies
// pointing inward.
package port

import (
	"context"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// AuthPort abstracts the authorization check against the auth-evaluation-service.
type AuthPort interface {
	CheckPrivilege(ctx context.Context, actorID, privilege, authorizationPolicyID string) (model.AuthResult, error)
}
