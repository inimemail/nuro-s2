# scoped 管理员密钥：纳入实施的独立阶段

> 本文保留实施前的设计与取证快照；其中“尚未实施/拟新增/当前版本”描述对应方案编写时点。2026-10-29 的实际改动、验证结果及尚未通过的发布门禁以 [v0219-status.md](v0219-status.md) 为准。

状态：方案，尚未写入项目。当前用户已要求将建议项纳入；本项从上一版的延期调整为纳入完整实施计划，不代表允许削弱已有TOTP、JWT、管理API、缓存权限屏障或保护字段。

## 1. 实际基线与必须解决的差异

- 本地`backend/internal/server/middleware/admin_auth.go/adminAuthWithSecurity`支持legacy x-api-key及管理员JWT，含管理WS subprotocol、session binding和审计。有效legacy key维持现有校验，不附加scoped仓库查询；JWT不走新key查询。实现时先无副作用地完成原已保存legacy值的常量时间匹配，再在gate开启且未匹配时进入scoped校验；不能按新prefix先分流误拒合法旧值，也不能先调用会Abort的旧校验再尝试scoped。保持原WS/JWT/header优先级与错误码；gate关闭的未匹配key走原拒绝路径。
- `backend/internal/server/middleware/step_up.go/enforceStepUp`按`auth_method == service.AuditAuthMethodAdminAPIKey`禁止机器密钥通过。scoped必须同样标识为机器密钥，Principal另存context；不能新建一个auth_method字符串躲过这个判断。本轮不修改step_up.go生产逻辑。
- `/admin/accounts/data`是有意不要求个人TOTP的JSON导入；`/import/codex-session`、导出、批量凭证等有独立step-up。保留原路由差异，不按“都是导入”统一增加/移除TOTP。scoped只开放精确catalog登记的路由，不继承JSON导入权限；需要step-up的操作继续仅允许满足原规则的管理员会话。
- `routes/admin.go/RegisterAdminRoutes`已有panel limiter、audit和GeminiCachePermissionBarrier；新受限分支仍在同一公共屏障下，不注册绕行路由。
- 本地`repository/account_repo.go`已有TxFromContext、group/billing原子更新、OnCommit、受管Extra保存和outbox；不复制上游accountRepository整文件，不再造第二个全局事务/失效框架。
- 当前`handler/admin/idempotency_helper.go/adminActorScope`仅按admin userID区分；两个scoped key可属于同一创建者，必须按稳定keyID隔离操作，policy revision只用于当前授权/响应投影。现有ActorScope只参与fingerprint，而仓库唯一键为scope+keyHash；因此scoped必须同时隔离实际存储scope，不能只改ActorScope。
- 本地panel limiter按userID限流且允许ExemptAdmin；scoped不能因带管理员角色而自动豁免，必须有独立key身份分支，legacy/JWT保留原规则。

## 2. 开关、数据与滚动升级

拟新增启动配置`security.scoped_admin_api_keys_enabled: false`，对应现有Viper规则的环境变量`SECURITY_SCOPED_ADMIN_API_KEYS_ENABLED`。这是拟新增名字，不是当前已有配置。控制新生命周期API、受限鉴权分支与UI可见性；关闭时不访问scoped表，不改变legacy/JWT行为，不进入业务生成链。配置关闭还需惰性构造仓库/maintenance，不因表缺失在启动DI/定时任务中报错；新API在执行仓库前拒绝、UI隐藏，不能只在请求鉴权处分支。启用/关闭按Go配置加载机制重启控制面，不改变现有可热更新的header-timeout/等待设置语义。

拟新增`256_scoped_admin_api_keys.sql`，实施时复查编号，排在图片255之后。独立表保留id/name/description/prefix/hash/enabled/expiry/IP policy/权限policy/creator/创建更新/使用/撤销/软删除字段，并增加明确的`policy_revision`用于权限变化、事务重验和响应投影失效；幂等操作身份不随revision变化。只存高熵随机密钥摘要，不存可恢复明文；创建/轮换只展示一次，撤销不可被普通启用操作复活；逻辑删除保留审计。DDL、索引和字段长度参照锁定上游migration247目的，以本地新编号和兼容校验落地，不回写历史SQL。

