package pubsub

import (
	"context"
	"sync"
	"time"

	"github.com/melvinodsa/go-iam/sdk"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
)

// ServiceImpl implements the PubSub interface with thread-safe operations
type ServiceImpl struct {
	Topics      map[string]*sdk.Topic
	mu          sync.RWMutex
	Uptime      time.Time
	MaxQueue    int // per-subscriber queue size
	MaxMessages int // per-topic ring buffer size
}

// NewService creates a new PubSub service instance
func NewService() *ServiceImpl {
	return &ServiceImpl{
		Topics:      make(map[string]*sdk.Topic),
		Uptime:      time.Now(),
		MaxQueue:    100, // configurable queue size for backpressure handling
		MaxMessages: 100, // configurable ring buffer size for replay
	}
}

// HandleWebSocket processes WebSocket connections and messages
func (s *ServiceImpl) HandleWebSocket(ctx context.Context, c *websocket.Conn) {
	defer c.Close()

	var clientID string
	var currentTopic *sdk.Topic
	var currentSub *sdk.Subscriber

	for {
		// Parse incoming message using SDK struct
		var req sdk.WebSocketRequest
		if err := c.ReadJSON(&req); err != nil {
			// Connection closed or invalid JSON
			break
		}

		// Handle different message types according to protocol specification
		switch req.Type {
		case sdk.MessageTypeSubscribe:
			if req.Topic == "" || req.ClientID == "" {
				sendErrorWS(c, req.RequestID, sdk.ErrorCodeBadRequest, "topic and client_id required")
				continue
			}

			clientID = req.ClientID

			// Check if topic exists
			s.mu.RLock()
			topic, ok := s.Topics[req.Topic]
			s.mu.RUnlock()

			if !ok {
				sendErrorWS(c, req.RequestID, sdk.ErrorCodeTopicNotFound, "topic not found")
				continue
			}

			// Create subscriber with bounded queue
			sub := &sdk.Subscriber{
				Conn:         c,
				ClientID:     clientID,
				Queue:        make(chan sdk.Message, s.MaxQueue),
				QueueSize:    s.MaxQueue,
				LastActive:   time.Now(),
				CloseChannel: make(chan struct{}),
			}

			// Add subscriber to topic and handle replay if requested
			topic.Mu.Lock()
			topic.Subscribers[clientID] = sub

			// Replay last_n messages from ring buffer
			if req.LastN > 0 && len(topic.Messages) > 0 {
				n := req.LastN
				if n > len(topic.Messages) {
					n = len(topic.Messages)
				}
				// Send the last n messages
				start := len(topic.Messages) - n
				for _, msg := range topic.Messages[start:] {
					select {
					case sub.Queue <- msg:
					default:
						// Queue full during replay - disconnect slow consumer
						close(sub.CloseChannel)
						delete(topic.Subscribers, clientID)
						sendErrorWS(c, req.RequestID, sdk.ErrorCodeSlowConsumer, "subscriber queue overflow during replay")
						topic.Mu.Unlock()
						goto nextMessage
					}
				}
			}
			topic.Mu.Unlock()

			currentTopic = topic
			currentSub = sub

			// Start message delivery goroutine
			go s.subscriberWriter(sub, req.Topic)
			sendAckWS(c, req.RequestID, req.Topic)

		case sdk.MessageTypeUnsubscribe:
			if req.Topic == "" || req.ClientID == "" {
				sendErrorWS(c, req.RequestID, sdk.ErrorCodeBadRequest, "topic and client_id required")
				continue
			}

			s.mu.RLock()
			topic, ok := s.Topics[req.Topic]
			s.mu.RUnlock()

			if !ok {
				sendErrorWS(c, req.RequestID, sdk.ErrorCodeTopicNotFound, "topic not found")
				continue
			}

			// Remove subscriber from topic
			topic.Mu.Lock()
			if sub, exists := topic.Subscribers[req.ClientID]; exists {
				close(sub.CloseChannel)
				delete(topic.Subscribers, req.ClientID)
			}
			topic.Mu.Unlock()

			sendAckWS(c, req.RequestID, req.Topic)

		case sdk.MessageTypePublish:
			if req.Topic == "" || req.Message == nil {
				sendErrorWS(c, req.RequestID, sdk.ErrorCodeBadRequest, "topic and message required")
				continue
			}

			s.mu.RLock()
			topic, ok := s.Topics[req.Topic]
			s.mu.RUnlock()

			if !ok {
				sendErrorWS(c, req.RequestID, sdk.ErrorCodeTopicNotFound, "topic not found")
				continue
			}

			// Add server timestamp
			req.Message.TS = time.Now().UTC()

			topic.Mu.Lock()

			// Add to ring buffer for replay functionality
			if len(topic.Messages) >= topic.MaxMessages {
				// Remove oldest message (ring buffer behavior)
				topic.Messages = topic.Messages[1:]
			}
			topic.Messages = append(topic.Messages, *req.Message)

			// Fan-out message to all subscribers
			slowConsumers := make([]string, 0)
			for clientID, sub := range topic.Subscribers {
				select {
				case sub.Queue <- *req.Message:
					// Message delivered successfully
				default:
					// Queue full - mark as slow consumer for removal
					slowConsumers = append(slowConsumers, clientID)
				}
			}

			// Remove slow consumers (backpressure policy: disconnect on overflow)
			for _, clientID := range slowConsumers {
				if sub, exists := topic.Subscribers[clientID]; exists {
					close(sub.CloseChannel)
					delete(topic.Subscribers, clientID)
					// Notify slow consumer about disconnection
					sendErrorWS(sub.Conn, req.RequestID, sdk.ErrorCodeSlowConsumer, "subscriber queue overflow")
				}
			}

			topic.Mu.Unlock()
			sendAckWS(c, req.RequestID, req.Topic)

		case sdk.MessageTypePing:
			sendPongWS(c, req.RequestID)

		default:
			sendErrorWS(c, req.RequestID, sdk.ErrorCodeBadRequest, "unknown message type")
		}
	nextMessage:
	}

	// Cleanup on connection close
	if currentTopic != nil && currentSub != nil {
		currentTopic.Mu.Lock()
		delete(currentTopic.Subscribers, currentSub.ClientID)
		currentTopic.Mu.Unlock()
	}
}

