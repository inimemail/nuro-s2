# Asynchronous Image Tasks

Clients can submit long-running OpenAI-compatible image requests without keeping one HTTP connection open. The dedicated async endpoints use the ordinary Images request schema, so downstream source code does not need private `taskrun` or `taskid` fields.

## Endpoints

```text
POST /v1/images/generations/async
POST /v1/images/edits/async
GET  /v1/images/tasks/{task_id}
```

The no-prefix aliases `/images/generations/async`, `/images/edits/async`, and `/images/tasks/{task_id}` are also available. Generation accepts the same JSON body as `/v1/images/generations`; edits accept the same JSON or multipart body as `/v1/images/edits`. `stream=true` is rejected because polling returns one final JSON result.

The older `/v1/image-tasks/*` API and `taskrun=true` compatibility fields remain supported for existing clients.

## Requirements

Async tasks use PostgreSQL for durable queue state and an S3-compatible object store for completed image data. Configure `image_storage` in `config.yaml`; the async endpoints fail closed with `503` when durable task storage or object storage is unavailable. Synchronous Images behavior is unchanged.

```yaml
image_storage:
  enabled: true
  endpoint: "https://<account>.r2.cloudflarestorage.com"
  region: "auto"
  bucket: "generated-images"
  access_key_id: "..."
  secret_access_key: "..."
  prefix: "images/"
  force_path_style: false
  public_base_url: ""
  presign_expiry_hours: 24
  max_image_bytes: 33554432
```

## Submit And Poll

Use an idempotency key when a client may retry an ambiguous submission:

```bash
curl -i https://api.example.com/v1/images/generations/async \
  -H 'Authorization: Bearer sk-...' \
  -H 'Idempotency-Key: order-20260716-42' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-1","prompt":"A lighthouse during a winter storm"}'
```

A new task returns `202 Accepted`, `Location`, `Retry-After`, and a body like:

```json
{
  "id": "imgtask_0123456789abcdef",
  "task_id": "imgtask_0123456789abcdef",
  "object": "image.generation.task",
  "status": "processing",
  "poll_url": "/v1/images/tasks/imgtask_0123456789abcdef"
}
```

Poll with the same API key:

```bash
curl https://api.example.com/v1/images/tasks/imgtask_0123456789abcdef \
  -H 'Authorization: Bearer sk-...'
```

The terminal status is `completed` or `failed`. A completed response includes the ordinary synchronous Images response under `result`; generated data is uploaded to object storage and returned as compact URLs. Failed responses contain only a sanitized error and never expose the upstream URL, provider diagnostics, HTML body, or credentials.

Task ownership is scoped to the submitting API key. Unknown and cross-key task IDs both return `404`. The queue reclaims interrupted work after the configured lock timeout, and finished tasks are removed after the configured retention period.
