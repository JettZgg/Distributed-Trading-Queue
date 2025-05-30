package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

var (
	natsURL = getEnv("NATS_URL", "nats://localhost:4222")
	port    = getEnv("PORT", "8080")
	log     = logrus.WithFields(logrus.Fields{
		"service": "orderservice",
		"version": "1.0.0",
	})

	// Async Publisher Configuration
	ackHandlerGoroutines  = getEnvInt("ACK_HANDLER_GOROUTINES", 30)                    // Number of goroutines handling NATS PubAck responses.
	publishTaskQueueSize  = getEnvInt("PUBLISH_TASK_QUEUE_SIZE", 4096)                 // Buffer size for the internal channel that queues messages for publishing.
	defaultPublishTimeout = getEnvDuration("DEFAULT_PUBLISH_TIMEOUT", 5*time.Second)   // Max time to wait for a PubAck from NATS server.
	maxPublishRetries     = getEnvInt("MAX_PUBLISH_RETRIES", 3)                        // Max number of times to retry publishing a message if it fails.
	retryBackoffBase      = getEnvDuration("RETRY_BACKOFF_BASE", 250*time.Millisecond) // Base duration for exponential backoff on publish retries.
)

// publishTask holds the NATS message to be published and retry metadata.
type publishTask struct {
	subject string // NATS subject to publish to.
	data    []byte // Message payload.
	retries int    // Current number of retry attempts for this task.
}

// AsyncPublisher handles asynchronous publishing of messages to NATS JetStream with retries.
type AsyncPublisher struct {
	js          nats.JetStreamContext // NATS JetStream context.
	taskChan    chan *publishTask     // Channel to queue messages for publishing.
	wg          sync.WaitGroup        // WaitGroup to manage handler goroutines.
	quitChan    chan struct{}         // Channel to signal handlers to stop.
	ackTimeout  time.Duration         // Timeout for waiting for a PubAck.
	maxRetries  int                   // Maximum publish retries for a single message.
	backoffBase time.Duration         // Base duration for exponential backoff.
	numHandlers int                   // Number of concurrent ACK handler goroutines.
}

// NewAsyncPublisher creates and initializes a new AsyncPublisher.
// It does not start the handler goroutines; call Start() for that.
func NewAsyncPublisher(js nats.JetStreamContext, numHandlers, queueSize, maxRetries int, ackTimeout, backoffBase time.Duration) *AsyncPublisher {
	publisher := &AsyncPublisher{
		js:          js,
		taskChan:    make(chan *publishTask, queueSize),
		quitChan:    make(chan struct{}),
		ackTimeout:  ackTimeout,
		maxRetries:  maxRetries,
		backoffBase: backoffBase,
		numHandlers: numHandlers,
	}
	return publisher
}

// Start launches the ACK handler goroutines that process messages from the taskChan.
func (p *AsyncPublisher) Start() {
	p.wg.Add(p.numHandlers)
	for i := 0; i < p.numHandlers; i++ {
		go p.ackHandler(i)
	}
	log.Infof("AsyncPublisher started with %d ACK handler goroutines", p.numHandlers)
}

// Publish attempts to send a message to the internal task queue for asynchronous publishing.
// It returns an error if the task queue is full or if the publisher is stopping.
func (p *AsyncPublisher) Publish(subject string, data []byte) error {
	task := &publishTask{
		subject: subject,
		data:    data,
		retries: 0,
	}

	select {
	case p.taskChan <- task: // Try to send the task to the queue.
		return nil
	case <-p.quitChan: // Check if publisher is stopping.
		log.Warnf("AsyncPublisher: Publish called after Stop. Subject: %s", subject)
		return errors.New("async publisher is stopping")
	default: // Non-blocking check if taskChan is full.
		log.Warnf("AsyncPublisher: Task channel full. Subject: %s. Consider increasing PUBLISH_TASK_QUEUE_SIZE or ACK_HANDLER_GOROUTINES.", subject)
		return errors.New("async publisher task channel full")
	}
}

