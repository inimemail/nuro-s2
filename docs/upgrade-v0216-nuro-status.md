# v0.2.16 本地适配实施记录

状态：已确认方案的代码适配和本机回归已完成。尚未提交、推送或部署；真实供应商、压力对照、生产灰度与浏览器视觉验收仍未完成。

基线：`315a574c7c87eb11dc79504cef50cdb92396d553`。手工适配上游 v0.2.16 的选定行为，没有 merge、rebase、cherry-pick 或整文件替换。全量审查完成后，按用户要求将后端版本更新为 `v0.2.16-nuro.1`，前端包版本同步为 `0.2.16-nuro.1`；尚未发布或部署。

## 实施范围

| 编号 | 已完成内容 |
| --- | --- |
| U01 | Codex image bridge 增加 OpenAI 平台限制，保留账号、渠道、全局优先级 |
| U02 | 非冻结错误分支检查完整读取、超限和错误结构；保留原执行状态与重试合同 |
| U03 | 模型级临时不可调度、实际映射模型传播；旧规则仍按账号级，缺模型或写失败不扩大为账号暂停 |
| U04 | Haiku 5.5 模型、能力、请求参数与定价适配，保留本地工具和 thinking 语义 |
| U05 | GPT-6.1 Sol 元数据和官方 OAuth 类长上下文计费适配，保留自定义价卡和倍率 |
| U06 | Gemini 缓存不存在、过期及不可访问错误归因，避免误惩罚账号和跨账号重试 |
| U07 | Vertex API Key / Service Account 认证和模型列表；缓存限定 AI Studio API Key、Vertex Service Account 及明确声明能力的第三方账号 |
| U08 | Gemini AI Studio、Vertex 和第三方配额隔离，AQ.Ab 密钥脱敏 |
| U09 | Gemini 工具结果图片保留、类型和体积校验 |
| U10 | Kimi 官方 coding 地址严格匹配；Antigravity 图片和文本限流隔离 |
| U11 | 分组删除与内容审计配置在事务内同步、统一锁顺序、提交后刷新配置 |
| U12 | 显式缓存创建、分页列表、读取、续期、删除、countTokens、报价、持久化金额预留、结算、Recovery、权限传播、固定账号引用、用户页面和管理员对账页面 |
| U13 | 分组名称和描述多语言、响应副本、本地 Ent 生成、定向语言刷新 |
| U14 | 支付绝对倒计时、后台恢复、IME 输入、运维时区、价格有限值、监控有界刷新；前端沿用现有组件、暗色和响应式样式 |
| U15 | 现有运维分页响应补充账号名称和异步保护 |
| U16 | 本地 Ent / Wire 生成与测试同步，保留依赖和二开结构；审查后同步更新二开版本号 |
| U17 | Gemini 3.6 Flash 按同笔结算时间及供应商资格计算优惠和到期价，自定义售价与统计价优先，不修改共享价格对象 |

明确保留原方案取舍：不新增 structured-input → raw 回退链，不引入上游 Monitor V2、全量账号名缓存和中间依赖升级，不覆盖本地 RPM、CN、监控和 Rust 数据面。

## Gemini 缓存运行方式

- `gateway.gemini_explicit_cache_enabled` 默认 `false`。未启用且没有历史资源或未结操作时不启动资源视图与 Recovery worker，普通流量保持现有入口。
- `gateway.gemini_explicit_cache_maintenance` 默认 `false`。维护期间禁止创建和续期，保留读取、删除、Recovery、对账与已有引用；关闭功能后，存在历史状态仍保留这些清理和恢复能力。
- 默认资源视图上限 10000，单用户最多 1000；TTL 最大 86400 秒，第三方账号可声明更低上限。第三方缓存接口能力和存储单价必须明确配置；Vertex API Key 不开放显式缓存。
- 用户页面使用登录身份选择 Key ID，服务器加载凭据；浏览器不接收明文 Key。创建和续期先报价，显示预留、已结算和分项金额；异常结果保存操作 ID，刷新后可查询结果。取消未登记操作有持久化防延迟执行记录。
- 原生 Gemini 本地缓存引用使用有界本地视图、当前权限和固定账号；视图未就绪或过期返回安全错误。缓存引用不新增同步资源数据库查询，也不换账号或重建 sticky。
- 已发送但结果未知的创建、续期和删除不盲目重放、不自动退款。Recovery 只核验供应商状态和收敛账务；证据不足转对账。管理员重查、补足已确认差额有权限校验及审计记录。
- 权限传播只等待实际授权事件；普通资源事件不拖延管理响应。账号调度修改等待资源视图屏障，不附加 API Key L1 TTL 等待；用户、Key 和分组撤权保留认证缓存安全窗口。

