# v0.2.15 Nuro 选择性升级交付记录

日期：2026-10-09。基线：`ac4b454632e9697b321fde28269195c25ccf27e7`。
后端版本 `v0.2.15-nuro.1`，前端版本 `0.2.15-nuro.1`。
本次修改未提交、未推送、未发布、未部署生产，未连接生产或真实支付渠道；仅启动隔离的本地验收容器。

上游比较：v0.2.14 `0363b8cdba8cec3e2ba4b2dbd49c4481143fa55d` →
v0.2.15 `f2669c8cf62555cd92389b3f55920e9e6e7c6ff2`。
按批准方案手工适配本地实现，没有整仓合并、cherry-pick 或整文件覆盖。
这不是上游所有功能的完整移植；已批准的暂缓和拒绝项保留如下。

## 已实施

| 任务 | 交付行为 |
| --- | --- |
| S01 | Go 1.27.2、x/net 0.60.0 及关联模块升级，三个 Docker 构建阶段统一；CI lint 2.14.0。本地 HTTP/2 超时、PING、GOAWAY 行为保留，仅对依赖弃用告警加精确排除。ent 生成工具在临时目录验证，不覆盖本地 runtime。 |
| M01 | 只对原 `invalid_encrypted_content` 或满足完整消息条件的 `thinking_signature_invalid` 明确 400 拒绝做原有一次恢复，不扩大 execution-unknown 重放。 |
| M02–M04 | Chat 指定 function tool_choice 扁平化；developer 角色保留；legacy function 同名 FIFO 配对，生成 ID 避免与已有 tool_calls 冲突。 |
| M05–M07 | refusal 同步、流式和缓冲转换保留，finish reason 为 content_filter，防重复 finalize；thinking 空 signature 字段保留；Anthropic Chat 缓冲 delta 双边界检查。native reader 本地已安全校验索引，保留。 |
| M08–M09 | mimic 保留合法客户端 tool-change beta，沿原 drop/block 规则；在 Messages、Chat、Responses、count_tokens 的既有 mimic 链修正 TTL 顺序，保留显式 1h，不改 API Key 或真实 Claude Code 客户端路径。 |
| M10、O01–O02 | Codex 普通和 pinned manifest 先按每账号 mapping 投影、再按分组 allowlist、再合并；保留有界缓存和固定账号顺序，输出变化重算 ETag，能力冲突安全回退，service_tiers 非法值转空数组，不污染共享原始目录。 |
| M12、O07 | Composite 公共 exact alias 进入目录；复用推理 route 优先级，一次 route 查询，Codex Responses 过滤 TypeSafe/SystemOne，保留分组 allowlist；返回完整保守 Codex manifest。查询只在目录接口，不加到推理选号链。 |
| M13、M15–M16、M20 | Antigravity 管理测试按公共映射 key 展示；裸 API 路径绕过 SPA；Zen qwen3.8-max 默认 Chat，Go 默认 Anthropic；标准 Zen 拒绝不支持模型，保留有效自定义规则和自定义 base URL；native Anthropic 模型 trim。 |
| M17–M18 | Grok 无 output/usage/error 的空 completed/done 沿原 failover，小请求 Chat bridge 也覆盖；usage 包括零 usage、refusal、tool、error 不误判。智谱监控官方/代理 URL 修复。 |
| M19 | 账号测试日志补 account/test/source/platform/type 归因，完整脱敏后截断，返回管理员测试错误也脱敏。 |
| M21、O03 | namespace 首次实际变化才重建，无变化保留原始 bytes，未知字段和大整数格式保留。 |
| M22–M24 | Grok SSO HTML tokenizer 提取 consent token，验证 consent URL，Origin/Referer 不跨 redirect；OpenCode usage Retry-After 限制 0–24h，403 保持中性状态。 |
| F01–F10 | 批量用户错误提示、通知邮箱请求/倒计时、两页零倍率、亚字节单位、公告失败状态、批量图片访问重试及用户隔离、cleanup 旧列表、disabled 下拉、OAuth 卸载、备份轮询生命周期修复。 |
| F11–F18 | step-up 并发共享验证并终结所有 waiter；Markdown 旧页、Stripe SDK/轮询/卸载、绑定邮箱草稿、计划测试结果、模板列表/草稿、OPS 分页、OPS 加载完成后保存等状态修复。 |
| F19–F26、O04/O06/O09 | 告警分钟整数校验；RPM 保留数字精度并拒绝非法编辑；Airwallex 卸载/身份保护；长上下文提示反映实际阶梯定价；错误规则采用服务端保存值；用户名新草稿/切换登录/卸载保护；倍率弹窗 group/session 隔离与加载失败禁止保存；默认价格仅新建空单模型条目可回填，并拒绝旧响应。 |
| F27 | 管理员及用户使用记录的延迟列固定显示首字、总耗时、输出 TPS，不能隐藏；TPS 使用文本输出 Token ÷ 请求总耗时，带 `tok/s` 单位，使用已加载 usage 数据，不新增查询；媒体、缺失或非法耗时显示 `-`，不声称生成阶段速率。 |
| X04 安全子集 | 根据本地实际 provider 支持显示渠道模型同步入口，同步请求隔离；补 Zen 协议、Composite 路由优先级等本地回归。 |

