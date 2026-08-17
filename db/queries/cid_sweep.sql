-- name: ListCIDCandidates :many
SELECT "externalID", COUNT(*) AS reference_count
FROM file
WHERE "externalID" ~ '^Qm[1-9A-HJ-NP-Za-km-z]{44}$'
GROUP BY "externalID"
ORDER BY "externalID";

-- name: ListCIDCaseAliases :many
SELECT "externalID", COUNT(*) AS reference_count
FROM file
WHERE char_length("externalID") = 64
  AND "externalID" ~ '^[0-9A-Fa-f]{64}$'
  AND "externalID" <> lower("externalID")
GROUP BY "externalID"
ORDER BY "externalID";

-- name: UpdateCIDGroup :execrows
UPDATE file
SET "externalID" = sqlc.arg(lowercase_target)
WHERE "externalID" = ANY(sqlc.arg(exact_aliases)::text[])
  AND "externalID" <> sqlc.arg(lowercase_target);

-- name: CountCIDAliasReferences :one
SELECT COUNT(*)
FROM file
WHERE "externalID" = sqlc.arg(exact_alias);
