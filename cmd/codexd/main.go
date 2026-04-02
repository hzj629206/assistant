package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	codexcli "github.com/godeps/codex-sdk-go"
	codexapp "github.com/pmenglund/codex-sdk-go"

	"github.com/hzj629206/assistant/adapter"
	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/cache"
	"github.com/hzj629206/assistant/config"
	"github.com/hzj629206/assistant/seatalk"

	"golang.org/x/crypto/ssh"
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func main() {
	cfg, err := config.ParseConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatalf("parse config failed: %v", err)
	}

	proc := newProcess(cfg)

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
	cfg         config.Config
	errCh       chan error
	dispatcher  *agent.Dispatcher
	runner      agent.Runner
	httpService *httpService
}

func newProcess(cfg config.Config) *process {
	return &process{
		cfg:   cfg,
		errCh: make(chan error, 3),
	}
}

func (p *process) start(ctx context.Context) error {
	// Startup rollback intentionally reuses the startup context. If startup has already timed out,
	// failing fast is preferable to extending startup with a separate cleanup budget.

	cache.SetGlobal(cache.NewMemoryStorage())

	runner, err := newRunner(ctx, p.cfg.Codex)
	if err != nil {
		return fmt.Errorf("create runner failed: %w", err)
	}
	p.runner = runner
	defer func() {
		if err != nil && runner != nil {
			if closeErr := closeRunner(runner); closeErr != nil {
				log.Printf("runner rollback failed: %v", closeErr)
			}
		}
	}()

	p.dispatcher = agent.NewDispatcher(agent.DispatcherOptions{
		Store:      agent.NewConversationStore(cache.Global()),
		Runner:     runner,
		FatalErrCh: p.errCh,
	})
	if err = p.dispatcher.Start(); err != nil { //nolint:contextcheck
		return fmt.Errorf("start dispatcher failed: %w", err)
	}
	defer func() {
		if err != nil {
			if shutdownErr := p.dispatcher.Shutdown(ctx); shutdownErr != nil {
				log.Printf("dispatcher rollback failed: %v", shutdownErr)
			}
		}
	}()

	seaTalkAdapter := adapter.NewSeaTalkAgentAdapter(p.dispatcher, p.cfg.SeaTalk)
	runner.RegisterSystemPrompt(seaTalkAdapter.SystemPrompt())
	runner.RegisterTools(seaTalkAdapter.Tools()...)
	callbackHandler := seatalk.NewCallbackHandler(p.cfg.SeaTalk, seaTalkAdapter)

	p.httpService = newHTTPService(newHTTPServer(callbackHandler), p.errCh)
	defer func() {
		if err != nil {
			if shutdownErr := p.httpService.shutdown(ctx); shutdownErr != nil {
				log.Printf("http service rollback failed: %v", shutdownErr)
			}
		}
	}()

	localListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", p.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s failed: %w", p.cfg.ListenAddr, err)
	}
	p.httpService.serve(localListener, "local")

	remoteListener, err := tryRemoteListen(p.cfg)
	if err != nil {
		return fmt.Errorf("remote listen failed: %w", err)
	}
	if remoteListener != nil {
		p.httpService.serve(remoteListener, "remote")
	}

	return nil
}

func (p *process) shutdown(ctx context.Context) error {
	var shutdownErr error

	if p.httpService != nil {
		if err := p.httpService.shutdown(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("http service shutdown failed: %w", err))
		}
	}

	if p.dispatcher != nil {
		if err := p.dispatcher.Shutdown(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("dispatcher shutdown failed: %w", err))
		}
	}

	if p.runner != nil {
		if err := closeRunner(p.runner); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("runner shutdown failed: %w", err))
		}
	}

	return shutdownErr
}

func (p *process) errors() <-chan error {
	return p.errCh
}

func newRunner(ctx context.Context, cfg config.CodexConfig) (agent.Runner, error) {
	switch cfg.Backend {
	case "appserver":
		return agent.NewAppServerRunner(ctx, agent.AppServerRunnerOptions{
			StartOptions: codexapp.ThreadStartOptions{
				Model: cfg.Model,
			},
			ResumeOptions: codexapp.ThreadResumeOptions{
				Model: cfg.Model,
			},
			TurnOptions: codexapp.TurnOptions{
				Model:  cfg.Model,
				Effort: appServerReasoningEffort(cfg.ReasoningEffort),
			},
		})
	case "cli":
		return agent.NewCodexRunner(agent.CodexRunnerOptions{
			ThreadOptions: codexcli.ThreadOptions{
				Model:                cfg.Model,
				ModelReasoningEffort: codexReasoningEffort(cfg.ReasoningEffort),
			},
		}), nil
	case "noop":
		return &agent.NoopRunner{}, nil
	default:
		return nil, fmt.Errorf("unsupported codex backend %q", cfg.Backend)
	}
}

