# Agent 安装与运维（部署在目标 VPS 上）

## 安装

`deploy/install.sh` 是自包含脚本（不依赖同目录下的其他文件），**不需要把整个仓库 clone 到 VPS**，只要把这一个文件传上去即可，例如：

```bash
scp deploy/install.sh vps:/tmp/install.sh
ssh -t vps bash /tmp/install.sh {version}   # {version} 换成具体版本号，或用 latest
```

注意 `ssh` 要带 `-t` 分配终端，否则无法进入下面的交互式向导（见「无人值守安装」）。也可以直接在 VPS 上执行（前提是仓库已经在本机）：

```bash
bash deploy/install.sh {version}
```

该脚本会：

1. 从 `https://github.com/krwu/compose-deploy-agent/releases/download/{version}/deploy-agent` 下载二进制（`latest` 用 `releases/latest/download/deploy-agent`），并用同目录的 `deploy-agent.sha256` 校验 sha256
2. 安装到 `/usr/local/bin/deploy-agent`
3. 创建 `/etc/deploy-agent/`、`/var/lib/deploy-agent/`（权限 0700）
4. 首次安装时写入 `/etc/systemd/system/deploy-agent.service`（unit 内容内嵌在脚本里，与仓库中的 `deploy/deploy-agent.service` 保持一致，无需额外拷贝）
5. 进入安装向导 `deploy-agent setup`（见下文）。安装的是不带 `setup` 子命令的旧版本时，只打印后续手动步骤

### 安装向导（`deploy-agent setup`）

向导依次完成首次安装需要的全部配置，每一步都会先展示结果再确认：

1. **绑定地址**：自动检测本机 IPv4 地址（排除 loopback、link-local 以及 `docker0`、`br-*`、`veth*` 等 docker 网卡）并列出供选择，也可以直接输入 IP；端口默认 `8866`
2. **证书地址**：补充客户端还会经由哪些地址访问 agent，可以是其他 IP（如前置的四层代理）或域名（如 `agent.example.com`），多个用逗号分隔，不需要时直接回车；服务端证书已存在时，默认沿用证书中已有的其他 IP 和域名（回车保留，输入 `none` 清空）。然后列出最终写入服务端证书 SAN 的全部地址供确认。监听仍然绑在第 1 步的 IP 上；域名要能访问，需要解析到这台机器，并且客户端 `--endpoint` 使用该域名
3. **serve.env 与证书**：写入 `/etc/deploy-agent/serve.env`；本机 CA 不存在时生成 CA 和服务端证书。**CA 已存在时绝不覆盖**，只在服务端证书的 IP 或域名与本次确认的不一致时询问是否重签服务端证书（已签发的客户端证书不受影响）
4. **services.yaml**（可选）：扫描本机的 docker compose 项目，按编号选择要纳管的服务后自动生成清单（已有清单时，已纳管的服务默认选中），细节见[清单配置](manifest.md)。选择时可输入 `1,3`、`2-4`、`all` 或 `none`，直接回车采用方括号中的默认值
5. **检查与启动**：执行与 `serve -t` 相同的配置检查，通过后询问是否 `systemctl enable --now deploy-agent`（已在运行则询问是否重启）

已存在的 `serve.env`、`services.yaml` 默认保留（`services.yaml` 已存在时「是否重新生成」默认为否）；选择覆盖时会先备份为 `.bak`。向导可以重复执行（`sudo deploy-agent setup`），例如新增代理 IP 或纳管新服务时，已有的配置会作为默认值。

用 `curl ... | bash` 方式执行 `install.sh` 时，向导会从当前终端（`/dev/tty`）读取回答。

向导的语言按终端的区域设置自动选择：`LC_ALL`、`LC_CTYPE`、`LANG` 中第一个非空值的字符集为 UTF-8（如 `zh_CN.UTF-8`、`en_US.UTF-8`、`C.UTF-8`）时用中文，否则（如 `C`、`POSIX`、`zh_CN.GBK`）用英文。可以用 `--lang zh` 或 `--lang en` 指定。确认题的提示为 `[Y/n]` / `[y/N]`（中文界面写作 `[Y/n, 默认 y]` / `[y/N, 默认 n]`），回答 `y`/`n`，中文界面也可以回答 `是`/`否`。向导里的配置检查结果跟向导语言走，证书有效期显示为 `2006-01-02 15:04:05`；单独执行的 `serve -t` 仍打印英文，写入文件中的注释也始终是英文。

