# sub2api v0.2.18 → v0.2.19 完整升级与优化方案（扩展版）

> 本文保留实施前的设计与取证快照；其中“尚未实施/拟新增/当前版本”描述对应方案编写时点。2026-10-29 的实际改动、验证结果及尚未通过的发布门禁以 [v0219-status.md](v0219-status.md) 为准。

分析日期：2026-10-28，Asia/Shanghai。本文是方案，不是升级完成报告。当前项目代码、Git工作区、版本、配置与数据库均未修改。所有取证与方案文件写在 `/private/tmp/nuro-v0219-plan.R2wa2B/report-expanded/`，不在项目中。旧版方案保留于同级report目录。

结论：以当前二开为唯一实现基线，按业务目的逐hunk适配；不执行merge/rebase/cherry-pick，不复制上游整文件。实施范围包含安全、兼容、模型价卡、计数与目录可靠性修复，以及已要求纳入的建议项；图片按张能力默认关闭；scoped管理员密钥纳入独立阶段、默认关闭并保留legacy/TOTP；会改变既有system前缀的UTF-16指纹修复暂不引入。首Token、冻结文件、未知执行防重放和账务一致性为否决门禁。

## A. 当前工作区与真实基线

| 项目 | 实查结果 |
|---|---|
| 项目目录 | `/Users/inim/nuro-s2` |
| 分支/HEAD | `main` / `861d2874a5e87706465ab53170736b97c1e8ccdb`，提交标题`fix：0.2.18` |
| Git状态 | 开始检查为空；最终复核见`verification.json` |
| remote | 仅`origin`，指向`inimemail/nuro-s2`；未向项目添加上游remote或tag |
| 当前后端版本 | `backend/cmd/server/VERSION` = `v0.2.18-nuro.1` |
| 当前前端版本 | `frontend/package.json` = `0.2.18-nuro.1` |
| 最近历史 | `b4c3e9d76 优化`、`b4e858b8c 调度优化`、`b7138cc47 断流避让改动`、`a95f144b6 fix：修复问题`、`3520f6e7c fix：0.2.17` |
| 本地tag | 未有v0.2.18/v0.2.19；不能用本地tag代替当前二开基线 |
| 数据库迁移 | SQL已到254；247/248已分别用于Gemini缓存授权事件、存储定价 |
| 测试库存 | backend树973个`*_test.go`；frontend/src 188个`*.spec.ts`；edge-rs/src 6个Rust文件；只是库存，不代表已运行 |
| 工具链 | 项目go.mod声明Go1.27.3；主机`go version`为1.26.1；rustc1.96.1；Node24.13.0；pnpm10.29.1 |
| 保护文件 | 20个路径逐一存在，当前SHA-256与旧保护清单全部相符；新取证基线为当前HEAD，不重用旧baseline文字 |

当前实现确有RCU/COW调度、缓存ABCD、下游缓存加成、强隔离、阶段补帧、Edge session续传/租约结算、Gemini显式缓存、独立平台和时段价等二开。已撤回的另一批未提交改动不作为本轮基线，也不因本方案而恢复。

## B. 上游完整变化摘要

隔离临时仓库锁定两个tag解引用提交：

- v0.2.18：`ac1b8f1271be0d674fa6f074403c1410f30a3a81`。
- v0.2.19：`dece26153621959ccd7635814f4b2d08304e681a`。
- 范围71条commit，50条非merge、21条merge；259个最终差异文件，151 M、108 A、0 D、0 R；22597行新增、650行删除。
- v0.2.19 tag中的`backend/cmd/server/VERSION`仍为`0.2.18`；不能照搬这个文件或用它识别上游目标版本。

业务变化共21组，编号与完整附录一致：U01 Go依赖；U02分页；U03管理员保护400；U04上传拒绝截断；U05 Antigravity畸形工具调用；U06 Terra；U07 DeepSeek usage；U08 Pro价保留；U09 Gemini同账号重试；U10 Gemini绑定缓存重试；U11 Gemini目录；U12 Grok目录/CAS/审计/诊断；U13图片计数/按张计费；U14 Channel搜索维度；U15模型列表singleflight复查；U16 UTF-16指纹；U17 scoped管理密钥；U18赞助/布局/历史版本；U19模型广场；U20国产限频429；U21 OpenCode DeepSeek角色。

依赖净变化：x/crypto 0.57→0.58、x/net 0.60→0.62、x/sync 0.23→0.24、x/term 0.46→0.47、x/sys 0.48→0.49、x/text 0.42→0.43、x/tools 0.50→0.51。上游这两个tag间没有Rust Cargo、前端package/lock、Docker/Compose/HAProxy/K8s/deploy环境变量文件变化。该结论仅针对完整tag差异，不代表可以跳过二开部署回归。

## C. Commit与文件分类

完整清单见同目录`v0219-upstream-classification.md`：逐条列出71个commit、259个文件的M/A状态、本地同路径是否存在、功能归属、分类、实际落点；完整40位SHA和关联见`inventory.json`（业务归属由commit溯源，最终选取仍以两个tag间净hunk为准）；原始patch见`upstream.diff`。三者可交叉核对，没有用“等文件”代替全量清单。文件归属不等于每个hunk均获准引入；merge中唯一可见的combined conflict是`3f4e13aaba9b`的billing注释，采用最终tag语义，不重复实现分支中途状态。147个上游路径在本地不存在，部分只是上游拆分文件，不能推断为147项功能缺失。

| 分类 | 本轮决定 |
|---|---|
| 1 必须同步 | U01/U02/U03/U15；模型/usage/安全修复中的U04/U05/U06/U07/U11/U13/U14/U19/U20/U21以本地手工适配实现目的 |
| 2 建议项已纳入 | Grok管理端只读诊断与草稿保护、Gemini重试可配置、scoped管理员密钥完整独立阶段；新增本地优化O01–O09见T |
| 3 本地更好/已满足 | DeepSeek Pro不按日期降成Flash；模型广场冷路径快照；本地Gemini缓存授权与账务；既有step-up、Edge与调度实现 |
| 4 与二开冲突 | 冻结raw模型限制、Gemini缓存绑定、计费链、受管Extra、迁移编号、前端i18n结构；均见G/K |
| 5 不同步 | scoped中停用legacy/削弱step-up/宽泛覆盖二开字段等冲突hunk；赞助资产、整页布局/i18n重构、历史VERSION值；UTF-16因稳定前缀硬冲突本轮不改 |

分类按hunk，不按文件整体。同一`account_repo.go`既有目录快照修复，也有scoped事务包裹；前者独立适配，后者复用本地既有Tx/OnCommit实现目的。`admin_service.go`兼有管理员400/图片字段/scoped ProxyMode，必须分别review，不能整个文件搬入。merge提交只说明来源关系，绝不作为执行操作。

## D. 必须同步项目与实施理由

1. **U01依赖安全**：Go官方漏洞记录GO-2026-6860指出`x/net/html`相邻文本合并可出现平方级分配/拷贝，修复版本0.62.0；本地0.60.0需升级。原始公告保存在`GO-2026-6860.json`。先用Go1.27.3隔离构建环境求解模块图，更新必要模块及sum；不得顺手降级Go、前端安全pin、TLS指纹依赖或Rust。用govulncheck区分“依赖存在”和“生产可达”，不把它夸大成已证实的所有HTTP网关可利用漏洞。
2. **U02分页**：本地`PaginationParams.Offset`仍直接乘法。统一限幅offset及offset+limit；query解析用受int位宽约束的整数解析。API key内存分页、repository分页、ops自解析、安全审计filter分别覆盖，不能只改一个helper。
3. **U03管理员400**：本地`admin_service.go`的`ensureNotLastAdmin`、`UpdateUser`、`DeleteUser`仍返回普通error，换成稳定BadRequest业务码。权限约束、现有审计、账户操作不变。
4. **U04上传**：本地`parseOpenAIImagesMultipartRequest`会截断到20MiB后继续转发。改成上限+1并拒绝超限，图片限额按目标版本50MiB与全请求上限共同约束；边界恰好上限必须成功。Grok也不能截断，但不直接照搬无局部限制的ReadAll：保留总body/内存预算，并明确超限错误。媒体profile不能改。
5. **U05畸形工具调用**：统一拒绝非流式以及SSE聚合末帧无parts的MALFORMED_FUNCTION_CALL，不能把前面合法parts拼成成功。保留真实usage观察与原有终态/取消语义，下游使用脱敏兼容错误，禁止直接err.Error泄露内部来源。
6. **U06模型/价卡**：Terra列举、别名、effort、工具endpoint限制、价格与长上下文策略必须一起完成；冻结raw外适配。不能只在UI加名字就宣称支持完成。
7. **U07 usage**：本地OpenAI通用parser缺`prompt_cache_hit_tokens`。补最后优先级fallback，不覆盖标准cached_tokens，不把cache-hit算cache-creation，不减两次prompt。已有Anthropic兼容parser支持hit/miss，保持。
8. **U11 Gemini目录**：同步curated默认列表与默认测试模型；列表/详情按声明模型同源响应，同时保留本地custom list/force platform/allowlist优先级与安全path检查。
9. **U13图片计数/计费**：实际返回图片数优先；支持Gemini fileData/snake_case；禁止按请求n给少返回的图片多扣费；新增按张能力必须显式开启。价格显示与结算优先级一起校正。
10. **U14 Channel**：更新搜索模拟某个平台维度时不能清空其他平台维度；缺键继承、明确{}或null清除，false不能丢失。只局部合并这个key，不做通用深merge，不触碰time_pricing。
11. **U15去重**：本地`refreshCachedAPIKeyCodexModelsManifest`进入singleflight闭包后重读fresh缓存，复用上一轮已完成结果；不移植上游不同的模型cache对象。
12. **U20限频429**：仅独立国产Coding Plan、明确频率文案、无quota/balance文案且可信快照各窗口低于90%时进入已有秒级兜底；不将额度耗尽误判为频控。无效/NaN/缺窗口证据应保守处理，保留本地403周额度重置保护和其他平台路径。
13. **U21角色**：本地`strictChatCompatibility`明确排除了OpenCode。只为选定的OpenCode目的地且映射后为DeepSeek的请求允许developer→system；真实reasoning_content、未知JSON和数值精度保留，不波及OpenAI/Grok/OAuth及其他OpenCode模型。

## E. 建议项目纳入与手工实现

**U09 Gemini同账号重试**：配置`gateway.max_same_account_retries_gemini`，默认4、范围0–10，表示首次之外的重试；默认总5次与本地当前循环相同。以小helper替换Forward、ForwardNative及Chat兼容路径的硬编码。签名兼容降级为有界独立阶段，不能由上游重复错误无限抬高上限；backoff必须响应ctx取消和原deadline。只影响原本可重试的响应，不扩展失败分类、不增加成功路径等待、不改OpenAI规则。

**U12 Grok目录可靠性与诊断（含界面，已纳入）**：解析器、scope、观测合并、CAS持久化采用目标版的业务规则，但对接本地接口。只在管理端手动同步和已经存在的控制面任务中执行；本地不存在上游`grok_observed_models.go`，不能因此在正常请求后新增fire-and-forget任务或无界sync.Map。本轮实现手动同步、只读诊断和前端竞态保护；自动后台观测不纳入，不新增业务触发任务。现有在线固定大小响应model审计只处理已得到的帧。

