package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHConfig contains the SSH reverse tunnel parameters.
type SSHConfig struct {
	SSHAddr         string
	SSHUser         string
	SSHKey          string
	LocalTargetAddr string
	RetryDelay      time.Duration
	MaxRetryDelay   time.Duration
}

// NewSSH creates an SSH reverse tunnel that proxies traffic to the local service port.
func NewSSH(cfg SSHConfig) (Tunnel, error) {
	if strings.TrimSpace(cfg.SSHAddr) == "" {
		return nil, nil
	}

	localTargetAddr, err := validateLocalTargetAddr(cfg.LocalTargetAddr)
	if err != nil {
		return nil, err
	}

	signer, err := loadPrivateKey(cfg.SSHKey)
	if err != nil {
		return nil, err
	}

	remoteListenAddr, err := deriveRemoteListenAddr(localTargetAddr)
	if err != nil {
		return nil, err
	}

	return &sshTunnel{
		remoteSSHAddr:    cfg.SSHAddr,
		remoteSSHUser:    cfg.SSHUser,
		localTargetAddr:  localTargetAddr,
		remoteListenAddr: remoteListenAddr,
		signer:           signer,
		retryDelay:       cfg.RetryDelay,
		maxRetryDelay:    cfg.MaxRetryDelay,
		activeSessions:   make(map[*proxySession]struct{}),
	}, nil
}

type sshTunnel struct {
	remoteSSHAddr    string
	remoteSSHUser    string
	localTargetAddr  string
	remoteListenAddr string
	signer           ssh.Signer
	retryDelay       time.Duration
	maxRetryDelay    time.Duration

	cancel context.CancelFunc
	done   chan struct{}

	mu             sync.Mutex
	listener       net.Listener
	activeSessions map[*proxySession]struct{}
}

//nolint:contextcheck // Remote forwarding must outlive the startup timeout context passed by the caller.
func (f *sshTunnel) Start(_ context.Context) {
	if f == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.done = make(chan struct{})

	go func() {
		defer close(f.done)
		f.run(ctx)
	}()
}

func (f *sshTunnel) Shutdown(ctx context.Context) error {
	if f == nil {
		return nil
	}

	if f.cancel != nil {
		f.cancel()
	}

	if err := f.closeCurrentListener(); err != nil {
		return err
	}
	f.closeActiveSessions()

	if f.done == nil {
		return nil
	}

	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *sshTunnel) run(ctx context.Context) {
	baseDelay := retryDelay(f.retryDelay)
	delay := baseDelay
	maxDelay := max(f.maxRetryDelay, delay)

	for {
		if ctx.Err() != nil {
			return
		}

		listener, err := f.openListener()
		if err != nil {
			log.Printf("remote ssh listener unavailable: %v", err)
			if !waitWithContext(ctx, delay) {
				return
			}
			delay = nextRetryDelay(delay, maxDelay)
			continue
		}

		delay = baseDelay
		log.Printf("remote ssh listener ready: %s over %s", f.remoteListenAddr, f.remoteSSHAddr)

		err = f.serveProxy(ctx, listener)
		if closeErr := ignoreNetClosed(listener.Close()); closeErr != nil {
			log.Printf("close remote ssh listener failed: %v", closeErr)
		}
		f.clearCurrentListener(listener)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("remote ssh listener stopped: %v", err)
		} else {
			log.Printf("remote ssh listener stopped, reconnecting")
		}
		if !waitWithContext(ctx, delay) {
			return
		}
	}
}

