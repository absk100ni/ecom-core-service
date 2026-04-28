// Package errcodes defines all application error codes, descriptions, and HTTP status mappings.
//
// Error code format: E{MODULE}{NUMBER}
//   - EAUTH001 → Auth module, error #001
//   - EPROD001 → Product module, error #001
//
// Each code has a human-readable description for logging and API responses.
package errcodes

import "net/http"

// ErrorInfo contains metadata for each error code
type ErrorInfo struct {
	Code        string
	Message     string // User-facing message
	Description string // Internal description for debugging
	HTTPStatus  int
}

// ==================== AUTH ERRORS (EAUTH) ====================
var (
	EAuthInvalidPhone     = ErrorInfo{"EAUTH001", "Invalid phone number", "Phone number validation failed", http.StatusBadRequest}
	EAuthOTPSendFailed    = ErrorInfo{"EAUTH002", "Failed to send OTP", "OTP delivery service error", http.StatusInternalServerError}
	EAuthOTPStoreFailed   = ErrorInfo{"EAUTH003", "OTP processing failed", "Failed to store OTP in database", http.StatusInternalServerError}
	EAuthOTPInvalid       = ErrorInfo{"EAUTH004", "Invalid or expired OTP", "OTP mismatch or TTL expired", http.StatusUnauthorized}
	EAuthOTPNotFound      = ErrorInfo{"EAUTH005", "OTP not found", "No OTP record found for phone", http.StatusBadRequest}
	EAuthTokenGenFailed   = ErrorInfo{"EAUTH006", "Authentication failed", "JWT token generation error", http.StatusInternalServerError}
	EAuthUserCreateFailed = ErrorInfo{"EAUTH007", "Account creation failed", "Failed to upsert user in database", http.StatusInternalServerError}
	EAuthTokenInvalid     = ErrorInfo{"EAUTH008", "Invalid token", "JWT parsing or validation failed", http.StatusUnauthorized}
	EAuthTokenMissing     = ErrorInfo{"EAUTH009", "Authorization required", "No Bearer token in header", http.StatusUnauthorized}
	EAuthAdminRequired    = ErrorInfo{"EAUTH010", "Admin access required", "User role is not admin", http.StatusForbidden}
)

// ==================== PRODUCT ERRORS (EPROD) ====================
var (
	EProdNotFound        = ErrorInfo{"EPROD001", "Product not found", "Product lookup returned no results", http.StatusNotFound}
	EProdCreateFailed    = ErrorInfo{"EPROD002", "Failed to create product", "Product insert error", http.StatusInternalServerError}
	EProdUpdateFailed    = ErrorInfo{"EPROD003", "Failed to update product", "Product update error", http.StatusInternalServerError}
	EProdDeleteFailed    = ErrorInfo{"EPROD004", "Failed to delete product", "Product soft-delete error", http.StatusInternalServerError}
	EProdListFailed      = ErrorInfo{"EPROD005", "Failed to list products", "Product query error", http.StatusInternalServerError}
	EProdInvalidData     = ErrorInfo{"EPROD006", "Invalid product data", "Product validation failed", http.StatusBadRequest}
	EProdSlugConflict    = ErrorInfo{"EPROD007", "Product slug already exists", "Duplicate slug detected", http.StatusConflict}
	EProdInvalidPrice    = ErrorInfo{"EPROD008", "Invalid price", "Price must be positive integer (paise)", http.StatusBadRequest}
	EProdCSVParseFailed  = ErrorInfo{"EPROD009", "CSV parse failed", "Failed to read/parse uploaded CSV", http.StatusBadRequest}
	EProdCSVExportFailed = ErrorInfo{"EPROD010", "CSV export failed", "Failed to generate CSV export", http.StatusInternalServerError}
)

