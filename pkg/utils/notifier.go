package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"ecom-core-service/pkg/logger"
)

var (
	notificationURL = getNotifURL()
	msg91AuthKey    = os.Getenv("MSG91_AUTH_KEY")
	msg91SenderID   = getEnvFallback("MSG91_SENDER_ID", "ECOMSM")
	smsMode         = getEnvFallback("SMS_MODE", "mock") // "msg91" or "mock"
	log             = logger.New("NOTIFIER", "SMS")
)

func getNotifURL() string {
	if url := os.Getenv("NOTIFICATION_SERVICE_URL"); url != "" {
		return url
	}
	return "http://localhost:9090"
}

func getEnvFallback(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

type NotificationRequest struct {
	Type      string            `json:"type"`
	Recipient string            `json:"recipient"`
	Metadata  map[string]string `json:"metadata"`
	Priority  string            `json:"priority"`
}

// SendNotification sends via the notification microservice (fire-and-forget)
func SendNotification(nType, recipient string, metadata map[string]string, priority string) {
	go func() {
		req := NotificationRequest{Type: nType, Recipient: recipient, Metadata: metadata, Priority: priority}
		data, err := json.Marshal(req)
		if err != nil {
			return
		}

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(fmt.Sprintf("%s/notification/send", notificationURL), "application/json", bytes.NewBuffer(data))
		if err != nil {
			log.Warn("SendNotification", "Notification service unreachable (non-blocking)", "err", err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 300 {
			log.Info("SendNotification", "Notification sent", "type", nType, "recipient", recipient)
		}
	}()
}

// ==================== DIRECT MSG91 SMS ====================

// SendOrderSMS sends an order update SMS directly via MSG91 API
// This is used for order confirmations, shipping updates, delivery notifications
func SendOrderSMS(phone, message string) {
	go func() {
		if smsMode == "mock" || msg91AuthKey == "" {
			log.Info("SendOrderSMS", "Mock SMS sent", "phone", phone, "message", message)
			return
		}

		// MSG91 Flow API for transactional SMS
		err := sendViaMSG91(phone, message)
		if err != nil {
			log.Error("SendOrderSMS", "MSG91 SMS failed, trying notification service fallback", "phone", phone, "err", err.Error())
			// Fallback to notification service
			SendSMS(phone, message, "HIGH")
			return
		}
		log.Info("SendOrderSMS", "SMS sent via MSG91", "phone", phone)
	}()
}

// sendViaMSG91 sends SMS using MSG91's Send SMS API
func sendViaMSG91(phone, message string) error {
	// MSG91 Send SMS API
	payload := map[string]interface{}{
		"sender":      msg91SenderID,
		"route":       "4", // Transactional route
		"country":     "91",
		"sms": []map[string]interface{}{
			{
				"message": message,
				"to":      []string{phone},
			},
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.msg91.com/api/v2/sendsms", bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("request error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("authkey", msg91AuthKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("MSG91 API error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("MSG91 returned status %d", resp.StatusCode)
	}
	return nil
}

// ==================== ORDER-SPECIFIC SMS TEMPLATES ====================

// SendOrderConfirmationSMS sends SMS when order is placed
func SendOrderConfirmationSMS(phone, orderNumber string, totalPaise int) {
	total := fmt.Sprintf("%.2f", float64(totalPaise)/100)
	msg := fmt.Sprintf("Order confirmed! Your order #%s for Rs.%s has been placed successfully. Track it in the app.", orderNumber, total)
	SendOrderSMS(phone, msg)
}

// SendOrderShippedSMS sends SMS when order is shipped
func SendOrderShippedSMS(phone, orderNumber, trackingID, trackingURL string) {
	msg := fmt.Sprintf("Your order #%s has been shipped! Tracking ID: %s. Track here: %s", orderNumber, trackingID, trackingURL)
	SendOrderSMS(phone, msg)
}

// SendOrderDeliveredSMS sends SMS when order is delivered
func SendOrderDeliveredSMS(phone, orderNumber string) {
	msg := fmt.Sprintf("Your order #%s has been delivered! Thank you for shopping with us. Please rate your experience.", orderNumber)
	SendOrderSMS(phone, msg)
}

// SendOrderCancelledSMS sends SMS when order is cancelled
func SendOrderCancelledSMS(phone, orderNumber string) {
	msg := fmt.Sprintf("Your order #%s has been cancelled. Refund will be processed within 5-7 business days.", orderNumber)
	SendOrderSMS(phone, msg)
}

// SendPaymentConfirmSMS sends SMS when payment is verified
func SendPaymentConfirmSMS(phone, orderNumber string, amountPaise int) {
	amount := fmt.Sprintf("%.2f", float64(amountPaise)/100)
	msg := fmt.Sprintf("Payment of Rs.%s received for order #%s. Your order is being processed.", amount, orderNumber)
	SendOrderSMS(phone, msg)
}

// ==================== LEGACY HELPERS (via notification service) ====================

func SendSMS(phone, body, priority string) {
	SendNotification("SMS", phone, map[string]string{"body": body}, priority)
}

func SendEmail(email, subject, body, priority string) {
	SendNotification("EMAIL", email, map[string]string{"subject": subject, "body": body}, priority)
}

func SendPush(token, title, body, priority string) {
	SendNotification("PUSH", token, map[string]string{"title": title, "body": body}, priority)
}
