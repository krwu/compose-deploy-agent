# deploy-agent

Compose 服务镜像自动更新 agent：CI 流水线镜像推送完成后，自动触发目标 VPS 上的 `docker compose` 服务更新，失败自动回滚。MIT 协议发布，见 [LICENSE](./LICENSE)。

`deploy-agent` 是单一 Go 静态二进制，通过子命令区分角色：

- **Agent（`serve` / `ca`）**：部署在目标 VPS 上的常驻服务，由 systemd 托管。
- **Client（`deploy`）**：部署在 CI 流水线脚本任务中，用来提交部署请求。

两者默认通过 mTLS 通信，身份由本机自签的证书体系承载；也可以另开一个入口，接受 GitHub Actions 的 OIDC token（见 [OIDC 认证](docs/auth_oidc.md)）。

## 目录结构

```text
cmd/deploy-agent/        # 二进制入口（serve / setup / ca / deploy / update / self-check / self-upgrade / version）
internal/                # 各功能模块
deploy/                  # systemd unit、清单示例、安装脚本
scripts/                 # CI 脚本模板、docker 集成测试
```

## 构建

```bash
make build            # 产出 bin/deploy-agent（当前平台）
make test             # 运行单测
```

也可以手动交叉编译：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags "-X main.version=1.0.0" \
  -o deploy-agent ./cmd/deploy-agent
```

---

## 特性

- 单一 Go 静态二进制，子命令区分 Agent（`serve` / `ca` / `setup` / `update` / `self-*`）与 Client（`deploy`）
- 每机自签 CA 的 mTLS 认证，证书可单独吊销，可按证书绑定可部署的服务（`access`）
- 可选的 GitHub Actions OIDC 认证：独立入口、免长期凭据，按仓库/owner 数字 ID、ref、事件、environment、workflow 绑定服务，jti 防重放
- 清单即白名单：只有 `services.yaml` 里的服务可以部署，请求无法注入任何执行参数
- 部署失败（拉取、预检、就绪、`post_deploy`）自动回滚到上一个健康版本，同 tag 重复提交幂等
- 可选只接受镜像 digest（`image_ref: digest`）和部署后固定命令（`post_deploy`）
- 交互式安装向导，自动扫描本机 compose 项目生成清单
- 本机 unix socket 上的 `update` 命令，SSH 上机即可手动部署
- 连接超时、请求体上限与连接数上限，可放在四层透传之后对外暴露

## 快速开始

在目标 VPS 上安装并完成向导（绑定内网 IP、生成 CA 和服务端证书、扫描生成 `services.yaml`、启动服务）：

```bash
scp deploy/install.sh vps:/tmp/install.sh
ssh -t vps bash /tmp/install.sh latest
```

为流水线签发客户端证书，把输出的 PEM 证书包存入 CI 密钥：

```bash
ssh vps sudo deploy-agent ca issue -n ci-web
```

在流水线里提交部署并等待结果：

```bash
export DEPLOY_AGENT_CREDENTIAL="<CI 密钥引用，内容为 ca issue 输出的多行 PEM>"
./deploy-agent deploy -e "https://10.0.0.5:8866" -s web -t "${GIT_COMMIT_SHORT}"
```

## 文档索引

| 文档 | 内容 |
| --- | --- |
| [docs/agent_install.md](docs/agent_install.md) | 安装、安装向导、systemd、日常运维、`update`、`self-upgrade`、数据目录与 socket 目录权限 |
| [docs/manifest.md](docs/manifest.md) | `services.yaml` 全部字段：基础字段、向导生成、`image_ref: digest`、`post_deploy`、`access`、`oidc` |
| [docs/auth_mtls.md](docs/auth_mtls.md) | 本机 CA、客户端证书签发、吊销与服务端证书重签 |
| [docs/auth_oidc.md](docs/auth_oidc.md) | GitHub Actions OIDC 认证：独立入口、token 校验、绑定规则、防重放、客户端锁定服务端证书 |
| [docs/client.md](docs/client.md) | 在 CI 中调用 `deploy-agent deploy`、参数、行为与退出码、GitHub Actions OIDC workflow 示例 |
| [docs/api.md](docs/api.md) | Agent HTTP 接口、连接限制、`/v1/version` |

## 参与开发

- 贡献指南（环境、代码结构、提交与 PR 要求）：[CONTRIBUTING.md](./CONTRIBUTING.md)
- 开源协议：[LICENSE](./LICENSE)

## 参考文档

- 端到端集成测试（需要 docker 环境）：[scripts/integration_test.sh](./scripts/integration_test.sh)
