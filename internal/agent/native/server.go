package native

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"c2agent/internal/agent"
	"c2agent/internal/crypto"
	"c2agent/internal/protocol"
)

// ServerOptions configures the native protocol listener.
type ServerOptions struct {
	Registry         *agent.Registry
	Host             string
	Port             int
	AuthToken        string
	HeartbeatTimeout time.Duration
	CmdTimeout       time.Duration
	TimeoutAction    string
	QueueCapacity    int
	Logger           *log.Logger
}

// Server accepts native agent connections and registers them.
type Server struct {
	opts     ServerOptions
	logger   *log.Logger
	ln       net.Listener
	wg       sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
	conns    sync.Map // live connections, closed on shutdown
}

// NewServer builds a native protocol server.
func NewServer(o ServerOptions) *Server {
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	return &Server{opts: o, logger: o.Logger, stop: make(chan struct{})}
}

// Start binds the listener and launches the accept + watchdog loops.
func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.opts.Host, s.opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	s.wg.Add(1)
	go s.watchdogLoop()
	return nil
}

// Addr returns the bound listener address.
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Close stops the server, closes every live connection and waits for all
// connection loops to finish.
func (s *Server) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.conns.Range(func(k, _ any) bool {
		if c, ok := k.(net.Conn); ok {
			_ = c.Close()
		}
		return true
	})
	s.wg.Wait()
	return nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stop:
				return
			default:
				// Back off on persistent accept errors (e.g. EMFILE) instead
				// of spinning tightly.
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) watchdogLoop() {
	defer s.wg.Done()
	timeout := s.opts.HeartbeatTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	interval := timeout / 2
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			now := time.Now()
			for _, a := range s.opts.Registry.List() {
				if a.Kind != agent.KindNative {
					continue
				}
				if a.ActiveOps() > 0 {
					continue
				}
				if now.Sub(a.LastHB()) > timeout {
					s.logger.Printf("native agent %s heartbeat timeout; disconnecting", a.ID)
					s.opts.Registry.UnregisterAgent(a)
				}
			}
		}
	}
}

func (s *Server) handleConn(raw net.Conn) {
	defer s.wg.Done()
	defer raw.Close()
	s.conns.Store(raw, struct{}{})
	defer s.conns.Delete(raw)

	pr := protocol.NewPacketReader()
	nonce, err := s.readRegister(raw, pr)
	if err != nil {
		s.logger.Printf("native handshake rejected from %s: %v", raw.RemoteAddr(), err)
		return
	}
	digest := sha256Hex(nonce + s.opts.AuthToken)
	if _, err := raw.Write(protocol.EncodeResponse(0, protocol.CmdRegisterResponse, digest)); err != nil {
		return
	}

	key := crypto.DeriveKey(s.opts.AuthToken)
	tx := crypto.NewChaCha20(key[:], crypto.NonceC2ToAgent(), 0)
	rx := crypto.NewChaCha20(key[:], crypto.NonceAgentToC2(), 0)

	if pr.Buffered() > 0 {
		rawBuf := pr.DrainAll()
		dec := make([]byte, len(rawBuf))
		rx.XORKeyStream(dec, rawBuf)
		pr.Feed(dec)
	}
	enc := crypto.NewEncryptedConn(raw, tx, rx)

	id, host, osName, err := s.waitConfirm(enc, pr)
	if err != nil {
		s.logger.Printf("native register_confirm failed from %s: %v", raw.RemoteAddr(), err)
		return
	}

	backend := NewBackend(enc)
	ag := agent.New(agent.Options{
		ID:            id,
		Kind:          agent.KindNative,
		Hostname:      host,
		OS:            osName,
		Backend:       backend,
		QueueCapacity: s.opts.QueueCapacity,
		CmdTimeout:    s.opts.CmdTimeout,
		TimeoutAction: s.opts.TimeoutAction,
		OnTimeout: func(a *agent.Agent) {
			s.logger.Printf("native agent %s execution timeout; disconnecting", a.ID)
			s.opts.Registry.UnregisterAgent(a)
		},
	})

	if old := s.opts.Registry.Get(id); old != nil && (old.Hostname != host || old.OS != osName) {
		s.logger.Printf("WARNING: duplicate agent id %q already held by %s/%s; replacing it with %s/%s. "+
			"Agent ids must be unique per controlled end, otherwise the two connections will keep evicting each other.",
			id, old.Hostname, old.OS, host, osName)
	}

	s.opts.Registry.Register(ag)
	s.logger.Printf("native agent registered: id=%s host=%s os=%s", id, host, osName)

	s.readLoop(enc, pr, backend, ag)
	s.opts.Registry.UnregisterAgent(ag)
}

