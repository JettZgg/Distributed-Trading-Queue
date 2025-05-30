package events

import "time"

// ItemDetail represents the details of an item in an order.
// Ensure all fields are exported (start with uppercase) for JSON serialization.
type ItemDetail struct {
	ItemID   string  `json:"itemID"`   // Unique identifier for the item.
	Quantity int     `json:"quantity"` // Number of units of this item.
	Price    float64 `json:"price"`    // Price per unit of this item.
}

// OrderPlacedEvent represents the event published when an order is successfully placed.
// Ensure all fields are exported for JSON serialization.
type OrderPlacedEvent struct {
	OrderID        string       `json:"orderID"`        // Unique identifier for the order.
	UserID         string       `json:"userID"`         // Identifier for the user who placed the order.
	Items          []ItemDetail `json:"items"`          // List of items included in the order.
	TotalAmount    float64      `json:"totalAmount"`    // Total monetary value of the order.
	OrderTimestamp time.Time    `json:"orderTimestamp"` // Timestamp when the order was placed.
}
