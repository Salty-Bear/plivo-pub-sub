package providers

import (
	"github.com/melvinodsa/go-iam/services/pubsub"
)

type Service struct {
	PubSub pubsub.PubSub
}

func NewServices() *Service {
	pubsubSvc := pubsub.NewService()
	return &Service{PubSub: pubsubSvc}
}
