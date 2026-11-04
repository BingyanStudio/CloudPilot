# 执行成功与任务完成

交互式 Agent 正常结束一轮，只表示该轮执行成功。CloudPilot 将 Run 保持为 `succeeded`，Event 设为 `awaiting_review`，并保留 `idle` 会话供检查和继续处理。

操作者检查产物后，通过现有 `POST /api/runs/:id/close` 确认完成并结束会话。清理成功时，会话变为 `closed`，待确认 Event 在同一数据库事务中变为 `succeeded`。清理失败时保留 `closing`、错误信息和 `awaiting_review`，可重试结束；关闭失败、中断或取消的执行不会把任务标为成功。

人工继续将 Event 设为 `waiting`，调度后为 `running`，正常结束后再次进入 `awaiting_review`。未完成事件及未关闭的会话阻止所属 Pipeline 被删除。

运行列表将 `succeeded` 显示为“执行成功”；任务列表区分“待确认”和“已完成”。概览分别统计成功执行、待确认任务与已完成任务。

批处理执行器保留原有成功语义。升级不会批量改写历史已有的 `succeeded` 事件；这些旧会话如果继续执行，之后采用新的待确认状态。API 使用者需接受新增状态 `awaiting_review`，并仅在确认任务结果后关闭成功会话。
