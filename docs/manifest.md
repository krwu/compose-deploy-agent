# 清单配置（services.yaml）

每个纳管的服务只需 4 个必填字段：

```yaml
version: 1
services:
  web:                           # 逻辑服务名，API 中的 service 字段
    dir: /srv/web                # compose 项目所在的绝对路径
    file: compose.yml            # compose 文件名（相对 dir）
    service: web                 # compose 文件里的服务名
    image: registry.example.com/web   # 镜像仓库地址（不含 tag）
    # tag_env: IMAGE_TAG         # 可选，默认 IMAGE_TAG
```

对应的 compose 文件需要用该环境变量引用镜像 tag，并要求变量必须存在：

```yaml
services:
  web:
    image: registry.example.com/web:${IMAGE_TAG:?IMAGE_TAG is required}
```

agent 拉取时使用 `docker compose pull --policy always`，因此即使始终推送同一个 tag 也能更新；这要求 Docker Compose 支持 `pull --policy`（可用 `docker compose pull --help | grep policy` 确认）。服务的 `pull_policy` 不能设为 `never` 或 `build`，否则预检返回 `config_invalid`；不设置、`missing`、`always` 均可。

不在清单中的服务一律拒绝部署（清单即白名单）。修改清单后需要 `systemctl restart deploy-agent` 生效。

### 由安装向导生成清单

向导通过 `docker compose ls -a` 找到本机的 compose 项目（包括已停止的），列出每个服务及其镜像。生成的清单中逻辑服务名默认为 compose 服务名；多个项目有同名服务、或该名字已被现有清单中的其他服务占用时，加上 `<项目名>-` 前缀（已纳管的服务保留原名，见下文）。

向导生成的服务统一使用 **`{SERVICE}_TAG`** 作为 `tag_env`：`{SERVICE}` 为 compose 服务名转大写，非字母数字字符替换为 `_`（如 `api-web` 对应 `API_WEB_TAG`），同一目录下不会重复。compose 文件中的 image 还没有这样引用时，向导会在确认后改写 compose 文件：

- tag 写死（`repo:1.2.3`，或不写 tag）、或引用了其他变量（如 `${IMAGE_TAG:?}`、`${TAG:-latest}`）的 image 行，改写为 `repo:${API_WEB_TAG:?API_WEB_TAG is required}`。只替换这一行的值，注释和格式保持不变，原文件先备份为 `.bak`
- 在项目目录的 `.env` 中写入当前正在使用的 tag（如 `API_WEB_TAG=1.2.3`），改写后手动 `docker compose up` 的结果不变；原来的变量行保留不删。当前 tag 无法从 compose 文件和 `.env` 中确定时，向导会要求手动输入
- 写入后用 `docker compose config` 校验每个服务解析出的镜像与改写前一致，否则还原所有改动并报错

以下情况无法自动处理，会列出原因但不可选择，需要手动把 image 改成上面的形式：digest 引用（`repo@sha256:...`）、镜像仓库地址中含变量、image 来自 `extends` / `include` / YAML 锚点或合并键、项目使用多个 compose 文件、build-only 服务。

已有 `services.yaml` 时选择重新生成，向导会读取现有清单：

- 已纳管的服务在列表中标注 `managed as <逻辑名>` 并默认选中，**保留原有的逻辑名**（流水线按逻辑名部署），但同样统一改写为 `{SERVICE}_TAG`，避免旧变量名与其他服务自动生成的变量名冲突
- 取消选中已纳管的服务时会列出它们并确认，默认不移除
- 清单中有、但扫描不到或无法自动改写的服务，确认后原样保留（默认保留）
- 改写了已纳管服务的 `tag_env` 后，需要重启 deploy-agent 才能使用新清单，向导最后一步会询问是否重启

手写清单时 `tag_env` 的默认值仍是 `IMAGE_TAG`，不受影响。

向导重新生成清单时，下文的 `image_ref`、`post_deploy`、`access`、`oidc` 只对「原样保留」的服务和段落有效：被重新选中的已纳管服务会按扫描结果重建，`access` 和顶层 `oidc` 段不会写回。使用这些字段的清单请手工维护，重新生成后务必核对。

### 只接受 digest（`image_ref: digest`，可选）

tag 可以被重新指向别的镜像，有推送权限的人就能改变「同一个 tag」实际部署的内容。对安全要求高的服务可以改为只接受镜像 digest：

```yaml
services:
  web:
    dir: /srv/web
    file: compose.yml
    service: web
    image: ghcr.io/example/web
    image_ref: digest            # 默认 tag
    tag_env: IMAGE_DIGEST        # 可选，便于区分；默认仍为 IMAGE_TAG
```

compose 文件改为用 `@` 引用：

```yaml
services:
  web:
    image: ghcr.io/example/web@${IMAGE_DIGEST:?IMAGE_DIGEST is required}
```

- 请求必须带 `digest`（客户端 `--digest sha256:<64 位小写十六进制>`），带 `tag` 一律返回 400；格式严格校验为 `^sha256:[a-f0-9]{64}$`，写进 `.env` 的就是这个值，不可能注入换行或其他引用
- 反过来，未开启 `image_ref: digest` 的服务不接受 `digest`，行为与之前完全一致
- 幂等判断、回滚与 `last_healthy` 都以 digest 为准，回滚时 `.env` 恢复为上一个 digest。从 tag 模式切换过来后，旧的 tag 不能作为回滚目标（会报告 `no usable rollback target`），切换后第一次成功部署起即可正常回滚
- 部署记录与状态接口中的 `target_tag`、`running_tag`、`rolled_back_to` 等字段此时填的是 digest

