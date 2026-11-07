# v0.2.20 手工适配实施与验收状态

基线：`f78ae396d799f06aea66bb02dcf383858ea2c1db`；实施分支：`codex/upgrade-v0220`。上游对照为 `v0.2.19@dece26153621959ccd7635814f4b2d08304e681a` → `v0.2.20@dab829a4e9b7d1cc307189aea8b0319eb29e29d0`，共 112 个 commit（40 个 merge）、459 个文件。原始分类和已确认的完整 A–R 方案存档在 [v0220-plan](v0220-plan/手工升级方案.md)，它们描述的是实施前决策，不能当成当前验收结果。

本次采用局部手工适配，没有 merge/rebase/cherry-pick，没有修改保护 manifest。代码实现和发布验收分开记录：**当前不是已批准上线版本，`release_ready=false`**。依照方案 Q 的收口条件，完整门禁通过后才统一版本为 `0.2.20-nuro.1`；目前前后端仍为 `0.2.19-nuro.1`，未创建版本 tag、提交、推送或部署。

**2026-11-08 用户范围调整：本地部署不采用本轮新增的恢复包、源码/配置自动恢复和显式灰度 finalize/recover 流程，现已撤回。** `upgrade_service` 已恢复至本轮修改前的实现（含原有启动失败镜像回退），仅保留强制 Git 覆盖前的源码脏检查和默认关闭的新配置接入。此决定覆盖归档方案 O07，不作为遗漏待办；下文历史恢复测试记录不代表当前脚本。

## 1. 范围逐项落实

|编号|当前实现与证据入口|
|---|---|
|D01 安全依赖|更新 brotli/compress、vue-i18n 及必要前端覆盖依赖；保留 CSP runtime alias，未引入 MCP/alpha JSON 依赖。govulncheck 与既有 audit policy 检查通过，没有扩大安全例外。|
|D02 Bedrock|`bedrock_stream.go` 在分配前验证 CRC、最小长度、headers 128KiB、payload 16MiB；新增畸形长度测试。|
|D03 OAuth/回显|保留客户端 stream 意图；缺 Content-Type 的 OAuth SSE 可聚合为 JSON，接受 incomplete/cancelled/canceled 终态及真实 usage；请求局部记录 instructions 缺失/null/空/原值，下游还原，原上游前缀不改。见 `openai_v0220_protocol_test.go`。|
|D04 Terra|补 catalog 缺失能力并保留原字段/顺序；APIKey 禁报 Responses Lite；拒绝 API minimal/ultra，Codex catalog 保留客户端 ultra；OAuth 长上下文豁免与 APIKey 原费率分开。Sol 第一、Terra 第二的本地排序不改。新增真实 RecordUsage 对照。|
|D05 GPT Image 2.6|flare/sunburst 使用原生 generations/edits；APIKey/OAuth/Agent Identity 复用既有凭据、隔离、媒体 profile 与 attempt 跟踪。支持复杂度、SVG、GIF、mask/markup、帧控制、JSON→completed SSE；只最后一帧附 usage。验证上传与别名、禁止 forced Responses 静默降级、未知执行禁止重放；按实际图片和 provider usage 计费。详见 `openai_images_native_test.go`。|
|D06 协议|空 message content、reasoning 回退、AG 直接/wrapper、Gemini 函数媒体、AG responseSchema、Grok agent_message 局部兼容；Cloudflare REST 仅精确 HTTPS 主机/路径例外去掉 beta query。原 namespace/additional_tools/工具续链不重构。|
|D07 鉴权/错误|鉴权取消归类 499；上传超时优先 408、超限 413、本地预算耗尽 503；Google 408 状态对应 DEADLINE_EXCEEDED。forced routing 跳过无用后台探测；413 自定义规则优先，仍统一脱敏且不扩大重放权限。|
|D08 WS TTFT|将每轮 token 观察移出连接 firstToken 的一次性判断；保留原 token 分类、responseID 归属及逐轮计费。见 `openai_ws_v2/passthrough_relay_internal_test.go`。|
|D09 被动额度/限流|weekly-only 头与 429 采样保留；精确 Kimi 并发 403 走短暂限流，其他 quota403 保留原规则；AG 图片 INTERNAL500 仅模型冷却，文本沿原处罚。|
|D10 RPM|auth snapshot 版本升至 25，成功 nil 按 groupID 缓存，查询错误不作负缓存；旧快照/跨组仍回原查询，全局 RPM 仍独立。|
|D11 入口资源|可选请求缓冲预算、H1/H2 逐次读写空闲 deadline、异步图片显式持有 reservation；解压 limit+1 拒绝超限，zstd window 有界。新预算与 deadline 默认均 0。|
|D12 导出|Codex/CC Switch 默认模型按权限选择；TOML/URL/特殊字符编码与显式图片配置保留。CSV 专用 keyset cursor，1000/页、有界 429 重试/取消/进度，失败不下载部分文件。|
|E01/O02 通知|4 worker/128 queue/30s，任务内检查至 SMTP 发送不另起游离 goroutine；最小 DTO；取消关闭 socket；通知先于邮件资源退出，真实计费/quota/WAL 不归此队列。|
|E02/O04 AG 展示|basic 成功后 tier/summary 在原 30s 总预算内最多两项并发；分来源成功时间/错误/last-good，当前认证失败优先；不在网关热路径拉额度。|
|E03 管理测试|AG Claude/Gemini 文本测试传入管理员 prompt，允许足够输出以显示真实回复；空输入保持最小探测。沿用原取消/超时 UI、模型映射与协议转换；Claude 首尾空白仍按既有转换归一化。新增 fake 上游整链测试。|
|E04/O03 CSV|HMAC cursor 绑定 owner、筛选、排序、时区、高水位和有效期；每页重新鉴权，后续页无 COUNT/OFFSET，保留 BOM/列/精度/公式防护。真实 PG 边界/插删/排序与取消测试。|
|E05 黑名单|默认空、最多 20000、独立 canonical key；不改登录邮箱。保存规则和存量禁用分开，JWT 管理员预览后明确确认；事务重验规则/用户版本、审计与批次幂等，机器 key 不获得该权限。真实 PG 测试含并发、审计失败回滚和重放。|
|E06/O05 Gemini 累积|仅流内 text/thought/tool 使用独立 append accumulator；兼容 trace、fuzz 与 delta/cumulative/mixed 基准，保留纯函数兼容。没有全局大缓冲池或请求体重写。|
|E07 已有快速路径|沿用原 thinking bytes 预检与顶层过滤；原单元/回归测试保留。不新增孤儿工具扫描框架或 alpha JSON 流水线。|
|E08 流预算|仅可分离 Go 转换路径加 reader/字节队列/单一错误终态；默认关闭，丢弃超大展示事件后继续原 usage 收集。覆盖表见下。|
|O01 AG 缓存|发布缓存快照只读，命中/flight 调用者各得可变层副本；倒计时在展示副本计算，原 key/TTL/flight 不变。并发与修改隔离 race 测试。|
|O06 验收入口|`make check-upgrade` 复用原 targets，匹配声明工具链，逐阶段日志/测试数量与关键集成测试必跑检查；缺工具/隔离 PG 明确失败；`--only` 报告不冒称完整验收。|
|O07 部署范围调整|按用户决定撤回新增恢复包、源码/配置自动恢复、恢复状态和 finalize/recover 命令及专用测试；恢复原有部署流程，仅保留强制 Git 覆盖前的脏源码检查和新配置接入。|

