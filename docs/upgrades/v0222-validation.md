# v0.2.22-nuro.1 手工适配与验证记录

日期：2026-11-17。起始分支 `main`，起始提交 `c969261a30bdcb6f30c74e0922b77e00141c2a41`。上游对照 `v0.2.21@10ba8f9b4dc2e6023d4a9879ab77b21841babd10` → `v0.2.22@b0fa9f3e6eeab1968a53e1289b69daa4bb40ca2b`，在临时目录对照后手工适配，没有 merge、rebase、cherry-pick 或整文件覆盖。

允许范围实施完成：40 组手工适配、7 组保留本地实现并验证，O01–O08 均已交付。另有 7 组保护阻断、1 组部分阻断、6 组按方案排除。后两类不等于已同步上游。前后端版本统一为 `0.2.22-nuro.1`。全部修改保留在工作区，未提交、push、部署或访问真实付费上游。

**完整门禁不能标记全绿**：golangci-lint 默认输出为 172 项，但该数量受每个 linter 及同文案的默认输出上限限制。最后复核使用同一工具、同一配置并关闭输出截断，起始 HEAD 和当前代码均为 551 项；按文件及诊断内容多重集逐条对比，新增/删除均为 0。没有修改规则或基线来隐藏失败。`v0222-status.json` 的 `release_ready` 保持 `false`。这表示尚未获得全部发布门禁通过，不表示下面已通过的测试失败。

## 主要实现与复核

- 支付宝退款：稳定请求号、事务内认领与身份审计、重试先查询。查询失败或未知保持 pending；只有明确失败才允许使用原号重试。金额/币种不能在重试中变更；历史未知退款需对账。并发退款测试验证提供方最多被调用一次。
- 配额：Fable 仅恢复有 owner token 且仍匹配的模型限制；5h 窗口变动不清 weekly。Kimi 支持月窗，零值与未知分开，畸形/截断或已声明但无效的窗口不会清旧快照。OpenAI 使用真实观测时间，刷新失败不伪装新鲜或无限。
- 流式错误：队列 ping 后保持对应 SSE 协议；兼容流使用既有 response ID/递增序号；取消不补终态，重复错误只发送一次。未更改冻结 coordinator、重放权限或 unknown 执行约束。
- Sticky：复用选号原 GET 的观测，无新增选号 GET；Redis miss 与传输错误分开。成功使用有界 CAS 写，AG 已提交空流仅 compare-delete 当前会话指向的失败账号；不改 key/hash/TTL 格式。
- 协议：per-turn beta、count_tokens 多值 beta 去重、声明 ID 与 opaque call ID 区分、加密工具内容保留、schema array items、结构化输出 strict 默认 false、中途 system 顺序保留为 developer、metadata.user_id 白名单与最终强隔离、Grok 服务端 search 分类及去重。
- 计价：Carbon/Hydrogen、Qwen3 精确回退与 Vertex 实际区域。默认字段分别乘 1.1，显式覆盖（含零值）按组件保留；标准与 priority 覆盖独立，组/渠道/自定义时间价不再叠加。批任务保存新任务的价格快照，不重算旧任务。
- 统计：`actual_cost` 始终为实际用户扣费，账号成本独立为 `account_cost`；隔离 PostgreSQL 真实 SQL 验证模型、入站/上游端点与路径维度、账号筛选和用户+账号组合。
- HTML：注入设置的页面 no-store，nonce 每响应替换；有 nonce 时不使用不含新正文的 304，普通无 nonce ETag 行为有独立回归。
- 定价后台：可用本地价卡先启动，远程检查沿用一个 worker；服务拥有可取消 context，Stop 幂等，取消后不发布候选，已停止服务不重新复制 fallback。未改网关 drain 或请求计价来源。

## 保护边界

`python3 tools/check_upgrade_contracts.py` 最后复验通过：20 个冻结文件及 283 个历史迁移与批准基线一致，无新增迁移冲突。旧 manifest/baseline 没改。本轮无新数据库迁移、Go/Rust/前端依赖升级或锁文件改动；未恢复撤回的部署脏目录检查/私有恢复包。

缓存 key、session hash、24h retention、ABCD/markup 与真实 usage 隔离、OpenAI sent/unknown 禁止重放、≤3 次账务事务重试、strict/adaptive、guard、RCU/COW、Cell、escrow、lease、WAL 与 drain 保留。通过既有相关 Go、Rust、真实 Rust HTTP/WS 与隔离数据库回归验证，不能把测试结果解释为生产环境绝对零风险。

