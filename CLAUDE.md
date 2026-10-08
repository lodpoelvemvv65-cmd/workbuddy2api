# CLAUDE.md — 本仓库工作须知

面向 AI 助手 / 维护者的操作备注。**改动、合并或部署本仓库前先读这份。**

---

## 1. 仓库与远端

| remote | 地址 | 用途 |
|---|---|---|
| `origin` | `git@github.com:lodpoelvemvv65-cmd/workbuddy2api.git`(SSH) | 自己的 fork,推送目标 |
| `upstream` | `https://github.com/HanawaBanana/workbuddy2api.git` | 上游来源,只拉取 |

- 原上游 `Sliverkiss/workbuddy2api` 已于 **2026-09-24 删除**(fork 网络已断)。本仓库是该项目的**延续副本 + 本地增强**。
- `master` 是唯一主线,已含上游全部公开提交 + 本地特性(Anthropic/Responses 兼容层、CORS、/v1/stats 等)。
- 不要加回已删除的 `Sliverkiss` 远端。

## 2. 同步上游(上游有新提交时)

```bash
cd /home/ww/workbuddy2api

# 0) 合并前打备份点,出问题可回退
git tag backup/before-merge-$(date +%Y%m%d_%H%M)

# 1) 拉上游,看有什么更新
git fetch upstream
git log --oneline HEAD..upstream/master

# 2) 预检冲突(不动工作区)
git merge-tree --write-tree HEAD upstream/master

# 3) 合并
git merge upstream/master
#    有冲突:手工解决 -> git add <文件> -> git commit

# 4) 验证
go build ./... && go test ./...
python3 -m py_compile scripts/task_runner.py

# 5) 重建重启
docker compose up -d --build
docker compose ps && curl -s localhost:7863/healthz

# 6) 推回 fork
git push origin master
```

### ⚠️ 注意事项

- **不要用 `git pull` 拉上游**:`origin` 是自己的 fork,`git pull` 只会拉 fork。必须 `git fetch upstream` + `git merge upstream/master`。
- **不要用 GitHub 网页的 "Sync fork" 按钮**:fork 网络已断,且它只做 fast-forward,有本地提交时不可用/会出问题。
- **警惕「语义冲突」**——两边独立实现了同一功能,文本能自动合、逻辑却撞车。合并后**必须人工看一眼**这些重叠文件:
  - `Dockerfile` / `docker-compose.yml` / `README.md` / `internal/server/handler.go`
  - **历史教训**:`GOPROXY` 上游(`2e85a7b`)与本地(`7de27b1`)各加了一套,自动合并后 `ARG GOPROXY` 重复声明,且 compose 传空值会把国内镜像覆盖成官方源导致构建卡死。最终统一为:默认 `goproxy.cn`,留空回落官方。

## 3. 构建 / 测试 / 运行

- Go 构建:`go build ./...`
- Go 测试:`go test ./...`
- Python 语法:`python3 -m py_compile scripts/task_runner.py`
- 重建并重启:`docker compose up -d --build`
- **端口**:网关 `7863`,可视化面板 `7864`
- 健康检查:`curl -s http://localhost:7863/healthz`
- 容器名:`workbuddy2api`(网关)、`workbuddy2api-dashboard`(面板)
- `GOPROXY`:默认 `https://goproxy.cn,direct`(国内可直连);要回落官方默认显式传空:`--build-arg GOPROXY=`。
- 改了 Go / Python 代码后,必须 `docker compose up -d --build` 才生效(代码是 `COPY` 进镜像的,不是挂载)。

## 4. 敏感信息 —— 绝不提交

以下均已写入 `.gitignore`,保持忽略:

```
config.json      auths/        data/        logs/       backups/
.apikey          *.key         *.pem        .env        .env.*
```

- `config.example.json` 是**占位模板**(`"api_key": "test_key"`、`device_token: ""`、`upstash.token: ""`),可安全提交。
- 本地 SSH 私钥在 `~/.ssh/id_ed25519_github`(无密码短语),**不属于本仓库**,别复制进来。
- **提交前自查**:
  ```bash
  git status
  git ls-files | grep -Ei '(^|/)(config\.json|\.apikey)$|auths/|data/|logs/|\.(key|pem|env)$'   # 应为空
  git grep -nEI 'BEGIN .* PRIVATE KEY|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}' HEAD   # 应为空
  ```

## 5. 提交与推送

- 提交信息用中文,尽量遵循 Conventional Commits(`feat:` / `fix:` / `docs:` / `chore:` …)。
- 推送目标:`git push origin master`。
- 推送前确认:在 `master` 分支、工作区干净、已通过构建/测试。

## 6. 常用自查命令

```bash
git remote -v
git log --oneline --graph --decorate -15
git log --oneline HEAD..upstream/master          # 上游还有哪些新提交
git merge-tree --write-tree HEAD upstream/master # 预检冲突
git status --ignored                             # 看被忽略的敏感文件
```

## 7. 备份 / 回退

- 合并前的备份 tag:`backup/before-merge-20261008_1253`(指向 2026-10-08 合并上游 8 提交之前的 `ddb087d`);更早一次是 `backup/pre-merge-upstream-5e2c2b4`。
- 回退方式:`git reset --hard <tag>`(丢弃)或 `git revert <commit>`(保留历史)。
- 确认无需回退后可删备份:`git tag -d backup/before-merge-20261008_1253`。

### 2026-10-08 合并教训(上游 auth 分组 / 签到)

上游新增「账号分组隔离 + 密钥分组调用 + 签到」时,与本地特性在同一处叠加,
`git merge` 报 `cmd/server/main.go`、`internal/server/handler.go` 冲突。经验:

- `main.go`:`metricsJSONPath`(本地)与 `checkinJSONPath`(上游)在同一位置,保留**两个**函数。
- `handler.go`:`Config` 结构字段、路由注册同样各加各的,两边都保留。
- **`withAuth` 是语义冲突重点**:上游改成多密钥表(`authKeyTable`/`matchAuthKey`),
  本地加了 Anthropic 系的 `X-Api-Key` 回落与错误信封按协议分流。合并后:
  - **保持 `matchAuthKey(keys, authz)` 原签名**(上游测试直接调用,改签名会让 `authkeys_test.go` 编译失败);
  - 在 `withAuth` 里把非 Bearer 的 `X-Api-Key` 拼成 `Bearer <key>` 再交给 `matchAuthKey`。
- 粘性号解绑处:本地补的 `acct = nil` 是**必需修复**(realm 不符时不置 nil 会用错域账号),
  不能因选上游注释而丢掉。
- 合并后必须跑 `go test ./...` 才能发现上面的 `matchAuthKey` 签名问题。
