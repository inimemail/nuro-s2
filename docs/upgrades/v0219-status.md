# v0.2.19-nuro.1 实施记录

记录日期：2026-10-29（Asia/Shanghai）。基线：`main` / `861d2874a5e87706465ab53170736b97c1e8ccdb`。保护清单：[v0219-protected.json](v0219-protected.json)。

**实现已落入未提交工作区；当前不满足完整发布门禁。** 后端 VERSION 与前端 package 版本均为 `0.2.19-nuro.1`。本记录区分代码实现、本地验证及真实发布，不把设计矩阵或测试库存当作全部通过。未连接生产数据库、未请求真实供应商模型、未部署、未提交或推送。

## 1. 实施范围

| 批次 | 状态 | 实际落点 |
|---|---|---|
| B0 基线/保护/工具链/CI | 已实现 | 20冻结文件及283份历史SQL校验；前端完整CI、Rust job、独立PostgreSQL/Redis/真实Rust fixture；关键skip与缺测阻断；离线秘密扫描 |
| B1 安全与边界 | 已实现并回归 | 最小Go依赖升级、分页溢出/输入边界、管理员保护400、OpenAI/Grok上传超限拒绝且不截断 |
| B2 协议/模型/usage/重试 | 已实现并回归 | Terra模型/别名/价卡/工具端点限制；DeepSeek cache-hit最后优先级；Gemini目录/绑定缓存/可配置重试；畸形工具错误；singleflight内复查；CN频率429；定向OpenCode角色兼容 |
| B3 图片计数/按张能力 | 已实现并回归，默认关闭 | 新255迁移、Ent/DTO/repo/auth snapshot字段贯通；实际图片观测、专用图片端点opt-in、价格显示对齐；混合token路径保留 |
| B4 Grok/管理前端/Channel/Plaza | 已实现并回归 | Grok受管目录CAS/只读诊断；来源及配置revision失效；同步取消/generation/draft fence；Channel搜索维度局部merge、Plaza价格优先级 |
| B5 scoped基础 | 已实现并回归，默认关闭 | 新256迁移、摘要存储/轮换/撤销/revision/IP、JWT生命周期与原step-up；legacy/JWT/管理WS隔离 |
| B6 scoped受限入口/UI | 已实现并回归，默认关闭 | 下列9个精确路由；SQL范围/窄投影；同库事务/操作记录/权限重验/独立限流；增量关联与提交后发布；显式代理模式；独立设置卡片 |
| B7 构建发布一致性/版本 | 代码与离线fixture已完成；真实发布未执行 | 统一不可变SHA、tag与前后端版本检查、frozen/locked、构建后锁校验、组件manifest及完整产物provenance；发布依赖同SHA完整CI与安全检查 |
| B8 验证/代码审查 | 本地自动验证及补审完成主要项；发布门禁仍未全通过 | 见测试证据和未满足门禁；新scoped/Grok组件视觉矩阵已验收，完整lint仍红，完整容量/TTFT及整站验收未完成 |
| B9 staging/canary/发布演练 | 未执行 | 属于独立发布操作；没有进行生产放量或修改线上环境 |

上游71个commit / 259个文件的逐项来源见 [v0219-upstream-classification.md](v0219-upstream-classification.md)。U16 UTF-16指纹改变稳定前缀，按方案明确排除；U18赞助资产/非必要布局/历史VERSION值未搬入。U08保留本地Pro计价规则。Rust生产源码、Cargo.toml/lock、部署拓扑与受保护实现不因本次升级重写。

### 本地优化 O01–O09

| 项目 | 结果 |
|---|---|
| O01 保护契约 | 现有checker扩展不可变基线、历史SQL内容、路径/符号链接检查与迁移冲突反例；未重算摘要来放行改动 |
| O02 CI覆盖 | 保留快速入口，新增完整前端/lint/typecheck/build、Rust debug/release/clippy和真实Go/Rust恢复；SQL/Redis关键测试必须实际执行 |
| O03 产物身份 | tag在临时fixture中验证branch≠tag；不存在/非法tag、版本不一致、旧组件、锁或产物变化均拒绝；发布入口使用同一SHA并等待验证job |
| O04 工具链 | Go1.27.3、Node24.13.0、pnpm9.15.9、Rust1.90.0显式pin，Node/Rust构建镜像multiarch digest已由官方registry实查并固定；govulncheck固定v1.9.0；本机pnpm10与Rust1.96的结果不冒充锁定工具链结果 |
| O05 秘密扫描 | 离线有界扫描及fixture；细粒度占位例外；安全CI真实调用，日志不输出匹配秘密 |
| O06 同步生命周期 | 两处同步及诊断支持abort/generation/unmount fence；草稿改动后仅显式合并；旧finally不能清新loading |
| O07 scoped列表 | 授权谓词直接进入SQL，count和分页同源、JSON窄投影，不加载全Account/Group DTO，不逐行查关联 |
| O08 保护字段契约 | 原账号/分组完整组件测试及新增SQL保护字段/关联整行保留用例实际执行；scoped不接受任意extra/credentials回写 |
| O09 视觉与可访问性 | 新scoped/Grok组件在隔离Chromium中完成320/375/768/1280真实viewport、亮暗色/中文英文、原生200%缩放、Tab/Shift+Tab/Esc和touch流程；axe无违规。仅组件fixture，未冒充整站/真机验收，详见补审证据 |