模型排序仍为 Sol 第一、Terra 第二；使用记录上游模型审计默认隐藏、已有用户偏好及管理员 JSON 导入权限未改。精确白名单拒绝新增 `*`，映射通配功能保留。

明确阻断 11/22/31/44/53/58。22 的冷却期间重建快照后、账号到期仍可能缺席的上游差异仍在，不能声称已修复。49 仅独立错误出口可适配，relay/coordinator/Rust 部分保持原实现。50 因现有 Go ultrafast=2x 与冻结 Rust 缺 ultrafast6x，无法证明 Go/Rust、D、markup 与 WS 逐轮一致，整项不新增 UI、迁移或开关。06/10/18/48/51/52 不引入。

## 本地优化 O01–O08

|项目|交付与验证|
|---|---|
|O01/O02|本地价卡启动不等待远程 hash；单 worker、可取消、幂等 Stop；fake remote、last-good、并发/race 回归|
|O03|国内配额按账号/配置身份归属，取消旧请求，快照优先；同次身份+快照更新不会误取消新 GET；有限缓存/退避，失败保留 last-good|
|O04|Ollama GET/POST 生命周期分开；旧结果/错误/finally 不覆盖新账号；快照更新期间初始 GET 仍可读取配置；不自动重发 POST|
|O05|用户摘要使用独立 AbortController、序号及 query key；卸载取消；同筛选失败保留旧值，换筛选不冒充旧结果|
|O06|统一 quota unknown/0/失败/last-good 状态、按钮键盘焦点与 aria；窄屏统计横向滚动，明暗/中英文适配|
|O07|Makefile critical 加入真实新增用例；完整前端测试继续保留；未禁用旧 lint 或降低 required-test 门禁|
|O08|标准库有界 SSE 离线计时工具与 8 项测试；网络必须显式 --url，默认不发真实请求；缺失 TTFT 不计 0，provider 来源无法证明时保持 unknown|

O08 离线使用：`python3 tools/benchmark_stream_ttft.py`。也可显式 `--fixture FILE`。默认 concurrency=1；上限 concurrency=160、requests=1000、整轮 duration≤600s、单连接 timeout≤120s，frame≤1MiB、响应≤16MiB。网络调用需显式 `--url` 与 `--body-file`，凭据仅从 `NURO_BENCH_API_KEY` 读取；拒绝重定向，摘要不输出正文/凭据。`first_progress_ms` 只统计注释心跳或明确的 transport progress，普通首个事件不冒充进度，缺失保持空值。没有执行真实容量、性能收益或升级前后 TTFT 对比，不能宣称固定提速比例。

## 验证结果

工具：Go 1.27.4；Node 24.13.0；pnpm 9.15.9；Rust 1.96.1（本地工具，未修改 CI 最低兼容约束）；golangci-lint 2.14.0；govulncheck 1.9.0。临时安全工具未加入项目依赖。

|检查|结果|
|---|---|
|保护契约/历史迁移/git diff --check|通过，20/283；未改批准基线|
|Python 工具测试|最后复核 43 项通过，包含本次 SSE 8 项|
|Go default、unit、build、vet|完整范围通过；最后补充修改另做对应包/计价回归|
|Go race|service/repository/pkg 完整范围通过；最后计价、区域、审计补丁另做 race 复验|
|Rust fmt/check/test debug/test release/clippy|通过；243 个测试，未修改 Rust 生产代码|
|隔离集成|通过；隔离 PostgreSQL + Docker/Redis fixture + release Rust；required-test 校验通过|
|前端|全量 lint、typecheck、201 文件/1276 测试、build 通过|
|embed 页面与构建|带真实前端构建的 embed web 测试通过；embed 服务构建成功，-version=0.2.22-nuro.1，go version -m=go1.27.4；仅执行 -version，没有启动服务|
|Go lint|默认输出 172 项；关闭输出截断后起始 HEAD 与当前均 551 项，按文件+诊断内容多重集对照新增/删除均 0；增量检查 0 问题|
|漏洞/依赖|govulncheck 可达漏洞 0、导入包漏洞 0；require 模块另有 7 个未调用漏洞，不能宣称依赖全无漏洞。pnpm audit 返回有 advisory，经现有例外策略验证通过；未改例外|
|部署检查|只做原脚本语法与 fake/临时凭据 fixture；没有真实部署。可选 datamanagement 源码不存在，记不适用，不算通过|
|视觉|375/768/1440 × 明暗 × 中英共 12 图；无页面错误/页面横向溢出；人工复核手机/桌面明暗，中英标题与费用列。表格在自身容器横向滚动|

