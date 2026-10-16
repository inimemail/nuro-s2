# v0.2.17 本地适配实施记录

状态：用户确认的升级项目与建议项已完成代码适配、本机回归及补充代码审查；尚未提交、推送或部署。已使用模拟 API 对实际前端组件做桌面/手机及亮暗主题截图检查，真实供应商和生产压力不在本机测试结论内。

- 上游范围：`Wei-Shaw/sub2api` 的 `v0.2.16..v0.2.17`，共 48 个提交（其中 30 个非 merge 提交）。上游代码在 `/private/tmp/sub2api-upstream-0217` 独立读取。
- 本地基线：`35cdebdc7d63ab41ac2ba5157606d89dccc7b783`，分支 `main`。
- 后端版本：`v0.2.17-nuro.1`；前端包版本：`0.2.17-nuro.1`。
- 逐项手工适配，没有 merge、rebase、cherry-pick 或整文件替换。没有新增数据库迁移或变更依赖、锁文件。
- `.git` 在本次环境中只读，未创建回滚标签；以上基线用于追溯。

## 实施对照

| 项目 | 最终实现与本地取舍 |
| --- | --- |
| Gemini Vertex 429 | 仅对无配额/重试明细的共享容量 `RESOURCE_EXHAUSTED` 取消兜底配额冷却，保留现有重试预算、取消和账号级错误策略；配额耗尽继续按原规则处理 |
| Vertex SA 凭据失效 | 仅识别 `invalid_grant` 的账号不存在错误，停止调度并安全换号；其他 token 获取错误不误停账号，公开错误脱敏 |
| Gemini 错误策略 | Native、Messages、Chat 对每个错误响应只检查一次；重试到新响应时重新检查，首次账号级 401 暂停不被重复检查升级为永久错误 |
| Anthropic 协议安全 | 请求和 count_tokens 使用 Anthropic 协议地址并保留本地 URL 校验；在构建请求前拒绝其他平台 OAuth 凭据误入 Anthropic 管线；保留显式 composite 路由 |
| Codex 导入 | 使用 workspace/account 与 user 组合身份，采用长度前缀防止键拼接碰撞；保留缺失身份时的兼容索引 |
| Responses 协议 | Go HTTP/SSE/WS/HTTP bridge 和 Rust SSE/WS 对缺失或 null 的 `created_at` 补字段，已有非 null 值保持原样；流内复用时间戳，WS 每轮独立；合成失败帧提供 object、时间戳和 sequence_number，无序号的裸 error 使用下一序号；修复裸 error 转终态后 EOF 再次补失败帧的问题，保留错误脱敏与原始内部 usage |
| GPT-5.5 退役 | OpenAI OAuth/setup/shadow 按实际映射目标筛选，API Key 保留；覆盖 compact 映射及全局兜底、旧选号及新调度器的最终重查；公开别名先投影再筛选，随后计算 ETag，空列表不会复活默认退役模型 |
| 透传边界 | 普通模型映射不改写本地 Responses 透传行为；透传最终请求另做发送前退役检查，防止旧映射掩盖实际仍为 GPT-5.5 的请求。不对这种本地拒绝施加账号冷却、健康惩罚或 sticky 驱逐 |
| 支付与退款 | 订单类型白名单；待确认退款保留扣减，禁止重复发起；成功不重复扣减，明确失败才归还；终态、归还和审计按事务收敛，未知/空查询结果继续 pending；失败补偿有审计与防重复扣减标记 |
| 支付供应商 | 支付宝保存可查询的退款请求号，业务错误不当作退款失败，无记录宽限 10 分钟；旧请求号无法可靠查询时要求核账。EasyPay 保留本地签名、金额、PID、重复参数及 HTTP 状态校验，使用原验签实例查询确认后再入账；请求 URL 中密钥不进入传输错误日志 |
| 退款补偿与缓存 | 生产仓储专用归还操作只恢复余额，不增加累计充值额，并复用外层事务；扣减/归还失效余额缓存，待确认退款提交后再次定向失效订阅缓存，避免旧订阅时长留在缓存中 |
| Stripe | `client_secret` 从同浏览器匹配订单的恢复快照读取，移出页面 URL；保留 resume_token 核验、异常存储处理及页面销毁/切换保护 |
| 智谱用量 | CREDIT_LIMIT 按 unit=3 识别 5h，unit=6 识别 weekly，不依赖 reset 时间排序推测 |
| Gemini/Claude 定价建议 | 加入 Argon 与 Cyber Permissive；Argon 优惠到 2026-12-01 00:00 UTC，此后使用常规价，Cyber 不参加优惠；接入本地同笔结算时间及供应商资格规则，显式价格覆盖优先，价格对象不共享改写；Claude 5.5 和 Gemini thinking-tier 别名按基础模型解析，精确自定义档位价优先 |
| Gemini 思考建议 | 根据实际模型转发 thinkingLevel/budget/includeThoughts，返回流式思考摘要；历史 thinking 块按兼容规则处理；usage 元数据记录实际转发档位，不仅依赖用户原始意图 |
| Antigravity 建议 | 保留本地显式映射、thinking 后缀及重试架构；加入 Argon/Claude 5.5 新映射、Pro/Ultra 细分档位和 UA；支持工具结果内联图片、原生 functionCall 签名及文本合并时 thoughtSignature 保留 |
| 模型目录与测试建议 | 同步目录保留 keyed map 的精确生成 ID；能力元数据作为编辑页面提示，不成为调度硬门槛，也不覆盖已有目标映射；使用实际转发地址获取目录，限制响应体且禁止重定向；测试按实际目标选择文本/图片协议，可返回图片预览，响应读取有界 |
| Token 下限建议 | `gateway.min_output_tokens` 默认 128，最小 16；仅兼容转换中已指定的输出上限使用该下限，较大值和省略值保留；不添加管理页、不覆盖账号阶段超时配置 |
| Web Search 建议 | OpenAI API Key 的 Messages 可复用已有搜索模拟；OAuth/setup 保留原生路径；前端使用三态选择并保留其他 extra 字段、渠道和全局规则 |
| 前端样式 | 搜索设置采用响应式卡片、暗色主题和可访问性标签；目录能力使用图像/思考/上下文/输出徽标；补齐中英文、新模型列表和图片测试判断，保留本地移动端与折叠筛选布局 |