## 最后审查修复

1. 修复普通 Gemini 3.6 旧计费入口忽略结算时间和供应商资格，默认账号统计成本使用同一时间快照。
2. Vertex 创建与计数使用模型对应区域，读取、更新、删除使用资源保存的区域并核验 project；Recovery 使用相同绑定。
3. 补齐调度开关、批量修改/删除、刷新及管理员 Key 修改的授权屏障覆盖。
4. 免费缓存操作发送前再次核验用户、Key、分组和账号关系；禁用、撤权、过期创建被拦截，额度耗尽和过期 Key 仍可免费删除。
5. 金额预留、结算、退款使用持久化 generation；迟到回填不能降低同周期用量，不能把新周期回退成旧周期，旧周期用量也不会并入新周期。
6. 认证缓存空值按 miss 处理，设置本地时间戳前复制条目，避免修改缓存持有的共享对象。
7. 取消操作记录保持持久防重放，限制每 Key 1000、每用户 10000；仅清理已物理删除 owner 的旧记录。
8. 保留旧渠道负价格错误文案，单独拒绝 NaN / Infinity，避免改变现有错误处理合同。
9. 缓存资源读库故障返回安全 503，资源不存在、非法请求、操作冲突和供应商明确拒绝分别分类；不泄露数据库或上游错误详情。
10. 已登记的创建/续期幂等操作先读取历史结果，维护模式、功能关闭和 TTL 变化不会触发供应商重复执行。
11. 删除及 Recovery 仅接受结构化 `NOT_FOUND` 作为不存在证据，普通 404 或代理 HTML 不会误判为删除成功。
12. Vertex 创建保存原 project/location；后续账号区域调整后，读取、续期、删除、恢复和引用仍使用原资源区域。引用复制账号凭据，不修改共享账号对象。
13. 严格校验供应商返回的资源名称、模型和 token 数；错误绑定进入受控对账，不能激活或用于引用。
14. 用户管理写接口限制 8 MiB 原始请求体，管理员对账限制 16 KiB，包括未知长度和 chunked 请求。
15. 前端未知操作重试复用原 pending 记录、创建时间和幂等 ID，避免失败后生成不同记录或丢失原操作身份。
16. 缓存页面按操作 ID 核验跨标签页迟到查询、取消和提交结果，旧操作不能清除新 pending；支付取消按订单会话核验，旧响应不能取消新页面状态。
17. 免费创建/续期发送前再次检查订阅 owner、分组、状态、开始和过期时间；删除保留原免费清理能力。
18. 订阅撤销、恢复、开始/过期时间调整写入授权事件并经过视图屏障；资源权限投影核验有效订阅，保持引用热路径无新增同步数据库读取。
19. Key/订阅额度重置推进财务 generation，认证缓存的新快照同步推进 Redis fence，阻止旧代快照覆盖重置结果。
20. 平台额度只在管理员强制重置入口推进 fence；移除通用下降触发器，避免正常跨周期回写被误判后使 flusher 永久停留在旧代。预留和独立缓存结算用量不被清零。
21. 平台额度单条读取适配器补齐缓存日/周/月已结算金额和窗口起点；Redis miss 或故障时从数据库读取，额度检查与回填都保留这些金额，避免少算存储费用。真实 PostgreSQL 回归核验生产适配器读取后的三窗口金额与起点。

