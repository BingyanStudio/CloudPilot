# 用户、权限与单点登录

## 权限

| 权限 | 能做什么 |
| --- | --- |
| 管理员 `admin` | 编辑者的全部能力，加上管理用户和单点登录配置 |
| 编辑者 `editor` | 创建、修改、删除资源，导入 YAML，触发流水线，重试事件，向 Agent 发送消息、中断、继续、结束和取消运行 |
| 查看者 `viewer` | 查看资源、事件、运行进度、日志、产物，导出 YAML |

权限在服务端由 `controller.RequireRole` 逐路由校验，前端只负责隐藏不可用的操作。角色变更立即生效；重置密码或删除用户会注销该用户的所有会话。管理员不能修改自己的角色或删除自己，因此系统中始终至少保留一个管理员。

启动配置中的 `admin` 账户在首次启动时以管理员身份创建。从无角色版本升级的数据库会把最早创建的用户提升为管理员。

## OIDC 单点登录

在 配置中心 → 单点登录 中填写：

- Discovery URL：Issuer 地址，或完整的 `/.well-known/openid-configuration` 地址。保存时会拉取并校验该文档，Issuer 以文档中声明的为准。
- Client ID / Client secret：secret 加密保存，留空表示保留已有值。
- Scope：空格分隔，`openid` 自动加入。
- SSO 用户默认权限：只在该身份首次登录时使用，之后在用户管理中调整。

页面显示的回调地址需要在身份提供方注册。回调地址优先使用 `server.public_url`，未配置时取当前请求的协议和 Host；部署在反向代理后时请配置 `public_url`。

登录流程使用 state、nonce 和 PKCE（S256），三者保存在仅限 `/api/auth/oidc` 路径、10 分钟有效的 HttpOnly cookie 中。ID token 由 go-oidc 校验签名、Issuer、Audience 和有效期。

账户按 Issuer + Subject 识别，不会与同名本地账户合并；用户名优先取 `preferred_username`，其次 `email`、`name`，冲突时追加短后缀。SSO 账户没有本地密码。删除 SSO 用户后，对方再次登录会以默认权限重新创建；如需禁止访问，请在身份提供方侧撤销授权。
