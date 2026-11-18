# 0.2.23-nuro.1 实施与验证

本轮基于 `83968b49c7dad83f53c32e682fae3615635851b2` 手工适配上游 v0.2.23，未合并上游、未提交、未推送、未部署。范围逐项记录于 [v0223-status.json](v0223-status.json)。原审批方案位于本机输出目录 `sub2api-v0223-plan-20261118`。

## 已完成

- 新增 GPT-6.2 Sol/Astra：精确已知别名、effort 校验、专用 instructions 启动解析、采样过滤、工具 Responses 限制、API Key manifest 缺省能力投影；保留上游已声明字段。API effort 支持 low/medium/high/xhigh/max，拒绝 none/minimal/ultra；不将 Ultra Fast 权益当 reasoning effort。OAuth manifest 保留原 passthrough。
- 两模型四 bucket 基础价格、priority 2x、flex/batch 0.5x、缺目录 fallback；长上下文严格 `>272000`，输入 2x/输出 1.5x。OAuth 官方附加费豁免与 D 定价快照对齐。Group/Channel 显式覆盖及免费 0 的原有优先级保留。
- 取消短合法终态的 72 字节过滤；Responses 终态选权威 nested envelope，普通 Chat/buffered 保留自身读取顺序；空对象不能遮住真实 usage，合法显式 0 不被别名覆盖。
- Anthropic 兼容用量保留 5m/1h presence 与部分更新，cache-only 终态不丢；native Messages/Chat/Responses 将 TTL 传到真实 cost/log。内部 TTL 字段不会额外输出到 OpenAI JSON。保留原图片用量字段、SignedThinking scope 与工具状态。
- 缓存创建总量 100、明细 20/30 时按 70/30 计费；保留原矛盾明细比例归一和 near-MaxInt 保护。真实计费和账号统计使用同一分桶规则，价格来源仍分别处理。
- 数值解析校验非负整数、精度与 int 边界，保留已有整数字符串兼容；浮点、小数、指数、溢出、非法类型有定向回归。互斥桶拆分不发生中间回绕；账务入口在成功副作用前拒绝不可表示的 aggregate。TTL overflow 转换不再生成正常零用量。
- 前端三个入口补齐：白名单、快捷映射、OpenCode 配置。6.1 Sol 第一、Terra 第二、6.2 Sol/Astra 紧随其后；新快捷项沿用 cyan/violet 与暗色主题。OpenCode 各模型配置对象独立，复制配置保持可解析；使用记录的上游模型审计仍默认隐藏。
- 后端与前端版本同步为 `0.2.23-nuro.1`，Go/Docker/依赖锁未改。

## 保留和未实施范围

20 个冻结文件及全部 286 个基线历史 SQL 逐文件 SHA256 一致；现有升级检查器的 283 条历史迁移也通过。217 条旧模型价格记录 deep-equal，仅增加两模型记录。没有改 scheduler、Edge/Rust 生产源码、强隔离、ABCD 请求策略、BPS/100000 阈值、prompt_cache_key/sticky、attempt/unknown 防重放、可靠队列或部署脚本。没有恢复已撤回的脏目录检查/私有恢复包。

B03/B04 Ultra 新权益、动态倍率和渠道 SQL/UI 受既有保护约束阻塞；B10 raw flush 冻结；B11 非等价 DeepSeek/Ollama 请求改造不纳入。版本号不代表这些能力完整落地。

B09/O07 精确 idle deadline 按原方案的条件门禁暂缓：现有 `handleStreamingResponse` 在 `Scanner.Scan()` 获得完整行后更新 `lastReadAt`，随后可能阻塞于 events 交付；select 端还会执行下游写入。单纯替换 ticker 不能区分持续字节但未成行、channel 背压和真正等待上游。未建立满足全部场景的计时证据，因此本轮保持旧 watchdog；没有声称做过精确 deadline/慢客户端性能优化或放宽重放。

非法 provider 字段会保留结果对象中可确认的计数并从 RecordUsage 返回错误，调用方沿既有错误日志/可靠提交路径报告；不把无效字段变成零价成功、不估算或重放。费用无法完整确认时拒绝整笔结算；本轮没有新建持久化异常用量账本或自动部分扣费能力。正常 partial/failed 流已确认合法 usage 继续使用原可靠提交与幂等机制。

## 验证结果与限制

工具：Go 1.27.4、Node 24.13.0、pnpm 9.15.9、golangci-lint 2.14.0、govulncheck 1.9.0；临时扫描工具和视觉 harness 均在仓库外，不进入项目依赖。