## 2. Scoped 精确路由与字段

所有入口保留公共panel limiter、audit与Gemini缓存权限屏障。未登记路由默认拒绝；不能借管理员角色继承全部管理API。

| Method + route | 必须的路由权限 | 允许输入/响应 |
|---|---|---|
| GET `/api/v1/admin/accounts` | `accounts.read` | SQL授权后的分页；仅id/name/platform/type/status，当前范围另有`accounts.models.read`才附model_mapping |
| GET `/api/v1/admin/accounts/:id` | `accounts.read` | 相同窄投影；无范围返回404 |
| POST `/api/v1/admin/accounts` | `accounts.create` | 仅API-key账号创建；字段另验下表权限；OAuth外部开户流程未开放 |
| PUT `/api/v1/admin/accounts/:id` | `accounts.update` | 仅白名单增量patch；不允许改platform/type或父/影子账号 |
| DELETE `/api/v1/admin/accounts/:id` | `accounts.delete` | 软删除及必要关系/测试计划清理，同事务outbox/操作记录；响应仅deleted |
| GET `/api/v1/admin/groups` | `groups.read` | SQL范围分页；仅id/name/platform/status |
| GET `/api/v1/admin/groups/:id` | `groups.read` | 相同窄投影 |
| PUT `/api/v1/admin/groups/:id` | `groups.update` | 仅name/description/models_list_config；保留其他分组定价/保护/调度字段 |
| GET `/api/v1/admin/proxies` | `proxies.read` | 仅id/name/status；不返回代理地址和认证信息 |

| 账号字段 | 额外权限与限制 |
|---|---|
| name / notes | `accounts.notes.write`；名称1–100字节、备注≤2000字节 |
| status | `accounts.status.write`；仅active/inactive，沿用既有账号编辑状态；拒绝disabled |
| concurrency / priority | `accounts.schedule.write`；1–10000 / 0–10000，不修改调度算法或方向 |
| credentials.model_mapping | `accounts.models.write`；≤500项且键/值≤256字节；{}显式清空 |
| credentials.api_key/access_token/refresh_token | `accounts.credentials.write`；校验账号类型；不提供读取secret的权限 |
| credentials.base_url | 同时需要credentials.write/network.write；限制URL结构；原转发网络安全约束继续适用 |
| proxy_mode / proxy_id | `accounts.network.write`，候选另验`proxies.read`范围；direct/specified/random/least_used只在保存事务内选择一次 |
| add_group_ids / remove_group_ids | `accounts.groups.write`且每个变化分组具有`groups.update`范围；≤200，保留未变化binding的priority/guard/created_at |

未知字段、null、任意extra及未开放credentials子字段原子拒绝；保护策略不能通过机器key回写。用户/余额/订阅/充值/导入导出/探测/刷新凭证任务等未登记动作保持关闭。生命周期API仅JWT管理员可用，写入继续经过原step-up；scoped始终按机器密钥标识，不能绕过二次验证。

每次资源操作在事务内重验key revision、启用/撤销/到期/IP/创建者有效性及当前资源范围。写操作要求8–128字符Idempotency-Key；去重身份按稳定keyID + method/fullroute + key摘要隔离，revision/secret轮换不重置操作身份。业务变更与最小receipt同库提交；重放重新授权/投影，不存敏感旧DTO。记录保留窗口为30天、每次成功写有界回收≤100条；不承诺无限历史去重。无通用coordinator降级执行分支。

代理候选须启用且未过期，最多1000个、稳定锁序；空池或查询/行读取错误显式失败，不悄悄退回第一个代理。least_used仅统计持久化关联，不读取推理热路径实时负载。账号outbox携带旧分组与新增分组，移除/删除后仍可刷新旧分组；回滚不发布，重放不重复发布。

分组授权中的ID与platform必须来自同一条rule，查询及写入重验一致，不跨rule拼接范围；无关资源维度不生成SQL参数。JWT管理密钥列表按ID降序cursor分页，每页最多200条，`before_id`必须为正整数，返回`items/next_before_id`并设置`Cache-Control: no-store`；界面支持继续加载及刷新重置，避免旧密钥被静默隐藏。

## 3. 默认值与运行边界