// ==================== CATEGORY ERRORS (ECAT) ====================
var (
	ECatNotFound      = ErrorInfo{"ECAT001", "Category not found", "Category lookup returned no results", http.StatusNotFound}
	ECatCreateFailed  = ErrorInfo{"ECAT002", "Failed to create category", "Category insert error", http.StatusInternalServerError}
	ECatUpdateFailed  = ErrorInfo{"ECAT003", "Failed to update category", "Category update error", http.StatusInternalServerError}
	ECatDeleteFailed  = ErrorInfo{"ECAT004", "Failed to delete category", "Category delete error", http.StatusInternalServerError}
	ECatListFailed    = ErrorInfo{"ECAT005", "Failed to list categories", "Category query error", http.StatusInternalServerError}
	ECatSlugConflict  = ErrorInfo{"ECAT006", "Category slug already exists", "Duplicate category slug", http.StatusConflict}
)

// ==================== ORDER ERRORS (EORD) ====================
var (
	EOrdNotFound       = ErrorInfo{"EORD001", "Order not found", "Order lookup returned no results", http.StatusNotFound}
	EOrdCreateFailed   = ErrorInfo{"EORD002", "Failed to create order", "Order insert error", http.StatusInternalServerError}
	EOrdCartEmpty      = ErrorInfo{"EORD003", "Cart is empty", "No items in cart to checkout", http.StatusBadRequest}
	EOrdStockFailed    = ErrorInfo{"EORD004", "Stock check failed", "Insufficient stock for one or more items", http.StatusBadRequest}
	EOrdStockRollback  = ErrorInfo{"EORD005", "Stock rollback triggered", "Rolling back stock deductions after failure", http.StatusInternalServerError}
	EOrdCancelFailed   = ErrorInfo{"EORD006", "Cannot cancel order", "Order status does not allow cancellation", http.StatusBadRequest}
	EOrdInvalidStatus  = ErrorInfo{"EORD007", "Invalid order status", "Status transition not allowed", http.StatusBadRequest}
	EOrdRefundFailed   = ErrorInfo{"EORD008", "Refund processing failed", "Payment not in refundable state", http.StatusBadRequest}
	EOrdAddressInvalid = ErrorInfo{"EORD009", "Invalid shipping address", "Required address fields missing", http.StatusBadRequest}
	EOrdListFailed     = ErrorInfo{"EORD010", "Failed to list orders", "Order query error", http.StatusInternalServerError}
)

// ==================== CART ERRORS (ECART) ====================
var (
	ECartNotFound     = ErrorInfo{"ECART001", "Cart not found", "No cart found for user", http.StatusNotFound}
	ECartAddFailed    = ErrorInfo{"ECART002", "Failed to add item", "Cart item insert/update error", http.StatusInternalServerError}
	ECartRemoveFailed = ErrorInfo{"ECART003", "Failed to remove item", "Cart item removal error", http.StatusInternalServerError}
	ECartClearFailed  = ErrorInfo{"ECART004", "Failed to clear cart", "Cart clear operation error", http.StatusInternalServerError}
	ECartProdNotFound = ErrorInfo{"ECART005", "Product not available", "Product not found or inactive", http.StatusBadRequest}
	ECartStockLimit   = ErrorInfo{"ECART006", "Exceeds available stock", "Requested quantity exceeds stock", http.StatusBadRequest}
)

// ==================== PAYMENT ERRORS (EPAY) ====================
var (
	EPayCreateFailed   = ErrorInfo{"EPAY001", "Payment creation failed", "Razorpay order creation error", http.StatusInternalServerError}
	EPayVerifyFailed   = ErrorInfo{"EPAY002", "Payment verification failed", "Razorpay signature mismatch", http.StatusBadRequest}
	EPayOrderNotFound  = ErrorInfo{"EPAY003", "Order not found for payment", "Payment linked order not found", http.StatusNotFound}
	EPayWebhookFailed  = ErrorInfo{"EPAY004", "Webhook processing failed", "Payment webhook error", http.StatusInternalServerError}
	EPayAlreadyPaid    = ErrorInfo{"EPAY005", "Order already paid", "Duplicate payment attempt", http.StatusConflict}
)