|检查|结果|
|---|---|
|Go default/unit|全量通过；unit 13,698 个测试/子测试通过、12 跳过。最后新增边界补丁另跑相关 package/定向 race；精确证据见输出日志|
|Go race|service/repository/pkg 全范围 unit race 通过，11,162 个测试/子测试通过、8 跳过；跳过不算通过|
|Go vet/build|unit vet、全包 build、真实前端资源 embed web 测试和 embed 服务构建通过；二进制仅运行 -version，确认版本与 Go 工具链|
|集成|独立临时 PostgreSQL、testcontainers Redis、真实 Rust binary；全量 integration 9,286 项通过、3 项跳过，required-test 检查通过；不使用生产 DSN|
|前端|lint:check/typecheck、201 文件/1,292 用例、production build 通过|
|前端视觉/交互|真实 EditAccountModal/UseKeyModal，1440/768/375 × 明/暗 × 两入口，共12组；无页面横向溢出/脚本错误，快捷映射、键盘焦点和配置复制检查通过；API 由合成 fixture 拦截|
|Rust|fmt/check/debug/release 243 tests/clippy 通过；新增8个6.2价格/阈值/OAuth fixture 由现有 Go/Rust 共用 golden 再次验证通过|
|Python/保护|43 项工具测试通过；SHA、迁移冲突、Go/Docker 对齐、secret scan、diff check 通过|
|Go lint|全量关闭输出截断，与基线逐条多重集比较均551项，新增/删除0；全量 lint 本身不通过，未改规则隐藏问题|
|安全|govulncheck 可达0、导入包0；require模块仍有7个未调用 advisory。embed 源码同样可达0/导入包0；二进制扫描仍报告上述7项公告及23个符号命中，不能声称产物不存在已知漏洞符号。pnpm audit 有 advisory，既有例外策略验证通过，未改例外|
|部署检查|三个脚本语法及 fake/bootstrap 测试通过，脚本无修改；可选 datamanagement 源码不存在，不计作测试通过|

`release_ready=false`：存量全量 Go lint 尚未清零；保护阻塞项未开放；没有执行生产部署、付费上游调用、生产 canary 或两副本故障演练。上述本地验证不能证明线上绝对无风险。

已有 usage benchmark 的候选样本约 1.9µs/op、96 B/op、1 alloc/op，初始同机基线约2.0µs、同分配数；这只涉及终态解析，不能代表首 Token p99 或吞吐改进。并发构建期间的一组基线测量受资源竞争污染，未用来宣称性能收益。没有新增首 Token 前 DB/Redis/目录请求/计时器。

完整日志、机器结果、保护对照与12张截图归档于本机 `~/.codex/outputs/sub2api-v0223-implementation-20261118/`。旧版本报告保留其历史记录，本文件只描述本轮。

最终交替 benchmark：同机依次运行3组 baseline/candidate，`-benchtime=300ms -count=1`；baseline=[2033.0, 1953.0, 1964.0] ns/op，candidate=[2694.0, 1856.0, 1853.0] ns/op；均96 B/op、1 alloc/op。仅记录本地微基准，不据此承诺生产收益。

## 最终代码审查

- 通用 Anthropic → Chat 路径现在用合并后的计数生成流式 `usage`，覆盖缓存别名、供应商显式 prompt 总量和跨帧 TTL。只提供缓存命中别名时保留已有 `input_tokens`，与原生解析一致。Antigravity 转换器生成的是标准 Anthropic 用量字段，现有转换路径已处理，无需增加别名转换或改预缓冲。
- 协议层对单个计数使用精确整数解析，也接受可精确表示的 JSON 小数、指数整数及整数字符串。坏字段不丢弃同事件的合法计数、内容或 stop reason。内部保留错误标记，既有账务入口拒绝无效用量，Chat/Responses 终态不发布无效的 `usage`。Chat → Responses 的总量求和也检查溢出。
- Anthropic 原生流在写出前合并已观察到的上游计数，覆盖首次写入失败的场景，保留原首包统计和断连排水行为。TTL 覆盖会清掉冲突分桶，即使目标桶的数量已经等于 aggregate。
- 最终全量 Go unit 通过 13,717 项、跳过 12 项。service/repository/pkg 全范围 unit race 通过 11,171 项、跳过 8 项，另补跑完整 apicompat race，验证随后新增的计数解码用例。首轮同时构建和扫描时，两项已有时序断言失败。独立重跑全范围通过，两项用例各重复 5 次通过，保留原断言和生产时序逻辑。
- Go unit vet、embed 页面测试与服务构建通过，产物版本为 `0.2.23-nuro.1`，使用 Go 1.27.4。重新运行前端全套与 Rust 检查，1,292 项前端用例全通过。前端视觉沿用前次 12 组证据，前端改动文件 SHA 与该次验证一致。
- 最终另建临时 PostgreSQL，运行全量 integration：9,303 项通过、3 项跳过，全部指定必跑用例通过。测试实例已清理，未使用生产 DSN。
- 20 个冻结文件、286 条历史 SQL 和 217 条旧价格重新核对一致。`git diff --check`、保护合同和离线密钥扫描通过。Go lint 按规则、文件和诊断内容比较，仍为基线 551 项。embed 源码扫描未发现可达或导入包漏洞，原 7 项仅模块依赖公告仍在。

