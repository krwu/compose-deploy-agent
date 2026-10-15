# Client 使用说明（在 CI 流水线中调用）

## 1. 前置条件

- 已经在目标 Agent 上执行过 `deploy-agent ca issue`，并把输出的 PEM 证书包存入 CI 密钥，由流水线注入环境变量 `DEPLOY_AGENT_CREDENTIAL`
- 已确认要纳管的服务名与 `services.yaml` 中的逻辑服务名一致
- 已确定要部署的镜像 tag（建议用 commit SHA 或构建号，便于回溯；`test`、`latest` 这类可变 tag 也能部署，只是没法靠 tag 判断跑的是哪次构建）
- 已确定流水线使用的 `deploy-agent` 客户端版本号。建议固定版本号（**不要用 `latest`**），避免 agent 发布新版后流水线客户端行为跟着漂移

## 2. 在脚本任务中调用

参考模板见 [scripts/ci-deploy.sh](../scripts/ci-deploy.sh)，核心用法：

```bash
# 从 GitHub Releases 下载固定版本的客户端二进制
# 注意：下载会经过一次 302 重定向，curl 必须带 --location
curl --fail --location -o deploy-agent \
  "https://github.com/krwu/compose-deploy-agent/releases/download/${DEPLOY_AGENT_CLIENT_VERSION}/deploy-agent"
chmod +x deploy-agent

# 凭据只从环境变量读取，不通过命令行参数传递
export DEPLOY_AGENT_CREDENTIAL="<CI 密钥引用，内容为 ca issue 输出的多行 PEM>"

./deploy-agent deploy \
  -e "https://10.0.0.5:8866" \
  -s "web" \
  -t "${GIT_COMMIT_SHORT}"
```

命令行参数说明：

| 参数 | 简写 | 必填 | 说明 |
| --- | --- | --- | --- |
| `--endpoint` | `-e` | 是 | Agent 地址，形如 `https://<vps-ip>:8866`。主机名也可以是服务端证书 SAN 里的域名 |
| `--service` | `-s` | 是 | 逻辑服务名，须在目标机 `services.yaml` 白名单内 |
| `--tag` | `-t` | 与 `--digest` 二选一 | 目标镜像 tag，格式 `^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`。建议用不可变 tag（commit SHA、构建号），可变 tag 也能部署，但回滚与幂等判断都以 tag 为准 |
| `--digest` | `-d` | 与 `--tag` 二选一 | 目标镜像 digest，格式 `^sha256:[a-f0-9]{64}$`，只用于清单中配置了 `image_ref: digest` 的服务 |
| `--credential-env` | 无 | 否，默认 `DEPLOY_AGENT_CREDENTIAL` | 存放 PEM 证书包的环境变量名 |
| `--poll-timeout` | 无 | 否，默认 `10m` | 提交重试与状态轮询的总时间预算 |

OIDC 模式（不用证书，见下节）额外的参数：

| 参数 | 说明 |
| --- | --- |
| `--oidc-audience` | 向 GitHub Actions 申请该 audience 的 OIDC token（读取 `ACTIONS_ID_TOKEN_REQUEST_URL` / `ACTIONS_ID_TOKEN_REQUEST_TOKEN`，job 需要 `permissions: id-token: write`），并改连 agent 的 OIDC 入口 |
| `--oidc-token-env` | 存放现成 OIDC token 的环境变量名，与 `--oidc-audience` 二选一 |
| `--server-ca-file` | 服务端证书必须由这份 CA 签发，并且 SAN 包含 `--endpoint` 的主机名 |
| `--server-cert-sha256` | 服务端证书 SHA-256 指纹锁定；与 `--server-ca-file` 至少配一个 |

### OIDC 模式（GitHub Actions）

agent 开启 OIDC 入口（见 [OIDC 认证](auth_oidc.md)）后，GitHub Actions 不需要保存客户端证书。客户端每次运行时申请 token，token 有效期只有几分钟，并且只能部署清单里绑定的服务。证书模式不受影响，`DEPLOY_AGENT_CREDENTIAL` 在 OIDC 模式下不使用。

```yaml
name: deploy-main
on:
  push:
    branches: [main]

permissions:
  contents: read
  id-token: write          # 申请 OIDC token 必需

jobs:
  # build: ...                   # 构建并推送镜像，outputs.digest 为镜像 digest（此处省略）
  deploy:
    needs: build
    runs-on: ubuntu-latest
    environment: preview-main    # 与清单规则的 environment 一致
    steps:
      - name: Download deploy-agent client
        env:
          DEPLOY_AGENT_CLIENT_VERSION: "1.2.0"     # 固定版本，不要用 latest
        run: |
          curl --fail --location -o deploy-agent \
            "https://github.com/krwu/compose-deploy-agent/releases/download/${DEPLOY_AGENT_CLIENT_VERSION}/deploy-agent"
          chmod +x deploy-agent

      - name: Deploy
        env:
          # agent 的 CA 证书（公开内容，不是私钥），也可以改用 --server-cert-sha256
          DEPLOY_AGENT_SERVER_CA: ${{ vars.DEPLOY_AGENT_SERVER_CA }}
        run: |
          printf '%s\n' "$DEPLOY_AGENT_SERVER_CA" > agent-ca.pem
          ./deploy-agent deploy \
            -e "https://deploy-oidc.img.ntest.top" \
            -s main \
            -d "${{ needs.build.outputs.digest }}" \
            --oidc-audience "https://deploy.img.ntest.top" \
            --server-ca-file agent-ca.pem
```

- `--oidc-audience` 必须显式传入，并与清单 `oidc.audience` 完全一致；客户端不会使用 GitHub 默认 audience
- 示例中的 `-d` 假设前面有一个输出镜像 digest 的 `build` job；tag 模式的服务改用 `-t`
- 客户端缓存 token，到期前 1 分钟重新申请，所以轮询超过 token 有效期也没问题；`--oidc-token-env` 传入的固定 token 过期后轮询会失败
- token 不会出现在客户端输出里。认证失败（401 `unauthenticated`）和绑定不匹配（403 `access_denied`）退出码都是 `2`，具体原因看 agent 日志

## 3. 行为说明

- 提交后立即受理为异步任务，客户端自动轮询直到部署完成或超时
- 同一服务同一 tag 重复提交会复用同一次部署（幂等），不会重复重启
- 同一服务不同 tag 正在部署时返回冲突，客户端会在总超时内自动退避重试
- 部署失败（`start_failed`、`post_deploy_failed`）时 Agent 会自动尝试回滚，客户端只反映最终结果

## 4. 输出与退出码

执行结束后，结果以 JSON 打印到 stdout（凭据全程不会出现在输出中）：

```json
{
  "deploy_id": "5a330bbe...",
  "service": "web",
  "tag": "a1b2c3d",
  "result": "success",
  "rollback": "none",
  "image_id": "sha256:...",
  "exit_code": 0
}
```

| 退出码 | 含义 |
| --- | --- |
| `0` | 部署成功（`success` 或 `noop`） |
| `1` | 部署失败（`pull_failed` / `config_invalid` / `start_failed` / `post_deploy_failed` / `interrupted`） |
| `2` | 参数错误或认证失败（凭据无效、证书未登记/已吊销/未绑定该服务、OIDC token 无效或未绑定该服务、service 不在白名单等） |
| `3` | 轮询总超时 |

流水线应将脚本任务的非零退出码视为部署失败，禁止把失败降级为警告。
