package websocket

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ClientConfig holds configuration for the WebSocket client
type ClientConfig struct {
	URL      string
	APIToken string

	// KubeletID is sent in every envelope and in the heartbeat. The service
	// identifies a kubelet by its API token, so this must equal APIToken.
	KubeletID string

	ReconnectInterval time.Duration
	MaxReconnectDelay time.Duration
	HeartbeatInterval time.Duration
	PongTimeout       time.Duration
	WriteTimeout      time.Duration
	ReadTimeout       time.Duration
}

// DefaultClientConfig returns a default configuration
func DefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		ReconnectInterval: 5 * time.Second,
		MaxReconnectDelay: 300 * time.Second,
		HeartbeatInterval: 30 * time.Second,
		PongTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		ReadTimeout:       60 * time.Second,
	}
}

// Client represents a WebSocket client for communicating with the backend
type Client struct {
	config    *ClientConfig
	conn      *websocket.Conn
	connMutex sync.RWMutex
	logger    *slog.Logger

	// Channels for communication
	commandCh  chan *Envelope
	outboundCh chan *Envelope

	// Control channels
	closeCh chan struct{}

	// Connection state
	connected   bool
	connectedAt time.Time

	// Called (in its own goroutine) after every successful (re)connect,
	// used by the kubelet to register itself.
	onConnect      func()
	onConnectMutex sync.RWMutex

	// Context for graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc

	// Wait group for goroutines
	wg sync.WaitGroup
}

// NewClient creates a new WebSocket client
func NewClient(config *ClientConfig, logger *slog.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())

	// Zero timeouts would make every write fail immediately, so fill unset
	// fields from the defaults.
	defaults := DefaultClientConfig()
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = defaults.HeartbeatInterval
	}
	if config.ReconnectInterval <= 0 {
		config.ReconnectInterval = defaults.ReconnectInterval
	}
	if config.MaxReconnectDelay <= 0 {
		config.MaxReconnectDelay = defaults.MaxReconnectDelay
	}
	if config.PongTimeout <= 0 {
		config.PongTimeout = defaults.PongTimeout
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = defaults.WriteTimeout
	}
	if config.ReadTimeout <= 0 {
		config.ReadTimeout = defaults.ReadTimeout
	}
	if config.KubeletID == "" {
		config.KubeletID = config.APIToken
	}

	return &Client{
		config:     config,
		logger:     logger,
		commandCh:  make(chan *Envelope, 100),
		outboundCh: make(chan *Envelope, 200),
		closeCh:    make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// SetOnConnect registers a callback invoked after each successful connect.
func (c *Client) SetOnConnect(fn func()) {
	c.onConnectMutex.Lock()
	c.onConnect = fn
	c.onConnectMutex.Unlock()
}

// KubeletID returns the id this client stamps on outgoing envelopes.
func (c *Client) KubeletID() string {
	return c.config.KubeletID
}

// Start begins the WebSocket connection and starts background goroutines
func (c *Client) Start() error {
	c.logger.Info("Starting WebSocket client", "url", redactURL(c.config.URL))

	c.wg.Add(1)
	go c.connectionManager()

	c.wg.Add(1)
	go c.heartbeatRoutine()

	return nil
}

// Stop gracefully shuts down the WebSocket client
func (c *Client) Stop() error {
	c.logger.Info("Stopping WebSocket client")

	c.cancel()
	close(c.closeCh)

	c.connMutex.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMutex.Unlock()

	c.wg.Wait()

	c.logger.Info("WebSocket client stopped")
	return nil
}

// SendResponse sends a command response to the backend
func (c *Client) SendResponse(response *Response) error {
	env, err := NewEnvelope(EnvelopeResponse, response, c.config.KubeletID)
	if err != nil {
		return err
	}
	return c.sendEnvelope(env)
}

// SendEvent sends an event to the backend
func (c *Client) SendEvent(event *Event) error {
	env, err := NewEnvelope(EnvelopeEvent, event, c.config.KubeletID)
	if err != nil {
		return err
	}
	return c.sendEnvelope(env)
}

// SendRegistration sends the kubelet registration to the backend
func (c *Client) SendRegistration(reg *Registration) error {
	env, err := NewEnvelope(EnvelopeRegistration, reg, c.config.KubeletID)
	if err != nil {
		return err
	}
	return c.sendEnvelope(env)
}

// SendHeartbeat sends a heartbeat to the backend
func (c *Client) SendHeartbeat() error {
	env, err := NewEnvelope(EnvelopeHeartbeat, NewHeartbeat(c.config.KubeletID), c.config.KubeletID)
	if err != nil {
		return err
	}
	return c.sendEnvelope(env)
}

// Commands returns a channel delivering "command" envelopes from the backend
func (c *Client) Commands() <-chan *Envelope {
	return c.commandCh
}

// IsConnected returns whether the client is currently connected
func (c *Client) IsConnected() bool {
	c.connMutex.RLock()
	defer c.connMutex.RUnlock()
	return c.connected
}

// connectionManager handles the WebSocket connection lifecycle
func (c *Client) connectionManager() {
	defer c.wg.Done()

	reconnectDelay := c.config.ReconnectInterval

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.closeCh:
			return
		default:
		}

		err := c.connect()
		if err != nil {
			c.logger.Error("Failed to connect", "error", err, "retry_delay", reconnectDelay)

			select {
			case <-c.ctx.Done():
				return
			case <-time.After(reconnectDelay):
				reconnectDelay = min(reconnectDelay*2, c.config.MaxReconnectDelay)
			}
			continue
		}

		reconnectDelay = c.config.ReconnectInterval

		c.onConnectMutex.RLock()
		onConnect := c.onConnect
		c.onConnectMutex.RUnlock()
		if onConnect != nil {
			go onConnect()
		}

		c.messageLoop()

		select {
		case <-c.ctx.Done():
			return
		default:
			c.logger.Warn("Connection lost, attempting to reconnect")
		}
	}
}