### 部署后执行固定命令（`post_deploy`，可选）

新版本通过就绪检查后，可以在容器里执行一条固定命令（例如数据库迁移、初始化）：

```yaml
services:
  web:
    # ...
    post_deploy:
      user: www-data                         # 必填，不允许 root
      args: [php, artisan, "lsky:preview-init"]
      # service: web                         # 可选，在哪个 compose 服务里执行，默认就是本服务
      # timeout: 3m                          # 可选，默认 3m，上限 5m
```

agent 实际执行的是 `docker compose -f <file> exec -T -u <user> <service> <args...>`，以参数数组调用，不经过 shell。

- 命令、用户、服务全部只来自本机清单，部署请求无法影响：请求体只允许 `service` 和 `tag`/`digest` 两个字段，带任何其他字段都返回 400
- `user` 必填，并且拒绝 root：`root`、`0`（含 `00` 等）以及 `root:…`、`0:…`、`…:root`、`…:0` 都会让清单加载失败。不提供默认值，因为镜像的默认用户往往就是 root
- 命令退出码非 0 或超时，都算部署失败：结果为 `post_deploy_failed`，并像就绪失败一样**自动回滚**到上一个版本，`message` 里写明退出码或超时（以及 stderr 末尾一段）。回滚后不会再次执行该命令；首次部署没有可回滚的版本时只报告失败
- 超时会 SIGKILL 整个 `docker compose exec` 进程组。注意 `docker compose exec` 客户端被杀后，容器里的进程不一定随之结束；回滚会用旧镜像重建容器，从而结束它。首次部署（无回滚目标）超时时，请自行确认容器内是否有残留进程
- 结果为 `noop`（目标版本已在运行）时不执行
- 整次部署仍受 10 分钟总时限约束，`timeout` 上限 5 分钟是为了给回滚留出时间

### 客户端证书与服务绑定（`access`，可选）

默认情况下，任何已登记且未吊销的客户端证书都可以部署清单里的任何服务。需要按证书限制时，在清单顶层加 `access`：

```yaml
access:
  require_binding: true          # 默认 false，见下文
  clients:
    ci-main:                     # 证书名称（ca issue -n），或 ca list 中的指纹
      services: [web]
    ci-preview:
      services: [web-preview]
```

- 列在 `clients` 里的证书只能操作自己的 `services`；对其他服务的 `POST /v1/deploy`、`GET /v1/status/{service}`、`GET /v1/deploys/{id}` 返回 403 `cert_not_allowed`，并记录告警日志（证书名、指纹、服务）
- 同一张证书既可按名称也可按指纹配置，指纹优先。按名称配置时，同名的所有证书共享该绑定（重签同名证书无需改清单），吊销仍按单张证书生效
- `require_binding: false`（默认，兼容旧行为）：没有列出的证书仍可操作所有服务；`require_binding: true`：没有列出的证书一律 403。建议所有证书都配置绑定后打开它
- `clients` 里引用了清单中不存在的服务、或 `services` 为空，清单加载失败
- 没有配置 `access`、或 `require_binding` 为 false 时，`serve` 启动时和 `serve -t` 都会打印告警，提示存在可以部署所有服务的证书
- 本机 unix socket（`deploy-agent update`）不受绑定限制，其信任根是主机 root 权限

### GitHub Actions OIDC 绑定（`oidc`，可选）

配合 `serve --oidc-listen` 使用，认证流程与安全设计见 [OIDC 认证](auth_oidc.md)。顶层 `oidc.audience` 是本 agent 专属的 audience，每个服务可以配一组 `oidc` 规则：

```yaml
oidc:
  audience: https://deploy.img.ntest.top   # 必须与 workflow 里申请 token 时的 audience 一致

services:
  main:
    # dir / file / service / image ...
    oidc:
      - repository_id: "123456789"         # 必填，仓库数字 ID
        repository_owner_id: "987654321"   # 必填，owner 数字 ID
        repository: kairee/img-webdn       # 可选，只作二次校验
        ref: refs/heads/main               # 必填，完整 ref，精确匹配
        event_name: [push]                 # 必填，允许的事件白名单
        environment: preview-main          # 可选，配置了就必须一致
        job_workflow_ref: kairee/img-webdn/.github/workflows/deploy.yml@refs/heads/main   # 可选
```

- 一个服务的多条规则之间是「或」，一条规则内所有已配置的字段是「且」；每个 claim 单独比较，不使用 `sub`
- `repository_id`、`repository_owner_id` 必须是数字（加不加引号都可以），用 `gh api repos/<owner>/<repo> --jq '.id,.owner.id'` 查询
- `event_name` 不能包含 `pull_request_target`、`workflow_run`，否则清单加载失败；运行时也会无条件拒绝这两种事件
- 有服务配置了 `oidc` 规则时 `oidc.audience` 必填；audience 不能是 GitHub 默认的 `https://github.com/<owner>`
- 规则既没有 `environment` 也没有 `job_workflow_ref` 时，`serve` 和 `serve -t` 会告警：同一仓库在该 ref 上的任何 workflow 都能部署这个服务。建议至少配置其中一个，`environment` 还可以在 GitHub 上加审批和分支保护
- 没有 `oidc` 规则的服务不接受 OIDC token；`oidc` 规则不影响证书入口和 `access`