## 保留本地实现的项目

- 不引入上游账号归属推断、歧义路由架构或模型目录持久化/调度重构：本地采用显式 composite 路由和显式模型映射，目录能力只作提示，避免改变现有业务路由。
- 不采用上游账号筛选布局回退：保留本地折叠筛选、移动端布局及现有账号管理交互。
- 不合入赞助商、README/图片资产、拆文件等非必要改动，不更新无关依赖。
- 不替换本地更完整的退款精确扣减、支付商户快照绑定、金额/签名安全检查和错误脱敏。
- 保留 Gemini 显式缓存、金额预留与 Recovery；保留普通用户 `/gemini-cache` 页面已关闭的本地状态。
- 保留部署菜单 2、私有仓库本地构建、随机管理员邮箱、`.env` 中初始管理员密码及首次部署流程；本次仅修改配置示例中的新 token 下限说明。

## 二开保护核对

附件列出的 20 个保护文件与基线逐一计算 SHA256：18 个内容完全一致。证据文件：`/private/tmp/nuro-v0217-protected-sha256.json`。

两处经过手工适配的保护文件，不能宣称“所有冻结文件完全未改”：

1. `openai_account_scheduler.go`：只增加退役模型资格检查及 compact 兜底参数传递；不改优先级、健康策略、sticky、倍率 guard、配额、并发槽和候选排序。
2. `edge-rs/src/main.rs`：只接入 Responses 时间戳/失败序号兼容及对应回归；不改缓存 A/B/C/D 或缓存加成算法、真实 usage 结算、execution-unknown、retry、lease、WAL、escrow 和首 Token 统计。

缓存创建策略/运行时/展示模块、raw Chat 转发、两种强隔离、Edge ingress/resume/settlement/SSE、补帧协调器、stream_session、连接池及 scheduler snapshot 的保护文件均与基线一致。其余共享入口中的窄适配由完整 unit 测试和定向 race 检查覆盖；这不是生产零回归保证。