schema先行、所有Go就绪、开关保持false，然后测试key启用。滚动期间不向旧Go随机发送新key：新key流量绑定具备能力的管理入口，或等所有Go升级后再开启；普通推理请求仍走原Go/Edge链。回滚先关闭gate并撤销测试key，保留兼容表与审计，不drop、不撤销已经成功的业务写入。

## 3. 权限与二开字段保护

- 精确登记`HTTP method + Gin full route`，未知route/permission默认拒绝，目录前缀不授予子路由权限；读权限不隐含写，credentials.write不隐含credentials.read，按分组范围不得拼接不同rule里的平台/账号类型条件。
- account/group SQL范围先过滤再Count/分页；批量操作每个ID都验权，不能以第一个ID结果授权整批。无权限资源统一使用与不存在一致的状态，防止枚举；list/total/error/关联字段均不能侧漏越权对象。
- `name/notes`、model_mapping、状态、调度参数、网络凭证、分组关联分别授权；平台/type创建后不允许通过受限key变更。所有`credentials`按白名单子字段patch，禁止整map替换；未知key、`extra`整对象、null清空保护凭证一律拒绝。省略值不写回零值。
- 缓存ABCD、cache markup三字段、strong isolation、shadow关系、prompt/session/sticky、placeholder stage/template、account retry/race、adaptive/manual multiplier、guard/Cell/lease/escrow和健康runtime字段，默认不向scoped开放写入口。不能拿accounts.credentials或accounts.schedule当成批量改这些二开字段的授权。
- 账号创建使用本地正常校验和默认关闭语义；不能用上游较少的credentials类型或平台列表覆盖本地CN/Composite能力。新key的可授权platform/type取本地真实支持集合、逐项登记；旧账号不因新catalog不认识而被自动修改或删映射。
- 已有敏感step-up路由仍遵守原middleware。上游catalog中的Codex导入/支付/订阅动作若本地要求二次验证，则对机器key显示为不可用并说明原因，不复制上游白名单来撤掉保护；普通JWT/legacy管理功能不变。
- key生命周期仅管理员JWT；写操作还必须通过原step-up，机器key不能给自己新建/扩权/轮换key。原legacy生成/读取/轮换/删除规则保持，不带入上游停用legacy新建及新增legacy禁止访问旧接口的hunk。

## 4. 事务、竞态、外部副作用

受限写调用链：鉴权principal→精确route→字段检查→有界事务→按固定锁序重读key有效性/policy revision及目标account/group→基于同一rule再验权→本地窄字段写/增量关联/outbox→commit→已有定向缓存发布→按当前读权限响应。撤销/改权限使用同一串行化约定，明确定义commit线性化点；不能在初始鉴权后放任长事务使用旧权限。

复用`UpdateAccountWithGroupConfig`、`UpdateAccountWithGroupConfigAndBillingSettings*`与现有clientFromContext/OnCommit，不将上游accountTransactionScope覆盖到每个普通管理操作；新受限仓库可使用事务client副本，但所有ORM/原生SQL/关系读取必须同一tx。分组关联只对授权变化做增删，保留其他分组、关联priority、guard override、created_at。原子保留Grok/倍率探测/placeholder/cache配置快照，不接受调用方伪造受管runtime值。

现有OnCommit已发布的内容只发布一次，不再注册重复hook。失败回滚不得刷新scheduler/prepare/auth snapshot、写健康/冷却或调用真实上游。commit后ctx处理复用本地既有约定并加有界耗时；不能把已commit的Tx context带给后续DB读。OAuth刷新等不可回滚外部动作不能在持有DB锁时执行；用已有可靠管理任务/明确操作记录在提交后有界执行，失败明确反馈“已保存/待执行”，不假装整次回滚、不静默fire-and-forget重放。

凡无法在不改变冻结scheduler/Edge语义的情况下贯通的动作，受限catalog保持禁用；功能开关不放量，保留具体阻塞证据，不能改保护区绕过。

## 5. 幂等、DTO与限流