排除项保持原决定：不引入图片尺寸分流、管理 MCP/全权密钥、会话 LRU/bounded drain；不改 scheduler/sticky/cache key/24h、ABCD 和缓存利润、隔离、Cell/escrow/lease/WAL、Edge 数据面与学习/重放边界，不用上游模板覆盖本地部署容量和显式配置。

## 2. 新配置与流预算覆盖

完整字段见 [环境示例](../../deploy/.env.example)、[YAML 示例](../../deploy/config.example.yaml) 及四份 Compose；部署菜单生成值同为 0，旧部署未配置时不自动启用。除字节预算外，idle timeout 单位是秒，范围 0–86400；不是流总时限。环境变量改动需要重启对应 Go 实例。

|配置|缺省|精确含义|
|---|---|---|
|SERVER_REQUEST_BODY_MEMORY_BUDGET|0|实例内已登记的请求体/图片解析转换缓冲字节；不是进程 heap/RSS 总上限。异步任务持有到完成，预算在申请前登记，拒绝不污染账号健康。|
|SERVER_REQUEST_READ_IDLE_TIMEOUT|0|单次 body 读取空闲期限；H1 隐式 drain 也有界；hijack 前清理 deadline。|
|SERVER_RESPONSE_WRITE_IDLE_TIMEOUT|0|逐次 Write/Flush 空闲期限，不取消原上游 usage/结算生命周期。|
|GATEWAY_SSE_MAX_LINE_BYTES|0|SSE 行字节上限，包含行尾。|
|GATEWAY_SSE_MAX_EVENT_BYTES|0|SSE 事件字节上限；建议与行数/队列限制配套启用。Bedrock 对应单帧 payload，仍有独立硬上限。|
|GATEWAY_SSE_MAX_EVENT_LINES|0|事件行数，含分隔空行。只设置行数不能证明每行或事件字节有界。|
|GATEWAY_SSE_QUEUE_MAX_BYTES|0|已排队展示数据的字节额度；一个大事件可独占空队列，因此需配合 event bytes。|

