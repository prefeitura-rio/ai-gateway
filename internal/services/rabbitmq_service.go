package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/sirupsen/logrus"

	"github.com/prefeitura-rio/app-eai-agent-gateway/internal/config"
)

// CircuitState represents the state of the circuit breaker
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // Normal operation
	CircuitOpen                         // Failing, reject requests immediately
	CircuitHalfOpen                     // Testing if service recovered
)

// PooledChannel represents a channel in the pool with its own mutex
type PooledChannel struct {
	channel *amqp.Channel
	mutex   sync.Mutex
}

// ChannelPool manages a pool of AMQP channels for concurrent publishing
type ChannelPool struct {
	channels   []*PooledChannel
	connection *amqp.Connection
	poolSize   int
	roundRobin uint64
	mutex      sync.Mutex
	logger     *logrus.Logger
	config     *config.Config
}

// RabbitMQService handles RabbitMQ operations with connection management
type RabbitMQService struct {
	config      *config.Config
	logger      *logrus.Logger
	connection  *amqp.Connection
	channel     *amqp.Channel
	mutex       sync.Mutex // Changed from RWMutex to Mutex - AMQP channels are NOT thread-safe
	isConnected bool

	// Channel pool for concurrent publishing
	channelPool *ChannelPool

	// Connection monitoring
	notifyConnClose chan *amqp.Error
	notifyChanClose chan *amqp.Error
	notifyReconnect chan bool
	isShutdown      bool

	// Circuit breaker state
	circuitState     CircuitState
	circuitMutex     sync.RWMutex
	failureCount     int
	lastFailureTime  time.Time
	circuitOpenUntil time.Time

	// Circuit breaker configuration
	failureThreshold int           // Number of failures before opening circuit
	resetTimeout     time.Duration // Time to wait before trying again (half-open)
}

// MessagePublisher defines the interface for publishing messages
type MessagePublisher interface {
	PublishMessage(ctx context.Context, queueName string, message interface{}) error
	PublishMessageWithDelay(ctx context.Context, queueName string, message interface{}, delay time.Duration) error
	PublishPriorityMessage(ctx context.Context, queueName string, message interface{}, priority uint8) error
}

// MessageConsumer defines the interface for consuming messages
type MessageConsumer interface {
	ConsumeMessages(ctx context.Context, queueName string, handler MessageHandler) error
	StartConsumer(ctx context.Context, queueName string, concurrency int, handler MessageHandler) error
	StopConsumer(queueName string) error
}

// MessageHandler is a function type for handling consumed messages
type MessageHandler func(ctx context.Context, delivery amqp.Delivery) error

// QueueManager defines the interface for queue management operations
type QueueManager interface {
	DeclareQueue(queueName string, durable, autoDelete bool) error
	DeclareExchange(exchangeName, exchangeType string, durable bool) error
	BindQueue(queueName, exchangeName, routingKey string) error
	PurgeQueue(queueName string) (int, error)
	GetQueueInfo(queueName string) (amqp.Queue, error)
}

// NewRabbitMQService creates a new RabbitMQ service with connection management
func NewRabbitMQService(cfg *config.Config, logger *logrus.Logger) (*RabbitMQService, error) {
	service := &RabbitMQService{
		config:          cfg,
		logger:          logger,
		notifyReconnect: make(chan bool),

		// Circuit breaker defaults
		circuitState:     CircuitClosed,
		failureThreshold: 5,
		resetTimeout:     30 * time.Second,
	}

	if err := service.connect(); err != nil {
		return nil, fmt.Errorf("failed to establish initial RabbitMQ connection: %w", err)
	}

	// Setup auto-reconnection
	go service.handleReconnect()

	logger.WithField("url", cfg.RabbitMQ.URL).Info("RabbitMQ service initialized successfully")
	return service, nil
}