完整门禁执行：`GOTOOLCHAIN=go1.27.4 PATH=/tmp/nuro-v0222-tools:$PATH NURO_CACHE_TEST_DSN=<隔离测试库> python3 tools/check_upgrade.py --log-dir /tmp/nuro-v0222-final-gates`。整轮结果 `success=false`，唯一失败的执行检查为存量全量 golangci。pnpm audit 的 exit=1 是捕获 advisory，后续策略检查通过；没有改扫描器结果。

随后新增的协议/配额/priority 回归、审计快速过滤分别完成对应包测试与 race；没有因旧日志中的早期失败误报当前结果。可复核机器报告、增量 lint、基线对照、构建信息和截图位于本轮输出目录 `sub2api-v0222-implementation-20261117`。集成 skip 必需项由既有检查器强制验证，不能由总测试数量推断未跳过。

## 最后一轮独立审查补充

先核对 133 个文件的上一轮 SHA256，确认用户没有插入新改动，再复查方案、退款/补偿、配额、计价、协议、sticky 和前端生命周期。本轮直接修复以下四类问题：

1. 配额清窗口范围：缺失窗口清理仅限 Kimi；其他平台保留原来的部分更新语义。Kimi 新容器及旧 `limits/detail`、`usage` 任一声明畸形时，不允许用另一个有效窗口的成功结果清掉旧快照。mock 上游经过真实 query/persist 入口验证失败时不调用 UpdateExtra，另验证可信 Kimi 快照仍可清理缺失周窗。
2. Carbon/Hydrogen priority 定价：日期价卡策略不再覆盖独立配置的 priority 输入、输出及缓存读取价格，含显式零值；标准价与 priority 覆盖分开。回归先在旧实现复现失败，再通过真实 override 文件和统一计费入口验证临界日期两侧的实际费用。没有改变计费引擎原有零 priority 回退规则。
3. 工具声明引用：`additional_tools` 的 opaque ID 即使以 `call_` 开头，其 `item_reference` 也保留相同声明身份；只有非声明引用继续使用原函数调用 ID 规则。覆盖 preserveReferences 开关两种状态及独立 tool output 的 call_id。
4. 离线计时：不再把普通首个 SSE 事件记为传输进度，明确 transport progress 与可见内容分别计时，缺失不填零；provider 来源仍保持 unknown。

本轮复验：全量 Go default/unit 通过；最后工具身份补丁另做完整 service 默认测试，以及 service/repository/pkg 全范围 unit race，均通过；完整 unit vet 和 embed 构建通过。前端全量 lint/typecheck、201 文件/1276 测试通过；Python 43 测试通过；新增 Go lint 为 0。全量 lint 去掉输出数量截断后与起始版本均为 551 项，文件+诊断多重集无差异。冻结文件/历史迁移及 secret scan 通过。构建仅执行 `-version`，确认 0.2.22-nuro.1 / Go 1.27.4，没有启动服务。

本轮没有修改 UI 样式，沿用上一轮 12 张真实组件视觉证据；没有重跑外部依赖集成或生产性能测试，本轮配额入口使用 mock 上游。新日志与摘要位于输出目录的 `review-20261117/`，上轮完整隔离集成证据继续单独保留。

## 扩大范围代码审查补充

继续核对本次 133 个未提交文件的调用方、共享对象、异常和并发路径，复查退款认领/补偿、分组事务、配额、计价、工具续链、流式错误、sticky 与前端请求归属。直接修复三类问题，均先用回归复现，再验证修复：

1. 国内配额 UI：显式本地 `cn_billing_mode=payg` 优先于旧凭证 `account_mode=coding`，与后端一致；非法/缺失本地值才回退旧凭证。协议端点、智谱组织/项目、请求头覆写或代理变更会使同账号旧请求失效，避免晚到响应覆盖新结果。无关使用时间/并发遥测变化不额外探测。
2. Grok 缓冲工具分类：结束事件省略 `call_id` 时沿用已积累的调用身份，服务端搜索不会变成客户端函数调用。经 Chat Completions 与 Messages 的真实缓冲入口验证，保留同前缀客户端函数、非 Grok 既有行为及原始输入/输出/cache-read usage。
3. 工具 schema 正则兼容：解码使用 `json.Number`，修复重新编码时大整数及高精度小数的舍入；枚举、default、minimum、const 和 examples 中数字原值有回归，实例数据保持原语义。

