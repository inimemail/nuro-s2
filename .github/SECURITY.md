[English](#english) | [中文](#中文)

## English

### Reporting a Vulnerability

Please do not report security vulnerabilities through public issues, pull requests, or discussions. Report them privately through GitHub's private vulnerability reporting:

**[Report a vulnerability](https://github.com/inimemail/nuro-s2/security/advisories/new)**

If private reporting is unavailable, contact a project maintainer through a trusted private channel. Do not publish details until a fix is available.

A useful report includes:

- The affected component and version (release tag or commit)
- The impact and privileges required to exploit the issue
- Minimal reproduction steps or a proof of concept
- Any non-default configuration required to trigger it
- A suggested fix, if available

### Supported Versions

Security fixes are released with the latest maintained version and are not backported to older releases. Please verify that the issue reproduces on the latest release or `main` before reporting.

| Version | Supported |
| --- | --- |
| Latest release | Yes |
| Older releases | No |

### Scope

In scope: the backend, frontend, deployment files, and container images maintained in this repository.

Usually out of scope:

- Issues requiring already-compromised administrator credentials or server access
- Risks caused by explicitly weakening documented security settings
- Vulnerabilities in upstream AI providers or third-party dependencies without a project-specific impact
- Missing best-practice headers or scanner output without demonstrated impact

### Disclosure Process

1. Maintainers acknowledge the report and assess its validity and severity.
2. A fix is developed privately; maintainers may ask the reporter to verify it.
3. A release containing the fix is published before the advisory is disclosed, with credit given unless the reporter prefers to remain anonymous.

This project is maintained on a best-effort basis, so response times may vary.

## 中文

### 报告漏洞

请不要通过公开 Issue、Pull Request 或 Discussion 报告安全漏洞。请使用 GitHub 私密漏洞报告功能提交：

**[提交漏洞报告](https://github.com/inimemail/nuro-s2/security/advisories/new)**

如果私密报告功能不可用，请通过可信的私密渠道联系项目维护者。在修复发布前，请勿公开漏洞细节。

报告建议包含：

- 受影响的组件与版本（Release tag 或 commit）
- 漏洞影响及利用所需的权限
- 最小复现步骤或 PoC
- 触发问题所需的非默认配置
- 修复建议（如有）

### 支持的版本

安全修复随最新维护版本发布，不向旧版本回移。报告前请确认问题在最新 Release 或 `main` 上仍可复现。

| 版本 | 是否支持 |
| --- | --- |
| 最新 Release | 是 |
| 更早的版本 | 否 |

### 报告范围

范围内：本仓库维护的后端、前端、部署文件和容器镜像。

通常不在范围内：

- 需要事先已掌握管理员凭据或服务器权限才能利用的问题
- 因主动削弱文档所述安全设置而产生的风险
- 上游 AI 服务商或第三方依赖自身的漏洞，且对本项目没有特定影响
- 未证明实际影响的最佳实践响应头缺失或扫描器告警

### 披露流程

1. 维护者确认收到报告并评估问题的有效性与严重程度。
2. 维护者私下开发修复，并可能请报告者协助验证。
3. 先发布包含修复的版本，再公开安全公告；除非报告者希望匿名，否则会注明致谢。

本项目尽力维护，响应时间可能有所不同。