22. Key/订阅额度重置失效改为读取提交后的财务代次，Redis 原子推进 fence 并删除旧 hash。先复现了“失效后旧代回填、新代同秒重置仍保留旧用量”的问题；最终采用专门的重置失效操作，保留正常财务代次更新对并发普通用量的最大值保护。回归覆盖旧回填被拦截、新代重置成功、同代/跨财务代次并发用量不被降低、迟到失效不能删除更新代缓存，以及生产服务调用的代次传播。未使用缓存的 generation=0 不新增持久 fence。
23. 资源视图在后台读库时查询现存缓存 owner 的订阅下一次自然开始/到期边界，将本地有效期限制到该时刻；越过边界即使没有 revision 事件也重新 bootstrap。空增量不能延长旧授权；普通资源增量引入 owner 时也重算边界。引用仍只读本地视图，不新增同步数据库查询。增加自然到期撤权、自然开始恢复、增量新资源边界及真实数据库时间边界断言。

## 数据库及发布边界

新增迁移 `244–254`：分组多语言、缓存资源和操作/账务/视图表、预留额度与 generation、授权事件、存储价卡、统计与维护、授权索引、取消操作记录与索引、订阅授权及重置保护。已在隔离 PostgreSQL 中验证完整迁移和再次 Apply；后续修正另写迁移，没有修改已在测试库应用的迁移 checksum。

启用前应备份数据库并验证目标供应商的 CRUD、计数、模型区域和实际账单。回滚首先关闭创建/续期并保留新版 Recovery，待未结操作完成后再评估降级；不能删除预留记录、直接退款未知执行或清空账务表。

## 二开保护核对

20 个冻结文件以方案开始时 SHA256 校验，保持一致。冻结范围涵盖 ABCD 缓存优化、真实 usage 展示、raw 转发、强隔离、Edge ingress/resume/settlement/SSE、补帧、HTTP 配置和 scheduler snapshot 等。

第二轮续审时原临时 SHA256 清单已不存在；根据留存的 20 个冻结路径逐一计算当前内容与基线 `315a574c7c87eb11dc79504cef50cdb92396d553` 内容 SHA256，全部一致。新证据为 `/private/tmp/nuro-v0216-second-review-frozen.log`，没有重新生成清单冒充方案开始时的校验。

ABCD / 24h / 固定点缓存加成、内部真实 usage、未知执行不重放、scheduler / sticky / 倍率 guard、Cell / tenant escrow / lease / WAL、阶段补帧和 Rust Edge 保留原实现，并纳入现有回归。TPS 仍永久显示，继续按输出 token / 总耗时计算，媒体排除规则保留。私有部署菜单 2、本地构建、随机邮箱、`.env` 初始管理员密码未修改。

## 验证证据

| 检查 | 结果 / 日志 |
| --- | --- |
| 后端 `go test ./...` | 通过；`/private/tmp/nuro-v0216-go-final.log` |
| 后端 `go test -tags=unit ./...` | 最后补丁后全量通过；`/private/tmp/nuro-v0216-go-unit-final.log` |
| 后端 `go vet ./...` | 通过；`/private/tmp/nuro-v0216-vet-final.log` |
| 服务、仓储、接口 `go test -race` | 最后补丁后通过；`/private/tmp/nuro-v0216-race-final.log` |
| PostgreSQL 真实事务 | 通过；余额/订阅预留、幂等结算、补差额、未知执行不退款、撤权、取消记录容量和维护、迁移二次 Apply；`/private/tmp/nuro-v0216-pg-final.log` |
| 真实数据库价卡读写 | 通过；普通价卡及账号统计价卡的存储价 null / 0 / 正数与零输入价分别保存读取 |
| 前端 typecheck / lint | 通过；`/private/tmp/nuro-v0216-typecheck-final.log`、`/private/tmp/nuro-v0216-lint-final.log` |
| 前端 Vitest | 187 文件、1160 测试通过；`/private/tmp/nuro-v0216-frontend-test-final.log` |
| 前端生产 build | 通过；`/private/tmp/nuro-v0216-frontend-build-final.log` |
| Rust `cargo test --locked` | 227 测试通过；`/private/tmp/nuro-v0216-rust-test-final.log` |
| Go 嵌入前端发布构建 / 资源测试 | 通过，构建使用 `CGO_ENABLED=0`、`-tags embed`、`-trimpath`；`/private/tmp/nuro-v0216-backend-build-final.log`、`/private/tmp/nuro-v0216-embed-final.log` |
| 冻结文件 / 差异格式 | 20 个 SHA256 全部一致，`git diff --check` 通过 |

