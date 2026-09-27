package protocol

import (
	"bytes"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	h := EncodeHeader(0x1122334455667788, 0x01020304, CmdExecCmd)
	reqID, bodyLen, cmd, err := DecodeHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if reqID != 0x1122334455667788 || bodyLen != 0x01020304 || cmd != CmdExecCmd {
		t.Fatalf("roundtrip mismatch: %x %x %#x", reqID, bodyLen, cmd)
	}
}

func TestTLVRoundTrip(t *testing.T) {
	in := []string{"hello", "", "世界", "line1\nline2"}
	buf := EncodeTLV(in)
	out, err := DecodeTLV(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("len mismatch: %v", out)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("param %d mismatch: %q != %q", i, out[i], in[i])
		}
	}
}

func TestDecodeTLVTruncated(t *testing.T) {
	buf := EncodeTLV([]string{"abc"})
	if _, err := DecodeTLV(buf[:len(buf)-1]); err == nil {
		t.Fatal("expected error on truncated TLV")
	}
}

func TestRequestRoundTrip(t *testing.T) {
	pkt, err := EncodeRequest(42, CmdReadFile, []string{"/etc/hosts", "1", "10"})
	if err != nil {
		t.Fatal(err)
	}
	reqID, _, cmd, err := DecodeHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if reqID != 42 || cmd != CmdReadFile {
		t.Fatalf("header mismatch: %d %#x", reqID, cmd)
	}
	params, err := DecodeTLV(pkt[HeaderLen:])
	if err != nil {
		t.Fatal(err)
	}
	if len(params) != 3 || params[0] != "/etc/hosts" || params[2] != "10" {
		t.Fatalf("params mismatch: %v", params)
	}
}

func TestEncodeDataPacketTooLarge(t *testing.T) {
	if _, err := EncodeDataPacket(1, EndFlagContinue, make([]byte, DataChunkSize+1)); err == nil {
		t.Fatal("expected error for oversized data chunk")
	}
}

func TestPacketReaderRejectsOversizedBody(t *testing.T) {
	h := EncodeHeader(1, MaxBodyLen+1, CmdListDir)
	pr := NewPacketReader()
	pr.Feed(h)
	if _, err := pr.NextPacket(); err == nil {
		t.Fatal("expected an error for an oversized body length")
	}
}

func TestPacketReaderHalfAndCoalesced(t *testing.T) {
	p1, _ := EncodeRequest(1, CmdListDir, []string{"."})
	p2 := EncodeResponse(1, CmdListDir, "ok")

	full := append(append([]byte{}, p1...), p2...)
	pr := NewPacketReader()
	// Feed in odd-sized chunks.
	for i := 0; i < len(full); i += 5 {
		end := i + 5
		if end > len(full) {
			end = len(full)
		}
		pr.Feed(full[i:end])
	}
	var got []*Packet
	for {
		pkt, err := pr.NextPacket()
		if err != nil {
			t.Fatal(err)
		}
		if pkt == nil {
			break
		}
		got = append(got, pkt)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 packets, got %d", len(got))
	}
	if got[0].Cmd != CmdListDir || !bytes.Equal(got[0].Body, EncodeTLV([]string{"."})) {
		t.Fatalf("packet 0 mismatch: %+v", got[0])
	}
	if got[1].Cmd != CmdListDir || string(got[1].Body) != "ok" {
		t.Fatalf("packet 1 mismatch: %+v", got[1])
	}
}