- `security.scoped_admin_api_keys_enabled=false`；环境变量名为`SECURITY_SCOPED_ADMIN_API_KEYS_ENABLED`。没有修改线上配置或将其启用。关闭时旧legacy值优先匹配、JWT与管理WS不查询新表；无新增后台维护任务。
- `groups.image_count_billing=false`。仅OpenAI/composite分组、已允许生图、实际专用图片端点且显式开启时生效；混合文本/image-tool仍走原token计费，不按请求n伪造实际产出。
- Gemini `gateway.max_same_account_retries_gemini=4`，即默认共5次尝试；范围0–10，0只尝试一次。只在原错误分支退避，支持ctx取消；绑定显式缓存的生成请求只在绑定账号内重试，不改缓存宿主健康。
- Grok目录只在管理端手动同步时保存；诊断GET仅读已存快照，无上游请求/写入。scope含已有账号配置updated_at revision；任何保存编辑均保守失效，避免同地址换凭证后继承旧目录。CAS只更新受管extra键，不推进配置revision，不覆盖倍率/缓存等配置。
- Gemini图片观测仅保留最多1024个有界指纹，重复累积快照去重，溢出不伪造真实数量。相同内容且没有稳定ID的重复增量仍有保守低估边界，不宣称所有供应商流均精确计数。真实usage/账务与下游展示隔离保持。
- 不改ABCD、下游缓存加成、强隔离、sticky/session/prompt cache key/24h、Cell/escrow/lease、重试未知执行边界、首Token阶段模板、响应头超时与原部署drain语义。hash证明仅覆盖冻结文件内容，语义仍以回归与发布实测为准。

## 4. 运行验证证据

原始日志位于本机`/private/tmp/nuro-v0219-*`，不将测试凭证或巨大日志纳入仓库。图片multipart两入口完整性修复后，已对最终Go源码重新运行完整unit/integration、媒体race、vet/embed及Linux双架构交叉编译，配置隔离PostgreSQL与真实Rust fixture；以下区分本轮新跑和经源码哈希复核后沿用的历史证据。

下表保留`final3`轮次的验证记录；之后的全部未提交改动复审另修复了Antigravity同步状态，并完整重跑前端及嵌入构建。当前工作区的验证身份和新旧证据划分见本节末尾“全部未提交改动复审”，不以旧前端产物代表当前产物。

| 验证 | 结果/日志 |
|---|---|
| 保护基线 | `python3 tools/check_upgrade_contracts.py`通过：20冻结文件、283历史迁移不变，无新增编号冲突，Go/Docker一致 |
| Go完整unit | 本轮最终源码`go test -count=1 -json -tags=unit ./...`：13355个test/subtest pass事件、51个package通过、无失败；5个test skip，原因逐项列于表后。日志`nuro-v0219-final3-unit.jsonl`，checker通过；包括新增图片multipart、scoped创建字段边界、Gemini预算、畸形工具usage/日志隐私及真实SQL/Rust测试 |
| Go完整integration | 本轮最终源码`go test -count=1 -json -tags=integration ./...`：8954个test/subtest pass事件、47个package通过、无失败；2个test skip（DingTalk disabled sentinel、未配置外部TLS capture）。日志`nuro-v0219-final3-integration.jsonl`；7个显式required关键测试全部实际通过，`nuro-v0219-final3-required.log` checker通过；2293个Go源码/配置文件与开跑前快照一致 |
| 真实fixture | PostgreSQL事务/迁移重复执行、Redis batch/Lua及三个真实Rust传输/WS恢复测试实际通过；checker显式require且拒绝关键skip，并新增require迁移契约测试。SQL收尾race：`nuro-v0219-sql-closeout.jsonl`；补审真实SQL与adminaccess/service race：`nuro-v0219-review-boundaries-final.log` |
| Race | 本轮最终源码媒体定向race：153个test/subtest pass、1包通过、0 skip、无失败，`nuro-v0219-final3-race.jsonl`。此前service/repository/middleware定向、adminaccess完整包、usage/parser、GrokCAS与代理异常race证据仍保留，见`nuro-v0219-race-final.log`、`nuro-v0219-parser-closeout.log`、`nuro-v0219-grok-closeout.log`及下文SQL边界记录 |
| Go vet/build | 本轮最终源码顺序运行`go vet ./...`及`GOFLAGS=-mod=readonly CGO_ENABLED=0 go build -tags embed -trimpath -o /private/tmp/nuro-v0219-final3-server ./cmd/server`，整体exit0，日志`nuro-v0219-final3-vet-embed.log`。前端dist哈希稳定，没有并发Vite重建；此前并发导致的旧资源缺失及顺序重跑证据保留在历史日志 |
| Linux交叉编译 | 本轮最终Go源码以CGO_ENABLED=0、embed、trimpath构建Linux amd64/arm64均exit0，`nuro-v0219-final3-cross-build.log`；两产物经file确认分别为x86-64和AArch64 ELF，二进制哈希写入最终快照。交叉编译不等于镜像运行或多架构发布验收 |
| 前端完整 | 上一轮以Node24.13.0及corepack pnpm9.15.9运行lint:check/typecheck/完整Vitest/生产build，全部通过，192个文件/1202个测试；日志`nuro-v0219-last-frontend-{lint,typecheck,tests,build}.log`。本轮前端未改，18个修改文件及全部embed dist与上一轮快照逐项相同，沿用同源结果，未冒称本轮重跑 |
| pnpm9隔离验证 | 临时副本中Node24.13.0 + corepack pnpm9.15.9执行frozen安装、lint/typecheck、191文件/1196测试、生产build全部通过；package.json/lock哈希与工作区相同。日志`nuro-v0219-pnpm9-verify.log` |
| Rust本机与生产基准 | fmt/check/243个debug测试/243个release测试/clippy全部通过；本机Rust1.96.1。日志`nuro-v0219-implementation-logs/rust-review.log`；官方Rust1.90.0-alpine隔离容器中locked debug/release也各243测试通过（`nuro-v0219-rust190-final.log`）；源码与共用Go/Rust价卡fixture均只读挂载，未改Cargo.lock |
| 安全 | govulncheck v1.9.0：代码可达0/导入包0，模块层另8项未发现可达调用；pnpm audit受控例外验证通过；离线secret scan通过，无高置信命中 |
| 工具/工作流 | 本轮Python工具29测试通过，包含实际CLI生成release inventory；保护/离线secret scan（3411文件，无高置信命中）及`git diff --check`通过，日志`nuro-v0219-final3-{tools,contracts,secrets,diff}.log`。工作流未改，沿用actionlint v1.7.7关闭外部shellcheck/pyflakes后的语义检查及发布配置check证据 |