// Stop signals all handler goroutines to stop and waits for them to finish.
func (p *AsyncPublisher) Stop() {
	log.Info("Stopping AsyncPublisher...")
	close(p.quitChan) // Signal handlers to stop by closing the quitChan.
	p.wg.Wait()       // Wait for all handler goroutines to complete.
	log.Info("AsyncPublisher stopped.")
}

// ackHandler is the main processing loop for each ACK handler goroutine.
// It receives tasks from taskChan and attempts to publish them.
func (p *AsyncPublisher) ackHandler(id int) {
	defer p.wg.Done()
	log.Infof("ACK Handler %d started", id)
	for {
		select {
		case task, ok := <-p.taskChan: // Wait for a task or channel close.
			if !ok { // taskChan has been closed.
				log.Infof("ACK Handler %d: taskChan closed, exiting.", id)
				return
			}
			p.processPublishTask(task, id) // Process the received task.
		case <-p.quitChan: // quitChan has been closed, signaling a stop.
			log.Infof("ACK Handler %d: quit signal received, draining remaining tasks...", id)
			// Drain remaining tasks from taskChan before exiting.
			for {
				select {
				case task, ok := <-p.taskChan:
					if !ok {
						log.Infof("ACK Handler %d: taskChan closed during drain, exiting.", id)
						return
					}
					p.processPublishTask(task, id)
				default: // taskChan is empty.
					log.Infof("ACK Handler %d: finished draining tasks, exiting.", id)
					return
				}
			}
		}
	}
}

// processPublishTask attempts to publish a single task to NATS JetStream.
// It handles waiting for the PubAck and potential retries.
func (p *AsyncPublisher) processPublishTask(task *publishTask, handlerID int) {
	l := log.WithFields(logrus.Fields{
		"handler_id": handlerID,
		"subject":    task.subject,
		"msg_size":   len(task.data),
		"attempt":    task.retries + 1, // Log current attempt number (1-based).
	})

	l.Infof("AsyncPublish: Processing task.")

	msg := &nats.Msg{Subject: task.subject, Data: task.data}
	ackFuture, err := p.js.PublishMsgAsync(msg) // Publish message asynchronously.

	if err != nil { // This error occurs if PublishMsgAsync itself fails before sending.
		l.WithError(err).Error("AsyncPublish: PublishMsgAsync initial call failed")
		p.handleFailedPublish(task, err, l) // Attempt to handle the failure (e.g., retry).
		return
	}

	// Wait for PubAck, error, timeout, or quit signal.
	select {
	case pa := <-ackFuture.Ok(): // Message successfully acknowledged by the NATS server.
		l.WithFields(logrus.Fields{
			"stream":    pa.Stream,
			"sequence":  pa.Sequence,
			"duplicate": pa.Duplicate,
		}).Info("AsyncPublish: Message published and ACKed successfully.")
		return

	case pubErr := <-ackFuture.Err(): // NATS server returned an error (NACK).
		l.WithError(pubErr).Warn("AsyncPublish: Publish failed (NACK or server error)")
		p.handleFailedPublish(task, pubErr, l)

	case <-time.After(p.ackTimeout): // Timeout waiting for PubAck.
		timeoutErr := errors.New("async publish timed out waiting for ACK/NACK")
		l.WithError(timeoutErr).Warn("AsyncPublish: Publish failed (timeout)")
		p.handleFailedPublish(task, timeoutErr, l)

	case <-p.quitChan: // Publisher is stopping.
		l.Warn("AsyncPublish: Quit signal received while waiting for PubAck. Abandoning task.")
		return // Do not retry if quitting.
	}
}

