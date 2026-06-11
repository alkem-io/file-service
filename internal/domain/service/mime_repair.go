package service

import (
	"bytes"
	"context"

	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/domain/model"
)

// MimeRepairSummary reports what the boot-time MIME repair did. The caller
// (cmd/server) feeds these counts into the mime_repair_total metric.
type MimeRepairSummary struct {
	Relabeled        int
	Unrecoverable    int
	SkippedNotOffice int
	Errors           int
}

var zipMagic = []byte("PK")

// RunMimeRepair heals documents corrupted by the pre-fix replace path
// (spec 019 FR-006): office-named rows stored with a generic MIME type.
// Content-verified office zips are relabeled to their extension's canonical
// type; zero-byte rows are unrecoverable (the content was never stored or
// was overwritten by an accepted empty save — no prior-version record
// exists to restore from); office-named rows whose content is not a zip
// are left untouched.
//
// Idempotent by construction: relabeled rows no longer match the suspect
// predicate, so a second run touches nothing.
func (s *FileService) RunMimeRepair(ctx context.Context) MimeRepairSummary {
	var sum MimeRepairSummary

	suspects, err := s.Repo.ListByMimeTypes(ctx, model.GenericMIMEList())
	if err != nil {
		s.Logger.Error("mime-repair: suspect scan failed", zap.Error(err))
		sum.Errors++
		return sum
	}

	for _, doc := range suspects {
		canonical, ok := model.OfficeMIMEForName(doc.DisplayName)
		if !ok {
			// Generic MIME but not office-named: legitimately generic
			// (plain text files, archives, ...) — not a suspect.
			continue
		}

		content, err := s.Storage.Read(doc.ExternalID)
		if err != nil {
			sum.Errors++
			s.Logger.Error("mime-repair: content read failed",
				zap.String("documentID", doc.ID.String()),
				zap.String("externalID", doc.ExternalID),
				zap.Error(err))
			continue
		}

		switch {
		case len(content) == 0:
			sum.Unrecoverable++
			s.Logger.Warn("mime-repair: zero-byte office document is unrecoverable",
				zap.String("documentID", doc.ID.String()),
				zap.String("displayName", doc.DisplayName),
				zap.String("mimeType", doc.MimeType),
				zap.String("reason", "content was never stored or was overwritten by an accepted empty save"))
		case bytes.HasPrefix(content, zipMagic):
			if err := s.Repo.UpdateMimeType(ctx, doc.ID, canonical); err != nil {
				sum.Errors++
				s.Logger.Error("mime-repair: relabel failed",
					zap.String("documentID", doc.ID.String()),
					zap.Error(err))
				continue
			}
			sum.Relabeled++
			s.Logger.Info("mime-repair: relabeled office document",
				zap.String("documentID", doc.ID.String()),
				zap.String("displayName", doc.DisplayName),
				zap.String("oldMime", doc.MimeType),
				zap.String("newMime", canonical))
		default:
			sum.SkippedNotOffice++
			s.Logger.Warn("mime-repair: office-named content is not a zip; skipping",
				zap.String("documentID", doc.ID.String()),
				zap.String("displayName", doc.DisplayName),
				zap.String("mimeType", doc.MimeType))
		}
	}

	s.Logger.Info("mime-repair: completed",
		zap.Int("relabeled", sum.Relabeled),
		zap.Int("unrecoverable", sum.Unrecoverable),
		zap.Int("skippedNotOffice", sum.SkippedNotOffice),
		zap.Int("errors", sum.Errors))
	return sum
}