## 本机验证

| 检查 | 结果 / 证据 |
| --- | --- |
| Go 全量 unit | 最终补丁后 `go test -tags unit ./...` 通过；`/private/tmp/nuro-go-verified.log` |
| Go 定向 race | service/repository 的退役模型、Responses、选号、退款、归还余额、Vertex 及错误策略相关回归通过；`/private/tmp/nuro-race-verified.log` |
| WS 异步测试稳定性 | 等待旧代预热连接真正关闭后再断言，保留池状态及关闭断言；该用例在 race 下连续 100 次通过，连接池生产逻辑未变；`/private/tmp/nuro-ws-pool-repeat.log` |
| Go 静态检查 | `go vet ./...` 通过；`/private/tmp/nuro-vet-verified.log` |
| 退款真实仓储适配 | SQLite/Ent 测试核验归还余额不增加累计充值额、外层事务回滚、终态认领/审计及重复操作拦截；不是 PostgreSQL 集成验收 |
| 前端完整 Vitest | 188 文件、1170 项通过；`/private/tmp/nuro-frontend-final2.log` |
| 前端 typecheck | 通过；`/private/tmp/nuro-type-final.log` |
| 前端 ESLint | 独立运行通过；`/private/tmp/nuro-frontend-lint-final.log` |
| 前端生产构建 | `pnpm build`（含 vue-tsc）通过；`/private/tmp/nuro-frontend-build-verified.log` |
| Rust | `cargo test --locked` 235 项通过；`/private/tmp/nuro-rust-verified.log` |
| Rust 静态检查 | `cargo clippy --all-targets -- -D warnings` 通过；`/private/tmp/nuro-clippy-verified.log` |
| Go 发布构建 | 嵌入本次前端产物的构建通过，版本输出为 `v0.2.17-nuro.1`；`/private/tmp/nuro-backend-build-verified.log` |
| 范围与格式 | `git diff --check` 通过；依赖锁、数据库迁移、路由和部署脚本未改 |

## 本轮补充审查与修复

用户要求再次扩大代码审查后，对当前所有改动的入口、调用方、补偿和前端保存路径再次核对，修复如下：

1. **Argon 动态价格与缓存 D**：OpenAI API Key 可使用任意文本模型别名，因此 Argon 也可能进入 D 的展示换算。给 `openai_cache_creation_pricing.go` 既有动态目录保护补入 Argon 及其档位别名：没有完整固定价格时保留原始用量；分组/渠道固定价格与完整显式覆盖继续生效。未修改 A/B/C/D 分配算法、原始 usage 或真实计费。
2. **订阅退款撤销后的恢复**：扣减天数会使订阅立即失效时，原流程软删除订阅，但退款失败时仍尝试延长已删除订阅，导致补偿失败。现在在计划及 pending 审计中保存撤销标记，失败时恢复同一条原订阅，保留原到期时间和用量；不会再把购买天数加一遍。pending 明确失败的恢复沿用订单终态事务；即时退款失败回滚也复用该路径。保留同分组重复订阅冲突保护，发生冲突时记录补偿失败，不擅自覆盖新订阅。
3. **本地 Messages → Chat 转换配置遗漏**：本地工具调用思考回放专用转换函数原来固定使用 128。新增保留回放行为的可配置入口，并在本地 Messages → Chat 路径读取 `gateway.min_output_tokens`；默认仍为 128。回归同时检查实际上游请求上限和工具回放思考内容，普通 raw Chat 转发保护文件未改。
4. **前端移动端溢出**：实际 Chromium 截图发现 Antigravity 编辑窗口两个已有帮助提示框在 390px 视口把滚动内容撑至 499px。仅调整提示框的定位基准、宽度及箭头响应式样式，修复后内容宽度与可视宽度均为 364px。模型名称增加宽度约束和换行，避免上游长名称撑破能力徽标区域。没有变更调度或超量请求字段逻辑。

补充验证结果：