前端沿用现有组件、图标、颜色和暗色风格；倍率/RPM 弹窗手机操作区换行，表格在自身区域横向滚动，页面不溢出。
支付页面 query 变化重新挂载，旧 SDK 回调和轮询不得影响当前订单。

## 最终复核修复

- 分组倍率/RPM：同 ID 分组刷新不再重置草稿；保存按提交快照更新已保存状态，期间的新输入保留并继续显示弹窗；RPM 清空请求保留期间的新编辑。
- 通知邮箱：取消验证、卸载、切换身份后拒绝旧响应，包括同用户 A→B→A；同卡片的设置/邮箱写入一次一个，失败后恢复可操作，避免完整用户响应反序覆盖。
- 计划测试/OPS：创建、编辑、保存期间的新草稿不再被关闭或清空；计划开关禁止同计划重复并发提交。
- 本地双副本共享目录实测发现既有初始化竞态：第二副本重写只读 `.installed` 失败。配置和安装标记均完整写入临时文件后原子只创建，保留已有文件及 0600/0400 权限。16 路并发 race 测试与实际共享卷双副本冷启动通过；管理员生成规则不变。
- Docker 构建忽略本地 Go 缓存、临时目录和二进制；避免约 7 GB 本地产物进入构建上下文。前后两次上下文传输量受 BuildKit 缓存影响，不能将增量大小当作干净构建的完整大小。

再次扩大范围复核：

- 通知阈值：保存期间的新输入不会被完整用户响应或父组件刷新覆盖；保存成功且没有后续编辑时恢复同步，切换用户清除旧草稿。
- 通知邮箱开关：请求返回前保持已确认状态，失败不会显示实际未保存的开关状态；补失败回滚回归与真实浏览器检查。
- 共享初始化：配置原子替换仍允许后启动副本覆盖已启用的自动生成 JWT 密钥。配置也改为原子只创建，先发布的完整配置保留；16 路不同密钥并发、已发布配置不变、未设置 JWT_SECRET 的双副本跨登录与重启会话均通过。
- 扩大阅读模型目录/映射/Composite 路由、缓存 TTL、协议转换、重试、测试日志、支付回调及前端异步生命周期；其余未发现新的可复现阻断问题。保护功能另执行定向 race，未改其冻结实现。

最后一次全量差异复核：

- 渠道默认价格回填：按行号复用组件时，删除原条目可能让旧响应给内容相同的下一行填价。新增实际提交对象身份和平台校验；已有推理倍率也不再被判断为空定价。6 项回归覆盖正常回填、相同内容换行、手动零价格、倍率、平台切换及卸载；真实渠道页面删除行用例在手机/桌面深浅色通过。
- 最终后端全量单元测试重新执行；setup/service/handler/repository 全包 race 通过，Rust locked 227 项通过。全仓不截断静态诊断与升级前再次逐条比较，均 494 项，新增/消失均 0。
- 最终前端 183 文件、1125 项测试、lint、类型检查和生产构建通过；浏览器原 40 组与渠道额外 4 组通过，检查了手机/桌面截图。完整镜像重建并重新执行双副本启动/自动 JWT/测试库升级验收。

## 保留与暂缓

