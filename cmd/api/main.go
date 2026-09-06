package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ecom-core-service/internal/admin"
	"ecom-core-service/internal/auth"
	"ecom-core-service/internal/cart"
	"ecom-core-service/internal/config"
	"ecom-core-service/internal/contact"
	"ecom-core-service/internal/coupon"
	"ecom-core-service/internal/guest"
	"ecom-core-service/internal/invoice"
	"ecom-core-service/internal/middleware"
	"ecom-core-service/internal/notify"
	"ecom-core-service/internal/order"
	"ecom-core-service/internal/payment"
	"ecom-core-service/internal/product"
	"ecom-core-service/internal/returns"
	"ecom-core-service/internal/review"
	"ecom-core-service/internal/sentry"
	"ecom-core-service/internal/shipping"
	"ecom-core-service/internal/upload"
	"ecom-core-service/internal/user"
	"ecom-core-service/internal/wishlist"
	"ecom-core-service/pkg/cache"
	"ecom-core-service/pkg/logger"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var appLog = logger.New("APP", "STARTUP")

func main() {
	cfg := config.Load()

	// Initialize structured logging
	isProduction := cfg.Environment == "production"
	useJSON := cfg.LogFormat == "json" || isProduction
	useColor := !useJSON
	logger.Init(cfg.LogLevel, useJSON, useColor)

	appLog.Info("Init", "Initializing ecom-core-service v3.0", "env", cfg.Environment, "port", cfg.Port)

	// Fail fast on dangerous production misconfiguration
	if isProduction {
		if cfg.JWTSecret == "" || cfg.JWTSecret == "dev-secret-key" {
			log.Fatal("FATAL: JWT_SECRET is unset or the default in production.")
		}
		if cfg.GuestTokenSecret == "" || cfg.GuestTokenSecret == "dev-guest-secret" {
			log.Fatal("FATAL: GUEST_TOKEN_SECRET is unset or the default in production.")
		}
		if cfg.CORSOrigins == "*" {
			appLog.Warn("Init", "CORS_ORIGINS is '*' in production — set explicit origins")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		log.Fatalf("MongoDB connect failed: %v", err)
	}
	defer mongoClient.Disconnect(context.Background())
	if err := mongoClient.Ping(ctx, nil); err != nil {
		log.Fatalf("MongoDB ping failed: %v", err)
	}
	log.Println("✅ Connected to MongoDB")

	db := mongoClient.Database(cfg.MongoDB)
	ensureIndexes(db)

	// Redis cache
	appCache := cache.New(cfg.RedisAddr)
	defer appCache.Close()

	if isProduction {
		gin.SetMode(gin.ReleaseMode)
	}

	// Sentry client (no-op when DSN unset or non-production)
	sentryClient := sentry.New(cfg.SentryDSN, cfg.Environment)

	r := gin.New()
	r.Use(middleware.SentryRecoveryMiddleware(sentryClient))

	// Observability middleware (all routes)
	r.Use(middleware.RequestIDMiddleware())
	r.Use(middleware.AccessLogMiddlewareWithSentry(sentryClient))

	// CORS
	corsOrigins := strings.Split(cfg.CORSOrigins, ",")
	for i := range corsOrigins {
		corsOrigins[i] = strings.TrimSpace(corsOrigins[i])
	}
	corsConfig := cors.Config{
		AllowOrigins:     corsOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}
	if len(corsOrigins) == 1 && corsOrigins[0] == "*" {
		corsConfig.AllowCredentials = false
	}
	r.Use(cors.New(corsConfig))
	r.Use(middleware.RateLimitMiddleware(appCache, 60, time.Minute))

	// Serve local uploads in development
	if !isProduction {
		r.Static("/uploads", "/tmp/ecom-uploads")
		// Accept the PUT the browser makes to the mock presigned URL (dev-only,
		// mirrors S3's presigned PUT). Writes under /tmp/ecom-uploads.
		r.PUT("/uploads/*filepath", func(c *gin.Context) {
			rel := filepath.Clean(strings.TrimPrefix(c.Param("filepath"), "/"))
			if rel == "." || strings.HasPrefix(rel, "..") {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
				return
			}
			dst := filepath.Join("/tmp/ecom-uploads", rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "mkdir failed"})
				return
			}
			f, err := os.Create(dst)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "create failed"})
				return
			}
			defer f.Close()
			if _, err := io.Copy(f, io.LimitReader(c.Request.Body, 5<<20)); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "write failed"})
				return
			}
			c.Status(http.StatusOK)
		})
	}

	// Health check
	r.GET("/health", func(c *gin.Context) {
		hCtx, hCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer hCancel()
		if err := mongoClient.Ping(hCtx, nil); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "db": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "ecom-core-service", "version": "3.0", "time": time.Now().Format(time.RFC3339)})
	})

	// Init handlers
	authH := auth.NewHandler(db, cfg)
	productH := product.NewHandler(db, appCache)
	cartH := cart.NewHandler(db)
	orderH := order.NewHandler(db, cfg)
	paymentH := payment.NewHandler(db, cfg)
	shippingH := shipping.NewHandler(db, cfg, appCache)
	uploadH := upload.NewHandler(cfg)
	couponH := coupon.NewHandler(db)
	wishlistH := wishlist.NewHandler(db)
	reviewH := review.NewHandler(db)
	userH := user.NewHandler(db)
	adminH := admin.NewHandler(db, cfg, appCache)

	// New handlers (v3.3)
	notifier := notify.New(cfg)
	orderH.WithNotifier(notifier)
	paymentH.WithNotifier(notifier)
	shippingH.WithNotifier(notifier)
	invoiceH := invoice.NewHandler(db, cfg)
	returnsH := returns.NewHandler(db, cfg)
	contactH := contact.NewHandler(db, cfg, appCache, notifier)
	guestH := guest.NewHandler(db, cfg, orderH, paymentH)

	// Seed admin
	adminH.SeedAdmin()

	// P0-1: Start background order expiry sweeper
	order.StartExpirySweeper(db, cfg)

	api := r.Group("/api/v1")
	{
		// ==================== PUBLIC ====================
		api.POST("/auth/send-otp", authH.SendOTP)
		api.POST("/auth/verify-otp", authH.VerifyOTP)
		api.POST("/auth/google", authH.GoogleSignIn)

		api.GET("/products", productH.List)
		api.GET("/products/:id", productH.Get)
		api.GET("/categories", productH.ListCategories)
		api.GET("/categories/tree", productH.CategoryTree)
		api.GET("/search/suggest", productH.Suggest)
		api.GET("/shipping/serviceability/:pincode", shippingH.CheckServiceability)
		api.GET("/reviews/product/:productId", reviewH.ListByProduct)
		api.POST("/payment/webhook", paymentH.Webhook)
		api.POST("/shipping/webhook", shippingH.Webhook)
		api.POST("/contact", contactH.Create)

		// Coupon validation — read-only, safe for guests + authed users
		api.POST("/coupons/validate", couponH.Validate)

		// ==================== GUEST CHECKOUT (rate-limited, no auth) ====================
		guestGroup := api.Group("/guest")
		guestGroup.Use(middleware.RateLimitMiddleware(appCache, 20, time.Minute))
		{
			guestGroup.POST("/orders", guestH.CreateOrder)
			guestGroup.POST("/payment/create", guestH.CreatePayment)
			guestGroup.POST("/payment/verify", guestH.VerifyPayment)
			guestGroup.POST("/orders/:id/abandon", guestH.Abandon)
			guestGroup.GET("/orders/track", guestH.Track)
		}
	}

	// ==================== AUTHENTICATED ====================
	protected := api.Group("")
	protected.Use(middleware.AuthMiddleware(cfg.JWTSecret))
	{
		protected.GET("/cart", cartH.Get)
		protected.POST("/cart/add", cartH.AddItem)
		protected.DELETE("/cart/item/:productId", cartH.RemoveItem)
		protected.PUT("/cart/item/:productId", cartH.UpdateQuantity)
		protected.DELETE("/cart", cartH.Clear)

		protected.POST("/orders", orderH.Create)
		protected.GET("/orders", orderH.List)
		protected.GET("/orders/:id", orderH.Get)
		protected.POST("/orders/:id/cancel", orderH.Cancel)
		protected.POST("/orders/:id/abandon", orderH.Abandon)
		protected.GET("/orders/:id/tracking", shippingH.Track)
		protected.GET("/orders/:id/invoice", invoiceH.GetInvoice)
		protected.POST("/orders/:id/return", returnsH.Create)
		protected.GET("/orders/:id/return", returnsH.Get)

		protected.POST("/payment/create", paymentH.Create)
		protected.POST("/payment/verify", paymentH.Verify)

		protected.GET("/shipping/track/:orderId", shippingH.Track)
		// coupons/validate moved to public routes (guest checkout) — see above

		protected.GET("/wishlist", wishlistH.Get)
		protected.POST("/wishlist", wishlistH.Add)
		protected.DELETE("/wishlist/:productId", wishlistH.Remove)
		protected.GET("/wishlist/check/:productId", wishlistH.Check)

		protected.POST("/reviews", reviewH.Create)
		protected.GET("/reviews/my-reviews", reviewH.ListByUser)
		protected.PUT("/reviews/:id", reviewH.Update)
		protected.DELETE("/reviews/:id", reviewH.Delete)

		protected.GET("/user", userH.GetProfile)
		protected.PUT("/user", userH.UpdateProfile)
		protected.GET("/user/addresses", userH.ListAddresses)
		protected.POST("/user/addresses", userH.AddAddress)
		protected.PUT("/user/addresses/:addressId", userH.UpdateAddress)
		protected.DELETE("/user/addresses/:addressId", userH.DeleteAddress)
		protected.PUT("/user/addresses/:addressId/default", userH.SetDefaultAddress)

		// Account linking (Google user attaches phone)
		protected.POST("/users/me/link-phone", authH.LinkPhone)
		protected.POST("/users/me/verify-phone", authH.VerifyPhoneLink)
	}

	// ==================== ADMIN (dedicated auth) ====================
	// Admin login (public)
	api.POST("/admin/auth/login", adminH.Login)

	// Admin routes — require admin JWT with role=admin claim
	adminGroup := api.Group("/admin")
	adminGroup.Use(middleware.AdminAuthMiddleware(cfg.JWTSecret))
	{
		adminGroup.POST("/products", productH.Create)
		adminGroup.PUT("/products/:id", productH.Update)
		adminGroup.DELETE("/products/:id", productH.Delete)
		adminGroup.POST("/products/upload-csv", productH.BulkUploadCSV)
		adminGroup.GET("/products/export-csv", productH.ExportCSV)

		adminGroup.POST("/categories", productH.CreateCategory)
		adminGroup.PUT("/categories/:id", productH.UpdateCategory)
		adminGroup.DELETE("/categories/:id", productH.DeleteCategory)

		adminGroup.POST("/uploads/presign", uploadH.PresignedURL)
		adminGroup.POST("/upload/presigned-url", uploadH.PresignedURL) // legacy route
		adminGroup.POST("/upload/image", uploadH.DirectUpload)

		adminGroup.GET("/orders", orderH.AdminList)
		adminGroup.GET("/orders/:id", orderH.Get) // admin scope via is_admin ctx key
		adminGroup.PUT("/order/:id/status", orderH.UpdateStatus)
		adminGroup.POST("/order/:id/refund", orderH.ProcessRefund) // legacy route
		adminGroup.POST("/orders/:id/refund", orderH.ProcessRefund)
		adminGroup.POST("/orders/:id/ship", shippingH.CreateShipment)
		adminGroup.POST("/shipping/create", shippingH.CreateShipment) // legacy route

		adminGroup.POST("/coupons", couponH.Create)
		adminGroup.GET("/coupons", couponH.List)
		adminGroup.GET("/coupons/:id", couponH.Get)
		adminGroup.PUT("/coupons/:id", couponH.Update)
		adminGroup.DELETE("/coupons/:id", couponH.Delete)

		adminGroup.GET("/reviews", reviewH.AdminList)
		adminGroup.PUT("/reviews/:id/approve", reviewH.AdminApprove)

		// Refactored from inline handlers
		adminGroup.GET("/users", adminH.ListUsers)
		adminGroup.GET("/stats", adminH.Stats)
		adminGroup.GET("/analytics/revenue", revenueAnalytics(db))

		// Returns management
		adminGroup.GET("/returns", returnsH.AdminList)
		adminGroup.PUT("/returns/:id", returnsH.AdminUpdate)

		// Contact messages
		adminGroup.GET("/contact-messages", contactH.AdminList)
		adminGroup.PUT("/contact-messages/:id", contactH.AdminUpdate)

		// Invoice (admin access any order)
		adminGroup.GET("/orders/:id/invoice", invoiceH.AdminGetInvoice)
	}

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second}

	go func() {
		log.Printf("🚀 E-Commerce Core Service v3.0 starting on port %s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
	log.Println("Server stopped")
}

// ==================== MONGODB INDEXES ====================
func ensureIndexes(db *mongo.Database) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Drop the legacy unique-sparse phone index (phone_1): it collides on the
	// empty-string phones of Google-only accounts. Replaced by the partial
	// index below. Ignore errors (index may not exist on fresh databases).
	_, _ = db.Collection("users").Indexes().DropOne(ctx, "phone_1")

	indexes := []struct {
		collection string
		model      mongo.IndexModel
	}{
		// Unique on phone, but only for non-empty values: Google-only accounts
		// are created with phone:"" and must not collide with each other.
		// (sparse doesn't help — it only excludes MISSING fields, not "".)
		{"users", mongo.IndexModel{Keys: bson.D{{Key: "phone", Value: 1}}, Options: options.Index().
			SetUnique(true).
			SetName("phone_unique_nonempty").
			SetPartialFilterExpression(bson.M{"phone": bson.M{"$gt": ""}})}},
		{"users", mongo.IndexModel{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetSparse(true)}},
		{"users", mongo.IndexModel{Keys: bson.D{{Key: "google_sub", Value: 1}}, Options: options.Index().SetSparse(true)}},
		{"products", mongo.IndexModel{Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true)}},
		{"products", mongo.IndexModel{Keys: bson.D{{Key: "category", Value: 1}}}},
		{"products", mongo.IndexModel{Keys: bson.D{{Key: "is_active", Value: 1}, {Key: "created_at", Value: -1}}}},
		{"products", mongo.IndexModel{Keys: bson.D{{Key: "sku", Value: 1}}}},
		{"products", mongo.IndexModel{Keys: bson.D{{Key: "price", Value: 1}}}},
		{"products", mongo.IndexModel{Keys: bson.D{{Key: "tags", Value: 1}}}},
		{"products", mongo.IndexModel{
			Keys:    bson.D{{Key: "name", Value: "text"}, {Key: "description", Value: "text"}, {Key: "tags", Value: "text"}},
			Options: options.Index().SetName("product_text").SetWeights(bson.D{{Key: "name", Value: 10}, {Key: "tags", Value: 5}, {Key: "description", Value: 1}}),
		}},
		{"categories", mongo.IndexModel{Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true)}},
		{"categories", mongo.IndexModel{Keys: bson.D{{Key: "is_active", Value: 1}}}},
		{"categories", mongo.IndexModel{Keys: bson.D{{Key: "parent_id", Value: 1}}}},
		{"categories", mongo.IndexModel{Keys: bson.D{{Key: "path", Value: 1}}}},
		{"categories", mongo.IndexModel{Keys: bson.D{{Key: "is_active", Value: 1}, {Key: "sort_order", Value: 1}}}},
		{"products", mongo.IndexModel{Keys: bson.D{{Key: "name", Value: 1}}}},
		{"carts", mongo.IndexModel{Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetUnique(true)}},
		{"orders", mongo.IndexModel{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}}}},
		{"orders", mongo.IndexModel{Keys: bson.D{{Key: "order_number", Value: 1}}, Options: options.Index().SetUnique(true)}},
		{"orders", mongo.IndexModel{Keys: bson.D{{Key: "status", Value: 1}}}},
		{"orders", mongo.IndexModel{Keys: bson.D{{Key: "payment_status", Value: 1}}}},
		{"orders", mongo.IndexModel{Keys: bson.D{{Key: "created_at", Value: -1}}}},
		{"payments", mongo.IndexModel{Keys: bson.D{{Key: "order_id", Value: 1}}}},
		{"payments", mongo.IndexModel{Keys: bson.D{{Key: "razorpay_order_id", Value: 1}}}},
		{"shipments", mongo.IndexModel{Keys: bson.D{{Key: "order_id", Value: 1}}}},
		{"shipments", mongo.IndexModel{Keys: bson.D{{Key: "awb", Value: 1}}}},
		{"coupons", mongo.IndexModel{Keys: bson.D{{Key: "code", Value: 1}}, Options: options.Index().SetUnique(true)}},
		{"wishlists", mongo.IndexModel{Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetUnique(true)}},
		{"reviews", mongo.IndexModel{Keys: bson.D{{Key: "product_id", Value: 1}, {Key: "created_at", Value: -1}}}},
		{"reviews", mongo.IndexModel{Keys: bson.D{{Key: "user_id", Value: 1}}}},
		{"reviews", mongo.IndexModel{Keys: bson.D{{Key: "product_id", Value: 1}, {Key: "user_id", Value: 1}}, Options: options.Index().SetUnique(true)}},
		{"otps", mongo.IndexModel{Keys: bson.D{{Key: "phone", Value: 1}}}},
		{"otps", mongo.IndexModel{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(0)}},
		{"admins", mongo.IndexModel{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true)}},
		// Return requests
		{"return_requests", mongo.IndexModel{Keys: bson.D{{Key: "order_id", Value: 1}}}},
		{"return_requests", mongo.IndexModel{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}}}},
		{"return_requests", mongo.IndexModel{Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: -1}}}},
		// Contact messages
		{"contact_messages", mongo.IndexModel{Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: -1}}}},
		{"contact_messages", mongo.IndexModel{Keys: bson.D{{Key: "created_at", Value: -1}}}},
	}

	for _, idx := range indexes {
		_, err := db.Collection(idx.collection).Indexes().CreateOne(ctx, idx.model)
		if err != nil {
			log.Printf("⚠️ Index creation warning [%s]: %v", idx.collection, err)
		}
	}
	log.Println("✅ MongoDB indexes ensured")
}