| 检查 | 本轮结果 / 证据 |
| --- | --- |
| 最终 Go 全量 unit | `go test -tags unit ./...` 通过，service 92.170s；`/private/tmp/nuro-go-review-final.log` |
| 新增路径定向回归 | 退款撤销恢复、一次收敛、重复订阅冲突、Argon 动态/固定价格、实际 Messages → Chat 上限及思考回放通过；`/private/tmp/nuro-review-extra-paths.log` |
| race | service/repository 的关键升级及退款/缓存路径通过；`/private/tmp/nuro-race-review-final.log`。最终兼容转换、退款与缓存补充 race 通过；`/private/tmp/nuro-race-extra-paths-final.log` |
| Go 静态检查 | `go vet ./...` 通过；`/private/tmp/nuro-vet-review-final.log` |
| 前端全量 | 188 文件、1170 项测试通过；`/private/tmp/nuro-frontend-review-final.log` |
| 前端 lint / 类型 / 构建 | lint 及含 vue-tsc 的生产构建通过；`/private/tmp/nuro-frontend-lint-review-final.log`、`/private/tmp/nuro-frontend-build-review-final.log` |
| 真实组件截图 | 模拟 API / 虚构账号，实际 Vue 编辑组件，1440px 桌面与 390px 手机，覆盖搜索卡片、目录能力及亮暗主题，四组均无横向溢出或运行时异常；`/private/tmp/nuro-ui-review-results.json` 与同目录 `nuro-ui-*.png`。临时预览文件已删除，临时服务和独立浏览器已关闭 |
| 后端发布构建 | 嵌入最新前端的 `go build -tags embed -trimpath` 通过；`/private/tmp/nuro-backend-build-review-final.log` |
| 二开保护复核 | 20 个列明保护文件仍为 18 个原样、2 个此前记录的窄适配；89 个新增/改动 Go 文件格式正常，`git diff --check` 通过 |

Rust 在本轮补充审查中没有新增改动，沿用前一轮 235 项测试和严格 clippy 的已通过结果。本轮检查没有提交、推送、部署或修改真实账号/支付数据。

## 最后一次扩大范围审查

继续复查升级改动及其调用链，包括 Gemini 流式返回与结果分类、退款查询/补偿、商户快照、Stripe 本地恢复、显式路由、退役模型筛选、动态价格和前端保存逻辑。新增确认并修复两类问题：

1. **Gemini 思考流断连分类遗漏**：Messages 新增思考块曾直接写入原始 writer，绕过既有首个写错误记录；Chat 新增思考分支在写失败时返回的结果遗漏断连与中性标记。现在 Messages 全部思考事件沿用 `stickyErrorWriter`，Chat 沿用 `disconnectedResult()`，避免中断请求被当作成功并触发成功侧效果。故障回归让请求上下文保持有效，仅在实际思考内容写入时返回 `io.ErrClosedPipe`，核验两种接口的断连/中性分类及当前响应已收到的用量；修复前两项均失败，修复后通过。
2. **退款查询漏校验历史商户快照**：发起退款已有快照元数据校验，但 pending 查询路径只校验实例绑定，未校验修改后的商户/币种元数据。现在查询前执行与发起退款相同的校验，不匹配时写审计并保留 pending，不向该配置查询、不扣减或归还。回归覆盖模拟供应商将返回成功、失败、pending 的三种情况，均在查询前拒绝；恢复匹配配置后可继续查询。修复前三项均失败，修复后通过。沿用既有快照字段与校验规则，不新增迁移。

最终验证：

- 新补丁后 `go test -tags unit ./...` 全量通过；`/private/tmp/nuro-go-expanded-final.log`。
- Gemini、退款、缓存价格、退役模型、Responses 时间戳和搜索/凭据相关定向 `go test -race` 通过；`/private/tmp/nuro-race-expanded-final.log`。
- `go vet ./...` 及嵌入前端的发布构建通过；`/private/tmp/nuro-vet-expanded-final.log`、`/private/tmp/nuro-build-expanded-final.log`。新构建版本输出仍为 `v0.2.17-nuro.1`。
- 89 个新增/修改 Go 文件 gofmt 检查正常，`git diff --check` 通过；20 个保护文件重新计算 SHA256，仍为 18 个原样、2 个此前列明的窄适配，本轮未新增保护文件变更。
- 本轮没有修改前端或 Rust，沿用上一轮前端 1170 项测试、lint、构建与四组实际组件截图，以及 Rust 235 项测试和严格 clippy 的通过结果；未重复执行这些未变化模块的检查。