- R01–R04：保留本地科学计数价格格式、按 z-index 的顶层 Escape、既有 Chat heartbeat、秒级倍率探测及最后可信倍率逻辑。
- M11/O10：WS 图片输入 usage 暂缓整体移植。本地 Go WS relay 只有既有图片输出提取，Rust 原生数据面/settlement 未建立完整新增字段契约；冻结 Rust/Edge 文件下无法完成一致覆盖，因此没有上线仅 Go 支持的半套计费。现有行为保留，不能宣称新增图片输入漏计已修复。
- M14：OAuth web-search history 自动声明暂缓。Rust/Edge 直发不统一经过 Go OAuth 转换，冻结边界内无法覆盖实际路径，未做局部声明后宣称全链路支持。
- W01：后续 WS turn 独立价格快照为独立阶段；现有连接快照行为保留。不在每 turn 增加 DB/Redis 查询，不追溯重算历史账单。
- X01/X02/X03/X05/X06/X07：不引入 provider profile/新平台 Command Code/Cline、账号筛选折叠改版、OPS raw TPS 分位数查询、无业务收益样式/元数据、不存在的插件/合规页面、监控 V2 样本阈值。

## 二开保护复核

批准的 20 个冻结文件逐字节与 HEAD 比较，20/20 一致，包含缓存创建/展示、raw Chat、强隔离、Edge 入口/续传/结算、首 Token coordinator、HTTP upstream、Rust main/stream_session、账号 scheduler 和 scheduler snapshot。
整个 Rust 源码/依赖、数据库 migrations、本地 ent runtime 无 diff；账号创建/编辑/批量组件及 AccountsView 无 diff。

共享 gateway 和 apicompat 有局部改动，因此文件不变不是唯一保护证据：另执行缓存、strong isolation、scheduler snapshot、健康/成本、runtime-clear、settlement、首 Token 及本次协议定向 race 回归。
没有修改调度阈值、并发容量、价格表、倍率探测周期、缓存 key、24h retention、计费幂等键或新增 migration。

首次管理员仍保留随机邮箱。Nuro 部署脚本仍生成原有随机密码、写入 `.env`，部署完成显示实际邮箱/密码；已有 `.env` 和既有管理员不重置。
未恢复用户撤回的“仅到容器日志查看初始配置邮箱/密码”提示。

## 验证结果

| 检查 | 结果 |
| --- | --- |
| 后端全量 | 最终 `go test -tags=unit ./...` 通过；Go vet 通过。 |
| 真实数据库集成 | Docker PostgreSQL 18.1 / Redis 8.4，`CI=true go test -tags=integration ./... -count=1 -timeout=4m` 通过；包含 migration 幂等、管理员并发初始化、仓储/缓存/账单/调度契约，未因 Docker 不可用跳过。 |
| 二开/升级 race | 最后 setup/service/handler/repository 全包 race 通过；此前保护功能定向 race 也通过。 |
| 后端静态 | 最新 `golangci-lint --new-from-rev=HEAD` 0 项，Go vet 通过。全仓默认报告 169 项；最终关闭报告截断后，升级前 HEAD 与最终代码均 494 项，按文件/诊断逐条对照新增/消失均 0。历史静态债务未借升级大范围重构。 |
| 前端全量 | 最终 183 个测试文件、1125 项测试通过；lint、typecheck、production build 通过。新增延迟响应测试覆盖同组刷新、保存/清空中新编辑、邮箱取消/身份切换/卸载/写入失败、阈值草稿保留、邮箱开关失败回滚、计划和 OPS 新草稿、默认价格条目身份与手动价格保护。 |
| 浏览器 | Playwright 模拟 API，中文，375/390/768/1440，深浅色，usage/group/倍率/RPM/profile 共 40 组，加渠道 390/1440 深浅色 4 组，共 44 组，无 pageerror、无页面横向溢出；检查了清晰截图，弹窗动画完成后采集。profile 实测保存中新输入保留、邮箱开关 500 回滚；渠道实测删除原条目后旧价格不得回填下一行。 |
| Rust | debug 和 release 各 227 项 locked 测试通过；clippy all-targets `-D warnings` 通过。 |
| 嵌入前端/安装/路由 | `go test -tags='unit embed' ./internal/web ./internal/setup ./internal/server/routes` 通过。 |
| 双架构 | 最终版本与嵌入前端 Linux amd64/arm64 静态构建通过；完整 Docker 镜像构建与启动验收仅 linux/arm64，不宣称 amd64 完整镜像验收。 |
| 完整镜像 | 最终 linux/arm64 五阶段 Docker 构建通过；本地镜像 `nuro-sub2api:v0.2.15-nuro.1-review`，ID `sha256:62ceb3c003a3f81816c779981600d63447163e7ccf088a5a25cd682b7a94b0c2`，未推送。 |
| 启动/升级验收 | 空库、v0.2.14 本地测试库 pg_dump/restore 克隆两条路径，均使用双副本共享应用数据卷与 PostgreSQL/Redis。Go/Edge health/ready、嵌入首页/裸 API 边界、仅一个管理员、原密码/访问令牌保留、账号映射/优先级/倍率、每条路径 12 次跨副本调度切换、冷启动路径重启保留会话均通过。额外空库不设置 JWT_SECRET，自动生成密钥的跨副本令牌与重启会话验证通过。克隆 migration 记录校验值不变。这是测试数据备份，未读取生产备份。 |
| 生成工具 | ent 在临时目录生成成功，本地生成文件不覆盖。 |
| Go 漏洞 | govulncheck：0 可达漏洞、0 导入包漏洞；依赖图仍有 8 项不可达模块漏洞提示。 |
| 部署/审计 | Bash 语法、管理员 bootstrap/失败恢复/已有凭证保留、Compose 安全、11 项 audit checker 测试通过；既有前端 audit 报告例外校验通过，未重新联网生成该报告。 |
| 性能 | 512-item namespace 基准：无 namespace 仍 0 alloc；含 namespace 但无实际转换 523 → 1 alloc；实际转换 1551 → 1026 alloc。只是局部 benchmark，不代表整站压测。 |
| 差异 | `git diff --check` 通过，临时 ent/构建/截图产物不纳入版本管理。 |