最终unit/integration均配置了SQL/Rust fixture，关键测试没有skip；没有把无测试包的package skip计入test skip。unit的5个skip分别是DingTalk disabled sentinel、未显式开启网络测试的BasicConnection/JA3/AllProfiles三个TLS测试，以及SQLite schema不能构造历史NULL session行的legacy adoption测试；integration另跳过了未配置外部URL的TLS capture。上述边界保持未覆盖记录，未修改历史迁移来制造通过。诊断/secret一次展示/同步取消测试不依赖真实供应商，不声称验证了真实供应商模型可用性。

### 审查中已修复的问题

1. scoped更新/删除缺旧分组outbox，及失败/重放副作用：同事务记录affected group IDs并补真实SQL回归。
2. Grok使用错误平台的base URL getter、无成功快照的失败状态、同地址配置/凭证变化时旧结果可写入：改正来源、配置revision与有界CAS校验并补测试。
3. random/least_used查询行错误可能退回首代理：改为fail closed，补query/rows/empty/scan四类故障测试。
4. Redis batch用例原TODO永久skip、CI缺SQL/Rust fixture：恢复该既有测试并要求关键测试实际完成；未改并发生产实现。
5. legacy密钥恰符合scoped前缀、gate关闭、JWT/管理WS与机器key step-up隔离：加入真实中间件回归，原优先级保持。
6. 发布只解析SHA但未等待完整CI、secret scan未真正接入安全job、工具测试未全收集：补可复用同SHA门禁、扫描job与完整Python测试发现。
7. 新增cache-hit fallback不应让既有命中的usage多做一次查找：新增字段改为仅旧字段全部未命中时读取；WS原helper已经短路，账务优先级不变。
8. 暗色辅助文字对比度、Grok单项复制/取消、首次失败误写“保留成功目录”的文案：局部修复并跑完整前端回归。
9. scoped分组SQL漏用platform范围、无关rule遗留未使用参数、后续All rule丢弃前面参数：统一按rule构造授权谓词，补真实PostgreSQL读写及跨平台拒绝回归。
10. 新255迁移未校验默认关闭、新256迁移仅检查少数字段：增加列类型/长度/nullability/默认值、唯一约束/主键/外键/revision与RPM约束校验，异常明确拒绝；新迁移契约15个子用例，历史283份迁移未改。
11. 管理密钥列表超过200条后无法管理旧密钥：服务/handler/UI贯通cursor分页；真实SQL插入205条验证完整性、顺序及无重复，前端验证追加/刷新。
12. Gemini纯文本/usage-only响应也分配图片去重状态：延迟到首张实际图片出现才分配，增加文本路径无图片状态回归，不改流式保护实现。
13. scoped停用使用disabled与既有active/inactive表单不一致，后续编辑可能误恢复启用：改为active/inactive并拒绝disabled，补真实SQL往返及幂等/关联保留检查；不允许以composite分组平台创建真实供应商账号。
14. 新密钥弹窗焦点可能逃出、暗色辅助文字对比度不足：BaseDialog新增默认关闭的trapFocus，仅新密钥三个弹窗开启；补顶层弹窗/双向Tab循环/默认行为测试，修复暗色文字并实测axe。
15. 编辑密钥名称可能因本地时间/DST转换损失原到期秒或精度：日期按自身时区转换并显示秒，未修改到期字段时保留原始完整timestamp；微秒timestamp回归通过。
16. secret复制失败提示在弹窗后方、关闭后焦点不稳定：弹窗内显示安全错误并清理secret，显式恢复创建/轮换按钮焦点；真实浏览器验证确认前不可关闭、Esc返回及storage无secret。
17. scoped创建分组账号时，字段校验可能借用“未分组账号”的字段权限：创建只按最终目标分组校验，补真实PostgreSQL notes/credentials/schedule/network四类拒绝、账号和receipt均不落库回归；JWT/legacy路径不改。
18. Gemini签名兼容重试在0/1次预算耗尽时仍退避、再次读取已关闭响应：仅剩余尝试大于0时允许签名降级，最终响应保留给原错误处理。新增关闭后读取检测及实际请求次数断言，保留客户端thinking配置的原回归。
19. Antigravity畸形工具终态在拒绝部分成功内容时丢失真实usage：直接/包装JSON仅提取已解析usage，避免调用旧转换器的内容诊断日志；Messages/Chat/Responses聚合错误携带最近实际usage进入既有结算路径，不求和、不估算；null/空metadata不覆盖已观测值。Messages使用原Claude错误格式，其他兼容入口使用原OpenAI格式，均不下发部分内容。不改账务handler、重放策略或Rust；原handler继续将失败请求的TTFT清空并按失败分类。
20. GoReleaser镜像原只打包Go、没有Rust及成对启动脚本：完整/simple配置均包含对应架构Edge与既有paired脚本，补flock依赖；simple也构建amd64 Edge。固定GoReleaser为已验证的v2.14.0，并更新等价的archive formats配置。原paired脚本、drain流程和部署拓扑未改。
21. 发布旧tag后自动回写默认分支VERSION可能降版本，且仅改后端：删除该冗余回写job。发布仍强制tag与前后端版本匹配，不自动提交或推送源码。
22. 可选焦点约束遗漏弹窗容器自身已获焦的情况，Shift+Tab可能跳到弹窗外：仅在trapFocus已开启的原分支补容器判断，正向Tab到首项、反向到末项；新增两方向回归先红后绿。默认关闭、旧弹窗及CSS不变，实际仅新scoped三个弹窗启用。
23. 图片multipart完整性遗漏：Grok的缺失boundary、畸形Content-Type或后续part头损坏可能静默返回已解析内容，转换后发出只含部分图片的请求；OpenAI图片入口也会把某些截断MIME头的EOF当作正常结束。两个入口共用完整性reader，真正的结束必须来自closing delimiter；Grok转换直接传递解析错误，畸形请求不发送上游。新增回归先红后绿，覆盖7种损坏输入，以及LF/CRLF、无末尾换行、preamble/epilogue、边界空白的7种合法形式；JSON与空体视频查询保持原行为。仅改图片multipart解析，不改缓存/sticky/session、调度、账务、重试边界或Rust实现。
24. Antigravity同步的待确认能力信息可能跨次串用：第一次结果因草稿编辑而待确认后，再次同步自动应用时可能用旧metadata覆盖新结果；无metadata的新结果显式应用后也可能留下旧能力标签。每次开始同步清理待确认metadata，自动/手动应用均使用对应结果的metadata（包括空对象），并保留用户已编辑的模型映射。新增两个组件行为回归先红后绿，仅修复前端状态，不改后端或受保护实现。

