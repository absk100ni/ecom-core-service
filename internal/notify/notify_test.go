package notify

import (
	"testing"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
)

func TestOrderConfirmedMock(t *testing.T) {
	cfg := &config.Config{SMTPHost: "", StoreName: "TestStore", StoreURL: "https://test.com"}
	n := New(cfg)
	order := &models.Order{
		ID: "order-123", OrderNumber: "ORD-12345", Total: 150000,
		Items:           []models.OrderItem{{Name: "Widget", Quantity: 2, Price: 75000}},
		ShippingAddress: models.Address{Name: "John", City: "Delhi", Pincode: "110001"},
	}
	user := &models.User{ID: "u1", Email: "test@example.com"}
	// Should not panic in mock mode
	n.sendOrderConfirmedEmail(order, user)
}

func TestOrderShippedMock(t *testing.T) {
	cfg := &config.Config{SMTPHost: "", StoreName: "TestStore"}
	n := New(cfg)
	order := &models.Order{ID: "o1", OrderNumber: "ORD-99999", CODAmount: 50000}
	user := &models.User{ID: "u1", Email: "buyer@example.com"}
	n.sendOrderShippedEmail(order, user, "AWB123", "BlueDart", "https://track.example.com/AWB123")
}

func TestRefundMock(t *testing.T) {
	cfg := &config.Config{SMTPHost: "", StoreName: "TestStore"}
	n := New(cfg)
	order := &models.Order{ID: "o1", OrderNumber: "ORD-88888"}
	user := &models.User{ID: "u1", Email: "buyer@example.com"}
	n.sendRefundEmail(order, user, 75000)
}

func TestContactNotifyNoEmail(t *testing.T) {
	cfg := &config.Config{SMTPHost: "", ContactNotifyEmail: ""}
	n := New(cfg)
	// Should log and return gracefully with no CONTACT_NOTIFY_EMAIL
	n.sendContactNotifyEmail("Alice", "alice@example.com", "Help", "I need help")
}

func TestContactNotifyMock(t *testing.T) {
	cfg := &config.Config{SMTPHost: "", ContactNotifyEmail: "admin@store.com", StoreName: "TestStore"}
	n := New(cfg)
	n.sendContactNotifyEmail("Bob", "bob@example.com", "Returns", "How do I return?")
}