本轮 Go default、完整 unit、service/repository/pkg 全范围 unit race、完整 unit vet 均通过；前端全量 201 文件/1291 测试、lint/typecheck/build 通过；实际前端产物的 embed 页面测试与服务构建通过，二进制仅执行 `-version` 并核对 Go 1.27.4 / 0.2.22-nuro.1。Python 43 项通过；20 冻结文件/283 历史迁移、secret scan 和 diff 检查通过。全量 Go lint 再次关闭输出截断，与起始 HEAD 均 551 项，逐条对照新增/删除 0，`release_ready=false` 保持。

本轮未改 UI 样式，未重新执行外部依赖集成、真实付费上游、生产性能或部署验证；上轮隔离集成及 12 张组件视觉证据单独保留。本轮日志、失败复现和最终 SHA256 位于输出目录 `expanded-review-20261117/`。本轮仅局部修改 3 个生产文件、4 个回归测试文件及两份验证记录；保护 manifest/baseline、冻结文件和依赖未改，未提交、push 或部署。

## 再次独立审查结论

本次审查开始时，133 个未提交文件与 `expanded-review-20261117/reviewed-files-sha256.json` 全部一致，没有新增、删除或插入修改。继续复查退款发起/查询/补偿、价格快照及显式覆盖、共享价卡、配额失败恢复、工具续链、流式终态、sticky 和前端请求生命周期，未发现新增需要修复的问题，本次未修改生产代码或测试代码。

重新执行升级回归及缓存创建、markup、强隔离、sticky、guard 相关测试：Go unit race 539 项通过、7 个包通过；1 项 PostgreSQL 集成测试因未提供隔离 DSN 跳过，不记为通过。前端配额、费用图表、使用记录和渠道字段 4 文件/47 项通过。20 冻结文件/283 历史迁移、secret scan、diff 检查通过。生产及测试文件指纹未变，因此上一轮全量 Go default/unit/race、前端 1291 项、构建/vet 和 551/551 lint 对照证据仍对应当前源码；本次没有把选择性复验计作重新执行完整门禁。Sol 第一、Terra 第二、上游模型审计默认隐藏均复核保留。

本次仅更新审查记录，未提交、push、部署或访问真实付费上游，未改保护基线、依赖、SQL 或 UI 样式。新证据位于输出目录 `closing-review-20261117/`；`release_ready=false` 保持。

## 最后一轮扩大审查补充

开始时 133 个未提交文件与 `closing-review-20261117` 指纹一致。扩大检查退款补偿、工具身份、区域价卡、配额恢复、健康同步、流式错误和前端生命周期后，修复两类生产问题，并修正一处既有测试夹具：

1. Fable 恢复依据：三分钟缓存中的旧低利用率结果可能清掉后来才写入的模型限流。现在仅实际执行本次上游查询的 singleflight 发起方可以尝试恢复，缓存读取和等待方不授权清理；窗口时间缺失、畸形或已过期也不能证明恢复。仍只清 owner token 完全匹配的 Fable scope，不增加查询或绕过现有缓存 TTL。旧缓存展示保留；清理失败后等待既有新查询再尝试。真实查询入口的回归先复现旧实现失败，再验证缓存不清理、有效新窗口可恢复、重复缓存不再次恢复及无效窗口不清理。
2. Redis 健康订阅：扩大 race 检测实际捕获 `runSubscriber` 复制主客户端可变维护配置时的数据竞争，并伴随复用 push handler registry 的重复注册。订阅客户端改为独立初始化这两个内部对象，保留既有连接凭证、健康 I/O 超时、key、TTL、事件格式和调度规则；没有改全局 Redis 客户端配置或升级依赖。原失败的跨实例传播测试在修复后连续 100 次通过 race，未再出现数据竞争或重复注册。
3. 取消测试夹具：既有 Antigravity 测试在 cancel 后立即放行成功响应，使两个结果竞速，整轮复验出现一次错误断言。取消场景现在保持服务端阻塞，待客户端取消结果返回后在清理时放行，避免测试服务器关闭等待；成功/部分失败/auth 场景继续显式放行。只调整测试同步，不改变生产取消语义。四个场景连续 100 轮、500 个测试事件通过 race，原始失败及夹具迭代日志保留。

