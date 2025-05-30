FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . /app/

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /app/paymentsimulatorworker ./cmd/paymentsimulatorworker/main.go

# Final stage
FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/paymentsimulatorworker /app/paymentsimulatorworker

EXPOSE 2112

CMD ["/app/paymentsimulatorworker"] 