所有数据库测试使用本机独立 `nuro_upgrade_verify` 数据库；没有连接生产。Redis 脚本测试使用隔离测试实例，没有清空任何实际环境。临时页面预览文件已清理、预览服务器已停止。

2026-10-14 续审：上述第 9–15 项补丁后重新执行全量 Go / unit / vet / race、真实 PostgreSQL 事务及迁移重复 Apply、前端 typecheck / lint / 全量 Vitest / build 和 Go embed 构建及资源测试，均通过。最后新增的管理员请求体限制另行通过完整 handler 测试、handler race 与 vet；日志为 `/private/tmp/nuro-v0216-review-handler.log`、`/private/tmp/nuro-v0216-review-handler-race.log`、`/private/tmp/nuro-v0216-review-handler-vet.log`。`bash -n deploy-nuro.sh` 和首次部署凭据回归通过；`/private/tmp/nuro-v0216-deploy-bootstrap-final.log`。冻结文件再次校验全部一致，依赖锁文件和部署脚本没有改动。当前 checkout 没有 Makefile 所引用的独立 `datamanagement/` 模块，不能将其独立测试记为通过。

`go test -tags=integration ./...` 退出成功，但 Docker daemon 没有运行，仓储 TestMain 跳过了容器集成测试，不能将其视为完整集成通过；`/private/tmp/nuro-v0216-go-integration-final.log`、`/private/tmp/nuro-v0216-integration-repo-detail.log`。这与已实际执行的独立 PostgreSQL 缓存事务测试分别记录。普通无引用请求的字段检测微基准为约 `37.53 ns/op`、`0 B/op`、`0 allocs/op`；`/private/tmp/nuro-v0216-reference-bench.log`。该微基准仅测字段检测，不等同于全请求差分压测或首 Token 性能验收。

2026-10-14 第二轮扩大范围审查：第 16–20 项修复后，全量 Go / unit / vet 及服务、仓储、接口 race 已通过。第 21 项最后适配器补丁后另行执行仓储 unit / vet / race、真实 PostgreSQL 事务与 Redis 脚本回归、Go embed 发布构建，均通过。前端最后补丁后为 187 文件、1162 测试通过，typecheck / lint / build 通过。数据库回归增加了未来生效订阅撤权、正常跨周期快照持续回写、管理员 reset 拦截旧代与接受新代、未使用缓存的 generation=0 保持原行为，以及缓存已结算金额读取断言。

| 第二轮检查 | 日志 |
| --- | --- |
| 全量后端 Go / unit / vet | `/private/tmp/nuro-v0216-second-review-go.log`、`/private/tmp/nuro-v0216-second-review-unit.log`、`/private/tmp/nuro-v0216-second-review-vet.log` |
| 服务、仓储、接口 race | `/private/tmp/nuro-v0216-second-review-race.log` |
| 最后适配器补丁的仓储 unit / vet / race | `/private/tmp/nuro-v0216-second-review-adapter-unit.log`、`/private/tmp/nuro-v0216-second-review-adapter-vet.log`、`/private/tmp/nuro-v0216-second-review-adapter-race.log` |
| 真实 PostgreSQL / Redis 脚本回归 | `/private/tmp/nuro-v0216-second-review-pg.log` |
| 前端 typecheck / lint / Vitest / build | `/private/tmp/nuro-v0216-second-review-typecheck.log`、`/private/tmp/nuro-v0216-second-review-lint.log`、`/private/tmp/nuro-v0216-second-review-frontend-test.log`、`/private/tmp/nuro-v0216-second-review-frontend-build.log` |
| 最后补丁的 Go embed 发布构建 / 资源测试 | `/private/tmp/nuro-v0216-second-review-backend-build.log`、`/private/tmp/nuro-v0216-second-review-embed.log` |
| 部署脚本语法 / 首次部署凭据 | `/private/tmp/nuro-v0216-second-review-deploy.log` |