|路径|新增 reader/帧限制|新增队列|usage/drain|
|---|---|---|---|
|Gemini Messages/Chat、非流 SSE 聚合|有|原本无异步队列的路径不新增队列|原解析继续收集后续合法 usage，超限单一脱敏终态|
|AG Gemini/Claude 转换与聚合|有|既有异步通道接字节额度|队列取消只停止展示等待，不作为取消账务许可|
|Images 原生流/Responses image tool|有；JSON fallback 保留原 body 上限|既有异步通道接字节额度|实际 usage/已生成图在错误返回时保留|
|Bedrock 二进制帧|分配前硬校验与可选 payload 限制|有|超大 payload 有界丢弃，后续合法 usage 保留|
|冻结 raw passthrough、Rust Edge、WS|不装配新 SSE reader|不装配|原有上限、首包、drain 与结算语义不变|

CSV cursor 依赖现有 JWT secret 的用途隔离派生密钥；所有副本须使用同一稳定 secret，轮换会使未完成导出失效并要求重启导出。跨 HTTP keyset 不承诺 MVCC 快照：上界防止通常的新记录混入，晚提交低 ID 与清理删除仍有边界。最终 Blob 仍占 O(文件大小) 内存，不宣称无限规模导出。

## 3. 验证与审查证据

原始日志保留在本机 `/private/tmp/nuro-v0220-*`，不用巨大日志或凭据污染仓库。最终验证索引与源码哈希见 [v0220-validation.json](v0220-validation.json)。测试数量为 pass 事件（包括 subtest），不是独立业务场景数。

- 冻结契约：20 文件/283 历史迁移不变；无新迁移，无编号冲突；Go/Docker 声明对齐。没有改保护基线来放行本次差异。
- 完整 Go default/unit/build/vet：收口轮 `closeout-go/report.json` 通过，default 8452 / unit 13404 个 pass，0 fail；后加测试和 streaming 回显去重由定向 race、最终完整 service unit 与最终集成补验；完整 service unit 使用 `-count=1`，9122 pass、0 fail、1 skip（旧数据库 NULL session reference 场景仅集成 fixture 适用）。
- 全范围 race：此前 `final-race/report.json` 10893 pass、0 fail；最后计费、原生图片/Agent Identity/unknown execution、Terra、AG prompt 另跑定向 race，不冒称此前全范围结果包含后来新增测试。
- PostgreSQL/Redis/真实 Rust：`final-integration2` 完整通过（9013 pass、0 fail），关键测试 checker 确认实际执行；最终 `closeout-integration2` 9039 pass、0 fail、2 skip，12 个显式 required 测试均实际执行并通过；两个 skip 是 DingTalk disabled sentinel 与未配置外部 TLS capture。前一轮因隔离 DSN 的 URI 格式未满足旧测试的安全断言而失败，改用该测试要求的 keyword DSN 后重跑通过，未放宽断言。测试仅用隔离 fixture，不访问生产或真实供应商。
- 前端完整：`final-frontend` lint/typecheck/196 文件 1224 测试/build 通过；embed 由该次构建生成。新增组件以本地 mock fixture 在 zh/en × light/dark × 375/768/1440 共 12 种组合检查预览、确认禁用状态、翻译和溢出，截图人工复看。临时 fixture/服务已移除，未操作真实用户。
- Rust：`nuro-v0220-rust-final1.log` fmt/check/debug+release tests/clippy 通过；Rust 源码零改动。局部 Go 修改没有改变冻结 Edge 文件。
- 工具/部署历史验收：35 项 Python 工具测试；脚本语法、引导凭据、当时的升级恢复故障矩阵通过。恢复流程现已按用户要求撤回，当前部署验证见下方范围调整记录。可选 datamanagement 源码在本仓库缺失，明确 NOT_APPLICABLE，未虚报测试通过。
- 安全：secret scan、govulncheck、既有 pnpm audit policy 通过；audit stdout JSON 与 stderr 分存，异常报告拒绝通过。govulncheck 报告调用路径受影响漏洞 0，不能据此宣称所有间接模块无公告。
- 完整 golangci-lint **仍失败**：同配置、取消数量截断，对原 HEAD archive 与本次代码比较均 563 项，新增 0、减少 0。未放宽规则或增加例外。既有问题包含冻结区，本轮未借此擅改保护实现。