// ==================== SHIPPING ERRORS (ESHIP) ====================
var (
	EShipCreateFailed = ErrorInfo{"ESHIP001", "Shipment creation failed", "Shiprocket API error", http.StatusInternalServerError}
	EShipTrackFailed  = ErrorInfo{"ESHIP002", "Tracking unavailable", "Shipment tracking lookup failed", http.StatusNotFound}
	EShipNotFound     = ErrorInfo{"ESHIP003", "Shipment not found", "No shipment record for order", http.StatusNotFound}
	EShipWebhookFailed = ErrorInfo{"ESHIP004", "Shipping webhook failed", "Webhook payload processing error", http.StatusInternalServerError}
)

// ==================== COUPON ERRORS (ECPN) ====================
var (
	ECpnNotFound      = ErrorInfo{"ECPN001", "Coupon not found", "Coupon code lookup failed", http.StatusNotFound}
	ECpnCreateFailed  = ErrorInfo{"ECPN002", "Failed to create coupon", "Coupon insert error", http.StatusInternalServerError}
	ECpnExpired       = ErrorInfo{"ECPN003", "Coupon expired", "Coupon past expiry date", http.StatusBadRequest}
	ECpnUsageLimit    = ErrorInfo{"ECPN004", "Coupon usage limit reached", "Coupon max uses exceeded", http.StatusBadRequest}
	ECpnMinOrderFail  = ErrorInfo{"ECPN005", "Minimum order not met", "Cart total below coupon minimum", http.StatusBadRequest}
	ECpnInvalidData   = ErrorInfo{"ECPN006", "Invalid coupon data", "Coupon validation failed", http.StatusBadRequest}
	ECpnDuplicate     = ErrorInfo{"ECPN007", "Coupon code already exists", "Duplicate coupon code", http.StatusConflict}
)

// ==================== WISHLIST ERRORS (EWISH) ====================
var (
	EWishAddFailed    = ErrorInfo{"EWISH001", "Failed to add to wishlist", "Wishlist update error", http.StatusInternalServerError}
	EWishRemoveFailed = ErrorInfo{"EWISH002", "Failed to remove from wishlist", "Wishlist item removal error", http.StatusInternalServerError}
	EWishGetFailed    = ErrorInfo{"EWISH003", "Failed to get wishlist", "Wishlist query error", http.StatusInternalServerError}
)

// ==================== REVIEW ERRORS (EREV) ====================
var (
	ERevCreateFailed  = ErrorInfo{"EREV001", "Failed to create review", "Review insert error", http.StatusInternalServerError}
	ERevUpdateFailed  = ErrorInfo{"EREV002", "Failed to update review", "Review update error", http.StatusInternalServerError}
	ERevDeleteFailed  = ErrorInfo{"EREV003", "Failed to delete review", "Review delete error", http.StatusInternalServerError}
	ERevNotFound      = ErrorInfo{"EREV004", "Review not found", "Review lookup returned no results", http.StatusNotFound}
	ERevDuplicate     = ErrorInfo{"EREV005", "Already reviewed", "User already reviewed this product", http.StatusConflict}
	ERevListFailed    = ErrorInfo{"EREV006", "Failed to list reviews", "Review query error", http.StatusInternalServerError}
	ERevInvalidRating = ErrorInfo{"EREV007", "Invalid rating", "Rating must be 1-5", http.StatusBadRequest}
)

// ==================== USER ERRORS (EUSR) ====================
var (
	EUsrNotFound       = ErrorInfo{"EUSR001", "User not found", "User lookup returned no results", http.StatusNotFound}
	EUsrUpdateFailed   = ErrorInfo{"EUSR002", "Profile update failed", "User update error", http.StatusInternalServerError}
	EUsrAddrNotFound   = ErrorInfo{"EUSR003", "Address not found", "Address ID not in user's address book", http.StatusNotFound}
	EUsrAddrAddFailed  = ErrorInfo{"EUSR004", "Failed to add address", "Address insert error", http.StatusInternalServerError}
	EUsrAddrDelFailed  = ErrorInfo{"EUSR005", "Failed to delete address", "Address removal error", http.StatusInternalServerError}
	EUsrAddrLimit      = ErrorInfo{"EUSR006", "Address limit reached", "Maximum 10 addresses allowed", http.StatusBadRequest}
)

