# mTLS 认证：证书签发与绑定

## 证书管理

Agent 使用每机自签的本地 CA，只信任自己签发的客户端证书。安装向导已经完成 CA 和服务端证书的初始化；手动配置时用 `ca init` 完成。以下命令均需在目标机上执行：

```bash
# 生成本机 CA + 服务端证书（仅手动配置时需要；--ip 为本机内网 IP，会写入服务端证书的 SAN）
deploy-agent ca init --ip 10.0.0.5

# 客户端还会用其他 IP 或域名访问时，把它们和绑定 IP 写在同一次 --ip 里。
# 证书同时带 IP SAN 和 DNS SAN，https://10.0.0.5:8866 和 https://agent.example.com:8866 都能通过校验
deploy-agent ca init --ip 10.0.0.5,10.0.0.6,agent.example.com

# 为一条流水线签发客户端证书，--name（或简写 -n）用于标识用途，建议用流水线名
deploy-agent ca issue -n ci-web
```

`ca issue` 会把 PEM 证书包（客户端证书 + 私钥 + 服务端 CA）打印到 stdout，**只显示一次，agent 不会持久化这份内容**。把它完整复制，作为多行文本存入 CI 密钥，由流水线注入环境变量 `DEPLOY_AGENT_CREDENTIAL`。

其他证书管理命令：

```bash
deploy-agent ca list                        # 查看所有已签发证书（指纹、名称、有效期、是否吊销）
deploy-agent ca revoke -f <fingerprint>      # 吊销单个证书（-f/--fingerprint），立即生效，不影响其他证书
deploy-agent ca reissue-server                 # 续期：沿用当前证书里的 IP 和域名
deploy-agent ca reissue-server --ip 10.0.0.5,agent.example.com
```

`ca reissue-server` 省略 `--ip` 时，新证书沿用当前服务端证书上的 IP 和域名，只更换证书本身（用于到期续期）。传入 `--ip` 时，新证书只包含这一次列出的名字，因此改地址时要列全（如 `--ip 10.0.0.5,10.0.0.6,agent.example.com`），至少要有一个 IP。重签只替换服务端证书，不动 CA，已签发的客户端证书无需更新。

所有 `ca` 子命令都必须在目标机上通过 SSH 手动执行，agent 的 HTTP 接口不提供签发能力。

默认数据目录是 `/var/lib/deploy-agent`，如需自定义，所有 `ca` 子命令都支持 `--data-dir` 参数。

证书与服务的绑定（`access`）见 [清单字段说明](manifest.md#客户端证书与服务绑定access可选)。
