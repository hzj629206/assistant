package seatalkoapisdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	ErrMissingCredential = errors.New("seatalk oapi sdk: app_id and app_secret are required")
	ErrAlreadyConnected  = errors.New("seatalk oapi sdk: already connected")
	ErrNotConnected      = errors.New("seatalk oapi sdk: not connected")
	ErrNotRegistered     = errors.New("seatalk oapi sdk: not registered")
	ErrMissingCallbackID = errors.New("seatalk oapi sdk: callback_id is required")
)

type Client struct {
	appID     string
	appSecret string

	wsURL              string
	dialer             websocket.Dialer
	requestHeader      http.Header
	writeTimeout       time.Duration
	pingInterval       time.Duration
	activePingInterval time.Duration
	readLimit          int64
	dispatcher         *EventDispatcher
	logger             Logger

	connMu  sync.RWMutex
	conn    *websocket.Conn
	token   string
	writeMu sync.Mutex
}

func NewClient(appID, appSecret string, opts ...ClientOption) *Client {
	c := &Client{
		appID: appID, appSecret: appSecret,
		wsURL:         DefaultWebSocketURL,
		requestHeader: http.Header{},
		dialer: websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
		},
		writeTimeout:       10 * time.Second,
		pingInterval:       15 * time.Second,
		activePingInterval: 15 * time.Second,
		readLimit:          1024 * 1024,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

func (c *Client) Connect(ctx context.Context) (*RegisterResult, error) {
	if c.appID == "" || c.appSecret == "" {
		return nil, ErrMissingCredential
	}

	c.connMu.Lock()
	if c.conn != nil {
		c.connMu.Unlock()
		return nil, ErrAlreadyConnected
	}
	c.connMu.Unlock()

	conn, resp, err := c.dialer.DialContext(ctx, c.wsURL, c.requestHeader.Clone())
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial websocket: %w (http %s)", err, resp.Status)
		}
		return nil, fmt.Errorf("dial websocket: %w", err)
	}
	conn.SetReadLimit(c.readLimit)

	c.connMu.Lock()
	if c.conn != nil {
		c.connMu.Unlock()
		_ = conn.Close()
		return nil, ErrAlreadyConnected
	}
	c.conn = conn
	c.token = ""
	c.connMu.Unlock()

	if err := c.Send(ctx, Envelope{
		Cmd: CommandRegister,
		Header: Header{
			AppID:     c.appID,
			AppSecret: c.appSecret,
		},
	}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("write register: %w", err)
	}

	result, err := c.readRegisterOK(conn)
	if err != nil {
		_ = c.Close()
		return nil, err
	}

	c.connMu.Lock()
	c.token = result.Token
	c.activePingInterval = c.pingInterval
	if result.HeartbeatInterval > 0 {
		c.activePingInterval = result.HeartbeatInterval
	}
	c.connMu.Unlock()
	return result, nil
}

func (c *Client) Run(ctx context.Context) error {
	if _, err := c.Connect(ctx); err != nil {
		return err
	}
	defer c.Close()
	return c.Start(ctx)
}

func (c *Client) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)
	go func() {
		errCh <- c.Listen(runCtx)
	}()
	pingInterval := c.currentPingInterval()
	if pingInterval > 0 {
		go func() {
			errCh <- c.pingLoop(runCtx, pingInterval)
		}()
	}

	select {
	case err := <-errCh:
		cancel()
		_ = c.Close()
		return normalizeListenError(ctx, err)
	case <-ctx.Done():
		cancel()
		_ = c.Close()
		return normalizeListenError(ctx, <-errCh)
	}
}

func (c *Client) Listen(ctx context.Context) error {
	conn := c.currentConn()
	if conn == nil {
		return ErrNotConnected
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-done:
		}
	}()

	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return normalizeListenError(ctx, err)
		}
		if messageType != websocket.TextMessage {
			continue
		}

		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			if handlerErr := c.dispatcher.handleInvalidFrame(ctx, data, fmt.Errorf("invalid json frame: %w", err)); handlerErr != nil {
				return handlerErr
			}
			continue
		}
		if err := c.dispatcher.dispatch(ctx, c, &env); err != nil {
			return err
		}
	}
}

func (c *Client) Send(ctx context.Context, env Envelope) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	conn := c.currentConn()
	if conn == nil {
		return ErrNotConnected
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.writeTimeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}
	ensureEnvelopeRid(&env)
	if err := conn.WriteJSON(&env); err != nil {
		return err
	}
	c.logf("sent %s command rid=%s", env.Cmd, env.Header.Rid)
	return nil
}

func (c *Client) Ack(ctx context.Context, callbackID string) error {
	if callbackID == "" {
		return ErrMissingCallbackID
	}
	token := c.Token()
	if token == "" {
		return ErrNotRegistered
	}
	return c.Send(ctx, Envelope{
		Cmd: CommandAck,
		Header: Header{
			Token:      token,
			CallbackID: callbackID,
		},
	})
}

func (c *Client) Ping(ctx context.Context) error {
	token := c.Token()
	if token == "" {
		return ErrNotRegistered
	}
	return c.Send(ctx, Envelope{
		Cmd:    CommandPing,
		Header: Header{Token: token},
	})
}

func (c *Client) pingLoop(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.Ping(ctx); err != nil {
				return err
			}
		}
	}
}

func (c *Client) Token() string {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.token
}

func (c *Client) Close() error {
	c.connMu.Lock()
	conn := c.conn
	c.conn = nil
	c.token = ""
	c.activePingInterval = c.pingInterval
	c.connMu.Unlock()
	if conn == nil {
		return nil
	}

	c.writeMu.Lock()
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(c.writeTimeout),
	)
	c.writeMu.Unlock()
	return conn.Close()
}

func (c *Client) readRegisterOK(conn *websocket.Conn) (*RegisterResult, error) {
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		if messageType != websocket.TextMessage {
			continue
		}

		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, fmt.Errorf("invalid json in register phase: %w", err)
		}
		if env.Cmd != CommandRegister {
			return nil, fmt.Errorf("expected cmd %q first, got %q (%s)", CommandRegister, env.Cmd, env.Message)
		}
		if env.Code != CodeOK {
			return nil, &RegisterError{Code: env.Code, Message: env.Message}
		}
		if env.Header.Token == "" {
			return nil, errors.New("register ok but empty token")
		}
		settings, err := readRegisterSettings(env.Data)
		if err != nil {
			return nil, fmt.Errorf("invalid register settings: %w", err)
		}
		return &RegisterResult{
			AppID:             env.Header.AppID,
			Token:             env.Header.Token,
			Sid:               env.Header.Sid,
			HeartbeatInterval: time.Duration(settings.HeartbeatInterval) * time.Second,
			HeartbeatTimeout:  time.Duration(settings.HeartbeatTimeout) * time.Second,
		}, nil
	}
}

func readRegisterSettings(data json.RawMessage) (RegisterSettings, error) {
	if len(data) == 0 {
		return RegisterSettings{}, nil
	}
	var settings RegisterSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return RegisterSettings{}, err
	}
	return settings, nil
}

func (c *Client) currentPingInterval() time.Duration {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.activePingInterval
}

func (c *Client) currentConn() *websocket.Conn {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.conn
}

func (c *Client) logf(format string, args ...interface{}) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

func ensureEnvelopeRid(env *Envelope) {
	if env.Header.Rid == "" {
		env.Header.Rid = newRid()
	}
}

func newRid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func normalizeListenError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return nil
	}
	if errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "use of closed network connection") {
		return nil
	}
	return err
}
