> [!NOTE]
> **延续仓库**：原上游 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 已于 2026-09-24 从 GitHub 消失（删除或转私有）。本仓库是其完整历史的延续副本（含上游最后的公开提交 `9a26ae7`），按原项目的 **MIT License** 继续维护，原始版权声明见 [LICENSE](LICENSE)。

<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API</h1>

<p align="center">
  <b>把 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关</b><br>
  OAuth 登录 · 账号池轮转 · 熔断与冷却 · 会话粘性 · 积分补充
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Docker_Compose-2496ED?logo=docker&logoColor=white&style=flat-square">
  <img alt="Transport" src="https://img.shields.io/badge/Transport-SSE%20%2F%20Streaming-0DBD8B?style=flat-square">
  <a href="https://t.me/sliverkiss_blog"><img alt="Telegram" src="https://img.shields.io/badge/Telegram-%E9%A2%91%E9%81%93-blue?logo=telegram&logoColor=white&style=flat-square"></a>
</p>

---

## 项目简介

WorkBuddy2API 是一个自托管的 **OpenAI 兼容上游网关**，将 ```CodeBuddy``` 账号包装为统一的 `/v1/chat/completions` 服务。

### 交流群组
- [@checkinHome](https://t.me/checkinHome)

### 本项目做什么

- 通过 **OAuth 设备授权**（`login.sh`）获取账号凭证，在网关侧做 token 自动刷新、账号池调度与流量治理；
- 面向 **个人多账号** 场景：多账号共享、单号故障自动换号、冷却 / 熔断防止雪崩、会话粘性保证多轮上下文不跳号；
- 对客户端只暴露 OpenAI 兼容接口，现有 SDK / 前端 / 工具 **零改造接入**。

### 本项目不做什么

- **只做上游网关，不做通用协议转换层** — 核心是对接上游 ```CodeBuddy``` 并暴露 OpenAI Chat 协议；另附两个**面向自家下游客户端**的协议垫片（`/v1/messages` 给 Claude Code、`/v1/responses` 给 Codex CLI），它们复用同一条上游管线、只做形状翻译。其余协议（Gemini 等）的适配仍应由下游网关负责；
- **不内嵌 Web 管理面板** — 网关核心保持精简，可视化面板作为独立项目维护，数据直取上游接口，不增加网关适配负担。

### 社区前端面板

需要 Web 管理面板的用户，可部署以下符合本理念的社区项目（独立维护，与网关解耦）：

- [workbuddy2api-gui](https://github.com/287775856/workbuddy2api-gui) — 账号池状态可视化面板
- [workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) — 账号管理工具

> ⚠️ 合规须知：本项目是**非官方**网关，使用 ```CodeBuddy``` 账号作为上游，**仅限本人授权账号、本机 / 私有环境测试**。详细边界见[安全与合规](#安全与合规)。

📖 完整文档见 [GitHub Wiki](https://github.com/Sliverkiss/workbuddy2api/wiki)。

## 核心能力

### 账号池治理

- **OAuth 设备授权登录** — `login.sh` 一条命令完成：取授权 URL → 浏览器登录 → token 轮询 → 凭证落盘 → 重启加载，全程无 PKCE（state 由服务端签发），重复执行即可连续添加多账号
- **三因子加权随机选号** — `credits 比例 ×10 + 快过期积分占比 ×8 + 闲置补偿` 三项加权（`pool.expiring_soon` 窗口内的积分优先消耗，默认 7 天），按权重降序取 **Top-5 候选短名单**，再在短名单内加权抽签（等权重候选先随机打乱防惊群、LRU 兜底覆盖全部候选），兼顾积分多、快过期积分先用掉、闲置久的账号；失败账号由熔断 / 冷却 / 连败降权状态机处置（不进权重公式）
- **防惊群** — 跳过 100ms 内刚被选中的账号，多账号同时待命时不打爆同一台
- **在途租约** — 单账号最大在途请求数（`pool.max_in_flight`）限制并发占用，占满的号不参与选号，避免单号过载
- **账本择优** — 每次成功请求按 `usage.credit` 折算每千 token 单价记入 `(账号, 模型)` 账本，免费 / 便宜的账号优先；观测按 EMA 平滑、6 小时未更新即失效（陈旧价格不复活），成本随上游活动实时变化；账本随池状态落盘 `state.json`，重启不丢学费；`/status` 透出 `model_costs` 台账（模型 / 单价 / 末次观测 / 样本数）
- **成本分层条件探索** — costTier 硬过滤（免费 > 未知 > 收费）会把全池锁死在唯一的实测免费号上：其余账号永远轮不到、也就永远学不到「它其实也免费」（垄断 + 学习冻结，issue #136）。破解方式是**搭车改道**：tier 0 垄断层存在且 tier 1 有成员时，距上次探索 ≥ `pool.cost_explore_interval`（默认 30m，`"0"` 关停）就把本次选号改道给一个未知号——承接的是完整真实用户请求，**零新增上游请求**（IP 维度零增量，WAF 友好）。成功即毕业（首观测入账，免费回 tier 0 / 收费出局 tier 2，学费只付一次）；失败走既有冷却 / 熔断策略，无探测风暴。探索频率硬性限幅 ≤ 48 次 / 天 / 模型（24h ÷ 30m），与池规模和 QPS 无关；tier 1 枯竭后自动停探。探索节奏按 `(域, 模型)` 独立；`/status` 透出 `cost_explore` 台账（累计事件数 + 各 (域, 模型) 最近探索时刻），与 `model_costs` 行对照即可读出「探索 → 毕业」全链路

- **模型别名表** — `config model_aliases` 把客户端惯用短名映射到真实上游模型名，`/v1/chat/completions`、`/v1/messages`、`/v1/responses` 三条入口同待遇；键按同名家族归一化匹配（大小写 / `cn:` `global:` 前缀 / `[1M]` 标记 / `-sg` 后缀都不影响命中），换名保留客户端写的前缀与标记。模型名写错会触发上游 11102 并被记成账号的模型级黑名单，后续重试退化成 `no_healthy_account`（看着像账号全挂），别名表是这一坑的解药
- **同名家族候选链（免费优先 → 积分兜底）** — 客户端只写模型名，网关把同族候选（跨域 / 区域变体 `-sg` / 计费档）展开成链并按「`x0.00` 免费档 → 倍率升序」排序，**只在当前候选全池无可用号时**沿链推进：免费额度在就白嫖、用完自动换积分档，国内国外同一套规则；`[1M]` 只做档位过滤（没有 1M 版本就退回原模型），目录未收录的模型名原样透传。日志 / 指标记实际出站模型，降级链路可见。见「Anthropic Messages 兼容接口」一节

### 流量治理

- **分级熔断与冷却** — 429 软冷却（600s 起指数退避、封顶 `soft_rate_max`）、404 固定浅冷却、402 / 余额耗尽硬冷却至次日 04:00、连续失败熔断（`breaker_threshold` 触发后指数退避封顶 6h）
- **模型级限流独立冷却** — 6004（该模型使用量超限）只冷却触发调用的模型，切其他模型立即可用；`/status` 透出 `rate_limited_models` 台账
- **账号临时停用 / 恢复** — 运维可把某个号临时摘出选号池、观察后再放回，不必删凭证（issue #138/#118）。语义是「对话流量摘除」而非「账号冻结」：停用期间签到、token 保活、排程任务照常执行，账号仍在池里、状态照常透出。与系统自动禁用是**两个独立状态位**（`manual_disabled` / `disabled`），各自清除、都清空才回到选号池——避免运维意图被签到解冻等自动复活路径意外解除；停用状态随池状态落盘，重启保留。入口：`/admin/accounts/{uid}/{disable,enable,revive}` 端点 + `cmd/acct` CLI（默认关闭，`admin.enabled` 显式开启）
- **状态持久化** — 池状态（积分 / 冷却 / 熔断 / 计数）本地原子落盘 `state.json`，可选镜像至 Upstash Redis，重启后择优恢复
- **请求统计持久化** — `/v1/stats`（请求数 / 缓存命中率 / 速度 / 扣费）默认落盘 `state.json` 同目录的 `metrics.json`（`metrics_persist` 默认 true，`metrics_file` 可显式指定路径），容器 / 进程重启后累计量与统计窗口延续，不再从零重新计数；`metrics_persist: false` 回到纯内存旧行为

### 请求链路

- **流式 + 非流式** — 出站强制 `stream:true`；SSE 帧按 OpenAI 规范白名单重建；非流式由本地聚合为单响应
- **DeepSeek 思维链注入** — 出站请求体注入 `thinking.type=enabled` + 默认档位，`reasoning_content` 多轮回填，`reasoning_effort` 按模型档位自动降级
- **系统提示词三模式**（`prompt.mode`，缺省 `passthrough`） —
  - `passthrough`（缺省）：透传客户端原始 system，遇内容拦截自动降级中性提示词重试
  - `custom`：网关用自有提示词**替换**客户端 system/developer（从源头消除模板句误报；不参与降级）
  - `append`：**两者并用**——开头连续 system/developer 块之后插入网关自有提示词，客户端项目规范/工具约定与网关人格共存（issue #129）；降级期与拦截首遇重试时退化为 `custom` 语义（换中性提示词，原文 system 移除）
  - `prompt.file`（custom/append 生效）指向自定义提示词文件，空 = 内置默认
- **会话头族注入** — 出站携带官方客户端会话头族（`X-Conversation-Request-ID` 聚合主键 · `X-Conversation-ID` 透传 · B3 链路），轮转 / 重试 / 路径回退复用同键，后台按对话轮聚合不再碎片化（issue #35）
- **指纹脱敏** — 出站请求体黑名单指纹字段清洗（可开关），与提示词体系两层叠加

### 选号语义

选号 = 会话粘性（命中即定）→ 成本分层（硬过滤）→ 加权随机（软均衡）三层串联，各层语义：

- **成本分层** — 账本把每个 `(账号, 模型)` 归入三档：**tier 0**（实测免费，单价 ≤ 0）、**tier 1**（无观测）、**tier 2**（实测收费）。同一次选号在**存活的最便宜档内**选：有 tier 0 就只在 tier 0 里挑，全池无免费观测才落到 tier 1，再不行才是 tier 2——即「贵号永远只作兜底」。tier 1 的号**不会被跳过**：新账号 / 新模型没跑过就没有观测，直接淘汰会把新号饿死。观测随 `usage.credit` 实时更新且 6 小时过期，所以限免窗口（如夜间免费）一结束，账号回到 tier 1 / tier 2，选号自动跟随——无需重启，日志会打 `free tier ended` 提示价格切换
- **会话粘性** — 同一对话固定走同一账号（多轮上下文不跳号、上游 prompt cache 不碎）。粘性键按此优先级取：**conversation 维度四键**（`metadata.conversation_id` / `metadata.conversationId` / `conversation_id` / `conversationId` 任一）→ **`prompt_cache_key`**（pi-ai 系客户端把会话 ID 放在这个 OpenAI 前缀缓存字段里）→ **首条 user 消息文本的 sha256 兜底**（OpenAI 兼容协议无会话 ID 字段，dsh / Codex 等客户端四键全缺，此前粘性恒不命中、逐请求换号；现由首条 user 消息派生会话级稳定键——会话内历史追加不影响该键，开新会话自然换键）。`user_id` **不是**粘性键——它会把一个用户的所有并行对话钉到同一个号上（粒度远粗于上游对话级缓存边界），发 `user_id` 的客户端回落加权轮换（**该回落同样适用于首条 user 消息兜底**：请求体带 `metadata.user_id` 或顶层 `user_id` 时不派生兜底键）。绑定 30 分钟滚动续期，空闲即过期释放
- **负载分布** — 粘性与分层都未限定时，三因子加权随机（`credits ×10 + 快过期积分 ×8 + 闲置补偿`）把流量摊开：高余额号多扛、快过期积分的号先用、闲置号补位；防惊群跳过 100ms 内刚选中的号。权重是**概率倾斜**而非硬排序（Top-5 短名单 + 名单内抽签），不会让单一账号垄断流量

### 定时积分任务

- **签到**（09 / 21 点）— 每日签到 + 余额查询，余额恢复自动解冻冷却账号
- **活跃地图**（10 点）— 对话事件连发上报点亮活跃地图与连登天数、解锁领养前置，补签卡保连登、连登档位兑换 + 抽奖、礼包/补偿领取，回读 streak 自检
- **猫猫旅行**（09 / 21 点）— 独立排程：领养 / 派出 / 领奖闭环推进
- **token 保活**（22 点）— 全账号刷新 token，session 失效连续 3 次才禁用
- **开学季任务**（12 点）— 任务点亮 + claim + 自动抽空抽奖余额，活动下线时自动跳过
- **夜猫子任务**（01 点）— 夜猫窗口（23:00–08:00 CST）内补一次 black_cat 任务

六类任务独立排程、独立开关（`schedule.*_enabled`），互不影响。

### 双域适配

- 同时适配**国内版（CN，`copilot.tencent.com` / `www.codebuddy.cn`）与国际版（Global，`www.workbuddy.ai`）**账号
- 共享同一账号池，由账号 `realm` 或请求模型名前缀（`cn:` / `global:`）决定路由；`global.enabled` 可一键锁死纯 CN 部署
- 国际版支持注册激活、地区完善、一次性 trial 加油包领取（`./trial.sh`）

### 辅助工具

- 积分日报：`./credit.sh`（美化 / `-json`，realm 感知双域）
- 手动签到：`./signin.sh`（批量、幂等不重复计）
- 账号停用 / 恢复：`./acct.sh list | disable <uid> [原因] | enable <uid> | revive <uid>`（需 `admin.enabled`，走网关管理端点）
- 领养联动 / 任务查询：`scripts/task_runner.py`（成长任务一体机，默认 dry-run）
- 可视化面板：`go run ./cmd/dashboard`（缓存命中率 / 速度 / 最近日志 / 账号池一页看，独立进程，见「可视化面板」）
- 个性化提示词：`prompt.file` 指向自定义提示词文件即整体替换内置默认（`custom`/`append` 模式生效）

## 架构总览

```mermaid
flowchart LR
    Client["客户端 / SDK\nOpenAI 兼容请求"] --> H

    subgraph GWI["WorkBuddy2API 网关 :7863"]
        H["HTTP Handler\n鉴权 · 提示词改写 · 轮转"] --> P
        H --> S
        P["账号池\n三因子加权 · 熔断 · 冷却 · 租约"] --> U
        S["会话粘性路由"] -.绑定镜像.-> REDIS
        T["定时调度\n签到 09/21 · 旅行 09/21 · 活跃地图 10 · 保活 22\n开学季 12 · 夜猫子 01"] --> P
        U["上游 Client\nChatHTTP 流式 · 短 RPC"]
    end

    P -. "读凭证 (0600)" .-> AUTH[("auths/*.json")]
    P -. "状态镜像" .-> REDIS[("Upstash Redis\n可选")]
    U -->|"chat/completions (SSE)"| CB["CodeBuddy\ncopilot.tencent.com"]
    U -->|"billing / auth / growth"| CB
```

上游请求在出站前经历统一的改写管线（`internal/upstream/payload.go`）：强制 `stream:true`、`developer` 角色归一、tool_choice 归一、`image_url` 字符串兼容为 OpenAI 对象形态、DeepSeek 思维链注入、`reasoning_effort` 档位降级、`reasoning_content` 回填、指纹脱敏。

## 快速开始

### 环境要求

- **Docker + Docker Compose**（推荐部署方式，镜像内已含 `app` 低权限用户与全部工具脚本）
- 一个或多个已注册的 CodeBuddy 账号，用于 OAuth 登录
- 宿主机 Go ≥ 1.22（仅源码构建时需要）

### Docker Compose 一键部署

```bash
git clone https://github.com/Sliverkiss/workbuddy2api.git
cd workbuddy2api
cp config.example.json config.json
```

编辑 `config.json`，**至少设置 `api_key`**（`留空 = 不鉴权`，公网部署务必设置）。示例中的 `test_key` 等均为占位符，`config.example.json` 不含任何真实密钥。

```bash
# 登录添加账号（重复执行可加多号；注意：执行过下方说明中的 chown 后，
# host 侧 login.sh 会被可写性预检拦截——此时请在容器内登录，见下方说明）
./login.sh

# 启动服务
docker compose up -d --build

# 健康检查（无可用账号时 503）；service 字段用于确认打到的是本网关
curl -s http://localhost:7863/healthz
# {"healthy":2,"total":3,"service":"workbuddy2api"}
```

`login.sh` 内置授权 URL 获取 + 浏览器登录 + token 轮询 + 首次签到 + `auths/workbuddy-<uid>.json` 落盘 + 容器重启，全程无 PKCE（state 由服务端签发）。账号池在容器启动时用 `auths/` 目录自动对齐，新增凭证文件即自动发现。

> **国内网络下构建会卡在第一层**：镜像构建的第一步是 `go mod download`，默认走官方
> `proxy.golang.org` —— 中国大陆不可达，表现为长时间停在这一层（看着像构建挂了），
> 有时直接失败。换个国内代理即可：
>
> ```bash
> docker compose build --build-arg GOPROXY=https://goproxy.cn,direct
> docker compose up -d
> ```
>
> 也可以直接填进 `docker-compose.yml` 的 `build.args.GOPROXY`。留空 = 官方默认，
> 与改动前行为一致。

> **非 root 宿主用户注意**：`./login.sh` 以**当前宿主用户**落盘凭证（权限 0600），而容器内网关以 `app(uid 10001)` 读 + 回写（refresh / realm 补标识走 tmp+rename，需要目录写权限）。二者 uid 不同（例如 Linux 非 root 账号通常是 uid 1000）时容器读不到凭证文件，`/status` 账号数为 0——与 `./data` 卷的属主问题同源。登录后、启动前把目录属主交给 10001（root 或部署用户执行）：
>
> ```bash
> chown -R 10001:10001 ./auths
> ```
>
> 之后新增账号**必须**进**容器内**登录（`app` 自身落盘，属主即 10001，无需反复 chown；chown 后 host 侧 `./login.sh` 无写权限，脚本会在启动浏览器授权前直接退出并提示，不会白走一遍 OAuth。容器内无 docker CLI，完成后回宿主机重启）：
>
> ```bash
> docker compose exec -it wb2api bash -c './login.sh' && docker compose restart wb2api
> ```

#### 日志持久化（断流 / 告警回查）

默认日志只进 `docker logs`（json-file 驱动）。容器**重建**（`docker compose up` 因镜像/配置变更重建容器）会连带丢弃日志，`upstream truncated`（断流）这类低频 WARN 事后无从回查。开启落盘：

```bash
mkdir -p ./logs && chown -R 10001:10001 ./logs   # app(uid 10001) 需写权限
```

在 `config.json` 增加：

```json
"log_file": "./logs/wb2api.log",
"log_max_mb": 64
```

- 日志**同时**写 stderr（`docker logs` 行为不变）与该文件；请求流水行与 `upstream truncated` 等 WARN 落在同一文件，跨容器重建留存。
- `log_max_mb`（默认 64）为轮转阈值，超过即切一份 `wb2api.log.1`（只留最近一份）。
- 打开失败只降级为仅 stderr（打一条 WARN），**不会**因日志权限问题拒绝启动。
- `log_file` 缺省为空 = 关闭（零回归）。`docker-compose.yml` 已挂载 `./logs:/app/logs`。

#### 请求统计持久化（`/v1/stats` 重启不清零）

面板「模型统计」卡片的数据源 `/v1/stats` 默认**落盘**，容器 / 进程重启后累计量（请求 / token / 缓存命中 / 扣费）与统计窗口 `since` 延续，不必从零重新计数。落盘路径与 `state.json` / `model.json` 同目录（默认 `./data/metrics.json`，Docker 已挂载 `./data`），无需额外配置。

```json
"metrics_persist": true,        // 默认 true；false = 纯内存旧行为（重启清零）
"metrics_file": ""              // 空 = 派生 state_file 同目录 metrics.json；非空显式指定
```

- 后台每 5s 检查脏标记原子落盘（tmp + rename），与池的 `state.json` 同范式；`POST /v1/stats/reset` 会同步清空并立即落盘。
- `metrics.json` 缺失 / 损坏 / 含负计数字段时静默零状态或剔除破损条目，不拒绝启动。
- 文件权限：与 `./data` 同口径（`chown -R 10001:10001 ./data`）。落盘失败只打节流 WARN，不影响请求。

### 源码构建

```bash
go build ./...
go vet ./...
go test ./...      # 完整测试套件
go run ./cmd/server -config config.json
```

构建二进制：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o signin_bin ./cmd/signin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o login ./cmd/login
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o credit ./cmd/credit
```

#### Windows 原生运行（无需 Docker）

Windows 10/11 自带的 PowerShell 与 `curl.exe` 即可管理后台进程。先准备配置并构建：

```powershell
Copy-Item config.example.json config.json
# 编辑 config.json；建议把 listen 设为 127.0.0.1:7863，且务必设置 api_key

go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server
go build -trimpath -ldflags="-s -w" -o login.exe ./cmd/login
go build -trimpath -ldflags="-s -w" -o signin_bin.exe ./cmd/signin
go build -trimpath -ldflags="-s -w" -o credit.exe ./cmd/credit
```

使用仓库自带脚本在后台启停并查看状态：

```powershell
.\start-workbuddy2api.cmd
.\status-workbuddy2api.cmd
.\stop-workbuddy2api.cmd
```

PID 写入 `wb2api.pid`，标准输出与错误日志分别写入 `data/server.out.log`、
`data/server.err.log`。停止脚本会先验证 PID 对应的可执行文件确为当前目录下的
`wb2api.exe`，不会因陈旧 PID 误杀其他进程。

添加账号可使用配套管理面板，或在 Git Bash 中运行现有 `login.sh`（它还负责 CN
首次签到以及 Global 注册地区/trial 流程；不建议只手工调用 `login.exe` 后跳过这些步骤）。

### 验证

```bash
# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

# 账号状态（汇总 + 每账号详情，含 disabled / manual_disabled 双位）
curl -s http://localhost:7863/status -H "Authorization: Bearer your-api-key"

# 临时停用一个账号（需 config 里 admin.enabled = true）
curl -s -X POST http://localhost:7863/admin/accounts/<uid>/disable \
  -H "Authorization: Bearer your-api-key" -H "Content-Type: application/json" \
  -d '{"reason":"观察几天"}'
# 或用 CLI（自动从 config.json 读网关地址与 key）
./acct.sh list && ./acct.sh disable <uid> 观察几天 && ./acct.sh enable <uid>

# 流式聊天
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 非流式聊天（本地聚合）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

### 可视化面板（独立进程）

网关本身不含 Web UI（见「本项目不做什么」）；配套的独立面板 `cmd/dashboard` 把网关已有的
数据端点聚合成一页——**缓存命中率**、**速度**（首字 TTFB / 吞吐）、**最近请求日志**、
**账号池状态**。面板只读、无状态、零第三方依赖，通过服务端代理访问网关（api_key 只留在
面板进程内，不下发到浏览器，也规避网关无 CORS 头）。

```bash
# 默认 :7864；网关地址与 key 自动取 config.json（也可用 -gateway / WB2A_URL 覆盖）
go run ./cmd/dashboard
go run ./cmd/dashboard -listen :9000 -gateway http://127.0.0.1:7863
```

然后浏览器打开 `http://127.0.0.1:7864/`。数据来源：

- `GET /v1/stats` — 按模型的请求 / 缓存 / 速度 / 扣费聚合（已有；默认落盘 `./data/metrics.json`，重启延续，见「请求统计持久化」）；
- `GET /status` — 账号池状态（已有）；
- `GET /v1/logs?limit=N` — **最近请求流水**（本仓库新增端点）：网关内一份**有界内存
  环形缓冲**（最近 1000 条，进程重启清零），字段与请求表格日志同构。它不依赖日志文件，
  `log_file` 为空时照常工作，也不需要面板与网关同机。

页面每 5 秒自动刷新；趋势曲线由面板侧滚动采样（刷新页面即重置）。Docker 部署时
`docker-compose.yml` 已附带 `dashboard` 服务，访问宿主机 `:7864` 即可。

> `/v1/logs` 是较新的端点：旧版网关会返回 404，页面会给出提示（其余卡片照常显示）。

### Anthropic Messages 兼容接口（Claude Code 直连）

`POST /v1/messages` 提供 Anthropic Messages 协议，供 Claude Code / 官方 Anthropic SDK 直连
（另附 `POST /v1/messages/count_tokens`）。鉴权两种头都认：`Authorization: Bearer <key>`
或 `x-api-key: <key>`。实现是一层协议垫片：请求翻译成 chat/completions 后复用同一条上游
管线（账号池 / 会话粘性 / 前缀缓存 / 指纹清洗 / 错误分类 / 成本账本全部继承）。

模型名决策：**同名家族候选链（免费档优先，用完自动换积分档）**。

客户端只写模型名即可。上游把「同一个模型」拆成了多个各自独立记账的 id——分域
（`cn:` / `global:`）、分区域变体（`-sg`）、分计费档（`x0.00` 免费 / `x0.03` 积分）。
网关把这些归到同一条**同名家族**候选链上，按「免费优先 → 倍率升序」排序，**只有当前
候选在整个池里都拿不到可用账号时**才沿链推进：免费额度在就白嫖，用完自动换积分档。
国内国外同一套规则，不用手动改配置。

1. **归一化（只作用于客户端入参）**：剥 realm 前缀（`cn:` / `global:`）、区域后缀
   （`-sg`）、上下文标记（`[1m]` / `[200k]` / `[1000000]`）——都不参与同名判定；
   出站一律用候选自身的 id（上游只认裸名）。
2. **候选排序**：`x0.00` 免费档 → 无倍率信息 → 倍率升序；同价时客户端显式写了
   `realm:` 前缀的那个域优先。注意这是**排序偏好而非硬过滤**——硬过滤会让跨域 /
   跨档兜底失效。
3. **`[1M]` 只做档位过滤**：带标记时只留上下文窗口 ≥ 标记值的候选；一个都不满足就
   退回全部候选（上游没有 1M 版本时按原来的模型跑，不退化成报错）。
4. **claude\* 兜底**：`claude-sonnet-*` 这类上游并不存在的名字 → 落到配置的默认模型
   （`anthropic.default_model`，内置 `deepseek-v4.1-flash`）；带 `global:` 前缀时
   自动映射到国际版的同一模型。
5. **未收录的模型名原样透传**：目录里没有同名家族 → 单候选透传（只剥 `[1M]` 标记），
   行为与不引入候选链时逐字一致，网关绝不臆造模型名。

**模型别名**（可选，`config model_aliases`）在候选链**之前**生效，把客户端惯用的短名换成
真实上游模型名，三条入口（`/v1/chat/completions`、`/v1/messages`、`/v1/responses`）同待遇：

```json
"model_aliases": { "deepseek-flash": "deepseek-v4.1-flash" }
```

别名键按同名家族归一化匹配（大小写 / realm 前缀 / `[1M]` 标记 / `-sg` 后缀都不影响命中），
换名时保留客户端写的前缀与标记：`global:deepseek-flash[1M]` → `global:deepseek-v4.1-flash[1M]`；
值自带 `realm:` 前缀时以值自身的域为准。

为什么值得配：模型名写错的代价很高——上游回 `11102 service info not found`，网关会把它记成
该账号的**模型级黑名单**（数小时），后续重试直接退化成 `no_healthy_account`
（`all accounts are temporarily unavailable`），看起来像账号全挂，实际只是名字写错。

链首即默认出站模型；日志 / 指标记的是**实际出站模型**，所以「免费档用完自动切到积分档」
在请求流水里直接可见。模型目录由启动预热 + 每 30 分钟续期，chat 热路径只读快照
（不在请求路径上额外打上游）；快照冷 / 过期时退化为单候选透传，不阻塞请求。

```bash
# 只写模型名：免费档在就免费，被限流自动换积分档
curl -s http://HOST:7863/v1/messages -H "x-api-key: $KEY" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"deepseek-v4.1-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'

# [1M]：只在 1M 档位的候选里挑（都不满足就退回原模型）
curl -s http://HOST:7863/v1/messages -H "x-api-key: $KEY" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"deepseek-v4.1-flash[1M]","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
```

**缓存在这一层同样是通的**：兼容层把客户端 `metadata.user_id` 映射为出站
`conversation_id`，会话因此同时拿到账号粘性与稳定的前缀缓存键。多轮对话实测（同一
`user_id`、前缀逐轮增长）：第 1 轮 `input=1281 / cache_read=0`，第 2、3 轮
`cache_read=1280`，只有增量部分计费。

```bash
# Claude Code 直连（Anthropic 协议；x-api-key 与 Authorization 均可）
curl -sN http://localhost:7863/v1/messages \
  -H "x-api-key: your-api-key" -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","max_tokens":1024,"stream":true,
       "metadata":{"user_id":"user_xxx_session_yyy"},
       "messages":[{"role":"user","content":"hi"}]}'
```

Claude Code 侧（把模型显式设成网关模型名即走透传；不配则默认 `claude-sonnet-*` 走兜底）：

```bash
export ANTHROPIC_BASE_URL=http://localhost:7863
export ANTHROPIC_API_KEY=your-api-key
export ANTHROPIC_MODEL=deepseek-v4.1-flash
```

可选配置：`"anthropic": {"default_model": "deepseek-v4.1-flash"}`（等价环境变量
`WB2A_ANTHROPIC_MODEL`）；纯国际版部署可直接填带前缀的值，如 `global:deepseek-v4.1-flash`。

### OpenAI Responses 兼容接口（Codex CLI 直连）

`POST /v1/responses` 提供 OpenAI Responses 协议，供 **Codex CLI**（`wire_api = "responses"`）
直连。鉴权与别的入口一致：`Authorization: Bearer <key>` 或 `x-api-key: <key>`。
实现同为协议垫片——请求翻译成 chat/completions 后复用同一条上游管线，账号池 / 会话粘性 /
前缀缓存 / 指纹清洗 / 错误分类 / 成本账本 / **模型别名与同名家族候选链**全部继承。

```toml
# ~/.codex/config.toml
model_provider = "workbuddy"
model = "deepseek-flash"          # 短名走 config model_aliases；也可直接写真实模型名

[model_providers.workbuddy]
name = "workbuddy"
base_url = "http://localhost:7863/v1"
wire_api = "responses"
env_key = "WB2A_API_KEY"          # 环境变量里放网关 api_key
```

请求侧翻译：`instructions` → 首条 system；`input` 条目数组里 `message`
（`developer` 归一为 `system`）→ 角色消息、`function_call` → 并簇进同一条
`assistant.tool_calls`（并行工具调用的规范形态）、`function_call_output` → 独立
`role:"tool"` 消息、`reasoning` 条目**丢弃**（上游不认，带签名的形态只在官方侧成立）；
工具定义摊平的 `name/parameters` → chat 的嵌套 `function` 形态，`namespace` /
`web_search` / `custom` 等上游无对应物的工具类型整条丢弃（直传会让上游 400 掉整个请求，
丢弃只是少一个工具）；`reasoning.effort` → 出站 `thinking` + `reasoning_effort`
（`xhigh`→`high`、`minimal`→`low`）；`text.format` → `response_format`。

`reasoning.effort` 为空时**不开思考**：Codex 常发 `{"summary":"auto"}`，那只表示
「要不要回传思考摘要」，不是「要不要思考」——见到 `reasoning` 就开思考会让每个请求平白变慢。

响应侧翻译：非流式给单个 `response` resource；流式给完整事件序列
`response.created` → `response.in_progress` → 每个条目一组
`output_item.added` / `content_part.added` / `output_text.delta`… / `content_part.done` /
`output_item.done` → `response.completed`（思考链走 `reasoning` 条目 +
`reasoning_summary_text.delta`，工具调用走 `function_call` 条目 +
`function_call_arguments.delta`）；上游流中报错终止为 `response.failed`。usage 口径与
Anthropic 层**相反**：`input_tokens` 是含缓存的总额，命中部分单列
`input_tokens_details.cached_tokens`。

**缓存在这一层同样是通的**：Codex 自带 `prompt_cache_key`（= 会话 uuid），网关**原样透传**
——同一字段同时喂饱会话粘性（`session.ExtractKey` 的 `prompt_cache_key` 兜底）与上游前缀
缓存键（`InjectPromptCacheKey` 客户端自带即保留）。实测一轮 Codex 会话（两轮请求）
`cache_hit_rate = 96.9%`（命中 12288 / 未命中 392）。

```bash
# 非流式（最小探针）
curl -s http://localhost:7863/v1/responses -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-flash","input":"say ok"}'

# 流式：事件序列与官方一致
curl -sN http://localhost:7863/v1/responses -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-flash[1M]","stream":true,"input":"hi"}'
```

模型名与候选链规则和 Anthropic 入口完全一致（同一条候选链实现），`[1M]` 档位标记、
`global:` 前缀、别名表都照常生效。

## 安全与合规

### 发布来源与合规边界

- **CI 自动打包**：GitHub Actions（`.github/workflows/build.yml`）每日定时 + push tag 触发多架构（amd64/arm64）构建，发布至 `ghcr.io`，同时输出 amd64 离线 `tar.gz` artifact 供 NAS / 离线环境使用；也可本地 `docker compose build` 自构建
- 登录 / 签到 / 积分工具：`./login.sh` / `./signin.sh` / `./credit.sh`
- **无产物校验和**：`go.sum` 仅约束 Go 模块依赖；Docker 镜像由本地 `docker compose build` 生成，未引用第三方镜像
- 上游 CodeBuddy 属第三方商业产品，本项目是其**非官方 OpenAI 兼容网关**；使用其账号做 API 网关涉及目标平台服务条款与账号风险，作者不对账号封禁、条款违约或使用结果负责

### 授权使用边界

- 仅限**本人授权账号**、本机 / 私有环境测试
- 不得共享、转售、违规分发，或用于违反目标平台条款的用途
- 遵守 CodeBuddy 平台服务条款与所在地法律
- 妥善保管 `auths/`（明文凭证）与网关端口

## 免责声明

本项目（包括但不限于代码、脚本、文档、配置示例及仓库内任何资源，下称「本项目内容」）**仅供个人学习与研究使用**。使用本项目表示您已阅读并接受本声明全部条款；如不同意，请立即停止使用并删除全部相关内容。

**1. 用途限制。** 本项目内容仅可用于个人学习、研究等非商业用途；请勿将本项目用于任何商业目的或牟利行为，请勿违反所属国家 / 地区 / 组织的任何法律法规。本项目不构成对任何软件、服务、平台的使用建议或授权。

**2. 账号与数据责任。** 本项目可能涉及个人账号凭证的获取、存储与使用。您应仅使用本人持有且已获授权的账号，自行确认相关平台的服务条款与允许范围，并自行承担使用、存储凭证（如 `auths/` 中的文件）及调用上游服务所产生的全部责任与风险。本项目不参与、不介入您与任何平台之间的契约关系。

**3. 内容与第三方界限。** 本项目内容中引用的第三方产品、服务、LOGO、图片、文案等，其权利均归各自权利人所有；本项目不保证此类内容的准确性、完整性、合法性，亦不代表支持或推荐任何第三方。如实存在侵权情形，请通过 Issues 告知，经核实后本项目会尽快处理。

**4. 无担保与风险自担。** 本项目内容按「现状」提供，不附带任何明示或默示的担保（包括但不限于适销性、特定用途适用性、准确性、不侵权等）。使用本项目（包括直接或间接）所产生的任何风险与后果（包括但不限于账号异常、数据丢失、服务中断、纠纷或损失），均由使用者自行承担，与本项目及其全部贡献者无关。

**5. 责任限定。** 在任何情况下，本项目及其作者、贡献者均不对任何直接、间接、偶然、特殊或后果性损害承担责任，无论该等损害是否基于合同、侵权或其他法律理论，即使已被告知发生该等损害的可能性。

**6. 修改与分发。** 基于本项目源代码进行的任何修改、衍生均系第三方自发行为，与本项目无关，相应后果由该第三方自行承担。本项目内所有资源文件，禁止任何公众号、自媒体进行任何形式的转载、发布。未经授权，任何组织或个人不得将本项目内容用于转载、发布或再分发。

**7. 条款变更。** 本项目保留随时修改、补充本声明的权利。修改后的声明自发布之日起生效，继续使用本项目即视为接受修订后的声明。本项目所有内容仅供学习和研究使用，请于学习研究完成后及时删除。

## ☕ Coffee

如果这个项目对你有帮助，欢迎请我喝杯咖啡～

<table>
  <tr>
    <td align="center"><b>💰 Solana</b></td>
    <td><code>AZAKF74rTu7UFVSNRzsKV4HHpTwarax6cG8KAh4fP5rQ</code></td>
  </tr>
  <tr>
    <td align="center"><b>💎 Ethereum</b></td>
    <td><code>0x1d418627aD6B043900CBE11fe439759bDF2b5170</code></td>
  </tr>
  <tr>
    <td align="center"><b>₿ Bitcoin</b></td>
    <td><code>bc1q9w7h4j9msyd9q6lhl0398n4s3g8h4vchpqvc2k</code></td>
  </tr>
</table>

## 特别感谢
- [@YuJunZhiXue](https://github.com/YuJunZhiXue)

## License

本项目采用 [MIT License](LICENSE) 开源协议。

- 在遵守 MIT License 前提下，允许使用、复制、修改、合并本项目源代码
- 再分发（源码或二进制形式）时，须保留原仓库的 MIT 版权声明与许可声明，并在 NOTICE 或 README 中注明原始出处 `https://github.com/Sliverkiss/workbuddy2api`
- 本项目不授予任何上游（CodeBuddy）接口或服务的权利；使用者仍需自行遵守上游服务条款
- 本项目的使用同时受上方**免责声明**约束；如免责声明与 MIT License 存在不一致，以免责声明为准
