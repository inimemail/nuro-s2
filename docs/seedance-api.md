# Seedance 原生任务 API

使用 OpenAI 平台的 API Key 账号，在账号创建／编辑中显式开启 Seedance。已有账号默认关闭。批量编辑默认为“保持不变”；禁用只停止新任务选号，已提交任务仍由原账号查询和结算。

## 配置

1. 配置上游 API Key 和自定义 Base URL，例如 Ark 的实际 API 地址。支持 origin、`/api/v3`、`/v3` 及代理路径前缀；通用 `/v1` 后缀会替换为 `/api/v3`。
2. 配置公开模型名到上游模型／推理接入点 ID 的映射。
3. 在分组或渠道配置明确的 **Token 或按次价格**。默认按客户端请求的模型名找价；渠道显式选择“渠道映射模型计费”或“上游模型计费”时沿用该选择。没有可用价格会在创建前拒绝，不猜测价格；动态价格的输出价缺失／为零不会自动视为免费，免费任务需要在分组或渠道明确配置零价格。
4. 开启分组媒体生成权限。Seedance 使用现有主调度器的账号能力、优先级、配额和 HTTP 并发检查。异步在途数另行限制：账号默认等于其并发（限制在 1–100），可用凭证字段 `seedance_max_inflight` 配置 1–100；每用户最多 100 个在途任务。

账号探测弹窗中有独立的“Seedance 对接测试”。它要求填写自己的下游 Key 并明确确认费用，走真实分组调度与计费，不保证选择当前正在查看的账号。普通文本监控／探测不会创建视频。测试 Key 不写入本地存储。

## 入口

以下四组别名使用相同身份认证与任务归属校验：

| 操作 | 方法 | 路径 |
| --- | --- | --- |
| 创建 | POST | `/api/v3/contents/generations/tasks` |
| 查询 | GET | `/api/v3/contents/generations/tasks/{id}` |
| 取消／删除 | DELETE | `/api/v3/contents/generations/tasks/{id}` |

前缀还可用 `/v3`、`/v1` 或留空。请求使用下游 `Authorization: Bearer ...`，上游认证由账号凭证生成，绝不转发下游 Key。

创建请求保留原生 `content`、图片／视频引用及未知参数，仅应用模型映射：

```http
POST /api/v3/contents/generations/tasks
Authorization: Bearer <YOUR_DOWNSTREAM_KEY>
Content-Type: application/json
Idempotency-Key: <A_UNIQUE_KEY_FOR_THIS_LOGICAL_TASK>

{
  "model": "<YOUR_CONFIGURED_MODEL_ALIAS>",
  "content": [{"type": "text", "text": "A quiet mountain lake at sunrise"}]
}
```

**该请求会产生实际费用。** 创建后保存原生响应中的 `id` 和响应头 `X-Seedance-Operation-ID`。查询可用任一 ID，但必须使用原用户、原 API Key 和原分组；未知／其他用户任务统一 404。

查询和取消已有任务不受新请求的余额、Key 配额／到期检查阻拦；禁用 Key、停用用户、IP 限制、分组权限和任务归属检查继续生效。创建任务仍执行原有计费准入检查。

不同上游账号如果返回相同 provider ID，会拒绝有歧义的查询／取消（409），此时使用唯一的本地 `X-Seedance-Operation-ID`。测试弹窗优先使用本地 ID，并展示两个 ID 供恢复。

强烈建议每个逻辑任务传 `Idempotency-Key`（1–256 字符）。同一 Key、用户、分组和相同请求体的重试返回已有任务，不重复创建；同一幂等键配不同请求体返回 409。请求体哈希基于原始字节，重试时请保持字节一致。

## 恢复与计费

- 提交前持久化操作记录；上游接受后绑定原账号和 provider ID。客户端断开不会立即丢弃创建响应，但整个提交保留有界截止时间。
- 已发送但结果未知时不切号、不重放 POST。返回 `seedance_submission_unknown` 时，用本地操作 ID 查询或以相同幂等键核对；不要换一个新键自动重试。
- 没有拿到 provider ID 的未知提交无法自动找回上游任务，记录保留用于人工对账。进程在收到上游结果前崩溃同样存在这一外部 API 限制。
- 后台 4 个有界 worker 按账号归属查询。任务 ID 持久保存在 PostgreSQL，不依赖 24 小时 Redis 绑定；重启后恢复已知任务。
- 任务同时保存原上游地址与凭证指纹（不保存明文 Key）。尚未取得终态时更换地址或 Key 会暂停上游查询，防止向另一个上游查询并错误计费；已经持久保存的终态可继续本地结算。需要换号时建议新增账号，旧账号先停止接收新生成任务。
- 终态必须有合法的真实 `usage.completion_tokens` 才能结算。缺失、负数、字符串或非整数 usage 保留待对账，不按请求估算 token、不按零费用结案。
- Token 模式按 completion token 计费；按次模式按一个任务计费，失败且明确返回 0 token 的任务不收按次费用。原始终态和 usage 保存在任务记录，用户扣费、Key 配额、账号配额、usage log 和结算标记在同一数据库事务中提交。
- 首次确认的终态和真实用量先独立持久化到 `terminal_response`，之后才尝试结算。结算暂时失败时直接使用已保存证据重试，不依赖供应商继续保留任务；迟到查询不能覆盖已保存终态。生成在途名额随已确认终态释放，不必等待计费恢复。
- 用户平台配额沿用当前持久化模式：数据库模式随结算事务更新；flusher 模式使用持久 outbox 与 Redis 幂等事件，再交给现有 flusher。不要在未结清任务期间切换 flusher 模式；检测到模式变化会保留待对账并报警。Redis 丢失未刷盘数据仍受现有 flusher 的恢复边界约束。
- DELETE 前先核对任务状态；已完成任务必须先保存真实终态用量，保存失败或终态缺 usage 时拒绝删除。运行中任务可请求取消，但 DELETE 成功不等于免费，也不直接删除本地计费记录；后台仍核对真实用量。如果供应商取消后不再允许查 usage，任务保持待对账，不能保证自动补回供应商已丢弃的数据。
- 未知提交保持在途占用；已返回终态但缺少 usage 的任务释放生成在途名额，继续对账。后台查询使用原账号的 HTTP 并发槽，不重新选生成账号。

## 部署与排查

新增迁移为 `234_seedance_tasks.sql`，只创建新表和索引，不修改旧平台约束。发布前应在可用 PostgreSQL / Redis 环境验证迁移、重启恢复和真实供应商任务。

查询日志中的 `seedance.submission_persist_failed`、`seedance.settlement_pending`、`seedance.effects_pending`、`seedance.quota_mode_changed`。排查时使用 operation ID；不要提供 API Key。

```sql
SELECT id, provider_id, account_id, user_id, api_key_id, state,
       settled, effects_pending, attempts, updated_at
FROM seedance_tasks
WHERE NOT settled OR effects_pending
ORDER BY updated_at;
```

该链路不进入文本 Edge、24h prompt cache、首 Token 补帧或 Grok 视频临时绑定路径。