// checkCircuitBreaker checks if the circuit breaker allows the operation
func (r *RabbitMQService) checkCircuitBreaker() error {
	r.circuitMutex.Lock()
	defer r.circuitMutex.Unlock()

	switch r.circuitState {
	case CircuitOpen:
		if time.Now().After(r.circuitOpenUntil) {
			// Timeout expired — transition to half-open to test recovery
			r.circuitState = CircuitHalfOpen
			r.logger.Info("Circuit breaker half-open — testing recovery")
			return nil
		}
		return fmt.Errorf("circuit breaker is open, rejecting request (retry after %v)", time.Until(r.circuitOpenUntil))
	case CircuitHalfOpen:
		// Allow requests through to test recovery
		return nil
	default:
		return nil
	}
}

// recordSuccess records a successful operation for the circuit breaker
func (r *RabbitMQService) recordSuccess() {
	r.circuitMutex.Lock()
	defer r.circuitMutex.Unlock()

	r.failureCount = 0
	if r.circuitState == CircuitHalfOpen {
		r.circuitState = CircuitClosed
		r.logger.Info("Circuit breaker closed - service recovered")
	}
}

// recordFailure records a failed operation for the circuit breaker
func (r *RabbitMQService) recordFailure() {
	r.circuitMutex.Lock()
	defer r.circuitMutex.Unlock()

	r.failureCount++
	r.lastFailureTime = time.Now()

	switch r.circuitState {
	case CircuitHalfOpen:
		// Recovery test failed — reopen circuit
		r.circuitState = CircuitOpen
		r.circuitOpenUntil = time.Now().Add(r.resetTimeout)
		r.logger.WithField("circuit_open_until", r.circuitOpenUntil).Warn("Circuit breaker reopened after failure in half-open state")
	case CircuitClosed:
		if r.failureCount >= r.failureThreshold {
			r.circuitState = CircuitOpen
			r.circuitOpenUntil = time.Now().Add(r.resetTimeout)
			r.logger.WithFields(logrus.Fields{
				"failure_count":      r.failureCount,
				"circuit_open_until": r.circuitOpenUntil,
			}).Warn("Circuit breaker opened due to repeated failures")
		}
	}
}

// resetCircuitBreaker resets the circuit breaker to closed state
func (r *RabbitMQService) resetCircuitBreaker() {
	r.circuitMutex.Lock()
	defer r.circuitMutex.Unlock()

	r.circuitState = CircuitClosed
	r.failureCount = 0
	r.logger.Info("Circuit breaker reset")
}

// GetCircuitState returns the current circuit breaker state (for health checks)
func (r *RabbitMQService) GetCircuitState() CircuitState {
	r.circuitMutex.RLock()
	defer r.circuitMutex.RUnlock()
	return r.circuitState
}

// IsCircuitOpen returns true if the circuit breaker is open
func (r *RabbitMQService) IsCircuitOpen() bool {
	r.circuitMutex.RLock()
	defer r.circuitMutex.RUnlock()
	return r.circuitState == CircuitOpen && time.Now().Before(r.circuitOpenUntil)
}

// NewChannelPool creates a new channel pool
func NewChannelPool(conn *amqp.Connection, poolSize int, cfg *config.Config, logger *logrus.Logger) (*ChannelPool, error) {
	if poolSize <= 0 {
		poolSize = 5 // Default pool size
	}

	pool := &ChannelPool{
		channels:   make([]*PooledChannel, poolSize),
		connection: conn,
		poolSize:   poolSize,
		logger:     logger,
		config:     cfg,
	}

	// Create channels for the pool
	for i := 0; i < poolSize; i++ {
		ch, err := conn.Channel()
		if err != nil {
			// Close any channels we've already created
			pool.Close()
			return nil, fmt.Errorf("failed to create channel %d for pool: %w", i, err)
		}

		// Set QoS for fair dispatch
		if err := ch.Qos(1, 0, false); err != nil {
			_ = ch.Close()
			pool.Close()
			return nil, fmt.Errorf("failed to set QoS for channel %d: %w", i, err)
		}

		pool.channels[i] = &PooledChannel{
			channel: ch,
		}
	}

	logger.WithField("pool_size", poolSize).Info("Channel pool created")
	return pool, nil
}