拟对scoped请求设置稳定`admin-key:<keyID>` ActorScope，并把原操作namespace与keyID组合成独立存储scope；method/fullroute/payload继续进入fingerprint。不能只改ActorScope导致两个key仍争用同一scope+keyHash。legacy/JWT维持原scope与`admin:<userID>`。`policy_revision`用于每次鉴权、事务内重验和响应投影，不进入操作去重身份；同一key改权限或轮换secret后，已完成操作仍被识别，不能因新revision/secret而再扣费、再创建资源。重放前验证当前有效性、路由/资源权限并重新投影；旧投影不得直接复用，权限不足则拒绝或只回当前允许的最小状态。不同key/JWT互不读取结果；新建另一key不是原key自动续接，客户端不能靠换key重试未确认的财务操作。

正常、重放及原有兜底均须投影；scoped非幂等写要求有效Idempotency-Key，coordinator为nil、ObserveOnly、存储不可用均不得降级执行。JWT/legacy现有fail-open集合保持。实际`service/idempotency.go/Execute`是在execute返回后才MarkSucceeded，且handler在coordinator为nil时直接执行，不能宣称单靠现有helper已覆盖“业务提交后进程崩溃”。scoped侧为非幂等写增加同库唯一操作记录，与业务变更同事务提交（拟256中独立`scoped_admin_operations`表或经验证可复用的等价记录）；持久化稳定keyID/操作namespace/幂等key摘要、请求fingerprint、资源ID和最小结果，不持久化完整敏感DTO。唯一约束与有界claim/事务锁保证并发只执行一次；generic成功标记失败/进程重启后，先读业务操作记录确认提交，不能超时reclaim后再次执行。记录保留至少覆盖对外承诺的重试窗口，窗口过期不承诺无限去重，响应与文档必须明确。

commit成功但响应屏障超时/连接断开时，使用同一稳定key与操作记录查询/重放，不重新扣余额、分订阅或重置quota。外部不可回滚动作必须有供应商幂等能力或可靠任务状态；执行结果未知时只查询/人工核对，不自动重试。相关非幂等路由只有完成此原子性适配才加入catalog，不能为凑齐上游功能绕过门禁。

受限DTO明确投影本地新增字段，不先返回通用Account再靠删几个secret字段。账户credential仅授权读到model_mapping；用户关联、分组价卡、上游URL/token、Grok诊断内部信息、订阅关联和usage请求内部细节逐字段控制。即使上游新增字段也默认不可见。

`panel_rate_limit.go/userScoped`只在存在scoped Principal时改用keyID身份，必须应用key规则且不继承管理员豁免；JWT/legacy仍原逻辑。安全审计记录keyID/动作/授权资源/result，绝不记录完整密钥、凭证、上传body或敏感内部错误。IP白名单复用本地`SecurityClientIP`和可信代理配置，不能直接信任任意X-Forwarded-For或重新解释整个站的真实IP规则。

## 6. 可选代理模式并入受限管理阶段

上游新增direct/specified/random/least_used属于管理保存时的选代理能力。纳入完整实施清单，但默认不选、不改现有proxy_id、不改变普通请求代理路由。新模式只允许具备accounts.network权限且账号/代理范围均授权的管理操作；省略模式完全保留当前绑定，冲突字段400；候选须启用且未过期，空池显式失败。direct显式清除proxy_id，specified只接受范围内的一个ID；random从授权候选等概率选一次，least_used按持久化账号关联数升序、相同负载按ID稳定决胜，均非实时请求负载均衡。候选资格、计数及写入在事务中重验，使用有界候选集合与稳定锁序避免并发批量都选中同一最小候选；禁止为least_used读取实时推理计数/扫描生成链。random/least_used只能在保存事务里固定选定一个proxy，不能每个生成请求随机换代理、不能额外网络探测、不能自动扩散shadow。事务竞争、回滚、相同负载tie和关联权限需独立测试。

## 7. 本地文件与拟新增接口

