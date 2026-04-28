package cart

import (
	"context"
	"net/http"
	"time"

	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var log = logger.New("CART", "ITEMS")

type Handler struct{ db *mongo.Database }

func NewHandler(db *mongo.Database) *Handler { return &Handler{db: db} }

type AddItemRequest struct {
	ProductID string `json:"product_id" binding:"required"`
	VariantID string `json:"variant_id,omitempty"`
	Quantity  int    `json:"quantity" binding:"required"`
}

func (h *Handler) Get(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var cart models.Cart
	err := h.db.Collection("carts").FindOne(ctx, bson.M{"user_id": userID}).Decode(&cart)
	if err != nil {
		log.Debug("GET", "No cart found, creating new", "user_id", userID)
		cart = models.Cart{ID: uuid.New().String(), UserID: userID, Items: []models.CartItem{}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		h.db.Collection("carts").InsertOne(ctx, cart)
	}

	total := 0
	for _, item := range cart.Items {
		total += item.Price * item.Quantity
	}
	c.JSON(http.StatusOK, gin.H{"cart": cart, "total": total, "item_count": len(cart.Items)})
}

func (h *Handler) AddItem(c *gin.Context) {
	userID := c.GetString("user_id")
	var req AddItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warn("ADD", "Invalid add item request", "user_id", userID, "err", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "product_id and quantity required"})
		return
	}
	if req.Quantity < 1 { req.Quantity = 1 }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var product models.Product
	if err := h.db.Collection("products").FindOne(ctx, bson.M{"_id": req.ProductID, "is_active": true}).Decode(&product); err != nil {
		log.WarnWithCode("ADD", errcodes.ECartProdNotFound.Code, "Product not found or inactive", "user_id", userID, "product_id", req.ProductID)
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found", "code": errcodes.ECartProdNotFound.Code})
		return
	}

	if product.Stock < req.Quantity {
		log.WarnWithCode("ADD", errcodes.ECartStockLimit.Code, "Insufficient stock", "user_id", userID, "product_id", req.ProductID, "stock", product.Stock, "requested", req.Quantity)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Insufficient stock", "code": errcodes.ECartStockLimit.Code})
		return
	}

	price := product.Price
	for _, v := range product.Variants {
		if v.ID == req.VariantID && v.Price > 0 { price = v.Price }
	}

	item := models.CartItem{
		ProductID: product.ID, VariantID: req.VariantID,
		Name: product.Name, Price: price, Quantity: req.Quantity,
		Image: product.Thumbnail,
	}

	var cart models.Cart
	err := h.db.Collection("carts").FindOne(ctx, bson.M{"user_id": userID}).Decode(&cart)
	if err != nil {
		cart = models.Cart{ID: uuid.New().String(), UserID: userID, Items: []models.CartItem{item}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		h.db.Collection("carts").InsertOne(ctx, cart)
	} else {
		found := false
		for i, ci := range cart.Items {
			if ci.ProductID == req.ProductID && ci.VariantID == req.VariantID {
				cart.Items[i].Quantity += req.Quantity
				found = true
				break
			}
		}
		if !found { cart.Items = append(cart.Items, item) }
		h.db.Collection("carts").UpdateOne(ctx, bson.M{"_id": cart.ID}, bson.M{"$set": bson.M{"items": cart.Items, "updated_at": time.Now()}})
	}

	log.Info("ADD", "Item added to cart", "user_id", userID, "product_id", req.ProductID, "quantity", req.Quantity, "price", price)
	c.JSON(http.StatusOK, gin.H{"message": "Item added to cart"})
}

func (h *Handler) RemoveItem(c *gin.Context) {
	userID := c.GetString("user_id")
	productID := c.Param("productId")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.db.Collection("carts").UpdateOne(ctx, bson.M{"user_id": userID},
		bson.M{"$pull": bson.M{"items": bson.M{"product_id": productID}}, "$set": bson.M{"updated_at": time.Now()}})

	log.Info("REMOVE", "Item removed from cart", "user_id", userID, "product_id", productID)
	c.JSON(http.StatusOK, gin.H{"message": "Item removed"})
}

func (h *Handler) UpdateQuantity(c *gin.Context) {
	userID := c.GetString("user_id")
	productID := c.Param("productId")
	var body struct { Quantity int `json:"quantity"` }
	c.ShouldBindJSON(&body)
	if body.Quantity < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Quantity must be >= 1"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.db.Collection("carts").UpdateOne(ctx, bson.M{"user_id": userID, "items.product_id": productID},
		bson.M{"$set": bson.M{"items.$.quantity": body.Quantity, "updated_at": time.Now()}})

	log.Debug("UPDATE_QTY", "Cart quantity updated", "user_id", userID, "product_id", productID, "quantity", body.Quantity)
	c.JSON(http.StatusOK, gin.H{"message": "Quantity updated"})
}

func (h *Handler) Clear(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.db.Collection("carts").UpdateOne(ctx, bson.M{"user_id": userID}, bson.M{"$set": bson.M{"items": []models.CartItem{}, "updated_at": time.Now()}})
	log.Info("CLEAR", "Cart cleared", "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Cart cleared"})
}
