package protocol

// Packet is one decoded frame.
type Packet struct {
	ReqID uint64
	Cmd   uint8
	Body  []byte
}

// PacketReader is a stream-oriented frame parser that tolerates half packets
// and coalesced packets.
type PacketReader struct {
	buf []byte
}

// NewPacketReader returns an empty reader.
func NewPacketReader() *PacketReader { return &PacketReader{} }

// Feed appends raw bytes to the internal buffer.
func (r *PacketReader) Feed(data []byte) { r.buf = append(r.buf, data...) }

// Buffered reports how many undecoded bytes are held.
func (r *PacketReader) Buffered() int { return len(r.buf) }

// NextPacket returns the next complete packet, or (nil, nil) when more bytes
// are needed. A malformed header returns an error.
func (r *PacketReader) NextPacket() (*Packet, error) {
	if len(r.buf) < HeaderLen {
		return nil, nil
	}
	reqID, bodyLen, cmd, err := DecodeHeader(r.buf)
	if err != nil {
		return nil, err
	}
	if int(bodyLen) > MaxBodyLen {
		return nil, errf("body too large: %d > %d", bodyLen, MaxBodyLen)
	}
	total := HeaderLen + int(bodyLen)
	if len(r.buf) < total {
		return nil, nil
	}
	body := make([]byte, bodyLen)
	copy(body, r.buf[HeaderLen:total])
	r.buf = r.buf[total:]
	pkt := &Packet{ReqID: reqID, Cmd: cmd, Body: body}
	return pkt, nil
}

// DrainAll returns and clears all buffered bytes.
func (r *PacketReader) DrainAll() []byte {
	out := r.buf
	r.buf = nil
	return out
}
