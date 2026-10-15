# Agent 接口

监听在内网地址的 `8866` 端口，除 `/v1/version` 外全部走 mTLS：

| 方法与路径 | 说明 |
| --- | --- |
| `POST /v1/deploy` | 提交部署请求，body 为 `{"service": "...", "tag": "..."}` 或 `{"service": "...", "digest": "sha256:..."}`，返回 202 和 `deploy_id`。未知字段、同时带 tag 和 digest 返回 400；body 超过 64 KiB 返回 413 |
| `GET /v1/deploys/{id}` | 查询某次部署的结果，供客户端轮询 |
| `GET /v1/status/{service}` | 查看某服务当前运行状态，供人工排查 |
| `GET /v1/version` | 查询 agent 版本号，**唯一不需要证书的 `/v1/*` 接口**，见下文 |

`GET /healthz` 单独监听在 `127.0.0.1:8867`，不需要证书。

开启 OIDC 后（`serve --oidc-listen`），同样的 `POST /v1/deploy`、`GET /v1/deploys/{id}`、`GET /v1/status/{service}`、`GET /v1/version` 还会在 OIDC 入口上提供，用 `Authorization: Bearer <token>` 认证，不要求客户端证书，见 [OIDC 认证](auth_oidc.md)。

认证与授权失败的错误码：

| 状态码 | `code` | 含义 |
| --- | --- | --- |
| 401 | `unauthenticated` | OIDC 入口：没有 token，或 token 无效、过期、已使用。响应体不说明原因 |
| 403 | `access_denied` | OIDC 入口：token 有效，但没有绑定该服务。响应体不说明是哪项不匹配 |
| 403 | `cert_not_registered` / `cert_revoked` | 证书入口：证书未登记或已吊销 |
| 403 | `cert_not_allowed` | 证书入口：证书未绑定该服务（`access`），或请求没有任何身份 |

所有监听都设置了连接限制：请求头（含 TLS 握手）必须在 10 秒内收完，整个请求 30 秒，响应 30 秒，空闲连接 60 秒后关闭，请求头最大 16 KiB，请求体最大 64 KiB（并且只能是一个 JSON 值，后面只允许空白，否则 400）。`:8866` 同时打开的连接数默认最多 64 个（`serve --max-conns` 调整），超出的连接在内核队列里等待空位，而不是各占一个 goroutine。这是全局上限而非按来源 IP：放在四层透传代理后面时，所有连接的来源地址都是代理。慢速连接无法长期占用资源，因此 `:8866` 可以放在四层透传之后对外暴露。

`GET /v1/version` 是一个特例：它与其他 `/v1/*` 接口共享同一个 `:8866` 端口，但不要求客户端证书，直接明文 HTTP 访问即可：

```bash
curl http://10.0.0.5:8866/v1/version
# 1.0.0
```

用途：流水线在拿到客户端证书、甚至在下载客户端二进制之前，就需要知道目标 agent 的版本号，以便下载与之匹配的客户端版本（[Client 使用说明](client.md) 第 2 节的 `DEPLOY_AGENT_CLIENT_VERSION` 仍然必须固定，不能用 `latest`；`/v1/version` 只是帮助确定该固定成哪个值）。除这一个接口外，`:8866` 上的其他路径不受影响，依然强制 mTLS。
