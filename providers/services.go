package providers

import (
	"github.com/Aryaman/pub-sub/services/pubsub"
)

type Service struct {
	PubSub pubsub.PubSub
}

func NewServices() *Service {
	pubsubSvc := pubsub.NewService()
	return &Service{PubSub: pubsubSvc}
}
