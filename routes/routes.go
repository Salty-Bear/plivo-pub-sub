package routes

import (
	"github.com/gofiber/fiber/v2"
	"github.com/melvinodsa/go-iam/routes/pubsub"
)

// RegisterRoutes registers all main API routes
func RegisterRoutes(app *fiber.App) {
	pubsub.RegisterRoutes(app.Group("/pubsub"))
}