### 扩大范围复审证据

本轮覆盖此次全部改动及直接依赖：scoped鉴权/SQL事务/幂等/审计与前端生命周期，图片计数/计费与模型协议，迁移、CI、两份发布配置及镜像启动。针对上述新缺陷先运行失败回归、再修改生产代码；没有修改冻结清单摘要来放行改动。

- scoped创建权限先红后绿：`nuro-v0219-wide-scope-before.log`、`nuro-v0219-wide-scoped.log`，真实PostgreSQL及定向race通过。
- Gemini预算边界先红后绿：`nuro-v0219-wide-signature-before.log`、`nuro-v0219-wide-signature-after.log`，0/1次重试及原thinking兼容定向race通过。
- 畸形工具usage先红后绿：`nuro-v0219-wide-malformed-before.log`、`nuro-v0219-wide-malformed-after.log`；内容日志隐私回归先红`nuro-v0219-wide-malformed-log-before.log`。最终扩展回归`nuro-v0219-wide-malformed-final.log`覆盖三入口、前帧/末帧/重复快照/null/空/缺失usage、错误协议、部分内容拒绝及日志不泄露，定向race通过。Antigravity包另完整race通过。
- GoReleaser v2.14.0对完整/simple两份实际配置执行check，均exit0：`nuro-v0219-wide-goreleaser-check-final.log`。保留的legacy Docker pipeline仍有弃用提示；未借此更换镜像拓扑或输出格式。
- 使用锁定Rust1.90.0官方容器离线编译真实Linux arm64 Edge，与最终源码的Linux arm64 Go及发布Dockerfile实际构建本地镜像；已验证两二进制、PostgreSQL18工具、非root成对进程、WAL flock锁、Rust健康端点及Go配置向导可用，TERM退出0。日志`nuro-v0219-wide-complete-packaging-build.log`、`nuro-v0219-wide-complete-packaging-smoke.log`。隔离启动为未配置的setup模式，不能据此声称生产数据库、正式Go健康检查或完整drain已验收。未publish/push镜像；临时容器及本地测试镜像在验证后清理。
- 扩大复审阶段前端未改；逐文件SHA-256与当时192文件/1200测试、lint/typecheck/build及组件浏览器验收的源码一致，沿用该验证记录。随后最后一轮复审另有焦点修复及完整重跑，见下文。