日志、原始上游 diff、完整批准方案和截图：`/private/tmp/sub2api-v0215-review/`。
完整静态对照基线仅在 `/private/tmp/sub2api-v0215-baseline-lint/`。

### TPS 固定显示复核（2026-10-10）

- 管理员及用户使用记录共用三行延迟组件，固定显示首字、总耗时和输出 TPS；旧隐藏设置不再隐藏延迟，其他列的偏好保留。
- 复核补充 `video_count` 排除条件，防止视频记录缺少计费模式时误显示文本 TPS。缺失、非法或零耗时显示 `-`；有效零输出显示 `0.0 tok/s`。
- 覆盖记录更新、旧配置、损坏配置、其他列开关及持久化；39 项定向测试通过。前端全量 184 个测试文件、1150 项测试通过，typecheck、修改文件 lint、production build、diff 检查通过。
- 此前两个页面的 1440/390px、深浅色共 8 组模拟 API 浏览器样式检查通过。本轮额外 320px 英文窄屏及 500 条记录虚拟滚动检查因自动审批服务连接中断未执行，不计入通过项。
- 本次调整仅涉及前端展示、测试和本文档；后端、数据库、计费、缓存、账号调度、Edge/Rust、部署及凭据逻辑未修改，未发布或部署。此前升级验证记录是历史结果，不代表已为本次 TPS 调整重新构建发布镜像。

## 发布前剩余验证

真实第三方支付、实际 Codex 客户端、Go/Rust 故障注入组合、同负载高并发 TTFT/CPU/RSS/缓存命中对照和生产 canary 未执行。
生产备份克隆和完整 amd64 镜像启动未执行，不能用本地 fixture 克隆代替生产数据升级验证。
历史全仓静态债务、前端 XLSX 有期限的写路径例外及其他依赖提示仍适用，不承诺零风险。

隔离验收容器、网络、卷与测试凭据文件已清理，本地镜像和受限日志保留用于复核。
发布前应完成剩余外部门禁，备份数据库/镜像 digest/`.env`/Compose，再部署一致版本并观察错误率、429/5xx、TTFT、缓存/sticky、槽位/lease/WAL、账单遗漏与重复。
菜单 2 的升级逻辑和用户环境变量保留逻辑未修改；只有本次代码完成发布并有对应镜像后，菜单 2 才能取得这个版本。
原先“机器离线、调度开关切换失败”的生产事故没有诊断证据，本次升级不等于已经确认或修复该事故。
