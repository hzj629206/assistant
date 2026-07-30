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
		return tunnel.NewSSH(tunnel.SSHConfig{
			SSHAddr:         config.NormalizeRemoteSSHAddr(cfg.SSHAddr),
			SSHUser:         cfg.SSHUser,
			SSHKey:          cfg.SSHKey,
			LocalTargetAddr: localTargetAddr,
			RetryDelay:      3 * time.Second,
			MaxRetryDelay:   30 * time.Second,
		})
	}

	localTargetAddr, err := config.DeriveLocalTargetAddr(listenAddr)
	if err != nil {
		return nil, err
	}
	return tunnel.NewCloudflared(tunnel.CloudflaredConfig{
		Token:           cfg.CloudflaredToken,
		LocalTargetAddr: localTargetAddr,
		RetryDelay:      3 * time.Second,
		MaxRetryDelay:   30 * time.Second,
	})
}

func (p *process) serveHTTP(ctx context.Context, listenAddr string) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return err
	}
	go func() {
		log.Printf("HTTP server listening on %s", listener.Addr())
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
	if err := p.httpServer.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (p *process) receiveWebSocketEvents(ctx context.Context) {
	if p == nil || p.wsClient == nil {
		return
	}
	err := p.wsClient.Start(ctx)
	if err != nil && ctx.Err() == nil {
		log.Printf("SeaTalk WebSocket stopped unexpectedly: %v", err)
		p.errCh <- err
	}
}

func (p *process) shutdownWebSocket(_ context.Context) error {
	if p == nil || p.wsClient == nil {
		return nil
	}
	if p.wsCancel != nil {
		p.wsCancel()
		p.wsCancel = nil
	}
	return p.wsClient.Close()
}
