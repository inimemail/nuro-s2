# v0.2.18 → v0.2.19 完整上游分类附录

> 本文保留实施前的设计与取证快照；其中“尚未实施/拟新增/当前版本”描述对应方案编写时点。2026-10-29 的实际改动、验证结果及尚未通过的发布门禁以 [v0219-status.md](v0219-status.md) 为准。

来源：两个tag的immutable tree，不是本地缺失文件清单。扩展版已将建议项纳入：U12诊断UI、U09配置重试、U17独立scoped阶段；保护冲突hunk仍排除。151 M + 108 A = 259；没有D/R。分类对象为业务hunk，同一文件可包含入选与排除功能，禁止按文件整份复制。

## 功能索引

| 编号 | 分类 | 业务目的 | 本地执行边界 |
|---|---|---|---|
| U01 | 必须同步 | Go 依赖安全更新 | 保留本地 Go/前端/Rust依赖；仅升级所列 Go 模块 |
| U02 | 必须同步 | 分页整数安全 | 在本地现存分页入口、服务和仓库适配，不复制拆分文件 |
| U03 | 必须同步 | 管理员保护返回400 | 本地 admin_service.go；不改变权限或TOTP |
| U04 | 手工适配 | multipart拒绝截断 | 限制+1读取并明确拒绝；保留媒体profile及资源上限 |
| U05 | 手工适配 | Antigravity畸形工具调用 | 转换层与本地聚合器适配；保留usage与错误脱敏 |
| U06 | 手工适配 | GPT-6.1 Terra模型/协议/价格 | 通过冻结raw文件外的wrapper完成工具约束；价格走现有plan |
| U07 | 手工适配 | DeepSeek缓存读取usage | 补本地OpenAI parser；保留已有Anthropic兼容解析和原始usage |
| U08 | 本地实现保留 | DeepSeek Pro不切Flash价 | 本地已无日期切价；另列本地Flash默认价差，不复制上游计费链 |
| U09 | 手工适配 | Gemini同账号可配置重试 | 默认4次重试/共5次；取消感知和有界签名阶段；OpenAI预算不变 |
| U10 | 手工适配 | Gemini绑定缓存重试 | 只接本地owner/绑定账号请求路径；不移植上游缓存存储 |
| U11 | 手工适配 | Gemini默认目录/声明模型 | 保留本地custom list、强制平台、白名单、安全路径优先级 |
| U12 | 手工适配（含诊断UI，已纳入） | Grok目录/审计/诊断 | 核心解析、CAS与管理同步；只读诊断UI已纳入；禁止新增推理请求触发的后台任务 |
| U13 | 手工适配 | 图片计数/可选按张计费 | 新开关默认false；仅专用图片端点；迁移重编号；保留混合token |
| U14 | 手工适配 | Channel搜索维度保留 | 局部merge和显式清除；保留本地time_pricing等字段 |
| U15 | 必须同步 | 模型列表singleflight内复查 | 落本地refreshCachedAPIKeyCodexModelsManifest，不扩缓存key |
| U16 | 冲突，本轮不改 | Claude UTF-16指纹 | 会改变system归因block；稳定前缀保护优先，仅设计测试 |
| U17 | 手工适配（独立阶段，默认关闭） | scoped管理员API密钥 | 完整权限/范围/事务/幂等投影/限流/UI；复用本地Tx；保留legacy与TOTP，拒绝保护字段 |
| U18 | 不同步 | 赞助、文档及非必要布局 | 品牌资产、纯布局、历史VERSION提交不搬入 |
| U19 | 手工适配 | 模型广场价格显示优先级 | 落本地model_plaza.go，保留冷路径快照架构 |
| U20 | 手工适配 | 国产Coding Plan频率型429 | 本地handle429/ratelimit_cn_quota.go；缺失/无效/近耗尽保守长冷却 |
| U21 | 手工适配 | OpenCode DeepSeek角色兼容 | 本地strictChatCompatibility明确识别映射后模型与官方目的地；不放宽其他平台 |

## 全部71条commit

SHA完整值见inventory.json；下列12位前缀已核对唯一。merge只作为来源关系，不是可执行合并指令。