// handleFailedPublish manages the retry logic for a failed publish attempt.
func (p *AsyncPublisher) handleFailedPublish(task *publishTask, pubErr error, l *logrus.Entry) {
	if task.retries < p.maxRetries { // Check if max retries have been reached.
		task.retries++
		// Calculate exponential backoff duration.
		backoffDuration := p.backoffBase * time.Duration(1<<uint(task.retries-1)) // base * 2^(retries-1)
		// Cap backoff to a reasonable maximum (e.g., 10 seconds).
		const maxBackoff = 10 * time.Second
		if backoffDuration > maxBackoff {
			backoffDuration = maxBackoff
		}

		l.WithError(pubErr).Warnf("AsyncPublish: Publish failed (attempt %d/%d). Retrying in %v...", task.retries, p.maxRetries, backoffDuration)

		retryTimer := time.NewTimer(backoffDuration)
		defer retryTimer.Stop() // Ensure timer is stopped to free resources.

		select {
		case <-retryTimer.C: // Wait for backoff duration.
			// Re-queue the task for another attempt.
			select {
			case p.taskChan <- task:
				l.Info("AsyncPublish: Task re-queued for retry.")
			case <-p.quitChan:
				l.Warn("AsyncPublish: Quit signal received while trying to re-queue for retry. Abandoning task.")
			default: // taskChan is full, cannot re-queue.
				l.Error("AsyncPublish: Failed to re-queue task for retry: task channel full. Abandoning task.")
			}
		case <-p.quitChan: // Publisher stopping during backoff.
			l.Warn("AsyncPublish: Quit signal received while waiting for retry backoff. Abandoning task.")
		}
	} else { // Max retries reached.
		l.WithError(pubErr).Errorf("AsyncPublish: Publish failed after %d retries. Giving up on message.", p.maxRetries)
		// Consider sending the message to a dead-letter queue (DLQ) here.
		// e.g., p.js.Publish("ORDERS.deadletter", task.data)
	}
}

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

// getEnvInt parses an environment variable as an integer.
// Returns defaultValue if the variable is not set or parsing fails.
func getEnvInt(key string, defaultValue int) int {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(valueStr)
	if err != nil {
		log.Warnf("Invalid value for %s: %s. Using default: %d", key, valueStr, defaultValue)
		return defaultValue
	}
	return value
}

// getEnvDuration parses an environment variable as a time.Duration (e.g., "5s", "250ms").
// Returns defaultValue if the variable is not set or parsing fails.
func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := time.ParseDuration(valueStr)
	if err != nil {
		log.Warnf("Invalid duration value for %s: %s. Using default: %v", key, valueStr, defaultValue)
		return defaultValue
	}
	return value
}

// Order defines the structure for an incoming order request.
// Note: Fields must be exported (start with uppercase) to be serialized/deserialized by encoding/json.
type Order struct {
	UserID string `json:"userID"`
	Items  []Item `json:"items"`
}

// Item defines the structure for an item within an order.
type Item struct {
	ItemID   string  `json:"itemID"`
	Quantity int     `json:"quantity"`
	Price    float64 `json:"price"`
}

