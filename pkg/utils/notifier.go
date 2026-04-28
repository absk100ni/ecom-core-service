package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

var notificationURL = getNotifURL()

func getNotifURL() string {
	if url := os.Getenv("NOTIFICATION_SERVICE_URL"); url != "" {
		return url
	}
	return "http://localhost:9090"
}

type NotificationRequest struct {
	Type      string            `json:"type"`
	Recipient string            `json:"recipient"`
	Metadata  map[string]string `json:"metadata"`
	Priority  string            `json:"priority"`
}

// SendNotification sends a notification via the notification microservice (fire-and-forget)
func SendNotification(nType, recipient string, metadata map[string]string, priority string) {
	go func() {
		req := NotificationRequest{Type: nType, Recipient: recipient, Metadata: metadata, Priority: priority}
		data, err := json.Marshal(req)
		if err != nil { return }

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(fmt.Sprintf("%s/notification/send", notificationURL), "application/json", bytes.NewBuffer(data))
		if err != nil {
			log.Printf("⚠️ Notification service unreachable: %v (non-blocking)", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 300 {
			log.Printf("📨 Notification sent: %s → %s", nType, recipient)
		}
	}()
}

func SendSMS(phone, body, priority string) {
	SendNotification("SMS", phone, map[string]string{"body": body}, priority)
}

func SendEmail(email, subject, body, priority string) {
	SendNotification("EMAIL", email, map[string]string{"subject": subject, "body": body}, priority)
}

func SendPush(token, title, body, priority string) {
	SendNotification("PUSH", token, map[string]string{"title": title, "body": body}, priority)
}