本轮全量 Go unit 在 Fable 修复后通过：13,662 个测试/子测试、52 个包通过，12 项跳过；随后 Redis 与测试夹具修正后，最终扩大 unit race（service/repository/handler/pkg/payment/web）12,971 个测试/子测试、37 个包通过，12 项跳过，无失败或数据竞争。跳过项包含隔离 PostgreSQL、真实 Rust 与可选网络条件，不能算通过。前端全量 201 文件/1,291 项、Python 43 项通过；最终生产源码 vet、embed 构建通过，二进制仅执行 -version 并确认 Go 1.27.4 / 0.2.22-nuro.1。全量 Go lint 关闭输出截断后仍为起始 551 / 当前 551，诊断逐条对照新增/删除均 0，release_ready=false 保持。20 冻结文件/283 历史迁移、secret scan 与 diff 检查通过。

本轮证据单独保存在输出目录 `broader-closing-review-20261117/`。未执行外部依赖集成、真实付费上游或生产部署；未改冻结文件、批准基线、历史 SQL、依赖或 UI 样式。

## 再次扩大审查：读取中断分支

本次开始时 135 个未提交文件与上一轮最终指纹全部一致。继续复查协议转换与终态、响应审计、metadata 强隔离、退款查询、Vertex/显式缓存计价、统计调用方及前端异步生命周期，确认并局部修复一处 Antigravity 异常路径遗漏：已发送排队 ping 或仅有推理内容时，如果上游读取失败，原分支返回空结果及 failover 错误，丢失已经观测的真实 usage，也没有进入已有的失败 sticky 解绑入口。上层已有响应字节检查，未确认实际发生重复请求。

修复仅在该读取错误分支补上与 EOF/超时相同的已提交检查：返回现有 committed-no-content 错误及真实 usage，复用现有协议错误终态去重和 handler sticky 处理；未提交时继续原有 failover，取消请求仍返回中性结果。不增加查询、timer 或首 Token 等待，不改 OpenAI relay/coordinator、缓存 key、计费公式或冻结文件。

回归覆盖 Chat Completions/Responses、未提交/排队 ping/仅推理，以及 unexpected EOF/超长 SSE 行。旧实现 8 个已提交场景复现失败；修复后连同仅推理终态、取消行为连续 100 轮 race，共 1,700 个测试事件通过。最终生产源码扩大 Go unit race（service 及 openai_ws_v2、repository、handler、pkg、payment、web）12,984 个测试及子测试、37 个包通过，12 项测试/3 个无测试包跳过；隔离 PostgreSQL、真实 Rust 与可选网络条件的跳过项不计通过。新测试随后按 lint 建议将 if/else 改为 switch，最终该测试单独 race 13 个事件通过。

前端全量 201 文件/1,291 项、lint、typecheck，Python 43 项及 Go unit vet、embed 构建通过；二进制仅执行 -version 并核对 Go 1.27.4 / 0.2.22-nuro.1。最终全量 Go lint 关闭输出截断后，基线/当前仍为 551/551，逐条新增/删除均 0，release_ready=false 保持。20 个冻结文件、283 个历史迁移、secret scan 和 git diff --check 通过。Sol/Terra 前端顺序、上游模型审计默认隐藏复核保留。

本次仅改 1 个生产文件、1 个测试文件与审查记录，证据位于输出目录 followup-expanded-review-20261117/。未重跑外部依赖集成、前端构建/视觉或生产性能验证，未执行真实付费上游、提交、push 或部署；未改依赖、保护基线、历史 SQL 或 UI 样式。

## 已知限制

- Sticky CAS 比较 account ID，不是新的 generation key；不同账号迟到写可拒绝，相同账号 A→B→A 的 ABA 无独立版本 fence。为保护既有 key/sticky，不扩大缓存协议。
- Gemini 没有调用 ID 的字符串最终快照与完全相同的新调用，协议上不可区分；保守保留快照。同名不同 ID 与独立 object 调用有回归，未宣称任意无 ID 交错可恢复。
- Carbon 优惠/标准价分界按 UTC 2026-12-01 00:00:00，临界前后纳秒测试；Hydrogen 独立价格。当前仅 implicit caching，显式缓存接口拒绝；AG 仅广告有上游证据的 Carbon。
- 未运行生产金丝雀、真实付费上游、两台真实宿主机故障演练或升级前后性能对比。测试使用隔离 fixture，不证明生产所有网络/容量条件。
- 本轮创建的隔离 PostgreSQL fixture 在完成验证后删除；没有操作其他运行容器、镜像或生产数据。

