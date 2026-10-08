# v0.2.14 Nuro 手工升级交付与验证记录

日期：2026-10-08。基线：`a57dc5ff72afd5a44bb49ae2d49bbd146da2fe85`（`fix 0.2.13`）。本次修改未提交、未部署、未发布。

上游比较范围：v0.2.13 `3040209f205472038c1ba745a1bedd2edd9053b1` → v0.2.14 `0363b8cdba8cec3e2ba4b2dbd49c4481143fa55d`，8 个提交、25 个文件。通过临时目录核对差异后按本地实现手工适配，没有 merge、rebase、cherry-pick 或整文件覆盖。

后端版本为 `v0.2.14-nuro.1`，前端为 `0.2.14-nuro.1`。Rust 的独立组件版本、源码和依赖保持原样。

## 已实施项目

| 范围 | 最终行为与本地适配 |
| --- | --- |
| EasyPay 通知 | 签名前校验固定字段白名单，拒绝未知字段（含空值）、重复字段及非法编码；加入上游签名重用攻击和本地重复参数测试。保留退款兼容、API base 归一化和商户 metadata。 |
| 支付对账 | QueryOrder 必须返回成功 HTTP 状态及业务 code，拒绝超限响应；paid 金额必须有效、有限且大于零。数据库故障保留重试，仅真正不存在的已验证订单按原合同确认。继续使用原服务层金额与商户快照校验、幂等到账逻辑。 |
| 支付回跳 | 移除客户端 query/fragment；URL 构建器只加入内部 order_id、out_trade_no、resume_token、status。保留同站校验、签名 token 与微信续付流程。 |
| 回调日志 | 验证失败只记录固定原因、HTTP 方法和正文长度，不记录原文、签名、token 或 provider 返回的任意错误文本。 |
| 首次管理员创建 | 新安装默认随机邮箱，密码统一按 UTF-8 的 8–72 字节校验；邮箱与登录校验一致。事务内取得 PostgreSQL advisory lock 后重新统计用户，双副本只创建一个管理员；凭证在 commit 成功后才输出。 |
| 已有实例 | 已有管理员或其他用户时沿原保护逻辑跳过建号，不生成或重新校验新凭证，不重置历史邮箱、密码、角色和并发。standard 管理员并发 5、simple 30 保留。 |
| 部署脚本 | 新安装检查随机输出及失败状态，凭证生成成功后才写 .env；已有 .env 保留。Nuro 新安装将随机邮箱持久化；按用户要求撤回访问提示改动，恢复原二开的“管理员账号／密码”文案与 fallback，部署目录提示保留原样。四套 Compose 移除固定邮箱 fallback。 |
| Codex 远程目录 | 仅 OpenAI Codex HTTP/WS 增加“当前配置／远程目录”分段选择，默认当前。重新打开或切换 key、分组、平台、base URL 时重置。remote 输出 model_catalog_url 和 api_key_model_discovery，沿用现有 backend manifest 服务和缓存。 |
| 配置生成 | URL API 保留部署路径前缀、移除 query/fragment、拒绝非 HTTP(S) 和 URL 内凭证；TOML 字符串安全转义、auth.json 使用 JSON.stringify。普通默认配置保持一致，Grok 原生与 OpenCode 配置保留。 |
| 前端样式 | 目录选择使用现有图标、主色与暗色样式；窄屏客户端标签可滚动，文件名可换行。安装页补齐密码字节计数、错误状态与无障碍关联，窄屏步骤标签和圆点稳定排布；同步中英文文案。 |
| 前端依赖 | 使用 pnpm 9.15.9 重算本地 lock 并 frozen install；Vue 3.5.43、axios 1.20.0、DOMPurify 3.4.16、source-map-js 1.2.2。保留其他二开依赖和既有 override。 |
| 安全 CI | Go 版本校验从 go.mod 读取；audit 空报告、错误报告、缺失高危详情均失败。移除过期 axios/lodash 例外，仅保留有责任人、期限及使用边界的 XLSX 例外；新增门禁和部署凭证测试。 |
| 文档／版本 | 同步中文、日文、新安装及升级凭证说明；不存在的 APPLE_CONTAINER.md 未创建替代文件。使用本地 Nuro 发布版本，避免复制上游滞后的 VERSION。 |

