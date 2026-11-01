# 部署与开发

## 启动配置

复制仓库根目录的 `config.example.yaml` 为 `config.yaml`。服务使用 Viper 读取该 YAML；未知字段会报错。`-config` 可以指定其他文件。相对路径以进程工作目录为基准。

| 配置项 | 默认值 | 用途 |
| --- | --- | --- |
| `server.address` | `:8080` | HTTP API 与静态前端 |
| `server.secure_cookies` | `false` | HTTPS 部署时设为 `true` |
| `server.public_url` | 空 | GitHub issue 回复中使用的 WebUI 基址，启用此 Input 时必填 |
| `database.driver` | `sqlite` | `sqlite`、`postgres` 或 `mysql` |
| `database.dsn` | `data/cloudpilot.db` | 数据库连接串或 SQLite 文件 |
| `admin.username` | `admin` | 首次启动创建的管理员 |
| `admin.password` | 空 | 首次创建管理员使用的密码 |
| `runtime.data_dir` | `data/runs` | 运行产物存储目录 |
| `scheduler.poll_interval` | `2s` | 调度检查间隔，100ms 至 1m |
| `metrics.address` | 空 | 独立 Metrics 监听地址；留空禁用 |

配置可通过 `CLOUDPILOT_` 环境变量覆盖，层级分隔符替换为下划线，例如 `CLOUDPILOT_DATABASE_DSN`、`CLOUDPILOT_SERVER_SECURE_COOKIES`。管理员密码为 12–72 字节，应使用 `CLOUDPILOT_ADMIN_PASSWORD` 注入。已存在的账号不会因为配置文件变更而重置密码。管理员会话有效期为 24 小时。

前端内嵌在二进制中。升级时删除旧配置中的 `server.frontend_dir`，否则严格配置校验会拒绝启动。

GitHub issue 接入需要将 `server.public_url` 或 `CLOUDPILOT_SERVER_PUBLIC_URL` 设为用户可访问的 HTTP(S) WebUI 基址，不含账号密码、查询参数或 fragment，例如 `https://cloudpilot.example.com`。Webhook 签名密钥和用于回复 issue 的 GitHub token 通过资源中的环境变量引用提供，配置步骤见 [GitHub issue 接入](2026-11-01-github-issue-input.md)。

数据库在启动时执行 GORM 自动迁移。其他数据库的 DSN 示例：

```yaml
database:
  driver: postgres
  dsn: "host=127.0.0.1 port=5432 user=cloudpilot password=replace-me dbname=cloudpilot sslmode=require"
```

MySQL 使用 `user:password@tcp(host:3306)/cloudpilot?charset=utf8mb4&parseTime=True&loc=UTC`。生产部署通过环境变量提供包含密码的连接串，数据库由运维预先创建。

启动配置与资源配置分开管理：启动配置控制进程、数据库和初始账号；网页 YAML 导入只处理 Input、Pipeline、Worker、Workspace、Runner，不会修改数据库连接或管理员账号。

## Docker Compose

先准备配置和密钥，再构建两个镜像：

```sh
cp config.example.yaml config.yaml
cp examples/docker.env.example .env
# 编辑 .env，设置管理员密码、Webhook secret、模型 API key 和示例所需的 GitHub token。
make worker-image
docker compose up --build -d
```

管理界面位于 <http://localhost:8080>。Compose 默认只向本机暴露 8080 和 9090；Metrics 使用独立端口 <http://localhost:9090/metrics>。数据库和文件产物持久化在 `cloudpilot-data` 卷。配置文件通过只读挂载提供；修改后重启服务。

Compose 默认挂载 Docker socket，Docker runner 使用 Docker CLI 和 `/var/run/docker.sock` 创建执行容器。执行容器使用 `docker cp` 接收工作配置、返回产物，无需映射服务容器内部的主机路径，也不挂载 Docker socket。自定义 `DOCKER_HOST` 时应确保服务能访问对应 daemon；运行镜像必须存在于该 daemon 或可从镜像仓库拉取。

仅使用 E2B runner 时，CloudPilot 执行任务不需要 Docker daemon、CLI 或 socket，可直接运行服务二进制。若仍使用 Compose 托管服务，Compose 本身需要 Docker，但可移除 socket 挂载；远程模板和密钥配置见 [E2B / Novita runner](2026-11-01-e2b-runner.md)。

需要自定义代理 CA 的构建环境可向两个 Dockerfile 传入可选的 BuildKit secret：`docker build --secret id=proxy_ca,src=/path/to/ca.pem ...`。CA 仅在依赖安装步骤挂载，不写入镜像层；继续保留环境提供的代理设置和 TLS 验证。

