package main

// Wire protocol shared with the C2 (byte-for-byte compatible with
// remote/common/protocol.py, remote/remote-c, and the Go control end).
//
// Header (16 bytes):
//   [0..7]   request_id  uint64 LE
//   [8..11]  body_len    uint32 LE
//   [12..14] reserved    3 bytes (zero)
//   [15]     cmd / flag  uint8
//
// Request body  : TLV chain (uint32 LE length + UTF-8 data)
// Response body : single UTF-8 string
// Data packet   : raw file bytes (binary-safe)

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	headerLen     = 16
	requestIDOff  = 0
	bodyLenOff    = 8
	cmdOff        = 15
	dataChunkSize = 1024
	readFileLimit = 51200
	maxBodyLen    = 32 << 20 // upper bound on a single packet body (32 MiB)
)

// Action command codes.
const (
	cmdListDir    = 0x01
	cmdMakeDir    = 0x02
	cmdDeleteDir  = 0x03
	cmdRenameDir  = 0x04
	cmdReadFile   = 0x05
	cmdWriteFile  = 0x06
	cmdDeleteFile = 0x07
	cmdEditFile   = 0x08
	cmdRenameFile = 0x09
	cmdCopy       = 0x0A
	cmdMove       = 0x0B
	cmdUpload     = 0x0C
	cmdDownload   = 0x0D
	cmdCreateFile = 0x0E
	cmdGetCwd     = 0x0F
	cmdExecCmd    = 0x10
)

// Control command codes.
const (
	cmdRegister         = 0x80
	cmdRegisterResponse = 0x81
	cmdHeartbeat        = 0x82
	cmdHeartbeatAck     = 0x83
	cmdDisconnect       = 0x84
	cmdShutdown         = 0x85
	cmdRegisterConfirm  = 0x86
)

// Data packet end flags (occupy the cmd byte).
const (
	endFlagContinue = 0
	endFlagLast     = 1
)

var actionParams = map[uint8][]string{
	cmdListDir:    {"path"},
	cmdMakeDir:    {"path"},
	cmdDeleteDir:  {"path"},
	cmdRenameDir:  {"path", "new_name"},
	cmdReadFile:   {"path", "start_line", "end_line"},
	cmdWriteFile:  {"path", "content"},
	cmdDeleteFile: {"path"},
	cmdEditFile:   {"path", "operation", "start_line", "end_line", "content"},
	cmdRenameFile: {"path", "new_name"},
	cmdCopy:       {"src", "dest"},
	cmdMove:       {"src", "dest"},
	cmdCreateFile: {"path"},
	cmdGetCwd:     nil,
	cmdExecCmd:    {"command"},
}

func isActionCmd(cmd uint8) bool { _, ok := actionParams[cmd]; return ok }

// packet is one decoded frame.
type packet struct {
	reqID uint64
	cmd   uint8
	body  []byte
}

// packetReader tolerates half packets and coalesced packets.
type packetReader struct{ buf []byte }

func (r *packetReader) feed(data []byte) { r.buf = append(r.buf, data...) }

func (r *packetReader) next() (*packet, error) {
	if len(r.buf) < headerLen {
		return nil, nil
	}
	reqID := binary.LittleEndian.Uint64(r.buf[requestIDOff:])
	bodyLen := binary.LittleEndian.Uint32(r.buf[bodyLenOff:])
	cmd := r.buf[cmdOff]
	if int(bodyLen) > maxBodyLen {
		return nil, errors.New("packet body too large")
	}
	total := headerLen + int(bodyLen)
	if len(r.buf) < total {
		return nil, nil
	}
	body := make([]byte, bodyLen)
	copy(body, r.buf[headerLen:total])
	r.buf = r.buf[total:]
	return &packet{reqID: reqID, cmd: cmd, body: body}, nil
}

func encodeHeader(reqID uint64, bodyLen uint32, cmd uint8) []byte {
	buf := make([]byte, headerLen)
	binary.LittleEndian.PutUint64(buf[requestIDOff:], reqID)
	binary.LittleEndian.PutUint32(buf[bodyLenOff:], bodyLen)
	buf[cmdOff] = cmd
	return buf
}

func encodeTLV(params []string) []byte {
	buf := make([]byte, 0, 64)
	var l [4]byte
	for _, p := range params {
		binary.LittleEndian.PutUint32(l[:], uint32(len(p)))
		buf = append(buf, l[:]...)
		buf = append(buf, p...)
	}
	return buf
}

func decodeTLV(body []byte) ([]string, error) {
	var out []string
	i := 0
	for i < len(body) {
		if i+4 > len(body) {
			return nil, errors.New("TLV truncated")
		}
		n := int(binary.LittleEndian.Uint32(body[i:]))
		i += 4
		if n < 0 || i+n > len(body) {
			return nil, errors.New("TLV overrun")
		}
		out = append(out, string(body[i:i+n]))
		i += n
	}
	return out, nil
}

func buildRequest(reqID uint64, cmd uint8, params []string) []byte {
	body := encodeTLV(params)
	pkt := append(encodeHeader(reqID, uint32(len(body)), cmd), body...)
	return pkt
}

func buildResponse(reqID uint64, cmd uint8, result string) []byte {
	body := []byte(result)
	return append(encodeHeader(reqID, uint32(len(body)), cmd), body...)
}

func buildDataPacket(reqID uint64, endFlag uint8, data []byte) []byte {
	return append(encodeHeader(reqID, uint32(len(data)), endFlag), data...)
}

// readPacket pulls one complete packet from pr, fetching from conn as needed.
// Callers (e.g. handleUpload) are responsible for skipping interleaved control
// packets (heartbeat acks) during transfers.
func readPacket(conn net.Conn, pr *packetReader, timeout time.Duration) (*packet, error) {
	buf := make([]byte, 4096)
	deadline := time.Now().Add(timeout)
	for {
		pkt, err := pr.next()
		if err != nil {
			return nil, err
		}
		if pkt != nil {
			return pkt, nil
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		n, rerr := conn.Read(buf)
		if n > 0 {
			pr.feed(buf[:n])
		}
		if rerr != nil {
			return nil, rerr
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("read timeout")
		}
	}
}
