# Build stage
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o pubsub ./cmd/main.go   # adjust path to your main.go

# Run stage
FROM alpine:3.18
WORKDIR /app
COPY --from=builder /app/pubsub .
EXPOSE 3000
CMD ["./pubsub"]
