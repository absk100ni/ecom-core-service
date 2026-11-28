package featureflags

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	WebsiteMaintenance   = "website_maintenance"
	PhoneLoginEnabled    = "phone_login_enabled"
	GuestCheckoutEnabled = "guest_checkout_enabled"
)

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,63}$`)

type Flag struct {
	Key     string `json:"key" bson:"_id"`
	Enabled bool   `json:"enabled" bson:"enabled"`
}
type Service struct{ db *mongo.Database }

func New(db *mongo.Database) *Service { return &Service{db: db} }
func (s *Service) Seed(ctx context.Context) error {
	for _, key := range []string{WebsiteMaintenance, PhoneLoginEnabled, GuestCheckoutEnabled} {
		if _, err := s.db.Collection("feature_flags").UpdateOne(ctx, bson.M{"_id": key}, bson.M{"$setOnInsert": bson.M{"enabled": false}}, options.Update().SetUpsert(true)); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) Enabled(ctx context.Context, key string) bool {
	var f Flag
	return s.db.Collection("feature_flags").FindOne(ctx, bson.M{"_id": key}).Decode(&f) == nil && f.Enabled
}
func (s *Service) Public(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	cur, err := s.db.Collection("feature_flags").Find(ctx, bson.M{})
	if err != nil {
		c.JSON(500, gin.H{"error": "Feature flags unavailable"})
		return
	}
	defer cur.Close(ctx)
	flags := []Flag{}
	_ = cur.All(ctx, &flags)
	values := gin.H{}
	for _, f := range flags {
		values[f.Key] = f.Enabled
	}
	c.JSON(200, gin.H{"flags": values})
}
func (s *Service) AdminList(c *gin.Context) { s.Public(c) }
func (s *Service) Create(c *gin.Context) {
	var req Flag
	if c.ShouldBindJSON(&req) != nil || !keyPattern.MatchString(req.Key) {
		c.JSON(400, gin.H{"error": "Key must be 3-64 lowercase letters, numbers, _ or -"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	_, err := s.db.Collection("feature_flags").InsertOne(ctx, Flag{Key: req.Key, Enabled: req.Enabled})
	if mongo.IsDuplicateKeyError(err) {
		c.JSON(409, gin.H{"error": "Feature flag already exists"})
		return
	}
	if err != nil {
		c.JSON(500, gin.H{"error": "Failed to create feature flag"})
		return
	}
	c.JSON(201, Flag{Key: req.Key, Enabled: req.Enabled})
}
func (s *Service) Update(c *gin.Context) {
	key := c.Param("key")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !keyPattern.MatchString(key) || c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "Invalid feature flag"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	result, err := s.db.Collection("feature_flags").UpdateOne(ctx, bson.M{"_id": key}, bson.M{"$set": bson.M{"enabled": req.Enabled}})
	if err != nil {
		c.JSON(500, gin.H{"error": "Failed to update feature flag"})
		return
	}
	if result.MatchedCount == 0 {
		c.JSON(404, gin.H{"error": "Feature flag not found"})
		return
	}
	c.JSON(200, Flag{Key: key, Enabled: req.Enabled})
}
func (s *Service) BlockDuringMaintenance(c *gin.Context) {
	if s.Enabled(c.Request.Context(), WebsiteMaintenance) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Website is temporarily under maintenance", "code": "EMAINT001"})
		return
	}
	c.Next()
}