// AcquireChannel gets a channel from the pool using round-robin selection
func (p *ChannelPool) AcquireChannel() (*PooledChannel, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if len(p.channels) == 0 {
		return nil, fmt.Errorf("channel pool is empty")
	}

	// Round-robin selection
	idx := int(p.roundRobin % uint64(len(p.channels)))
	p.roundRobin++

	pooledCh := p.channels[idx]
	if pooledCh == nil || pooledCh.channel == nil {
		return nil, fmt.Errorf("channel at index %d is nil", idx)
	}

	return pooledCh, nil
}

// PublishWithPool publishes a message using a channel from the pool
func (p *ChannelPool) PublishWithPool(ctx context.Context, exchange, routingKey string, publishing amqp.Publishing) error {
	pooledCh, err := p.AcquireChannel()
	if err != nil {
		return fmt.Errorf("failed to acquire channel from pool: %w", err)
	}

	// Lock the specific channel for this publish operation
	pooledCh.mutex.Lock()
	defer pooledCh.mutex.Unlock()

	return pooledCh.channel.PublishWithContext(ctx, exchange, routingKey, false, false, publishing)
}

// Close closes all channels in the pool
func (p *ChannelPool) Close() {
	p.mutex.Lock()
	channels := p.channels
	p.channels = nil
	p.mutex.Unlock()

	for i, pooledCh := range channels {
		if pooledCh == nil {
			continue
		}
		// Lock the slot so any in-flight publish finishes before we close the channel
		pooledCh.mutex.Lock()
		ch := pooledCh.channel
		pooledCh.channel = nil
		pooledCh.mutex.Unlock()

		if ch != nil {
			if err := ch.Close(); err != nil {
				p.logger.WithError(err).WithField("channel_index", i).Warn("Failed to close pooled channel")
			}
		}
	}
}

// RecreateChannels recreates all channels in the pool (called after reconnection).
// Updates each PooledChannel in-place so in-flight publishers holding a pointer
// to the same struct will see the new channel after acquiring the per-slot mutex.
func (p *ChannelPool) RecreateChannels(conn *amqp.Connection) error {
	p.mutex.Lock()
	p.connection = conn
	channels := p.channels
	p.mutex.Unlock()

	for i, pooledCh := range channels {
		if pooledCh == nil {
			continue
		}

		ch, err := conn.Channel()
		if err != nil {
			return fmt.Errorf("failed to recreate channel %d: %w", i, err)
		}

		if err := ch.Qos(1, 0, false); err != nil {
			_ = ch.Close()
			return fmt.Errorf("failed to set QoS for channel %d: %w", i, err)
		}

		// Lock the slot so any in-flight publish finishes before we swap the channel
		pooledCh.mutex.Lock()
		old := pooledCh.channel
		pooledCh.channel = ch
		pooledCh.mutex.Unlock()

		if old != nil {
			_ = old.Close()
		}
	}

	p.logger.Info("Channel pool recreated after reconnection")
	return nil
}

// connect establishes connection to RabbitMQ and sets up exchanges and queues
func (r *RabbitMQService) connect() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Connect to RabbitMQ
	conn, err := amqp.Dial(r.config.RabbitMQ.URL)
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	// Create main channel (for topology setup and health checks)
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to open channel: %w", err)
	}

	// Set QoS for fair dispatch
	if err := ch.Qos(1, 0, false); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return fmt.Errorf("failed to set QoS: %w", err)
	}

	r.connection = conn
	r.channel = ch
	r.isConnected = true

	// Setup connection monitoring
	r.notifyConnClose = make(chan *amqp.Error)
	r.notifyChanClose = make(chan *amqp.Error)
	r.connection.NotifyClose(r.notifyConnClose)
	r.channel.NotifyClose(r.notifyChanClose)

	// Setup exchanges and queues
	if err := r.setupTopology(); err != nil {
		return fmt.Errorf("failed to setup RabbitMQ topology: %w", err)
	}

	// Create or recreate channel pool for concurrent publishing
	if r.channelPool == nil {
		pool, err := NewChannelPool(conn, 5, r.config, r.logger)
		if err != nil {
			r.logger.WithError(err).Warn("Failed to create channel pool, falling back to single channel")
			// Don't fail connection if pool creation fails - we can still use the main channel
		} else {
			r.channelPool = pool
		}
	} else {
		// Recreate channels in existing pool after reconnection
		if err := r.channelPool.RecreateChannels(conn); err != nil {
			r.logger.WithError(err).Warn("Failed to recreate channel pool, falling back to single channel")
		}
	}

	// Reset circuit breaker on successful connection
	r.resetCircuitBreaker()

	r.logger.Info("RabbitMQ connection established successfully")
	return nil
}