### 无人值守安装

没有终端（`ssh` 不带 `-t`）时向导只打印后续步骤。需要脚本化安装时，传 `--yes` 并通过参数给出答案，`install.sh` 会把版本号之后的参数透传给 `setup`：

```bash
ssh vps bash /tmp/install.sh {version} --yes --ip 10.0.0.5 --extra-ips 10.0.0.6
```

| 参数 | 说明 |
| --- | --- |
| `--ip` | 绑定的内网 IP；`--yes` 模式下必填（`serve.env` 中已有时可省略） |
| `--port` | 绑定端口，默认 `8866` |
| `--extra-ips` | 服务端证书额外包含的 IP 或域名，逗号分隔；不传时沿用现有证书中的其他 IP 和域名 |
| `--yes` | 全部采用默认值：不覆盖已有文件、不生成 `services.yaml`、不启动服务 |
| `--env-file` / `--manifest` / `--data-dir` | 使用非默认路径时指定。此时向导只做配置检查、不启动或重启服务，因为 systemd unit 只读取默认路径 |
| `--lang` | 向导语言：`zh`、`en` 或 `auto`（默认，按区域设置选择，见上文） |

### 手动配置（备选）

也可以跳过向导，手动完成配置：

- 复制 `deploy/serve.env.example` 为 `/etc/deploy-agent/serve.env`，填入本机内网 IP（`DEPLOY_AGENT_BIND=10.0.0.5:8866`）
- 参考 `deploy/services.example.yaml` 编写 `/etc/deploy-agent/services.yaml`（见[清单配置](manifest.md)）
- 按下一节执行 `deploy-agent ca init`

## 启动

安装向导的最后一步会询问是否启动；手动配置时执行：

```bash
systemctl daemon-reload
systemctl enable --now deploy-agent
```

验证存活（注意端口是 `8867`、明文 HTTP，不是主端口 `8866`）：

```bash
curl http://127.0.0.1:8867/healthz
```

## 日常运维

```bash
journalctl -u deploy-agent -f     # 查看日志（包括证书到期告警、反向校准告警等）
systemctl restart deploy-agent    # 修改清单后重启生效
```

修改清单或证书后，建议重启前先检查配置（类似 `nginx -t`，不影响正在运行的服务）：

```bash
sudo deploy-agent serve -t --bind 10.0.0.5:8866   # --bind 与 serve.env 中的 DEPLOY_AGENT_BIND 一致
# deploy-agent: configuration test is successful (1 services, server certificate valid until 2027-09-24 01:54:48)
```

`serve -t` 除了 serve 启动所需的检查（清单格式与路径、CA 与证书可加载），还会检查服务端证书当前有效、由本机 CA 签发、且 SAN 包含 `--bind` 的 IP。

Agent 重启时会自动把上次未完成的部署标记为 `interrupted`，并对每个服务做一次只读的反向校准（比对 `.env`、`last_healthy` 与 docker 实际运行状态，仅记录告警日志）。

## 在 VPS 上手动更新服务（`deploy-agent update`）

开发人员已经 SSH 上机时，不需要走流水线也能直接触发一次部署：

```bash
sudo deploy-agent update -s web -t a1b2c3d
sudo deploy-agent update -s web -d sha256:<64 位十六进制>   # image_ref: digest 的服务
```

与流水线用的 `deploy-agent deploy` 相比，`update` 有两个不同点：

- 不需要 `--endpoint`：请求默认发给本机正在运行的 `deploy-agent serve` 进程
- 不需要 mTLS 凭据：走 serve 额外监听的本机 unix socket（`/run/deploy-agent/deploy-agent.sock`，权限 0600，目录 0700），信任根是主机文件权限而非证书，因此必须以 root 执行（`sudo`），这与 `docker.sock` 访问权等价于 root 的定位一致。升级后 serve 还没重启时，`update` 会自动改用旧位置 `/var/lib/deploy-agent/agent.sock`

