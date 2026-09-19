# v0.2.7 手工适配交付清单

基线：`67ccc81d117ab53adc6b48bce18e05c7f1bd0eaf`。当前改动未提交、未部署。

应用版本已更新：`backend/cmd/server/VERSION` 为 `v0.2.7`，`frontend/package.json` 为 `0.2.7`。后端 `--version` 已实测输出 `v0.2.7`；Rust 包自身的 `0.1.0` 是独立组件版本，本次未调整。

此前核对的上游范围：v0.2.5 `86f93c28ee34cc74b629dafb748bd5ac5ca8c5ea` → v0.2.7 `aea725f2ea644d5592d0bbb1d63b607efa7e200a`，71 个提交（38 个非 merge）、131 个文件。采用手工适配，没有 merge、rebase、cherry-pick 或整文件覆盖。离线临时上游副本随后被环境清理；本地改动仍保留。

## 已实施范围

| 项目 | 当前实现 |
| --- | --- |
| Seedance | 独立原生 POST/GET/DELETE 与四组路径别名；显式账号能力、精简快照透传、普通/高级调度、HTTP 与异步并发限制；持久任务、幂等创建、原账号查询、重启恢复、冻结定价、真实 usage 结算、缓存更新 outbox；创建/编辑/批量开关和独立对接测试弹窗 |
| Seedance 计费安全 | 没有明确价格不提交；上游/请求/渠道映射计费来源均保留；终态缺 usage 不估算结案；账单与用量原子写入；未知提交不重发；凭证/地址变化暂停对账；重复 provider ID 要求本地恢复 ID；延迟对账避免 duration 字段溢出 |
| 国内平台 | 按选中账号/目标域名限制 developer role 兼容；DeepSeek 保留已有 reasoning，仅补缺失字段；工具图片提升为用户消息，保留连续工具结果和原始输入；Coding Plan 403 配额耗尽进入可恢复冷却 |
| Anthropic | 根 allOf/anyOf/oneOf 仅在语义可保持时转换，不能安全转换的 schema 明确拒绝；普通 schema no-op |
| Antigravity/Gemini | thinking variant 尊重显式映射；特定 Go/Python SDK 原生 Gemini 流禁用 SSE 注释；只移除前导 billing attribution；动态模型发现与单模型查询遵循分组配置，保留原生模型元数据 |
| OpenAI | HTTP response 账号绑定脱离下游取消但保留 750ms 上限；manifest 删除重复解析，原验证保留 |
| 兑换历史 | 新增可选分页；不传分页参数仍返回旧的 25 条数组；稳定排序和前端迟到响应隔离 |
| 前端 | 分页输入、模型 Tab、退款金额、限额非法输入、弹窗 ID、代理批测、金额回退、剪贴板、TOTP、注册优惠码初始状态、订单过滤与竞态、公告部分成功/退出隔离、支付配置合并请求、移动端模型广场入口 |
| 依赖注入 | 内容审核可变参数改用明确 provider；渠道调度事件总线放进 provider，避免 Wire 再生成丢绑定；生成代码与 Wire diff 一致 |

Seedance 配置、协议、收费、恢复和部署边界见 [seedance-api.md](seedance-api.md)。新增迁移 `234_seedance_tasks.sql` 只创建独立表及索引，不改旧表。前端延续现有卡片、暗色和窄屏布局；尚未做真实浏览器视觉验收。

## 保留本地方案／不引入

- Edge、Cell/lease/WAL、同连接恢复、execution-unknown 不重放、24h 缓存、sticky/接力、ABCD、缓存利润、强隔离、真实计费与展示 usage 隔离、OAuth/API Key 首包与首 Token 阶段保持本地实现。
- 健康调度、严格优先级和 RCU/COW 保留；精简快照仅增加两个 Seedance 字段，不修改选号算法。
- 暂停账号的 OAuth 刷新过滤本地已通过 `ListActive` 实现，不重复移植。
- 不引入插件系统、上游 rollup/group cache 替换或非必要重构；Rust 源码及依赖、前端依赖、部署配置未改。
- 用户列出的 20 个禁止修改文件已逐一核对，全部空 diff。