20 个冻结文件与基线 SHA256 再次一致。版本、依赖锁文件、部署脚本与 Rust 源码保持本次适配前内容；Rust 沿用上表既有 227 项通过记录，没有因本轮 Go / Vue 修复重复运行。

## 第三轮全文件审查（2026-10-14）

本轮覆盖原清单 221 个文件及新增的服务代次回归文件，共 222 个改动/新增文件（手写后端与迁移/测试 139、Ent 生成代码 27、前端 53、配置/实施文档 3）。逐文件清单和检查方式见 [审查范围](upgrade-v0216-review-files.md)；审查清单自身另计为文档。重点核验共享缓存与充值/重置/正常结算的交互、自然订阅授权、调度、固定账号引用、Recovery、前端异步响应和取消、默认关闭分支。

第 22–23 项最终生产代码补丁后，全量 Go / unit / vet、服务/仓储/接口 race、前端 typecheck / lint / 187 文件 1162 测试 / build、Go embed 发布构建和资源测试均通过。随后仅补充真实 PostgreSQL 增量资源边界断言，单独运行最终仓储测试二进制复核迁移重复 Apply、预留/结算/撤权和 Redis 脚本。最初编译检查发现新增 `errors` 导入遗漏，已修正，并以修正后的全量测试结果为准。

| 第三轮检查 | 日志 |
| --- | --- |
| 全量后端 Go / unit / vet | `/private/tmp/nuro-v0216-third-review-go.log`、`/private/tmp/nuro-v0216-third-review-unit.log`、`/private/tmp/nuro-v0216-third-review-vet.log` |
| 服务、仓储、接口 race | `/private/tmp/nuro-v0216-third-review-race.log` |
| 最后新增数据库断言与 Redis 回归 | `/private/tmp/nuro-v0216-third-review-pg-final.log` |
| 前端 typecheck / lint / Vitest / build | `/private/tmp/nuro-v0216-third-review-typecheck.log`、`/private/tmp/nuro-v0216-third-review-lint.log`、`/private/tmp/nuro-v0216-third-review-frontend-test.log`、`/private/tmp/nuro-v0216-third-review-frontend-build.log` |
| Go embed 发布构建 / 资源测试 | `/private/tmp/nuro-v0216-third-review-backend-build.log`、`/private/tmp/nuro-v0216-third-review-embed.log` |
| 部署脚本语法 / 首次部署凭据 | `/private/tmp/nuro-v0216-third-review-deploy.log` |
| Ent 重新生成 / 对照 | `/private/tmp/nuro-v0216-third-review-ent.log`、`/private/tmp/nuro-v0216-third-review-ent-compare.log` |
| 20 冻结文件与版本/锁/部署/Rust | `/private/tmp/nuro-v0216-third-review-frozen.log` |

Ent 使用项目原有版本重新生成，并在隔离目录对照。Go 1.27 将 `encoding/json.RawMessage` 展开为其别名 `encoding/json/jsontext.Value`，只涉及 mutation 和原有 usage cleanup 的 4 个文件；归一这项类型别名写法后无未解释差异。实际工作区保留原有 RawMessage 内容，未引入相关文件变化或依赖升级。Wire 注入/清理变更和已有生成回归随全量 Go 核验。

20 个受保护文件与基线 SHA256 再次全部一致；版本、依赖锁、部署脚本和 Rust 源码未改。TPS 永久显示、随机邮箱及 `.env` 初始管理员密码保留。没有提交、推送或部署，没有运行生产数据库迁移。

## 审查后版本同步

按用户要求将 `backend/cmd/server/VERSION` 更新为 `v0.2.16-nuro.1`，`frontend/package.json` 更新为 `0.2.16-nuro.1`。上文“版本未改”描述的是版本同步前的审查快照。同步后重新执行 Go embed 发布构建，运行二进制 `-version` 确认输出 `Sub2API v0.2.16-nuro.1`；前后端版本一致性及 `git diff --check` 通过。没有更改依赖锁、功能代码或部署脚本，也未创建发布标签。

## 尚未验证

