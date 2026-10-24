# v0.2.18-nuro.1 validation and rollout runbook

Baseline: `b4c3e9d766274bc062acb16ff9922897658a511a`. This upgrade was
manually adapted from upstream v0.2.17 to v0.2.18. No merge, rebase,
cherry-pick, deployment, migration backfill or production request was made.

## Completed local verification

| Check | Command or evidence | Result |
| --- | --- | --- |
| Protected contract | `python3 tools/check_upgrade_contracts.py` | Pass: all 20 protected files retain their baseline hashes; no newly introduced migration-prefix collision; go.mod and both Dockerfiles use 1.27.3. |
| Fence tests | `python3 tools/test_check_upgrade_contracts.py` | Pass: 2 tests. |
| Targeted upgraded Go behavior | `go -C backend test -tags unit ./internal/handler ./internal/service -run 'TestUpgrade(GeminiImageGate|VertexQuota)' -count=1` | Pass. Covers handler-side image rejection before selection, `countTokens` exemption, Vertex DTO/capability/settlement boundaries. |
| Grok price parity | `go -C backend test -tags unit ./internal/service ./internal/pkg/xai -run 'TestUpgradeGrok|TestCanonicalModelPricingSnapshot|TestV028DeepSeekImageAliases' -count=1` | Pass. Covers aliases and 199999/200000/200001 token boundaries. |
| Earlier full unit pass | `go -C backend test -tags unit ./...` | The run completed after the protocol changes. A later rerun exposed a native catalog ordering assertion; the assertion and order were corrected and the affected admin package passed. The final all-package rerun remains required below. |
| Earlier frontend static checks | `pnpm run lint:check`, `pnpm run build` | Passed before the final expanded-review fixes. Production assets were built successfully then; this is not a final-file-set pass. |
| Earlier frontend regression | `pnpm run test:run` | Passed 1184 tests before the final expanded-review fixes. Includes stale response, unmount, filter persistence, quota capability and account-stat coalescing coverage. |
| Earlier frontend focused quota UI | `pnpm exec vitest run src/components/account/__tests__/AccountUsageCell.spec.ts` | Passed 23 tests before the final expanded-review fixes. Confirms Gemini service-account quota bars coexist with local today statistics and that Anthropic service accounts do not display them. |
| Rust | `cargo check --locked`; `cargo test --locked`; `cargo test --locked --release`; `cargo fmt --check`; `cargo clippy --locked --all-targets` | Pass. Debug and release tests each passed 243 tests. |
| Cross-language edge stream | `NURO_EDGE_TEST_BINARY=.../edge-rs/target/debug/sub2api-edge-rs go -C backend test -race -tags unit ./internal/handler -run TestEdgeStreamRealRustTransportRecovery -count=1 -v` | Pass: six real Rust/Go TCP interruption and recovery variants. |
| Targeted race | `go -C backend test -race -tags unit ./internal/service ./internal/handler -run 'TestUpgrade|Test.*(SchedulerSnapshot|FreshLoad|PrepareCache|SharedBudget|Continuation|AttemptState)' -count=1` | Pass. |
| Shell syntax and deployment guard | `bash -n deploy/paired-entrypoint.sh deploy/docker-entrypoint.sh deploy/docker-deploy.sh`; `bash tools/test_deploy_admin_bootstrap.sh` | Pass. The bootstrap test only validates script behavior; it did not deploy. |
| Earlier dependency install | `pnpm install --frozen-lockfile --ignore-scripts` | Passed earlier; dependencies are currently incomplete after a later interrupted install. Final restoration is blocked as described below. |
| Earlier production dependency audit | `pnpm audit --prod --json` plus `tools/check_pnpm_audit_exceptions.py` | Passed the policy check with the two existing, explicitly scoped `xlsx` export-only exceptions. Counts: 0 critical, 2 high, 13 moderate, 2 low. The upgrade removes the former DOMPurify and follow-redirects production advisories. This is historical audit evidence, not a fresh advisory lookup. |

`GOCACHE=/private/tmp/nuro-v0218-go-build` and `GOTOOLCHAIN=go1.27.3`
were used for Go commands where needed.

## Expanded review — 2026-10-24

Reviewed the complete upgrade diff and relevant callers for protocol conversion,
disconnect drain and terminal output, usage/billing deduplication, platform-scoped
quotas, image permissions, pricing aliases, model/header caches, request-local
metadata, account-page request lifetimes, filter persistence and build/CI pins.
The 20 frozen-file hashes were also checked; hash equality alone does not prove
the behavior of every caller.

Confirmed findings were fixed:

- Compat TTFT ignores empty, ping and signature-only frames; actual text,
  thinking and tool output still start TTFT. Targeted regression tests cover it.