## 已执行验证

- 前端全量：170 个文件、1,046 项测试通过。
- 前端生产构建通过（含 vue-tsc）；仍有现有 Browserslist 过期、动态/静态导入、Node DEP0190 提示。
- Go 全包编译（含 `-tags unit`）通过；这不等于全量测试运行通过。
- 新增 `TestV027` 的协议、取消边界、403 配额、任务归属/幂等/用量、事务回滚、路由及 CORS 测试通过；主要升级包的定向 race 通过。
- apicompat 全包及迁移包测试通过；迁移包测试不代替真实 PostgreSQL 执行。
- 原有缓存创建/ABCD 展示、账号隔离、调度快照、选号、WS 状态、首 Token 占位及 Edge continuation 的选定回归通过。
- 此任务此前执行的 Rust 全量离线测试 198 项通过；其后 Rust 无改动。
- Wire diff 通过；`git diff --check` 通过。

## 本轮扩大审查补修

1. **Seedance 终态证据先落盘。** 修复结算暂时失败后仍依赖再次查询上游、可能因任务删除／过期丢失已知真实 usage 的问题。首次终态不可覆盖，后续结算从持久记录恢复；取消已完成任务前必须保存证据，缺用量或保存失败时拒绝删除。对应变更在新表迁移 `234` 内；该迁移尚未部署。
2. **已有任务的查询／取消不被新消费门槛阻断。** 仅四组 Seedance GET/DELETE 精确路径绕过余额、Key 配额／到期检查，继续执行身份、禁用状态、IP、分组及任务归属校验；POST 不豁免。
3. **缺失输出价不能变成免费任务。** 不再仅凭 LiteLLM 来源就接受零输出价；免费任务仍可通过分组／渠道明确配置零价格。
4. **Gemini 混合发现与单模型查询一致。** 已发现的 Antigravity 模型在原生 Gemini 返回 404 时提供模型元数据；非混合账号、未知模型和 503 不被误放行。忽略空目标及无法通过单模型路径访问的映射，成功的原生模型元数据仍原样保留。

本轮补修后重新通过：Go 全树编译（含 unit 标签）、六个升级包的 `TestV027`、认证中间件 unit 回归、四个主要包的定向 race，以及缓存创建／展示、调度快照、选号、首 Token 占位和 Edge continuation 定向回归。新增回归验证结算失败后的持久恢复、删除前证据保存、保存失败不扣费／不删除、鉴权豁免边界和混合模型的真实 handler 调用。

20 个保护文件再次确认全部空 diff。前端本轮未追加修改，沿用此前 1,046 项测试及生产构建结果。带 unit 标签的升级测试中，Redis outbox 用例仍因 `listen tcp 127.0.0.1:0: bind: operation not permitted` 无法运行；不能将它计为通过。

## 再次扩大未提交范围审查

- **Seedance 空错误误报。** 真实 handler 回归复现上游 `error: null` 被重写为 `generation_failed`；现在仅脱敏非空错误，成功内容与 null 原样保留。
- **Gemini 原生流 SDK 兼容遗漏。** 原生 Gemini 转发也过滤不兼容 SDK 的 SSE 注释；同时识别分散在 User-Agent 与 X-Goog-Api-Client 的 SDK／语言标识。覆盖 API Key 与 OAuth 的真实流处理，断言正文和输入／输出用量不变，Node SDK 保留原行为。
- **Thinking 变体选号与转发不一致。** 仅在原生 Gemini 入口设置请求级思考档位，Antigravity 变体的模型支持、真实主调度入口、指定账号重试、模型冷却、渠道上游模型限制和转发使用同一个变体。支持变体的裸模型别名也纳入发现；显式映射／拒绝、混合调度 opt-in、严格优先级保持。未设置原生请求标记的 Messages、原生 Gemini 账号及其他平台保持原模型解析。
- **版本遗漏。** 修正后端和前端仍显示 0.2.4 的问题，统一为 0.2.7；Docker／Makefile 原有版本注入优先级不改。

