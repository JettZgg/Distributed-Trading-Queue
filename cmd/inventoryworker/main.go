package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

var (
	natsURL     = getEnv("NATS_URL", "nats://localhost:4222")
	metricsPort = getEnv("METRICS_PORT", "2112")
	log         = logrus.WithFields(logrus.Fields{
		"service": "inventoryworker",
		"version": "1.0.0",
	})
)

// AppConfig holds application configuration
type AppConfig struct {
	NatsURL     string
	StreamName  string
	Subject     string
	QueueName   string
	DurableName string
	AckWait     time.Duration
	MaxDeliver  int
}

var appConfig AppConfig

// Constants for simulating error scenarios
const (
	transientErrorUserID = "user_transient_error" // User ID to simulate transient inventory errors
	invalidItemID        = "ITEM_INVALID"         // Item ID to simulate an invalid item
	stockIssueUserID     = "user_stock_issue"     // User ID to simulate stock issues (e.g., insufficient stock)
	transientNakDelay    = 5 * time.Second
)

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

type Order struct {
	UserID string `json:"userID"`
	Items  []Item `json:"items"`
}

type Item struct {
	ItemID   string  `json:"itemID"`
	Quantity int     `json:"quantity"`
	Price    float64 `json:"price"`
}

func main() {
	// Set log format to JSON
	logrus.SetFormatter(&logrus.JSONFormatter{})
	// Set log level
	logrus.SetLevel(logrus.InfoLevel)

	log.Info("Starting InventoryWorker...")

	appConfig = AppConfig{
		NatsURL:     natsURL,
		StreamName:  "ORDERS",
		Subject:     "ORDERS.placed",
		QueueName:   "INVENTORY_EVENT_QUEUE",
		DurableName: "INVENTORY_WORKER_DURABLE",
		AckWait:     30 * time.Second,
		MaxDeliver:  3,
	}

	// Connect to NATS
	nc, err := nats.Connect(appConfig.NatsURL)
	if err != nil {
		log.WithError(err).Fatal("Failed to connect to NATS")
	}
	defer nc.Close()

	log.WithField("nats_url", appConfig.NatsURL).Info("Connected to NATS server")

	// Get JetStream context
	js, err := nc.JetStream()
	if err != nil {
		log.WithError(err).Fatal("Failed to get JetStream context")
	}

	// Define and ensure durable consumer exists
	consumerConfig := &nats.ConsumerConfig{
		Durable:        appConfig.DurableName,
		Name:           appConfig.DurableName,
		Description:    "Durable consumer for Inventory Worker",
		FilterSubject:  appConfig.Subject,
		DeliverSubject: appConfig.DurableName + "_INBOX", // Important for queue groups
		DeliverGroup:   appConfig.QueueName,
		AckPolicy:      nats.AckExplicitPolicy,
		AckWait:        appConfig.AckWait,
		MaxDeliver:     appConfig.MaxDeliver,
		ReplayPolicy:   nats.ReplayInstantPolicy, // Replay all available messages
	}

	_, err = js.ConsumerInfo(appConfig.StreamName, appConfig.DurableName)
	if err != nil {
		if errors.Is(err, nats.ErrConsumerNotFound) {
			log.Infof("Consumer %s not found, creating...", appConfig.DurableName)
			_, addErr := js.AddConsumer(appConfig.StreamName, consumerConfig)
			if addErr != nil {
				log.WithError(addErr).Fatalf("Failed to add durable consumer %s", appConfig.DurableName)
			}
			log.Infof("Durable consumer %s created", appConfig.DurableName)
		} else {
			log.WithError(err).Fatalf("Failed to get consumer info for %s", appConfig.DurableName)
		}
	} else {
		log.Infof("Durable consumer %s already exists. Ensure config is up-to-date if necessary.", appConfig.DurableName)
	}

	log.Printf("InventoryWorker queue subscribing to NATS subject '%s' with queue group '%s', durable consumer '%s'",
		appConfig.Subject, appConfig.QueueName, appConfig.DurableName)

	// Subscribe with Queue Group and Durable Name
	sub, err := js.QueueSubscribe(
		appConfig.Subject,   // Subject to subscribe to
		appConfig.QueueName, // Queue group name
		func(msg *nats.Msg) { // Message handler
			meta, err := msg.Metadata() // Get JetStream metadata
			if err != nil {
				log.WithError(err).Warn("Error getting message metadata")
			}
			if meta != nil {
				log.WithFields(logrus.Fields{
					"stream_seq":      meta.Sequence.Stream,
					"consumer_seq":    meta.Sequence.Consumer,
					"num_delivered":   meta.NumDelivered,
					"subject":         msg.Subject,
					"deliver_subject": msg.Reply,
				}).Info("Received a message")
			} else {
				log.WithField("subject", msg.Subject).Info("Received a message (no JS metadata)")
			}

			var order Order
			if err := json.Unmarshal(msg.Data, &order); err != nil {
				log.WithError(err).Error("Failed to unmarshal order")
				// Terminate message if it cannot be unmarshalled
				if termErr := msg.Term(); termErr != nil {
					log.WithError(termErr).Error("Error sending Term for unmarshalling error")
				}
				return
			}

			log.WithFields(logrus.Fields{
				"user_id": order.UserID,
				"items":   len(order.Items),
			}).Info("Processing order")

			// Simulate specific NACK/Term scenarios for InventoryWorker
			if order.UserID == transientErrorUserID {
				log.WithFields(logrus.Fields{
					"user_id":       order.UserID,
					"items":         len(order.Items),
					"num_delivered": meta.NumDelivered,
				}).Info("Simulating transient inventory error")
				// Nack with delay for transient errors
				if nakErr := msg.NakWithDelay(transientNakDelay); nakErr != nil {
					log.WithError(nakErr).Error("Error sending NakWithDelay")
				}
				return // Message will be redelivered
			}

			for _, item := range order.Items {
				if item.ItemID == invalidItemID {
					log.WithField("item_id", item.ItemID).Error("Simulating invalid ItemID")
					// Terminate message for invalid item ID
					if termErr := msg.Term(); termErr != nil {
						log.WithError(termErr).Error("Error sending Term for invalid ItemID")
					}
					return // Message will not be redelivered
				}
			}

			if order.UserID == stockIssueUserID {
				log.WithField("user_id", order.UserID).Error("Simulating insufficient stock")
				// Terminate message for stock issues
				if termErr := msg.Term(); termErr != nil {
					log.WithError(termErr).Error("Error sending Term for insufficient stock")
				}
				return // Message will not be redelivered
			}

			// Simulate successful inventory processing
			log.WithFields(logrus.Fields{
				"user_id": order.UserID,
				"items":   len(order.Items),
			}).Info("Simulating successful inventory processing")
			// time.Sleep(1 * time.Second) // Simulate some work
			log.WithFields(logrus.Fields{
				"user_id": order.UserID,
				"items":   len(order.Items),
			}).Info("Finished processing")

			// Acknowledge the message
			if err := msg.Ack(); err != nil {
				log.WithError(err).Error("Error sending ACK")
			}
		},
		nats.Durable(appConfig.DurableName),   // Make the subscription durable
		nats.ManualAck(),                      // Require manual acknowledgement
		nats.BindStream(appConfig.StreamName), // Bind to a specific stream
	)

	if err != nil {
		log.WithError(err).Fatal("Failed to subscribe to subject")
	}
	log.Info("InventoryWorker successfully subscribed")

	// Start HTTP server for metrics
	metricsRouter := http.NewServeMux()
	metricsRouter.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:    ":" + metricsPort,
		Handler: metricsRouter,
	}

	go func() {
		log.WithField("port", metricsPort).Info("Starting metrics server")
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Error("Metrics server ListenAndServe error")
			// Do not kill the main worker if metrics server fails to start
		}
	}()

	// Channel to receive errors from goroutines
	errChan := make(chan error, 1)

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Wait for interrupt signal or an error
	select {
	case err := <-errChan:
		log.WithError(err).Fatal("Worker error occurred")
	case <-quit:
		log.Info("Shutting down...")
	}

	// Create a shutdown context with a timeout
	ctxShutdown, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	// Shutdown metrics server
	if err := metricsSrv.Shutdown(ctxShutdown); err != nil {
		log.WithError(err).Error("Metrics server shutdown error")
	}

	// Unsubscribe NATS for shutdown
	if sub != nil && sub.IsValid() {
		log.Info("Unsubscribing NATS subscription...")
		if err := sub.Unsubscribe(); err != nil {
			log.WithError(err).Error("Error during NATS Unsubscribe")
		}
		log.Info("NATS subscription unsubscribed.")
	}

	log.Info("InventoryWorker shut down")
}
