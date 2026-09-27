// Package protocol implements the wire format shared with the native
// controlled-end agents (remote-c / remote-py). It is byte-for-byte
// compatible with common/protocol.py.
//
// Packet layout (16-byte header + variable body):
//
//	[0..7]   request_id  uint64 LE
//	[8..11]  body_len    uint32 LE
//	[12..14] reserved    3 bytes (zero)
//	[15]     cmd / flag  uint8
//
// Request body:  TLV chain (uint32 LE length + UTF-8 data)
// Response body: single UTF-8 string
// Data packet:   raw file bytes (binary-safe)
package protocol

// Header / framing constants.
const (
	HeaderLen     = 16
	RequestIDOff  = 0
	BodyLenOff    = 8
	CmdOff        = 15
	DataChunkSize = 1024
	ReadFileLimit = 51200    // whole-file read truncation (matches remote constant)
	MaxBodyLen    = 32 << 20 // upper bound on a single packet body (32 MiB)
)

// Action command codes (C2 -> Agent; the Agent echoes the same code back).
const (
	CmdListDir    = 0x01
	CmdMakeDir    = 0x02
	CmdDeleteDir  = 0x03
	CmdRenameDir  = 0x04
	CmdReadFile   = 0x05
	CmdWriteFile  = 0x06
	CmdDeleteFile = 0x07
	CmdEditFile   = 0x08
	CmdRenameFile = 0x09
	CmdCopy       = 0x0A
	CmdMove       = 0x0B
	CmdUpload     = 0x0C
	CmdDownload   = 0x0D
	CmdCreateFile = 0x0E
	CmdGetCwd     = 0x0F
	CmdExecCmd    = 0x10
)

// Control command codes.
const (
	CmdRegister         = 0x80
	CmdRegisterResponse = 0x81
	CmdHeartbeat        = 0x82
	CmdHeartbeatAck     = 0x83
	CmdDisconnect       = 0x84
	CmdShutdown         = 0x85
	CmdRegisterConfirm  = 0x86
)

// File-transfer data packet end flags (occupy the cmd byte).
const (
	EndFlagContinue = 0
	EndFlagLast     = 1
)

// Action describes one action command: its code, name and ordered TLV params.
type Action struct {
	Code   uint8
	Name   string
	Params []string
}

// Actions lists every action command in code order (matching ACTION_CMDS).
var Actions = []Action{
	{CmdListDir, "list_dir", []string{"path"}},
	{CmdMakeDir, "make_dir", []string{"path"}},
	{CmdDeleteDir, "delete_dir", []string{"path"}},
	{CmdRenameDir, "rename_dir", []string{"path", "new_name"}},
	{CmdReadFile, "read_file", []string{"path", "start_line", "end_line"}},
	{CmdWriteFile, "write_file", []string{"path", "content"}},
	{CmdDeleteFile, "delete_file", []string{"path"}},
	{CmdEditFile, "edit_file", []string{"path", "operation", "start_line", "end_line", "content"}},
	{CmdRenameFile, "rename_file", []string{"path", "new_name"}},
	{CmdCopy, "copy", []string{"src", "dest"}},
	{CmdMove, "move", []string{"src", "dest"}},
	{CmdCreateFile, "create_file", []string{"path"}},
	{CmdGetCwd, "get_cwd", nil},
	{CmdExecCmd, "exec_cmd", []string{"command"}},
}

var (
	actionByName = map[string]Action{}
	// requestCmds are codes valid in encode_request (actions + upload/download).
	requestCmds = map[uint8]bool{CmdUpload: true, CmdDownload: true}
)

func init() {
	for _, a := range Actions {
		actionByName[a.Name] = a
		requestCmds[a.Code] = true
	}
}

// ActionByName returns the action for a name.
func ActionByName(name string) (Action, bool) {
	a, ok := actionByName[name]
	return a, ok
}

// IsRequestCmd reports whether code is a valid request (action/upload/download).
func IsRequestCmd(code uint8) bool { return requestCmds[code] }

// IsControlCmd reports whether code is a control command.
func IsControlCmd(code uint8) bool {
	return code >= 0x80 && code <= 0x86
}