func closeRunner(runner agent.Runner) error {
	type closer interface {
		Close() error
	}

	typed, ok := runner.(closer)
	if !ok {
		return nil
	}
	return typed.Close()
}

func appServerReasoningEffort(value string) any {
	switch value {
	case "none":
		return codexapp.ReasoningEffortNone
	case "minimal":
		return codexapp.ReasoningEffortMinimal
	case "medium":
		return codexapp.ReasoningEffortMedium
	case "high":
		return codexapp.ReasoningEffortHigh
	case "xhigh":
		return codexapp.ReasoningEffortXHigh
	default:
		return codexapp.ReasoningEffortLow
	}
}

func codexReasoningEffort(value string) codexcli.ModelReasoningEffort {
	switch value {
	case "minimal":
		return codexcli.ReasoningMinimal
	case "medium":
		return codexcli.ReasoningMedium
	case "high":
		return codexcli.ReasoningHigh
	case "xhigh":
		return codexcli.ReasoningXHigh
	default:
		return codexcli.ReasoningLow
	}
}

func tryRemoteListen(cfg config.Config) (net.Listener, error) {
	if cfg.RemoteSSHAddr == "" {
		return nil, nil
	}

	remoteListenAddr, err := config.DeriveRemoteListenAddr(cfg.ListenAddr)
	if err != nil {
		return nil, err
	}

	signer, err := loadPrivateKey(cfg.SSHKeyPath)
	if err != nil {
		return nil, err
	}

	sshConfig := &ssh.ClientConfig{
		User: cfg.RemoteSSHUser,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // Personal project: host key verification is intentionally skipped.
	}

	client, err := ssh.Dial("tcp", config.NormalizeRemoteSSHAddr(cfg.RemoteSSHAddr), sshConfig)
	if err != nil {
		return nil, err
	}

	// Check sshd config: AllowTcpForwarding=yes, GatewayPorts=clientspecified.
	ln, err := client.Listen("tcp", remoteListenAddr)
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	log.Printf("remote ssh listener ready: %s over %s", remoteListenAddr, cfg.RemoteSSHAddr)

	return &sshRemoteListener{
		Listener: ln,
		client:   client,
	}, nil
}

func ignoreNetClosed(err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}

type sshRemoteListener struct {
	net.Listener

	client *ssh.Client
	once   sync.Once
	err    error
}

func (l *sshRemoteListener) Close() error {
	l.once.Do(func() {
		l.err = errors.Join(
			ignoreNetClosed(l.Listener.Close()),
			ignoreNetClosed(l.client.Close()),
		)
	})

	return l.err
}

type httpService struct {
	server    *http.Server
	errCh     chan<- error
	mu        sync.Mutex
	listeners []net.Listener
}

func newHTTPService(server *http.Server, errCh chan<- error) *httpService {
	return &httpService{
		server: server,
		errCh:  errCh,
	}
}

func (s *httpService) serve(listener net.Listener, name string) {
	s.mu.Lock()
	s.listeners = append(s.listeners, listener)
	s.mu.Unlock()

	go func() {
		// Treat any listener failure as a process-level failure. Local and remote listeners
		// are two entry points for the same callback service, not independent availability domains.
		if serveErr := serveHTTP(s.server, listener, name); serveErr != nil {
			s.errCh <- serveErr
		}
	}()
}

func (s *httpService) shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}

	var shutdownErr error
	if err := s.server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		shutdownErr = errors.Join(shutdownErr, err)
	}

	s.mu.Lock()
	listeners := s.listeners
	s.listeners = nil
	s.mu.Unlock()

	for _, listener := range listeners {
		if listener == nil {
			continue
		}
		shutdownErr = errors.Join(shutdownErr, ignoreNetClosed(listener.Close()))
	}

	return shutdownErr
}

func newHTTPServer(callbackHandler http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/callback", callbackHandler)

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func serveHTTP(server *http.Server, listener net.Listener, name string) error {
	log.Printf("http server listening on %s (%s)", listener.Addr(), name)
	err := server.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func loadPrivateKey(path string) (ssh.Signer, error) {
	//nolint:gosec // SSH private key path comes from trusted local configuration.
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(key)
}