// connect establishes a WebSocket connection to the backend.
// The token is sent both as the api_key query parameter and as a bearer
// header; the service accepts either.
func (c *Client) connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
	}

	dialURL, err := buildDialURL(c.config.URL, c.config.APIToken)
	if err != nil {
		return err
	}

	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+c.config.APIToken)

	conn, _, err := dialer.Dial(dialURL, headers)
	if err != nil {
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	readTimeout := c.config.ReadTimeout
	conn.SetPongHandler(func(string) error {
		if readTimeout > 0 {
			return conn.SetReadDeadline(time.Now().Add(readTimeout))
		}
		return nil
	})

	c.connMutex.Lock()
	c.conn = conn
	c.connected = true
	c.connectedAt = time.Now()
	c.connMutex.Unlock()

	c.logger.Info("WebSocket connection established")

	return nil
}

// buildDialURL appends the api_key query parameter unless one is present.
func buildDialURL(rawURL, token string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid backend URL: %w", err)
	}
	q := u.Query()
	if q.Get("api_key") == "" && q.Get("token") == "" {
		q.Set("api_key", token)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// redactURL strips credentials from a URL for logging.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	for _, k := range []string{"api_key", "token"} {
		if q.Get(k) != "" {
			q.Set(k, "***")
		}
	}
	u.RawQuery = q.Encode()
	return u.Redacted()
}

// messageLoop handles incoming and outgoing messages
func (c *Client) messageLoop() {
	defer func() {
		c.connMutex.Lock()
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.connected = false
		c.connMutex.Unlock()
	}()

	readerDone := make(chan struct{})
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(readerDone)
		c.messageReader()
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.closeCh:
			return
		case <-readerDone:
			return
		case env := <-c.outboundCh:
			if err := c.writeEnvelope(env); err != nil {
				c.logger.Error("Failed to send message", "type", env.Type, "error", err)
				return
			}
		}
	}
}

// messageReader reads messages from the WebSocket connection
func (c *Client) messageReader() {
	c.connMutex.RLock()
	conn := c.conn
	c.connMutex.RUnlock()

	if conn == nil {
		return
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.closeCh:
			return
		default:
		}

		if c.config.ReadTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(c.config.ReadTimeout))
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Error("WebSocket read error", "error", err)
			}
			return
		}

		env, err := ParseEnvelope(data)
		if err != nil {
			c.logger.Error("Failed to parse message", "error", err)
			continue
		}

		c.handleIncomingMessage(env)
	}
}

// handleIncomingMessage routes incoming envelopes to the appropriate channel
func (c *Client) handleIncomingMessage(env *Envelope) {
	switch env.Type {
	case EnvelopeCommand:
		c.logger.Debug("Received command", "id", env.ID)
		select {
		case c.commandCh <- env:
		default:
			c.logger.Warn("Command channel full, dropping command", "id", env.ID)
		}
	case EnvelopeHeartbeat:
		c.logger.Debug("Received heartbeat from backend")
	default:
		c.logger.Warn("Unexpected message type received", "type", env.Type, "id", env.ID)
	}
}

// sendEnvelope queues an envelope for sending
func (c *Client) sendEnvelope(env *Envelope) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected")
	}

	select {
	case c.outboundCh <- env:
		return nil
	default:
		return fmt.Errorf("outbound channel full")
	}
}

// writeEnvelope writes an envelope to the WebSocket connection
func (c *Client) writeEnvelope(env *Envelope) error {
	c.connMutex.RLock()
	conn := c.conn
	c.connMutex.RUnlock()

	if conn == nil {
		return fmt.Errorf("connection not available")
	}

	data, err := env.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}

	c.logger.Debug("Sending message", "type", env.Type, "id", env.ID, "bytes", len(data))

	conn.SetWriteDeadline(time.Now().Add(c.config.WriteTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("failed to write message: %w", err)
	}

	// Piggyback a protocol-level ping on each heartbeat so the read deadline
	// is refreshed by the pong even when the backend has nothing to send.
	if env.Type == EnvelopeHeartbeat {
		deadline := time.Now().Add(c.config.WriteTimeout)
		if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
			c.logger.Debug("Failed to send ping frame", "error", err)
		}
	}

	return nil
}

// heartbeatRoutine sends a heartbeat envelope every HeartbeatInterval
func (c *Client) heartbeatRoutine() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.closeCh:
			return
		case <-ticker.C:
			if c.IsConnected() {
				if err := c.SendHeartbeat(); err != nil {
					c.logger.Error("Failed to send heartbeat", "error", err)
				}
			}
		}
	}
}

// Helper function for min (Go 1.21+ has this built-in)
func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