func (f *sshTunnel) openListener() (net.Listener, error) {
	sshConfig := &ssh.ClientConfig{
		User: f.remoteSSHUser,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(f.signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // Personal project: host key verification is intentionally skipped.
	}

	client, err := ssh.Dial("tcp", f.remoteSSHAddr, sshConfig)
	if err != nil {
		return nil, err
	}

	// Check sshd config: AllowTcpForwarding=yes, GatewayPorts=clientspecified.
	ln, err := client.Listen("tcp", f.remoteListenAddr)
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	listener := &sshRemoteListener{
		Listener: ln,
		client:   client,
	}

	f.mu.Lock()
	f.listener = listener
	f.mu.Unlock()

	return listener, nil
}

func (f *sshTunnel) closeCurrentListener() error {
	f.mu.Lock()
	listener := f.listener
	f.listener = nil
	f.mu.Unlock()

	if listener == nil {
		return nil
	}

	return ignoreNetClosed(listener.Close())
}

func (f *sshTunnel) clearCurrentListener(listener net.Listener) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.listener == listener {
		f.listener = nil
	}
}

func (f *sshTunnel) registerSession(session *proxySession) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.activeSessions[session] = struct{}{}
}

func (f *sshTunnel) unregisterSession(session *proxySession) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.activeSessions, session)
}

func (f *sshTunnel) closeActiveSessions() {
	f.mu.Lock()
	sessions := make([]*proxySession, 0, len(f.activeSessions))
	for session := range f.activeSessions {
		sessions = append(sessions, session)
	}
	f.mu.Unlock()

	for _, session := range sessions {
		session.close()
	}
}

func (f *sshTunnel) serveProxy(ctx context.Context, listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return classifyAcceptErr(ctx, err)
		}

		go f.proxyConn(ctx, conn)
	}
}

func (f *sshTunnel) proxyConn(ctx context.Context, remoteConn net.Conn) {
	localConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", f.localTargetAddr)
	if err != nil {
		log.Printf("ssh tunnel proxy dial %s failed: %v", f.localTargetAddr, err)
		_ = remoteConn.Close()
		return
	}

	session := &proxySession{
		localConn:  localConn,
		remoteConn: remoteConn,
		done:       make(chan struct{}),
	}
	f.registerSession(session)
	defer f.unregisterSession(session)
	defer close(session.done)
	defer session.close()

	go func() {
		select {
		case <-ctx.Done():
			session.close()
		case <-session.done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		copyConn(localConn, remoteConn)
	}()

	go func() {
		defer wg.Done()
		copyConn(remoteConn, localConn)
	}()

	wg.Wait()
}

func copyConn(dst net.Conn, src net.Conn) {
	_, err := io.Copy(dst, src)
	if shouldLogProxyCopyErr(err) {
		log.Printf("ssh tunnel proxy copy failed: %v", err)
	}
	closeConnWrite(dst)
}

func shouldLogProxyCopyErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return false
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return false
	}

	return true
}

func closeConnWrite(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}

	writer, ok := conn.(closeWriter)
	if ok {
		err := writer.CloseWrite()
		if shouldLogProxyCopyErr(err) {
			log.Printf("ssh tunnel proxy close write failed: %v", err)
		}
		return
	}

	err := conn.Close()
	if shouldLogProxyCopyErr(err) {
		log.Printf("ssh tunnel proxy close failed: %v", err)
	}
}

func deriveRemoteListenAddr(localTargetAddr string) (string, error) {
	_, port, err := net.SplitHostPort(localTargetAddr)
	if err != nil {
		return "", fmt.Errorf("split local target addr %q failed: %w", localTargetAddr, err)
	}

	return ":" + port, nil
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

func ignoreNetClosed(err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}

func classifyAcceptErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}

type proxySession struct {
	localConn  net.Conn
	remoteConn net.Conn
	done       chan struct{}
	once       sync.Once
}

func (s *proxySession) close() {
	s.once.Do(func() {
		if s.localConn != nil {
			err := s.localConn.Close()
			if shouldLogProxyCopyErr(err) {
				log.Printf("ssh tunnel proxy session local close failed: %v", err)
			}
		}
		if s.remoteConn != nil {
			err := s.remoteConn.Close()
			if shouldLogProxyCopyErr(err) {
				log.Printf("ssh tunnel proxy session remote close failed: %v", err)
			}
		}
	})
}

func loadPrivateKey(path string) (ssh.Signer, error) {
	//nolint:gosec // SSH private key path comes from trusted local configuration.
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(key)
}