// ==================== UPLOAD ERRORS (EUPLD) ====================
var (
	EUpldFailed        = ErrorInfo{"EUPLD001", "Upload failed", "File upload processing error", http.StatusInternalServerError}
	EUpldInvalidFile   = ErrorInfo{"EUPLD002", "Invalid file", "File type or size not allowed", http.StatusBadRequest}
	EUpldS3Failed      = ErrorInfo{"EUPLD003", "Storage service error", "S3 presign/upload failed", http.StatusInternalServerError}
	EUpldTooLarge      = ErrorInfo{"EUPLD004", "File too large", "Exceeds 10MB limit", http.StatusBadRequest}
)

// ==================== MIDDLEWARE ERRORS (EMID) ====================
var (
	EMidRateLimit      = ErrorInfo{"EMID001", "Too many requests", "Rate limit exceeded", http.StatusTooManyRequests}
	EMidAuthFailed     = ErrorInfo{"EMID002", "Authentication failed", "Token validation error in middleware", http.StatusUnauthorized}
)

// ==================== DATABASE ERRORS (EDB) ====================
var (
	EDBConnectFailed   = ErrorInfo{"EDB001", "Service unavailable", "MongoDB connection failed", http.StatusServiceUnavailable}
	EDBQueryFailed     = ErrorInfo{"EDB002", "Internal error", "Database query execution failed", http.StatusInternalServerError}
	EDBIndexFailed     = ErrorInfo{"EDB003", "Index creation warning", "MongoDB index creation failed", http.StatusInternalServerError}
)

// Lookup returns the ErrorInfo for quick access patterns
var Lookup = map[string]ErrorInfo{}

func init() {
	// Register all codes for lookup
	all := []ErrorInfo{
		EAuthInvalidPhone, EAuthOTPSendFailed, EAuthOTPStoreFailed, EAuthOTPInvalid,
		EAuthOTPNotFound, EAuthTokenGenFailed, EAuthUserCreateFailed, EAuthTokenInvalid,
		EAuthTokenMissing, EAuthAdminRequired,
		EProdNotFound, EProdCreateFailed, EProdUpdateFailed, EProdDeleteFailed,
		EProdListFailed, EProdInvalidData, EProdSlugConflict, EProdInvalidPrice,
		EProdCSVParseFailed, EProdCSVExportFailed,
		ECatNotFound, ECatCreateFailed, ECatUpdateFailed, ECatDeleteFailed,
		ECatListFailed, ECatSlugConflict,
		EOrdNotFound, EOrdCreateFailed, EOrdCartEmpty, EOrdStockFailed,
		EOrdStockRollback, EOrdCancelFailed, EOrdInvalidStatus, EOrdRefundFailed,
		EOrdAddressInvalid, EOrdListFailed,
		ECartNotFound, ECartAddFailed, ECartRemoveFailed, ECartClearFailed,
		ECartProdNotFound, ECartStockLimit,
		EPayCreateFailed, EPayVerifyFailed, EPayOrderNotFound, EPayWebhookFailed, EPayAlreadyPaid,
		EShipCreateFailed, EShipTrackFailed, EShipNotFound, EShipWebhookFailed,
		ECpnNotFound, ECpnCreateFailed, ECpnExpired, ECpnUsageLimit,
		ECpnMinOrderFail, ECpnInvalidData, ECpnDuplicate,
		EWishAddFailed, EWishRemoveFailed, EWishGetFailed,
		ERevCreateFailed, ERevUpdateFailed, ERevDeleteFailed, ERevNotFound,
		ERevDuplicate, ERevListFailed, ERevInvalidRating,
		EUsrNotFound, EUsrUpdateFailed, EUsrAddrNotFound, EUsrAddrAddFailed,
		EUsrAddrDelFailed, EUsrAddrLimit,
		EUpldFailed, EUpldInvalidFile, EUpldS3Failed, EUpldTooLarge,
		EMidRateLimit, EMidAuthFailed,
		EDBConnectFailed, EDBQueryFailed, EDBIndexFailed,
	}
	for _, e := range all {
		Lookup[e.Code] = e
	}
}
