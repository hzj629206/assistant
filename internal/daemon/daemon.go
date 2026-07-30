package daemon

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hzj629206/assistant/adapter"
	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/cache"
	"github.com/hzj629206/assistant/internal/config"
	"github.com/hzj629206/assistant/internal/tunnel"

	seatalkoapisdk "github.com/hzj629206/assistant/seatalk-oapi-sdk-go"
)

// RunnerFactory creates the runner for one daemon process.
type RunnerFactory func(context.Context, config.Config) (agent.Runner, error)

// Run starts the shared daemon lifecycle using the provided runner factory.
func Run(factory RunnerFactory) {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg, err := config.ParseConfig(os.Args[0], os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatalf("parse config failed: %v", err)
	}

	proc := newProcess(cfg, factory)

	startCtx, cancelStart := context.WithTimeout(context.Background(), 30*time.Second)
	if err = proc.start(startCtx); err != nil {
		cancelStart()
		log.Fatalf("start process failed: %v", err)
	}
	cancelStart()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	var runErr error
	var ok bool
	select {
	case <-ctx.Done():
		log.Printf("shutdown signal received: %v", ctx.Err())
	case runErr, ok = <-proc.errors():
		if !ok {
			log.Printf("process error channel closed")
		} else {
			log.Printf("service stopped unexpectedly: %v", runErr)
		}
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err = proc.shutdown(shutdownCtx); err != nil {
		log.Printf("process shutdown failed: %v", err)
	}
	cancel()

	log.Printf("service stopped")
	if runErr != nil {
		os.Exit(1)
	}
}

type process struct {
	cfg           config.Config
	errCh         chan error
	dispatcher    *agent.Dispatcher
	runner        agent.Runner
	wsClient      *seatalkoapisdk.Client
	wsCancel      context.CancelFunc
	httpServerMux *http.ServeMux
	httpServer    *http.Server
	remoteTunnel  tunnel.Tunnel
	runnerFactory RunnerFactory
}

func newProcess(cfg config.Config, factory RunnerFactory) *process {
	return &process{
		cfg:           cfg,
		errCh:         make(chan error, 3),
		runnerFactory: factory,
		httpServerMux: http.NewServeMux(),
	}
}

func (p *process) start(ctx context.Context) error {
	// Startup rollback intentionally reuses the startup context. If startup has already timed out,
	// failing fast is preferable to extending startup with a separate cleanup budget.
	var err error

	cache.SetGlobal(cache.NewMemoryStorage())

	p.runner, err = p.runnerFactory(ctx, p.cfg)
	if err != nil {
		return fmt.Errorf("create runner failed: %w", err)
	}
	defer func() {
		if err != nil && p.runner != nil {
			if closeErr := p.runner.Close(); closeErr != nil {
				log.Printf("runner rollback failed: %v", closeErr)
			}
		}
	}()

	p.dispatcher = agent.NewDispatcher(agent.DispatcherOptions{
		Store:      agent.NewConversationStore(cache.Global()),
		Runner:     p.runner,
		FatalErrCh: p.errCh,
	})
	defer func() {
		if err != nil {
			if shutdownErr := p.dispatcher.Shutdown(ctx); shutdownErr != nil {
				log.Printf("dispatcher rollback failed: %v", shutdownErr)
			}
		}
	}()

	//nolint:contextcheck // Dispatcher workers are process-scoped and are stopped through Shutdown.
	if err = p.dispatcher.Start(); err != nil {
		return fmt.Errorf("start dispatcher failed: %w", err)
	}

	seaTalkAdapter := adapter.NewSeaTalkAgentAdapter(p.dispatcher, p.cfg.SeaTalk)
	p.runner.RegisterSystemPrompt(seaTalkAdapter.SystemPrompt())
	p.runner.RegisterTools(seaTalkAdapter.Tools()...)

	if strings.TrimSpace(p.cfg.SeaTalk.SigningSecret) == "" {
		p.wsClient = seatalkoapisdk.NewClient(
			p.cfg.SeaTalk.AppID,
			p.cfg.SeaTalk.AppSecret,
			seatalkoapisdk.WithEventDispatcher(seaTalkAdapter.NewWebSocketEventDispatcher()),
		)
		registerResult, connectErr := p.wsClient.Connect(ctx)
		if connectErr != nil {
			return fmt.Errorf("connect SeaTalk WebSocket: %w", connectErr)
		}
		log.Printf("SeaTalk WebSocket connected: app_id=%s session_id=%s", registerResult.AppID, registerResult.Sid)
		defer func() {
			if err != nil {
				if shutdownErr := p.shutdownWebSocket(ctx); shutdownErr != nil {
					log.Printf("SeaTalk WebSocket rollback failed: %v", shutdownErr)
				}
			}
		}()

		// The WebSocket lives for the process lifetime, rather than the bounded startup context.
		wsCtx, wsCancel := context.WithCancel(context.WithoutCancel(ctx))
		p.wsCancel = wsCancel
		go p.receiveWebSocketEvents(wsCtx)
		log.Printf("SeaTalk event delivery configured for WebSocket")
	} else {
		p.httpServerMux.Handle("/callback", seaTalkAdapter.NewCallbackHandler())
		log.Printf("SeaTalk event delivery configured for HTTP callbacks")
	}

	p.httpServer = &http.Server{
		Handler:           p.httpServerMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	defer func() {
		if err != nil {
			if shutdownErr := p.shutdownHTTP(ctx); shutdownErr != nil {
				log.Printf("HTTP service rollback failed: %v", shutdownErr)
			}
		}
	}()

	if err = p.serveHTTP(ctx, p.cfg.ListenAddr); err != nil {
		return fmt.Errorf("listen on %s failed: %w", p.cfg.ListenAddr, err)
	}

	p.remoteTunnel, err = newRemoteTunnel(p.cfg.ListenAddr, p.cfg.Tunnel)
	if err != nil {
		return fmt.Errorf("create remote tunnel failed: %w", err)
	}
	if p.remoteTunnel != nil {
		defer func() {
			if err != nil {
				if shutdownErr := p.remoteTunnel.Shutdown(ctx); shutdownErr != nil {
					log.Printf("remote tunnel rollback failed: %v", shutdownErr)
				}
			}
		}()

		p.remoteTunnel.Start(ctx)
	}

	return nil
}

func (p *process) shutdown(ctx context.Context) error {
	var shutdownErr error

	if p.remoteTunnel != nil {
		if err := p.remoteTunnel.Shutdown(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("remote tunnel shutdown failed: %w", err))
		}
	}

	if p.httpServer != nil {
		if err := p.shutdownHTTP(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("HTTP service shutdown failed: %w", err))
		}
	}

	if err := p.shutdownWebSocket(ctx); err != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("SeaTalk WebSocket shutdown failed: %w", err))
	}

	if p.dispatcher != nil {
		if err := p.dispatcher.Shutdown(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("dispatcher shutdown failed: %w", err))
		}
	}
	if p.runner != nil {
		if err := p.runner.Close(); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("runner shutdown failed: %w", err))
		}
	}

	return shutdownErr
}

func (p *process) errors() <-chan error {
	return p.errCh
}
