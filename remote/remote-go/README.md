# remote-go — Go 受控端 / Go controlled end

`c2agent_remote` 的 **Go 实现**，与 `remote-c` / `remote-py` 及 Go 控制端**逐字节协议兼容**
（16B 包头 + TLV + 挑战-响应注册 + 认证后 ChaCha20 全流量加密）。**零三方依赖**，纯 Go。

A **Go implementation** of the C2Agent controlled end, byte-for-byte compatible with
the native protocol (16B header + TLV + challenge-response registration + ChaCha20)
used by the C2 and the C/Python agents. **Zero third-party dependencies.**

## 编译 / Build

```bash
cd remote/remote-go
go build -o c2agent_remote .            # 当前平台 / current platform

# 交叉编译 / cross-compile
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o c2agent_remote_linux_amd64 .
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -o c2agent_remote_linux_arm64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o c2agent_remote_windows_amd64.exe .
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o c2agent_remote_darwin_arm64 .
```

## 运行 / Run

```bash
./c2agent_remote \
    --c2-address 192.168.1.100:8881 \
    --agent-id server-01 \
    --auth-token <与 C2 的 native.auth_token 一致> \
    --heartbeat-interval 30 \
    --cmd-timeout 60 \
    --reconnect-initial 1 \
    --reconnect-max 60
```

或使用扁平 JSON 配置（键名与 `config_remote.json` 相同）：

```json
{
  "c2_address": "192.168.1.100:8881",
  "agent_id": "server-01",
  "auth_token": "change-me-shared-token",
  "heartbeat_interval_sec": 30,
  "cmd_timeout": 60,
  "reconnect_initial_sec": 1,
  "reconnect_max_sec": 60
}
```

```bash
./c2agent_remote --config config_remote.json
```

命令行参数优先于配置文件；必填：`c2-address` / `agent-id` / `auth-token`。

## 实现要点 / Implementation notes

- **无头守护进程**：主动拨号 → 注册 → 心跳 → 处理指令；断线按指数退避重连。
- **握手**：`register(随机 nonce，明文)` → 校验 `register_response = sha256(nonce+token)` →
  `register_confirm(agent_id/hostname/os，已加密)`；其后全流量 ChaCha20
  （密钥 = `sha256(auth_token)`，方向独立 nonce）。
- **心跳**：每 `heartbeat-interval` 秒发送；读取使用读超时以便在空闲时也按时发送。
- **动作**：`get_cwd` / `list_dir` / `make_dir` / `create_file` / `delete_dir` / `delete_file` /
  `rename_dir` / `rename_file` / `read_file`（整文件截断 51200 字符、支持行范围）/
  `write_file` / `edit_file`(add/del/modify) / `copy` / `move` / `exec_cmd`（带超时）。
- **文件传输**：`upload`（接收 1024B 数据包落盘）/ `download`（分块发送）。
- **Windows**：`exec_cmd` 经 `cmd /C` 执行；子进程按控制台/OEM 代码页输出的字节会由 `exec_windows.go` 转码为 UTF-8（取 `GetConsoleOutputCP`，回退 `GetOEMCP`），非 ASCII（如中文）可直接显示。

## 目录 / Files

```
remote-go/
├── main.go         # 入口：配置/参数、拨号、握手、心跳、分发
├── protocol.go     # 包头/TLV 编解码、PacketReader、阻塞取包
├── crypto.go       # ChaCha20 + EncryptedConn + key 派生
├── actions.go      # 本地文件动作
├── exec.go         # 跨平台 shell 执行（含超时）
├── exec_windows.go # Windows 控制台/OEM 代码页→UTF-8 转码
└── exec_other.go   # 非 Windows：恒等透传
```