// setupTopology declares exchanges, queues, and bindings based on configuration
func (r *RabbitMQService) setupTopology() error {
	// Declare main exchange
	if err := r.channel.ExchangeDeclare(
		r.config.RabbitMQ.Exchange, // name
		"direct",                   // type
		true,                       // durable
		false,                      // auto-deleted
		false,                      // internal
		false,                      // no-wait
		nil,                        // arguments
	); err != nil {
		return fmt.Errorf("failed to declare exchange %s: %w", r.config.RabbitMQ.Exchange, err)
	}

	// Declare dead letter exchange
	if err := r.channel.ExchangeDeclare(
		r.config.RabbitMQ.DLXExchange, // name
		"direct",                      // type
		true,                          // durable
		false,                         // auto-deleted
		false,                         // internal
		false,                         // no-wait
		nil,                           // arguments
	); err != nil {
		return fmt.Errorf("failed to declare DLX exchange %s: %w", r.config.RabbitMQ.DLXExchange, err)
	}

	// Declare user messages queue
	if err := r.declareQueueWithDLX(r.config.RabbitMQ.UserQueue); err != nil {
		return fmt.Errorf("failed to declare user queue: %w", err)
	}

	// Declare user messages queue
	if err := r.declareQueueWithDLX(r.config.RabbitMQ.UserMessagesQueue); err != nil {
		return fmt.Errorf("failed to declare user messages queue: %w", err)
	}

	// Declare DANFE processing queue
	if err := r.declareQueueWithDLX("danfe_processing"); err != nil {
		return fmt.Errorf("failed to declare DANFE processing queue: %w", err)
	}

	// Declare dead letter queues
	if err := r.declareDLQ(r.config.RabbitMQ.UserQueue+"_dlq", nil); err != nil {
		return fmt.Errorf("failed to declare DLQ for %s: %w", r.config.RabbitMQ.UserQueue, err)
	}

	if err := r.declareDLQ(r.config.RabbitMQ.UserMessagesQueue+"_dlq", nil); err != nil {
		return fmt.Errorf("failed to declare DLQ for %s: %w", r.config.RabbitMQ.UserMessagesQueue, err)
	}

	// DANFE DLQ with 48 hours TTL
	danfeDLQArgs := amqp.Table{
		"x-message-ttl": int32(172800000), // 48 hours in milliseconds
	}
	if err := r.declareDLQ("danfe_processing_dlq", danfeDLQArgs); err != nil {
		return fmt.Errorf("failed to declare DLQ for danfe_processing: %w", err)
	}

	r.logger.Info("RabbitMQ topology setup completed")
	return nil
}

// declareQueueWithDLX declares a queue with dead letter exchange configuration
func (r *RabbitMQService) declareQueueWithDLX(queueName string) error {
	args := amqp.Table{
		"x-dead-letter-exchange":    r.config.RabbitMQ.DLXExchange,
		"x-dead-letter-routing-key": queueName + "_dlq",
		"x-message-ttl":             1200000, // 20 minutes TTL
	}

	_, err := r.channel.QueueDeclare(
		queueName, // name
		true,      // durable
		false,     // delete when unused
		false,     // exclusive
		false,     // no-wait
		args,      // arguments
	)
	if err != nil {
		return err
	}

	// Bind queue to exchange
	return r.channel.QueueBind(
		queueName,                  // queue name
		queueName,                  // routing key (same as queue name)
		r.config.RabbitMQ.Exchange, // exchange
		false,                      // no-wait
		nil,                        // arguments
	)
}

