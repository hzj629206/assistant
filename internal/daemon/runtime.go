package daemon

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/hzj629206/assistant/internal/config"
	"github.com/hzj629206/assistant/internal/tunnel"
)

func newRemoteTunnel(listenAddr string, cfg config.TunnelConfig) (tunnel.Tunnel, error) {
	if cfg.SSHAddr == "" && cfg.CloudflaredToken == "" {
		return nil, nil
	}

	if cfg.SSHAddr != "" {
		localTargetAddr, err := config.DeriveLocalTargetAddr(listenAddr)
		if err != nil {
			return nil, err
		}

		remoteTunnel, err := tunnel.NewSSH(tunnel.SSHConfig{
			SSHAddr:         config.NormalizeRemoteSSHAddr(cfg.SSHAddr),
			SSHUser:         cfg.SSHUser,
			SSHKey:          cfg.SSHKey,
			LocalTargetAddr: localTargetAddr,
			RetryDelay:      3 * time.Second,
			MaxRetryDelay:   30 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		return remoteTunnel, nil
	}

	localTargetAddr, err := config.DeriveLocalTargetAddr(listenAddr)
	if err != nil {
		return nil, err
	}

	remoteTunnel, err := tunnel.NewCloudflared(tunnel.CloudflaredConfig{
		Token:           cfg.CloudflaredToken,
		LocalTargetAddr: localTargetAddr,
		RetryDelay:      3 * time.Second,
		MaxRetryDelay:   30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return remoteTunnel, nil
}

func newHTTPServer(callbackHandler http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/callback", callbackHandler)

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func (p *process) serveHTTP(ctx context.Context, listenAddr string) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return err
	}

	go func() {
		log.Printf("http server listening on %s", listener.Addr())

		// Treat local listener failure as a process-level failure because it is the only
		// in-process HTTP entry point managed by the daemon.
		serveErr := p.httpServer.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			p.errCh <- serveErr
		}
	}()

	return nil
}

func (p *process) shutdownHTTP(ctx context.Context) error {
	if p == nil || p.httpServer == nil {
		return nil
	}

	var shutdownErr error
	if err := p.httpServer.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		shutdownErr = errors.Join(shutdownErr, err)
	}

	return shutdownErr
}
