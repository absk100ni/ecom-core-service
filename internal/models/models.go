package models

import "time"

// ==================== USER ====================
type User struct {
	ID        string    `json:"id" bson:"_id,omitempty"`
	Phone     string    `json:"phone" bson:"phone"`
	Email     string    `json:"email,omitempty" bson:"email,omitempty"`
	Name      string    `json:"name,omitempty" bson:"name,omitempty"`
	IsAdmin   bool      `json:"is_admin" bson:"is_admin"`
	Addresses []Address `json:"addresses,omitempty" bson:"addresses,omitempty"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}

type OTP struct {
	ID        string    `json:"id" bson:"_id,omitempty"`
	Phone     string    `json:"phone" bson:"phone"`
	Code      string    `json:"code" bson:"code"`
	ExpiresAt time.Time `json:"expires_at" bson:"expires_at"`
	Verified  bool      `json:"verified" bson:"verified"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
}

// ==================== PRODUCT ====================
type Product struct {
	ID          string    `json:"id" bson:"_id,omitempty"`
	Name        string    `json:"name" bson:"name" binding:"required"`
	Slug        string    `json:"slug" bson:"slug"`
	Description string    `json:"description" bson:"description"`
	Category    string    `json:"category" bson:"category" binding:"required"`
	Price       int       `json:"price" bson:"price" binding:"required,gt=0"` // in paise
	CompareAt   int       `json:"compare_at_price,omitempty" bson:"compare_at_price,omitempty"`
	Images      []string  `json:"images" bson:"images"`
	Thumbnail   string    `json:"thumbnail" bson:"thumbnail"`
	Variants    []Variant `json:"variants" bson:"variants"`
	Tags        []string  `json:"tags,omitempty" bson:"tags,omitempty"`
	IsActive    bool      `json:"is_active" bson:"is_active"`
	Stock       int       `json:"stock" bson:"stock" binding:"gte=0"`
	SKU         string    `json:"sku" bson:"sku"`
	Weight      int       `json:"weight,omitempty" bson:"weight,omitempty"` // grams
	CreatedAt   time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" bson:"updated_at"`
}

type Variant struct {
	ID    string `json:"id" bson:"id"`
	Name  string `json:"name" bson:"name"`  // e.g. "Size", "Color"
	Value string `json:"value" bson:"value"` // e.g. "XL", "Black"
	Stock int    `json:"stock" bson:"stock"`
	Price int    `json:"price,omitempty" bson:"price,omitempty"` // override price
}

type Category struct {
	ID       string    `json:"id" bson:"_id,omitempty"`
	Name     string    `json:"name" bson:"name" binding:"required"`
	Slug     string    `json:"slug" bson:"slug"`
	ParentID string    `json:"parent_id,omitempty" bson:"parent_id,omitempty"`
	Image    string    `json:"image,omitempty" bson:"image,omitempty"`
	IsActive bool      `json:"is_active" bson:"is_active"`
	SortOrder int      `json:"sort_order" bson:"sort_order"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}

// ==================== CART ====================
type Cart struct {
	ID        string     `json:"id" bson:"_id,omitempty"`
	UserID    string     `json:"user_id" bson:"user_id"`
	Items     []CartItem `json:"items" bson:"items"`
	CreatedAt time.Time  `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time  `json:"updated_at" bson:"updated_at"`
}

type CartItem struct {
	ProductID string `json:"product_id" bson:"product_id"`
	VariantID string `json:"variant_id,omitempty" bson:"variant_id,omitempty"`
	Name      string `json:"name" bson:"name"`
	Price     int    `json:"price" bson:"price"`
	Quantity  int    `json:"quantity" bson:"quantity"`
	Image     string `json:"image,omitempty" bson:"image,omitempty"`
}

