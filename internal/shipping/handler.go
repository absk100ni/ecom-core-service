package shipping

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var log = logger.New("SHIPPING", "LOGISTICS")
var _ = errcodes.EShipCreateFailed

type Handler struct {
	db  *mongo.Database
	cfg *config.Config
}

func NewHandler(db *mongo.Database, cfg *config.Config) *Handler { return &Handler{db: db, cfg: cfg} }

// CreateShipment — POST /admin/shipping/create
func (h *Handler) CreateShipment(c *gin.Context) {
	var req struct {
		OrderID string `json:"order_id" binding:"required"`
		Weight  int    `json:"weight"`
		Length  int    `json:"length"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
	}
	if err := c.ShouldBindJSON(&req); err != nil { c.JSON(http.StatusBadRequest, gin.H{"error": "order_id required"}); return }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": req.OrderID}).Decode(&order); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	if req.Weight == 0 { req.Weight = 500 }
	if req.Length == 0 { req.Length = 30 }
	if req.Width == 0 { req.Width = 25 }
	if req.Height == 0 { req.Height = 5 }

	shipment := models.Shipment{
		ID: uuid.New().String(), OrderID: order.ID, Provider: "shiprocket",
		Status: "created", Weight: req.Weight, Length: req.Length, Width: req.Width, Height: req.Height,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	// Call Shiprocket API if configured
	if h.cfg.ShiprocketToken != "" {
		srID, awb, trackURL, err := h.createShiprocketShipment(order, shipment)
		if err != nil {
			log.Error("CreateShipment", "Shiprocket API error", "err", err.Error())
		} else {
			shipment.ShipmentID = srID
			shipment.AWB = awb
			shipment.TrackingURL = trackURL
			shipment.Status = "booked"
		}
	} else {
		// Mock shipment
		shipment.ShipmentID = "MOCK-" + uuid.New().String()[:8]
		shipment.AWB = "AWB" + fmt.Sprintf("%010d", time.Now().UnixNano()%10000000000)
		shipment.TrackingURL = fmt.Sprintf("https://track.example.com/%s", shipment.AWB)
		shipment.Status = "booked"
		log.Info("CreateShipment", "Mock shipment created", "shipment_id", shipment.ShipmentID, "order_id", order.ID)
	}

	h.db.Collection("shipments").InsertOne(ctx, shipment)

	// Update order with tracking info
	h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": order.ID}, bson.M{"$set": bson.M{
		"tracking_id": shipment.AWB, "tracking_url": shipment.TrackingURL,
		"shipment_id": shipment.ShipmentID, "status": "shipped", "updated_at": time.Now(),
	}})

	c.JSON(http.StatusCreated, shipment)
}

// TrackShipment — GET /shipping/track/:orderId
func (h *Handler) Track(c *gin.Context) {
	orderID := c.Param("orderId")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var shipment models.Shipment
	if err := h.db.Collection("shipments").FindOne(ctx, bson.M{"order_id": orderID}).Decode(&shipment); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Shipment not found"})
		return
	}
	c.JSON(http.StatusOK, shipment)
}

func (h *Handler) createShiprocketShipment(order models.Order, ship models.Shipment) (string, string, string, error) {
	// Build Shiprocket API request
	items := make([]map[string]interface{}, 0)
	for _, item := range order.Items {
		items = append(items, map[string]interface{}{
			"name": item.Name, "sku": item.SKU, "units": item.Quantity,
			"selling_price": fmt.Sprintf("%.2f", float64(item.Price)/100), "discount": "0",
		})
	}

	payload := map[string]interface{}{
		"order_id":         order.OrderNumber,
		"order_date":       order.CreatedAt.Format("2006-01-02 15:04:05"),
		"billing_customer_name": order.ShippingAddress.Name,
		"billing_address":       order.ShippingAddress.Line1,
		"billing_city":          order.ShippingAddress.City,
		"billing_pincode":       order.ShippingAddress.Pincode,
		"billing_state":         order.ShippingAddress.State,
		"billing_country":       "India",
		"billing_phone":         order.ShippingAddress.Phone,
		"shipping_is_billing":   true,
		"order_items":           items,
		"payment_method":        "Prepaid",
		"sub_total":             float64(order.Total) / 100,
		"length": ship.Length, "breadth": ship.Width, "height": ship.Height, "weight": float64(ship.Weight) / 1000,
	}

	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://apiv2.shiprocket.in/v1/external/orders/create/adhoc", bytes.NewBuffer(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.cfg.ShiprocketToken)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil { return "", "", "", err }
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	shipmentID := fmt.Sprintf("%v", result["shipment_id"])
	awb := ""
	trackURL := ""
	if sid, ok := result["shipment_id"]; ok { shipmentID = fmt.Sprintf("%v", sid) }

	return shipmentID, awb, trackURL, nil
}