| Commit | 类型 | 上游原始标题 | 分类/功能 |
|---|---|---|---|
| 5db2d6956ba6 | 变更 | fix(billing): retain DeepSeek V4 Pro pricing after September 14 | U08 本地实现保留 |
| 3f4e13aaba9b | merge | Merge upstream main and resolve pricing comment conflict | 来源merge；不执行，不额外重复计入功能 |
| de908ed6d293 | 变更 | fix(ratelimit): 国产 Coding Plan 的频率型 429 改用秒级冷却 | U20 手工适配 |
| 4197fa62021c | 变更 | feat(gemini): 同账号重试次数改为可配置 gateway.max_same_account_retries_gemini | U09 手工适配 |
| f9652804894e | 变更 | fix(pagination): keep page offsets from overflowing int | U02 必须同步 |
| 9aa16c8c6632 | 变更 | fix(pagination): validate numeric ranges and reuse safe offsets | U02 必须同步 |
| 5c185c5c5d4f | 变更 | chore: sync VERSION to 0.2.18 [skip ci] | U18 不同步 |
| e445cc3e2d9c | 变更 | 新增按操作与资源范围授权的管理员 API 密钥 | U17 手工适配（独立阶段，默认关闭） |
| 37067e8e340e | merge | Merge pull request #8319 from lyen1688/feat/scoped-admin-api-keys | 来源merge；不执行，不额外重复计入功能 |
| fd998129ec83 | 变更 | feat(billing): support per-image billing for multi-image requests | U13 手工适配 |
| 1487fa511bff | 变更 | fix(billing): count only images returned in upstream response | U13 手工适配 |
| cb2959245f7a | 变更 | test(api): include image_count_billing in available groups contract | U13 手工适配 |
| aa1a1b11c864 | 变更 | fix(billing): gate per-image billing by endpoint and pricing mode | U13 手工适配 |
| 917dbdc705d6 | 变更 | fix(gemini): count fileData image outputs for billing | U13 手工适配 |
| 014cc016f9a0 | 变更 | fix(grok): 补全原生 Responses 响应模型审计 | U12 手工适配（含诊断UI，已纳入） |
| 7131b997ca0b | 变更 | fix(grok): 补齐聚合路径跨事件模型冲突 | U12 手工适配（含诊断UI，已纳入） |
| d01f2eb42cf4 | 变更 | fix(grok): 固定聚合前序模型审计并跳过非 Grok | U12 手工适配（含诊断UI，已纳入） |
| 6202da0f53b0 | 变更 | fix(grok): 统一模型目录解析与观测状态 | U12 手工适配（含诊断UI，已纳入） |
| 87266a7b1810 | 变更 | fix(grok): 原子保存模型目录观测快照 | U12 手工适配（含诊断UI，已纳入） |
| b1cb2eb1f30d | 变更 | fix(grok): 补齐取消与调用方事务的回归断言 | U12 手工适配（含诊断UI，已纳入） |
| 41a6ff8ac120 | 变更 | fix(grok): 对齐手动同步与后台目录观察 | U12 手工适配（含诊断UI，已纳入） |
| 635b8353508e | 变更 | fix(grok): 让旧后台观察测试走 CAS 持久化 | U12 手工适配（含诊断UI，已纳入） |
| d4136cea5e6c | 变更 | fix(grok): 提供账号模型目录差异诊断 | U12 手工适配（含诊断UI，已纳入） |
| bb11e83c44ab | 变更 | fix(grok): 裸别名无法核验且自定义 ID 保留大小写 | U12 手工适配（含诊断UI，已纳入） |
| 60931bce3114 | 变更 | fix(grok): 展示模型目录诊断并保留编辑草稿 | U12 手工适配（含诊断UI，已纳入） |
| ae5705da1d0b | 变更 | fix(grok): 同账号刷新不再卡住目录同步 | U12 手工适配（含诊断UI，已纳入） |
| 2db7ce3d25a9 | 变更 | fix(grok): 隔离过期目录同步并在切模式后刷新诊断 | U12 手工适配（含诊断UI，已纳入） |
| 02db748cd5a8 | 变更 | fix(grok): 保留受管目录快照并阻止非法时间与敏感失败日志 | U12 手工适配（含诊断UI，已纳入） |
| 3f1c8f36ce8c | 变更 | chore: update Sponsors | U18 不同步 |
| 0d374b25494d | 变更 | fix(grok): 精简目录诊断并按需展开详情 | U12 手工适配（含诊断UI，已纳入） |
| c68d31237299 | 变更 | fix(grok): 未知目录详情改为历史标题 | U12 手工适配（含诊断UI，已纳入） |
| 32c00d6e7213 | 变更 | fix(grok): 诊断文案不再读取继承属性 | U12 手工适配（含诊断UI，已纳入） |
| 21b5b49d0a5a | 变更 | fix(grok): 统一目录同步入口并保留模式隔离 | U12 手工适配（含诊断UI，已纳入） |
| 7f18239be2c0 | 变更 | fix(grok): 对齐模型编辑布局与响应式展示 | U12来源；纯布局/响应式重排本轮不同步；必要竞态修复另按hunk取用 |
| 9534f1c089d6 | 变更 | fix(grok): 提高删除图标对比度并补齐同步按钮 Label in Name | U12来源；诊断UI已纳入并保留必要可访问性断言；纯图标重排不引入 |
| 3ce09c8aff08 | 变更 | fix(billing): keep tokens for channel per-request image tiers | U13 手工适配 |
| ba81c7fd758e | merge | Merge pull request #8323 from wx-11/images-price | 来源merge；不执行，不额外重复计入功能 |
| dfc13ce7f9ff | 变更 | fix(grok): 修复模型目录审计相关 lint 错误 | U12 手工适配（含诊断UI，已纳入） |
| cde6ddfd8f41 | 变更 | fix(images): reject oversized multipart uploads instead of truncating them | U04 手工适配 |
| 3e18e78ab1d0 | 变更 | fix(usage): DeepSeek prompt_cache_hit_tokens 计为缓存读取 | U07 手工适配 |
| 88d9c43b3e9a | 变更 | fix(admin): 管理员保护规则返回 400 而不是 500 | U03 必须同步 |
| 14a5fe305061 | 变更 | fix(antigravity): reject malformed non-streaming tool calls | U05 手工适配 |
| 342366742238 | 变更 | feat(openai): support GPT-6.1 Terra | U06 手工适配 |
| d0afece15f9f | 变更 | fix(model-plaza): prefer group model pricing over image fallback tiers | U19 手工适配 |
| a20741237544 | 变更 | fix(gemini): 绑定显式缓存的请求在池模式上游网关账号上按配置同账号重试 | U10 手工适配 |
| b2b1bcf260b5 | 变更 | fix(models): 模型列表刷新进入 singleflight 后复查缓存，避免并发冷启动重复请求上游 | U15 必须同步 |
| ba5b21879430 | 变更 | fix(gateway): 修复 OpenCode 转发 DeepSeek 的角色兼容 | U21 手工适配 |
| 7bce2cfb45d8 | 变更 | fix(claude): match JavaScript UTF-16 billing fingerprint semantics | U16 冲突，本轮不改 |
| b332f169f9c6 | merge | Merge pull request #8364 from wucm667/feat/issue-8363-gpt61-terra | 来源merge；不执行，不额外重复计入功能 |
| 15701ba187af | merge | Merge pull request #8339 from aomsir/fix/grok-model-catalog-audit | 来源merge；不执行，不额外重复计入功能 |
| 0232cb7137b8 | merge | Merge pull request #8286 from coder-zhw/fix/cn-frequency-429-short-cooldown | 来源merge；不执行，不额外重复计入功能 |
| 3bd166babc30 | merge | Merge pull request #8377 from StarryKira/codex/fix-cc-version-utf16 | 来源merge；不执行，不额外重复计入功能 |
| bb021d330e20 | merge | Merge pull request #8326 from r266-tech/codex/gemini-file-images | 来源merge；不执行，不额外重复计入功能 |
| 3f1ae7ee76ae | merge | Merge pull request #8003 from Yukinoshita-Yukino/codex/fix-deepseek-pro-pricing | 来源merge；不执行，不额外重复计入功能 |
| 98bcee7a8934 | merge | Merge pull request #8366 from wx-11/fix/model-plaza-group-pricing | 来源merge；不执行，不额外重复计入功能 |
| 8e231ff5d66a | merge | Merge pull request #8354 from ychy7001/fix/deepseek-cache-hit-tokens | 来源merge；不执行，不额外重复计入功能 |
| af4462ebaef3 | merge | Merge pull request #8376 from fxjs/fix/opencode-deepseek-developer-role | 来源merge；不执行，不额外重复计入功能 |
| 2c9d9760d8d6 | merge | Merge pull request #8303 from SipengXie2024/fix/pagination-offset-overflow | 来源merge；不执行，不额外重复计入功能 |
| 449b65a2ed9a | merge | Merge pull request #8348 from SipengXie2024/fix/images-upload-part-limit | 来源merge；不执行，不额外重复计入功能 |
| f178e5ae3265 | merge | Merge pull request #8362 from lkon2112/fix/nonstream-malformed-function-call | 来源merge；不执行，不额外重复计入功能 |
| 9eba61736746 | 变更 | fix(channels): preserve web_search_emulation platform dimensions on update | U14 手工适配 |
| b2e7c6094307 | merge | Merge pull request #8352 from ychy7001/fix/admin-guard-400 | 来源merge；不执行，不额外重复计入功能 |
| 1352e899ecd4 | 变更 | test(channels): use checked type assertions in web search merge tests | U14 手工适配 |
| 0a2a29b32e67 | 变更 | chore(deps): bump golang.org/x/net to v0.62.0 (GO-2026-6860) | U01 必须同步 |
| de44e3a0bfd6 | 变更 | fix(gemini): 分组账号都声明模型映射时，/v1beta/models 列表与详情按声明的模型应答 | U11 手工适配 |
| 31564b2e86cc | 变更 | fix(gemini): 刷新 Gemini 默认模型列表，移除已停用的 2.x 模型 | U11 手工适配 |
| 8b3f33f28ae3 | merge | Merge pull request #8390 from wc4440222/pr/fix-websearch-emulation-dimensions | 来源merge；不执行，不额外重复计入功能 |
| bf02c1a6e0f7 | merge | Merge pull request #8395 from feeeei/fix/gemini-model-lists | 来源merge；不执行，不额外重复计入功能 |
| 7311b865f327 | merge | Merge pull request #8287 from feeeei/feat/gemini-same-account-retries-config | 来源merge；不执行，不额外重复计入功能 |
| 184193f01ea0 | merge | Merge pull request #8369 from feeeei/fix/gemini-bound-cache-pool-retry | 来源merge；不执行，不额外重复计入功能 |
| dece26153621 | merge | Merge pull request #8373 from feeeei/fix/openai-models-cache-singleflight-recheck | 来源merge；不执行，不额外重复计入功能 |

