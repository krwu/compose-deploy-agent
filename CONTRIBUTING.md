# 贡献指南

欢迎提 issue 和 PR。这个仓库目前由我一个人在维护，改动节奏比较快，所以提交前花几分钟看完这篇，能省掉后面来回沟通的时间。

## 环境

Go 版本跟 `go.mod` 走（`go 1.27.0`）。本地跑单测不需要 docker；要跑集成测试则需要 docker 和 docker compose，且脚本会真实拉取镜像、起容器。

```bash
git clone https://github.com/krwu/compose-deploy-agent.git
cd compose-deploy-agent
make build     # 产出 bin/deploy-agent，当前平台
make test      # go test ./...
make lint      # gofmt -l . 加 go vet ./...
```

`make lint` 有输出就是有问题，`gofmt -l` 列出文件说明格式没过，`go vet` 会直接报错。提交前这两个都得干净。不想记命令的话，直接 `make lint && make test`。

## 改代码

二进制入口在 `cmd/deploy-agent/`，一个子命令一个文件：`serve.go`、`ca_cmd.go`、`deploy_cmd.go`、`setup_cmd.go`、`update_cmd.go`、`self_cmd.go`。加子命令就照着现有文件的模式加，不要把逻辑堆进 `main.go`。

业务逻辑放 `internal/`，按职责分包：`internal/deployer` 负责部署与回滚，`internal/ca` 管证书，`internal/store` 是 sqlite 持久化，`internal/config` 读 `services.yaml`。跨包的东西不要反向依赖 `cmd`。

几个不太显然、改的时候容易踩的点：

- `deployer` 里的 docker 调用走了一层 fake，单测可以不开 docker 就跑通。改了 docker 调用方式，记得同步改 fake，否则单测会假绿。
- 清单即白名单。往 `services.yaml` 之外放行服务、或者放宽清单校验，都属于改安全模型，PR 里说清楚为什么。
- `pull_policy` 为 `never` 和 `build` 是被预检拒绝的，这个判断在预检里，别顺手删掉。
- agent 的 HTTP 接口不提供签发证书的能力，`ca` 系列命令只能在本机执行。给接口加能力之前先想清楚信任边界。

## 测试

单测跟代码一起提交，放在同包下的 `*_test.go`。表驱动测试为主，跟现有风格保持一致。

`internal/deployer` 之外还有一个端到端脚本：

```bash
scripts/integration_test.sh
```

它会 build 二进制、建临时数据目录、起真实容器，覆盖单测覆盖不到的部分（真实 `docker compose pull/up`、HEALTHCHECK 语义、`.env` 与实际运行容器是否一致）。改动涉及部署流程、回滚、`.env` 写入的话，本地跑一遍再提。脚本结束会清理容器和临时目录。

## 提交

用 Conventional Commit，一条提交只做一件事：

```text
feat(deploy): 支持复用同 tag 的部署
fix(ca): 重签服务端证书时保留原 SAN
docs: 补充 setup 向导的清单生成说明
ci: 用 GitHub Actions 发布 Release
```

类型用 `feat` / `fix` / `docs` / `refactor` / `test` / `ci` / `chore`，作用域写包名或模块名。一个 PR 里如果既有功能又有文档又有 CI 改动，拆成多条提交，review 时好按提交逐个看。

## PR

- 从 `main` 拉分支，分支名跟着内容走，比如 `fix/ca-reissue-san`
- 描述里写清楚改了什么、为什么改、怎么验证的。改了行为就附上运行输出
- 涉及用户可见变化的（新增参数、退出码、清单字段、接口），同步改 [README.md](./README.md) 或 [docs/](./docs/) 下对应的文档
- 别往仓库里提交真实凭据：PEM 证书包、`serve.env`、`.db`、本机 `services.yaml`。`.gitignore` 已经挡掉了 `bin/`、`dist/` 和数据库文件，其余自己留意

## 协议

代码以 MIT 协议发布，见 [LICENSE](./LICENSE)。你提交的贡献默认同样按 MIT 授权。