## 61 组逐项状态

详细执行边界、原上游 SHA 与证据索引见 [v0222-status.json](v0222-status.json)。这里的“已适配”仅指第二版方案允许的范围。

|编号|业务组|结果|
|---|---|---|
|01|登录身份 nil 防护|已适配并验证|
|02|支付宝退款幂等|已适配并验证|
|03|RPM 分组作用域|保留本地实现并验证|
|04|分组价格/RPM 同步事务|已适配并验证|
|05|Fable 配额与窗口恢复|已适配并验证|
|06|Anthropic JSON 热路径优化|按方案不引入|
|07|Gemini Carbon 与 Hydrogen|已适配并验证|
|08|Gemini response_format|已适配并验证|
|09|发布版本|已适配并验证|
|10|赞助商与宣传素材|按方案不引入|
|11|Codex forks 父缓存 key|保护阻断，未开放|
|12|Grok 专属错误解析与脱敏|已适配并验证|
|13|Antigravity thoughts 与超时锚点|保留本地实现并验证|
|14|Qwen embedding/reranker 兜底价|已适配并验证|
|15|Bedrock 错误分类与真实 usage|已适配并验证|
|16|排队 ping 关闭与合法 SSE 错误|已适配并验证|
|17|加密函数调用内容|已适配并验证|
|18|分组日汇总非阻塞重建|按方案不引入|
|19|中途 system 转 developer|已适配并验证|
|20|客户端断开后的 sticky|已适配并验证|
|21|响应模型/档位/原生端点审计|已适配并验证|
|22|scheduler cooldown membership|保护阻断，未开放|
|23|精确白名单拒绝通配符|已适配并验证|
|24|Claude 并发 token 轮换 401|已适配并验证|
|25|国内配额快照 UI 与 critical CI|已适配并验证|
|26|Anthropic 兼容 effort 策略|已适配并验证|
|27|Gemini refetch 保留 last-good|保留本地实现并验证|
|28|per-turn beta 输出配置|已适配并验证|
|29|计费测试固定时钟|已适配并验证|
|30|OpenAI 配额 freshness 与监控中性|已适配并验证|
|31|透传模型过滤 opt-in|保护阻断，未开放|
|32|账号测试模型默认值|保留本地实现并验证|
|33|additional_tools 身份和 namespace|已适配并验证|
|34|Antigravity schema items|已适配并验证|
|35|CC→Responses strict 缺省 false|已适配并验证|
|36|Kimi monthly 与安全清快照|已适配并验证|
|37|Codex HTTP additional/untrusted context|已适配并验证|
|38|Go 安全工具链|保留本地实现并验证|
|39|工具 schema 正则兼容|已适配并验证|
|40|注入设置 HTML 禁缓存|已适配并验证|
|41|WebSearch 账号三态|已适配并验证|
|42|定价启动后台 hash 检查|已适配并验证|
|43|count_tokens header 去重|已适配并验证|
|44|TCP keepalive/TCP_USER_TIMEOUT|保护阻断，未开放|
|45|CC→Anthropic metadata.user_id|已适配并验证|
|46|Composite 对外模型名恢复|已适配并验证|
|47|实际扣费与账号成本统计分列|已适配并验证|
|48|first-output 磁盘 spool 回退|按方案不引入|
|49|OpenAI 首内容前后错误与 keepalive|独立部分已适配，冻结部分阻断|
|50|Ultra Fast 档位与模型计价|保护阻断，未开放|
|51|按用户 billing lane 批量结算|按方案不引入|
|52|整仓 zero-copy/unsafe 变更|按方案不引入|
|53|Gemini session hash 与强制缓存计费|保护阻断，未开放|
|54|DeepSeek 显式覆盖价|保留本地实现并验证|
|55|Antigravity 空流释放会话 sticky|已适配并验证|
|56|Vertex 区域端点与定价|已适配并验证|
|57|Gemini 并行工具与终态快照|已适配并验证|
|58|scheduler bucket 合并优化|保护阻断，未开放|
|59|scheduler capability 元数据|保留本地实现并验证|
|60|渠道 Bedrock boolean|已适配并验证|
|61|Grok 服务端 search 调用分类|已适配并验证|