- `MergeGrokCatalogObservation`分开保留最后成功与最后尝试，较旧开始时间不能覆盖新结果；失败保留最后成功目录。
- `PersistGrokCatalogObservation`最多3次CAS，冲突重读，不退化为整Extra无锁覆盖；这个3次属于目录CAS，不是OpenAI计费重试。
- 在本地`updateAccountExtraPreservingGrokRuntimeSnapshots`及其他写Extra入口原子保留目录/metadata，同时保留已有quota、billing、probe快照、placeholder与手动倍率字段；调用方transaction通过本地`clientFromContext`，定向失效在commit后。
- source scope至少账户、类型、endpoint fingerprint；API URL/token模式改变后旧快照只作为历史，不能直接判当前not_listed。自定义ID大小写不折叠、裸别名unverifiable、媒体条目out_of_scope；非法时间与未来快照不视为fresh。
- 管理GET详情诊断只读，不请求token、不fetch目录、不写数据库。目录fallback仅能力补充，不能把configured当上游已验证。
- 响应model审计在Grok原生Responses及buffered Chat/Messages间固定大小累计前序声明与冲突位；非Grokexact no-op；不能用响应model冲突改变sticky或选定账务模型。

**U17 scoped管理员密钥（已纳入独立阶段）**：实现精确权限catalog/policy、资源范围SQL、窄字段patch、可信IP、事务与已有after-commit衔接、按稳定keyID隔离幂等操作、按当前revision验权、结果投影、key限流、摘要/撤销及UI。新增能力默认关闭，旧legacy生成/验证/JWT/管理WS/step-up不改。scoped不获得二开缓存/强隔离/倍率/阶段等保护字段的写权限，也不能绕过原有TOTP；上游catalog与本地路由不一致时本地保护优先。管理保存时的显式代理模式一并纳入，不影响普通请求代理选择。完整文件/函数链与S01–S11验收见S和scoped-admin-key-plan.md；不是只做密钥表或middleware的半成品。

## F. 保留本地较好实现

- `applyDeepSeekOfficialPeakPricing`已经独立识别Pro，并无9月14日自动转Flash函数；U08修复目标已满足。保留本地时段价与显式override优先级。
- `ModelPlazaService.List`已有原子快照、TTL、singleflight和revision防丢失失效，不替换成上游服务。仅在`build`/`modelPlazaImageDisplayPricing`补分组模型固定价优先断言，必要时小修helper。
- Gemini缓存相关实现及本地246–254迁移有授权、pending quota、存储账、取消意图、恢复/maintenance、subscription与regional binding；不替换为上游简单缓存实现，不删除旧迁移。
- `POST /api/v1/admin/accounts/data`已有admin鉴权且不带个人TOTP；export与管理key轮换/删除带step-up，保留这些有意差别。
- Go/Rust冻结的缓存ABCD、强隔离、placeholder/session续传/结算、profile隔离、scheduler比上游通用路径更复杂，保持当前实现；上游没有Rust差异不意味着可以重写。
- 当前前端Vue/axios/DOMPurify及follow-redirects、form-data等安全override保留；不因为同步上游而覆盖package.json/lock。

## G. 冲突与适配决策

| 冲突 | 处理 |
|---|---|
| raw文件冻结，目标版在其中加Terra工具限制 | 不改raw。扩展`openai_chat_roles.go`的`validateNewModelChatReasoningTools`/`forwardAsCompatibleRawChatCompletions`，并覆盖Responses/Messages的raw fallback入口；列出所有raw直接调用，测试证明无旁路 |
| 上游拆分Go服务，本地集中实现 | 修改本地实际函数，不为同步目的新增同名拆分文件；K逐项映射 |
| migration247/248已占用 | 图片schema拟255；scoped独立表拟256。实施前查最大编号与迁移runner/checksum规则，绝不改既有SQL |
| auth snapshot版本不同 | 本地现为23，拟升24（实施时复核），不硬套上游v25；新增字段缺失=false，滚动期间关闭新能力，避免旧副本按另一口径结算 |
| Grok受管Extra与本地更多快照 | 在本地原子保存表达式内保留字段，不复制上游FOR NO KEY UPDATE整函数；scoped复用本地Tx/OnCommit，不加重复事务/发布框架 |
| Gemini缓存owner/区域/计费保护 | 只在现有绑定生成路径里按原账号重试，保持Owner/Key/Group/region/模型/订阅/expiry检查，不触发create/patch/delete重放或跨账号迁移 |
| UTF-16指纹与稳定前缀 | 指纹写入system归因block，非ASCII修复会改prompt字节。无法同时保证修复与旧前缀字节恒等；本轮排除生产改动，记录为单独兼容决策，不假称无缓存影响 |
| 上游前端i18n分目录，本地zh.ts/en.ts | 只补key，不迁移整个locale结构 |
| 上游Grok编辑页大改 | 只接同步竞态保护/必要诊断；不替换账号modal，不动ABCD、倍率、强隔离、阶段表单 |
| 图片未知数量按1兜底 | 该上游hunk不采用；实际观测与旧兼容收费来源分离，新开关不能制造provider usage或按请求n补收 |
| 官方默认价与用户自定义价 | 仅更新官方fallback/内置价卡，不覆盖channel/group/account倍率、cn_billing_mode、cn_api_mode、time_pricing |

**U10特殊边界**：本地`WithGeminiCacheBoundRequest`、`isGeminiCacheBoundRequest`、`GeminiCacheBoundAccount`是实际接口，不存在上游同名bound passthrough字段链。新增请求级错误标志时只用于绑定生成请求，Pool明确允许的429/5xx按同账号次数耗尽，不能改本账号健康或无感换号；最终错误走本地脱敏envelope，不能原样回写上游错误body。权限或本地缓存租约失效时停止，不因一次重试再走缓存创建/扣费授权。

## H. 明确不引入项目

scoped功能及其管理界面、受限查询/字段/代理能力已纳入，但不照搬上游的全局handler事务包裹，不重复创建本地已有after-commit/失效机制；不停止legacy key创建，不改变其旧接口规则。scoped不能绕过本地step-up，也不能把支付/订阅路由的鉴权或限流变化扩散到JWT/legacy。具体拒绝边界见S。

不引入赞助banner/logo、README赞助条目、纯删除图标/布局重排、i18n目录重构、无业务需要的文件拆分；不搬上游wire/Ent生成结果。`.gitignore`对应hunk只在本地确需放行新增scoped说明文档时适配，不搬其他规则。

不引入官方profit/profit-preview或利润控制migration；不改变本地下游缓存加成。U16因冻结语义冲突排除，而不是漏审。保留当前Rust、Cell、escrow、lane、lease、autoscaler与部署容量，不以本轮升级为由做结构优化。

## I. 数据库迁移与兼容顺序

1. 实施分支建立时记录HEAD、20文件当前哈希、SQL清单、真实数据库schema/checksum与已保存设置的脱敏快照。本文没有连接数据库，不能声称线上已应用到254，只确认源码到254。
2. 以新增`255_group_image_count_billing.sql`承接上游248目的：`groups.image_count_billing BOOLEAN NOT NULL DEFAULT false`及comment；用幂等DDL，测试已有同名不同类型时显式拒绝，不能静默认为兼容。实际编号以实施时空位为准。
3. 先在隔离数据库演练全新安装和当前schema升级；核查PostgreSQL锁等待、lock_timeout、DDL耗时与旧binary兼容。绝不自动运行线上迁移或回填。
4. 本地Ent schema最小增加字段，按本地工具链生成对应group实体/where/mutation/schema/runtime；Wire只按真实provider变更生成。review禁止其他schema漂移。
5. 服务/DTO/repo/auth snapshot贯通字段；PUT缺字段保留原值，明确false关闭，非OpenAI/Composite或未允许生图时拒绝启用/按既有归一化清除，并测试复制分组。
6. 新旧Go副本共存期间字段保持false；所有Go/Edge/embedded前端部署并确认后再对选定测试组显式启用。分组更新定向失效auth cache/控制面prepare缓存，不能全局FLUSHDB。
7. Grok目录仅Extra管理字段，采用CAS，无新schema；新字段必须可被旧代码忽略。若旧副本会整Extra写回，滚动期间不要启用目录同步；无需数据库大回填。
8. scoped能力以拟新增256承接上游247目的，并含本地policy_revision等兼容字段；实施时重新核对空号，禁止覆盖本地Gemini 247/248。表先创建、gate=false、全Go升级后才测试启用。
9. 回滚保留新增列/表与审计，先关闭图片/scoped开关，先drain再回旧镜像；不drop列、不篡改migration checksum、不回滚usage账本。旧代码无新能力，账单价格已发生的差异应按request/transaction审计人工对账，不重算历史扣费。

## J. 模型、usage、价格与计费

下列Terra金额是v0.2.19锁定源码快照的USD/百万token，供版本适配用，不宣称为部署当天实时官方售价。上线前再核价，不凭模型名字猜能力。

| gpt-6.1-terra | 输入 | 输出 | 缓存读取 | 缓存创建 |
|---|---:|---:|---:|---:|
| standard | 1 | 5 | 0.05 | 1.25 |
| priority | 2 | 10 | 0.10 | 2.50 |
| flex/batches（资源价卡） | 0.50 | 2.50 | 0.025 | 0.625 |

快照context_window1050000、max_input922000、max_output128000；长上下文阈值272000，input/cache倍率2、output1.5，阈值相等/跨阈值必须单测。支持none与max等已声明effort，不能把Sol禁止none的规则泛化给Terra；Terra工具调用须Responses，raw Chat只允许非工具兼容路径。不得重命名用户自定义alias或替换已保存映射。PricingService资源价卡、fallback、BillingService fallback、fast ratio=2、cache creation premium、实际结算上下文必须一致。

**DeepSeek**：Pro仍0.66/1.98/0.022（输入/输出/cache read，百万token）且不按2026-09-14切Flash，本地已满足这个逻辑。另查出本地Flash默认0.22/0.66/0.007与目标快照0.15/0.60/0.003不一致。这不是两个tag间新增的常量变化，而是当前二开与目标快照的遗留差异，必须独立列账；本轮纳入官方默认价卡同步，保留显式自定义价格与渠道时段覆盖，并核对原有DeepSeek峰谷/时点。不得删除本地未知模型不暗默归类的独立路由保护。

**Gemini默认目录**：移除curated的2.0-flash、2.5-flash、2.5-flash-image、2.5-pro、3-pro-preview；加入3.1-flash-image-preview、3.1-flash-lite-image、3-pro-image及preview、3-flash、3.1-flash-lite及preview、3.5-flash-lite、3.6/3.7/3.8-flash；保留3.1-flash-image、3-flash-preview、3.1-pro-preview/customtools、3.5-flash、4-argon。其中3.1-pro-preview-customtools在本地native列表已有、管理测试列表尚无，后者需补齐。默认连接测试改3.5-flash，Google One仍保守列表。只改默认目录，不删用户保存的映射/别名或禁止已有可用自定义模型。

**图片与混合计费**：group开关默认false；专用`/v1/images/*`及已确认的图片专用模型路径显式设置endpoint标志，不能只凭ImageCount>0推断。渠道token价与图片/按次价优先级按本地resolver处理，按次context tier仍传tokens以选档；混合Responses/Chat/WS含image tool仍计文本token，不能被开关吞掉。实际返回数>请求n/少于n/流partial/failure都需要独立用例。缺失可靠图片计数不能伪造“provider实际出了1张”；上游`resolveBillableImageCount(..., fallbackToOne=true)`这一hunk本轮不采用。实际观测计数、既有兼容计费数量及其来源必须分开，兼容兜底不得写入provider原始usage或“实际出图数”。新开关开启后计数不可靠时，不按1或请求n新增收费；有真实provider token usage且原定价允许时按既有token路径，否则保持未计费/待审计状态并阻止该能力放量，不能为避免漏计伪造usage。开关关闭保留当前合法旧计价及已存在的兼容语义；实际少返回时仍以可靠返回数修复多扣。失败请求不能自动补1张。

**ABCD与缓存高用量加成验收（全部为保持，不重写冻结代码）**：