| 层 | 本地修改/拟新增 | 具体落点 |
|---|---|---|
| 配置/迁移 | config/config.go及测试；拟256_scoped_admin_api_keys.sql | 默认false与校验；独立key表/修订号/操作记录/幂等DDL兼容 |
| policy | 拟pkg/adminaccess/catalog.go、policy.go及测试 | 精确route、rule不可拼接、保护字段默认拒绝 |
| middleware | server/middleware/admin_auth.go、panel_rate_limit.go；新增scoped helper | 原legacy/JWT分支保留，scoped分支与key限流；step_up.go不改 |
| key service | service/setting_service.go窄依赖；拟custom_admin_keys.go | 生命周期、摘要/撤销/expiry/IP/revision；不更改原legacy生成方法 |
| key repo | 拟repository/custom_admin_keys.go | 明确事务与CAS/撤销竞争；无普通账号热路径查询 |
| account service/repo | 本地admin_service.go、account_repo.go；拟custom_admin_key_accounts.go、custom_admin_key_scope.go | 已有交易接口复用、窄patch和范围分页；不整文件覆盖上游拆分模块 |
| group/proxy | group_repo.go、proxy_repo.go及拟窄helper | 分组白名单只写该字段；授权范围代理候选/保存时选择 |
| handler/routes | handler/admin/account_handler.go、setting_handler.go、idempotency_helper.go；拟custom_admin_keys.go；routes/admin.go | scoped专用分支和JWT生命周期；复用共同audit/屏障，禁止通用DTO越权 |
| 其他管理返回 | user/subscription/usage/announcement/promo/payment等本地handler中确有catalog开放的路由 | 逐条DTO投影和step-up拒绝；未适配路由不放入catalog |
| 前端 | 拟api/admin/adminAccessKeys.ts、AdminAccessKeysSettings.vue与测试；SettingsView.vue最小挂载；types/index.ts；本地zh.ts/en.ts | 新独立卡片/表单、secret一次展示、权限摘要，不替换原legacy UI |
| DI/生成 | repository/service provider、cmd/server/wire_gen.go | 从本地provider重新生成必要差异，不复制上游生成文件 |

上述backend路径相对backend/internal（迁移/DI路径依表述）；表内文件均为未来实施落点，新增模块不声称已经存在。逐文件上游关联见新版分类附录，U17中停用legacy/纯布局等hunk仍排除。

## 8. 测试与独立启用门禁

S01：gate关闭时有效legacy/JWT/管理WS/request响应及DB/Redis调用数保持；无新表也可启动并处理旧管理/普通推理。
S02：route漏登记、HTTP方法、跨rule拼接、group/platform/type、未分组/shared账户、批量混入越权ID、list count/分页与显式deny。
S03：未知二开extra/credentials、null/省略/false/0、保护开关全量round-trip；写不允许字段不触发副作用；shadow与quota/probe快照保留。
S04：scoped无法绕过enforceStepUp；原JSON导入不新增TOTP，Codex导入/导出仍原要求；key lifecycle只JWT+step-up。
S05：keyA/keyB同创建者与JWT使用相同Idempotency-Key，不互相重放；权限削减/撤销后不返回旧敏感结果；改revision/轮换secret不重复执行旧操作；两key同header不相互冲突；coordinator=nil/ObserveOnly/缺key拒绝非幂等写；业务提交后MarkSucceeded失败/进程崩溃/claim过期不重复创建或记账；首次/重放投影一致。
S06：验权后资源被移动/删除、key被撤销或改scope；group关联竞争、取消/deadline、死锁重试上限；业务commit与aftercommit故障；回滚DB/缓存/outbox一致。
S07：scoped key独立限流、不受ExemptAdmin绕过；IP代理伪造、IPv4/IPv6/CIDR、expiry与停用；普通管理员限流策略不变。
S08：敏感DTO快照覆盖全部新增二开字段，普通JWT全功能不误裁剪；错误/审计/幂等缓存无明文secret。
S09：N+1预防、页大小/请求body/权限rule数/批量ID数有界、取消停止；无新增后台同步、生成链DB/Redis/控制面仍零增加。
S10：新旧binary/schema/gate组合、只读key→窄写key→测试账号灰度；回滚关gate保留表/审计/已提交账务。
S11：前端创建/轮换一次展示、键盘/移动/暗色、权限summary、过期/撤销/限流、错误安全、网络晚到不覆盖草稿；不存secret到URL/localStorage/持久化store。

所有S项通过并与主方案T01–T18保护矩阵合并通过后才启用。账号缓存/强隔离/Edge/调度等保护出现任何差异，即使scoped管理功能自身通过，也不能发布。