// declareDLQ declares a dead letter queue with optional arguments (e.g., TTL)
func (r *RabbitMQService) declareDLQ(queueName string, args amqp.Table) error {
	_, err := r.channel.QueueDeclare(
		queueName, // name
		true,      // durable
		false,     // delete when unused
		false,     // exclusive
		false,     // no-wait
		args,      // arguments (can include x-message-ttl, etc.)
	)
	if err != nil {
		return err
	}

	// Bind DLQ to DLX exchange
	return r.channel.QueueBind(
		queueName,                     // queue name
		queueName,                     // routing key
		r.config.RabbitMQ.DLXExchange, // exchange
		false,                         // no-wait
		nil,                           // arguments
	)
}

// PublishMessage publishes a message to the specified queue
func (r *RabbitMQService) PublishMessage(ctx context.Context, queueName string, message interface{}) error {
	// Check circuit breaker first (fast fail)
	if err := r.checkCircuitBreaker(); err != nil {
		return err
	}

	// Serialize message to JSON
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	publishing := amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent, // Make message persistent
		Timestamp:    time.Now(),
		MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
	}

	// Try to use channel pool for better concurrency
	if r.channelPool != nil {
		err = r.channelPool.PublishWithPool(ctx, r.config.RabbitMQ.Exchange, queueName, publishing)
		if err != nil {
			r.recordFailure()
			r.logger.WithError(err).WithFields(logrus.Fields{
				"queue":   queueName,
				"message": string(body),
			}).Error("Failed to publish message via pool")
			return fmt.Errorf("failed to publish message: %w", err)
		}
		r.recordSuccess()
		r.logger.WithFields(logrus.Fields{
			"queue":      queueName,
			"message_id": publishing.MessageId,
			"via":        "pool",
		}).Debug("Message published successfully")
		return nil
	}

	// Fallback to single channel if pool is not available
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if !r.isConnected {
		r.recordFailure()
		return fmt.Errorf("RabbitMQ connection is not available")
	}

	// Publish message
	err = r.channel.PublishWithContext(
		ctx,
		r.config.RabbitMQ.Exchange, // exchange
		queueName,                  // routing key
		false,                      // mandatory
		false,                      // immediate
		publishing,
	)

	if err != nil {
		r.recordFailure()
		r.logger.WithError(err).WithFields(logrus.Fields{
			"queue":   queueName,
			"message": string(body),
		}).Error("Failed to publish message")
		return fmt.Errorf("failed to publish message: %w", err)
	}

	r.recordSuccess()
	r.logger.WithFields(logrus.Fields{
		"queue":      queueName,
		"message_id": publishing.MessageId,
	}).Debug("Message published successfully")

	return nil
}

