package pubsub

import (
	"context"
	"sync"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
)

// Topic represents a pubsub topic with a name and a set of subscribers.
type Topic struct {
	Name        string
	Subscribers map[*websocket.Conn]struct{}
	mu          sync.Mutex
}

type ServiceImpl struct {
	Topics     map[string]*Topic
	mu         sync.RWMutex
	LastSocket *websocket.Conn // store last socket connection (or use a slice for all)
}

func NewService() *ServiceImpl {
	return &ServiceImpl{Topics: make(map[string]*Topic)}
}

func (s *ServiceImpl) HandleWebSocket(ctx context.Context, c *websocket.Conn) {
	s.LastSocket = c // inject the socket connection into the service
	var currentTopic *Topic
	for {
		var req struct {
			Action string `json:"action"`
			Topic  string `json:"topic"`
			Data   string `json:"data"`
		}
		if err := c.ReadJSON(&req); err != nil {
			break
		}
		switch req.Action {
		case "subscribe":
			s.mu.RLock()
			topic, ok := s.Topics[req.Topic]
			s.mu.RUnlock()
			if !ok {
				c.WriteMessage(websocket.TextMessage, []byte("topic not found"))
				continue
			}
			topic.mu.Lock()
			topic.Subscribers[c] = struct{}{}
			topic.mu.Unlock()
			currentTopic = topic
		case "publish":
			s.mu.RLock()
			topic, ok := s.Topics[req.Topic]
			s.mu.RUnlock()
			if !ok {
				c.WriteMessage(websocket.TextMessage, []byte("topic not found"))
				continue
			}
			topic.mu.Lock()
			for sub := range topic.Subscribers {
				if sub != c {
					sub.WriteMessage(websocket.TextMessage, []byte(req.Data))
				}
			}
			topic.mu.Unlock()
		}
	}
	if currentTopic != nil {
		currentTopic.mu.Lock()
		delete(currentTopic.Subscribers, c)
		currentTopic.mu.Unlock()
	}
}

func (s *ServiceImpl) CreateTopic(ctx context.Context, c *fiber.Ctx) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.BodyParser(&req); err != nil || req.Name == "" {
		return c.Status(fiber.StatusBadRequest).SendString("invalid request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.Topics[req.Name]; exists {
		return c.Status(fiber.StatusConflict).SendString("topic exists")
	}
	s.Topics[req.Name] = &Topic{Name: req.Name, Subscribers: make(map[*websocket.Conn]struct{})}
	return c.SendStatus(fiber.StatusCreated)
}

func (s *ServiceImpl) DeleteTopic(ctx context.Context, c *fiber.Ctx) error {
	name := c.Query("name")
	if name == "" {
		return c.Status(fiber.StatusBadRequest).SendString("missing name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Topics, name)
	return c.SendStatus(fiber.StatusOK)
}

func (s *ServiceImpl) ListTopics(ctx context.Context, c *fiber.Ctx) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var names []string
	for name := range s.Topics {
		names = append(names, name)
	}
	return c.JSON(names)
}

func (s *ServiceImpl) Health(ctx context.Context, c *fiber.Ctx) error {
	return c.SendString("ok")
}

func (s *ServiceImpl) Stats(ctx context.Context, c *fiber.Ctx) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stats := make(map[string]int)
	for name, topic := range s.Topics {
		topic.mu.Lock()
		stats[name] = len(topic.Subscribers)
		topic.mu.Unlock()
	}
	return c.JSON(stats)
}