复审修复了图片缓存读取的长上下文费率漏乘，保留基础费用分项/ActualCost 的既有区别；自定义渠道/价格区间显式 0 费率、priority/flex、区间拆分 token 守恒、内部原 usage 不变均有回归。流式 instructions 还原只在下游展示阶段执行一次，避免重复转换，也让内部观察保留上游数据。

历史扩大复审记录（以下恢复流程相关三项现已随 O07 撤回，SVG 修复保留），未修改冻结实现：

- 恢复源码时，原逻辑只识别 `.pnpm-store` 根链接，遗漏项目实际跟踪的子目录链接。现在恢复全部 Git 跟踪的 store 条目，并在替换源码前检查还原树干净；真实浅克隆测试覆盖两种链接布局。
- 回滚及 finalize 先核对恢复标签仍指向记录的旧完整镜像 ID；标签被重新指向时，在恢复源码/配置或删除标签前拒绝继续。
- 源码/配置恢复后，旧容器启动暂时失败时，同一恢复包现在可继续重试；只有恢复后源码仍与本包记录一致时才复用，不覆盖后来修改，也不覆盖首次失败源码备份。覆盖失败后重试、另行编辑拒绝和浅克隆重复恢复。
- 图片 multipart 的 SVG 作者选项必须是单个完整 JSON 对象，拒绝第二个值及尾随垃圾，保留合法空白及未知字段拒绝。新增反例先验证修复前失败，修复后图片定向 race 28 个 pass、0 fail，service vet 通过。

同时修正部署测试在 macOS Bash 3.2 下对 `set -e` 的依赖：所有独立断言显式退出，避免失败断言仍输出 PASS。旧 `final-deploy` 的恢复矩阵结果不再单独作为通过依据，以强化断言后的 `review-deploy/report.json` 为当时依据；脚本语法、引导凭据、完整恢复故障矩阵均通过。该测试只使用本地 fake Docker 与临时 Git 仓库，不执行实际部署。

2026-11-08 再次扩大检查：重新追踪协议序列化、计费/展示隔离、鉴权与 RPM、黑名单事务、导出筛选/游标、通知取消、AG 共享快照、请求体/异步图片预算及部署恢复调用链。新增修复两项：

- `ResponsesOutput.MarshalJSON` 先前为保留空 `content` 复用了文本投影 helper，会丢失非空消息的 `refusal`、`image_url` 和文件字段。现在只覆盖 `content` 的省略规则，保留原有结构体字段；同步响应与 `response.completed` 终态均验证空数组、拒绝、文本、图片/文件及真实 usage 往返不变。新反例在修复前实际失败。
- CSV 单元格原防公式规则遗漏换行和前导空白之后的公式。现在将这些单元格明确转为文本，保留原始字符；补充换行、空格/制表符、BOM、正常文本及引号回归。未改账务数据、费用精度或 CSV 列结构。

本轮 `review2-go` 完整 default/unit/build/vet、`review2-integration-fixed` 隔离 PostgreSQL/Redis/真实 Rust 与全部 12 项 required 测试、`review2-deploy`、35 项工具测试通过。全范围 race 为 10921 pass、0 fail、7 skip，启动于本轮序列化修复之前，修复后另补完整 apicompat 包 race（321 pass、0 fail）；default/unit/最终集成均在修复后运行。前端完整 lint/typecheck、196 文件 1225 测试及 production embed build 通过，最后两份前端文件再次 ESLint 通过；本轮无样式变更，沿用原视觉检查的明确范围。完整 lint 再次为基线 563 / 当前 563 / 新增 0，仍未通过，不放宽规则。首轮集成因传入带引号的数据库名未满足旧测试的 DSN 安全断言而失败，修正命令参数后完整重跑通过；未修改断言，失败日志的 fixture 密码已脱敏。原始日志及源码、embed 哈希已更新到验证索引，旧证据保留其原始范围。

Gemini 微基准（Apple M4，三次采样中位数，非隔离整机）：delta 约 6.25ms/36.57MB/999 alloc → 81.8µs/286KB/19 alloc；mixed 约 4.33ms/34.29MB/937 alloc → 330µs/2.98MB/95 alloc；cumulative 两者 0 B/0 alloc。这证明局部累积收益，不替代真实 TTFT、GC 峰值或整站 p99 门禁。