// ==================== REVENUE ANALYTICS ====================
func revenueAnalytics(db *mongo.Database) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		thirtyDaysAgo := time.Now().AddDate(0, 0, -30)

		pipeline := mongo.Pipeline{{{Key: "$facet", Value: bson.M{
			"paid_summary": []bson.M{
				{"$match": bson.M{"payment_status": "paid"}},
				{"$group": bson.M{"_id": nil, "revenue": bson.M{"$sum": "$total"}, "orders": bson.M{"$sum": 1}, "items": bson.M{"$sum": bson.M{"$size": bson.M{"$ifNull": bson.A{"$items", bson.A{}}}}}}},
			},
			"daily": []bson.M{
				{"$match": bson.M{"payment_status": "paid", "created_at": bson.M{"$gte": thirtyDaysAgo}}},
				{"$group": bson.M{"_id": bson.M{"$dateToString": bson.M{"format": "%Y-%m-%d", "date": "$created_at"}}, "revenue": bson.M{"$sum": "$total"}}},
			},
			"status_breakdown": []bson.M{
				{"$group": bson.M{"_id": "$status", "count": bson.M{"$sum": 1}}},
			},
			"payment_breakdown": []bson.M{
				{"$group": bson.M{"_id": "$payment_status", "count": bson.M{"$sum": 1}}},
			},
			"top_products": []bson.M{
				{"$match": bson.M{"payment_status": "paid"}},
				{"$unwind": "$items"},
				{"$match": bson.M{"items.name": bson.M{"$exists": true, "$ne": ""}}},
				{"$group": bson.M{"_id": "$items.name", "quantity": bson.M{"$sum": bson.M{"$ifNull": bson.A{"$items.quantity", 1}}}}},
				{"$sort": bson.M{"quantity": -1}},
				{"$limit": 10},
			},
		}}}}

		cursor, err := db.Collection("orders").Aggregate(ctx, pipeline)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to compute analytics"})
			return
		}
		defer cursor.Close(ctx)

		var facet []struct {
			PaidSummary []struct {
				Revenue int64 `bson:"revenue"`
				Orders  int64 `bson:"orders"`
				Items   int64 `bson:"items"`
			} `bson:"paid_summary"`
			Daily []struct {
				Day     string `bson:"_id"`
				Revenue int64  `bson:"revenue"`
			} `bson:"daily"`
			StatusBreakdown []struct {
				Key   string `bson:"_id"`
				Count int    `bson:"count"`
			} `bson:"status_breakdown"`
			PaymentBreakdown []struct {
				Key   string `bson:"_id"`
				Count int    `bson:"count"`
			} `bson:"payment_breakdown"`
			TopProducts []struct {
				Name     string `bson:"_id"`
				Quantity int    `bson:"quantity"`
			} `bson:"top_products"`
		}
		if err := cursor.All(ctx, &facet); err != nil || len(facet) == 0 {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read analytics"})
			return
		}
		f := facet[0]

		totalRevenue, totalOrders, totalItems := 0, 0, 0
		if len(f.PaidSummary) > 0 {
			totalRevenue = int(f.PaidSummary[0].Revenue)
			totalOrders = int(f.PaidSummary[0].Orders)
			totalItems = int(f.PaidSummary[0].Items)
		}

		type dayRevenue struct {
			Date    string `json:"date"`
			Revenue int    `json:"revenue"`
		}
		revByDay := make(map[string]int, len(f.Daily))
		for _, d := range f.Daily {
			revByDay[d.Day] = int(d.Revenue)
		}
		dailyArr := make([]dayRevenue, 0)
		for d := thirtyDaysAgo; d.Before(time.Now().AddDate(0, 0, 1)); d = d.AddDate(0, 0, 1) {
			day := d.Format("2006-01-02")
			dailyArr = append(dailyArr, dayRevenue{Date: day, Revenue: revByDay[day]})
		}

		statusBreakdown := map[string]int{}
		for _, s := range f.StatusBreakdown {
			statusBreakdown[s.Key] = s.Count
		}
		paymentBreakdown := map[string]int{}
		for _, p := range f.PaymentBreakdown {
			paymentBreakdown[p.Key] = p.Count
		}

		type productSales struct {
			Name     string `json:"name"`
			Quantity int    `json:"quantity"`
		}
		topArr := make([]productSales, 0, len(f.TopProducts))
		for _, t := range f.TopProducts {
			topArr = append(topArr, productSales{Name: t.Name, Quantity: t.Quantity})
		}

		avgOrderValue := 0
		if totalOrders > 0 {
			avgOrderValue = totalRevenue / totalOrders
		}

		c.JSON(http.StatusOK, gin.H{
			"total_revenue": totalRevenue, "total_paid_orders": totalOrders,
			"total_items_sold": totalItems, "avg_order_value": avgOrderValue,
			"daily_revenue": dailyArr, "status_breakdown": statusBreakdown,
			"payment_breakdown": paymentBreakdown, "top_products": topArr,
		})
	}
}
