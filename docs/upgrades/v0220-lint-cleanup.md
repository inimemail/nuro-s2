# v0.2.20 后续 lint 治理记录

基于提交 `f1a8f1f5d3c742e5631b868670973a198a817cfc`，按用户确认的范围处理实际缺陷和低风险清理。初始工作区干净。本记录独立于历史升级验证索引，不把旧日志作为本次修改后的通过证据。

## 本次结果

使用原 `.golangci.yml`、Go 1.27.3 和同一 golangci-lint，取消告警数量截断后重新采集：**563 → 553，减少 10 项，新增 0 项**。完整 lint 仍以退出码 1 结束，不能宣称通过。未修改规则、排除项或增加 `nolint`。

| 检查器 | 修改前 | 修改后 |
| --- | ---: | ---: |
| errcheck | 300 | 297 |
| gofmt | 3 | 0 |
| govet | 3 | 0 |
| staticcheck | 89 | 88 |
| ineffassign | 21 | 21 |
| unused | 120 | 120 |
| depguard | 17 | 17 |
| gosec | 10 | 10 |

## 实际缺陷修复

`BackupService.createCompressedBackupFile` 原先只检查读取和压缩错误，忽略导出流的 `Close` 错误。实际 `cmdReadCloser.Close` 会调用 `cmd.Wait`，因此 stdout 到达 EOF、但 `pg_dump` 最终非零退出时，手动和后台备份均可能上传部分数据并标记完成。

现在在交付归档前检查导出进程完成结果。失败时清理归档并返回错误，两个上传入口均记录失败；保证流只关闭一次。读取等更早的失败仍保留原错误，关闭阶段的次要错误写入日志。成功备份的压缩内容和文件权限不变。

新增回归先在原实现上运行，确认“进程失败后仍成功”及两个上传入口的反例失败，再运行修复后测试。覆盖正常归档、读取中断、导出退出失败、主次错误同时发生、归档清理、只关闭一次及两条路径均不上传。

## 低风险清理

- 消除图片任务错误回包中对刚构造的 map 的类型断言，保持错误字段、状态码和脱敏规则一致。
- TypeSafe JSON 键解析显式检查类型断言；合并没有递归引用的局部函数声明。
- 三处 `reflect.Ptr` 改为等值别名 `reflect.Pointer`。
- 清理三处 formatter 告警，包括测试中的 `interface{}` 改为等价的 `any` 和结构体字段对齐。

## 保留项及边界

剩余 553 项不是 553 个已确认漏洞，也没有全部被认定为误报。后续仍应结合调用链逐项判断：

- `errcheck` 包含不会失败的 Builder 写入、只读资源关闭和内部接口类型断言；不批量添加 `_ =`。
- `unused` 需要核对带构建标签的测试、恢复用途及保护功能，不批量删除。
- `ineffassign` 与部分 staticcheck 涉及调度、重试、缓存、计费展示和恢复状态，保留现有语义。Live 停机时的空临界区是等待最后一个 worker 注册完成的同步屏障，不能直接删除。
- `depguard` 涉及 Redis 依赖分层，留给独立架构改动。
- `gosec` 的生产告警涉及操作者配置的邀请 URL、调试日志路径和价格数据目录；测试告警涉及本地进程/HTTP 夹具。目前追踪未确认其为直接接收终端用户输入的注入路径，不因此改写路径或网络策略，也不压制告警。
- OIDC 的旧椭圆曲线 API 迁移和错误文案大小写调整不混入本批次；前者需要完整曲线及负例验证，后者须先排查字符串依赖。

20 个冻结文件和 283 个历史迁移通过原保护契约。没有修改缓存 ABCD/markup、sticky/24h、调度隔离、Cell/escrow/lease/WAL、未知执行不重放、Rust Edge、部署脚本或前端，也没有提交、推送或部署。

## 验证

原始日志位于本机 `/private/tmp/nuro-lint-cleanup-*`。测试计数包括子测试。

| 检查 | 结果 | 本机日志/报告 |
| --- | --- | --- |
| 新增回归在修复前 | 按预期失败：退出失败被当作成功，两个入口均受影响 | `nuro-lint-cleanup-backup-before.log` |
| 新增回归在修复后 | 通过 | `nuro-lint-cleanup-backup-after.log` |
| 完整 Go default | 8467 pass / 0 fail / 7 skip | `nuro-lint-cleanup-go/report.json` |
| 完整 Go unit | 13429 pass / 0 fail / 11 skip | 同上 |
| Go build / vet | 通过 | 同上 |
| 备份相关 race | 38 pass / 0 fail / 0 skip | `nuro-lint-cleanup-backup-race.jsonl` |
| 保护契约 | 20 文件 / 283 历史迁移不变 | `nuro-lint-cleanup-contracts.log` |
| 完整 lint | 553 项，新增 0 / 减少 10，仍失败 | `nuro-lint-cleanup-comparison.json` |

本批次源码 SHA-256 与日志摘要见本机 `/private/tmp/nuro-lint-cleanup-evidence.json`。历史 `v0220-validation.json` 的源码指纹与结果保持其原始范围。

## 后续代码审查

再次逐项审查全部未提交文件及备份、图片任务错误回包、TypeSafe 校验、设置字段映射的调用关系，未发现本批次新增的业务缺陷，没有继续修改生产逻辑。对 `reflect.Ptr` 的替换与目标 Go 源码中的 `const Ptr = Pointer` 核对，两个测试中的 `any` 与原空接口等价。

此前备份回归使用模拟导出流，本轮补充实际子进程测试验证真实 `cmdReadCloser`：stdout 正常结束后分别以 0/7 退出，以及 stdout 已结束但子进程尚未退出时取消 context。检查退出错误仍能通过 `errors.As` 取得原退出码，且进程已被回收。夹具仅重启本地测试程序，不执行 pg_dump、不连接数据库。

备份、进程回收、图片错误脱敏、TypeSafe、设置更新、重试和 usage 扫描相关定向 race 使用 `-count=1`，共 **104 pass / 0 fail / 0 skip**。完整 lint 再次为 **553 项，新增 0 / 减少 0**；20 文件/283 历史迁移保护契约和 `git diff --check` 通过。本轮只新增测试文件；此前 11 份后端源码/测试文件的 SHA-256 保持一致，完整 default/unit/build/vet 沿用上表的同源码证据，不声称重新执行全量测试。

本轮日志为 `/private/tmp/nuro-lint-review-race.jsonl`、`nuro-lint-review-lint.json`、`nuro-lint-review-lint-comparison.json` 和 `nuro-lint-review-contracts.log`；独立指纹与摘要为 `/private/tmp/nuro-lint-review-evidence.json`。此前证据保持原值，不覆盖其历史指纹。

完整 lint 可从 `backend/` 复现（本机工具路径需按环境调整）：

```sh
GOTOOLCHAIN=go1.27.3 /private/tmp/nuro-v0220-tools/golangci-lint run ./... \
  --timeout=30m --output.json.path /private/tmp/nuro-lint-cleanup-after.json \
  --output.text.path /dev/null --max-issues-per-linter 0 --max-same-issues 0
```

前后比较按文件名、检查器、文案、源代码行的多重集合匹配，忽略行号移动；详见 `/private/tmp/nuro-lint-cleanup-comparison.json`。没有重跑 PostgreSQL/Redis/Rust 集成、前端构建或真实部署，不据本轮单元测试宣称这些门禁通过。
