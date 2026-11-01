# GitHub issue 接入

`github_issue` Input 将指定仓库中符合 assignee 或 label 条件的 issue 转为持久化任务，交给关联的 Pipeline。任务入队后，CloudPilot 会在原 issue 回复 WebUI 任务链接；Agent 收到 issue 与仓库上下文，可以在创建 PR 时关联原 issue。

## 配置

1. 创建或选择已有的 Pipeline，并确认其 Workspace 检出需要修改的仓库。`examples/nightly-feedback.yaml` 提供一组完整资源；该示例按计划执行，如需立即执行，将 Pipeline 的 `schedule` 留空。
2. 设置 `server.public_url` 或环境变量 `CLOUDPILOT_SERVER_PUBLIC_URL`，例如 `https://cloudpilot.example.com`。地址必须是用户可访问的绝对 HTTP(S) URL，不含账号密码、查询参数或 fragment。
3. 准备随机 Webhook 验证密钥和 GitHub Token。后者用于读取和回复 issue，可使用具有该仓库 **Issues: Read and write** 权限的 fine-grained PAT，或具有相同权限的 GitHub App installation token。使用短期 token 时由部署方负责更新。
4. 在接入点表单中选择 GitHub issue，直接填写 Webhook 验证密钥和 GitHub Token，或在 [`examples/github-issues.yaml`](../examples/github-issues.yaml) 中填写 `secret`、`github.token` 后导入。替换 `repository`、`pipelineRef`、assignee 和 label；凭据使用启动主密钥加密保存，读取和导出不会回显，编辑时留空保留已有值。
5. 在 GitHub 仓库的 Settings → Webhooks 添加 webhook：Payload URL 为 `https://cloudpilot.example.com/hooks/github-issues`，Content type 为 `application/json`，Secret 与表单中填写的 Webhook 验证密钥相同，选择 **Issues** 事件并启用。

保存新凭据不需要修改服务环境变量或重启容器。旧 `secretRef` 和 `github.tokenSecretRef` 引用继续兼容；输入直接凭据后会替换对应引用。后台评论记录保留当时的加密 Token，服务重启后仍能重试。

## 触发规则

`github.assignees` 和 `github.labels` 至少配置一个非空列表。列表内任意一项匹配即可；两个列表同时配置时也按“或”判断，名称不区分大小写。

| 配置 | 触发条件 |
| --- | --- |
| `assignees: [your-agent-account]` | issue 当前已指派给该 GitHub 用户 |
| `assignees: ['*']` | issue 当前至少有一个 assignee |
| `labels: [cloudpilot]` | issue 当前带有 `cloudpilot` label |
| 同时配置 assignees 和 labels | 满足任一 assignee 或 label 条件 |

只处理所配置仓库中状态为 `open` 的 issue，支持 `opened`、`reopened`、`assigned` 和 `labeled` action。CloudPilot 使用该次 webhook 中的 issue 状态进行匹配；PR、其他仓库、不支持的事件或 action、未命中的 issue 均返回 `200`，不会入队。签名有效的 `ping` 同样返回 `200`。

每次投递都通过 `X-Hub-Signature-256` 验证原始请求体的 HMAC-SHA256 签名。`github_issue` 不使用普通 webhook Input 的 Bearer token。请求体上限为 1 MiB；签名不正确返回 `401`，无效 JSON 返回 `400`，已暂停的 Input 返回 `409`，匹配任务所需的 token 或公共地址不可用返回 `503`。

同一 Input、仓库 ID 和 issue ID 只创建一个事件。GitHub 重投、多个匹配 action、移除后重新添加 label、重新打开 issue，都不会重复创建任务；重复命中返回原事件。需要重新执行失败、取消或中断的任务时，在 WebUI 中使用重试操作，保留已有运行记录。已存在的 issue 不会被定期扫描；配置生效后需要发生支持的事件。

## 回复与任务链接

任务入队与待发送评论在同一数据库事务内保存。评论由后台发送，包含以下形式的链接：

```text
https://cloudpilot.example.com/#/events/123
```

链接指向该事件的任务详情，可以在尚未开始执行时查看排队状态，也可以查看后续运行及重试记录。访问页面需要登录 CloudPilot。配置了定时调度或 Pipeline 暂停时，回复仍可先发出，执行按 Pipeline 配置进行。

GitHub 暂时不可用、权限不足或限流导致评论失败时，待发送记录会保留并重试，服务重启后继续处理；评论重试不会创建新任务或重新运行 Agent。排查时检查服务日志以及 token 对仓库的 Issues 权限。

## Agent 上下文与 PR

事件 payload 保留 GitHub 投递的原始字段，包括 `action`、`repository`、`issue` 和 `sender`，并添加 `source: github_issue`。若请求包含 `X-GitHub-Delivery`，其值保存在 `deliveryId`。其中 `issue.id` 是 GitHub 的全局 issue ID，`issue.number` 是仓库内用于链接 PR 的编号；Agent 同时收到标题、正文、URL、labels 和 assignees。WebUI 任务详情可查看该 payload。

批处理和交互式 Agent 都会获得这些上下文，以及在修复 PR 中使用 `Closes owner/repo#number` 关联原 issue 的提示。issue 正文作为任务内容处理，Worker 和 Workspace 配置继续提供执行指令。

用于评论的 `github.token` 不会自动成为 Agent 的 GitHub 凭据。若 Agent 需要推送分支或创建 PR，请通过 Workspace 的 `secretEnv` 显式提供所需凭据，并按任务配置仓库 Contents、Pull requests 等权限，例如：

```yaml
secretEnv:
  GH_TOKEN: AGENT_GITHUB_TOKEN
```

只填写关联编号或 URL 不会自动创建 PR；具体交付方式仍由 Worker/Workspace 指令和 Agent 的工具权限决定。
