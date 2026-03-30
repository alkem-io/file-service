-- name: GetDocumentByID :one
SELECT id, "externalID", "mimeType", size, "displayName", "createdBy",
       "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
       "createdDate", "updatedDate", version
FROM document
WHERE id = $1;

-- name: CreateDocument :one
INSERT INTO document (id, "externalID", "mimeType", size, "displayName", "createdBy",
                      "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
                      "createdDate", "updatedDate", version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 1)
RETURNING id;

-- name: UpdateDocumentFile :execrows
UPDATE document
SET "externalID" = $2, "mimeType" = $3, size = $4, "updatedDate" = $5
WHERE id = $1;

-- name: UpdateDocumentLocation :execrows
UPDATE document
SET "storageBucketId" = $2, "temporaryLocation" = $3, "updatedDate" = $4
WHERE id = $1;

-- name: DeleteDocument :one
DELETE FROM document
WHERE id = $1
RETURNING "authorizationId", "tagsetId";

-- name: CountDocumentsByExternalID :one
SELECT COUNT(*) FROM document WHERE "externalID" = $1;