真实 AI Studio / Vertex / 第三方账号调用及供应商账单、生产多副本故障演练、真实 Redis Cluster、差分压力测试和 Shadow / Canary 尚未执行，不能据此宣称零风险或生产验收完成。新增页面已通过组件测试、类型检查和构建，但没有完成浏览器截图及手机/桌面视觉验收。`golangci-lint` 未安装，没有将其记为通过。

## OpenAI 全文本模型缓存创建优化（2026-10-15）

本节是独立于上述历史升级验收的新增改动记录；上文全量后端、数据库及冻结文件校验结果不能作为本轮新代码的验收证据。

### 实施范围

- 保留账号级开关及 A/B/C/D 存储值，将 OpenAI OAuth / API Key 文本账号的创建优化扩展到全部文本模型和自定义别名；继续排除媒体模型、图片意图、图片池及 shadow。关闭时沿用原请求和响应分支。
- D 前端名称为“创建成本分摊”，按当前模型、分组/渠道、生效计费模型、服务档位、价格区间和长上下文开关分摊创建费用。下游分摊至普通输入、少量输出和缓存读取，内部账务及审计保留原始 usage；既有缓存加成仍以原始输入/缓存读取为基础继续执行。
- 无可靠创建价、独立 5m/1h 价格无法确定、非法价格/数值溢出或转换会跨价格档位时，D 保留原始 usage。普通分摊最多有半个普通输入 token 的金额舍入差异；不承诺下游使用不同价卡时账单仍完全一致。
- 同步 HTTP Responses、raw Chat、协议转换/fallback、Go WS 与 Rust Edge 路径。WS 按轮次识别模型、服务档位和图片意图；最终扩大审查后，D 的 WebSocket 固定回退现有 Go 逐轮结算通道，HTTP D 仍可使用 Edge，详见末节。
- 上游不支持策略时按“账号 + 上游模型 + 协议”退避 10 分钟，避免某模型拒绝影响其他模型；本地状态最多 4096 条并合并并发 Redis 轮询。
- 新 Edge 声明创建策略能力版本 1；Go 遇到不具备能力的旧 Edge，开启优化的账号在转发前回退 Go，关闭优化的账号保留原 Edge 分支。Go / Rust 应同步更新以使用新版 Edge 能力，全局控制协议版本未变。
- 前端中英文说明、D 定价回退说明、A/B/C/D 标识及卡片行距同步更新，保留现有双列布局和暗色样式。

### 本轮检查

| 检查 | 结果 |
| --- | --- |
| 相关 Go 扩大回归 | 138 个顶层测试、含子测试共 271 项通过；`/private/tmp/nuro-cache-creation-go-tests.jsonl` |
| Go 新增缓存创建逻辑 race | `go test -race ./internal/service -run '^TestOpenAICacheCreation' -count=1` 通过 |
| Go 协议兼容与 WS 核心包 | `go test ./internal/pkg/apicompat ./internal/service/openai_ws_v2 -count=1` 通过 |
| Go vet / 服务构建 | service、handler、apicompat 的 vet 通过，服务构建通过 |
| 最终发布构建 / 编译 | 最新共享夹具 Go 回归、service / handler 的 unit 标签编译及 `CGO_ENABLED=0 go build -tags embed -trimpath` 通过，已嵌入本轮前端生产构建 |
| Rust | 修正共享区间测试夹具缺失的零值 rates / priority 字段后，229 项测试及 `cargo check --offline` 通过；没有放宽断言 |
| 前端类型 / 相关文件 lint | vue-tsc 及本轮相关文件 ESLint 通过 |
| 前端完整 Vitest | 188 个文件、1164 项测试通过；`/private/tmp/nuro-cache-creation-frontend-tests.log` |
| 前端生产构建 | 通过；`/private/tmp/nuro-cache-creation-frontend-build.log` |
| Go / Rust 同源定价夹具 | 涵盖标准/priority、长上下文开关、区间跨档与溢出回退 |
| 差异与范围 | `git diff --check` 通过；下述账务/部署/依赖/版本路径已与 HEAD 对照确认未改 |

本轮未改账务服务、模型价格解析器、账号设置实现、部署脚本、依赖锁、数据库结构或版本号；部分缓存与 Edge 文件属于本轮授权修改范围，不沿用历史“20 个冻结文件全未改”的结论。调度、强隔离、TPS 永久显示、Cell / tenant escrow / lease / WAL 及首次部署凭据未纳入功能改写范围。

