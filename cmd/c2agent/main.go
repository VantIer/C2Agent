// Command c2agent is the C2Agent control端 entry point. It starts the selected
// controlled-end listeners (native protocol and/or reverse shell) and runs
// either the CLI or the browser panel.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"c2agent/internal/agent"
	native "c2agent/internal/agent/native"
	"c2agent/internal/agent/shell"
	"c2agent/internal/cli"
	"c2agent/internal/config"
	"c2agent/internal/engine"
	"c2agent/internal/web"
)

func main() {
	mode := flag.String("mode", "cli", "run mode: cli | web")
	cfgPath := flag.String("config", "config_c2.json", "path to the JSON config file")
	flag.Parse()

	logger := log.New(os.Stderr, "[c2agent] ", log.LstdFlags)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	reg := agent.NewRegistry()
	cmdTimeout := time.Duration(cfg.Policy.CmdTimeout) * time.Second

	var nativeSrv *native.Server
	if cfg.Native.Enabled {
		nativeSrv = native.NewServer(native.ServerOptions{
			Registry:         reg,
			Host:             cfg.Native.ListenHost,
			Port:             cfg.Native.ListenPort,
			AuthToken:        cfg.Native.AuthToken,
			HeartbeatTimeout: time.Duration(cfg.Native.HeartbeatTimeoutSec) * time.Second,
			CmdTimeout:       cmdTimeout,
			TimeoutAction:    cfg.Policy.TimeoutAction,
			QueueCapacity:    cfg.Policy.QueueCapacity,
			Logger:           logger,
		})
		if err := nativeSrv.Start(); err != nil {
			logger.Fatalf("native listener: %v", err)
		}
		logger.Printf("native protocol listening on %s", nativeSrv.Addr())
	}

	var shellSrv *shell.Server
	if cfg.Shell.Enabled {
		shellSrv = shell.NewServer(shell.ServerOptions{
			Registry:      reg,
			Host:          cfg.Shell.ListenHost,
			Port:          cfg.Shell.ListenPort,
			BotPrefix:     cfg.Shell.BotPrefix,
			CmdTimeout:    cmdTimeout,
			TimeoutAction: cfg.Policy.TimeoutAction,
			QueueCapacity: cfg.Policy.QueueCapacity,
			Logger:        logger,
		})
		if err := shellSrv.Start(); err != nil {
			logger.Fatalf("shell listener: %v", err)
		}
		logger.Printf("reverse-shell listening on %s", shellSrv.Addr())
	}

	if nativeSrv == nil && shellSrv == nil {
		logger.Fatal("no controlled-end listener enabled (native.enabled / shell.enabled)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng := engine.New(cfg, reg)

	switch *mode {
	case "cli":
		done := make(chan struct{})
		go func() {
			cli.Run(cfg, eng)
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			logger.Printf("interrupt received; shutting down")
		}
	case "web":
		srv := web.New(cfg, eng)
		webErr := make(chan error, 1)
		go func() { webErr <- srv.ListenAndServe() }()
		logger.Printf("web panel on http://%s:%d", cfg.Web.ListenHost, cfg.Web.ListenPort)
		select {
		case <-ctx.Done():
			logger.Printf("interrupt received; shutting down")
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(shutCtx)
			cancel()
		case err := <-webErr:
			if err != nil && err != http.ErrServerClosed {
				logger.Fatalf("web: %v", err)
			}
		}
	default:
		logger.Fatalf("unknown mode %q (use cli|web)", *mode)
	}

	if nativeSrv != nil {
		_ = nativeSrv.Close()
	}
	if shellSrv != nil {
		_ = shellSrv.Close()
	}
}
