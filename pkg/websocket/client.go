package websocket

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ClientConfig holds configuration for the WebSocket client
type ClientConfig struct {
	URL                string
	APIToken           string
	ReconnectInterval  time.Duration
	MaxReconnectDelay  time.Duration
	PingInterval       time.Duration
	PongTimeout        time.Duration
	WriteTimeout       time.Duration
	ReadTimeout        time.Duration
}

// DefaultClientConfig returns a default configuration
func DefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		ReconnectInterval: 5 * time.Second,
		MaxReconnectDelay: 300 * time.Second,
		PingInterval:      30 * time.Second,
		PongTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		ReadTimeout:       60 * time.Second,
	}
}

// Client represents a WebSocket client for communicating with the backend
type Client struct {
	config     *ClientConfig
	conn       *websocket.Conn
	connMutex  sync.RWMutex
	logger     *slog.Logger

	// Channels for communication
	commandCh   chan *Message
	responseCh  chan *Message
	eventCh     chan *Message

	// Control channels
	reconnectCh chan struct{}
	closeCh     chan struct{}

	// Connection state
	connected   bool
	connectedAt time.Time

	// Context for graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc

	// Wait group for goroutines
	wg sync.WaitGroup
}

// NewClient creates a new WebSocket client
func NewClient(config *ClientConfig, logger *slog.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())

	return &Client{
		config:      config,
		logger:      logger,
		commandCh:   make(chan *Message, 100),
		responseCh:  make(chan *Message, 100),
		eventCh:     make(chan *Message, 100),
		reconnectCh: make(chan struct{}, 1),
		closeCh:     make(chan struct{}),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Start begins the WebSocket connection and starts background goroutines
func (c *Client) Start() error {
	c.logger.Info("Starting WebSocket client", "url", c.config.URL)

	// Start the connection manager
	c.wg.Add(1)
	go c.connectionManager()

	// Start the ping routine
	c.wg.Add(1)
	go c.pingRoutine()

	return nil
}

// Stop gracefully shuts down the WebSocket client
func (c *Client) Stop() error {
	c.logger.Info("Stopping WebSocket client")

	c.cancel()
	close(c.closeCh)

	// Close connection if open
	c.connMutex.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMutex.Unlock()

	// Wait for all goroutines to finish
	c.wg.Wait()

	c.logger.Info("WebSocket client stopped")
	return nil
}

// SendResponse sends a response message to the backend
func (c *Client) SendResponse(response *Response) error {
	msg := NewMessage(response.CommandID, ResponseResult, response)
	return c.sendMessage(msg)
}

// SendEvent sends an event message to the backend
func (c *Client) SendEvent(event *Event) error {
	msg := NewMessage("", event.Type, event)
	return c.sendMessage(msg)
}

// Commands returns a channel for receiving command messages
func (c *Client) Commands() <-chan *Message {
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
				// Exponential backoff with jitter
				reconnectDelay = min(reconnectDelay*2, c.config.MaxReconnectDelay)
			}
			continue
		}

		// Reset reconnect delay on successful connection
		reconnectDelay = c.config.ReconnectInterval

		// Handle messages until connection fails
		c.messageLoop()

		c.logger.Warn("Connection lost, attempting to reconnect")
	}
}

// connect establishes a WebSocket connection to the backend
func (c *Client) connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
	}

	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+c.config.APIToken)

	conn, _, err := dialer.Dial(c.config.URL, headers)
	if err != nil {
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	c.connMutex.Lock()
	c.conn = conn
	c.connected = true
	c.connectedAt = time.Now()
	c.connMutex.Unlock()

	c.logger.Info("WebSocket connection established")

	return nil
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

	// Start reader goroutine
	readerDone := make(chan struct{})
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(readerDone)
		c.messageReader()
	}()

	// Handle outgoing messages and connection events
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.closeCh:
			return
		case <-readerDone:
			return
		case msg := <-c.responseCh:
			if err := c.writeMessage(msg); err != nil {
				c.logger.Error("Failed to send response", "error", err)
				return
			}
		case msg := <-c.eventCh:
			if err := c.writeMessage(msg); err != nil {
				c.logger.Error("Failed to send event", "error", err)
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

		// Set read deadline
		conn.SetReadDeadline(time.Now().Add(c.config.ReadTimeout))

		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Error("WebSocket read error", "error", err)
			}
			return
		}

		msg, err := FromJSON(data)
		if err != nil {
			c.logger.Error("Failed to parse message", "error", err)
			continue
		}

		c.handleIncomingMessage(msg)
	}
}

// handleIncomingMessage routes incoming messages to appropriate channels
func (c *Client) handleIncomingMessage(msg *Message) {
	switch msg.Type {
	case CommandDeploy, CommandTerminate, CommandStatus:
		select {
		case c.commandCh <- msg:
		default:
			c.logger.Warn("Command channel full, dropping message", "type", msg.Type, "id", msg.ID)
		}
	case CommandPing:
		// Respond to ping immediately
		pong := NewMessage(msg.ID, EventPong, nil)
		if err := c.sendMessage(pong); err != nil {
			c.logger.Error("Failed to send pong", "error", err)
		}
	default:
		c.logger.Warn("Unknown message type received", "type", msg.Type)
	}
}

// sendMessage queues a message for sending
func (c *Client) sendMessage(msg *Message) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected")
	}

	switch msg.Type {
	case ResponseResult, ResponseError:
		select {
		case c.responseCh <- msg:
			return nil
		default:
			return fmt.Errorf("response channel full")
		}
	default:
		select {
		case c.eventCh <- msg:
			return nil
		default:
			return fmt.Errorf("event channel full")
		}
	}
}

// writeMessage writes a message to the WebSocket connection
func (c *Client) writeMessage(msg *Message) error {
	c.connMutex.RLock()
	conn := c.conn
	c.connMutex.RUnlock()

	if conn == nil {
		return fmt.Errorf("connection not available")
	}

	data, err := msg.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}

	conn.SetWriteDeadline(time.Now().Add(c.config.WriteTimeout))
	err = conn.WriteMessage(websocket.TextMessage, data)
	if err != nil {
		return fmt.Errorf("failed to write message: %w", err)
	}

	return nil
}

// pingRoutine sends periodic ping messages to keep the connection alive
func (c *Client) pingRoutine() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.closeCh:
			return
		case <-ticker.C:
			if c.IsConnected() {
				ping := NewMessage("ping", CommandPing, nil)
				if err := c.sendMessage(ping); err != nil {
					c.logger.Error("Failed to send ping", "error", err)
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