1. A=稳定system/developer长前缀一个断点，explicit+24h；B清旧断点/retention、不新建断点，仍explicit+24h；C/D请求侧完全B。A/B不改下游创建usage。
2. C只将下游创建别名归零并从下游input/prompt扣创建量；本地账单仍使用原始创建量。D按当前有效模型/渠道/分组/请求service tier/长上下文区间固定点分摊，可靠计价缺失、动态区间不完整、溢出/跨区间保留原响应；不假称所有失败场景都能精确分摊。
3. 仅显式启用的OpenAI OAuth/APIKey文本账号及自定义文本alias；排除shadow、图片池、image/audio/video/realtime/search/embedding/moderation、非OpenAI。关闭或未知usage字节no-op。
4. 原始usage先保留，attempt账务汇总与presentation副本独立；下游重试累计开关只决定最终一次usage展示，绝不影响真实attempt账。
5. 缓存加成三字段不变，阈值默认100000、BPS固定点、默认关闭；阈值读取的是改C/D之前的raw cache-read，Go/Rust饱和结果一致；比例0、缺价、阈值下no-op。
6. 同步JSON、Responses终态、Chat最后usage chunk、Messages fallback、SSE/WS终态均测；前置/结构/文本/tool delta不得usage改写。D WS继续Go逐轮、按response_id结算；A/B/C、关闭及HTTP D继续原Rust本地路径，不因Terra切Go。
7. requestID/逻辑billingID对照raw provider usage→每attempt内部账→展示副本→下游面板。真实面板读内部数据，下游第三方面板可显示C免费/D分摊/加成；不能将二者混为“本地价格变了”。
8. prompt_cache_key、稳定前缀、session hash、sticky、24h、缓存接力/命中率、强隔离、health/guard/slots保持；首Token前无新DB/Redis/控制面/上游请求或重复JSON转换。

**其他计费**：保留GPT-5.6统计维度、search/image/tool费用；Kimi/智谱/DeepSeek独立身份、Composite显式路由、cn_billing_mode/cn_api_mode、time_pricing及209–211迁移不变；OpenAI每个有provider usage的attempt必达提交、逻辑账单幂等、失败未知无估算、OpenAI事务最多3次、其他平台原路径均作为门禁。

## K. Go实际文件、函数与调用链

以下都是本地实查落点；标“拟新增”的函数/文件尚不存在。

| 目的 | 本地文件/函数 | 调用链及禁止扩散 |
|---|---|---|
| Terra基础 | `internal/pkg/openai/constants.go`、`service/openai_model_alias.go`、`openai_compat_model.go`、`openai_codex_transform.go`、`pricing_service.go`、`billing_service.go`、resources价卡 | model alias→映射后模型→现有price resolver→plan/结算；不替换自定义映射 |
| Terra工具 | `service/openai_chat_roles.go`：`forwardAsCompatibleRawChatCompletions`、`validateNewModelChatReasoningTools` | `ForwardAsChatCompletions`→compatible wrapper→冻结raw；Responses/Messages raw fallback各自预检；无额外上游探测 |
| usage | `service/openai_gateway_service.go`：`openAICacheReadTokensFromUsage`；`openai_ws_v2/passthrough_relay.go`同名helper按真实可达性补一致性 | provider payload→raw usage parser→原内部账→原presentation；不导入不存在的response_handling.go |
| 既有CN解析 | `openai_gateway_cn_compat.go`：`normalizeAnthropicCompatiblePromptUsage` | 已有hit/miss支持不重写；避免二次减cached |
| 目录去重 | `openai_codex_models_service.go`：`refreshCachedAPIKeyCodexModelsManifest` | 同key singleflight闭包→fresh复查→必要时既有上游GET；ETag/304/隔离key不变 |
| 分页 | `pkg/pagination/pagination.go`、`pkg/response/response.go`、`service/api_key_service.go`、`repository/api_key_repo.go`、对应列表服务、`handler/admin/ops_dashboard_handler.go`、`securityaudit/handler.go` | HTTP校验→安全Offset→query/slice，所有自解析入口同测 |
| admin400 | `service/admin_service.go`上述三个方法 | 原保护拒绝返回稳定400；与scoped新分支分开review，正常JWT/legacy语义不变 |
| 图片上传 | `openai_images.go`：`parseOpenAIImagesMultipartRequest`；`grok_media.go`：multipart parser | 有界body→有界part+1→超限拒绝→既有media profile，文本链无影响 |
| 图片计费 | `service/group.go`、`admin_service.go` Create/UpdateGroup、`group_service.go`、`admin_group_duplicate.go`、DTO/handler、`repository/group_repo.go`/`api_key_repo.go`、auth snapshot双向映射 | DTO→schema→repo→auth snapshot→现有ForwardResult/OpenAIForwardResult endpoint标志→本地费用函数 |
| 本地费用函数 | `gateway_service.go`：`calculateRecordUsageCost`/`calculateImageCost`；`openai_gateway_service.go`：`calculateOpenAIRecordUsageCost`/`calculateOpenAIImageCost` | 新开关只在专用图端点起作用；混合token和历史上下文价优先级保留 |
| 图片计数 | `openai_images.go`/`openai_images_responses.go`；Gemini结果处理在`gemini_messages_compat_service.go`等本地文件；可拟新增小型`image_billing_count.go`与图片计数helper | 实际输出→有界计数→账务，不能把未知provider usage估算成真实值 |
| Antigravity | `pkg/antigravity/response_transformer.go`、`antigravity_gateway_compat.go`：`mapAntigravityCompatCollectionError`；`antigravity_gateway_service.go`：`collectClaudeStreamResponse`/非流转换入口 | finishReason先检查→脱敏协议失败；不覆盖本地大服务 |
| Gemini重试 | `config/config.go`及测试；`gemini_messages_compat_service.go` Forward/ForwardNative；`gemini_chat_completions_compat_service.go` | 单账号总尝试helper（拟新增）→ctx-aware退避；原总deadline与签名阶段有界 |
| Gemini缓存 | `handler/gemini_v1beta_handler.go`、`gemini_cache_reference.go`、`gemini_cached_content_service.go`/`gemini_cache_provider.go` | owner授权/绑定→本地native转发→同账号生成重试；cache管理操作不自动重放 |
| Gemini目录 | `handler/gemini_v1beta_handler.go` List/Get、`service/gemini_model_discovery.go`（拟增DeclaredModelIDs/cover helper）、`pkg/gemini/models.go`/`pkg/geminicli/models.go`、`account_test_service.go` | custom列表优先→声明+允许混合映射→原发现回退；只用于模型端点 |
| Grok | `upstream_models.go`、`openai_gateway_grok.go`、`openai_gateway_messages.go`：`readOpenAICompatBufferedTerminal`、`openai_gateway_chat_completions.go` | 管理sync→scope/merge/CAS→admin诊断；Grok响应固定大小审计→原usage结果；非Grokno-op |
| Grok新增小模块 | 拟新增`grok_model_catalog.go`、`grok_model_catalog_store.go`、`grok_model_diagnostics.go`、`repository/account_repo_grok_models.go` | 使用本地repo事务/定向失效，不增加AccountRepository全量框架或在线自动同步任务 |
| 受管Extra | `repository/account_repo.go`：`updateAccountExtraPreservingGrokRuntimeSnapshots`、`updateAccountExtraPreservingUpstreamBillingProbeSnapshot`及本地带group/billing事务保存链 | 原子保留受管值→commit后定向刷新；不丢倍率/placeholder等配置 |
| Channel/Plaza/CN429 | `channel_service.go` update；`model_plaza.go` build/helper；`ratelimit_service.go` handle429 + `ratelimit_cn_quota.go` | 管理保存局部merge/展示冷路径/明确上游错误后冷却，均不新建调度器 |

scoped具体本地文件/拟新增模块、事务与鉴权调用链见S第7节；本地优化涉及Makefile、现有tools、CI/release、模型同步组件与API、窄查询，见T逐项列明。

所有表中backend路径相对`backend/`。`wire_gen.go`和Ent只从本地模型生成；不手写覆盖。`admin_account.go/admin_user.go/openai_gateway_usage.go/gateway_usage_billing.go/gemini_models_endpoints.go`等上游路径在本地不存在，不为“对齐目录”而引入大重构。

**Terra入口核对（当前真实代码）**：`ForwardAsChatCompletions`在`openai_gateway_chat_completions.go`的六处分支调用compatible wrapper。`openai_gateway_messages_chat_fallback.go/forwardAnthropicViaRawChatCompletions`已有`validateNewModelChatReasoningTools`调用，扩展共用helper即可覆盖；`openai_gateway_responses_chat_fallback.go/forwardResponsesViaRawChatCompletions`没有这项工具校验，须在已确定upstreamModel、有效tools后补同一helper，避免“客户端走Responses、实际降成Chat”绕过限制。`openai_gateway_grok_chat_bridge.go`另有直调raw，但入口被`PlatformGrok + IsGrokOAuth`限定，保留Grok行为，不能机械改为Terra wrapper。冻结raw文件里的递归兼容重试沿用已校验的同一模型/请求约束，补测试证明不成为绕过路径，不修改该文件。调用链检查必须同时覆盖`tools`、旧`functions`、namespace与`additional_tools`，而不是只看raw函数引用数。

**Gemini实际图片落点**：本地`gemini_messages_compat_service.go/Forward`、`ForwardNative`与`gemini_chat_completions_compat_service.go/Forward`当前均有按模型名设1张的分支。拟新增请求级`gemini_image_output_accounting.go`，在`handleNonStreamingResponse`、`handleStreamingResponse`、`handleNativeNonStreamingResponse`、`handleNativeStreamingResponse`和`collectGeminiSSE`的既有解包结果处观测；每attempt重置，不能沿用前一账号计数。计数应区分上游实际产出与本入口下游交付集合；内部成本/审计保留完整原始provider数量，不能用转换器删减后的数量覆盖raw usage。按张收费仍受原endpoint/pricing契约约束，不能为增加计数把混合Messages/Chat误标为专用图片端点。上游采用单payload最大值以防累积SSE重复计数，对分散增量多图可能低估，不能声称完全精确；保留这个保守边界，除非存在可验证的稳定图片ID可有界去重，禁止新增无界payload/hash集合。fileData/inlineData及snake_case识别只从既有响应字节读取，不额外下载图片或增加上游请求。

## L. edge-rs方案

上游无edge-rs差异，本轮Rust生产文件计划0修改；`main.rs`、`stream_session.rs`严格冻结。`http_lane_pool.rs`保持当前connect timeout进入key、H1/H2界限、最小inflight、扩容/回收和资源上限，Cargo.toml/lock不升级。

既有链路：Go prepare/plan→Rust `relay_upstream_direct_core_with_early_placeholder`→原retry budget→SSE sanitizer→usage→complete/abort+WAL。新模型价卡必须被Go现有policy构造器序列化，经已有`creation_pricing`/模型区间传给Rust；`apply_openai_prompt_cache_creation_optimization_value`、`normalize_openai_downstream_usage_object_with_markup`、`allocate_openai_cache_creation_priced_usage`仍由现有Rust执行。

验证Terra文本资格、服务档位/长上下文、A/B/C和HTTP D原生路径、D WSGo逐轮、工具delta/响应终态、retry/continuation换账号policy重建。若现有可扩展plan无法表达新增价卡，则该模型发布门禁失败：不能静默改冻结文件、不能新增Go fallback、不能伪造可用或价格。先给出具体冲突再决定是否排除这项发布。

## M. Scheduler、缓存、倍率、Cell与资源保护

