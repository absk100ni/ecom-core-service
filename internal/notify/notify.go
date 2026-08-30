package notify

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/logger"
	"ecom-core-service/pkg/utils"
)

var log = logger.New("NOTIFY", "EMAIL")

// Notifier holds config for email/SMS dispatch
type Notifier struct {
	cfg *config.Config
}

// New creates a Notifier
func New(cfg *config.Config) *Notifier {
	return &Notifier{cfg: cfg}
}

// OrderConfirmed fires async email + WhatsApp on order placement
func (n *Notifier) OrderConfirmed(order *models.Order, user *models.User) {
	go n.safeRun(func() {
		n.sendOrderConfirmedEmail(order, user)
		// Guests have no account — the /orders page requires login, so send them
		// to the public track page instead (order number + phone).
		orderLink := n.storeURL() + "/orders/" + order.ID
		if order.UserID == "" {
			orderLink = n.storeURL() + "/track"
		}
		n.sendWhatsAppTemplate(order.ShippingAddress.Phone, n.cfg.WhatsAppTplOrderConfirmed, []string{
			order.ShippingAddress.Name,
			order.OrderNumber,
			rupees(paidNow(order)),
			rupees(order.CODAmount),
			orderLink,
		})
	})
}

// OrderShipped fires async email + SMS + WhatsApp on shipment creation
func (n *Notifier) OrderShipped(order *models.Order, user *models.User, awb, courier, trackingURL string) {
	go n.safeRun(func() {
		n.sendOrderShippedEmail(order, user, awb, courier, trackingURL)
		// SMS via MSG91 for shipped only
		if user.Phone != "" {
			utils.SendOrderShippedSMS(user.Phone, order.OrderNumber, awb, trackingURL)
		}
		n.sendWhatsAppTemplate(order.ShippingAddress.Phone, n.cfg.WhatsAppTplOrderShipped, []string{
			order.OrderNumber,
			courier,
			awb,
			rupees(order.CODAmount),
			trackingURL,
		})
	})
}

// RefundProcessed fires async email + WhatsApp on refund success
func (n *Notifier) RefundProcessed(order *models.Order, user *models.User, amountPaise int) {
	go n.safeRun(func() {
		n.sendRefundEmail(order, user, amountPaise)
		n.sendWhatsAppTemplate(order.ShippingAddress.Phone, n.cfg.WhatsAppTplRefund, []string{
			order.OrderNumber,
			rupees(amountPaise),
		})
	})
}

// paidNow is the amount actually charged online: the advance for partial-plan
// orders, the full total otherwise.
func paidNow(order *models.Order) int {
	if order.PaymentPlan == "partial" && order.AdvanceAmount > 0 {
		return order.AdvanceAmount
	}
	return order.Total
}

// ContactReceived fires async email to admin on new contact message
func (n *Notifier) ContactReceived(name, email, subject, message string) {
	go n.safeRun(func() {
		n.sendContactNotifyEmail(name, email, subject, message)
	})
}

func (n *Notifier) safeRun(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("safeRun", "Recovered from panic in notification goroutine", "panic", fmt.Sprintf("%v", r))
		}
	}()
	fn()
}

func (n *Notifier) storeName() string {
	if n.cfg.StoreName != "" {
		return n.cfg.StoreName
	}
	return "ElectroMart"
}

func (n *Notifier) storeURL() string {
	if n.cfg.StoreURL != "" {
		return n.cfg.StoreURL
	}
	return "https://electromart.in"
}

// sendEmail dispatches via SMTP or logs in mock mode
func (n *Notifier) sendEmail(to, subject, htmlBody string) {
	if n.cfg.SMTPHost == "" {
		log.Info("sendEmail", "Mock email sent",
			"to", to,
			"subject", subject,
			"body_len", len(htmlBody),
		)
		return
	}

	from := n.cfg.SMTPFrom
	if from == "" {
		from = n.cfg.SMTPUser
	}
	fromName := n.cfg.SMTPFromName
	if fromName == "" {
		fromName = n.storeName()
	}

	headers := fmt.Sprintf("From: %s <%s>\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n",
		fromName, from, to, subject)
	msg := []byte(headers + htmlBody)

	addr := net.JoinHostPort(n.cfg.SMTPHost, n.cfg.SMTPPort)
	auth := smtp.PlainAuth("", n.cfg.SMTPUser, n.cfg.SMTPPass, n.cfg.SMTPHost)

	// Use STARTTLS
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Error("sendEmail", "SMTP dial failed", "err", err.Error(), "addr", addr)
		return
	}
	client, err := smtp.NewClient(conn, n.cfg.SMTPHost)
	if err != nil {
		log.Error("sendEmail", "SMTP client creation failed", "err", err.Error())
		conn.Close()
		return
	}
	defer client.Close()

	tlsConfig := &tls.Config{ServerName: n.cfg.SMTPHost}
	if err := client.StartTLS(tlsConfig); err != nil {
		log.Warn("sendEmail", "STARTTLS failed, continuing plain", "err", err.Error())
	}

	if err := client.Auth(auth); err != nil {
		log.Error("sendEmail", "SMTP auth failed", "err", err.Error())
		return
	}
	if err := client.Mail(from); err != nil {
		log.Error("sendEmail", "SMTP MAIL FROM failed", "err", err.Error())
		return
	}
	if err := client.Rcpt(to); err != nil {
		log.Error("sendEmail", "SMTP RCPT TO failed", "err", err.Error())
		return
	}
	w, err := client.Data()
	if err != nil {
		log.Error("sendEmail", "SMTP DATA failed", "err", err.Error())
		return
	}
	if _, err := w.Write(msg); err != nil {
		log.Error("sendEmail", "SMTP write failed", "err", err.Error())
		return
	}
	if err := w.Close(); err != nil {
		log.Error("sendEmail", "SMTP close failed", "err", err.Error())
		return
	}
	client.Quit()
	log.Info("sendEmail", "Email sent", "to", to, "subject", subject)
}

