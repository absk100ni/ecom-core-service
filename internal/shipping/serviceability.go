package shipping

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"ecom-core-service/pkg/errcodes"

	"github.com/gin-gonic/gin"
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

	cacheKey := fmt.Sprintf("serviceability:%s:%s", h.cfg.WarehousePincode, pincode)

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

	// 2. Resolve (Shiprocket when configured, else mock)
	s := h.resolveServiceability(pincode)

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
func (h *Handler) resolveServiceability(pincode string) Serviceability {
	// Preferred: the configured courier provider (Shipmozo when keys are set).
	if h.cfg.ShipmozoPublicKey != "" && h.cfg.ShipmozoPrivateKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if _, ok, err := h.provider.CheckServiceability(ctx, pincode, 0); err == nil {
			return Serviceability{
				Pincode:      pincode,
				Serviceable:  ok,
				CODAvailable: ok, // Shipmozo's endpoint doesn't split COD; refine if their response includes it
				EstDays:      3,  // provider response doesn't carry EDD; conservative default
				Source:       "shipmozo",
			}
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