- Persisted account filters and platform selectors now include MiniMax,
  OpenCode Go and TypeSafe. The type selector also exposes upstream and service
  accounts, with English and Chinese service-account labels.
- `backend/Dockerfile` now matches the root Dockerfile and go.mod at Go 1.27.3.
  The protected-contract checker validates both Dockerfiles.
- Account queries use a shallow snapshot of their scalar parameters, avoiding
  a new `structuredClone` browser requirement while retaining stale-query guards.
- Cancelling an account-list request no longer consumes the following query's
  today-statistics refresh. The refresh now begins only after rows for the
  active query are committed; a Vue regression test covers the race.

Final backend targeted tests passed across service, handler, admin handler and
Antigravity packages, covering `TestUpgrade*`, both Anthropic streaming adapters,
model mapping, header overrides, pricing snapshots, DeepSeek image aliases and
Grok account model mapping. The matching targeted service/Antigravity race run
also passed. `go vet ./...`, server build and the built binary's `--help` check
passed. Shell syntax and deployment-bootstrap guard tests passed without a
deployment. No additional confirmed defect remains open from this review;
the environment-dependent checks below remain outstanding.

A dependency-free Node.js check against the final utility source passed for all
11 platforms from the actual `AccountPlatform` union, scoped filter persistence,
malformed/disabled storage and Gemini-only service-account quota eligibility.
This limited check does not replace Vue component tests, typecheck or a build.

## Required environment reruns

The full Go suite cannot complete in this sandbox because test fixtures cannot
bind loopback listeners. An attempt to run outside that restriction did not
execute because the automatic approval service disconnected. Run these in CI or
a local environment with Go 1.27.3 and loopback access:

```sh
GOTOOLCHAIN=go1.27.3 go -C backend test -tags unit ./...
GOTOOLCHAIN=go1.27.3 go -C backend test ./...
```

A non-escalated changed-package rerun also hit this environment's blocked
`127.0.0.1:0` listener while miniredis fixtures started. Those failures are
from socket setup, not assertion mismatches in the changed behavior. Run the
commands above where loopback listeners are permitted. Final vet and build have
already passed separately.

Final frontend checks are also required. `node_modules` currently lacks Vitest,
vue-tsc and ESLint. Offline restoration failed because the package store lacks
`@airwallex/components-sdk@1.30.2`; sandbox registry access is unavailable. The
network-enabled frozen install was not executed because automatic approval
review disconnected before completion (a review-service failure, not an unsafe
action determination). Dependency versions and the lockfile were not changed to
work around this failure. In an environment with registry access, run:

```sh
cd frontend
pnpm install --frozen-lockfile --ignore-scripts
pnpm run lint:check
pnpm run typecheck
pnpm run test:run
pnpm run build
```

The integration suite was invoked with `-tags integration`. Its harness checks
`docker info` and exits successfully without executing database tests when the
daemon is unavailable. Docker was installed but its daemon socket was absent:
`unix:///Users/inim/.docker/run/docker.sock`. Start Docker, then run:

```sh
GOTOOLCHAIN=go1.27.3 go -C backend test -tags integration ./...
```

This will execute the added platform-scoped Vertex quota settlement test against
temporary PostgreSQL and Redis containers.

## Security follow-up

The complete dependency-tree audit still reports development/build-chain
findings, including the pre-existing Vite/Vitest ecosystem advisories. They are
outside this narrow, compatibility-preserving upgrade and should be handled in
a separate dependency migration with its own browser/build regression plan.
Do not add broad exception suppressions. The remaining two production high
findings are the existing `xlsx` advisories. The project only writes exports;
the exceptions name the affected GHSA records, owner, mitigation and expiry in
`.github/audit-exceptions.yml`.

## Release sequence

1. Run the required Go, frontend and Docker-backed checks above on the candidate commit.
2. Build the release image and verify the image reports `v0.2.18-nuro.1`; the
   `Dockerfile`, `backend/Dockerfile`, and `backend/go.mod` all pin Go 1.27.3.
3. Apply the existing schema migrations through the normal release mechanism.
   No migration was added or renumbered for this upgrade.
4. Canary with a representative non-production account mix. Exercise Gemini
   image allowed/denied paths, `countTokens`, Vertex service-account quotas,
   Grok Fast at the 200k boundary, and Anthropic-compatible streaming aborts.
5. Compare error rate, billing settlement counts, first-token latency and
   stream completion rate with the current release. Roll back the image if any
   protected cache, sticky-routing, retry, billing or stream terminal invariant
   regresses.

Production shadow traffic, canary traffic, load testing, deployment and real
provider model calls were deliberately not performed from this workspace.

The browser automation approval service disconnected twice while opening a
localhost-only visual fixture for manual dark/light and narrow-width inspection.
The fixture never opened. Component rendering is instead covered by the focused
quota UI test and the production build listed above; perform the manual visual
check during candidate review.
