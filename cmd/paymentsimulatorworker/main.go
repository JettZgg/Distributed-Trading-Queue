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
		"service": "paymentsimulatorworker",
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

// Constants for simulating error scenarios for PaymentSimulatorWorker
const (
	paymentTransientErrorUserID = "user_payment_transient" // User ID to simulate transient payment errors
	paymentCriticalErrorUserID  = "user_payment_fail"      // User ID to simulate critical payment failures
	transientNakDelay           = 5 * time.Second
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

	log.Info("Starting PaymentSimulatorWorker...")

	appConfig = AppConfig{
		NatsURL:     natsURL,
		StreamName:  "ORDERS",
		Subject:     "ORDERS.placed",
		QueueName:   "PAYMENT_EVENT_QUEUE",
		DurableName: "PAYMENT_SIMULATOR_WORKER_DURABLE",
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
		Description:    "Durable consumer for Payment Simulator Worker",
		FilterSubject:  appConfig.Subject,
		DeliverSubject: appConfig.DurableName + "_INBOX", // Important for queue groups with durable consumers
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
		// Optional: Update consumer if config has changed
		// _, updateErr := js.UpdateConsumer(appConfig.StreamName, consumerConfig)
		// if updateErr != nil {
		//    log.WithError(updateErr).Warnf("Failed to update durable consumer %s", appConfig.DurableName)
		// }
	}

	log.Printf("PaymentSimulatorWorker queue subscribing to NATS subject '%s' with queue group '%s', durable consumer '%s'",
		appConfig.Subject, appConfig.QueueName, appConfig.DurableName)

	// Subscribe with Queue Group and Durable Name
	// The JetStream server will load balance messages among members of the queue group.
	// Using a durable name ensures that if the worker restarts, it will resume from where it left off.
	sub, err := js.QueueSubscribe(
		appConfig.Subject,   // Subject to subscribe to
		appConfig.QueueName, // Queue group name
		func(msg *nats.Msg) { // Message handler
			meta, err := msg.Metadata() // Get JetStream metadata
			if err != nil {
				// This might happen for plain NATS messages if not a JetStream message, though unlikely with QueueSubscribe
				log.WithError(err).Warn("Error getting message metadata (may be expected for plain NATS sub if not JS msg)")
			}
			// Log with metadata if available
			if meta != nil {
				log.WithFields(logrus.Fields{
					"stream_seq":      meta.Sequence.Stream,
					"consumer_seq":    meta.Sequence.Consumer,
					"num_delivered":   meta.NumDelivered,
					"subject":         msg.Subject,
					"deliver_subject": msg.Reply, // The subject NATS delivers to this specific consumer instance
				}).Info("Received a message on subject")
			} else {
				log.WithField("subject", msg.Subject).Info("Received a message (no JS metadata)")
			}

			var order Order
			if err := json.Unmarshal(msg.Data, &order); err != nil {
				log.WithError(err).Error("Error unmarshalling message data")
				// Terminate message if it cannot be unmarshalled, as it's likely a poison pill
				if termErr := msg.Term(); termErr != nil {
					log.WithError(termErr).Error("Error sending Term for unmarshalling error")
				}
				return
			}

			log.WithFields(logrus.Fields{
				"user_id": order.UserID,
				"items":   len(order.Items),
			}).Info("Processing payment")

			// Simulate specific NACK/Term scenarios for PaymentSimulatorWorker
			if order.UserID == paymentTransientErrorUserID {
				log.WithFields(logrus.Fields{
					"user_id": order.UserID,
					// Assuming order.Items is not empty for logging a pseudo order_id
					"order_id":      order.Items[0].ItemID, // Use ItemID as a pseudo order_id for logging
					"num_delivered": meta.NumDelivered,
				}).Info("Simulating transient payment error")
				// Nack with delay for transient errors
				if nakErr := msg.NakWithDelay(transientNakDelay); nakErr != nil {
					log.WithError(nakErr).Error("Error sending NakWithDelay")
				}
				return // Message will be redelivered after delay
			}

			if order.UserID == paymentCriticalErrorUserID {
				log.WithFields(logrus.Fields{
					"user_id":  order.UserID,
					"order_id": order.Items[0].ItemID,
				}).Info("Simulating critical payment failure")
				// Terminate message for critical errors
				if termErr := msg.Term(); termErr != nil {
					log.WithError(termErr).Error("Error sending Term for critical payment failure")
				}
				return // Message will not be redelivered
			}

			// Simulate payment processing
			log.WithFields(logrus.Fields{
				"user_id": order.UserID,
				"items":   len(order.Items),
			}).Info("Simulating payment processing for order")
			// time.Sleep(1 * time.Second) // Simulate some work
			log.WithFields(logrus.Fields{
				"user_id": order.UserID,
				"items":   len(order.Items),
			}).Info("Finished processing payment")

			// Acknowledge the message
			if err := msg.Ack(); err != nil {
				log.WithError(err).Error("Error sending ACK")
				// If ACK fails, the message will be redelivered based on AckWait and MaxDeliver policies
			}
		},
		nats.Durable(appConfig.DurableName),   // Make the subscription durable
		nats.ManualAck(),                      // Require manual acknowledgement
		nats.BindStream(appConfig.StreamName), // Bind to a specific stream (optional if subject implies stream)
		// nats.AckWait(appConfig.AckWait),     // AckWait can be set here or on consumer config
		// nats.MaxDeliver(appConfig.MaxDeliver), // MaxDeliver can be set here or on consumer config
	)

	if err != nil {
		log.WithError(err).Fatal("Failed to subscribe to subject")
	}
	log.Info("PaymentSimulatorWorker successfully subscribed")

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

	// Unsubscribe NATS (optional, but good practice for shutdown)
	// This will remove the consumer if it's ephemeral, or stop receiving messages for durable ones.
	if sub != nil && sub.IsValid() {
		log.Info("Unsubscribing NATS subscription...")
		if err := sub.Unsubscribe(); err != nil {
			log.WithError(err).Error("Error during NATS Unsubscribe")
		}
		// Drain will wait for all messages currently in flight to be processed by the handler.
		// if err := sub.Drain(); err != nil {
		//    log.WithError(err).Error("Error during NATS Drain")
		// }
		log.Info("NATS subscription unsubscribed.")
	}

	log.Info("PaymentSimulatorWorker shut down")
}
