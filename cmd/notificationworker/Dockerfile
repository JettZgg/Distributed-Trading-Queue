FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . /app/

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /app/notificationworker ./cmd/notificationworker/main.go

# Final stage
FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/notificationworker /app/notificationworker

EXPOSE 2112

CMD ["/app/notificationworker"] 