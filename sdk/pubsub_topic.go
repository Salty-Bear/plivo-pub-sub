package sdk

import (
	"sync"

	"github.com/gofiber/websocket/v2"
)

type PubSubTopic struct {
	Name        string
	Subscribers map[*websocket.Conn]struct{}
	mu          sync.Mutex
}
