module github.com/alkem-io/file-service

go 1.26.1

require (
	github.com/davidbyttow/govips/v2 v2.18.0
	github.com/gabriel-vasile/mimetype v1.4.13
	github.com/go-chi/chi/v5 v5.2.5
	github.com/google/uuid v1.6.0
	github.com/jackc/pgerrcode v0.0.0-20250907135507-afb5586c32a6
	github.com/jackc/pgx/v5 v5.9.2
	github.com/nats-io/nats-server/v2 v2.12.11
	github.com/nats-io/nats.go v1.51.0
	github.com/pashagolub/pgxmock/v4 v4.9.0
	github.com/sony/gobreaker/v2 v2.4.0
	go.uber.org/zap v1.27.1
	golang.org/x/net v0.54.0
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0-default-no-op // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/nats-io/jwt/v2 v2.8.2 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/image v0.39.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/davidbyttow/govips/v2 => github.com/antst/govips/v2 v2.0.0-20260612014756-be0d7643869e