// PublishMessageWithHeaders publishes a message with custom headers (for trace context)
func (r *RabbitMQService) PublishMessageWithHeaders(ctx context.Context, queueName string, message interface{}, headers map[string]interface{}) error {
	// Check circuit breaker first (fast fail)
	if err := r.checkCircuitBreaker(); err != nil {
		return err
	}

	// Serialize message to JSON
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Convert headers to AMQP Table
	amqpHeaders := amqp.Table{}
	for k, v := range headers {
		amqpHeaders[k] = v
	}

	publishing := amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
		Headers:      amqpHeaders,
	}

	// Try to use channel pool for better concurrency
	if r.channelPool != nil {
		err = r.channelPool.PublishWithPool(ctx, r.config.RabbitMQ.Exchange, queueName, publishing)
		if err != nil {
			r.recordFailure()
			r.logger.WithError(err).WithFields(logrus.Fields{
				"queue":   queueName,
				"message": string(body),
				"headers": headers,
			}).Error("Failed to publish message with headers via pool")
			return fmt.Errorf("failed to publish message with headers: %w", err)
		}
		r.recordSuccess()
		r.logger.WithFields(logrus.Fields{
			"queue":      queueName,
			"message_id": publishing.MessageId,
			"headers":    headers,
			"via":        "pool",
		}).Debug("Message with headers published successfully")
		return nil
	}

	// Fallback to single channel
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if !r.isConnected {
		r.recordFailure()
		return fmt.Errorf("RabbitMQ connection is not available")
	}

	err = r.channel.PublishWithContext(
		ctx,
		r.config.RabbitMQ.Exchange,
		queueName,
		false,
		false,
		publishing,
	)

	if err != nil {
		r.recordFailure()
		r.logger.WithError(err).WithFields(logrus.Fields{
			"queue":   queueName,
			"message": string(body),
			"headers": headers,
		}).Error("Failed to publish message with headers")
		return fmt.Errorf("failed to publish message with headers: %w", err)
	}

	r.recordSuccess()
	r.logger.WithFields(logrus.Fields{
		"queue":      queueName,
		"message_id": publishing.MessageId,
		"headers":    headers,
	}).Debug("Message with headers published successfully")

	return nil
}

// PublishMessageWithDelay publishes a message with a delay using RabbitMQ delayed message plugin
func (r *RabbitMQService) PublishMessageWithDelay(ctx context.Context, queueName string, message interface{}, delay time.Duration) error {
	// Check circuit breaker first (fast fail)
	if err := r.checkCircuitBreaker(); err != nil {
		return err
	}

	// Serialize message to JSON
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Use headers for delay information
	headers := amqp.Table{
		"x-delay": int64(delay.Milliseconds()),
	}

	publishing := amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
		Headers:      headers,
	}

	// Try to use channel pool for better concurrency
	if r.channelPool != nil {
		err = r.channelPool.PublishWithPool(ctx, r.config.RabbitMQ.Exchange, queueName, publishing)
		if err != nil {
			r.recordFailure()
			return fmt.Errorf("failed to publish delayed message: %w", err)
		}
		r.recordSuccess()
		return nil
	}

	// Fallback to single channel
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if !r.isConnected {
		r.recordFailure()
		return fmt.Errorf("RabbitMQ connection is not available")
	}

	err = r.channel.PublishWithContext(
		ctx,
		r.config.RabbitMQ.Exchange,
		queueName,
		false,
		false,
		publishing,
	)

	if err != nil {
		r.recordFailure()
		return fmt.Errorf("failed to publish delayed message: %w", err)
	}

	r.recordSuccess()
	return nil
}

// PublishPriorityMessage publishes a message with priority
func (r *RabbitMQService) PublishPriorityMessage(ctx context.Context, queueName string, message interface{}, priority uint8) error {
	// Check circuit breaker first (fast fail)
	if err := r.checkCircuitBreaker(); err != nil {
		return err
	}

	// Serialize message to JSON
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	publishing := amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Priority:     priority,
		Timestamp:    time.Now(),
		MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
	}

	// Try to use channel pool for better concurrency
	if r.channelPool != nil {
		err = r.channelPool.PublishWithPool(ctx, r.config.RabbitMQ.Exchange, queueName, publishing)
		if err != nil {
			r.recordFailure()
			return fmt.Errorf("failed to publish priority message: %w", err)
		}
		r.recordSuccess()
		return nil
	}

	// Fallback to single channel
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if !r.isConnected {
		r.recordFailure()
		return fmt.Errorf("RabbitMQ connection is not available")
	}

	err = r.channel.PublishWithContext(
		ctx,
		r.config.RabbitMQ.Exchange,
		queueName,
		false,
		false,
		publishing,
	)

	if err != nil {
		r.recordFailure()
		return fmt.Errorf("failed to publish priority message: %w", err)
	}

	r.recordSuccess()
	return nil
}

