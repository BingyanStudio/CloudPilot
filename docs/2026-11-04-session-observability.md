# 会话活动与用量观测

Pipeline 的 `stallTimeoutSeconds` 控制交互轮次的无活动超时，范围为 0–86400 秒。省略或填写 0 表示禁用，旧资源和旧会话快照保持原有行为；网页新建 Pipeline 时建议值为 300 秒。该设置从 `turn/start` 成功后生效，遵循会话配置快照。

有效 app-server 协议输入刷新活动时间，包括没有展示到时间线的通知。日志输出、网页轮询和人工输入不刷新活动时间；时间线达到输出上限后仍会检测活动。已有 `timeoutSeconds` 继续限制整轮执行时间，RPC 请求仍有独立超时。

无活动超时将本轮标记为失败，保留 thread 和工作目录，等待人工继续，不自动重放。长时间没有输出的构建、测试或远端工具也可能达到阈值，应根据任务调高阈值或禁用。工作目录准备和 app-server 初始化继续受整轮及 RPC 超时约束。

运行详情展示最近协议活动及线程累计输入、输出、总 token。`usageUpdatedAt` 为 null 时显示“未提供”，不把缺失上报解释为零。用量只读取 `thread/tokenUsage/updated.tokenUsage.total`，忽略 `last`；同一 thread 恢复后重复上报不叠加，倒退、负值、缺字段或其他 thread/turn 的报告不更新累计值。恢复会话返回不同 thread ID 时执行失败，避免把不同线程的用量混在一起。

用量和活动独立于时间线输出限额持久化。活动时间最多每秒写入一次，并在轮次退出时刷新；token 数据是 provider 报告的运行观测值，不作为账单依据。数据库迁移自动新增字段，无需重建已有会话。

协议依据：[OpenAI app-server 文档](https://developers.openai.com/codex/app-server/)，字段经本机 Codex `0.166.0-alpha.6` 的 `ThreadTokenUsageUpdatedNotification` JSON Schema 核对。