// CreateTopic creates a new topic via REST API
func (s *ServiceImpl) CreateTopic(ctx context.Context, c *fiber.Ctx) error {
	var req sdk.CreateTopicRequest

	if err := c.BodyParser(&req); err != nil || req.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(sdk.ErrorResponse{
			Error: "invalid request - name required",
		})
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if topic already exists
	if _, exists := s.Topics[req.Name]; exists {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"status": sdk.StatusConflict,
			"topic":  req.Name,
		})
	}

	// Create new topic
	s.Topics[req.Name] = &sdk.Topic{
		Name:        req.Name,
		Subscribers: make(map[string]*sdk.Subscriber),
		Messages:    make([]sdk.Message, 0, s.MaxMessages),
		MaxMessages: s.MaxMessages,
	}

	return c.Status(fiber.StatusCreated).JSON(sdk.CreateTopicResponse{
		Status: sdk.StatusCreated,
		Topic:  req.Name,
	})
}

// DeleteTopic removes a topic and disconnects all subscribers
func (s *ServiceImpl) DeleteTopic(ctx context.Context, c *fiber.Ctx) error {
	name := c.Params("name")
	if name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(sdk.ErrorResponse{
			Error: "missing topic name",
		})
	}

	s.mu.Lock()
	topic, exists := s.Topics[name]
	if !exists {
		s.mu.Unlock()
		return c.Status(fiber.StatusNotFound).JSON(sdk.ErrorResponse{
			Error: "topic not found",
		})
	}

	// Notify all subscribers about topic deletion and disconnect them
	topic.Mu.Lock()
	for _, sub := range topic.Subscribers {
		sendInfoWS(sub.Conn, name, "topic_deleted")
		close(sub.CloseChannel)
	}
	topic.Subscribers = make(map[string]*sdk.Subscriber)
	topic.Mu.Unlock()

	// Remove topic
	delete(s.Topics, name)
	s.mu.Unlock()

	return c.Status(fiber.StatusOK).JSON(sdk.DeleteTopicResponse{
		Status: sdk.StatusDeleted,
		Topic:  name,
	})
}