// handleReconnect monitors connection and handles automatic reconnection
func (r *RabbitMQService) handleReconnect() {
	for {
		select {
		case err := <-r.notifyConnClose:
			if r.isShutdown {
				return
			}
			r.logger.WithError(err).Error("RabbitMQ connection lost, attempting to reconnect")
			r.mutex.Lock()
			r.isConnected = false
			r.mutex.Unlock()
			r.reconnect()

		case err := <-r.notifyChanClose:
			if r.isShutdown {
				return
			}
			r.logger.WithError(err).Error("RabbitMQ channel lost, attempting to reconnect")
			r.mutex.Lock()
			r.isConnected = false
			r.mutex.Unlock()
			r.reconnect()

		case <-r.notifyReconnect:
			if r.isShutdown {
				return
			}
			r.logger.Info("Manual reconnection requested")
			r.reconnect()
		}
	}
}

// reconnect attempts to reconnect to RabbitMQ with exponential backoff
func (r *RabbitMQService) reconnect() {
	retryCount := 0
	maxRetries := r.config.RabbitMQ.MaxRetries

	for retryCount < maxRetries {
		if r.isShutdown {
			return
		}

		// Exponential backoff
		delay := time.Duration(retryCount*retryCount+1) * time.Second
		r.logger.WithFields(logrus.Fields{
			"retry_count": retryCount + 1,
			"max_retries": maxRetries,
			"delay":       delay,
		}).Info("Attempting to reconnect to RabbitMQ")

		time.Sleep(delay)

		if err := r.connect(); err != nil {
			r.logger.WithError(err).WithField("retry_count", retryCount+1).Error("Failed to reconnect to RabbitMQ")
			retryCount++
			continue
		}

		r.logger.Info("Successfully reconnected to RabbitMQ")
		return
	}

	r.logger.WithField("max_retries", maxRetries).Error("Failed to reconnect to RabbitMQ after maximum retries")
}

// HealthCheck implements the HealthChecker interface
func (r *RabbitMQService) HealthCheck(ctx context.Context) error {
	// Check circuit breaker first - if open, fail fast
	if r.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker is open, RabbitMQ health check skipped")
	}

	// Read connection state under a brief lock — no I/O while holding the mutex
	r.mutex.Lock()
	connected := r.isConnected
	conn := r.connection
	r.mutex.Unlock()

	if !connected || conn == nil || conn.IsClosed() {
		r.recordFailure()
		return fmt.Errorf("RabbitMQ connection is not available")
	}

	r.recordSuccess()
	return nil
}

// GetQueueInfo returns information about a specific queue
func (r *RabbitMQService) GetQueueInfo(queueName string) (amqp.Queue, error) {
	r.mutex.Lock() // Use Lock - channel operations are not thread-safe
	defer r.mutex.Unlock()

	if !r.isConnected {
		return amqp.Queue{}, fmt.Errorf("RabbitMQ connection is not available")
	}

	return r.channel.QueueDeclarePassive(queueName, true, false, false, false, nil)
}

// Close gracefully closes the RabbitMQ connection
func (r *RabbitMQService) Close() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.isShutdown = true
	r.isConnected = false

	// Close channel pool first
	if r.channelPool != nil {
		r.channelPool.Close()
		r.channelPool = nil
	}

	if r.channel != nil {
		if err := r.channel.Close(); err != nil {
			r.logger.WithError(err).Error("Failed to close RabbitMQ channel")
		}
	}

	if r.connection != nil {
		if err := r.connection.Close(); err != nil {
			r.logger.WithError(err).Error("Failed to close RabbitMQ connection")
			return fmt.Errorf("failed to close RabbitMQ connection: %w", err)
		}
	}

	r.logger.Info("RabbitMQ connection closed")
	return nil
}

// IsConnected returns the current connection status
func (r *RabbitMQService) IsConnected() bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.isConnected
}

// TriggerReconnect manually triggers a reconnection attempt
func (r *RabbitMQService) TriggerReconnect() {
	select {
	case r.notifyReconnect <- true:
	default:
		// Channel is full, reconnection already in progress
	}
}
