package native

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"c2agent/internal/agent"
	"c2agent/internal/crypto"
	"c2agent/internal/protocol"
)

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestNativeEndToEnd simulates a remote agent and verifies the full
// handshake, ChaCha20 stream, request forwarding and response matching.
func TestNativeEndToEnd(t *testing.T) {
	const token = "unit-test-token"
	reg := agent.NewRegistry()
	srv := NewServer(ServerOptions{
		Registry:         reg,
		Host:             "127.0.0.1",
		Port:             0,
		AuthToken:        token,
		HeartbeatTimeout: 60 * time.Second,
		CmdTimeout:       5 * time.Second,
		TimeoutAction:    "disconnect",
		QueueCapacity:    16,
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	addr := srv.Addr().String()
	agentErr := make(chan error, 1)

	go func() {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			agentErr <- err
			return
		}
		defer conn.Close()
		pr := protocol.NewPacketReader()

		// 1. register (plaintext, nonce only)
		nonce := "0123456789abcdef"
		regPkt, _ := protocol.EncodeControl(1, protocol.CmdRegister, []string{nonce})
		if _, err := conn.Write(regPkt); err != nil {
			agentErr <- err
			return
		}

		// 2. register_response (plaintext)
		resp, err := readPacketFrom(conn, pr, 5*time.Second)
		if err != nil {
			agentErr <- err
			return
		}
		if resp.Cmd != protocol.CmdRegisterResponse {
			agentErr <- errUnexpected{what: "expected register_response", got: resp.Cmd}
			return
		}
		if string(resp.Body) != hexSHA(nonce+token) {
			agentErr <- errUnexpected{what: "digest mismatch", got: resp.Cmd}
			return
		}

		// 3. enable ChaCha20 and send register_confirm (encrypted)
		txKey, txNonce := crypto.DeriveMaterial(token, nonce, crypto.DirAgentToC2)
		rxKey, rxNonce := crypto.DeriveMaterial(token, nonce, crypto.DirC2ToAgent)
		tx := crypto.NewChaCha20(txKey[:], txNonce[:], 0)
		rx := crypto.NewChaCha20(rxKey[:], rxNonce[:], 0)
		enc := crypto.NewEncryptedConn(conn, tx, rx)

		conf, _ := protocol.EncodeControl(2, protocol.CmdRegisterConfirm, []string{"agent-1", "host-1", "Linux"})
		if _, err := enc.Write(conf); err != nil {
			agentErr <- err
			return
		}

		// 4. serve requests until the test closes the connection
		for {
			pkt, err := readPacketFrom(enc, pr, time.Second)
			if err != nil {
				return
			}
			switch pkt.Cmd {
			case protocol.CmdListDir:
				enc.Write(protocol.EncodeResponse(pkt.ReqID, protocol.CmdListDir, "DIR 0 docs\nFILE 12 a.txt"))
			case protocol.CmdHeartbeat:
				enc.Write(protocol.EncodeResponse(pkt.ReqID, protocol.CmdHeartbeatAck, "1"))
			}
		}
	}()

	// Wait for registration.
	var ag *agent.Agent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ag = reg.Get("agent-1"); ag != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ag == nil {
		t.Fatal("agent was not registered")
	}
	if ag.OS != "Linux" || ag.Hostname != "host-1" {
		t.Fatalf("identity mismatch: os=%q host=%q", ag.OS, ag.Hostname)
	}

	job := agent.NewJob(agent.JobAction)
	job.Action = "list_dir"
	job.Params = map[string]any{"path": "."}
	if err := ag.Enqueue(job); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r := <-job.Result
	if r.Err != nil {
		t.Fatalf("job error: %v", r.Err)
	}
	want := "DIR 0 docs\nFILE 12 a.txt"
	if r.Output != want {
		t.Fatalf("response mismatch:\n got %q\nwant %q", r.Output, want)
	}
	select {
	case err := <-agentErr:
		t.Fatalf("agent sim error: %v", err)
	default:
	}
}

func readPacketFrom(conn net.Conn, pr *protocol.PacketReader, timeout time.Duration) (*protocol.Packet, error) {
	buf := make([]byte, 4096)
	deadline := time.Now().Add(timeout)
	for {
		pkt, err := pr.NextPacket()
		if err != nil {
			return nil, err
		}
		if pkt != nil {
			return pkt, nil
		}
		_ = conn.SetReadDeadline(deadline)
		n, err := conn.Read(buf)
		if n > 0 {
			pr.Feed(buf[:n])
		}
		if err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, errTimeout{}
		}
	}
}

// Close must not hang while an agent connection is established.
func TestServerCloseWithOpenConn(t *testing.T) {
	reg := agent.NewRegistry()
	srv := NewServer(ServerOptions{
		Registry:         reg,
		Host:             "127.0.0.1",
		Port:             0,
		AuthToken:        "tok",
		HeartbeatTimeout: 60 * time.Second,
		CmdTimeout:       5 * time.Second,
		TimeoutAction:    "disconnect",
		QueueCapacity:    8,
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	time.Sleep(100 * time.Millisecond) // connection is open but idle

	done := make(chan struct{})
	go func() { _ = srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Server.Close hung with an open connection")
	}
}

type errTimeout struct{}

func (errTimeout) Error() string { return "timeout" }

type errUnexpected struct {
	what string
	got  uint8
}

func (e errUnexpected) Error() string { return e.what }