收尾完整Go测试、vet/embed构建、lint对照与工具门禁证据以`nuro-v0219-wide-complete-*`日志及`nuro-v0219-wide-verification-summary.json`为准；完整测试前后逐文件复核Go源码哈希，避免把中间态的通过结果算到最终源码上。旧`nuro-v0219-review-final-verification-summary.json`及本轮中间态日志不再代表最终源码。

### 本轮隔离浏览器证据

使用实际新scoped/Grok组件和现有样式建立临时fixture，API与Clipboard使用mock；只绑定127.0.0.1，阻断其他HTTP地址，独立Chromium profile，不读取用户浏览器数据、不连接真实后端。证据在`/private/tmp/nuro-v0219-browser-review/`，harness未加入项目。

- `layout.json`：16组（四宽度×两主题×两语言），卡片/编辑表单/Grok诊断无页面横向溢出，键盘循环和Esc焦点恢复通过，无运行时错误。
- `zoom-a11y.json`：8组（四宽度×两主题），Chrome原生zoom读回2且innerWidth减半；卡片/表单无根横向溢出，axe WCAG2A/AA/2.1AA违规为0。
- `flows.json`：8组（320/1280×两主题×两语言），touch/Enter创建、secret确认前不可关闭、确认后清理、创建/轮换焦点返回全部通过；storage无secret，axe违规及运行时错误为0。
- 已人工查看320暗色和1280亮色表单截图。该证据仅覆盖新组件fixture，不代表整站、实际GroupsView图片分组完整页、真实设备或真实供应商流程已验收。

### 上一轮焦点补审证据（历史快照）

2026-10-29再次复核权限重验/创建字段范围、幂等重放、关联增量修改、生命周期与限流；图片计数跨尝试状态、默认关闭和原兼容边界；CN频率证据、Grok CAS、Channel局部merge、OpenCode角色兼容；模型同步取消/草稿隔离及发布门禁。本轮开始时所有改动文件均与扩大复审快照一致，本轮项目内只新增上述第22项修复、两个回归用例及本记录；没有借审查扩大重构或改动冻结摘要。

- 新焦点回归：`nuro-v0219-last-dialog-before.log`中容器正反向Tab用例均失败，最小修复后`nuro-v0219-last-dialog-after.log`的5项测试全部通过，包含默认关闭及仅顶层弹窗生效。
- 完整前端：lint:check、typecheck、192文件/1202测试及生产build全部exit0；698个源文件/配置的验证前后哈希一致，使用Node24.13.0 / pnpm9.15.9。之后顺序重跑`go vet ./...`及`CGO_ENABLED=0 go build -tags embed -trimpath`，exit0，日志`nuro-v0219-last-vet-embed.log`。
- 真实Chromium补验：在已有隔离mock fixture中检查新scoped编辑/确认/一次性secret三个弹窗，320/1280 × 亮暗色 × 中文英文，共24组；真实键盘验证容器正反向Tab、首尾双向循环、关闭后焦点恢复及storage无secret，运行时错误为0。证据`nuro-v0219-browser-review/last-focus.json`、`nuro-v0219-last-browser-focus.log`。仅组件fixture，不外推整站或真机验收。
- 真实PostgreSQL与定向race：adminaccess/repository/service/middleware四包，94个test/subtest pass、0 skip；scoped事务及迁移契约均实际执行，required checker通过。日志`nuro-v0219-last-boundaries.jsonl`、`nuro-v0219-last-required.log`；2290个Go源文件/配置与开跑时一致。
- Go完整unit/integration、Rust、lint对照、发布配置及镜像的上轮证据按源码和日志SHA-256复核后沿用，本轮不冒称再次全量执行。完整Go仍为13310/8909个pass；原5/2个skip与未满足门禁继续保留。前端变更后的嵌入式构建已重新完成。
- 最终保护、29个Python工具测试、离线secret scan及diff检查通过，日志`nuro-v0219-last-{contracts,tools,secrets,diff}.log`。本轮自建PostgreSQL与本地fixture服务已停止；未提交、推送或部署。

该轮历史快照为`/private/tmp/nuro-v0219-last-verification-summary.json`，记录当时改动文件、证据日志及二进制SHA-256，并区分新跑与同源沿用。下面的multipart补充终审及`final3`快照取代其作为当前工作区身份记录；历史快照不删除，也不替代尚未通过的发布门禁。

### 图片上传完整性补充终审（历史快照）

2026-10-29再次逐文件复核，此轮开始时全部改动与上述历史快照一致。复核图片上传错误传播时确认第23项缺陷：损坏的后续MIME头可能被当作正常EOF。针对Grok先增加失败回归，并通过OpenAI公共parser确认同类问题，再对两个入口做最小修复；没有进入文本热路径、缓存/sticky/session、调度、账务、Rust或重试实现。

