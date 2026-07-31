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

func (p *process) serveHTTP(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", p.cfg.ListenAddr)
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

	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p.wsCancel = cancel

	go func() {
		for {
			if ctx.Err() != nil {
				return
			}

			registerResult, connectErr := p.wsClient.Connect(ctx)
			if connectErr != nil {
				if ctx.Err() != nil {
					return
				}

				log.Printf("SeaTalk WebSocket connect failed: %v", connectErr)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}

			//nolint:gosec // SeaTalk returns the app ID and session ID; both are safe diagnostic values.
			log.Printf("SeaTalk WebSocket connected: app_id=%s session_id=%s", registerResult.AppID, registerResult.Sid)

			err := p.wsClient.Start(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("SeaTalk WebSocket stopped unexpectedly: %v", err)
			} else {
				log.Printf("SeaTalk WebSocket stopped unexpectedly")
			}

			if closeErr := p.wsClient.Close(); closeErr != nil {
				log.Printf("SeaTalk WebSocket close before reconnect failed: %v", closeErr)
			}
		}
	}()
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
