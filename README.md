# CloudPilot

可插拔的 Agent 调度与观测平台。Webhook 接收事件，Pipeline 管理队列和调度，Worker 定义 Agent，Workspace 准备仓库、提示词和工具，Runner 提供执行环境，Artifact 保存运行产物。

- 后端：Go、Echo v5、MVC、GORM、Viper；支持 SQLite、PostgreSQL 和 MySQL。
- 前端：React、TypeScript、Carbon Design System、Vite、pnpm，代码位于 `frontend/`。
- 已适配：Webhook input、Codex worker、Docker 与 E2B 兼容 runner（含 Novita）。资源支持网页编辑与声明式 YAML 导入导出。

## 本地开发

需要 Go 1.27.3+、Node.js 24、pnpm 12.16.1；使用 Docker runner 时还需要可访问的 Docker daemon。启动管理界面和 API 不需要模型密钥；执行任务时需要配置相应环境变量。

```sh
cp config.example.yaml config.yaml
export CLOUDPILOT_ADMIN_PASSWORD='replace-with-a-strong-password'
make install
make dev
```

另一终端执行 `make frontend-dev`，访问 <http://localhost:5173>。管理员用户名默认 `admin`；密码来自启动配置，只在首次创建账号时使用。生产构建执行 `make build`，随后运行 `./bin/cloudpilot -config config.yaml`，访问 <http://localhost:8080>。

`make test` 运行 Go 测试、静态检查与前端构建。`make worker-image` 构建 Codex 执行镜像。

## 项目结构

| 路径 | 职责 |
| --- | --- |
| `cmd/cloudpilot` | 配置、服务启动与关闭 |
| `internal/model` | GORM 模型和数据库迁移 |
| `internal/controller`、`internal/views` | 请求处理和 Echo 路由 |
| `internal/resource` | 声明式资源校验与导入导出 |
| `internal/engine`、`internal/runtime` | 队列调度与执行适配器 |
| `frontend` | Carbon 管理界面 |
| `deploy/worker` | Codex 执行镜像 |
| `deploy/sandbox` | Novita sandbox 模板构建 |

配置与部署见 [部署文档](docs/2026-10-31-deployment.md)，远程执行见 [E2B / Novita runner](docs/2026-11-01-e2b-runner.md)，工作流与扩展见 [资源和执行说明](docs/2026-10-31-resources-and-execution.md)，对话、工具调用与人工介入见 [交互式会话](docs/2026-10-31-interactive-sessions.md)。完整接口见 [OpenAPI](openapi.yaml)，夜间反馈处理示例见 [examples/nightly-feedback.yaml](examples/nightly-feedback.yaml)。
