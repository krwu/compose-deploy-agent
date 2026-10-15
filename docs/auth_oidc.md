# OIDC 认证：GitHub Actions 免证书部署

除了 mTLS 客户端证书，agent 还可以接受 GitHub Actions 签发的 OIDC ID token。流水线不用保存任何长期凭据：每次运行时向 GitHub 申请一个几分钟有效、只给本 agent 用的 token，agent 校验签名和声明后，再按清单里的绑定规则决定能部署哪个服务。

OIDC 默认关闭。不传 `serve --oidc-listen` 时，agent 的行为与没有这项功能时完全一样。

## 独立入口

OIDC 不和证书共用端口，而是单独监听一个地址：

| 入口 | 监听 | TLS | 身份来源 |
| --- | --- | --- | --- |
| 证书入口 | `--bind`（如 `10.0.0.5:8866`） | agent 自己终止 TLS，`RequireAndVerifyClientCert` | 已登记、未吊销的客户端证书 |
| OIDC 入口 | `--oidc-listen`（如 `10.0.0.5:8868`） | agent 自己终止 TLS，沿用同一张服务端证书，不请求客户端证书 | `Authorization: Bearer <OIDC token>` |
| 本机 socket | `/run/deploy-agent/deploy-agent.sock` | 无 | 主机 root 权限 |

- 证书入口的 TLS 配置完全不变，仍然是没有客户端证书就握手失败；在证书入口带 Bearer token 但不带证书，连 HTTP 层都到不了
- OIDC 入口只认 Bearer token：没有 token、格式不对、签名或声明校验失败、token 已经用过，一律 401 `unauthenticated`，带 `WWW-Authenticate: Bearer`；客户端证书在这里不起作用
- 每个入口都显式写入自己的身份，之后统一进入同一个权限检查点（证书查 `access`，OIDC 查服务的 `oidc` 规则，本机 socket 放行）。没有身份的请求仍然返回 403
- 两个 TLS 入口使用同样的超时（请求头 10 秒、请求 30 秒、响应 30 秒、空闲 60 秒）、16 KiB 请求头上限和 64 KiB 请求体上限；`--max-conns` 对两个入口分别生效（各自最多 N 个连接）

### 开启

