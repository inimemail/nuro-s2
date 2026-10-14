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
