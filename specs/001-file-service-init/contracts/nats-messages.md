# NATS Message Contract: auth.evaluate

File-service-go is a **client** of the authorization-evaluation-service via NATS request-reply.

## Subject

`auth.evaluate` (configurable via `NATS_SUBJECT` env var)

## Request (sent by file-service-go)

```json
{
  "pattern": "evaluate",
  "data": {
    "actorId": "<uuid>",
    "privilege": "read",
    "authorizationPolicyId": "<uuid>"
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `pattern` | string | yes | Always `"evaluate"` |
| `data.actorId` | UUID string | yes | Actor's ID (from JWT `alkemio_actor_id` claim) |
| `data.privilege` | string | yes | Always `"read"` for file serving |
| `data.authorizationPolicyId` | UUID string | yes | From Document record in Alkemio DB |

## Response (received from authorization-evaluation-service)

```json
{
  "allowed": true,
  "reason": "Actor has Reader credential for resource",
  "error": null,
  "metrics": {}
}
```

| Field | Type | Present | Description |
|-------|------|---------|-------------|
| `allowed` | boolean | always | Whether the actor has the requested privilege |
| `reason` | string | always | Human-readable explanation |
| `error` | object or null | optional | Structured error for transient failures |
| `error.code` | string | when error | e.g., `"circuit_breaker_open"`, `"dependencies_unavailable"` |
| `error.dependency` | string | when error | e.g., `"database"`, `"nats"`, `"nats,database"` |
| `error.retryAfterMs` | integer | when error | Suggested retry delay in milliseconds |
| `metrics` | map[string]float64 | optional | Diagnostic metrics (can be ignored) |

## Client Behavior

- Use `nats.Conn.RequestWithContext(ctx, subject, payload)` for synchronous request-reply
- Context timeout: 10 seconds (matching auth-eval-service's `EVALUATION_TIMEOUT_SECS` default)
- If `error` field is present with `retryAfterMs > 0`, the auth-eval-service is in degraded state — file-service should return 503 to the caller
- If NATS request fails (timeout, connection error), return 503 — never serve files without auth