实施时 govulncheck 在 Go 1.26.5 标准库发现 6 项可达漏洞，因此将 go.mod 和 Docker Go 构建阶段最小补丁升级为 1.26.6。复扫无可达漏洞；未改 go.sum 或 Go 模块依赖。这是本次实测发现的安全补修，不是上游 v0.2.14 的模块变化。

## 保护区复核

缓存创建 ABCD、下游缓存 markup、原始 usage／真实 billing 隔离、sticky／24h retention、强隔离、attempt／execution-unknown／retry budget、scheduler／RCU／健康恢复、Cell／escrow／lease／WAL、Edge relay／stream session／lane pool／首 Token 与媒体 timeout 的生产实现无 diff。

批准方案 R 节列出的 21 个冻结文件均无 diff；所有数据库迁移、Rust 源码、Cargo.toml／Cargo.lock、backend/go.sum 无 diff。本次没有迁移、价格调整、模型能力调整或调度阈值调整。目录选择只生成客户端配置，不预取目录，也不自动启用分组 pinned manifest；显式 remote 的客户端启动目录请求仍产生正常网络开销。

## 验证结果

| 检查 | 结果 |
| --- | --- |
| Go 1.26.6 全量 unit | `go test -tags unit ./...` 通过。 |
| Go 静态检查／构建 | `go vet ./...`、`go build -tags embed ./...` 通过。 |
| Go 定向 race | service、repository、handler、setup 的支付、安装及冻结区回归通过。 |
| 真实 PostgreSQL 安装测试 | 隔离容器、两个独立连接并发只生成一个管理员；锁等待取消测试通过。 |
| Go 完整 integration | 审查后修正四项旧测试，`go test -tags integration ./... -count=1 -timeout=3m` 不跳过任何包通过，最终日志 `/private/tmp/nuro-go-integration-final-v0214.log`。 |
| Embedded frontend | `go test -tags 'unit embed' ./internal/web` 通过。 |
| 前端全量 | 176 个文件、1086 项测试通过；typecheck、lint、production build 通过。 |
| 前端安全回归 | DOMPurify HTML／SVG XSS、配置转义、remote 默认与 URL 边界、安装密码字节边界通过。 |
| 浏览器视觉验收 | Playwright 实际组件、真实 CSS/i18n，1440×1000、390×844、320×740；浅／深色截图核对，无页面错误及横向溢出；72 字节通过、73 字节禁用继续。安装连接使用 mock，未连接生产。 |
| Rust | locked check、clippy（-D warnings）通过；debug／release 各 227 项测试通过。 |
| Go 漏洞扫描 | Go 1.26.6：0 项可达漏洞、0 项 imported package 漏洞；14 项仅模块层、未导入的提示仍记录在扫描日志。 |
| 前端依赖审计 | 门禁通过；2 项 high 为下述有限例外，另有 13 moderate、2 low 的传递依赖提示，不宣称前端零漏洞。 |
| 部署／审计门禁 | Bash 语法、四套 Compose config、安全配置检查、随机凭证失败／重复安装保留／下载失败／文件权限测试及 7 项 audit checker 测试通过。 |
| Docker 完整镜像 | 已完成 linux/arm64 完整构建（Go、embedded frontend、当前源码 Rust Edge），镜像 `nuro-sub2api:v0.2.14-nuro.1-review`。第二轮修复后已重建并验收，Docker inspect ID `sha256:fa3802297aad4afefdab94ca8215061af32c99b5fc6c0e49214d0f98d86482ca`，运行内容 manifest `sha256:5b6cbc1b0cf90c9594207b097582f55d872df628a1559683927694ee7e409fbc`。最终日志 `/private/tmp/nuro-docker-build-second-review-v0214.log`。 |
| 完整镜像双副本验收 | 独立 PostgreSQL／Redis／网络，两副本健康；空库仅一个随机管理员，生成凭证两边均可登录；embedded frontend JS 返回 200，配对 Edge 运行；重启后登录成功且仍只有一个用户。临时容器／网络已清理。日志 `/private/tmp/nuro-image-smoke-v0214.log`。 |
| Diff | `git diff --check` 通过。 |

