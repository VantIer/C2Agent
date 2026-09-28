package protocol

import (
	"encoding/binary"
	"fmt"
)

// ProtocolError marks a malformed packet / framing violation.
type ProtocolError struct{ msg string }

func (e *ProtocolError) Error() string { return "protocol: " + e.msg }

func errf(format string, args ...any) error {
	return &ProtocolError{msg: fmt.Sprintf(format, args...)}
}

// EncodeHeader builds a 16-byte packet header.
func EncodeHeader(reqID uint64, bodyLen uint32, cmdFlag uint8) []byte {
	buf := make([]byte, HeaderLen)
	binary.LittleEndian.PutUint64(buf[RequestIDOff:], reqID)
	binary.LittleEndian.PutUint32(buf[BodyLenOff:], bodyLen)
	buf[CmdOff] = cmdFlag
	return buf
}

// DecodeHeader parses a 16-byte header.
func DecodeHeader(data []byte) (reqID uint64, bodyLen uint32, cmdFlag uint8, err error) {
	if len(data) < HeaderLen {
		return 0, 0, 0, errf("header too short: %d", len(data))
	}
	reqID = binary.LittleEndian.Uint64(data[RequestIDOff:])
	bodyLen = binary.LittleEndian.Uint32(data[BodyLenOff:])
	cmdFlag = data[CmdOff]
	return reqID, bodyLen, cmdFlag, nil
}

// EncodeTLV encodes params as a TLV chain.
func EncodeTLV(params []string) []byte {
	total := 0
	for _, p := range params {
		total += 4 + len(p)
	}
	buf := make([]byte, 0, total)
	var lenbuf [4]byte
	for _, p := range params {
		binary.LittleEndian.PutUint32(lenbuf[:], uint32(len(p)))
		buf = append(buf, lenbuf[:]...)
		buf = append(buf, p...)
	}
	return buf
}

// DecodeTLV decodes a TLV chain into strings.
func DecodeTLV(buf []byte) ([]string, error) {
	var out []string
	i := 0
	for i < len(buf) {
		if i+4 > len(buf) {
			return nil, errf("TLV truncated at %d", i)
		}
		n := uint64(binary.LittleEndian.Uint32(buf[i:]))
		i += 4
		if n > uint64(len(buf)-i) {
			return nil, errf("TLV overrun at %d (len=%d)", i, n)
		}
		out = append(out, string(buf[i:i+int(n)]))
		i += int(n)
	}
	return out, nil
}

// EncodeRequest builds a request packet (action / upload / download).
func EncodeRequest(reqID uint64, cmd uint8, params []string) ([]byte, error) {
	if !IsRequestCmd(cmd) {
		return nil, errf("unknown request cmd: %#x", cmd)
	}
	body := EncodeTLV(params)
	pkt := make([]byte, 0, HeaderLen+len(body))
	pkt = append(pkt, EncodeHeader(reqID, uint32(len(body)), cmd)...)
	pkt = append(pkt, body...)
	return pkt, nil
}

// EncodeResponse builds a response packet with a single UTF-8 body.
func EncodeResponse(reqID uint64, cmd uint8, result string) []byte {
	body := []byte(result)
	pkt := make([]byte, 0, HeaderLen+len(body))
	pkt = append(pkt, EncodeHeader(reqID, uint32(len(body)), cmd)...)
	pkt = append(pkt, body...)
	return pkt
}

// EncodeControl builds a control packet (TLV body).
func EncodeControl(reqID uint64, cmd uint8, params []string) ([]byte, error) {
	if !IsControlCmd(cmd) {
		return nil, errf("unknown control cmd: %#x", cmd)
	}
	body := EncodeTLV(params)
	pkt := make([]byte, 0, HeaderLen+len(body))
	pkt = append(pkt, EncodeHeader(reqID, uint32(len(body)), cmd)...)
	pkt = append(pkt, body...)
	return pkt, nil
}

// EncodeDataPacket builds a file-transfer data packet (cmd byte = end flag).
func EncodeDataPacket(reqID uint64, endFlag uint8, data []byte) ([]byte, error) {
	if len(data) > DataChunkSize {
		return nil, errf("data chunk too large: %d > %d", len(data), DataChunkSize)
	}
	pkt := make([]byte, 0, HeaderLen+len(data))
	pkt = append(pkt, EncodeHeader(reqID, uint32(len(data)), endFlag)...)
	pkt = append(pkt, data...)
	return pkt, nil
}
