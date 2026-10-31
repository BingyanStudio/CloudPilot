# Agent 会话与人工介入

Codex + Docker 使用 app-server 会话处理反馈。首次反馈创建一个 Run 和一个 AgentSession；同一任务的人工补充、打断与继续记录在该会话中。一个会话可以包含多个 AgentTurn，每轮都有独立状态、输入和时间记录。

## 在 Web UI 中使用

打开运行详情后，“对话与工具”展示用户输入、Agent 回复、命令执行、MCP 调用及其输出；“原始日志”保留执行器日志，便于排查启动和运行错误。页面根据执行器能力和当前状态显示可用操作，历史批处理任务仍可查看原始日志。

| 操作 | 行为 |
| --- | --- |
| 发送补充信息 | 正在执行时引导当前轮次；会话空闲、中断或失败时，排队开始下一轮 |
| 中断当前轮次 | 请求 Agent 停止当前轮次，保留会话和工作目录 |
| 继续执行 | 在原会话、原工作目录中开始新的一轮，可附加说明 |
| 结束任务 | 停止活动执行并删除保留的会话工作目录；历史日志、时间线和已收集产物保留 |
| 重新入队 | 关闭原会话后，为失败、取消或中断的事件创建新的 Run，从头执行 |

发送请求成功表示 CloudPilot 接收了操作。运行中的补充信息还需要 Agent 确认，页面会区分等待投递、已接受和投递失败。若当前轮次恰好完成，补充请求可能被拒绝，此时可以在会话空闲后重新发送。客户端不应把 HTTP 202 当作模型已经处理了该输入。

中断不会撤销已经执行的 Git push、已创建的 Issue、PR 或其他外部操作。继续执行继承对话和已有文件修改，重新开始一轮 Agent 推理，不会恢复被打断工具进程的 CPU 执行位置。

## 完成、排队和关闭

一次 Agent 轮次自然结束时，Run 和 Event 变为 `succeeded`，会话变为 `idle`，并收集本轮产物。因此“运行成功”与“会话仍可继续”可以同时成立。会话空闲时不会保留运行中的容器，也不占用 Pipeline 的并发额度；工作目录和 Codex 会话数据继续保留。

人工继续会将 Run 设为 `queued`、Event 设为 `waiting`，并创建状态为 `queued` 的下一轮。它不需要等待下一次 cron 触发，但仍遵守 Pipeline 当前的暂停状态和并发限制。相同会话不会并行执行两轮，活动轮次中的补充信息也不会额外占用并发额度。

| 情况 | Run / Event 状态 | 会话状态 |
| --- | --- | --- |
| 轮次执行中 | `running` | `running` |
| 人工继续等待调度 | `queued` / `waiting` | `queued` |
| 自然完成 | `succeeded` | `idle` |
| 人工中断或服务中断 | `interrupted` | `interrupted` |
| 超时或执行错误 | `failed` | `failed` |
| 清理会话存储中 | 保留结果或变为 `cancelled` | `closing` |
| 已结束会话 | 保留结果或变为 `cancelled` | `closed` |

已经建立 Codex thread 的轮次失败后保留现场，等待人工继续，不使用 `maxRetries` 自动重放；人工追加的轮次也不自动重试。首次执行在 thread 创建前失败时，只有成功清理尚未建立对话的会话存储后，才会按 `maxRetries` 和退避策略重新入队。清理失败会保留 `closing` 状态和错误，等待管理员重试结束。批处理适配器仍按 Pipeline 的重试策略执行。每轮执行有独立超时，使用初始会话快照中的 `timeoutSeconds`。

会话建立时保存 Worker、Workspace、Runner 和 Pipeline 执行配置的快照，避免中途修改资源导致同一会话使用不同模型、仓库或工具设置。后续轮次沿用该快照，实际密钥仍按环境变量引用解析；调度时读取 Pipeline 当前的暂停状态与并发限制。需要使用新的执行配置时，应关闭旧会话并开始新的 Run。

## API 和时间线

所有会话接口都要求管理员会话，并沿用现有 Origin 校验。

| 方法与路径 | 用途 |
| --- | --- |
| `GET /api/runs/:id` | 返回 Run、Event、产物、`session`、`capabilities` 和按轮次编号排序的 `turns` |
| `GET /api/runs/:id/timeline?after=0` | 按游标获取最多 1000 条结构化时间线事件 |
| `POST /api/runs/:id/messages` | 提交 `{"message":"补充说明"}` |
| `POST /api/runs/:id/interrupt` | 中断当前轮次 |
| `POST /api/runs/:id/resume` | 提交可选的 `{"message":"继续说明"}`，空输入使用默认继续指令 |
| `POST /api/runs/:id/close` | 明确结束会话并清理其运行存储 |

人工输入最多为 32 KiB UTF-8 文本。不存在的 Run 返回 404；不支持的交互或状态已变化的操作返回 409；输入格式错误返回 400。操作接受后返回 202，最终状态通过运行详情和时间线查询。

`capabilities` 包含 `interactive`、`canSend`、`canInterrupt`、`canResume`、`canClose`。这些字段描述当前可执行的操作，客户端仍需处理点击按钮后状态发生变化的情况。旧批处理 Run 返回 `session: null` 和关闭的交互能力。

时间线是追加式事件流，通过事件 `id` 递增读取。消息与工具项由 `(turnId, itemId)` 标识；`delta: true` 表示追加文本或工具输出，`delta: false` 表示替换该项的当前快照。`control` 事件的 `controlId` 指向待投递的用户消息，`accepted` 或 `rejected` 表示投递结果。结构化事件与原始日志使用不同的游标，不能混用。

## 持久化和恢复

Docker 为交互式会话保留命名卷 `cloudpilot-<deployment-runtime-id>-run-<run-id>-workspace`，挂载到 `/workspace`，保存仓库文件和 `.codex` 中的 Codex 会话数据。每轮使用独立的执行容器和新的 `/output`；容器正常退出、中断或服务关闭后，工作目录卷仍然保留。只有“结束任务”会清理会话卷。文件产物按轮次收集到服务的数据目录，后续轮次不会覆盖以前已下载的产物。需要跨轮继续使用的中间文件应放在 `/workspace`，`/output` 用于本轮交付。

服务启动时先清理当前部署遗留的执行容器。被中断的活动轮次标记为 `interrupted`，不会自动重放；已排队的人工作业继续等待调度。保留的工作目录、Codex thread ID 和会话文件用于继续执行。部署标识保存在 `runtime.data_dir/runtime-id`，恢复时必须与数据库、会话卷一起保留。

结束任务时，如果清理失败，会话停留在 `closing` 并记录错误，管理员可以再次请求结束。不要把尚未成功清理的会话视为已释放存储。需要回收磁盘空间时，通过运行详情结束不再使用的会话；直接删除卷会使对应会话失去可恢复的工作目录。

## 执行器扩展

`engine.Executor` 保持批处理接口。交互式适配器实现 `engine.InteractiveExecutor`，提供能力判断、轮次执行、活动控制和会话清理；统一调度器负责状态持久化、并发与时间线。Docker 中的交互式 Worker 还通过 `runtime.SessionWorker.PrepareSession` 提供双向进程入口。当前入口使用 Codex app-server 协议：`thread/start`、`thread/resume`、`turn/start`、`turn/steer` 和 `turn/interrupt`，再将协议事件转换为平台的消息和工具事件。接入其他协议时，需要提供相应的交互适配器或协议转换入口。

运行设施和 Agent 的交互能力由适配器声明，前端不假定每种 Worker 都支持会话继续。
