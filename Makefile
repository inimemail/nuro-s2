.PHONY: build build-backend build-frontend build-datamanagementd test test-backend test-frontend test-frontend-critical test-frontend-full test-rust test-datamanagementd secret-scan

FRONTEND_CRITICAL_VITEST := \
	src/views/auth/__tests__/LinuxDoCallbackView.spec.ts \
	src/views/auth/__tests__/WechatCallbackView.spec.ts \
	src/views/user/__tests__/PaymentView.spec.ts \
	src/views/user/__tests__/PaymentResultView.spec.ts \
	src/components/user/profile/__tests__/ProfileInfoCard.spec.ts \
	src/views/admin/__tests__/SettingsView.spec.ts

# 一键编译前后端
build: build-backend build-frontend

# 编译后端（复用 backend/Makefile）
build-backend:
	@$(MAKE) -C backend build

# 编译前端（需要已安装依赖）
build-frontend:
	@cd frontend && corepack pnpm run build

# 编译 datamanagementd（宿主机数据管理进程）
build-datamanagementd:
	@cd datamanagement && go build -o datamanagementd ./cmd/datamanagementd

# 运行测试（后端 + 前端）
test: test-backend test-frontend

test-backend:
	@$(MAKE) -C backend test

test-frontend:
	@cd frontend && corepack pnpm run lint:check
	@cd frontend && corepack pnpm run typecheck
	@$(MAKE) test-frontend-critical

test-frontend-critical:
	@cd frontend && corepack pnpm exec vitest run $(FRONTEND_CRITICAL_VITEST)

test-frontend-full:
	@cd frontend && corepack pnpm run lint:check
	@cd frontend && corepack pnpm run typecheck
	@cd frontend && corepack pnpm run test:run
	@cd frontend && corepack pnpm run build

test-rust:
	@cd edge-rs && cargo fmt --check && cargo check --locked && cargo test --locked && cargo test --release --locked && cargo clippy --all-targets --locked -- -D warnings

test-datamanagementd:
	@cd datamanagement && go test ./...

secret-scan:
	@python3 tools/secret_scan.py

# Full upgrade gates; requires the declared toolchain and isolated integration services.
.PHONY: check-upgrade
check-upgrade:
	@python3 tools/check_upgrade.py $(UPGRADE_CHECK_ARGS)