**调度不改算法**：本地priority数字越小越优先，`strict_priority`默认且禁止健康/倍率/负载跨优先级抢占。`health_first`/`health_cost_balanced`只有显式opt-in才启用；保留现有健康/精确成本层优先、priority作为后续比较项的独立排序，不能把strict模式的排序强加给它们。所有模式仍遵守显式LockedPriority、模型不匹配开关、隔离/能力/配额/冷却/并发等硬限制。OpenAI自适应路径还保留OAuth/setup-token主路、non-OAuth回退分区；health_first保留较慢/较差候选作为有序回退，health_cost_balanced继续锁精确倍率层，不能为了可用性擅自越层。样本窗口默认15min（1–120）、TTFT最多50样本；0–2样本参与，3可信样本才允许成本升级；0.05与0.06精确层不合并；慢速阈值默认60s（1–3600），关闭只禁TTFT升层。错误/TTFT独立freshness，sticky成功后才写，未知/Redis故障按既有降级。

字段`StrictModelPriorityOnModelMismatch`在本地Ent、SQL176和前端创建默认均为false，遵循`gateway_service.go/groupStrictModelPriorityOnMismatch`实际语义：**false时锁最高合格层；true允许模型不匹配时跨层回退**。不因名字带strict擅自反转UI或API。新增Terra/Gemini列表不能绕过这个规则。

**倍率不改**：最后可信声明值过期仍参与成本排序，保留原probe诊断；factor有限0.001–100默认1，只用于调度/guard；manual仅unsupported且有限0–100生效，1/clear移除，不再乘factor；保存原有定向失效和容量口径。raw probe/自动同步/账务倍率不变，官方profit不引入。

**Cell与租约不改**：OpenAI/Anthropic独立Cell、固定assignment、禁止在线迁移/scale-in；普通/Pool/runtime三类冷却原子联合检查、按所有权清除、generation CAS/floor单调。runtime-clear拒绝旧probe清新cooldown，错误健康清理不清TTFT/usage。tenant escrow独立、稳定holder、有界可回收；lease/permit/user/account槽幂等release与fencing，Edge先drain、Go后退出、最后关闭Redis。

**等待/容量不改**：用户100ms、账号50ms、Edge队列50ms、额外5、Retry-After1000ms；等待范围1–30000、Retry-After1–60000、额外0–50；数据库显式值优先。Edge全局9999（1–999999999）、per-account0、WS固定max0但动态目标/资源界限不变，queue512只队列。Autoscale target9999，warmup/headroom/FD/queue/GC等原策略保留，理论9999×副本不是容量承诺。

**重试与未知执行不改**：401=1/403=1/408=2/429=10/502=1/503=3/其余5xx=2、总20、精确码优先。Retry-After只429，Go最多30000ms、Rust实际等最多250ms、继承同deadline；Rust重试入口0ms不改。OpenAI发送后unknown/5xx不得借“还没真实输出”放宽重放；明确安全拒绝与pre_send按既有规则。响应头/body各自计数但共总预算。

**缓存与控制面不改**：不改prompt_cache_key/session hash/sticky/retention；不向模型目录加入每请求解析/查询；RCU/COW、不全池扫描，prepare缓存/Fresh Load合并/定向失效和现有指标保留。Grok目录和分组图片字段只在保存后必要失效，不能删这些优化。

## N. 前端、配置与部署

前端沿现有modal/card/间距/颜色/深色主题，紧凑独立模块。模型列表增加Terra与Gemini默认项；GroupsView增加按张开关，仅合法平台/允许生图时显示，默认false，文案明确专用图片端点且不会吞掉混合文本费用。Grok诊断采用折叠详情、清楚区分fresh/stale/source_changed/unknown，警告不只靠颜色；同步按钮有loading、取消/序号fence，切账号/模式/凭证/关闭弹窗和晚到响应均不能覆盖草稿。合并EditAccountModal时完整保留ABCD、缓存加成、强隔离、倍率、重试、阶段字段与批量组件。

API类型在本地`frontend/src/types/index.ts`、`api/admin/accounts.ts`等小补；i18n只本地zh.ts/en.ts。ChannelsView的平台集合从本地后端白名单核对，缺维度保留、关掉显式false，不能认为上游硬编码集合自动同步后端。UseKeyModal只补模型使用信息，不重做页面。

**新增建议界面**：scoped设置独立卡片，只有JWT管理员管理生命周期且写入需原step-up；权限范围/到期/IP明确分区，secret只一次展示，不替换旧legacy卡片。Grok诊断与两处模型同步均纳入O06/O09草稿/取消/视觉验收。构建及CI优化可修改相应workflow/工具pin，但不得改变Compose/HAProxy/K8s运行拓扑、线上显式env、容量与drain。

**现有设置模块原样保持**：网关等待与OpenAI响应头超时独立。后者enabled默认true、30000ms、关/0不限时、只拒负数及Duration溢出、部分payload保留旧值、原子热更新，text/media/default/TLS池按profile和timeout隔离；媒体跳过Transport和请求deadline、Billing Probe独立、流body不受头超时。恢复默认按钮只改表单，不自动保存。

**阶段补帧专项**：OAuth/APIKey开关、标量、stage与模板完全隔离；最多10阶段，新增留空，阈值1–100000ms/guard有效正数且递增；默认(800/5000),(3000/10000),(5000/15000),(10000/30000)。旧OAuth标量1–3000兼容，新stage可100000；超末guard暂停、恢复降档。全局仅表单模板，首次开/关闭再开深拷贝当前模板，保存账号后不跟全局动态变；reset只表单，清use_gateway_default，旧标量编辑清同类型stage，关闭清同类型全字段。不得新增运行时全局读取或影响另一类型。

**部署无新拓扑**：上游这次不需要改Compose/HAProxy/K8s。保留本地Dockerfile已真实cargo build并复制Rust二进制的多阶段构建；deploy菜单2构建Go+embedded前端+Rust，记录统一release SHA，不因Cargo package0.1.0不变就误称旧binary。构建完成再发布`v0.2.19-nuro.1`与前端`0.2.19-nuro.1`，本次方案不修改版本。

菜单2历史默认迁移仍只识别global256/512→9999、per-account4/32/64/128→0；显式自定义保留。旧Retry-After秒迁移只1–60、溢出回1000ms。Rust环境改变必须重启每个Edge，数据库保存不live-resize semaphore；Go等待与header timeout继续热更新。

HAProxy容量/Redis按副本计算，保留session reserve/start/resume/cancel同一Edge进程路由、/metrics外网阻断。K8s保持Edge先排空、Go后停、有限终止窗口、PDB/warmup，不引入随机session负载均衡或在线Cell迁移。本文未连接生产，不声称实际副本环境符合。

## O. 首Token前调用链对比与逐项影响

基线：认证/已有auth快照→本地billing与价格校验→请求级placeholder coordinator→本地scheduler RCU/sticky/guard/cooldown→原用户/账号/Cell/escrow admission→既有Edge prepare/lease或Go HTTP/WS→上游headers→首真实reasoning/text/tool→后续原usage保存/展示/settlement。

升级后保留同链：新图片字段随原auth snapshot走；新模型价卡复用原内存resolver与plan；仅目标Chat兼容路径复用现有JSON解析做模型/角色预检。分页/admin/scoped/Grok管理诊断/目录去重只在管理或模型目录端点，不进入生成链。usage、model审计/图片计数在读取上游事件后处理。**新增业务前DB=0、Redis=0、跨节点控制面=0、上游试探=0、fallback=0、等待timer=0**；这是实现验收目标，尚未通过运行测试证明。

以下矩阵对每个功能回答缓存/隔离/性能/调度/JSON/关闭边界；“原样”是必须由测试证明的要求，不是本次测试结论。T编号见P。

| 功能 | key/sticky/24h | 隔离/调度/槽/健康 | 首Token前新增工作 | JSON/usage/计费 | 关闭/非目标边界 | 测试 |
|---|---|---|---|---|---|---|
| U01依赖 | 原样，网络协议实现需回归 | 原池key、TLS/profile、错误分类 | 无业务I/O；库性能实测 | 不改应用协议/账 | 无开关，生产可达性分析 | T01/T12/T17 |
| U02分页 | 无关系 | 不改权限/槽 | 不在生成链 | 管理分页边界合法化 | 合法请求兼容 | T02 |
| U03admin400 | 无关系 | 权限原样 | 不在生成链 | 原500改稳定400 | 无开关，只有保护拒绝分支 | T02/T16 |
| U04上传 | 原cache策略不变 | 媒体profile/permit照旧 | 仅媒体有界多读1字节 | 超限拒绝而非截断 | 文本/合法体不变 | T03/T12 |
| U05畸形工具 | 原样 | 原错误分类、无假成功 | 上游返回后常量检查 | 错误终态，保留真实用量 | 其他finishReason no-op | T04/T10 |
| U06Terra | 既有key构造/稳定前缀不改 | 原scheduler与强隔离 | 复用现有模型/JSON判定，无I/O | 新模型/别名/effort/价卡；工具限定端点 | 非Terra不改，原开关关闭exact no-op | T05/T06/T10/T12 |
| U07usage | 原样 | 不污染health/multiplier | usage到达后解析 | hit仅fallback为cache-read | 标准字段优先、无字段no-op | T06/T07 |
| U08Pro保留 | 原样 | 原样 | 无 | 生产逻辑不改；Flash价差独立门禁 | 显式自定义价不覆盖 | T05/T07 |
| U09Gemini重试 | 不跨账号改key | 本地原健康规则，非OpenAI | 成功不加工作；仅失败重试有界 | 默认总5次不变 | 0=一次；其他平台no-op | T08 |
| U10绑定缓存 | owner/账号/region绑定不变 | 不封本地缓存宿主、不换号 | 不加授权查询，仅复用绑定结果 | 只生成重试；错误脱敏 | 非绑定保留原路径 | T08/T13 |
| U11Gemini目录 | 不改保存映射/生成key | 只读列表不改调度 | 仅models端点既有快照合并 | 列表/详情一致及默认项 | 自定义list优先，非Gemini不改 | T05/T08/T16 |
| U12Grok | 不改cache affinity | 诊断不驱逐/禁用账号，CAS定向失效 | 同步在管理端；生成仅已解析model常量审计 | admin新DTO/可信度；内部model冲突可见 | 非Grok nil；自动观测默认不启用 | T09/T10/T16 |
| U13图片 | 缓存增强/ABCD无关路径原样 | 原槽/lease/price guard | 原snapshot多一bool，无新查 | 计数修复；开关只专用端点计费 | false保留旧选价；未知值不估造真实usage | T03/T07/T13/T16 |
| U14搜索维度 | 不改缓存key | 原搜索开关/平台范围 | 管理保存无生成额外查询 | 仅局部配置merge | missing继承/{}、null清/false关闭 | T02/T16 |
| U15singleflight | 原cache key/ETag，不是prompt key | 原账号隔离 | 只目录刷新，减少重复HTTP | 不改models envelope | fresh复用，其余原路 | T05/T17 |
| U16指纹 | 非ASCII会改system前缀 | 保持当前 | 本轮0改动 | 本轮0改动 | 完全排除 | T18设计基线 |
| U17scoped | 不进入prompt链；保存配置仍原定向失效 | 仅新受限管理入口精确授权/事务/key限流；保护字段拒绝 | 生成链新增I/O=0；管理请求有独立有界鉴权 | 新key/受限DTO/patch/replay投影，原JWT/legacy不改 | gate=false无scoped表查询；旧key生成/step-up保持 | T19/S01–S11/T16 |
| U18布局资产 | 不变 | 不变 | 0 | 0 | 不引入 | diff审查 |
| U19Plaza | 不触生成缓存 | 原只读冷snapshot | 不在生成链 | 展示价对齐实收优先级 | 非图片/无override不改 | T07/T16 |
| U20CN429 | 不清sticky/缓存 | 只明确限频短冷却，真实额度原长冷却 | 仅失败后读已有快照 | 下游错误仍脱敏；账不改 | 非CN/未知证据原路 | T08/T11 |
| U21OpenCode | 不改key生成；目标角色表示变化需cache对照 | 只选定目的地/映射后模型 | 复用现有parser，无新往返 | 仅目标developer角色兼容 | 非目标字节no-op | T05/T06/T18 |