浏览器截图保存在本机 `/private/tmp/nuro-ui-check/`，各项测试日志位于 `/private/tmp/nuro-*-v0214.log`。这些是本机临时验证产物，不进入发布包。

## 追加代码审查与修复

- 支付 POST 回调改为读取最多限制 + 1 字节，超出 1 MiB 明确拒绝，避免截断后验签忽略尾部；GET 使用同一限制。日志不再记录未验签订单号或任意失败文本。边界和请求拒绝测试通过。
- 支付 URL canonicalizer 与内部 builder 拒绝 URL 内用户名／密码，builder 同时拒绝非 HTTP(S)，新增回归通过。
- 新 .env 从创建时即使用 umask 077，消除写入到 chmod 之间的宽权限窗口；补 Edge 密钥生成失败显式退出；通用部署下载使用 curl -f，HTTP 错误不创建凭证文件。新安装、失败及已有 .env 保留测试通过。
- audit 门禁校验 High／Critical 总数与详情数一致，部分缺失报告拒绝；兼容 advisories 和 vulnerabilities schema，7 项测试及真实报告均通过。
- 四项旧集成测试在升级前 HEAD 复现后按现有合同修正：outbox 按当前账号计数避免其他测试数据污染；max=0 明确不限且不分配 slot；两个满队列测试使用 100ms deadline，验证取消和未落盘／丢弃状态。只改测试，没有改冻结的生产并发、调度和 usage 实现。全量集成不跳过通过。
- Docker Rust 1.90 arm64 Alpine 镜像中自包含 libc.a 为损坏文件；新下载的同版本标准库归档正常。Dockerfile 增加 ar 完整性检查，仅损坏时重装相同工具链标准库；版本及 Cargo 锁不变，完整构建通过。
- 最终完整 Go unit／integration、支付／安装／handler／provider 全包 race、go vet 与 embed build 均通过。前端／Rust 源码在追加审查中无修改，前轮全量及视觉验证仍有效。
- 完整镜像中的 pg_dump／psql 实测为 PostgreSQL 18.6；Edge 为可执行静态二进制，实际 paired 进程和重启已验收。该轮 diff 检查通过；后续扩大审查的新增问题与修复见下节。

## 第二轮扩大审查

扩大到支付实例解析、旧订单兼容、服务层第二次查询、第三方 HTTP 响应，以及部署下载、文件系统失败和共享健康异步测试前提。新增修复如下：

| 级别／问题 | 修复与回归 |
| --- | --- |
| 高：回调 provider 查询出错时应答成功，数据库故障可能使支付平台停止重试 | provider 解析失败返回 503，不宣称通知已验证；仅真正不存在的订单仍按原已验签合同成功应答。handler 用窄接口接入真实调用链测试，覆盖查询故障、缺少 provider、到账故障、已验签未知订单和正常支付。 |
| 高：confirmPayment 第二次读取订单的所有错误被吞掉 | 真正不存在返回 ErrOrderNotFound；数据库断开等错误向上传播，handler 保留 500 重试。旧 out_trade_no 查询失败也不再错误回退到其他 provider。使用真实 SQLite client 关闭注入故障验证，两类错误不混淆。没有改变到账事务、金额校验或幂等规则。 |
| 中：EasyPay HTTP 错误正文或截断的响应可被当作正常查询结果 | Create／Query 路径拒绝非 2xx；postRaw 读取限制 + 1 字节，超限明确拒绝。新增 HTTP 500 携带成功 JSON、1 MiB 合法前缀后附非法尾部的回归；退款原有专用状态处理保留。 |
| 中：配置生成途中失败留下部分 .env，后续安装跳过 | 两条部署路径在 0600 临时文件中完成生成和全部字段更新后才原子改名。Nuro 字段替换的临时文件位于目标同目录，失败清理；scheduler 配置补写显式传播每一步失败，即使外层条件抑制 Bash errexit 也不会漏报。默认字段值和已有凭证不变。 |
| 中：通用部署脚本接受缺少凭证字段的下载模板 | 下载暂存并检查 Compose services、四个必需凭证键，失败不产生目标 .env。新增缺字段、替换失败、早期 scheduler 写失败、后处理失败测试；失败后无目标或临时 .env 残留。 |
| 中：共用 .env 字段替换函数对值中的反斜杠二次转义 | awk 从 ENVIRON 读取原值，避免 -v 将字面量反斜杠转成换行、制表等。测试验证自定义密钥内容、其他字段和文件行数均保留。 |
| 测试稳定性：共享健康传播用例在订阅未建立前发布唯一事件 | 首轮全量 unit 复现一次超时，定向连续三次通过。测试改为先确认两个 Redis 订阅已就绪，避免初次空状态加载与发布竞态；生产健康调度源码未改。修复后连续 20 次 race 及最终全量 unit 通过。 |