2026-11-08 本地部署范围调整后验证：恢复后的 `upgrade_service` 与原 HEAD 逐字一致；脚本语法、引导凭据测试、35 项工具测试、20 个保护文件/283 个历史迁移检查及 secret scan 均通过。临时 Git fixture 验证干净目录可同步，已修改/已暂存/未跟踪文件均阻止强制覆盖。与上次验证源码哈希相比，仅部署脚本、检查入口及被移除的恢复测试发生变化；其余升级源码保持原值。验证只操作临时目录，未执行部署。日志为 `nuro-v0220-local-deploy-revert` 与 `nuro-v0220-local-deploy-revert-contracts`。

2026-11-08 最终复审覆盖全部未提交升级改动，并沿调用链复核缓存策略/展示 usage 与真实计费、调度/隔离、异步图片、流预算、黑名单事务、导出、通知和本地部署。新增修复两项，均先增加回归并确认修复前失败：

- 黑名单预览在局域网 HTTP 环境没有 `crypto.randomUUID` 时失败；现用浏览器 `getRandomValues` 生成符合 UUID v4 的批次 ID，并在重试时保留同一 ID。生成成功后才发布预览，避免显示可操作却缺少批次标识的数据。没有改变管理员确认、规则版本或后端幂等校验。
- 图片上游返回 usage 但没有图片时，原先保留真实 usage 后会漏发错误，客户端可能得到空 HTTP 200；现复用既有错误回包判断补发脱敏 502，已回包和断连场景沿用原判断。完整 handler 回归验证上游只调用一次、输入 37 token 只记录一次，不把失败当成功或重放请求。

修复后本轮完整 Go default 8462 / unit 13421 个 pass、build/vet 通过；handler/admin、httputil 和 ssebudget 的 race 共 1547 pass、0 fail、4 skip。前端 lint/typecheck、196 文件 1226 测试和 production embed build 全通过；脚本语法/引导凭据、35 项工具测试、20 文件/283 迁移冻结契约与 secret scan 通过。完整 Go lint 仍为原基线 563 / 当前 563 / 新增 0 / 减少 0，没有放宽规则。本轮未改数据库、Rust 或部署脚本，沿用之前集成/Rust 证据的原始范围，没有声称本轮重跑；未操作生产、真实供应商或实际部署。当前源码和 embed 指纹、失败反例及后验日志记入验证索引 `final_review`。

2026-11-08 独立 lint 治理：在用户确认后处理实际缺陷和低风险清理，完整 lint 从 563 降至 553 项，减少 10 项、新增 0 项，规则不变，仍未通过。修复备份流忽略 `Close` 返回的 `pg_dump` 退出错误，防止部分导出被标为成功并上传；新增反例先在原实现失败。完整 Go default 8467 / unit 13429 个 pass、0 fail，build/vet 通过；备份相关 race 38 pass、0 fail，20 文件/283 历史迁移保护契约通过。未修改部署、前端、Rust 或保护功能。此次独立证据及保留项见 [lint 治理记录](v0220-lint-cleanup.md)，历史升级验证索引不冒充此次源码的结果。

独立 lint 治理随后再次代码审查：未发现新增业务缺陷，生产逻辑未再修改；补充真实子进程的成功/失败退出与取消回收测试，相关定向 race 104 pass、0 fail。完整 lint 仍为 553 项、新增 0，保护契约再次通过。既有全量 default/unit/build/vet 的 11 份后端源码/测试指纹未变，新增测试另有本轮证据，详见上述 lint 治理记录。

## 4. 未通过的发布门禁及回滚边界

下列条件不能以单元测试或组件截图替代，尚未验收，因此没有 bump 版本或标记可发布：

1. 完整 Go lint 仍有 553 项告警，lint CI 检查尚未通过。修改冻结区或放宽规则不属于本轮自动解决方式。
2. 同源码完整 128/256/512 及更高并发阶梯、三轮 TTFT p50/p95/p99/错误率/FD/RSS/GC/队列/WAL/取消后恢复与基线对照未完成；微基准不能外推。
3. 真实新旧副本组合、generation fence/故障恢复与全链路账务对账、真实供应商新协议、完整 staging/canary/drain/回滚未验收。
4. Linux 多架构完整镜像、digest/provenance 与 Go/Edge/前端同版产物的真实发布演练未完成；当前本机 build/真实 Rust fixture 不能替代。
5. 组件 fixture 视觉通过不代表整站及真实设备、完整设置页/导出页与后端联调全部验收。

后续发布先处理红门禁，保留新可选资源配置为 0、黑名单为空、图片按张原 opt-in。本地部署恢复原有升级流程，不增加恢复包或 finalize/recover 操作。沿用 Edge 先 drain、Go 后退出；不删除 WAL/账本/usage、不全清 Redis、不回拨 generation、不重放 unknown attempt。存量禁用不在本次执行，恢复使用原启用用户流程并保留审计。