1. 在清单里配置 `oidc.audience` 和服务的 `oidc` 规则，见 [清单字段说明](manifest.md#github-actions-oidc-绑定oidc可选)
2. 服务端证书要包含 OIDC 入口对外的域名。线上用 Traefik 按 SNI 做 TCP 透传时，把域名加进证书（列全现有的 IP 和域名）：

   ```bash
   sudo deploy-agent ca reissue-server --ip 10.0.0.5,deploy.img.ntest.top,deploy-oidc.img.ntest.top
   ```

3. 给 serve 加上 `--oidc-listen`。`install.sh` 不会覆盖已有的 unit，建议用 drop-in：

   ```ini
   # /etc/systemd/system/deploy-agent.service.d/oidc.conf
   [Service]
   ExecStart=
   ExecStart=/usr/local/bin/deploy-agent serve --bind ${DEPLOY_AGENT_BIND} --oidc-listen ${DEPLOY_AGENT_OIDC_LISTEN} --manifest /etc/deploy-agent/services.yaml --data-dir /var/lib/deploy-agent
   ```

   并在 `/etc/deploy-agent/serve.env` 里加 `DEPLOY_AGENT_OIDC_LISTEN=10.0.0.5:8868`，然后 `systemctl daemon-reload && systemctl restart deploy-agent`

4. 用 `serve -t` 检查（同样带上 `--oidc-listen`）。配置了 `--oidc-listen` 但清单没有 `oidc.audience` 时拒绝启动；反过来配置了 audience 却没开监听，会打印告警
5. agent 需要能访问 `https://token.actions.githubusercontent.com` 拉取签名公钥

## token 校验

| 项目 | 规则 |
| --- | --- |
| 签名算法 | 只允许 `RS256`，`none`、`HS256`、`RS512` 等一律拒绝；header 必须有 `kid`，带 `crit` 拒绝 |
| 公钥 | 只从写死的 `https://token.actions.githubusercontent.com/.well-known/jwks` 获取，不读 token 里的任何地址。首次使用时拉取并缓存；之后只有遇到未知 `kid` 才刷新，每分钟最多一次（失败也计数），请求超时 10 秒、响应最大 1 MiB，RSA 公钥至少 2048 位 |
| `iss` | 必须严格等于 `https://token.actions.githubusercontent.com`（写死） |
| `aud` | 必须等于清单里的 `oidc.audience`，数组形式只接受恰好一个元素。不接受 GitHub 默认的 `https://github.com/<owner>`，audience 为空时 OIDC 不可用 |
| `exp` / `nbf` / `iat` | 三者都必须存在且通过，时钟误差 30 秒；`exp - iat` 不能超过 1 小时 |
| 必备声明 | `jti`、`repository_id`、`repository_owner_id`、`ref`、`event_name` |

## 绑定与权限

token 通过校验只说明「这是 GitHub 为本 agent 签发的」，能部署哪个服务由清单里每个服务的 `oidc` 规则决定：

- 每个 claim 单独比较（`repository_id`、`repository_owner_id`、`repository`、`ref`、`event_name`、`environment`、`job_workflow_ref`），从不拼 `sub` 字符串去比
- 仓库用数字 ID 绑定：仓库改名、删除后被别人重新注册同名仓库，ID 都不会一样
- `event_name` 为 `pull_request_target` 或 `workflow_run` 的 token 一律拒绝，即使规则里写了也不行（清单加载时就会报错）
- 没有 `oidc` 规则的服务，OIDC token 一律 403；有规则的服务，证书入口的行为不变
- 不通过时返回 403 `access_denied`，响应体只有 `access denied`；具体哪个 claim 不匹配写在 agent 日志里

## 防重放

- `POST /v1/deploy` 在签名、`iss`、`aud`、时间和绑定规则全部通过之后，才把 `jti` 写进 agent 的 SQLite（`oidc_jti` 表），保存到 `exp` 加 30 秒
- 主键冲突即重放，原子判断，并发的两个相同 token 只会有一个成功；重启后记录仍在
- 重放返回 401 `unauthenticated`。伪造签名、错误 audience、绑定不匹配的 token 不会写入 `jti`，不会占用空间，也不会「烧掉」合法 token
- 部署没有真正开始时（例如 409 已有部署在进行），`jti` 会被释放，客户端可以用同一个 token 重试；一个 token 最多启动一次部署
- 状态查询（`GET /v1/deploys/{id}`、`GET /v1/status/{service}`）同样要求有效 token 和绑定，但不消耗 `jti`，所以客户端可以用同一个 token 轮询
- 过期记录每 10 分钟清理一次

## 日志与响应

- token 和 `Authorization` 头不会出现在日志、错误信息和部署记录里；日志只记录 `repository`、`repository_id`、`ref`、`event_name`、`environment`、`job_workflow_ref`、`actor`、`run_id`、`jti` 等声明
- 401 / 403 的响应体只有固定文字，不说明是哪项校验失败

## 客户端如何校验服务端证书

连 OIDC 入口时，客户端绝不关闭服务端证书校验，必须至少配置下面一种锁定方式：

| 参数 | 校验方式 |
| --- | --- |
| `--server-ca-file <pem>` | 只信任这份 CA（通常是 agent 的 `/var/lib/deploy-agent/ca/ca.crt`，它是公开的证书，不是私钥）。正常校验证书链、有效期，并校验 `--endpoint` 的主机名在服务端证书 SAN 里 |
| `--server-cert-sha256 <hex>` | 服务端叶子证书（DER）的 SHA-256 必须完全一致，可带冒号、大小写不限，同时检查证书在有效期内。只配这一项时，用指纹比对代替证书链和主机名校验 |
| 两者都配 | 两项校验都要通过 |

在 agent 上获取指纹：

```bash
sudo openssl x509 -in /var/lib/deploy-agent/server.crt -outform DER | sha256sum
```

注意 `ca reissue-server` 会换掉服务端证书，指纹随之变化；用 CA 文件锁定则不受影响。TLS 最低版本为 1.3。

客户端用法和 workflow 示例见 [Client 使用说明](client.md#oidc-模式github-actions)。
