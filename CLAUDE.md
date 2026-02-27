# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Alkemio **file-service** — a NestJS microservice that serves files with authentication/authorization via RabbitMQ. It acts as a secure file proxy: receives HTTP requests, validates access through a RabbitMQ message queue (communicating with the main Alkemio server), then streams files from local storage.

## Commands

```bash
npm run build          # Compile TypeScript (nest build → dist/)
npm run start:dev      # Development with file watching
npm run start:debug    # Debug mode (debugger on 0.0.0.0:9227)
npm run start:prod     # Production (node dist/main)
npm run lint           # Type-check + ESLint (tsc --noEmit && eslint src/**/*.ts{,x})
npm test               # Run Jest tests
```

## Architecture

**HTTP adapter**: Fastify (not Express). Uses `@nestjs/platform-fastify` with plugins: `@fastify/helmet`, `@fastify/cors`, `@fastify/cookie`, `@fastify/etag`.

**Modules** (registered in `src/app.module.ts`):
- **FileModule** (`src/services/file-reader/`) — core file serving logic
- **HealthModule** (`src/services/health/`) — health check endpoint

### Request Flow

1. `GET /rest/storage/document/:id` with auth headers (Authorization, Cookie, x-guest-name)
2. `FileController` → `FileService.readDocument()`
3. `FileAdapterService.fileInfo()` sends RabbitMQ message to auth queue, gets back file metadata + authorization result
4. On success, reads file from local storage path and returns `StreamableFile` with cache headers

### Key Services

- **FileController** (`file.controller.ts`) — single endpoint, maps `FileInfoErrorCode` to HTTP status codes (403/404/500)
- **FileService** (`file.service.ts`) — orchestrates auth check and file reading
- **FileAdapterService** (`file.adapter.service.ts`) — RabbitMQ client proxy (`ClientRMQ`), sends/receives messages on the auth queue

### Configuration

Loaded from `config.yml` with environment variable substitution (`${VAR_NAME}:default_value` syntax). Key settings:
- RabbitMQ connection (host, port, user, password)
- `LOCAL_STORAGE_PATH` — where files are stored (default: `../server/.storage`)
- `PORT` — HTTP port (default: 4003)
- `AUTH_QUEUE` — RabbitMQ queue name (default: `alkemio-files`)
- `DOCUMENT_MAX_AGE` — cache TTL in seconds (default: 86400)

Config types defined in `src/config/config.type.ts`.

### Error Handling

Custom exception hierarchy: `BaseException` → `FileInfoException` / `FileReadException` / `LocalStorageReadFailedException`. Global `BaseExceptionFilter` formats all errors as JSON with errorId (UUID), stack traces only in non-production.

### Logging

Winston via `nest-winston`. Console-only transport. JSON format in production, colorized NestLike format in development. Configured in `src/config/winston.config.ts`.

## Tech Stack

- **Runtime**: Node 22 (pinned via Volta: 22.20.0)
- **Framework**: NestJS 11 with Fastify
- **Language**: TypeScript 5, target ES2021, CommonJS modules
- **Messaging**: RabbitMQ via `amqp-connection-manager`
- **Code style**: Prettier (single quotes, trailing commas), ESLint

## Docker

Multi-stage build: node:22-bookworm (build) → distroless/nodejs22-debian12:nonroot (runtime). Exposes port 4003.