本轮沙箱禁止监听本机端口，Redis / 本机网络集成测试未完成；提权请求因自动审批流连接断开未执行，没有绕过审批，也没有将这类测试记为通过。未完成真实供应商账单、生产多副本/负载及浏览器截图视觉验收，不能承诺零风险。没有提交、推送或部署，版本仍为 `v0.2.16-nuro.1` / `0.2.16-nuro.1`。

### 全文件续审与修复（2026-10-15）

本轮重新核对上述 34 个改动/新增文件，重点审查 D 定价与真实账务的一致性、关闭开关的旁路、模型映射、协议转换、并发退避和混合版本 Edge。发现并修复以下边界：

1. Go HTTP / 协议桥接上游省略 `service_tier` 时，D 可能退回普通价格。现在仅 D 在响应处理上下文中携带本次请求的规范化服务档位；空的响应档位不会抹掉该回退值，后续无档位请求会显式清空旧值。开启原缓存加成时也保留这份元数据。补充 raw Chat、Responses→Chat 的流式/非流式完整转发处理链回归，使用内存上游替身，不监听端口；核验 priority 分摊和内部真实 usage。
2. 渠道指定按请求别名计价时，OAuth GPT-6.1 的长上下文豁免必须由实际上游模型决定。现在复用真实账务使用的模型识别函数，分别传递计价模型与上游模型；回归同时核验 OAuth 豁免和 API Key 保持长上下文计价。
3. `channel_mapped` 在未发生映射或仅映射为原名称时，真实账务继续使用上游模型。D 已同步这一条件，避免账号还有后续模型映射时错误选择请求别名的价卡；新增无映射、同名映射用例。
4. DeepSeek / Gemini 3.6 目录价格可能在实际账务阶段按时段/优惠调整，不能把会话开始时的价格当成整个 WS 会话的固定价格。此类未完整覆盖的目录价格下 D 保留原始 usage；明确的分组/渠道价格或完整显式价格仍可分摊。A/B/C 不受这一价格回退影响，中英文说明同步提示动态时段定价边界。

最终补丁后的核验结果：

| 检查 | 结果 / 证据 |
| --- | --- |
| 后端相关回归及二开保护回归（unit 标签） | 179 个顶层测试、含子测试 343 项全部通过，无缺失；`/private/tmp/nuro-cache-creation-review-final-unit-tests.jsonl` |
| 保护项专门回归 | 37 个顶层测试、含子测试 60 项通过，涵盖调度快照/取消守卫、compact/WS 调度、强隔离、会话隔离、原缓存加成及 GPT-6；`/private/tmp/nuro-cache-creation-review-protected-unit-tests.jsonl` |
| 新增缓存创建逻辑 race | 最新补丁后 `go test -race ./internal/service -run '^TestOpenAICacheCreation' -count=1` 通过 |
| 协议与 WS 核心包 | apicompat、openai_ws_v2 整包测试通过 |
| vet / 发布构建 | service、handler、apicompat 的 vet，以及嵌入最新前端产物的 `CGO_ENABLED=0 go build -tags embed -trimpath` 通过 |
| 前端 | 全量 188 文件、1164 测试通过，vue-tsc、本轮相关文件 ESLint、生产构建通过；`/private/tmp/nuro-cache-creation-review-frontend-tests.log`、`/private/tmp/nuro-cache-creation-review-frontend-build.log` |
| Rust | 229 项测试及 `cargo check --offline` 再次通过 |
| 范围核对 | `git diff --check` 通过；账务服务、模型价格解析器、账号设置实现、部署脚本、依赖及版本与 HEAD 对照无改动 |

本节保护项测试包含在最终 179 项中，不能重复相加。仍未执行真实 Redis/本机端口集成、真实供应商账单与生产负载验证，也未完成浏览器截图视觉验收；前述沙箱及审批流限制未解除。本轮没有提交、推送或部署。

### 最后一次扩大审查（2026-10-15）