第二轮最终验证：完整 `go test -tags unit ./...` 与 `go test -tags integration ./... -count=1 -timeout=3m` 通过，支付 handler／service／provider 定向 race、`go vet ./...` 通过；真实 audit 报告及 7 项门禁测试通过。前端与 Rust 生产源码本轮无新增改动，前轮完整测试和视觉结果继续有效。新镜像重新执行双副本、唯一管理员、双方登录、内嵌前端、配对 Edge、重启保留账号验证并通过；容器和网络已清理。冻结文件、Rust／迁移／go.sum 无 diff，`git diff --check` 通过。

最终日志：`/private/tmp/nuro-go-unit-second-review-final-v0214.log`、`/private/tmp/nuro-go-integration-second-review-v0214.log`、`/private/tmp/nuro-race-second-review-v0214.log`、`/private/tmp/nuro-health-race-second-review-v0214.log`、`/private/tmp/nuro-image-smoke-second-review-v0214.log`。这些验证没有连接生产或真实支付渠道，也没有提交、推送或上线。

部署测试已在本机 Bash 3／BSD sed 和只读挂载仓库的 Linux Bash 5／GNU sed 中通过；GitHub token 安装测试在 Linux 通过。日志 `/private/tmp/nuro-deploy-linux-second-review-final-v0214.log`；最新反斜杠保留补修另记 `/private/tmp/nuro-deploy-linux-literal-review-v0214.log`。

## 最后一轮全面复核

再次逐项检查支付错误传播／旧订单兼容、setup 事务与已有凭证保护、Codex 默认配置与分组重置、部署失败恢复、依赖锁及 CI。业务代码未新增修改；补修一处安全门禁边界：同一报告同时包含两个非空 audit schema 时，主集合总数校验与跨集合去重可能忽略相同 advisory 的严重性冲突。现在明确拒绝混合报告，仍接受任一单独 schema 及空的替代集合。新增两项测试，9 项门禁测试与既有真实 audit 报告均通过。

- 使用 `git show a57dc5ff72afd5a44bb49ae2d49bbd146da2fe85:<path>` 读取基线，与批准方案 R 节 21 个冻结文件逐字节比较，全部一致。
- 迁移、ent 生成文件、go.sum、整个 Rust 源码／依赖、二开账号／分组组件与界面无 diff；缓存 ABCD／markup、真实 usage／billing、调度、强隔离、Cell／escrow／lease／WAL、首 Token 和媒体超时生产实现保持原样。
- 专门执行 service／handler／repository 的二开定向 race：缓存、强隔离、attempt、runtime-clear、scheduler、settlement、首 Token、placeholder、倍率、usage billing、admission、escrow、lease、Responses Lite、continuation、stream session 等匹配测试通过；三个包分别 13.983s／4.518s／2.417s。
- 前端再次全量 176 文件／1086 项测试通过，typecheck 与 lint:check 通过；覆盖账号创建／编辑／批量保存、缓存 markup、独立超时阶段、分组调度、usage 展示及新配置／安装界面。
- Rust 再次 `cargo test --release --locked`：227 项全部通过；本轮没有改源码、版本或依赖。
- 部署凭证与异常路径测试、Compose 安全检查、`git diff --check` 通过。上轮最终完整 Go unit／integration、Go vet、完整镜像和双副本启动／重启验收仍对应当前业务代码；本轮仅改未打入运行镜像的 audit checker 和测试，未重复构建相同业务镜像。