// ==================== ORDER ====================
type Order struct {
	ID              string      `json:"id" bson:"_id,omitempty"`
	UserID          string      `json:"user_id" bson:"user_id"`
	OrderNumber     string      `json:"order_number" bson:"order_number"`
	Items           []OrderItem `json:"items" bson:"items"`
	Subtotal        int         `json:"subtotal" bson:"subtotal"`
	ShippingCost    int         `json:"shipping_cost" bson:"shipping_cost"`
	Discount        int         `json:"discount" bson:"discount"`
	Total           int         `json:"total" bson:"total"` // in paise
	CouponCode      string      `json:"coupon_code,omitempty" bson:"coupon_code,omitempty"`
	Status          string      `json:"status" bson:"status"`
	PaymentStatus   string      `json:"payment_status" bson:"payment_status"`
	PaymentMethod   string      `json:"payment_method,omitempty" bson:"payment_method,omitempty"`
	PaymentID       string      `json:"payment_id,omitempty" bson:"payment_id,omitempty"`
	ShippingAddress Address     `json:"shipping_address" bson:"shipping_address"`
	BillingAddress  Address     `json:"billing_address,omitempty" bson:"billing_address,omitempty"`
	TrackingID      string      `json:"tracking_id,omitempty" bson:"tracking_id,omitempty"`
	TrackingURL     string      `json:"tracking_url,omitempty" bson:"tracking_url,omitempty"`
	ShipmentID      string      `json:"shipment_id,omitempty" bson:"shipment_id,omitempty"`
	Notes           string      `json:"notes,omitempty" bson:"notes,omitempty"`
	CancelReason    string      `json:"cancel_reason,omitempty" bson:"cancel_reason,omitempty"`
	CancelledAt     *time.Time  `json:"cancelled_at,omitempty" bson:"cancelled_at,omitempty"`
	RefundID        string      `json:"refund_id,omitempty" bson:"refund_id,omitempty"`
	RefundAmount    int         `json:"refund_amount,omitempty" bson:"refund_amount,omitempty"`
	RefundedAt      *time.Time  `json:"refunded_at,omitempty" bson:"refunded_at,omitempty"`
	CreatedAt       time.Time   `json:"created_at" bson:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at" bson:"updated_at"`
}

type OrderItem struct {
	ProductID string `json:"product_id" bson:"product_id"`
	VariantID string `json:"variant_id,omitempty" bson:"variant_id,omitempty"`
	Name      string `json:"name" bson:"name"`
	SKU       string `json:"sku" bson:"sku"`
	Price     int    `json:"price" bson:"price"`
	Quantity  int    `json:"quantity" bson:"quantity"`
	Image     string `json:"image,omitempty" bson:"image,omitempty"`
}

type Address struct {
	ID      string `json:"id,omitempty" bson:"id,omitempty"`
	Label   string `json:"label,omitempty" bson:"label,omitempty"` // Home, Office, etc.
	Name    string `json:"name" bson:"name"`
	Line1   string `json:"line1" bson:"line1"`
	Line2   string `json:"line2,omitempty" bson:"line2,omitempty"`
	City    string `json:"city" bson:"city"`
	State   string `json:"state" bson:"state"`
	Pincode string `json:"pincode" bson:"pincode"`
	Country string `json:"country,omitempty" bson:"country,omitempty"`
	Phone   string `json:"phone" bson:"phone"`
	IsDefault bool `json:"is_default,omitempty" bson:"is_default,omitempty"`
}

// ==================== PAYMENT ====================
type Payment struct {
	ID                string    `json:"id" bson:"_id,omitempty"`
	OrderID           string    `json:"order_id" bson:"order_id"`
	UserID            string    `json:"user_id" bson:"user_id"`
	RazorpayOrderID   string    `json:"razorpay_order_id" bson:"razorpay_order_id"`
	RazorpayPaymentID string    `json:"razorpay_payment_id,omitempty" bson:"razorpay_payment_id,omitempty"`
	RazorpaySignature string    `json:"razorpay_signature,omitempty" bson:"razorpay_signature,omitempty"`
	Amount            int       `json:"amount" bson:"amount"`
	Currency          string    `json:"currency" bson:"currency"`
	Method            string    `json:"method,omitempty" bson:"method,omitempty"`
	Status            string    `json:"status" bson:"status"`
	CreatedAt         time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt         time.Time `json:"updated_at" bson:"updated_at"`
}

