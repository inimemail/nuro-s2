# 自用

## OpenAI edge-rs data plane

`edge-rs/` is an optional Rust data-plane entrypoint for OpenAI streaming hot
paths. Go remains the control plane: API key auth, billing checks, user/account
concurrency slots, account scheduling, priority, sticky routing, pool-mode
retry, soft cooling, content moderation, error policy, and usage recording all
stay in Go.

Current relay coverage:

- `/v1/chat/completions` and `/openai/v1/chat/completions`, only
  `stream=true`, only raw OpenAI API-key accounts that do not require Go-side
  Chat/Responses conversion.
- `/v1/responses` and `/openai/v1/responses`, only `stream=true`, only native
  OpenAI API-key passthrough accounts with Responses API enabled.
- Responses WebSocket (`Upgrade: websocket`) is enabled by the one-click edge
  env by default, but only relays when the selected account resolves to WSv2
  passthrough mode without a proxy. Go still owns ctx_pool/shared/dedicated
  stateful modes.
- Upstream 401/403/429/5xx before the first client write calls Go `/retry`;
  Go decides same-account retry, account switch, soft cooling, or fallback.
- Successful streams call Go `/complete` for usage recording and slot release;
  failures/client aborts call `/abort`, with lease TTL as crash recovery.
- The Rust edge now executes relay plans through bounded account/proxy/host
  worker queues, uses prewarmed SSE scratch buffers, prefers raw body bytes over
  JSON re-materialization for upstream relay, and can keep unused preconnected
  WSv2 passthrough sockets per account/header key.

Unsupported or risky paths intentionally fall back to the existing Go gateway:
OAuth/ChatGPT conversion, Responses WebSocket ctx_pool/shared/dedicated stateful
routing, WS proxy accounts, non-streaming requests, image generation, multipart,
`/responses/compact`, `previous_response_id`, `function_call_output`, and any
request outside the configured rollout gates.

Rollout controls live under `gateway.openai_edge_rs`: enable/mode,
`ingress_proxy_enabled`, `relay_chat_completions`, `relay_responses`,
`relay_responses_websocket`, `rollout_percent`, `allowed_api_key_ids`,
`allowed_group_ids`, and `allowed_models`.

The Linux one-click systemd installer installs `sub2api-edge-rs`, writes
`/opt/sub2api/edge-rs.env`, registers `sub2api-edge-rs.service`, and enables Go
ingress proxying for eligible OpenAI hot paths. If the edge process is missing
or down, Go restores the request body and continues through the existing gateway
path.