func (n *Notifier) sendOrderConfirmedEmail(order *models.Order, user *models.User) {
	email := user.Email
	if email == "" {
		log.Debug("sendOrderConfirmedEmail", "No email for user, skipping", "user_id", user.ID)
		return
	}

	itemsHTML := ""
	for _, item := range order.Items {
		itemsHTML += fmt.Sprintf(`<tr><td style="padding:8px;border-bottom:1px solid #eee">%s</td><td style="padding:8px;border-bottom:1px solid #eee;text-align:center">%d</td><td style="padding:8px;border-bottom:1px solid #eee;text-align:right">₹%.2f</td></tr>`,
			item.Name, item.Quantity, float64(item.Price*item.Quantity)/100)
	}

	codNote := ""
	if order.CODAmount > 0 {
		codNote = fmt.Sprintf(`<p style="margin:10px 0;color:#e67e22;font-weight:bold">💰 Cash to pay on delivery: ₹%.2f</p>`, float64(order.CODAmount)/100)
	}

	advanceNote := ""
	if order.AdvanceAmount > 0 && order.PaymentPlan == "partial" {
		advanceNote = fmt.Sprintf(`<p style="margin:10px 0;color:#27ae60">✅ Advance paid: ₹%.2f</p>`, float64(order.AdvanceAmount)/100)
	}

	addr := order.ShippingAddress
	addrStr := strings.Join([]string{addr.Name, addr.Line1, addr.Line2, addr.City, addr.State, addr.Pincode}, ", ")
	addrStr = strings.ReplaceAll(addrStr, ", , ", ", ")

	subject := fmt.Sprintf("Order Confirmed - %s | %s", order.OrderNumber, n.storeName())
	body := fmt.Sprintf(tplOrderConfirmed, n.storeName(), order.OrderNumber,
		itemsHTML, float64(order.Total)/100, advanceNote, codNote,
		addrStr, n.storeURL(), order.ID, n.storeName())

	n.sendEmail(email, subject, body)
}

func (n *Notifier) sendOrderShippedEmail(order *models.Order, user *models.User, awb, courier, trackingURL string) {
	email := user.Email
	if email == "" {
		return
	}

	codNote := ""
	if order.CODAmount > 0 {
		codNote = fmt.Sprintf(`<p style="margin:10px 0;color:#e67e22;font-weight:bold">💰 Please keep ₹%.2f ready for the delivery agent.</p>`, float64(order.CODAmount)/100)
	}

	subject := fmt.Sprintf("Order Shipped - %s | %s", order.OrderNumber, n.storeName())
	body := fmt.Sprintf(tplOrderShipped, n.storeName(), order.OrderNumber, courier, awb, trackingURL, trackingURL, codNote, n.storeName())
	n.sendEmail(email, subject, body)
}

func (n *Notifier) sendRefundEmail(order *models.Order, user *models.User, amountPaise int) {
	email := user.Email
	if email == "" {
		return
	}
	subject := fmt.Sprintf("Refund Processed - %s | %s", order.OrderNumber, n.storeName())
	body := fmt.Sprintf(tplRefundProcessed, n.storeName(), order.OrderNumber, float64(amountPaise)/100, n.storeName())
	n.sendEmail(email, subject, body)
}

func (n *Notifier) sendContactNotifyEmail(name, email, subject, message string) {
	notifyEmail := n.cfg.ContactNotifyEmail
	if notifyEmail == "" {
		log.Info("sendContactNotifyEmail", "CONTACT_NOTIFY_EMAIL unset, skipping", "from", email)
		return
	}
	subj := fmt.Sprintf("New Contact Message: %s | %s", subject, n.storeName())
	body := fmt.Sprintf(tplContactNotify, n.storeName(), name, email, subject, message, n.storeName())
	n.sendEmail(notifyEmail, subj, body)
}
