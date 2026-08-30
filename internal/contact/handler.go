package contact

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/notify"
	"ecom-core-service/pkg/cache"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var log = logger.New("CONTACT", "MSG")

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

type Handler struct {
	db       *mongo.Database
	cfg      *config.Config
	cache    *cache.Cache
	notifier *notify.Notifier
}

func NewHandler(db *mongo.Database, cfg *config.Config, c *cache.Cache, n *notify.Notifier) *Handler {
	return &Handler{db: db, cfg: cfg, cache: c, notifier: n}
}

type contactMessage struct {
	ID        string    `json:"id" bson:"_id,omitempty"`
	Name      string    `json:"name" bson:"name"`
	Email     string    `json:"email" bson:"email"`
	Phone     string    `json:"phone,omitempty" bson:"phone,omitempty"`
	Subject   string    `json:"subject" bson:"subject"`
	Message   string    `json:"message" bson:"message"`
	Status    string    `json:"status" bson:"status"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}

// Create handles POST /api/v1/contact — public, rate-limited
func (h *Handler) Create(c *gin.Context) {
	var req struct {
		Name    string `json:"name" binding:"required,min=2,max=100"`
		Email   string `json:"email" binding:"required"`
		Phone   string `json:"phone"`
		Subject string `json:"subject" binding:"required,min=3,max=200"`
		Message string `json:"message" binding:"required,min=10,max=5000"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": errcodes.ECtcInvalidData.Code})
		return
	}

	if !emailRegex.MatchString(req.Email) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid email address", "code": errcodes.ECtcInvalidData.Code})
		return
	}

	// Rate limit: 5 per hour per IP via Redis
	ip := c.ClientIP()
	key := "contact_rate:" + ip
	if h.cache != nil {
		allowed, _ := h.cache.RateAllow(context.Background(), key, 5, time.Hour)
		if !allowed {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many contact messages, please try later", "code": errcodes.EMidRateLimit.Code})
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now()
	msg := contactMessage{
		ID:        uuid.New().String(),
		Name:      req.Name,
		Email:     req.Email,
		Phone:     req.Phone,
		Subject:   req.Subject,
		Message:   req.Message,
		Status:    "open",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if _, err := h.db.Collection("contact_messages").InsertOne(ctx, msg); err != nil {
		log.Error("Create", "Failed to store contact message", "err", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to submit message", "code": errcodes.ECtcCreateFailed.Code})
		return
	}

	// Async notify admin
	if h.notifier != nil {
		h.notifier.ContactReceived(req.Name, req.Email, req.Subject, req.Message)
	}

	c.JSON(http.StatusCreated, gin.H{"message": "Message received. We'll get back to you soon.", "id": msg.ID})
}

// AdminList handles GET /api/v1/admin/contact-messages?status=
func (h *Handler) AdminList(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := bson.M{}
	if status := c.Query("status"); status != "" {
		filter["status"] = status
	}

	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(100)
	cursor, err := h.db.Collection("contact_messages").Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list messages", "code": errcodes.ECtcListFailed.Code})
		return
	}
	defer cursor.Close(ctx)

	results := make([]contactMessage, 0)
	if err := cursor.All(ctx, &results); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decode messages", "code": errcodes.ECtcListFailed.Code})
		return
	}
	c.JSON(http.StatusOK, gin.H{"contact_messages": results, "total": len(results)})
}

// AdminUpdate handles PUT /api/v1/admin/contact-messages/:id
func (h *Handler) AdminUpdate(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Status string `json:"status" binding:"required,oneof=open resolved"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": errcodes.ECtcInvalidData.Code})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := h.db.Collection("contact_messages").UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"status": req.Status, "updated_at": time.Now()}},
	)
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Message not found", "code": errcodes.ECtcNotFound.Code})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Contact message updated", "status": req.Status})
}