继续审查服务档位、WebSocket 换模型/重试、Edge 会话汇总结算与前端说明，修复以下边界。本节描述最终实现，优先于前面各阶段的审查快照。

1. D 分摊现在优先采用实际本地结算使用的请求服务档位，上游响应不能通过返回其他档位改变分摊价。没有请求档位时显式记录 `default`，不再将“已知普通档”与“缺少请求元数据”混用。raw Chat / Responses 的 fast-mode 过滤即使移除上游请求字段，也继续与现有结算档位一致；OAuth 使用其现有结算路径采用的最终请求档位。JSON 与 typed usage 均已覆盖，内部原始 usage 不变。
2. Rust 的 `session.update` 只更新后续请求的默认模型，不再污染仍在输出的当前轮模型及缓存加成策略。`response.create` 开始时快照内部重试请求的有效模型，避免随后的 `session.update` 将退避范围错误记入另一模型；真实转发帧不受快照补字段影响。
3. Edge WebSocket 以初始 lease 的模型/档位汇总整个会话，D 逐轮转换在换模型、换档位或跨上下文区间时无法保证与该汇总结算口径一致。最终选择在创建上游前将开启 D 的 WebSocket 路由到既有 Go 逐轮结算通道，保留原账务、lease 与 WAL；A/B/C、关闭优化的 WebSocket 及 HTTP D 继续沿用原 Edge 能力。新增 OAuth / API Key、文本/别名/非文本首轮及开关模式组合回归，前端中英文同步说明此路由行为。移除不再使用的 WS 全价卡目录扫描。

最终代码补丁后的核验：

| 检查 | 结果 / 证据 |
| --- | --- |
| 扩大后端 unit 回归 | 220 个顶层测试、含子测试 419 项全部通过，无缺失或失败；包含前轮二开保护用例，不能重复相加；`/private/tmp/nuro-cache-creation-last-review-unit-tests.jsonl` |
| 新增缓存创建逻辑 race | `go test -race ./internal/service -run '^TestOpenAICacheCreation' -count=1` 通过 |
| 协议 / WS 核心包与 vet | apicompat、openai_ws_v2 整包测试通过；service、handler、apicompat 的 vet 通过 |
| Rust | 233 项测试与 `cargo check --offline --quiet` 通过；`/private/tmp/nuro-cache-creation-last-review-rust-tests.log`。部分端口用例受沙箱限制在用例内跳过，不能视作真实网络集成通过 |
| 前端 | 本轮控件 1 文件 2 测试、vue-tsc、相关文件 ESLint 及生产构建通过；`/private/tmp/nuro-cache-creation-last-review-frontend-build.log`。完整 188 文件 / 1164 项通过是前一轮记录，本轮没有重跑全量 |
| 发布构建 | 嵌入本轮最新前端产物的 `CGO_ENABLED=0 go build -tags embed -trimpath` 通过，产物位于 `/private/tmp/nuro-s2-cache-creation-review-final-embed` |
| 范围 | 账务服务、模型价格解析器、账号设置实现、部署脚本 / Dockerfile、Go / 前端依赖及版本与 HEAD 对照未改；`git diff --check` 通过 |

补充整包执行 `go test ./internal/service ./internal/handler ./internal/pkg/apicompat ./internal/service/openai_ws_v2 -count=1` 时，service 的 `TestQueryOpenAIWhamUsagePassesThroughInvites` 与 handler 的 `TestDingTalkClient_ExchangeCodeForUserToken_Success` 因沙箱 `bind: operation not permitted` 中止，因此未将 service / handler 整包记为通过；后两个协议包实际通过。未申请绕过端口限制。

`cargo fmt --check` 对 HEAD 已存在 130 处格式差异；本轮新增的 22 处差异已按 rustfmt 整理，重新比较后，与本次修改行重叠的格式差异为 0，剩余 130 处旧差异保留，未将全仓格式检查记为通过。整理后重新执行 Rust 233 项测试及编译检查。

真实供应商调用/账单、Redis / 端口集成、生产负载及浏览器截图仍未完成，本轮结果不能替代生产验收。没有提交、推送或部署，前后端版本保持 `v0.2.16-nuro.1` / `0.2.16-nuro.1`。