两次扩大范围检查之后，当前未发现其他已确认而未修复的问题；真实供应商、数据库集成和生产压力的验收边界仍如下所列。未提交、推送或部署。

## 部署与回退边界

- Go 与 Rust Edge 应一起更新，以覆盖两端的 Responses 协议适配。无需为本次升级执行新增数据库迁移，token 下限未配置时使用 128。
- GPT-5.5 仍可用于 API Key 上游；OAuth 类账号不再向已退役目标发送请求。显式映射只有在该入口原本会应用映射时生效，Responses 透传不因本次升级自动改写映射。
- 新版 `REFUND_PENDING` 可能保留余额/订阅扣减。**存在这类未结订单时，不可直接降级到不识别 held 审计字段的旧退款逻辑**；先用新版查询收敛或人工核账，保留订单及审计，避免重复扣减或误归还。
- 旧支付宝 pending 订单如果没有保存可查询的退款请求号，需要核账处理；不会盲目重新申请退款或归还余额。
- 本轮未做真实 OpenAI/Gemini/Antigravity 调用、供应商支付/退款和账单核对，也未做真实 PostgreSQL/Redis Cluster 集成、生产多副本故障演练或压测/Canary。前端截图使用模拟 API 和虚构账号，未连接生产后台；代码测试和本地组件截图不能代替这些生产观察。

## 上游提交清单

以下为本次比较的完整提交列表（包括 merge 提交）；业务适配与保留项目见上表。