本轮日志：`/private/tmp/nuro-frozen-race-last-review-v0214.log`、`/private/tmp/nuro-frontend-last-review-v0214.log`、`/private/tmp/nuro-frontend-static-last-review-v0214.log`、`/private/tmp/nuro-rust-last-review-v0214.log`。未发现本轮剩余待修复的业务回归；下面列出的有限依赖例外、真实渠道和生产验证边界仍适用，不能把本地检查通过描述为生产零风险。

## 凭证确认后的最终复核

用户最终确认保留随机邮箱，撤回访问提示文案变化。`show_access()` 整个函数已与升级前 HEAD 字符串比较，完全一致；新安装仍由 Nuro 脚本生成 24 位随机密码、写入 .env，部署完成直接显示实际邮箱／密码，无需翻容器日志。新增测试断言正常安装的访问输出不含“请查看”备用提示；重复创建的 .env 校验和保持不变。deploy/README.md 明确区分 Nuro 一键脚本与留空凭证的手工 Compose 流程。

本轮再次复核所有累计差异（支付、setup、前端功能／样式、依赖锁、部署、CI、版本、文档及测试），没有新增业务代码改动。另补一项门禁校验：同一 audit schema 内，相同包／advisory 的重复条目若严重性不同，原先去重可能跳过后一条更高严重性；现在直接拒绝，严重性一致的重复条目仍支持。两项新增回归、共 11 项 checker 测试、真实 audit 报告、部署凭证／失败路径和 Compose 安全测试全部通过，diff 检查通过。

21 个冻结文件再次逐字节与升级前基线比较，全部一致；其他 backend 生产差异仅为批准的支付、setup 和最小 Go 安全补丁；Rust、迁移、ent、go.sum、二开账号／分组界面、真实计费及 usage 实现无差异。先前全量 Go unit／integration、定向 race、前端 1086 项／Rust 227 项、静态检查、镜像双副本验收仍对应当前业务源码。本轮仅补 audit checker、测试和文档，未重复执行不受影响的业务全量测试或重建镜像。未发现本轮剩余待修复的业务回归；下面的外部验证与依赖例外仍保留。

## 审计例外与剩余门禁

- XLSX `GHSA-4r6h-8v6p-xvw6`、`GHSA-5pgg-2g8v-p4x9`：当前只有 UsageView 管理导出写路径，没有 XLSX.read 或上传解析。责任人为 `inimemail/nuro-s2 maintainers`，到期 `2027-01-06`；添加任何读取路径前必须重评，不能视为漏洞已修复。
- 13 moderate／2 low 主要来自 @lobehub/icons 的 mermaid、uuid、yaml、colord、decode-uri、katex 等传递链；本轮保留高危逐项门禁，没有扩展 blanket 例外。
- `cargo fmt --check` 在冻结的 main.rs 上发现基线已有格式差异；遵守保护范围，未自动格式化该文件。
- `deploy/tests/install-github-token-test.sh` 已在只读挂载仓库的 Bash 5 隔离容器通过，本机 Bash 3 跳过项已补齐。
- 本机完整镜像验证为 linux/arm64；生产架构的构建与外部 GitHub CI 仍由发布流水线执行，不能将本机成功当作所有架构已验证。
- 真实第三方支付通知／退款／续付、实际 Codex 客户端远程目录、生产 canary 和高并发压测未执行。发布前需在隔离环境验收合法通知与拒绝未知字段的渠道兼容，不能通过放宽白名单恢复攻击入口。

## 发布前操作

1. 在网络可用环境完成完整 Docker 构建及 CI，记录镜像 digest、源码 commit 和三组件构建信息。
2. 备份数据库、旧镜像 digest、.env、Compose 与 HAProxy 配置；使用一致新版本启动副本，已有凭证保留。本次没有数据库迁移，不清 migration ledger 或 Redis。
3. 在隔离新库验证并发初始化；在已有实例确认管理员和用户数据保持原样。凭证仅由授权管理员保存，不复制到工单／日志报告。
4. 验收真实支付合法回调、QueryOrder 对账、重复到账防护、退款和微信续付；使用 HTTP／WS Codex 客户端验证 remote 与分组隔离。
5. Canary 对比错误率、429／5xx／fallback、首 Token p50／p95／p99、缓存命中／sticky、slot／permit／WAL 与账本幂等指标；不能以错误率升高换取延迟改善。通过后再扩大部署，必要时按旧镜像及原配置回滚。