## 全部259个文件

| 状态 | 上游路径 | 本地同路径存在 | 功能 | 分类 | 本地落点/处理 |
|---|---|---|---|---|---|
| M | .gitignore | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 仅必要时放行本地scoped说明文档；不搬其他忽略规则 |
| M | README.md | 是 | U18 | U18 不同步 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| M | README_CN.md | 是 | U18 | U18 不同步 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| M | README_JA.md | 是 | U18 | U18 不同步 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| A | assets/partners/banners/wandapi-cn-dark.svg | 否 | U18 | U18 不同步 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| A | assets/partners/banners/wandapi-cn.svg | 否 | U18 | U18 不同步 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| A | assets/partners/banners/wandapi-en-dark.svg | 否 | U18 | U18 不同步 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| A | assets/partners/banners/wandapi-en.svg | 否 | U18 | U18 不同步 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| A | assets/partners/logos/NiuProxy.jpg | 否 | U18 | U18 不同步 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| A | assets/partners/logos/soleapi.png | 否 | U18 | U18 不同步 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| M | backend/cmd/server/VERSION | 是 | U18 | U18 不同步 | 发布时手动定为v0.2.19-nuro.1；上游tag内该文件仍0.2.18 |
| M | backend/cmd/server/wire_gen.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 从本地wire provider生成；不复制上游生成结果 |
| M | backend/ent/group.go | 是 | U13 | U13 手工适配 | 本地schema最小变更后定向重新生成；不可覆盖整目录 |
| M | backend/ent/group/group.go | 是 | U13 | U13 手工适配 | 本地schema最小变更后定向重新生成；不可覆盖整目录 |
| M | backend/ent/group/where.go | 是 | U13 | U13 手工适配 | 本地schema最小变更后定向重新生成；不可覆盖整目录 |
| M | backend/ent/group_create.go | 是 | U13 | U13 手工适配 | 本地schema最小变更后定向重新生成；不可覆盖整目录 |
| M | backend/ent/group_update.go | 是 | U13 | U13 手工适配 | 本地schema最小变更后定向重新生成；不可覆盖整目录 |
| M | backend/ent/migrate/schema.go | 是 | U13 | U13 手工适配 | 本地schema最小变更后定向重新生成；不可覆盖整目录 |
| M | backend/ent/mutation.go | 是 | U13 | U13 手工适配 | 本地schema最小变更后定向重新生成；不可覆盖整目录 |
| M | backend/ent/runtime/runtime.go | 是 | U13 | U13 手工适配 | 本地schema最小变更后定向重新生成；不可覆盖整目录 |
| M | backend/ent/schema/group.go | 是 | U13 | U13 手工适配 | 本地schema最小变更后定向重新生成；不可覆盖整目录 |
| M | backend/go.mod | 是 | U01 | U01 必须同步 | 现存同路径，逐hunk适配 |
| M | backend/go.sum | 是 | U01 | U01 必须同步 | 现存同路径，逐hunk适配 |
| M | backend/internal/config/config.go | 是 | U09 | U09 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/config/config_test.go | 是 | U09 | U09 手工适配 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/handler/admin/account_handler.go | 是 | U12,U17 | U12 手工适配（含诊断UI，已纳入）；U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| A | backend/internal/handler/admin/account_handler_grok_model_diagnostics_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/handler/admin/admin_helpers_test.go | 是 | U02 | U02 必须同步 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/handler/admin/announcement_handler.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| A | backend/internal/handler/admin/custom_admin_key_accounts.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/handler/admin/custom_admin_key_accounts_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/handler/admin/custom_admin_key_codex_import.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 本地Codex导入受step-up保护；不导入key绕过实现；测试改为拒绝机器key，普通JSON导入原样 |
| A | backend/internal/handler/admin/custom_admin_key_codex_import_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/handler/admin/custom_admin_key_commit_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/handler/admin/custom_admin_key_group_models.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/handler/admin/custom_admin_key_proxies.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/handler/admin/custom_admin_key_proxies_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/handler/admin/custom_admin_key_users.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/handler/admin/custom_admin_key_users_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/handler/admin/custom_admin_keys.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/handler/admin/custom_admin_keys_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/handler/admin/custom_legacy_admin_key_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/handler/admin/group_handler.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/handler/admin/idempotency_helper.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | scoped存储scope/ActorScope均按稳定keyID隔离；revision只用于验权/投影；同库操作记录覆盖提交后崩溃，JWT/legacy不变 |
| M | backend/internal/handler/admin/ops_dashboard_handler.go | 是 | U02 | U02 必须同步 | 现存同路径，逐hunk适配 |
| M | backend/internal/handler/admin/promo_handler.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| M | backend/internal/handler/admin/proxy_handler.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| A | backend/internal/handler/admin/scoped_admin_content_response.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/handler/admin/scoped_admin_content_response_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/handler/admin/scoped_admin_subscription_response.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/handler/admin/scoped_admin_subscription_response_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/handler/admin/scoped_admin_usage_response.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/handler/admin/scoped_admin_usage_response_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/handler/admin/setting_handler_runtime.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | backend/internal/handler/admin/setting_handler.go + 拟新增custom_admin_keys.go；scoped生命周期纳入，停用legacy hunk排除 |
| M | backend/internal/handler/admin/subscription_handler.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| M | backend/internal/handler/admin/usage_handler.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| M | backend/internal/handler/admin/user_handler.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| M | backend/internal/handler/dto/account_mapper_redact_test.go | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，保留本地断言并增补 |
| M | backend/internal/handler/dto/mappers.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/handler/dto/types.go | 是 | U12,U13 | U12 手工适配（含诊断UI，已纳入）；U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/handler/gateway_models_test.go | 是 | U11 | U11 手工适配 | 现存同路径，保留本地断言并增补 |
| A | backend/internal/handler/gemini_cached_content_bound_retry_test.go | 否 | U10 | U10 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/handler/gemini_mixed_models_test.go | 否 | U11 | U11 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/handler/gemini_v1beta_handler.go | 是 | U11 | U11 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/pkg/adminaccess/catalog.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/pkg/adminaccess/policy.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/pkg/adminaccess/policy_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/pkg/antigravity/response_transformer.go | 是 | U05 | U05 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/pkg/antigravity/response_transformer_malformed_test.go | 否 | U05 | U05 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/pkg/apicompat/anthropic_to_responses.go | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/pkg/gemini/models.go | 是 | U11 | U11 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/pkg/gemini/models_test.go | 是 | U11 | U11 手工适配 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/pkg/geminicli/models.go | 是 | U11 | U11 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/pkg/geminicli/models_test.go | 是 | U11 | U11 手工适配 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/pkg/openai/constants.go | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/pkg/pagination/pagination.go | 是 | U02 | U02 必须同步 | 现存同路径，逐hunk适配 |
| M | backend/internal/pkg/pagination/pagination_test.go | 是 | U02 | U02 必须同步 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/pkg/response/response.go | 是 | U02 | U02 必须同步 | 现存同路径，逐hunk适配 |
| M | backend/internal/pkg/response/response_test.go | 是 | U02 | U02 必须同步 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/repository/account_repo.go | 是 | U12,U17 | U12 手工适配（含诊断UI，已纳入）；U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| A | backend/internal/repository/account_repo_grok_managed_extra_integration_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/account_repo_grok_managed_extra_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/account_repo_grok_models.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 拟新增同名最小CAS保存接口；衔接本地account_repo.go受管Extra原子保留表达式 |
| A | backend/internal/repository/account_repo_grok_models_integration_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/account_repo_grok_models_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/repository/account_repo_opencode_go_usage_test.go | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，保留本地断言并增补 |
| M | backend/internal/repository/account_repo_upstream_billing_probe_update_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/repository/api_key_repo.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/repository/custom_account_proxy_selection.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/repository/custom_account_proxy_selection_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/custom_admin_key_group_models.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/repository/custom_admin_key_group_models_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/custom_admin_key_review_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/custom_admin_key_scope.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 拟新增受限SQL查询/窄更新；授权后分页，关联ID批量获取防N+1；不加载完整敏感group对象 |
| A | backend/internal/repository/custom_admin_key_scope_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/custom_admin_key_transaction.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 复用本地account_repo.go/clientFromContext/Tx/OnCommit；仅scoped窄适配，不覆盖全局repo |
| A | backend/internal/repository/custom_admin_keys.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/repository/custom_admin_keys_postgres_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/custom_admin_keys_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/custom_proxy_options.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/repository/custom_proxy_sample_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/repository/custom_proxy_search.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| M | backend/internal/repository/group_repo.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/repository/pagination_test.go | 否 | U02 | U02 必须同步 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/repository/setting_repo.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| M | backend/internal/repository/wire.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| M | backend/internal/securityaudit/prompt_handler.go | 否 | U02 | U02 必须同步 | backend/internal/securityaudit/handler.go（ListEvents/eventFilterFromQuery） |
| M | backend/internal/securityaudit/prompt_handler_test.go | 否 | U02 | U02 必须同步 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/server/api_contract_test.go | 是 | U13 | U13 手工适配 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/server/middleware/admin_auth.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | adminAuthWithSecurity保留旧key/JWT/WS/session分支；gate下新增scoped；auth_method继续机器key不可通过step-up |
| A | backend/internal/server/middleware/custom_admin_key_rate_limit_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/server/middleware/custom_admin_keys_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/server/middleware/panel_rate_limit.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | scoped存在时按keyID且不被ExemptAdmin豁免；JWT/legacy仍userScoped原逻辑 |
| M | backend/internal/server/routes/admin.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| A | backend/internal/server/routes/custom_admin_key_access_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/server/routes/custom_admin_key_group_models_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/server/routes/custom_admin_key_payment_rate_limit_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/server/routes/custom_admin_key_user_responses_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/server/routes/custom_admin_keys.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/server/routes/custom_admin_keys_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/server/routes/payment.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 仅新增scoped精确授权与响应边界；保留本地step-up/鉴权/现有JWT与legacy限流 |
| M | backend/internal/service/account_test_service.go | 是 | U11 | U11 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/account_test_service_gemini_test.go | 是 | U11 | U11 手工适配 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/service/admin_account.go | 否 | U12,U17 | U12 手工适配（含诊断UI，已纳入）；U17 手工适配（独立阶段，默认关闭） | backend/internal/service/admin_service.go（CreateAccount/UpdateAccount；目录及scoped窄patch分开；复用本地Tx/OnCommit，不覆盖二开字段） |
| A | backend/internal/service/admin_account_grok_catalog_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/admin_group.go | 否 | U13 | U13 手工适配 | backend/internal/service/admin_service.go（CreateGroup/UpdateGroup） |
| M | backend/internal/service/admin_group_duplicate.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/admin_service.go | 是 | U13,U17 | U13 手工适配；U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| M | backend/internal/service/admin_service_role_test.go | 否 | U03 | U03 必须同步 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/admin_user.go | 否 | U03 | U03 必须同步 | backend/internal/service/admin_service.go（ensureNotLastAdmin/UpdateUser/DeleteUser） |
| M | backend/internal/service/antigravity_gateway_compat.go | 是 | U05 | U05 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/antigravity_gateway_gemini.go | 否 | U13 | U13 手工适配 | backend/internal/service/antigravity_gateway_service.go 对应native结果路径 |
| A | backend/internal/service/antigravity_gateway_nonstream_malformed_test.go | 否 | U05 | U05 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/antigravity_gateway_streaming.go | 否 | U05,U11 | U05 手工适配；U11 手工适配 | backend/internal/service/antigravity_gateway_service.go（collectClaudeStreamResponse/isImageGenerationModel） |
| M | backend/internal/service/antigravity_image_test.go | 是 | U11 | U11 手工适配 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/service/api_key_auth_cache.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/api_key_auth_cache_impl.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/api_key_service.go | 是 | U02 | U02 必须同步 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/billing_service.go | 是 | U06,U08 | U06 手工适配；U08 本地实现保留 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/billing_service_test.go | 是 | U06 | U06 手工适配 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/service/channel_service.go | 是 | U14 | U14 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/service/channel_websearch_merge_test.go | 否 | U14 | U14 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/custom_admin_key_accounts.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/service/custom_admin_key_accounts_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/custom_admin_key_codex_import.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 不导入绕过本地Codex导入step-up的专用服务；允许范围只登记已适配路由 |
| A | backend/internal/service/custom_admin_key_codex_import_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/custom_admin_key_group_models.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/service/custom_admin_key_group_models_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/custom_admin_key_proxy_selection.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/service/custom_admin_key_proxy_selection_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/custom_admin_key_recovery_review_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/custom_admin_key_testdb_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/custom_admin_key_transaction.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 复用本地已有post-commit/outbox；缺少的受限动作薄封装，不能重复注册或提交前副作用 |
| A | backend/internal/service/custom_admin_key_transaction_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/custom_admin_keys.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | backend/internal/service/custom_admin_keys_test.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/custom_proxy_options.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| M | backend/internal/service/deepseek_pricing_test.go | 否 | U08 | U08 本地实现保留 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/gateway_billing_block.go | 是 | U16 | U16 冲突，本轮不改 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| A | backend/internal/service/gateway_billing_block_test.go | 否 | U16 | U16 冲突，本轮不改 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| M | backend/internal/service/gateway_billing_header_test.go | 是 | U16 | U16 冲突，本轮不改 | 本轮不修改/不新增；仅保留清单和设计证据（U16稳定前缀冲突；U18非必要） |
| M | backend/internal/service/gateway_service.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/gateway_usage_billing.go | 否 | U13 | U13 手工适配 | backend/internal/service/gateway_service.go（calculateRecordUsageCost/calculateImageCost） |
| M | backend/internal/service/gemini_cached_content.go | 是 | U10 | U10 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/gemini_cached_content_hardening_test.go | 否 | U10 | U10 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/gemini_chat_completions_compat_service.go | 是 | U09,U13 | U09 手工适配；U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/gemini_error_policy_single_check_test.go | 是 | U09 | U09 手工适配 | 现存同路径，保留本地断言并增补 |
| M | backend/internal/service/gemini_image_output_accounting.go | 否 | U13 | U13 手工适配 | 拟新增小型请求级计数器；挂接本地gemini_messages_compat_service.go及gemini_chat_completions_compat_service.go实际响应处理器；区分实际与兼容估计 |
| M | backend/internal/service/gemini_image_output_accounting_test.go | 否 | U13 | U13 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/gemini_messages_compat_service.go | 是 | U09,U10,U13 | U09 手工适配；U10 手工适配；U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/gemini_models_endpoints.go | 否 | U11 | U11 手工适配 | backend/internal/service/gemini_model_discovery.go + gemini_messages_compat_service.go |
| A | backend/internal/service/gemini_same_account_retries_test.go | 否 | U09 | U09 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/gemini_vertex_api_key_forward_test.go | 否 | U09 | U09 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/grok_media.go | 是 | U04,U13 | U04 手工适配；U13 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/service/grok_model_catalog.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 拟新增同名解析小模块；本地upstream_models.go统一调用，保留scope及有界状态 |
| A | backend/internal/service/grok_model_catalog_store.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 拟新增同名merge/CAS小模块；使用本地repository与事务接口，与scoped各自review |
| A | backend/internal/service/grok_model_catalog_store_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/grok_model_catalog_sync_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/grok_model_catalog_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/grok_model_diagnostics.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 已纳入：只读admin诊断，不触生成链，不fetch/刷新token/写库 |
| A | backend/internal/service/grok_model_diagnostics_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/grok_observed_models.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 本地无此文件；只借鉴固定大小响应审计，不新增请求触发后台任务/无界缓存；自动观测本轮关闭 |
| A | backend/internal/service/grok_observed_models_log_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/grok_p2_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/grok_quota_service_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/grok_response_model_audit_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/group.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/group_service.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/service/image_billing_count.go | 否 | U13 | U13 手工适配 | 拟新增最小计数规则；不采用未知数自动补1的hunk，保留provider原始usage与兼容计费来源隔离 |
| A | backend/internal/service/image_billing_count_test.go | 否 | U13 | U13 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/image_count_billing_switch_test.go | 否 | U13 | U13 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/media_price_config.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/model_plaza_service.go | 否 | U19 | U19 手工适配 | backend/internal/service/model_plaza.go（modelPlazaImageDisplayPricing/build） |
| M | backend/internal/service/model_plaza_service_test.go | 否 | U19 | U19 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/openai_chat_roles.go | 是 | U21 | U21 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_chat_roles_test.go | 否 | U21 | U21 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/openai_codex_models_service.go | 是 | U06,U15 | U06 手工适配；U15 必须同步 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_codex_transform.go | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_compat_model.go | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_gateway_chat_completions.go | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_gateway_chat_completions_raw.go | 是 | U06 | U06 手工适配 | 冻结，不修改；用openai_chat_roles.go/forwardAsCompatibleRawChatCompletions适配 |
| M | backend/internal/service/openai_gateway_grok.go | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_gateway_messages.go | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_gateway_messages_chat_fallback.go | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_gateway_response_handling.go | 否 | U07 | U07 手工适配 | backend/internal/service/openai_gateway_service.go（openAICacheReadTokensFromUsage）；WS v2另有同名helper |
| M | backend/internal/service/openai_gateway_responses_chat_fallback.go | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_gateway_service.go | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_gateway_usage.go | 否 | U13 | U13 手工适配 | backend/internal/service/openai_gateway_service.go（calculateOpenAIRecordUsageCost/calculateOpenAIImageCost） |
| A | backend/internal/service/openai_gpt61_terra_test.go | 否 | U06 | U06 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/openai_images.go | 是 | U04,U13 | U04 手工适配；U13 手工适配 | 现存同路径，逐hunk适配 |
| M | backend/internal/service/openai_images_responses.go | 是 | U04,U13 | U04 手工适配；U13 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/service/openai_images_upload_limit_test.go | 否 | U04 | U04 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/openai_model_alias.go | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/service/openai_usage_deepseek_cache_test.go | 否 | U07 | U07 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | backend/internal/service/pagination_test.go | 否 | U02 | U02 必须同步 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/pricing_service.go | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| A | backend/internal/service/ratelimit_cn_frequency_429_test.go | 否 | U20 | U20 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/ratelimit_cn_providers.go | 否 | U20 | U20 手工适配 | backend/internal/service/ratelimit_cn_quota.go + ratelimit_service.go（handle429） |
| M | backend/internal/service/response_model_billing_test.go | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | backend/internal/service/setting_features.go | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | backend/internal/service/setting_service.go；接scoped窄依赖，保留legacy key生成策略 |
| M | backend/internal/service/setting_service.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| M | backend/internal/service/upstream_models.go | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，逐hunk适配 |
| M | backend/internal/service/upstream_models_test.go | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，保留本地断言并增补 |
| M | backend/internal/service/upstream_response_model_test.go | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，保留本地断言并增补 |
| M | backend/internal/service/wire.go | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| A | backend/migrations/247_scoped_admin_api_keys.sql | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 拟新增256_scoped_admin_api_keys.sql；保留本地247/248，新增policy_revision与原子操作记录；实际空号复核 |
| A | backend/migrations/248_group_image_count_billing.sql | 否 | U13 | U13 手工适配 | 拟新增 backend/migrations/255_group_image_count_billing.sql；实施时再查空位 |
| M | backend/resources/model-pricing/model_prices_and_context_window.json | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| A | docs/admin-api-keys.md | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | frontend/src/api/__tests__/admin.accounts.grokModelDiagnostics.spec.ts | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | frontend/src/api/admin/accounts.ts | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，逐hunk适配 |
| A | frontend/src/api/admin/adminAccessKeys.ts | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| M | frontend/src/components/account/EditAccountModal.vue | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，逐hunk适配 |
| A | frontend/src/components/account/GrokModelDiagnosticsPanel.vue | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 已纳入：沿本地样式实现折叠只读诊断，保留原账号表单；不整文件复制 |
| M | frontend/src/components/account/ModelWhitelistSelector.vue | 是 | U12 | U12 手工适配（含诊断UI，已纳入） | 现存同路径，逐hunk适配 |
| M | frontend/src/components/account/__tests__/EditAccountModal.grokUpstream.spec.ts | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | frontend/src/components/account/__tests__/GrokModelDiagnosticsPanel.spec.ts | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | frontend/src/components/account/__tests__/ModelWhitelistSelector.spec.ts | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| A | frontend/src/components/admin/settings/AdminAccessKeysSettings.vue | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按本地接口新增小模块并测试；U17仅scoped入口，保留legacy/step-up及保护字段 |
| A | frontend/src/components/admin/settings/__tests__/AdminAccessKeysSettings.spec.ts | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |
| M | frontend/src/components/keys/UseKeyModal.vue | 是 | U06 | U06 手工适配 | 现存同路径，逐hunk适配 |
| M | frontend/src/components/keys/__tests__/UseKeyModal.spec.ts | 是 | U06 | U06 手工适配 | 现存同路径，保留本地断言并增补 |
| M | frontend/src/composables/__tests__/useModelWhitelist.spec.ts | 是 | U06,U11 | U06 手工适配；U11 手工适配 | 现存同路径，保留本地断言并增补 |
| M | frontend/src/composables/useModelWhitelist.ts | 是 | U06,U11 | U06 手工适配；U11 手工适配 | 现存同路径，逐hunk适配 |
| M | frontend/src/i18n/locales/en/admin/accounts.ts | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| M | frontend/src/i18n/locales/en/admin/channels.ts | 否 | U14 | U14 手工适配 | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| M | frontend/src/i18n/locales/en/admin/overview.ts | 否 | U13 | U13 手工适配 | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| A | frontend/src/i18n/locales/en/adminAccessKeys.ts | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| M | frontend/src/i18n/locales/en/index.ts | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| M | frontend/src/i18n/locales/zh/admin/accounts.ts | 否 | U12 | U12 手工适配（含诊断UI，已纳入） | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| M | frontend/src/i18n/locales/zh/admin/channels.ts | 否 | U14 | U14 手工适配 | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| M | frontend/src/i18n/locales/zh/admin/overview.ts | 否 | U13 | U13 手工适配 | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| A | frontend/src/i18n/locales/zh/adminAccessKeys.ts | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| M | frontend/src/i18n/locales/zh/index.ts | 否 | U17 | U17 手工适配（独立阶段，默认关闭） | 本地 zh.ts / en.ts 按key增补；不导入上游分目录重构 |
| M | frontend/src/types/index.ts | 是 | U12,U13 | U12 手工适配（含诊断UI，已纳入）；U13 手工适配 | 现存同路径，逐hunk适配 |
| M | frontend/src/views/admin/ChannelsView.vue | 是 | U14 | U14 手工适配 | 现存同路径，逐hunk适配 |
| M | frontend/src/views/admin/GroupsView.vue | 是 | U13 | U13 手工适配 | 现存同路径，逐hunk适配 |
| M | frontend/src/views/admin/SettingsView.vue | 是 | U17 | U17 手工适配（独立阶段，默认关闭） | 现存同路径，逐hunk适配 |
| A | frontend/src/views/admin/__tests__/ChannelsView.webSearchEmulation.spec.ts | 否 | U14 | U14 手工适配 | 按所属功能在本地实际包新增/改写测试；不假定可原样编译 |

## 统计

- commits: 71 = 50 non-merge + 21 merge。
- files: 259 = 151 modified + 108 added；deleted/renamed = 0。
- 本地不存在同名路径：147；这包含上游拆分文件与新增测试，不代表147项业务全部缺失。
- 完整patch保存在upstream.diff；commit完整SHA和逐文件归属保存在inventory.json。
- 本地新增O01–O09优化不属于上游259文件统计，见主方案T；不能把全量归类误读成全量照搬。