若角色规范化导致保护的稳定prefix内容发生额外变化且无法在现有兼容允许范围内证明可接受，则U21也必须按U16方式排除发布，不能承诺缓存命中“绝对不下降”。缓存指标受模型/流量影响，要固定account/model/prompt分桶对照。

**请求级输出/重放门禁**：gatewayWriteObserved、下游TTFT观察、upstreamCommitted、real_first_token四种状态不合并；Responses占位`response.transport_progress.delta`且delta=in_progress、Chat空chunk、注释都不算真实输出，结构事件不取消补帧。coordinator在billing后第一次账号前启动，原始deadline跨尝试、不扣/重置headers时间、最多一次，写锁串行；真实reasoning/text/tool已下发禁止跨账号拼接。即便只有placeholder可同连接处理失败，也必须先过execution_state安全判断，发送后unknown不能因“无可见输出”重放。

## P. 测试矩阵（后续实施门禁，非已通过）

| 编号 | 层级/用例 | 必须断言 |
|---|---|---|
| T01 | Go工具链/模块/漏洞 | Go1.27.3可重复构建；sum/module图最小差异；govulncheck及网络回归；无前端/Rust回退 |
| T02 | 分页/admin/Channel单测 | 负数、空、非数字、超int、MaxInt、offset+limit、内存slice；审计/ops独立入口；保护码400；search dims缺失/false/clear/别平台与time_pricing保留 |
| T03 | 上传/图片输出 | 上限-1/等于/+1、多个part、mask、malformed multipart、取消、body总上限；URL/base64/fileData/snake_case、空壳/重复累积与分散增量SSE、多candidate及转换交付集合差异、内部raw计数不被覆盖、少于请求n、未知计数来源隔离、失败不能补1 |
| T04 | Antigravity工具错误 | direct/wrapped JSON、末帧无parts、前序合法parts+坏终态、空candidate、非坏reason、非流/聚合各入口；usage/错误均不泄露 |
| T05 | 模型/目录/映射 | Terra canonical及支持suffix/未知suffix、自定义alias、none/max、仅Responses tools、raw旁路；Gemini custom/declared/force/allowlist/Google One、列表详情一致；singleflight闭包竞态/ETag/304/取消/隔离key |
| T06 | Go缓存ABCD/markup | 全文本alias、媒体排除、关闭原字节、原始usage副本、service tier请求优先、100000阈值/0BPS、Go/Rust固定点/饱和、C创建展示0且实账不变、D整段可靠价格/跨区间回退 |
| T07 | 计费/图片/价卡 | 新模型各tier、272000边界、长上下文、channel/group/time_pricing覆盖、Pro跨9月14/峰谷时点、Flash价差；按张开关false/true、混合文本image tool、token/按次/context tier、零价/缺价与Plaza显示 |
| T08 | Gemini/CN重试 | retry0/4/10与越界、配置未注入默认、签名阶段有界、ctx中断、同一deadline；绑定owner/model/region/expiry/subscription、429/5xx同账号耗尽不换、不改cache宿主健康；CN频率/额度混合、90%、缺失/NaN/无效、403周额度与各平台隔离 |
| T09 | GrokCAS/前端竞态 | 同账号旧sync晚到、新旧scope、CAS冲突3次/ctx取消/transaction回滚、普通Extra注入/缺失/旧值不能覆盖受管快照；裸alias/大小写/未来时间；切账号/模式/凭证/关闭modal晚到不覆draft，无继承属性读取 |
| T10 | SSE/WS协议与工具 | created/in_progress/item/part/role-only不算real；reasoning/text/function/custom/MCP及参数delta保留id/call_id/顺序；additional_tools与parallel_tool_calls；统一response.failed带status/output/安全id，Chat error+[DONE]，malformed EOF补双换行仅一终态，客户端取消不补 |
| T11 | Scheduler/Redis/race | strict小数字、mismatch本地反向helper、strict同层健康与opt-in独立排序/OAuth主路及回退分区/精确倍率/0–2样本/3可信升层/50TTFT/独立freshness/reset fence；成功后sticky；last可信倍率/factor/manual；generation CAS/floor、三类cooldown联合门禁/并发0仍检查、legacy key/Cell单故障；Redis超时有界 |
| T12 | Rust/Go跨语言Edge | A/B/C/HTTPD原生及DWS Go；提前placeholder headers前准时/一次/原deadline/同连接；发送unknown禁replay；429等待250ms/30s协议边界；session reserve单执行、任意字节offset含tool半帧、512KiB/1024chunk/256session、attached背压/detached75s/Go恢复约25s/同进程路由；上传30s idle/300s total；fallback额外ceiling默认0 |
| T13 | Lease/WAL/结算 | complete/abort/commit/renew/recover幂等、原始billingID、加密snapshot/WAL ack、Go重启/错节点恢复、usage冲突不重复扣、queue65536/worker32、有界full/closed/shutdown、永久4xx不毒占worker、408/425/429/5xx退避、grace120s/明确拒绝fence、replacement首次剩余窗口；drain/Redis槽均回收 |
| T14 | OpenAI attempt账务 | body未发可重试、send_started/unknown真实5xx禁切号/fallback、明确拒绝原规则、规则精确优先/总20、header/body共享、无usage无估算、usage提交queue拒绝有界同步兜底、最多3次OpenAI事务、其他平台原单次；多attempt只一逻辑下游账、WS按response_id |
| T15 | 补帧模板/语义/WS | OAuth/APIKey字段隔离/10阶段/空新增/递增/旧标量/重置仅表单/深拷贝/关闭再开新模板/marker清理/非法shadow媒体拒绝；真实TTFT与下游观察分离；learner8样本/64上限/4096route/P95+25%+2s/12–30s或20–45s/首可见冻结/image禁用/一次语义切号/5次3败暂停10min；WS不发语义字段；WS preflight45s/700ms、unsafe tool replay拒绝、group+response Redis750ms |
| T16 | 前端/API/安全契约 | lint:check/typecheck/Vitest/build；settings/账号/分组payload保留未知字段与null/省略语义、桌面/移动/暗色/键盘/tooltip；导入无TOTP、导出/批量凭证step-up；所有error/header/log无URL/domain/secret/internal rule/continuation/session；Grok诊断只admin详情、不进入公共DTO |
| T17 | 高并发/性能/资源 | 并发阶梯越过128/256/512；TTFT p50/p95/p99、成功率/429/502/503、account/Edge wait、queue depth/bytes、active/header waiters、fallback/retry、FD/RSS/CPU/GC、settlement/WAL、prepare hit/miss/evict/FreshLoad shared/DBfallback；取消/Go/Edge重启后资源回基线 |
| T18 | 稳定字节/前缀专项 | 20文件hash；fixed request/account/model各transport golden；key/session/sticky/24h；U16只建立差异证据，不改生产；U21目标角色修改前后cache seed/prefix对照并决定是否可纳入 |
| T19 | scoped完整保护 | S01–S11：gate关闭旧路径；key/rule/资源字段、TOTP、交易/撤权竞态、稳定keyID幂等/权限revision更新不重复执行与投影、IP、限流、旧账号配置/缓存屏障不变 |
| T20 | 本地优化/构建门禁 | O01–O09：原保护checker扩展、完整测试且禁止关键skip、统一SHA与locked依赖、工具链对照、secret-scan fixture、前端晚到/草稿、scope列表SQL计数、视觉回归 |

执行命令在隔离构建/测试副本中运行，真实DB/Redis用专门容器，不接生产/供应商；先确认工具链及服务就绪：

```sh
cd backend
go test ./...
go test -tags=unit ./...
go test -tags=integration ./...
go vet ./...
go build ./...
golangci-lint run ./...
# 定向race选中本地真实测试名：snapshot、FreshLoad、prepare、attempt、budget、continuation、GrokCAS
go test -race ./internal/service ./internal/handler ./internal/repository
govulncheck ./...

cd ../frontend
pnpm run lint:check
pnpm run typecheck
pnpm run test:run
pnpm run build

cd ../edge-rs
cargo fmt --check
cargo check --locked
cargo test --locked
cargo test --release --locked
cargo clippy --all-targets --locked -- -D warnings

cd ..
bash -n deploy-nuro.sh deploy/single-host-autoscaler.sh deploy/install.sh
git diff --check
```

根`make test-frontend`只跑critical subset，不能替代这次账号/模型/分组等完整Vitest。Docker多阶段构建需另做实际离线mock测试与产物身份核查。失败按真实原因归类；当前权限并非旧沙箱，不能把所有监听/测试失败预先归为环境问题。测试必须保存命令、SHA、工具版本、输出、服务环境和失败归因，红项不得用“历史问题”一句话跳过。

## Q. Shadow、Canary、回滚与验收

**分批实现与review顺序**：先锁基线/保护hash、修CI与构建前置，再分别实施安全修复、协议模型、图片schema、Grok诊断/表单、scoped基础及完整受限入口，最后完整回归/发布身份/压测与独立开关灰度；B0–B9逐批清单见U。每批独立可审diff与测试，禁止合成一大包再机械处理冲突。

**Shadow**：只用脱敏离线fixture、mock upstream或读取同一份已获得响应做纯解析/计费比较，不对生产模型重复发送shadow请求，不双取lease/slots、不双扣费，不写真实health/sticky。旧/新只读计价可并行比较，但只有主路径提交账务。

**Canary**：隔离staging完整通过后，才由发布操作启用单Go+对应Edge配对，逐步1%→5%→20%→50%→100%；样本足够且跨峰谷再升档，不能只按几分钟自动放量。新图片开关与scoped gate全副本就绪前false；Grok同步只对测试账户手动操作；scoped先只读测试key，再窄写测试账号，独立于普通生成流量放量。不要随机跨Edge分发已有session；WS/长SSE保留原进程直到结束/drain。

**硬门禁**：20冻结文件零差异；cache key/sticky/24h与强隔离golden；execution unknown重放=0；重复扣费=0；provider usage丢失=0；跨租户/账号/Cell/模型泄漏=0；内网secret/URL/header泄漏=0；新增生成业务前DB/Redis/控制面/上游往返=0；scoped越权/跨key重放泄漏/step-up绕过=0；取消/重启/drain无持久资源泄漏；完整Go/Rust/前端/Redis/race测试无未解释失败。任一不满足停止发布。

**性能门禁**：同机同并发、相同账号/model/prompt/effort分桶、新模型冷缓存与普通热缓存分开，至少三轮；要求p99无统计可信退化、错误率/限流不升且峰值资源有headroom。可使用p99增长超过5%且绝对超过50ms作为快速停量告警，但低于该阈值不等于验收通过；T17所有指标共同判断。响应头设置2000/5000/15000/30000/0ms分别测试，不以更高失败率换TTFT。超出机台稳定容量不宣称达到9999×副本。

**回滚**：停止放量→停新可选能力→按session黏性与Edge先Go后排空→回已验证旧release镜像/静态assets/配置→保留新增兼容列与WAL/账务记录→确认旧副本版本/env与原saved settings。不要覆盖线上env或回拨Redis generation；不要全删sticky/槽/lease/Cell/WAL；有待结算旧lease先按现有协议完成。数据库还原仅严重灾难恢复并经过明确独立操作，不能作为普通版本回滚步骤。

**验收证据**：基线/目标SHA、逐hunk分类、20 hash、新migration演练、依赖漏洞结果、各包测试日志、Go/Rust固定点golden、requestID账务对照、pre-TTFT调用计数、三轮压测数据、每个Go/Edge副本真实env/metrics、故障恢复及回滚演练。只有这些完成后才称“升级完成”；本文只完成方案与静态取证。

## R. 绝对冻结文件与语义保护

以下20个文件本轮不得修改、覆盖、格式化或生成替换：

