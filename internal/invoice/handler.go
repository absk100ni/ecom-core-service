package invoice

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
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

func invoiceEligible(order *models.Order) error {
	if order.PaymentStatus != "paid" {
		return fmt.Errorf("invoice available only after payment is confirmed")
	}
	if order.PaymentPlan == "partial" && order.CODAmount > 0 && !order.CODCollected {
		return fmt.Errorf("final invoice is available only after the COD balance is collected")
	}
	return nil
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

	if eligibilityErr := invoiceEligible(&order); eligibilityErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": eligibilityErr.Error(), "code": errcodes.EInvNotReady.Code})
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

	if eligibilityErr := invoiceEligible(&order); eligibilityErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": eligibilityErr.Error(), "code": errcodes.EInvNotReady.Code})
		return
	}

	h.serveInvoice(c, &order)
}

func (h *Handler) serveInvoice(c *gin.Context, order *models.Order) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if h.cfg.BusinessLegalName == "" || h.cfg.BusinessAddress == "" || h.cfg.BusinessGSTIN == "" || h.cfg.BusinessState == "" || h.cfg.BusinessStateCode == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Business tax identity is incomplete; invoice cannot be issued", "code": errcodes.EInvNotReady.Code})
		return
	}

	// Validate product tax data before allocating an invoice number.
	productIDs := make([]string, 0, len(order.Items))
	for _, item := range order.Items {
		productIDs = append(productIDs, item.ProductID)
	}
	prodMap := h.fetchProducts(ctx, productIDs)
	lines, lineErr := h.computeInvoiceLines(order, prodMap)
	if lineErr != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": lineErr.Error(), "code": errcodes.EInvNotReady.Code})
		return
	}

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

	// Store only if no invoice has been assigned. Concurrent callers may consume
	// extra counter values, but exactly one number can win for an order.
	updateResult, err := h.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": order.ID, "$or": []bson.M{{"invoice_number": ""}, {"invoice_number": bson.M{"$exists": false}}}},
		bson.M{"$set": bson.M{"invoice_number": invNum, "invoice_date": invDate, "updated_at": now}},
	)
	if err != nil {
		return "", nil, fmt.Errorf("order update failed: %w", err)
	}
	if updateResult.MatchedCount == 0 {
		var winner struct {
			InvoiceNumber string     `bson:"invoice_number"`
			InvoiceDate   *time.Time `bson:"invoice_date"`
		}
		if readErr := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&winner); readErr != nil {
			return "", nil, fmt.Errorf("invoice assignment lost race and winner could not be read: %w", readErr)
		}
		if winner.InvoiceNumber == "" || winner.InvoiceDate == nil {
			return "", nil, fmt.Errorf("invoice assignment lost race without a completed winner")
		}
		return winner.InvoiceNumber, winner.InvoiceDate, nil
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
	UnitPrice    int // paise before line discount
	Discount     int // paise allocated proportionally from order discount
	TaxableValue int // paise
	GSTRate      int // percent
	GSTAmount    int // paise
	LineTotal    int // paise (inclusive, after discount)
}

func (h *Handler) computeInvoiceLines(order *models.Order, prodMap map[string]*models.Product) ([]InvoiceLine, error) {
	if len(order.Items) == 0 {
		return nil, fmt.Errorf("invoice requires at least one order item")
	}

	grossTotals := make([]int, len(order.Items))
	grossSubtotal := 0
	for i, item := range order.Items {
		if item.Quantity <= 0 || item.Price <= 0 {
			return nil, fmt.Errorf("invalid quantity or price for %s", item.Name)
		}
		grossTotals[i] = item.Price * item.Quantity
		grossSubtotal += grossTotals[i]
	}
	if grossSubtotal != order.Subtotal {
		return nil, fmt.Errorf("invoice subtotal mismatch: items total %d, order subtotal %d", grossSubtotal, order.Subtotal)
	}
	if order.Discount < 0 || order.Discount > grossSubtotal {
		return nil, fmt.Errorf("invoice discount is outside the merchandise subtotal")
	}

	// Allocate the order discount proportionally using the largest-remainder
	// method. This keeps every paise accounted for while avoiding tax on the
	// pre-discount merchandise value.
	discounts := make([]int, len(order.Items))
	type remainder struct {
		index int
		value int64
	}
	remainders := make([]remainder, len(order.Items))
	allocated := 0
	for i, gross := range grossTotals {
		product := int64(order.Discount) * int64(gross)
		discounts[i] = int(product / int64(grossSubtotal))
		allocated += discounts[i]
		remainders[i] = remainder{index: i, value: product % int64(grossSubtotal)}
	}
	sort.SliceStable(remainders, func(i, j int) bool { return remainders[i].value > remainders[j].value })
	for i := 0; i < order.Discount-allocated; i++ {
		discounts[remainders[i].index]++
	}

	lines := make([]InvoiceLine, 0, len(order.Items)+1)
	invoiceTotal := 0
	for i, item := range order.Items {
		product, ok := prodMap[item.ProductID]
		if !ok {
			return nil, fmt.Errorf("tax data is unavailable for product %s", item.ProductID)
		}
		if product.GSTPercent <= 0 || product.GSTPercent > 100 || product.HSNCode == "" {
			return nil, fmt.Errorf("GST rate and HSN code are required for %s", item.Name)
		}
		lineTotal := grossTotals[i] - discounts[i]
		taxable := GSTInclusiveTaxable(lineTotal, product.GSTPercent)
		gst := lineTotal - taxable
		lines = append(lines, InvoiceLine{
			Description: item.Name, HSN: product.HSNCode, Qty: item.Quantity,
			UnitPrice: item.Price, Discount: discounts[i], TaxableValue: taxable,
			GSTRate: product.GSTPercent, GSTAmount: gst, LineTotal: lineTotal,
		})
		invoiceTotal += lineTotal
	}

	if order.ShippingCost > 0 {
		if h.cfg == nil || h.cfg.ShippingGSTPercent <= 0 || h.cfg.ShippingGSTPercent > 100 || h.cfg.ShippingHSNCode == "" {
			return nil, fmt.Errorf("shipping GST percentage and HSN/SAC code are required when shipping is charged")
		}
		taxable := GSTInclusiveTaxable(order.ShippingCost, h.cfg.ShippingGSTPercent)
		lines = append(lines, InvoiceLine{
			Description: "Shipping", HSN: h.cfg.ShippingHSNCode, Qty: 1,
			UnitPrice: order.ShippingCost, TaxableValue: taxable,
			GSTRate: h.cfg.ShippingGSTPercent, GSTAmount: order.ShippingCost - taxable,
			LineTotal: order.ShippingCost,
		})
		invoiceTotal += order.ShippingCost
	}
	if invoiceTotal != order.Total {
		return nil, fmt.Errorf("invoice total mismatch: computed %d, order total %d", invoiceTotal, order.Total)
	}
	return lines, nil
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
