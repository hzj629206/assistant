package seatalkoapisdk

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

type Logger interface {
	Printf(format string, args ...interface{})
}

type ClientOption func(*Client)

func WithWebSocketURL(wsURL string) ClientOption {
	return func(c *Client) {
		if wsURL != "" {
			c.wsURL = wsURL
		}
	}
}

func WithDialer(dialer *websocket.Dialer) ClientOption {
	return func(c *Client) {
		if dialer != nil {
			c.dialer = *dialer
		}
	}
}

func WithRequestHeader(header http.Header) ClientOption {
	return func(c *Client) {
		if header != nil {
			c.requestHeader = header.Clone()
		}
	}
}

func WithHandshakeTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if timeout > 0 {
			c.dialer.HandshakeTimeout = timeout
		}
	}
}

func WithWriteTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if timeout > 0 {
			c.writeTimeout = timeout
		}
	}
}

func WithPingInterval(interval time.Duration) ClientOption {
	return func(c *Client) {
		c.pingInterval = interval
		c.activePingInterval = interval
	}
}

func WithReadLimit(limit int64) ClientOption {
	return func(c *Client) {
		if limit > 0 {
			c.readLimit = limit
		}
	}
}

func WithEventDispatcher(dispatcher *EventDispatcher) ClientOption {
	return func(c *Client) {
		c.dispatcher = dispatcher
	}
}

func WithLogger(logger Logger) ClientOption {
	return func(c *Client) {
		c.logger = logger
	}
}
