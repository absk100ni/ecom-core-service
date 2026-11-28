package shipping

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
)

// Serviceability is the "check delivery by pincode" result shown on the PDP. It answers
// two questions the customer cares about: can we deliver here, and by when.
type Serviceability struct {
	Pincode      string `json:"pincode"`
	Serviceable  bool   `json:"serviceable"`
	CODAvailable bool   `json:"cod_available"`
	EstDays      int    `json:"estimated_days,omitempty"`     // transit estimate in days
	EDD          string `json:"estimated_delivery,omitempty"` // human EDD, e.g. "Tue, 01 Jul"
	Courier      string `json:"courier,omitempty"`            // cheapest/fastest courier name
	Source       string `json:"source"`                       // "cache" | "shiprocket" | "mock"
}

var pincodeRe = regexp.MustCompile(`^[1-9][0-9]{5}$`)

// serviceabilityTTL — serviceability rarely changes, so we cache hard. A pincode briefly
// showing stale data is far cheaper than calling the courier API on every PDP view.
const serviceabilityTTL = 12 * time.Hour

// CheckServiceability — GET /shipping/serviceability/:pincode (public, hit on the PDP).
//
// Read path: validate → Redis cache → courier API (Shiprocket) → mock fallback. The
// external call is never on the hot path when the cache is warm; if everything fails we
// still return a usable (conservative) promise rather than blocking the page.
func (h *Handler) CheckServiceability(c *gin.Context) {
	pincode := c.Param("pincode")
	if !pincodeRe.MatchString(pincode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errcodes.EShipInvalidPincode.Message, "code": errcodes.EShipInvalidPincode.Code})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	cacheKey := fmt.Sprintf("serviceability:%s:%s:%s:%s:%s", h.cfg.WarehousePincode, pincode, c.Query("product_id"), c.Query("variant_id"), c.DefaultQuery("quantity", "1"))

	// 1. Cache
	if cached, ok := h.cache.Get(ctx, cacheKey); ok {
		var s Serviceability
		if err := json.Unmarshal([]byte(cached), &s); err == nil {
			s.Source = "cache"
			log.Debug("SERVICEABILITY", "Cache hit", "pincode", pincode)
			c.JSON(http.StatusOK, s)
			return
		}
	}

	// 2. Resolve against actual product package data when supplied.
	rateReq := RateRequest{DeliveryPincode: pincode, PaymentType: "COD", OrderAmountPaise: 50000, CODAmountPaise: 50000, WeightGrams: 500, LengthCM: 10, WidthCM: 10, HeightCM: 10}
	if productID := c.Query("product_id"); productID != "" {
		var product models.Product
		if err := h.db.Collection("products").FindOne(ctx, bson.M{"_id": productID, "is_active": true}).Decode(&product); err == nil {
			quantity := 1
			fmt.Sscanf(c.DefaultQuery("quantity", "1"), "%d", &quantity)
			if quantity < 1 {
				quantity = 1
			}
			price := product.Price
			if variantID := c.Query("variant_id"); variantID != "" {
				for _, variant := range product.Variants {
					if variant.ID == variantID && variant.Price > 0 {
						price = variant.Price
						break
					}
				}
			}
			rateReq.OrderAmountPaise = price * quantity
			rateReq.CODAmountPaise = rateReq.OrderAmountPaise
			rateReq.WeightGrams = product.Weight * quantity
			rateReq.LengthCM = product.LengthCM
			rateReq.WidthCM = product.WidthCM
			rateReq.HeightCM = product.HeightCM * quantity
		}
	}
	s := h.resolveServiceability(rateReq)
	if s.Source == "unavailable" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Delivery availability could not be confirmed. Please try again.", "code": errcodes.EShipServiceLookupFailed.Code})
		return
	}

	// 3. Populate the human-readable EDD from the day estimate
	if s.Serviceable && s.EstDays > 0 {
		s.EDD = addBusinessDays(time.Now(), s.EstDays).Format("Mon, 02 Jan")
	}

	// 4. Cache the resolved result (best-effort)
	if payload, err := json.Marshal(s); err == nil {
		h.cache.Set(ctx, cacheKey, string(payload), serviceabilityTTL)
	}

	log.Info("SERVICEABILITY", "Resolved", "pincode", pincode, "serviceable", s.Serviceable, "days", s.EstDays, "source", s.Source)
	c.JSON(http.StatusOK, s)
}

// resolveServiceability calls Shiprocket if a token is configured, otherwise returns a
// deterministic mock so local/dev and unconfigured environments still work (mirrors the
// mock-shipment convention used in CreateShipment).
func (h *Handler) resolveServiceability(req RateRequest) Serviceability {
	pincode := req.DeliveryPincode
	// Preferred: the configured courier provider (Shipmozo when keys are set).
	if h.cfg.ShipmozoPublicKey != "" && h.cfg.ShipmozoPrivateKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		quote, err := h.provider.CheckServiceability(ctx, req)
		if err == nil {
			return Serviceability{Pincode: pincode, Serviceable: quote.Serviceable, CODAvailable: quote.CODAvailable, EstDays: quote.EstimatedDays, Courier: quote.CourierName, Source: "shipmozo"}
		} else {
			log.WarnWithCode("SERVICEABILITY", errcodes.EShipServiceLookupFailed.Code,
				"Shipmozo lookup failed, falling back", "pincode", pincode, "err", err.Error())
		}
	}
	// Legacy: Shiprocket token path (kept for provider-swap flexibility).
	if h.cfg.ShiprocketToken != "" {
		if s, err := h.shiprocketServiceability(pincode); err == nil {
			return s
		} else {
			log.WarnWithCode("SERVICEABILITY", errcodes.EShipServiceLookupFailed.Code,
				"Shiprocket lookup failed, falling back to mock", "pincode", pincode, "err", err.Error())
		}
	}
	if h.cfg.Environment == "production" {
		// Never invent a deliverable lane in production. An unavailable courier
		// check is represented as not serviceable so checkout cannot promise or
		// sell delivery it has not confirmed.
		return Serviceability{Pincode: pincode, Serviceable: false, CODAvailable: false, Source: "unavailable"}
	}
	return mockServiceability(pincode)
}