- 先红证据分别为`nuro-v0219-final2-multipart-before.log`和`nuro-v0219-final2-openai-multipart-before.log`；最终定向回归`nuro-v0219-final2-multipart-final.log`通过，共45个test/subtest pass事件。覆盖7类损坏输入及7类合法multipart，两入口均校验；Grok畸形请求断言未调用mock上游。JSON及空体视频查询兼容回归通过，无真实供应商请求。
- 最终Go源码共2293文件的哈希快照为`nuro-v0219-final3-go-source.json`。全量unit 13355 pass/51包/5 skip、integration 8954 pass/47包/2 skip、媒体race 153 pass/0 skip均无失败；原skip原因保留，没有改断言或增加skip来放行。7个required SQL/Redis/真实本机Rust fixture测试均实际执行。
- 中间态第一次integration在外部`tls.peet.ws`探测时握手EOF，原始失败保留于`nuro-v0219-final2-integration-attempt1.jsonl`；当前TLS包独立重跑、与原HEAD同源的TLS基线对照及后续完整integration均通过，见`nuro-v0219-final2-{tls-retry,tls-baseline,integration}.jsonl`。没有改TLS实现、测试断言或skip；最终两入口补丁后的完整`final3` integration再次通过，外部探测波动仍如实记录。
- 最终vet/embed、Linux amd64/arm64交叉编译均通过；前端源码和embed dist与上一轮一致，沿用1202测试和24组弹窗键盘检查。本轮没有重跑前端全量、锁定Rust容器构建或新Go二进制的镜像启动；此前arm64镜像smoke仅为历史证据，不外推为本轮产物验收。
- 最终完整golangci-lint v2.14.0仍exit1：基线565、当前563，新增0、减少2；日志`nuro-v0219-final3-lint.{json,log}`及`nuro-v0219-final3-lint-delta.json`，保持发布阻断。29个Python工具测试、20文件/283历史迁移保护及离线secret scan通过；本轮自建PostgreSQL已停止并移除，见`nuro-v0219-final3-cleanup.log`。

该轮历史快照为`/private/tmp/nuro-v0219-final3-verification-summary.json`：记录当时全部150个改动文件、日志及三份Go二进制SHA-256，区分当轮重跑与同源沿用。相对其上一轮快照仅两处解析入口、共用reader、新回归文件及本记录共5个文件变化；冻结清单自身、前端、版本和HEAD当时未变。以下复审快照取代其作为当前工作区身份记录；历史证据和未通过门禁继续保留。

### 全部未提交改动复审（当前工作区）

2026-10-29按全部已跟踪差异和未跟踪文件逐项复核，涵盖模型/usage/图片兼容、管理接口和scoped事务、迁移及Ent贯通、前端同步生命周期与草稿保护、密钥展示/弹窗、CI/构建/发布工具及测试断言变化。进一步核对共享usage/helper与保护代码的调用关系、缓存字段优先级和新能力关闭分支。相对`final3`仅新增第24项前端修复、两个组件回归及本记录；未扩大后端改动或重算保护摘要。

- 新缺陷先红后绿：`nuro-v0219-all-uncommitted-sync-before.log`中两个新增用例均实际失败，修复后`nuro-v0219-all-uncommitted-sync-after.log`中编辑弹窗75项和同步生命周期6项全部通过。断言最新能力替换旧能力、空metadata移除旧标签，以及编辑后的alias保留；无真实供应商请求。
- 前端完整重跑：Node24.13.0 / corepack pnpm9.15.9，lint:check、typecheck、192文件/1204测试及生产build均exit0，日志`nuro-v0219-all-uncommitted-frontend-{lint,typecheck,tests,build}.log`；701份源码/配置的验证前后SHA-256一致。此次未改样式或scoped弹窗，原真实Chromium组件fixture证据保留，不外推为整站/真机验收。
- 新前端资源构建完成后顺序重跑Go vet/embed，并重新交叉编译Linux amd64/arm64，均exit0，日志`nuro-v0219-all-uncommitted-{vet-embed,cross-build}.log`。当前三份产物另存SHA-256，不以旧`final3`二进制代表新嵌入内容；未执行这些新产物的镜像运行或实际发布。
- 全部2293份Go源码/配置与`nuro-v0219-final3-go-source.json`逐项一致，旧证据日志哈希复核一致，因此沿用该源码的完整unit 13355 pass/51包/5 skip、integration 8954 pass/47包/2 skip、媒体race 153 pass/0 skip及required关键fixture证据，不冒称本轮全量重跑。Rust源码/Cargo和发布配置未改，沿用此前明确记录的验证范围。
- 保护契约20冻结文件/283历史迁移、29项Python工具测试、离线secret scan（3411文件，无高置信命中）及`git diff --check`通过，日志`nuro-v0219-all-uncommitted-{contracts,tools,secrets,diff}.log`。完整Go lint仍为563项既有问题、相对原HEAD新增0，继续阻断发布；容量/TTFT、整站/真机、多架构真实发布和staging/canary/回滚演练仍未验收，两项新能力保持默认关闭。

当前快照为`/private/tmp/nuro-v0219-all-uncommitted-verification-summary.json`：记录全部151个未提交文件、证据及新构建产物SHA-256，区分本轮新跑与同源沿用；相对`final3`只有编辑弹窗、其测试及本记录3个文件变化。HEAD与前后端版本未变，未提交、推送或部署，未连接生产数据库或真实供应商，未启动新的服务/容器；`release_ready=false`。