```text
backend/internal/service/openai_prompt_cache_creation_optimization.go
backend/internal/service/openai_prompt_cache_creation_runtime.go
backend/internal/service/openai_prompt_cache_usage_presentation.go
backend/internal/service/openai_gateway_chat_completions_raw.go
backend/internal/service/openai_upstream_strong_isolation.go
backend/internal/service/anthropic_upstream_strong_isolation.go
backend/internal/handler/openai_edge_continuation.go
backend/internal/handler/openai_edge_ingress.go
backend/internal/handler/openai_edge_resume.go
backend/internal/handler/openai_edge_rs.go
backend/internal/handler/openai_edge_settlement.go
backend/internal/handler/openai_edge_sse.go
backend/internal/service/openai_edge_rs.go
backend/internal/service/openai_first_token_placeholder_coordinator.go
backend/internal/service/http_upstream_profile.go
backend/internal/repository/http_upstream.go
edge-rs/src/main.rs
edge-rs/src/stream_session.rs
backend/internal/service/openai_account_scheduler.go
backend/internal/service/scheduler_snapshot_service.go
```

当前SHA-256保存在`protected-current-sha256.json`，baseline为861d2874a5e87706465ab53170736b97c1e8ccdb。逐文件hash零差异只是必要条件；调用者新增helper/改变输入顺序也可能破坏冻结实现，因此还须完成O/P语义回归。

除20文件外，以下语义冻结：缓存key/stable prefix/sticky/24h与ABCD/markup原usage副本；OpenAI/Anthropic强隔离及关闭no-op；attempt与logical billingID/必达usage；Retry-After与共有deadline；严格优先级/健康样本/倍率guard；Cell/escrow/lease/fencing/drain；Go独立header timeout与media/TLS pool隔离；coordinator/阶段模板/真实TTFT/工具id续链/唯一合法终态；Edge reserve单执行/精确offset/有限journal与registry/上传预算；WAL快照/跨Go恢复/lease grace；WS45s/700ms与750ms分组状态；自动扩容headroom、prepare/FreshLoad/RCU缓存；独立国产平台/Composite与time_pricing；账号导入不TOTP、敏感导出/凭证step-up。不能通过改外部调用绕过这些语义。

## 本次执行范围说明（仅方案）

已完成真实Git/版本/迁移/依赖/工具链/测试库存读取、隔离获取两个tag、全部commit/file清单与扩展业务分类、本地关键函数/接口与冻结冲突核对、scoped完整独立阶段设计、O01–O09本地优化取证、20文件哈希及方案。没有执行项目代码修改、版本修改、合并/提交、依赖安装、应用迁移、部署、生产/模型请求、Go/Rust/前端测试。静态审阅不等于不存在运行风险；后续能否发布由P/Q证据决定。外部GitHub/漏洞数据只写临时分析目录，最终项目干净状态见verification.json。


## S. scoped管理员密钥完整独立实施方案


状态：方案，尚未写入项目。当前用户已要求将建议项纳入；本项从上一版的延期调整为纳入完整实施计划，不代表允许削弱已有TOTP、JWT、管理API、缓存权限屏障或保护字段。

### 1. 实际基线与必须解决的差异

- 本地`backend/internal/server/middleware/admin_auth.go/adminAuthWithSecurity`支持legacy x-api-key及管理员JWT，含管理WS subprotocol、session binding和审计。有效legacy key维持现有校验，不附加scoped仓库查询；JWT不走新key查询。实现时先无副作用地完成原已保存legacy值的常量时间匹配，再在gate开启且未匹配时进入scoped校验；不能按新prefix先分流误拒合法旧值，也不能先调用会Abort的旧校验再尝试scoped。保持原WS/JWT/header优先级与错误码；gate关闭的未匹配key走原拒绝路径。
- `backend/internal/server/middleware/step_up.go/enforceStepUp`按`auth_method == service.AuditAuthMethodAdminAPIKey`禁止机器密钥通过。scoped必须同样标识为机器密钥，Principal另存context；不能新建一个auth_method字符串躲过这个判断。本轮不修改step_up.go生产逻辑。
- `/admin/accounts/data`是有意不要求个人TOTP的JSON导入；`/import/codex-session`、导出、批量凭证等有独立step-up。保留原路由差异，不按“都是导入”统一增加/移除TOTP。scoped只开放精确catalog登记的路由，不继承JSON导入权限；需要step-up的操作继续仅允许满足原规则的管理员会话。
- `routes/admin.go/RegisterAdminRoutes`已有panel limiter、audit和GeminiCachePermissionBarrier；新受限分支仍在同一公共屏障下，不注册绕行路由。
- 本地`repository/account_repo.go`已有TxFromContext、group/billing原子更新、OnCommit、受管Extra保存和outbox；不复制上游accountRepository整文件，不再造第二个全局事务/失效框架。
- 当前`handler/admin/idempotency_helper.go/adminActorScope`仅按admin userID区分；两个scoped key可属于同一创建者，必须按稳定keyID隔离操作，policy revision只用于当前授权/响应投影。现有ActorScope只参与fingerprint，而仓库唯一键为scope+keyHash；因此scoped必须同时隔离实际存储scope，不能只改ActorScope。
- 本地panel limiter按userID限流且允许ExemptAdmin；scoped不能因带管理员角色而自动豁免，必须有独立key身份分支，legacy/JWT保留原规则。

### 2. 开关、数据与滚动升级

拟新增启动配置`security.scoped_admin_api_keys_enabled: false`，对应现有Viper规则的环境变量`SECURITY_SCOPED_ADMIN_API_KEYS_ENABLED`。这是拟新增名字，不是当前已有配置。控制新生命周期API、受限鉴权分支与UI可见性；关闭时不访问scoped表，不改变legacy/JWT行为，不进入业务生成链。配置关闭还需惰性构造仓库/maintenance，不因表缺失在启动DI/定时任务中报错；新API在执行仓库前拒绝、UI隐藏，不能只在请求鉴权处分支。启用/关闭按Go配置加载机制重启控制面，不改变现有可热更新的header-timeout/等待设置语义。

拟新增`256_scoped_admin_api_keys.sql`，实施时复查编号，排在图片255之后。独立表保留id/name/description/prefix/hash/enabled/expiry/IP policy/权限policy/creator/创建更新/使用/撤销/软删除字段，并增加明确的`policy_revision`用于权限变化、事务重验和响应投影失效；幂等操作身份不随revision变化。只存高熵随机密钥摘要，不存可恢复明文；创建/轮换只展示一次，撤销不可被普通启用操作复活；逻辑删除保留审计。DDL、索引和字段长度参照锁定上游migration247目的，以本地新编号和兼容校验落地，不回写历史SQL。

schema先行、所有Go就绪、开关保持false，然后测试key启用。滚动期间不向旧Go随机发送新key：新key流量绑定具备能力的管理入口，或等所有Go升级后再开启；普通推理请求仍走原Go/Edge链。回滚先关闭gate并撤销测试key，保留兼容表与审计，不drop、不撤销已经成功的业务写入。

### 3. 权限与二开字段保护

- 精确登记`HTTP method + Gin full route`，未知route/permission默认拒绝，目录前缀不授予子路由权限；读权限不隐含写，credentials.write不隐含credentials.read，按分组范围不得拼接不同rule里的平台/账号类型条件。
- account/group SQL范围先过滤再Count/分页；批量操作每个ID都验权，不能以第一个ID结果授权整批。无权限资源统一使用与不存在一致的状态，防止枚举；list/total/error/关联字段均不能侧漏越权对象。
- `name/notes`、model_mapping、状态、调度参数、网络凭证、分组关联分别授权；平台/type创建后不允许通过受限key变更。所有`credentials`按白名单子字段patch，禁止整map替换；未知key、`extra`整对象、null清空保护凭证一律拒绝。省略值不写回零值。
- 缓存ABCD、cache markup三字段、strong isolation、shadow关系、prompt/session/sticky、placeholder stage/template、account retry/race、adaptive/manual multiplier、guard/Cell/lease/escrow和健康runtime字段，默认不向scoped开放写入口。不能拿accounts.credentials或accounts.schedule当成批量改这些二开字段的授权。
- 账号创建使用本地正常校验和默认关闭语义；不能用上游较少的credentials类型或平台列表覆盖本地CN/Composite能力。新key的可授权platform/type取本地真实支持集合、逐项登记；旧账号不因新catalog不认识而被自动修改或删映射。
- 已有敏感step-up路由仍遵守原middleware。上游catalog中的Codex导入/支付/订阅动作若本地要求二次验证，则对机器key显示为不可用并说明原因，不复制上游白名单来撤掉保护；普通JWT/legacy管理功能不变。
- key生命周期仅管理员JWT；写操作还必须通过原step-up，机器key不能给自己新建/扩权/轮换key。原legacy生成/读取/轮换/删除规则保持，不带入上游停用legacy新建及新增legacy禁止访问旧接口的hunk。

### 4. 事务、竞态、外部副作用

受限写调用链：鉴权principal→精确route→字段检查→有界事务→按固定锁序重读key有效性/policy revision及目标account/group→基于同一rule再验权→本地窄字段写/增量关联/outbox→commit→已有定向缓存发布→按当前读权限响应。撤销/改权限使用同一串行化约定，明确定义commit线性化点；不能在初始鉴权后放任长事务使用旧权限。

复用`UpdateAccountWithGroupConfig`、`UpdateAccountWithGroupConfigAndBillingSettings*`与现有clientFromContext/OnCommit，不将上游accountTransactionScope覆盖到每个普通管理操作；新受限仓库可使用事务client副本，但所有ORM/原生SQL/关系读取必须同一tx。分组关联只对授权变化做增删，保留其他分组、关联priority、guard override、created_at。原子保留Grok/倍率探测/placeholder/cache配置快照，不接受调用方伪造受管runtime值。

现有OnCommit已发布的内容只发布一次，不再注册重复hook。失败回滚不得刷新scheduler/prepare/auth snapshot、写健康/冷却或调用真实上游。commit后ctx处理复用本地既有约定并加有界耗时；不能把已commit的Tx context带给后续DB读。OAuth刷新等不可回滚外部动作不能在持有DB锁时执行；用已有可靠管理任务/明确操作记录在提交后有界执行，失败明确反馈“已保存/待执行”，不假装整次回滚、不静默fire-and-forget重放。

凡无法在不改变冻结scheduler/Edge语义的情况下贯通的动作，受限catalog保持禁用；功能开关不放量，保留具体阻塞证据，不能改保护区绕过。

### 5. 幂等、DTO与限流

拟对scoped请求设置稳定`admin-key:<keyID>` ActorScope，并把原操作namespace与keyID组合成独立存储scope；method/fullroute/payload继续进入fingerprint。不能只改ActorScope导致两个key仍争用同一scope+keyHash。legacy/JWT维持原scope与`admin:<userID>`。`policy_revision`用于每次鉴权、事务内重验和响应投影，不进入操作去重身份；同一key改权限或轮换secret后，已完成操作仍被识别，不能因新revision/secret而再扣费、再创建资源。重放前验证当前有效性、路由/资源权限并重新投影；旧投影不得直接复用，权限不足则拒绝或只回当前允许的最小状态。不同key/JWT互不读取结果；新建另一key不是原key自动续接，客户端不能靠换key重试未确认的财务操作。

正常、重放及原有兜底均须投影；scoped非幂等写要求有效Idempotency-Key，coordinator为nil、ObserveOnly、存储不可用均不得降级执行。JWT/legacy现有fail-open集合保持。实际`service/idempotency.go/Execute`是在execute返回后才MarkSucceeded，且handler在coordinator为nil时直接执行，不能宣称单靠现有helper已覆盖“业务提交后进程崩溃”。scoped侧为非幂等写增加同库唯一操作记录，与业务变更同事务提交（拟256中独立`scoped_admin_operations`表或经验证可复用的等价记录）；持久化稳定keyID/操作namespace/幂等key摘要、请求fingerprint、资源ID和最小结果，不持久化完整敏感DTO。唯一约束与有界claim/事务锁保证并发只执行一次；generic成功标记失败/进程重启后，先读业务操作记录确认提交，不能超时reclaim后再次执行。记录保留至少覆盖对外承诺的重试窗口，窗口过期不承诺无限去重，响应与文档必须明确。