// ==================== SHIPPING ====================
type Shipment struct {
	ID             string    `json:"id" bson:"_id,omitempty"`
	OrderID        string    `json:"order_id" bson:"order_id"`
	Provider       string    `json:"provider" bson:"provider"` // shiprocket, delhivery
	ShipmentID     string    `json:"shipment_id" bson:"shipment_id"`
	AWB            string    `json:"awb,omitempty" bson:"awb,omitempty"`
	TrackingURL    string    `json:"tracking_url,omitempty" bson:"tracking_url,omitempty"`
	Status         string    `json:"status" bson:"status"`
	EstDelivery    string    `json:"est_delivery,omitempty" bson:"est_delivery,omitempty"`
	Weight         int       `json:"weight" bson:"weight"`
	Length         int       `json:"length" bson:"length"`
	Width          int       `json:"width" bson:"width"`
	Height         int       `json:"height" bson:"height"`
	CreatedAt      time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt      time.Time `json:"updated_at" bson:"updated_at"`
}

// ==================== INVENTORY ====================
type InventoryItem struct {
	ID        string    `json:"id" bson:"_id,omitempty"`
	ProductID string    `json:"product_id" bson:"product_id"`
	VariantID string    `json:"variant_id,omitempty" bson:"variant_id,omitempty"`
	SKU       string    `json:"sku" bson:"sku"`
	Stock     int       `json:"stock" bson:"stock"`
	Reserved  int       `json:"reserved" bson:"reserved"`
	Warehouse string    `json:"warehouse,omitempty" bson:"warehouse,omitempty"`
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}

// ==================== COUPON ====================
type Coupon struct {
	ID           string    `json:"id" bson:"_id,omitempty"`
	Code         string    `json:"code" bson:"code" binding:"required"`
	Type         string    `json:"type" bson:"type" binding:"required"` // percentage, fixed
	Value        int       `json:"value" bson:"value" binding:"required,gt=0"`
	MinOrder     int       `json:"min_order" bson:"min_order"`
	MaxDiscount  int       `json:"max_discount,omitempty" bson:"max_discount,omitempty"`
	UsageLimit   int       `json:"usage_limit" bson:"usage_limit"`
	UsedCount    int       `json:"used_count" bson:"used_count"`
	IsActive     bool      `json:"is_active" bson:"is_active"`
	Description  string    `json:"description,omitempty" bson:"description,omitempty"`
	ExpiresAt    time.Time `json:"expires_at" bson:"expires_at"`
	CreatedAt    time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" bson:"updated_at"`
}

// ==================== WISHLIST ====================
type Wishlist struct {
	ID        string         `json:"id" bson:"_id,omitempty"`
	UserID    string         `json:"user_id" bson:"user_id"`
	Items     []WishlistItem `json:"items" bson:"items"`
	CreatedAt time.Time      `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time      `json:"updated_at" bson:"updated_at"`
}

type WishlistItem struct {
	ProductID string    `json:"product_id" bson:"product_id"`
	AddedAt   time.Time `json:"added_at" bson:"added_at"`
}

// ==================== REVIEWS ====================
type Review struct {
	ID        string    `json:"id" bson:"_id,omitempty"`
	ProductID string    `json:"product_id" bson:"product_id" binding:"required"`
	UserID    string    `json:"user_id" bson:"user_id"`
	UserName  string    `json:"user_name" bson:"user_name"`
	UserPhone string    `json:"user_phone" bson:"user_phone"`
	OrderID   string    `json:"order_id,omitempty" bson:"order_id,omitempty"`
	Rating    int       `json:"rating" bson:"rating" binding:"required,gte=1,lte=5"`
	Title     string    `json:"title,omitempty" bson:"title,omitempty"`
	Comment   string    `json:"comment,omitempty" bson:"comment,omitempty"`
	Images    []string  `json:"images,omitempty" bson:"images,omitempty"`
	IsVerified bool     `json:"is_verified" bson:"is_verified"` // verified purchase
	IsApproved bool     `json:"is_approved" bson:"is_approved"` // admin moderation
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}

// ==================== UPLOAD ====================
type UploadResponse struct {
	UploadURL string `json:"upload_url"`
	PublicURL string `json:"public_url"`
	Key       string `json:"key"`
	ExpiresIn int    `json:"expires_in"`
}