func (s *Server) readRegister(conn net.Conn, pr *protocol.PacketReader) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	buf := make([]byte, 4096)
	for {
		pkt, err := pr.NextPacket()
		if err != nil {
			return "", err
		}
		if pkt != nil {
			if pkt.Cmd != protocol.CmdRegister {
				return "", fmt.Errorf("first packet not register: %#x", pkt.Cmd)
			}
			params, derr := protocol.DecodeTLV(pkt.Body)
			if derr != nil || len(params) == 0 || params[0] == "" {
				return "", fmt.Errorf("register missing nonce")
			}
			return params[0], nil
		}
		_ = conn.SetReadDeadline(deadline)
		n, rerr := conn.Read(buf)
		if n > 0 {
			pr.Feed(buf[:n])
		}
		if rerr != nil {
			return "", rerr
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("register timeout")
		}
	}
}

func (s *Server) waitConfirm(conn net.Conn, pr *protocol.PacketReader) (string, string, string, error) {
	deadline := time.Now().Add(30 * time.Second)
	buf := make([]byte, 4096)
	for {
		pkt, err := pr.NextPacket()
		if err != nil {
			return "", "", "", err
		}
		if pkt != nil {
			if pkt.Cmd != protocol.CmdRegisterConfirm {
				return "", "", "", fmt.Errorf("expected register_confirm, got %#x", pkt.Cmd)
			}
			params, derr := protocol.DecodeTLV(pkt.Body)
			if derr != nil || len(params) < 3 || params[0] == "" {
				return "", "", "", fmt.Errorf("register_confirm missing identity")
			}
			return params[0], params[1], params[2], nil
		}
		_ = conn.SetReadDeadline(deadline)
		n, rerr := conn.Read(buf)
		if n > 0 {
			pr.Feed(buf[:n])
		}
		if rerr != nil {
			return "", "", "", rerr
		}
		if time.Now().After(deadline) {
			return "", "", "", fmt.Errorf("register_confirm timeout")
		}
	}
}

func (s *Server) readLoop(conn net.Conn, pr *protocol.PacketReader, backend *Backend, ag *agent.Agent) {
	buf := make([]byte, 4096)
	for {
		pkt, err := pr.NextPacket()
		if err != nil {
			return
		}
		if pkt != nil {
			if !s.dispatch(backend, ag, pkt) {
				return
			}
			continue
		}
		_ = conn.SetReadDeadline(time.Time{})
		n, rerr := conn.Read(buf)
		if n > 0 {
			pr.Feed(buf[:n])
		}
		if rerr != nil {
			return
		}
	}
}

func (s *Server) dispatch(backend *Backend, ag *agent.Agent, pkt *protocol.Packet) bool {
	switch pkt.Cmd {
	case protocol.CmdHeartbeat:
		s.opts.Registry.TouchHB(ag.ID)
		params, _ := protocol.DecodeTLV(pkt.Body)
		ts := ""
		if len(params) > 0 {
			ts = params[0]
		}
		if ts == "" {
			ts = fmt.Sprintf("%d", time.Now().Unix())
		}
		_ = backend.write(protocol.EncodeResponse(pkt.ReqID, protocol.CmdHeartbeatAck, ts))
		return true
	case protocol.CmdDisconnect:
		return false
	case protocol.CmdRegisterResponse, protocol.CmdShutdown:
		return true
	default:
		backend.deliver(pkt)
		return true
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