### 追加复审（同一工作区源码）

2026-10-29再次复核scoped权限重验/幂等事务/代理选择、模型同步草稿及metadata、图片计费关闭分支、Gemini取消与重试预算、Antigravity畸形工具调用的usage保留，以及新增迁移与CI门禁。未发现新的确定缺陷，本轮未修改生产代码或测试。复审前151个未提交文件、2293份Go源码/配置、701份前端源码/配置、195份嵌入资源、上轮日志及三份二进制逐项SHA-256一致，见`nuro-v0219-recheck-source-identity.log`。

- 新跑定向race回归共129个test/subtest pass、0 fail、0 skip（主批107项、畸形工具usage补充22项），覆盖scoped认证/限流/规则、图片两入口的损坏与合法multipart、计费开关、缓存契约、重试预算和已有sticky缓存计费；日志`nuro-v0219-recheck-{go-race,malformed-race}.jsonl`。只使用本地测试替身，无生产数据库或真实供应商请求；scoped SQL事务仍沿用同源的上一轮实际数据库测试证据。
- 新跑前端6文件/110测试全部通过，包括编辑弹窗、同步生命周期、模型白名单/渠道同步、管理密钥及BaseDialog；29项Python工具测试、20冻结文件/283历史迁移保护、3411文件离线secret scan及diff检查通过，日志前缀`nuro-v0219-recheck-`。
- 源码未变，完整前端1204测试/lint/typecheck/build、后端全量unit/integration、vet/embed及交叉编译沿用上节哈希已核对的证据，不冒称本轮全量重跑。本轮仅追加本记录；新快照`/private/tmp/nuro-v0219-recheck-verification-summary.json`记录对应文件与日志哈希。完整Go lint既有563项失败及其他未验收门禁继续保留，`release_ready=false`；未提交、推送、部署或启动服务/容器。

## 5. 尚未满足的发布门禁

本机Apple M4/Go1.27.3做了三个局部微基准、每项三次（500ms），原HEAD临时archive与当前工作区分别运行：

| 微基准 | 基线/当前中位数ns/op | 基线/当前内存分配 |
|---|---|---|
| ExtractCacheableContent_System | 349.0 / 348.3 | 同为496 B、5 allocs |
| GeminiCacheReferencePlain | 40.88 / 40.24 | 同为0 B、0 allocs |
| OpenAIUsageExtractOptimized | 2046 / 2052 | 同为96 B、1 alloc |

日志`nuro-v0219-bench-baseline-final.log`与`nuro-v0219-bench-current-final.log`。这只是局部smoke对照；三次采样且未隔离整机其他活动，不具备统计显著性，也不证明真实TTFT、容量或p99不退化。

**完整golangci-lint仍失败，未放宽规则或加入大范围排除。** 本轮最终源码以同一v2.14.0、相同配置、取消数量截断重新运行，对照`git archive`导出的原HEAD：基线565项，当前563项；按文件/linter/消息/源码行多重集合对比，新增0、减少2。原始对照：`nuro-v0219-lint-baseline-uncapped.json`、`nuro-v0219-final3-lint.json`，差异记录`nuro-v0219-final3-lint-delta.json`。包括errcheck/unused/staticcheck/depguard/gosec/govet及旧格式问题，部分触及冻结区；本轮没有擅改保护代码或把红门禁改成只检新增。发布workflow会因此被阻断。

以下也没有验收通过，不能将“已实现”说成“已可上线”：

- 完整128/256/512及更高并发阶梯、三轮TTFT p50/p95/p99/错误率/FD/RSS/GC/队列/WAL/取消后资源恢复对照。局部微基准不替代此门禁。
- 整站及实际GroupsView图片分组完整页的设备/视觉验收、真实设备触屏与真实后端流程。新scoped/Grok组件fixture的四宽度、原生200%缩放、键盘/touch/axe已补验收，不能据此外推整站通过。
- Linux多架构Go/Edge/镜像全产物构建、镜像digest与完整provenance的真实发布演练。历史轮次补验了本地Linux arm64镜像构建与setup模式成对启动，本轮最终Go仅补验amd64/arm64交叉编译；这些结果以及离线manifest/branch≠tag测试和actionlint通过，不能代替多架构GitHub Actions真正执行。
- 新旧二进制/schema/gate组合、全副本配置/多副本故障、staging/canary、真实供应商兼容与完整回滚/drain演练。

## 6. 启用与回滚约束

未来发布前必须先处理或明确解决上面的红门禁，并保存同一源码的验证证据。schema先行，两项可选能力保持关闭；仅在全副本就绪且独立验收后先只读scoped key、再窄写测试资源，图片按张独立开启。关闭scoped gate不删除密钥/receipt/audit；关闭图片开关不回写历史账务。

异常时停止放量、关闭新可选能力，遵循原Edge先排空、Go后退出及session黏性，回到已验证旧镜像/前端/原saved settings。保留255/256新增兼容schema、历史账务、WAL及待结算lease；不drop表、不全清Redis、不回拨generation、不重放unknown请求。生产迁移、回滚与发布须作为独立操作执行。