commit成功但响应屏障超时/连接断开时，使用同一稳定key与操作记录查询/重放，不重新扣余额、分订阅或重置quota。外部不可回滚动作必须有供应商幂等能力或可靠任务状态；执行结果未知时只查询/人工核对，不自动重试。相关非幂等路由只有完成此原子性适配才加入catalog，不能为凑齐上游功能绕过门禁。

受限DTO明确投影本地新增字段，不先返回通用Account再靠删几个secret字段。账户credential仅授权读到model_mapping；用户关联、分组价卡、上游URL/token、Grok诊断内部信息、订阅关联和usage请求内部细节逐字段控制。即使上游新增字段也默认不可见。

`panel_rate_limit.go/userScoped`只在存在scoped Principal时改用keyID身份，必须应用key规则且不继承管理员豁免；JWT/legacy仍原逻辑。安全审计记录keyID/动作/授权资源/result，绝不记录完整密钥、凭证、上传body或敏感内部错误。IP白名单复用本地`SecurityClientIP`和可信代理配置，不能直接信任任意X-Forwarded-For或重新解释整个站的真实IP规则。

### 6. 可选代理模式并入受限管理阶段

上游新增direct/specified/random/least_used属于管理保存时的选代理能力。纳入完整实施清单，但默认不选、不改现有proxy_id、不改变普通请求代理路由。新模式只允许具备accounts.network权限且账号/代理范围均授权的管理操作；省略模式完全保留当前绑定，冲突字段400；候选须启用且未过期，空池显式失败。direct显式清除proxy_id，specified只接受范围内的一个ID；random从授权候选等概率选一次，least_used按持久化账号关联数升序、相同负载按ID稳定决胜，均非实时请求负载均衡。候选资格、计数及写入在事务中重验，使用有界候选集合与稳定锁序避免并发批量都选中同一最小候选；禁止为least_used读取实时推理计数/扫描生成链。random/least_used只能在保存事务里固定选定一个proxy，不能每个生成请求随机换代理、不能额外网络探测、不能自动扩散shadow。事务竞争、回滚、相同负载tie和关联权限需独立测试。

### 7. 本地文件与拟新增接口

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

### 8. 测试与独立启用门禁

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


## T. 按当前项目核实的九项本地优化


范围：优化维护、管理操作可靠性、验证与产物一致性。生成链、20冻结文件和其受保护语义不做架构优化。每项区分静态确认与待运行验证；不宣称尚未测量的吞吐/TTFT收益。旧版升级记录只作历史参考，不能当本次测试结果。

### O01 扩展现有保护契约，避免漏测和重复造轮子（P0）

**证据**：`tools/check_upgrade_contracts.py/main`写死读`docs/upgrades/v0218-protected.json`，已检查20文件hash、迁移编号和两份Go Dockerfile；`.github/workflows/backend-ci.yml`已经运行它和对应2个测试。当前20 hash与该旧清单一致，不能误称保护门禁不存在。

**实施**：在原工具增加明确manifest/基线参数并默认沿已批准清单；拟新增`docs/upgrades/v0219-protected.json`以本次HEAD记录20 hash和所有既有migration filename/历史重复prefix。历史文件内容checksum也纳入新增门禁，防止不改编号却篡改旧SQL。baseline可追溯，不允许先改冻结文件再更新hash“修绿”。生产函数一字不动，只改tools/docs/CI；增补manifest缺失、格式错误、路径越界/symlink、hash变化、历史SQL修改、已有历史重复与新冲突、两个Dockerfile失配测试。

**验收**：任意冻结文件改1字节/删除/替换应失败；恢复后通过；合法新增255/256通过、重复编号失败；既有历史重复不误拦。静态hash不是语义证明，仍必须跑ABCD/Edge/调度golden与调用链测试。

### O02 让CI覆盖实际发布内容，明确集成测试不能空跑（P0）

**证据**：根`Makefile/FRONTEND_CRITICAL_VITEST`仅6个文件，`make test-frontend`为lint/typecheck+这6个文件，前端CI也调用它；当前backend CI包含unit/integration/lint，但没有Rust测试job。部分集成测试Docker不可用时t.Skip；历史验证文档还明确记录“最终完整复跑待做”。

**实施**：保留快速critical入口，新增`test-frontend-full`（完整Vitest）、生产build和独立Rust fmt/check/debug+release test/clippy job；发布required checks覆盖完整矩阵。精确记录测试数量/skip原因，关键PostgreSQL/Redis测试先检查隔离服务可达，缺服务使该required job失败，不能把skip当通过。Rust产物路径通过现有`NURO_EDGE_TEST_BINARY`接已有`TestEdgeStreamRealRustTransportRecovery`，不新造不相干的mock替代真实Go/Rust恢复验证。并行job仅用于无依赖检查；冻结/迁移/编译失败不能继续产物发布。

**验收**：账号ABCD/markup/stage、分组、模型、诊断、scoped的新测试都被完整job发现并执行；Docker断开时关键集成job明确失败；没有skip掩盖SQL/Lua/幂等测试；不因CI扩充改线上默认或引入运行时查询。

### O03 修复发布源码身份与可重复构建风险（P0）

**证据**：`.github/workflows/release.yml`的update-version/build-frontend checkout没有显式采用`inputs.tag`；release job则使用`inputs.tag || github.ref`。manual dispatch选择的workflow分支与输入tag可以不同，存在前端/后端源码不一致的静态风险，尚未执行一次错误发布来“证明”。该workflow还把input.tag直接插入shell，存在输入解释风险；Rust cross命令未带--locked；`.goreleaser.yaml`的before hook运行`go mod tidy`。

**实施**：只解析一次受约束的release tag并解析immutable SHA，经job outputs传递；所有checkout使用同一SHA。输入经env传入、验证允许的版本格式并安全引用，不能把表达式直接拼进命令。版本以本地nuro发布规则生成，校验tag/backend/前端/embedded资源一致。`go mod tidy`移到有review的依赖变更阶段；发布只验证已提交mod/sum不变。cross build补--locked并验证构建后Cargo.lock未变。产物manifest记录releaseSHA、版本、Go/Rust/frontend sourceSHA、依赖锁hash、工具链、镜像digest与二进制hash；Rust包0.1.0不变不当成“没更新”。

**验收**：branch≠tag的离线fixture/mock workflow解析测试、非法tag/空tag/不存在tag、前端旧artifact混入、锁文件变更都被拒绝；amd64/arm64使用同一源码。只修改构建/发布配置，不改变Docker运行拓扑、容器env、菜单2容量、端口、HAProxy/session路由和drain。

### O04 对齐已存在的构建工具差异，保留依赖锁（P1，实施于构建验证前）

**证据**：Go模块/两份Go Dockerfile/CI已对齐1.27.3，主机Go1.26.1；Node CI/release为20，Docker为24，主机24.13.0；pnpm CI/Docker为9，主机10.29.1，frontend没有packageManager精确pin；Docker Rust1.90、release stable、主机1.96.1。不是发现所有构建都错，而是相同源码不一定同工具链。

**实施**：Go沿1.27.3隔离工具链，不降模块directive。前端沿生产Docker的Node24路线，优先用已记录24.13.0做一致性验证并锁定审核的patch/image digest；pnpm沿现有CI/Docker的9.x选择确切可验证patch，写packageManager并同步CI/Docker，不凭主机10.x强制升major，不无故重锁依赖。Rust以已批准生产1.90容器构建作为基准验证MSRV/locked graph，release停止漂移stable；1.96.1仅作额外兼容对照，若1.90不再满足则独立说明工具链适配，不改Rust生产源码绕过。精确pnpm patch/镜像digest和govulncheck pin在实施前从可信发布记录校验后写入，不在方案里伪造已核验版本。

**验收**：同一锁文件在本地隔离/CI/Docker可复现install/build；没有因pnpm脚本策略差异丢失必需构建步骤；现有安全override和xlsx受控例外保留且重新审计。build缓存key包含锁文件/工具版本/target，缓存只是加速，不能遮蔽不一致。

### O05 修复失效的secret-scan目标，审计结果不外泄（P0）

**证据**：根Makefile的secret-scan调用`python3 tools/secret_scan.py`，当前tools只有保护契约、pnpm审计例外和部署测试脚本，该scanner不存在；在现有security workflow也未见调用该目标。没有扫描真实密钥内容，也不声称已发现泄漏。

**实施**：补实现该缺失入口及针对性测试，或将目标接到经审核的同等现有实现；选无新增运行时依赖的离线规则，优先检测新增/变更内容中的高置信私钥/令牌模式，显式fixture例外须逐条注明原因，不能全目录跳过。安全CI纳入make secret-scan，输出只含规则ID/路径/行号与脱敏摘要，不打印匹配内容；不读取生产.env、Keychain或用户目录，不上传扫描数据。scanner能力仅为防误提交，不宣传能发现所有秘密。

**验收**：安全模拟令牌/私钥fixture失败、合法文档占位符合例外、路径含空格/二进制/大文件有界、错误退出码能阻断CI、日志没有完整测试秘密。新scoped密钥一次展示不写URL/localStorage/持久store。

### O06 完善模型同步取消/草稿生命周期，保留已有局部保护（P1）

**证据**：`ModelWhitelistSelector.vue/syncUpstreamModels`只有isSyncing与catch中的取消识别，没有请求signal/作用域generation与卸载校验；结果返回后读取当前props.modelValue并emit。`EditAccountModal.vue/syncAntigravityUpstreamModels`已有show+account对象身份校验，不能说完全无保护，但同一对象内凭证/模式变化、关后同对象重开、finally覆盖新请求状态仍需防护；catch使用error.message。`accountsAPI.syncUpstreamModels/Preview`当前没有signal参数。

**实施**：这两个API只加可选signal参数，保持旧调用签名兼容；组件维护本地递增generation和AbortController，identity按账号ID/平台/type/endpoint/凭证revision与modal session定义。凭证revision只用内存变化计数，不把密钥拼成cache key/log。关闭/切账号/切模式/改凭证/卸载使generation失效且取消旧请求；相同账号对象重新打开也视为新session。处理结果、错误、finally都验证generation，旧响应不得清除新请求的loading/弹过时toast。用户同步期间主动修改草稿时不自动覆盖/复活已删除模型，保留待添加差异并让用户应用；正常未编辑才合并新增，保留手写alias和大小写。统一复用已有`isApiRequestCancelled`与`extractSafeApiErrorMessage`，取消静默。

**验收**：用deferred Promise覆盖同账号重开、对象不变改凭证、A→B→A、模式切换、连续两次同步/先后颠倒、卸载、用户删/清空/新加映射和旧finally；无stale emit/toast/loading。不改AccountsView已完成的查询快照/ETag/today-stat批量合并，不添加页面加载自动同步或新后台任务。后端已存在body limit及ctx复用，前端abort不被误称为保证取消已提交的远端副作用。

### O07 预防新增scoped列表N+1与关联信息泄露（P1）

**证据**：上游拟采用`custom_admin_key_scope.go/ScopedAdminList`在每行调用loadAccountGroupIDs；这是新功能导入时会带来的逐账号查询，当前本地尚无该scoped接口，不冒称是现网性能故障。本地已有`account_repo.go/loadAccountGroups`可批量，但它WithGroup载完整价卡/guard等信息，不宜为受限列表直接取全对象。

