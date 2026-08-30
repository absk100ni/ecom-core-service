package invoice

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var log = logger.New("INVOICE", "GST")

type Handler struct {
	db  *mongo.Database
	cfg *config.Config
}

func NewHandler(db *mongo.Database, cfg *config.Config) *Handler {
	return &Handler{db: db, cfg: cfg}
}

// GetInvoice serves the GST invoice HTML for a customer's own order
func (h *Handler) GetInvoice(c *gin.Context) {
	userID := c.GetString("user_id")
	orderID := c.Param("id")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var order models.Order
	err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}
	if order.UserID != userID {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}

	if order.PaymentStatus != "paid" && order.AdvanceAmount == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invoice available only for paid orders", "code": errcodes.EInvNotReady.Code})
		return
	}

	h.serveInvoice(c, &order)
}

// AdminGetInvoice serves the GST invoice for admin
func (h *Handler) AdminGetInvoice(c *gin.Context) {
	orderID := c.Param("id")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var order models.Order
	err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}

	if order.PaymentStatus != "paid" && order.AdvanceAmount == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invoice available only for paid orders", "code": errcodes.EInvNotReady.Code})
		return
	}

	h.serveInvoice(c, &order)
}

func (h *Handler) serveInvoice(c *gin.Context, order *models.Order) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Assign invoice number if not already set (idempotent)
	if order.InvoiceNumber == "" {
		invNum, invDate, err := h.assignInvoiceNumber(ctx, order)
		if err != nil {
			log.Error("serveInvoice", "Failed to assign invoice number", "order_id", order.ID, "err", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Invoice generation failed", "code": errcodes.EInvGenFailed.Code})
			return
		}
		order.InvoiceNumber = invNum
		order.InvoiceDate = invDate
	}

	// Fetch products for HSN codes and GST rates
	productIDs := make([]string, 0, len(order.Items))
	for _, item := range order.Items {
		productIDs = append(productIDs, item.ProductID)
	}
	prodMap := h.fetchProducts(ctx, productIDs)

	// Compute tax lines
	lines := h.computeInvoiceLines(order, prodMap)

	// Determine intra vs inter state
	buyerState := order.ShippingAddress.State
	isIntra := h.isIntraState(buyerState)

	html := h.renderInvoiceHTML(order, lines, isIntra)
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, html)
}

// assignInvoiceNumber atomically generates INV-<FY>-<6digit> and stores it on the order
func (h *Handler) assignInvoiceNumber(ctx context.Context, order *models.Order) (string, *time.Time, error) {
	now := time.Now()
	fy := fiscalYear(now)

	// Atomic increment on counters collection
	var result struct {
		Seq int `bson:"seq"`
	}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	err := h.db.Collection("counters").FindOneAndUpdate(ctx,
		bson.M{"_id": fmt.Sprintf("invoice_%s", fy)},
		bson.M{"$inc": bson.M{"seq": 1}},
		opts,
	).Decode(&result)
	if err != nil {
		return "", nil, fmt.Errorf("counter increment failed: %w", err)
	}

	invNum := fmt.Sprintf("INV-%s-%06d", fy, result.Seq)
	invDate := now

	// Store on order
	_, err = h.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": order.ID},
		bson.M{"$set": bson.M{"invoice_number": invNum, "invoice_date": invDate, "updated_at": now}},
	)
	if err != nil {
		return "", nil, fmt.Errorf("order update failed: %w", err)
	}

	return invNum, &invDate, nil
}

// fiscalYear returns FY string like "2526" for April 2025 – March 2026
func fiscalYear(t time.Time) string {
	year := t.Year()
	if t.Month() < 4 {
		year--
	}
	return fmt.Sprintf("%02d%02d", year%100, (year+1)%100)
}

func (h *Handler) fetchProducts(ctx context.Context, ids []string) map[string]*models.Product {
	m := make(map[string]*models.Product, len(ids))
	if len(ids) == 0 {
		return m
	}
	cursor, err := h.db.Collection("products").Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return m
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var p models.Product
		if err := cursor.Decode(&p); err == nil {
			m[p.ID] = &p
		}
	}
	return m
}

type InvoiceLine struct {
	Description  string
	HSN          string
	Qty          int
	UnitPrice    int // paise
	TaxableValue int // paise
	GSTRate      int // percent
	GSTAmount    int // paise
	LineTotal    int // paise (inclusive)
}

func (h *Handler) computeInvoiceLines(order *models.Order, prodMap map[string]*models.Product) []InvoiceLine {
	defaultRate := h.cfg.DefaultGSTPercent
	if defaultRate == 0 {
		defaultRate = 18
	}
	defaultHSN := h.cfg.DefaultHSNCode

	lines := make([]InvoiceLine, 0, len(order.Items))
	for _, item := range order.Items {
		rate := defaultRate
		hsn := defaultHSN
		if p, ok := prodMap[item.ProductID]; ok {
			if p.GSTPercent > 0 {
				rate = p.GSTPercent
			}
			if p.HSNCode != "" {
				hsn = p.HSNCode
			}
		}
		lineTotal := item.Price * item.Quantity // GST-inclusive paise
		taxable := GSTInclusiveTaxable(lineTotal, rate)
		gst := lineTotal - taxable

		lines = append(lines, InvoiceLine{
			Description:  item.Name,
			HSN:          hsn,
			Qty:          item.Quantity,
			UnitPrice:    item.Price,
			TaxableValue: taxable,
			GSTRate:      rate,
			GSTAmount:    gst,
			LineTotal:    lineTotal,
		})
	}
	return lines
}

// GSTInclusiveTaxable extracts taxable amount from GST-inclusive total
// taxable = round(total * 100 / (100 + rate))
func GSTInclusiveTaxable(totalPaise, ratePercent int) int {
	return int(math.Round(float64(totalPaise) * 100 / float64(100+ratePercent)))
}

func (h *Handler) isIntraState(buyerState string) bool {
	return equalsIgnoreCase(buyerState, h.cfg.BusinessState)
}

func equalsIgnoreCase(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}