本轮新增验证覆盖实际 `GatewayService.SelectAccountWithLoadAwareness`（负载批处理开／关）、`SelectRequiredAccountWithLoadAwareness`、变体冷却拦截和渠道模型限制。扩展的 Gemini／Antigravity／通用网关调度回归通过；apicompat、antigravity、迁移及路由包测试通过。前端 0.2.7 的生产构建（含 vue-tsc）重新通过，原有构建提示仍存在。

最后补修后，Go 全树 unit 编译、服务／handler／认证中间件的升级 race、缓存／选号／首 Token 占位／Edge continuation 定向回归均重新通过。20 个保护文件仍全部空 diff；未进行提交或部署。

本轮再次尝试隔离下载安全依赖，仍然无法解析 `proxy.golang.org`；正式提权下载被自动审批服务以 `502 Bad Gateway` 拒绝，没有绕过审批或改写未核验的依赖版本。下面的未完成发布门禁仍然有效，版本号更新不代表这些门禁已通过。

## 提交前最终复核

- 补修一个前后端配置兼容遗漏：通过 `openai_capabilities` 数组／对象启用 Seedance 的账号，测试弹窗原先不展示入口；对象形式及 null 独立开关的配置还可能在编辑保存时被意外关闭。编辑页与测试弹窗现在共用同一读取规则，匹配后端的大小写／空白规范化和显式 boolean 优先级，显式关闭仍保持关闭。
- 新增回归先复现 4 个失败场景，修复后账号创建／编辑／批量编辑／测试弹窗及 Seedance 组件的 6 个测试文件、123 项测试全部通过。包含上述修改的前端生产构建（含 vue-tsc）通过；既有构建提示未新增。
- 对服务、handler、repository、认证中间件、路由、apicompat、antigravity 和迁移包执行 `go vet -tags unit`，全部通过。使用当前前端产物的 `CGO_ENABLED=0 go build -tags embed ./cmd/server` 通过，生成程序 `--version` 为 `v0.2.7`。
- 核对了迁移的嵌入／Docker 复制路径、Seedance worker 启停挂接、终态保存和事务／outbox 的幂等边界。20 个保护文件仍全部空 diff，旧迁移、Go/Rust/前端依赖与部署配置未追加变更，`git diff --check` 通过。
- 未发现其他已确认需要补修的问题。以下未完成发布门禁仍然有效；构建和代码审查不能代替真实环境验收。本轮未提交或部署。

## 尚未完成的发布门禁

1. **安全依赖尚未升级。** 当前仍为 `x/net v0.56.0`、`x/crypto v0.53.0`。计划核验并适配 `v0.58.0` / `v0.55.0`，但下载被沙箱 DNS 和自动审批服务 502 阻断；没有伪造 go.sum 或提交未验证的版本号。`grpc v1.75.1` 仍在间接依赖图；此前 `go list -deps ./cmd/server` 没有 grpc 包，`go mod why` 不需要它，本次不引入插件链路。仍需在可联网环境完成依赖安全复核。
2. **Go 全量运行及 Redis Lua 回归未完成。** httptest/miniredis 需要本机监听端口；提权测试被自动审批拒绝，实际原因是审批服务 `502 Bad Gateway`。新 quota outbox 的 miniredis 测试已编译，不能据此声称执行通过。
3. **真实环境验证未执行。** PostgreSQL 迁移、Redis flusher、多副本 lease、节点重启和真实 Seedance 供应商端到端仍待 staging。没有调用真实收费上游，也没有使用对话历史中的凭证。

允许执行后应完成：安全依赖下载/校验和最小升级 → `go test -tags unit ./...` → 相关 race → staging 迁移/并发/重启/收费任务与取消对账 → 浏览器桌面/移动端验收。不要把本清单当作“已完成所有发布门禁”或“零风险”承诺。

仍有 Seedance 任务未结算时，不应切换其原账号凭证/上游地址或平台配额 flusher 模式；确需变更时先完成任务对账。回退应用版本也应先排空在途任务，保留新任务表和已产生的账务记录。