Docker socket 使控制平面拥有管理 daemon 的权限。仅向受信任的管理员开放控制平面；Workspace 的 setup 命令、MCP、仓库及镜像均由管理员配置，执行代码可使用分配给该任务的凭据。生产环境应使用专用 runner 主机或隔离的 Docker daemon，并按实际任务权限配置模型及 GitHub 凭据。

## HTTPS 和数据备份

将服务放在 HTTPS 反向代理后，设置 `server.secure_cookies: true`，保留原始 `Host`。浏览器写请求会校验 `Origin` 与请求 `Host`，前端和 API 应部署在同一站点。代理只应转发受信任的客户端地址头。首次设置账号后，避免把启动配置、`.env` 或数据库文件提交到仓库。

数据库保存配置、事件、运行记录、日志、会话、轮次、远程实例记录、结构化时间线和产物元信息；`runtime.data_dir` 保存文件产物及部署标识。备份应覆盖数据库和完整数据目录。Docker 会话的仓库与 Codex 状态另存于 runner daemon 的命名卷中，不在 Compose 的 `cloudpilot-data` 卷内，需同时备份这些卷及 `runtime.data_dir/runtime-id`。E2B 会话状态保存在远端 sandbox，恢复需保留对应实例、服务商访问凭据及 `runtime.data_dir/e2b/runtime-id`；数据库和本地数据目录的备份不包含远端文件。SQLite 在线备份应使用 SQLite 的备份机制；复制工作目录或会话卷前应停止活动轮次。

当前版本只运行一个调度服务实例。多个实例共享数据库可能超出 Pipeline 并发配额，尚未提供 leader election 或分布式租约。停止服务会中断活动轮次并保留交互式工作目录；重启后由各 runner 核对并清理遗留执行环境，再将中断的交互式轮次标记为 `interrupted`，工作目录仍在时可人工继续。批处理执行仍按原重试策略恢复。

空闲和中断会话不占 Pipeline 并发额度，但继续占用 Docker 卷空间或远端 sandbox 存储。通过运行详情的“结束任务”清理不再需要的会话；清理失败会保留错误并允许重试。自然完成一轮不会自动删除会话工作目录，详情见 [Agent 会话与人工介入](2026-10-31-interactive-sessions.md)，远端存储费用与清理见 [E2B / Novita runner](2026-11-01-e2b-runner.md)。

## 开发和检查

`make install` 安装锁定的 Go 与前端依赖；`make dev` 先构建前端再启动 API；`make frontend-dev` 启动 Vite，代理 `/api`、`/hooks`、`/health` 到 `127.0.0.1:8080`。`make build` 先生成 `frontend/dist`，再将其内嵌到 `bin/cloudpilot`；部署不需要单独复制前端目录。直接使用 `go run`、`go build` 或 `go test` 前，先执行 `make frontend-build`。

`make test` 执行 Go 测试、`go vet`、前端时间线归并测试和 TypeScript/Vite 构建。CI 还执行 Go race 检测。Docker runner 的集成验证使用确定性的执行镜像，无需调用付费模型：

```sh
make worker-image
docker build -t cloudpilot-fixture:latest internal/runtime/testdata
docker build -t cloudpilot-session-fixture:latest internal/runtime/testdata/session
CLOUDPILOT_TEST_DOCKER_IMAGE=cloudpilot-fixture:latest go test ./internal/runtime -run TestDockerExecution -v
CLOUDPILOT_TEST_SESSION_IMAGE=cloudpilot-session-fixture:latest go test ./internal/runtime -run TestInteractiveDockerSession -v
CLOUDPILOT_TEST_CODEX_IMAGE=cloudpilot-codex:latest go test ./internal/runtime -run 'TestCodex(Interactive)?Protocol' -v
```

这些检查覆盖容器生命周期、产物收集、密钥脱敏、取消清理，以及交互式会话的补充信息、中断、跨容器恢复、工作目录保留和明确结束。真实 Codex 的协议检查连接本地模拟 Responses endpoint，均不调用付费模型。未设置相应镜像变量时，日常 `go test ./...` 跳过需要 daemon 的集成用例。

E2B / Novita 的协议、模板和真实运行验证见 [E2B runner 验收说明](2026-11-01-e2b-runner.md#验收)；这些集成检查显式提供凭据后才会访问远端服务。

设置 `CLOUDPILOT_TEST_POSTGRES_DSN` 或 `CLOUDPILOT_TEST_MYSQL_DSN` 后，运行 `go test ./internal/model -run TestDatabaseIntegration -v` 可检查对应数据库的迁移、长文本、幂等约束和事务回滚。必须使用名称以 `cloudpilot_test_` 开头的专用空测试数据库，不能指向生产数据库。