// shiprocketServiceability queries Shiprocket's courier serviceability API for the
// warehouse→destination lane and reduces the courier list to the fastest option.
func (h *Handler) shiprocketServiceability(pincode string) (Serviceability, error) {
	url := fmt.Sprintf(
		"https://apiv2.shiprocket.in/v1/external/courier/serviceability/?pickup_postcode=%s&delivery_postcode=%s&weight=0.5&cod=1",
		h.cfg.WarehousePincode, pincode,
	)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return Serviceability{}, err
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.ShiprocketToken)

	resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
	if err != nil {
		return Serviceability{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Serviceability{}, fmt.Errorf("shiprocket status %d", resp.StatusCode)
	}

	var result struct {
		Data struct {
			AvailableCourierCompanies []struct {
				CourierName   string  `json:"courier_name"`
				EstimatedDays string  `json:"estimated_delivery_days"`
				ETD           string  `json:"etd"`
				COD           int     `json:"cod"`
				Rate          float64 `json:"rate"`
			} `json:"available_courier_companies"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return Serviceability{}, err
	}

	couriers := result.Data.AvailableCourierCompanies
	if len(couriers) == 0 {
		// Valid response, but nobody delivers here.
		return Serviceability{Pincode: pincode, Serviceable: false, Source: "shiprocket"}, nil
	}

	// Pick the fastest courier (smallest estimated days).
	best := couriers[0]
	bestDays := atoiSafe(best.EstimatedDays)
	codAvailable := best.COD == 1
	for _, cc := range couriers[1:] {
		if cc.COD == 1 {
			codAvailable = true
		}
		if d := atoiSafe(cc.EstimatedDays); d > 0 && (bestDays == 0 || d < bestDays) {
			best, bestDays = cc, d
		}
	}

	return Serviceability{
		Pincode:      pincode,
		Serviceable:  true,
		CODAvailable: codAvailable,
		EstDays:      bestDays,
		Courier:      best.CourierName,
		Source:       "shiprocket",
	}, nil
}

// mockServiceability gives a deterministic, plausible answer without any external call.
// Metro-ish pincodes (starting 1–6) are faster; remote ranges slower; a tiny slice is
// marked non-serviceable so the frontend's "not serviceable" path can be exercised.
func mockServiceability(pincode string) Serviceability {
	first := pincode[0]
	if first >= '8' { // far north-east / remote ranges in this toy model
		return Serviceability{Pincode: pincode, Serviceable: false, Source: "mock"}
	}
	days := 5
	if first <= '6' {
		days = 3
	}
	return Serviceability{
		Pincode:      pincode,
		Serviceable:  true,
		CODAvailable: true,
		EstDays:      days,
		Courier:      "Mock Express",
		Source:       "mock",
	}
}

// addBusinessDays advances from `start` by n delivery days, skipping Sundays (couriers
// generally don't deliver Sundays in India). Good enough for an EDD estimate; a full
// holiday calendar is a future enhancement.
func addBusinessDays(start time.Time, n int) time.Time {
	d := start
	for added := 0; added < n; {
		d = d.AddDate(0, 0, 1)
		if d.Weekday() != time.Sunday {
			added++
		}
	}
	return d
}

func atoiSafe(s string) int {
	n := 0
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0
	}
	return n
}

// IsServiceable is the checkout-facing delivery gate. Production never falls
// back to synthetic availability when the courier cannot verify the lane.
func (h *Handler) IsServiceable(ctx context.Context, order *models.Order) (bool, error) {
	if order == nil || !pincodeRe.MatchString(order.ShippingAddress.Pincode) {
		return false, fmt.Errorf("invalid pincode")
	}
	if h.cfg.ShipmozoPublicKey != "" && h.cfg.ShipmozoPrivateKey != "" {
		quote, err := h.provider.CheckServiceability(ctx, rateRequestForOrder(order))
		return quote.Serviceable, err
	}
	if h.cfg.Environment == "production" {
		return false, fmt.Errorf("courier serviceability provider is not configured")
	}
	return mockServiceability(order.ShippingAddress.Pincode).Serviceable, nil
}

func rateRequestForOrder(order *models.Order) RateRequest {
	request := RateRequest{DeliveryPincode: order.ShippingAddress.Pincode, PaymentType: "PREPAID", OrderAmountPaise: order.Total, CODAmountPaise: order.CODAmount}
	if order.CODAmount > 0 {
		request.PaymentType = "COD"
	}
	for _, item := range order.Items {
		request.WeightGrams += item.Weight * item.Quantity
		if item.LengthCM > request.LengthCM {
			request.LengthCM = item.LengthCM
		}
		if item.WidthCM > request.WidthCM {
			request.WidthCM = item.WidthCM
		}
		request.HeightCM += item.HeightCM * item.Quantity
	}
	return request
}