`update` 复用与 `deploy` 相同的并发控制、幂等判断、失败自动回滚与退出码，输出格式也相同（见 [Client 使用说明](client.md) 第 4 节）；前提是 `deploy-agent serve` 正在运行，否则会报错并提示 `systemctl status deploy-agent`。

如果 `serve` 使用了非默认数据目录，需要加上 `--data-dir`（用来定位 socket 文件）；`serve` 用 `--socket` 指定了路径时，`update` 也传同样的 `--socket`：

```bash
sudo deploy-agent update -s web -t a1b2c3d --data-dir /custom/data-dir
```

## 升级 agent 自身（`self-check` / `self-upgrade`）

注意与上一节的 `update`（更新业务服务）区分：`self-*` 命令只作用于 deploy-agent 自己，只能在部署 agent 的机器上执行，不提供 HTTP 接口。

检查是否有新版本（读取 GitHub 上最新 Release 的 tag）：

```bash
deploy-agent self-check
# current: 1.0.0
# latest:  1.1.0
# a new version is available; run `sudo deploy-agent self-upgrade`
```

退出码：`0` 已是最新，`10` 有新版本，`1` 出错，便于脚本判断。

升级并重启服务：

```bash
sudo deploy-agent self-upgrade
```

执行过程：

1. 下载最新版本到 `/usr/local/bin/` 下的临时文件，用同一次 Release 的 `deploy-agent.sha256` 校验
2. 用新二进制执行 `version` 与 `serve -t`，确认版本号正确、能通过当前配置；失败则中止，线上二进制不变
3. 若有正在进行的部署则拒绝升级，避免重启打断部署
4. 旧二进制保留为 `/usr/local/bin/deploy-agent.prev`，新二进制原子替换
5. 服务在运行时执行 `systemctl restart deploy-agent`，等待 `/healthz` 返回新版本；超时则自动换回旧二进制并再次重启。服务未运行时只替换二进制

常用参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--force` | `false` | 同版本重装、降级、本机为 `dev` 版本、或有部署进行中时仍然升级 |
| `--bind` | 读取 `--env-file` 中的 `DEPLOY_AGENT_BIND` | serve 的 `--bind`，用于 `serve -t` |
| `--env-file` | `/etc/deploy-agent/serve.env` | |
| `--manifest` / `--data-dir` | 与 serve 默认值相同 | serve 使用了非默认路径时需指定 |
| `--healthz-bind` | `127.0.0.1:8867` | serve 使用了非默认 `--healthz-bind` 时需指定 |
| `--health-timeout` | `1m` | 重启后等待新版本健康的时间 |

## 本机 unix socket

同样的 `/v1/*` 接口还额外监听在本机 unix socket `/run/deploy-agent/deploy-agent.sock`（权限 0600，仅 root 可访问），不走 mTLS，供 `deploy-agent update`（见上文「在 VPS 上手动更新服务」）使用。

- socket 所在目录由 systemd unit 中的 `RuntimeDirectory=deploy-agent`、`RuntimeDirectoryMode=0700` 创建。旧版本安装的 unit 文件没有这两行（`install.sh` 不会覆盖已有 unit），此时 serve 会自己以 0700 创建 `/run/deploy-agent`；也可以手动把这两行加进 `/etc/systemd/system/deploy-agent.service` 后 `systemctl daemon-reload`
- serve 启动时检查 socket 的父目录：必须是真实目录（不接受符号链接）、属主为当前运行用户、并且 `mode & 0077 == 0`（组和其他用户没有任何权限），否则**拒绝启动**并给出原因。目录才是真正的访问边界，socket 本身的 0600 是第二层。agent 不修改进程 umask（umask 是进程级的，会影响其他 goroutine 写 sqlite 和 `.env`）
- 使用非默认 `--data-dir` 的安装保持原来的 `<data-dir>/agent.sock`，也可以用 `serve --socket <path>` 显式指定；两种情况都同样检查父目录，例如 `<data-dir>` 必须是 0700（`install.sh` 创建的 `/var/lib/deploy-agent` 本来就是 0700）
- 请求在 socket 上带有 `local` 身份，不受 `access` 绑定限制。所有访问检查默认拒绝：没有身份（既不是已登记证书，也不是本机 socket）的请求一律 403