// ListTopics returns all topics with subscriber counts
func (s *ServiceImpl) ListTopics(ctx context.Context, c *fiber.Ctx) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	topics := make([]sdk.TopicInfo, 0, len(s.Topics))
	for name, topic := range s.Topics {
		topic.Mu.RLock()
		topics = append(topics, sdk.TopicInfo{
			Name:        name,
			Subscribers: len(topic.Subscribers),
		})
		topic.Mu.RUnlock()
	}

	return c.JSON(sdk.ListTopicsResponse{Topics: topics})
}

// Health returns system health information
func (s *ServiceImpl) Health(ctx context.Context, c *fiber.Ctx) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalSubscribers := 0
	for _, topic := range s.Topics {
		topic.Mu.RLock()
		totalSubscribers += len(topic.Subscribers)
		topic.Mu.RUnlock()
	}

	uptimeSeconds := int(time.Since(s.Uptime).Seconds())

	return c.JSON(sdk.HealthResponse{
		UptimeSeconds: uptimeSeconds,
		Topics:        len(s.Topics),
		Subscribers:   totalSubscribers,
	})
}

// Stats returns detailed statistics per topic
func (s *ServiceImpl) Stats(ctx context.Context, c *fiber.Ctx) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := make(map[string]sdk.TopicStats)
	for name, topic := range s.Topics {
		topic.Mu.RLock()
		stats[name] = sdk.TopicStats{
			Messages:    len(topic.Messages),
			Subscribers: len(topic.Subscribers),
		}
		topic.Mu.RUnlock()
	}

	return c.JSON(sdk.StatsResponse{Topics: stats})
}

// WebSocket helper functions for protocol compliance

func sendAckWS(c *websocket.Conn, requestID, topic string) {
	resp := sdk.WebSocketResponse{
		Type:      sdk.MessageTypeAck,
		RequestID: requestID,
		Topic:     topic,
		Status:    sdk.StatusOK,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	c.WriteJSON(resp)
}

func sendErrorWS(c *websocket.Conn, requestID, code, message string) {
	resp := sdk.WebSocketResponse{
		Type:      sdk.MessageTypeError,
		RequestID: requestID,
		Error: &sdk.ErrorDetail{
			Code:    code,
			Message: message,
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	c.WriteJSON(resp)
}

func sendPongWS(c *websocket.Conn, requestID string) {
	resp := sdk.WebSocketResponse{
		Type:      sdk.MessageTypePong,
		RequestID: requestID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	c.WriteJSON(resp)
}

func sendEventWS(c *websocket.Conn, topic string, msg sdk.Message) {
	resp := sdk.WebSocketResponse{
		Type:      sdk.MessageTypeEvent,
		Topic:     topic,
		Message:   &msg,
		Timestamp: msg.TS.Format(time.RFC3339),
	}
	c.WriteJSON(resp)
}

func sendInfoWS(c *websocket.Conn, topic, message string) {
	resp := sdk.WebSocketResponse{
		Type:      sdk.MessageTypeInfo,
		Topic:     topic,
		Msg:       message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	c.WriteJSON(resp)
}

// subscriberWriter delivers messages from subscriber queue to WebSocket
func (s *ServiceImpl) subscriberWriter(sub *sdk.Subscriber, topic string) {
	for {
		select {
		case msg := <-sub.Queue:
			sub.LastActive = time.Now()
			sendEventWS(sub.Conn, topic, msg)
		case <-sub.CloseChannel:
			return
		}
	}
}