| Commit | 日期 | 上游内容 |
| --- | --- | --- |
| `4d9ac3e48210` | 2026-10-13 | chore: sync VERSION to 0.2.16 [skip ci] |
| `89ad4c2caa0a` | 2026-10-13 | fix(gemini): Vertex 共享容量不足的 429 不冷却账号，改为同号退避重试 |
| `9c2cca9e6272` | 2026-10-14 | chore: update sponsors |
| `20c4bf5ae59e` | 2026-10-14 | fix(gemini): Vertex SA 已不存在时账号置为 error 并换号 |
| `14cd5871752d` | 2026-10-14 | fix(gemini): 每个上游错误响应只检查一次错误策略 |
| `c6c9935ae370` | 2026-10-14 | feat(gemini): 补齐 Gemini 4 Argon 计费 |
| `867563ae76f3` | 2026-10-14 | fix(gateway): honor Anthropic protocol base URLs |
| `26e55cf332e7` | 2026-10-14 | fix(openai): emulate web_search on /v1/messages for OpenAI API Key accounts |
| `7de15113c77c` | 2026-10-14 | fix(openai): exclude OAuth accounts from web search emulation |
| `16611a8ee0b4` | 2026-10-14 | fix(openai): isolate Codex imports by workspace |
| `373d43533a0f` | 2026-10-15 | feat(antigravity): support Gemini 4 Argon and accurate upstream model catalogs |
| `2a8cb088af20` | 2026-10-15 | feat(apicompat): configure minimum output token floor |
| `feab6d80dc09` | 2026-10-15 | fix(antigravity): preserve tool_result images, native Gemini functionCall/thoughtSignature, and composite /v1beta routing |
| `92c9433d40db` | 2026-10-15 | chore: update sponsors |
| `1774b79dcbf9` | 2026-10-14 | fix(zhipu): 修正 GLM Coding Plan 5h/weekly 用量窗口标反 |
| `d19265ddb218` | 2026-10-15 | fix(gateway): 拒绝含 OpenAI 网关 OAuth 的 composite 歧义路由，防止凭据误入 Anthropic 管线 |
| `3856a02856ca` | 2026-10-15 | Merge pull request #8106 from Wei-Shaw/fix/composite-oauth-ambiguous-misroute |
| `fdc7f1fa011a` | 2026-10-15 | Merge pull request #8087 from coder-zhw/fix/zhipu-credit-limit-window-classification |
| `3b83e045ed5d` | 2026-10-15 | Merge pull request #8060 from feeeei/fix/gemini-vertex-shared-capacity-429 |
| `abdc82725f1c` | 2026-10-15 | Merge pull request #8076 from feeeei/fix/gemini-vertex-sa-and-error-policy |
| `dcde68aa5881` | 2026-10-15 | Merge pull request #8078 from feeeei/feat/gemini-4-argon-pricing |
| `8afd2abd979f` | 2026-10-15 | Merge remote-tracking branch 'origin/main' into contrib/gemini-4-argon-support |
| `9d3cc4507ff7` | 2026-10-15 | fix(antigravity): keep legacy default passthroughs and bill Claude 5.5 tier IDs at base rates |
| `857a76d4a0e7` | 2026-10-15 | Merge pull request #8093 from baryon/contrib/gemini-4-argon-support |
| `64211de8defd` | 2026-10-15 | feat(gemini): 兼容入口转发思考档位并流式返回思考摘要 |
| `58b5e858c82d` | 2026-10-15 | Merge pull request #8094 from wucm667/feat/issue-8092-configurable-min-output |
| `ddef05527eca` | 2026-10-15 | Merge pull request #8079 from wucm667/fix/issue-8074-anthropic-base-url |
| `698d142468ab` | 2026-10-15 | Merge pull request #8088 from wucm667/fix/issue-8077-workspace-aware-import |
| `c2cca510f2dc` | 2026-10-15 | revert(accounts): restore inline account filters (#7853) |
| `26aab6dc0760` | 2026-10-15 | fix(openai): respect GPT-5.5 retirement on ChatGPT upstreams |
| `1b2243a4f506` | 2026-10-15 | Merge pull request #8120 from Wei-Shaw/revert/inline-account-filters |
| `7c6f70680d76` | 2026-10-15 | fix(payment): validate order types and keep refund deductions while pending |
| `f996b0bfaae1` | 2026-10-15 | fix(payment): load Stripe client secret from the local order snapshot |
| `44e35fbf8440` | 2026-10-15 | fix(payment): finalize failed pending refunds atomically |
| `83337a0c6547` | 2026-10-15 | fix(payment): query Alipay refund status and close unsupported order types |
| `f37b69b77934` | 2026-10-15 | fix(payment): treat Alipay refund query errors as pending and delay not-found failure |
| `86f844304591` | 2026-10-15 | fix(openai): evaluate retirement after public alias projection |
| `52b3ca960960` | 2026-10-16 | Merge pull request #8122 from Wei-Shaw/fix/payment-order-type-refund-hold |
| `1b92038eaf60` | 2026-10-16 | fix(payment): fix EasyPay order queries and confirm notifications before crediting |
| `b8cfb7884084` | 2026-10-16 | Merge pull request #8133 from Wei-Shaw/fix/easypay-query-confirm |
| `a431e53f9e3d` | 2026-10-16 | Merge remote-tracking branch 'origin/main' into pr-8114 |
| `455d317d6a87` | 2026-10-16 | Merge pull request #8114 from feeeei/feat/gemini-compat-thinking |
| `bdfe215cbb06` | 2026-10-16 | fix(responses): 网关送出的 Responses 对象一律带 created_at，修复 grok-build 反序列化失败 |
| `f2e5b55f3e80` | 2026-10-16 | test(responses): 模型改写转发测试夹具补齐上游 created_at |
| `71e6b6a97170` | 2026-10-16 | test(responses): 模型改写转发测试夹具补齐上游 created_at |
| `43157c478ec2` | 2026-10-16 | Merge pull request #8121 from Larry-Labs/fix/gpt55-subscription-retirement |
| `b14f316ca2de` | 2026-10-16 | Merge pull request #8080 from SipengXie2024/fix/openai-messages-web-search-emulation |
| `060d01604177` | 2026-10-16 | Merge pull request #8140 from Wei-Shaw/fix/issue-8118-responses-created-at |
