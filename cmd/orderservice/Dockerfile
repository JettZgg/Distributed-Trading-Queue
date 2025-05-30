FROM golang:1.24-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy go.mod and go.sum from the project root (context is project root)
COPY go.mod go.sum ./
RUN go mod download

# Copy the entire project source from the root (context is project root)
COPY . .

# Build the OrderService application
# The path to main.go is relative to the project root copied into /app
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /app/orderservice ./cmd/orderservice/main.go

# Final stage: use a minimal Alpine image
FROM alpine:latest

WORKDIR /app

# Copy the built binary from the builder stage
COPY --from=builder /app/orderservice /app/orderservice

# Expose the port the OrderService listens on
EXPOSE 8080

# Command to run the application
CMD ["/app/orderservice"] 