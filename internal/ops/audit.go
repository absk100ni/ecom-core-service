package ops

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type AuditEvent struct {
	ID                string    `json:"id" bson:"_id"`
	Timestamp         time.Time `json:"timestamp" bson:"timestamp"`
	ActorAdminID      string    `json:"actor_admin_id" bson:"actor_admin_id"`
	ActorEmail        string    `json:"actor_email" bson:"actor_email"`
	RequestID         string    `json:"request_id" bson:"request_id"`
	Action            string    `json:"action" bson:"action"`
	ResourceType      string    `json:"resource_type" bson:"resource_type"`
	ResourceID        string    `json:"resource_id,omitempty" bson:"resource_id,omitempty"`
	PreviousState     bson.M    `json:"previous_state,omitempty" bson:"previous_state,omitempty"`
	NewState          bson.M    `json:"new_state,omitempty" bson:"new_state,omitempty"`
	Outcome           string    `json:"outcome" bson:"outcome"`
	HTTPStatus        int       `json:"http_status" bson:"http_status"`
	Reason            string    `json:"reason,omitempty" bson:"reason,omitempty"`
	ProviderReference string    `json:"provider_reference,omitempty" bson:"provider_reference,omitempty"`
}

type auditRoute struct{ action, resource, collection string }

var auditRoutes = map[string]auditRoute{
	"POST /api/v1/admin/products":           {"product.create", "product", "products"},
	"PUT /api/v1/admin/products/:id":        {"product.update", "product", "products"},
	"DELETE /api/v1/admin/products/:id":     {"product.delete", "product", "products"},
	"POST /api/v1/admin/categories":         {"category.create", "category", "categories"},
	"PUT /api/v1/admin/categories/:id":      {"category.update", "category", "categories"},
	"DELETE /api/v1/admin/categories/:id":   {"category.delete", "category", "categories"},
	"PUT /api/v1/admin/order/:id/status":    {"order.status.update", "order", "orders"},
	"POST /api/v1/admin/order/:id/refund":   {"order.refund", "order", "orders"},
	"POST /api/v1/admin/orders/:id/refund":  {"order.refund", "order", "orders"},
	"POST /api/v1/admin/orders/:id/ship":    {"shipment.create", "order", "orders"},
	"POST /api/v1/admin/coupons":            {"coupon.create", "coupon", "coupons"},
	"PUT /api/v1/admin/coupons/:id":         {"coupon.update", "coupon", "coupons"},
	"DELETE /api/v1/admin/coupons/:id":      {"coupon.delete", "coupon", "coupons"},
	"PUT /api/v1/admin/returns/:id":         {"return.update", "return_request", "return_requests"},
	"PUT /api/v1/admin/reviews/:id/approve": {"review.publish", "review", "reviews"},
}

func AuditMiddleware(db *mongo.Database) gin.HandlerFunc {
	return func(c *gin.Context) {
		route, tracked := auditRoutes[c.Request.Method+" "+c.FullPath()]
		if !tracked {
			c.Next()
			return
		}
		resourceID := c.Param("id")
		previous := safeSnapshot(c.Request.Context(), db, route.collection, resourceID)
		c.Next()
		if value := c.GetString("audit_resource_id"); value != "" {
			resourceID = value
		}
		status := c.Writer.Status()
		outcome := "succeeded"
		if status >= 400 {
			outcome = "failed"
		}
		event := AuditEvent{
			ID: uuid.NewString(), Timestamp: time.Now().UTC(), ActorAdminID: c.GetString("user_id"), ActorEmail: c.GetString("email"),
			RequestID: c.GetString("request_id"), Action: route.action, ResourceType: route.resource, ResourceID: resourceID,
			PreviousState: previous, NewState: safeSnapshot(context.Background(), db, route.collection, resourceID), Outcome: outcome, HTTPStatus: status,
			Reason: c.GetString("audit_reason"), ProviderReference: c.GetString("audit_provider_reference"),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := db.Collection("admin_audit_events").InsertOne(ctx, event); err != nil {
			log.Error("AUDIT", "Failed to append admin audit event", "action", route.action, "resource_id", resourceID, "request_id", event.RequestID, "err", err)
		}
	}
}

func safeSnapshot(parent context.Context, db *mongo.Database, collection, id string) bson.M {
	if id == "" {
		return nil
	}
	fields := bson.M{"_id": 1, "updated_at": 1}
	switch collection {
	case "products":
		fields = bson.M{"_id": 1, "name": 1, "sku": 1, "category": 1, "price": 1, "stock": 1, "is_active": 1, "updated_at": 1}
	case "categories":
		fields = bson.M{"_id": 1, "name": 1, "slug": 1, "parent_id": 1, "is_active": 1, "updated_at": 1}
	case "coupons":
		fields = bson.M{"_id": 1, "code": 1, "type": 1, "value": 1, "usage_limit": 1, "used_count": 1, "is_active": 1, "updated_at": 1}
	case "orders":
		fields = bson.M{"_id": 1, "order_number": 1, "status": 1, "payment_status": 1, "refund_amount": 1, "refund_requested_amount": 1, "shipment_id": 1, "tracking_id": 1, "updated_at": 1}
	case "return_requests":
		fields = bson.M{"_id": 1, "order_id": 1, "type": 1, "status": 1, "updated_at": 1}
	case "reviews":
		fields = bson.M{"_id": 1, "product_id": 1, "is_approved": 1, "updated_at": 1}
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	var result bson.M
	if err := db.Collection(collection).FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(fields)).Decode(&result); err != nil {
		return nil
	}
	return result
}

func (s *Service) ListAudit(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	page, limit := boundedPagination(c)
	filter := bson.M{}
	if value := c.Query("action"); value != "" {
		filter["action"] = value
	}
	if value := c.Query("resource_id"); value != "" {
		filter["resource_id"] = value
	}
	if value := c.Query("request_id"); value != "" {
		filter["request_id"] = value
	}
	total, _ := s.db.Collection("admin_audit_events").CountDocuments(ctx, filter)
	cursor, err := s.db.Collection("admin_audit_events").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "timestamp", Value: -1}}).SetSkip(int64((page-1)*limit)).SetLimit(int64(limit)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list audit events"})
		return
	}
	defer cursor.Close(ctx)
	items := []AuditEvent{}
	if err := cursor.All(ctx, &items); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decode audit events"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"audit_events": items, "total": total, "page": page, "limit": limit})
}
