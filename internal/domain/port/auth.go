package port

import (
	"context"

	"github.com/alkem-io/file-service-go/internal/domain/model"
)

// AuthPort abstracts the authorization check against the auth-evaluation-service.
type AuthPort interface {
	CheckPrivilege(ctx context.Context, actorID, privilege, authorizationPolicyID string) (model.AuthResult, error)
}
