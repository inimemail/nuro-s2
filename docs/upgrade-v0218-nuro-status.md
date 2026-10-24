# v0.2.18-nuro.1 upgrade execution record

Baseline: `b4c3e9d766274bc062acb16ff9922897658a511a`.
Upstream comparison: `060d0160417782cd962a0c7f460f6cd4565d4916` → `ac1b8f1271be0d674fa6f074403c1410f30a3a81`.

## Scope and protection

Implement U1–U9, R1–R4 and O1–O6 from the accepted manual plan. Preserve all
20 frozen production files byte-for-byte, request/usage isolation, scheduling,
retry/attempt budgets, cache keys, retention, Edge routing and settlement.
No upstream merge, cherry-pick, production deployment or migration backfill.
Queued/metadata cross-language output classification remains deferred because
the Rust implementation is frozen. The upstream eight-second overload delay,
scheduler memo replacement, quota SQL refactor and remote image downloading
are excluded.

## Execution checklist

- [x] U1 security dependencies and Go/Docker toolchain
- [x] U2 Grok Fast pricing and aliases
- [x] U3 Gemini/Antigravity nonnegative ordinary input
- [x] U4 schema-node-aware Gemini normalization
- [x] U5 canonical DeepSeek image payloads
- [x] U6 bounded Anthropic compat disconnect drain and safe terminal
- [x] U7 Cloudflare 1010 JSON classification
- [x] U8 Gemini non-EOF residual rejection
- [x] U9 Gemini image permissions without default/backfill changes
- [x] R1 isolated Vertex quota capability and complete UI/settlement integration
- [x] R2 model-mapping signature optimization and benchmarks
- [x] R3 request-local lazy Antigravity single-account detection
- [x] R4 validated account-filter persistence
- [x] O1 header override scan benchmark and equivalent implementation
- [x] O2 stale-response/unmount guards and query-scoped ETags
- [x] O3 bounded same-batch account-statistics request coalescing
- [x] O4 dependency-preserving account-page initialization
- [x] O5 request-local parse reuse for newly added gates
- [x] O6 frozen-file and protocol regression gates

## Verification

The repeatable command list, observed outcomes and environment limitations are
in [v0218-validation.md](upgrades/v0218-validation.md). The frozen-file
manifest is [v0218-protected.json](upgrades/v0218-protected.json).

The expanded review on 2026-10-24 fixed compat TTFT classification, missing
account filter options/persistence values, the secondary Dockerfile toolchain
pin, a browser compatibility requirement in query snapshots, and a cancelled
account-list request consuming the next query's today-statistics refresh. Final backend
targeted/race tests, vet and server build passed. Final full Go, frontend and
Docker-backed checks remain limited by the test environment; earlier frontend
passes must not be treated as validation of the final review fixes. See the
validation runbook for exact outcomes and rerun commands.

Production shadow, canary, load tests and deployment require an isolated test
environment and operational access. Do not duplicate real model execution or
payment during shadow validation. No production capacity claim is made.