func main() {
	// Set log format to JSON for structured logging.
	logrus.SetFormatter(&logrus.JSONFormatter{})
	// Set log level.
	logrus.SetLevel(logrus.InfoLevel)

	log.Info("Starting OrderService...")

	// Connect to NATS server.
	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.WithError(err).Fatalf("Failed to connect to NATS at %s", natsURL)
	}
	defer nc.Close()
	log.Infof("Connected to NATS server at %s", natsURL)

	// Get JetStream context.
	js, err := nc.JetStream()
	if err != nil {
		log.WithError(err).Fatal("Failed to get JetStream context")
	}

	// Ensure the ORDERS stream exists.
	streamName := "ORDERS"
	_, err = js.StreamInfo(streamName)
	if err != nil {
		if errors.Is(err, nats.ErrStreamNotFound) {
			log.Infof("Stream %s not found, creating...", streamName)
			_, err = js.AddStream(&nats.StreamConfig{
				Name:     streamName,
				Subjects: []string{streamName + ".*"}, // Subject hierarchy for orders, e.g., ORDERS.placed
				Storage:  nats.FileStorage,            // Use FileStorage for persistence.
				// Retention: nats.LimitsPolicy, // Default, keeps all messages unless limits are hit.
				// MaxAge: 24 * time.Hour, // Example: Retain messages for 24 hours.
			})
			if err != nil {
				log.WithError(err).Fatalf("Failed to create stream %s", streamName)
			}
			log.Infof("Stream %s created", streamName)
		} else {
			log.WithError(err).Fatalf("Failed to get stream info for %s", streamName)
		}
	} else {
		log.Infof("Stream %s already exists.", streamName)
	}

	// Initialize and start the AsyncPublisher.
	asyncPublisher := NewAsyncPublisher(js, ackHandlerGoroutines, publishTaskQueueSize, maxPublishRetries, defaultPublishTimeout, retryBackoffBase)
	asyncPublisher.Start()

	// Set up HTTP router.
	r := mux.NewRouter()
	r.HandleFunc("/api/orders", createOrderHandler(asyncPublisher)).Methods("POST")
	r.HandleFunc("/health", healthCheckHandler).Methods("GET")
	r.Handle("/metrics", promhttp.Handler()).Methods("GET") // Expose Prometheus metrics.

	httpSrv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	// Start HTTP server in a goroutine.
	go func() {
		log.Infof("OrderService HTTP server starting on port %s", port)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithError(err).Fatal("OrderService HTTP server ListenAndServe error")
		}
	}()

	// Shutdown handling.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit // Block until a signal is received.

	log.Info("OrderService shutting down...")

	// Stop the async publisher.
	asyncPublisher.Stop()

	// Create a context with timeout for HTTP server shutdown.
	ctxShutdown, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()

	if err := httpSrv.Shutdown(ctxShutdown); err != nil {
		log.WithError(err).Error("OrderService HTTP server shutdown error")
	}

	// Drain NATS connection (optional, but good practice).
	log.Info("Draining NATS connection...")
	if err := nc.Drain(); err != nil {
		log.WithError(err).Error("Error draining NATS connection")
	}

	log.Info("OrderService shut down.")
}

// healthCheckHandler responds to health check requests.
func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// createOrderHandler handles incoming order requests.
func createOrderHandler(publisher *AsyncPublisher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var order Order
		if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
			log.WithError(err).Error("Invalid order payload")
			http.Error(w, "Invalid order payload: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Basic validation (example)
		if order.UserID == "" {
			log.Error("Missing userID in order")
			http.Error(w, "Missing userID", http.StatusBadRequest)
			return
		}
		if len(order.Items) == 0 {
			log.Error("No items in order")
			http.Error(w, "No items in order", http.StatusBadRequest)
			return
		}

		orderData, err := json.Marshal(order)
		if err != nil {
			log.WithError(err).Error("Failed to marshal order data")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Publish to NATS JetStream asynchronously.
		subject := "ORDERS.placed" // Define the subject for order placement events.
		if err := publisher.Publish(subject, orderData); err != nil {
			// Handle specific errors from the async publisher.
			if err.Error() == "async publisher task channel full" {
				log.WithError(err).Warn("Order rejected: service busy (task channel full)")
				http.Error(w, "Service temporarily unavailable, please try again later.", http.StatusServiceUnavailable)
			} else if err.Error() == "async publisher is stopping" {
				log.WithError(err).Warn("Order rejected: service shutting down")
				http.Error(w, "Service shutting down, please try again later.", http.StatusServiceUnavailable)
			} else {
				log.WithError(err).Error("Failed to publish order event")
				http.Error(w, "Failed to process order", http.StatusInternalServerError)
			}
			return
		}

		// Respond with HTTP 202 Accepted, as the message is queued for processing.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{
			"message": "Order received and queued for processing.",
			// Potentially include a tracking ID if generated before publishing.
		})
		log.WithFields(logrus.Fields{
			"user_id": order.UserID,
			"subject": subject,
		}).Info("Order placed event published to NATS successfully (queued by async publisher)")
	}
}