**实施**：新受限列表按已授权筛选出的本页accountIDs，一次查询AccountGroup的account_id/group_id/priority等必需列，构造窄映射；禁止把全group service DTO流出。总数和分页仍使用相同SQL授权谓词，不先全表查后内存裁剪；权限规则/页大小/ID集合均有界，scope/fields不在每行重复解析。代理least_used选择仅管理事务中用有界聚合候选，不引入生成链全池扫描。

**验收**：SQL计数随页大小保持固定数量级（Count+本页+关联批量等明确固定查询），结果与原授权语义、关系priority一致；空页/无权限/shared组/大页/deadline/撤权竞态无越权、无无界分配。对普通accountRepo列表/调度快照不做这次重构。

### O08 用局部契约fixture防止账号/分组表单回写丢失二开字段（P1）

**证据**：本次U12/U13/U17均接触本地EditAccountModal、GroupsView、types与DTO；这些同文件还包含ABCD/markup、强隔离、倍率、阶段和模型配置。整页或整DTO替换风险高；现有本地测试与保护文件hash不能替代这些调用者的payload语义。

**实施**：扩展现有账号/分组测试中的显式fixture，覆盖create/edit/bulk/duplicate/import/update的“省略/false/0/null/明确清除”；scoped额外拒绝不开放字段。保存前后对照protect属性，不把未知字段当成可直接发送的任意map。新图片bool通过repo/auth snapshot双向round-trip，前端/后端契约同时检查。模型广场仅新增price优先级断言与必要helper修复，保留现有revision/TTL/singleflight架构。

**验收**：新控件保存不清空原账号stage或把OAuth/APIKey串用、不改倍率/缓存/隔离，旧payload仍兼容；显式关闭才清同类型策略，历史marker不复活；价格override/时段价保留。无需为此新增通用schema生成器或大范围共享表单重构。

### O09 新界面集中做好视觉与可访问性，不重做旧页面（P1）

**范围**：Grok诊断、图片按张开关、scoped管理卡片及必要同步状态。沿本地卡片、圆角、间距、字体、深色主题与图标；不引入新UI框架，不整体重排账号创建/编辑/批量表单。

- Grok顶部显示状态/来源/最后成功时间，差异数量为summary，详情按需展开；长modelID换行或局部滚动，单项可复制；同步按钮有加载和明确取消/失败提示，诊断GET只读且不自动请求供应商。
- 按张开关附简短作用范围，明确默认关闭、专用图片端点、真实计数、混合token不受影响；禁用时说明平台/生图权限原因，不能只隐藏导致用户误认为保存丢了。
- scoped列表展示名称、权限摘要、范围、到期/状态和最近使用；新建/编辑分开权限组和资源范围；高风险动作给清楚范围说明，机器key不可执行的TOTP动作不伪装可用。新secret只一次揭示/复制，关闭提示保存、禁止持久缓存。
- 320/375/768/1280px、暗色、200%缩放、键盘Tab/Enter/Esc、focus与label/aria、非颜色唯一状态；复杂权限表移动端改可展开卡片，控件不横向溢出。

**验收**：完整组件测试+浏览器视觉截图核验；只新增测试视图/截图证据不往生产注入调试按钮。未实际运行浏览器前不宣称样式“已验收”。

### 明确不做的所谓优化

不改20冻结文件，不拆大service以追求目录对齐；不换调度器、不改strict/health排序、不改优先级方向和mismatch反向helper；不改sticky/session/cache key/24h，不全清Redis；不把Edge原生ABCD/HTTP D退回Go；不改header/body/shared retry、0ms入口、attempt/execution-unknown；不改WAL/fencing/Cell/escrow/autoscale默认和部署路由；不为诊断增加生成链网络/DB/Redis/控制面开销；不自动后台同步模型/刷新token；不重做已经实现的RCU/COW/FreshLoad/prepare缓存、模型广场snapshot和AccountsView合并请求；不一次升级Vite/Vitest/xlsx等整个前端生态。


## U. 分批实施、交付与风险关闭清单

本节是未来编码与验收顺序，不是已完成任务表。每批从本地真实函数手工适配，产出可独立审阅的diff与验证记录；前置批未通过不进入依赖阶段。图片/scoped即使代码完成也保持默认关闭。实现期间若HEAD、迁移或线上已保存配置变化，先刷新取证与差异评估，不重新生成保护hash来掩盖变化。

| 批次 | 内容与对应项 | 依赖及交付物 | 验收与失败处理 |
|---|---|---|---|
| B0 | 锁定当前HEAD/20冻结文件/历史SQL；O01保护契约、O04工具链精确pin、O02验证框架与O05 secret-scan前置 | 基线manifest、工具链对照、隔离PostgreSQL/Redis与mock服务、快速/完整CI入口、扫描fixture；不接生产 | checker反例、关键集成服务缺失不可假绿、锁不漂移；工具链或保护基线未确认则不编码依赖变更 |
| B1 | U01依赖安全、U02分页、U03管理员400、U04上传 | B0；最小module/sum差异、安全业务错误、上传上限拒绝及边界用例 | T01–T03、既有媒体profile/timeout与管理鉴权不变；失败定位具体hunk，不退回截断/估算/宽泛吞错 |
| B2 | U05/U06/U07/U08/U09/U10/U11/U15/U20/U21：协议、Terra、默认价卡、usage、Gemini目录/绑定/重试、去重复查、国产429与定向角色 | B1；本地文件/函数映射、Go/Rust共用golden、真实usage与展示usage对照 | T04–T08/T10/T14/T18；U21改变受保护缓存seed则不纳入该hunk；无法在冻结文件外正确限制工具则阻断发布，不能改保护区 |
| B3 | U13图片255、分组字段、DTO/repo/auth snapshot v24、实际计数与价格优先级 | B2；新增兼容迁移、生成代码最小diff、默认false贯通及新旧binary测试 | T03/T07/T16；raw provider产出与交付计数分离，未知不补1，少返回不按n多收；混合文本image tool与关闭路径无变化，失败只关新能力不改历史账 |
| B4 | U12 Grok CAS/手动同步/只读诊断；U14 Channel；U19 Plaza；O06/O08/O09管理前端 | B2，图片相关UI依赖B3；窄字段更新、受管快照保护、取消/generation/draft revision与中文/英文文案 | T02/T09/T16；只读GET零外部请求/写入，晚到不覆草稿，亮暗色与移动/键盘验收；禁止新增推理触发后台观测任务 |
| B5 | U17 scoped配置/gate=false、256表与稳定key身份/操作记录、生命周期、摘要/撤销/revision/IP、catalog基础 | B0/B4；配置、兼容schema、JWT+原step-up生命周期API、惰性仓库/任务、独立受限Principal | S01/S04/S07/S10；无新表且gate=false仍可启动，legacy任意已保存有效值/JWT/WS优先级保留；只完成基础不启用功能 |
| B6 | U17全受限路由/字段/资源范围、事务重验、稳定scope幂等/同库原子操作记录、DTO投影、key限流、显式代理模式、设置UI；O07 | B5；逐method+route+权限+字段+DTO+测试映射表，已有Tx/OnCommit复用与S01–S11结果 | T19/S01–S11；改权限/轮换与崩溃不得重复执行，禁止TOTP绕过/保护字段写入；未完整适配的route不进catalog，存在缺项不称U17完成、不放量 |
| B7 | O03发布统一immutable SHA/输入校验/锁只读/manifest，完成O01–O09全部落点与依赖复核 | B1–B6；release workflow离线验证、amd64/arm64产物来源、精确工具链/锁hash；版本拟v0.2.19-nuro.1与0.2.19-nuro.1 | T20；分支与tag不同也只构建同一目标SHA，旧前端artifact拒绝，禁止go mod tidy暗改发布依赖；版本仅在准备实施产物时更新，不照搬上游旧VERSION |
| B8 | 完整Go/Rust/前端/Redis/SQL/race、跨语言恢复、性能资源与前端视觉；逐hunk代码审查 | B7；T01–T20、S01–S11、O01–O09矩阵及命令/输出/测试数/skip/源码SHA；三轮同机对比 | 所有Q硬门禁；保护hash不变仍须验证调用者语义。任何红项/关键skip/未查明资源增长都阻断发布，修复后重跑受影响全链路 |
| B9 | 隔离staging、离线shadow、故障恢复/回滚演练与独立开关canary | B8；Q规定证据、原env/容量与每副本版本、旧镜像/静态资源、session排空顺序、迁移兼容报告 | 未来进入发布操作后才执行，不属于本轮方案授权；1%→5%→20%→50%→100%须真实样本/跨峰谷，异常停量关新开关，保留表/账本/WAL，不重放unknown |

### 1. 实施必须交付的完整材料

1. **范围台账**：U01–U21逐hunk结论，所有71条commit/259文件来源可追溯；每个排除项写明保护冲突或本地已满足证据。
2. **保护证明**：20文件SHA-256零差异、所有历史SQL内容校验、保护字段payload/golden、cache key/稳定前缀/sticky/24h、Go/Rust ABCD与markup及真实usage账务对照。
3. **实现清单**：每批实际变更路径/函数、schema/DTO/config/UI链路、新旧默认、取消/重试/回滚语义；scoped逐路由与字段授权表不得空缺。
4. **运行验证**：精确命令、环境、依赖锁、源码SHA、通过/失败/skip数量，跨语言真实Edge测试、DB事务/撤权/崩溃注入、TTFT与资源数据；不得拿历史日志或单测库存冒充本次通过。
5. **前端验收**：Grok诊断、图片分组开关、scoped卡片的亮暗色/320、375、768、1280宽度/200%缩放截图及键盘流程；沿现有设计系统，secret不持久化。
6. **产物与回滚**：拟版本v0.2.19-nuro.1、各架构二进制/Edge/embedded前端同一releaseSHA、manifest与hash、迁移兼容、旧产物与原saved settings、drain/关gate/回滚演练记录。

### 2. 风险关闭责任与停止条件

| 风险 | 具体防线 | 对应批次/门禁 |
|---|---|---|
| 冻结文件未变但调用语义改变 | raw旁路清单、输入/输出golden、关闭功能no-op和pre-TTFT调用计数；不能只依靠hash | B0/B2/B8；T06/T10–T18 |
| 真实usage被展示/图片数覆盖，新增价卡误覆盖自定义价 | raw usage先保存、交付计数独立、价格来源显式、tier/区间边界与已保存override回归 | B2/B3/B8；T03/T06/T07/T14 |
| 新管理key扩大权限、继承管理员豁免或跨key泄漏 | 精确catalog+默认拒绝、SQL范围、保护字段拒绝、机器key标识、JWT生命周期、key独立限流和当前权限投影 | B5/B6；S01–S09 |
| 幂等scope碰撞或权限revision变化造成二次执行 | 实际存储scope按稳定keyID分隔；授权revision与操作身份分开；业务同事务唯一记录与崩溃恢复 | B6；S05/S06，失败不开放非幂等路由 |
| 前端晚到覆盖草稿或保存清空二开配置 | AbortController/generation/draft revision，省略/false/0/null/明确清除fixture，旧响应finally不清新loading | B3/B4/B6；T09/T16/O06/O08 |
| CI假绿、混编前后端、工具链/依赖漂移 | required完整job与关键skip失败、统一SHA、frozen/locked、构建后锁校验、产物manifest | B0/B7/B8；T01/T20 |
| 新旧副本混跑影响session/账务/新能力 | schema先行且gate=false，全副本就绪、session黏性/drain、保留WAL/lease/旧账、不drop回滚 | B3/B5/B9；T12/T13/S10/Q |

不能保证任何改动“绝对零风险”。本方案将已知风险具体化为实现约束和阻断门禁；尚未运行的测试、生产schema、真实副本配置及视觉/性能结果，必须在对应阶段取得证据后才允许关闭。若实现目的与保护要求无法同时满足，保护要求优先，明确记录被阻断的hunk，不能暗改保护区或把缺项说成完成。