本轮最终日志和状态归档于原输出目录的 `final-review/`。早先一次全阶段检查缺少独立 PostgreSQL DSN，且 PATH 未找到扫描工具，整体返回失败；随后已以明确工具路径、独立 DSN 分别完成扫描和集成验证，保留原始失败记录。上述限制和受保护/暂缓项仍适用，`release_ready` 不变；未提交、推送或部署。

## 测试弹窗排序与扩大审查

- 依用户后续要求，两个测试弹窗均将 GPT-6.2 Sol、GPT-6.2 Astra 排在最前。OpenAI 正式型号优先于映射到它们的自定义别名，兼容 `openai/` 前缀；请求仍发送原始公开模型 ID，不增加账号未开放的模型。白名单、快捷映射和 OpenCode 的顺序维持本轮原方案。
- 管理页实际使用的测试弹窗补齐异步账号切换与卸载保护，旧模型查询和旧测试结果不会写回新账号。OpenAI 文本测试可取消，并沿用普通账号弹窗的 190 秒浏览器超时；按映射上游型号识别图片测试，图片请求不套用文本超时。两弹窗复用同一图片能力判断。
- 管理弹窗兼容 `data:` 和 `data: `、EOF 前无换行的最后事件，EOF 缺失终态时明确报错。成功后继续读取账号恢复的 `account_updated` 事件，保留账号状态刷新；结果之后的重复内容/终态不再覆盖已显示结果。reader 按标准浏览器 API 清理，测试替身补齐 cancel/releaseLock。
- 最后一次全量前端为 202 个文件、1,301 项测试通过，无未处理异常；全量 lint、typecheck、生产 build 通过。补充回归覆盖换账号旧查询、文本超时/计时器清理、映射图片免文本超时、取消、卸载、缺失终态和结果后的账号刷新。真实管理弹窗 1440/768/375 × 明暗 6 组，排序、取消、页宽与脚本错误检查通过，截图/报告归档 `modal-review/`，API 和测试请求均使用合成 fixture。
- 重新构建 embed 服务并仅执行 `-version`，确认 `0.2.23-nuro.1`。后端生产源码 SHA 与上次最终验证一致，本次沿用其 unit/race/集成证据；20 个冻结文件、286 条历史 SQL 再核验无变化。没有修改调度、Edge、缓存策略、可靠账务或部署脚本，未提交/推送/部署。存量 lint/依赖公告与原保护阻塞/暂缓项仍适用。

## 续审：测试流接收边界（2026-11-19）

- 继续核对全部未提交改动；接续时 63 个改动文件指纹与上一轮 `modal-review/` 一致。扩大检查模型身份、映射、客户端配置、价格与计费入口，未发现新的确定后端缺陷。
- 修复管理页 OpenAI 测试弹窗缺少接收预算的问题：按累计原始字节限制为 32 MiB，包含图片和未成行 SSE；最多保留最近 2,000 行输出，与普通弹窗已有边界对齐。其他平台媒体测试不增加此限制，未改变请求模型 ID、成功后的账号恢复事件读取或后端行为。
- 新增 4 项回归，覆盖累计完整行超量、单行超量、输出截断后仍保留结果/账号刷新，以及切换账号后丢弃旧流结果。超量用真实 `ReadableStream` 验证 reader 取消；定向 29 项通过。最初测试源在触发限制前已预取关闭，未调用 underlying cancel；改为持续流源验证取消，不为测试修改生产清理逻辑。
- 重跑全量前端：202 文件、1,305 项用例通过；全量 lint、typecheck、production build 通过。重跑 embed web 测试、embed 服务构建并仅执行 `-version`，确认版本 `0.2.23-nuro.1`、Go 1.27.4。本轮未修改模板/样式，沿用上轮管理弹窗 6 组视觉证据。
- 20 个冻结文件、286 条历史 SQL、217 条旧价格再次核对一致；后端改动文件 SHA 与上一轮一致，沿用既有 unit/race/集成结果。保护合同、离线密钥扫描和 `git diff --check` 通过。证据归档于 `last-review-20261119/`；本轮仅新增上述前端逻辑、回归与验证记录，未提交、推送或部署。既有 lint、依赖公告和保护门禁限制仍适用。
