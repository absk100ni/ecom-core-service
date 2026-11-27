package cart

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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

	// Rehydrate stock, identity and price from the catalog. The cart is a list of
	// purchase intentions, not a price authority.
	productByID := map[string]models.Product{}
	if len(cart.Items) > 0 {
		ids := make([]string, 0, len(cart.Items))
		for _, item := range cart.Items {
			ids = append(ids, item.ProductID)
		}
		cur, qErr := h.db.Collection("products").Find(ctx, bson.M{"_id": bson.M{"$in": ids}, "is_active": true})
		if qErr == nil {
			var products []models.Product
			if cur.All(ctx, &products) == nil {
				for _, product := range products {
					productByID[product.ID] = product
				}
			}
			cur.Close(ctx)
		}
	}

	total := 0
	enriched := make([]gin.H, 0, len(cart.Items))
	refreshedItems := make([]models.CartItem, 0, len(cart.Items))
	for _, item := range cart.Items {
		product, exists := productByID[item.ProductID]
		available := exists
		name, image, price, stock := item.Name, item.Image, item.Price, 0
		variantLabel := item.VariantLabel
		if exists {
			name, image, price, stock = product.Name, product.Thumbnail, product.Price, product.Stock
			if item.VariantID != "" {
				stock = 0
				variantFound := false
				for _, variant := range product.Variants {
					if variant.ID == item.VariantID {
						variantFound = true
						stock = variant.Stock
						variantLabel = variant.Name + ": " + variant.Value
						if variant.Price > 0 {
							price = variant.Price
						}
						break
					}
				}
				if !variantFound {
					available = false
				}
			}
		}
		total += price * item.Quantity
		refreshedItems = append(refreshedItems, models.CartItem{
			ProductID: item.ProductID, VariantID: item.VariantID, VariantLabel: variantLabel, Name: name,
			Price: price, Quantity: item.Quantity, Image: image,
		})
		enriched = append(enriched, gin.H{
			"product_id":    item.ProductID,
			"variant_id":    item.VariantID,
			"variant_label": variantLabel,
			"name":          name,
			"price":         price,
			"quantity":      item.Quantity,
			"image":         image,
			"stock":         stock,
			"available":     available,
		})
	}

	if len(refreshedItems) > 0 {
		h.db.Collection("carts").UpdateOne(ctx, bson.M{"_id": cart.ID}, bson.M{"$set": bson.M{"items": refreshedItems, "updated_at": time.Now()}})
	}

	c.JSON(http.StatusOK, gin.H{
		"cart":       gin.H{"id": cart.ID, "user_id": cart.UserID, "items": enriched, "created_at": cart.CreatedAt, "updated_at": cart.UpdatedAt},
		"total":      total,
		"item_count": len(cart.Items),
	})
}

func (h *Handler) AddItem(c *gin.Context) {
	userID := c.GetString("user_id")
	var req AddItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warn("ADD", "Invalid add item request", "user_id", userID, "err", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "product_id and quantity required"})
		return
	}
	if req.Quantity < 1 {
		req.Quantity = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var product models.Product
	if err := h.db.Collection("products").FindOne(ctx, bson.M{"_id": req.ProductID, "is_active": true}).Decode(&product); err != nil {
		log.WarnWithCode("ADD", errcodes.ECartProdNotFound.Code, "Product not found or inactive", "user_id", userID, "product_id", req.ProductID)
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found", "code": errcodes.ECartProdNotFound.Code})
		return
	}

	price := product.Price
	availableStock := product.Stock
	variantLabel := ""
	if len(product.Variants) > 0 && req.VariantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Select a product option before adding to cart", "code": errcodes.ECartProdNotFound.Code})
		return
	}
	if req.VariantID != "" {
		variantFound := false
		for _, variant := range product.Variants {
			if variant.ID == req.VariantID {
				variantFound = true
				if variant.Price > 0 {
					price = variant.Price
				}
				variantLabel = variant.Name + ": " + variant.Value
				if variant.Stock < availableStock {
					availableStock = variant.Stock
				}
				break
			}
		}
		if !variantFound {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Selected product variant is unavailable", "code": errcodes.ECartProdNotFound.Code})
			return
		}
	}

	if availableStock < req.Quantity {
		log.WarnWithCode("ADD", errcodes.ECartStockLimit.Code, "Insufficient stock", "user_id", userID, "product_id", req.ProductID, "variant_id", req.VariantID, "stock", availableStock, "requested", req.Quantity)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Insufficient stock", "code": errcodes.ECartStockLimit.Code})
		return
	}

	item := models.CartItem{
		ProductID: product.ID, VariantID: req.VariantID, VariantLabel: variantLabel,
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
				// Stock check must cover the TOTAL (existing + increment), not just
				// the increment — otherwise listing-page add + PDP max add = stock+1.
				newQty := ci.Quantity + req.Quantity
				if newQty > availableStock {
					available := availableStock - ci.Quantity
					if available < 0 {
						available = 0
					}
					log.WarnWithCode("ADD", errcodes.ECartStockLimit.Code, "Total cart quantity exceeds stock", "user_id", userID, "product_id", req.ProductID, "stock", availableStock, "in_cart", ci.Quantity, "requested", req.Quantity)
					c.JSON(http.StatusBadRequest, gin.H{
						"error": fmt.Sprintf("Only %d more can be added — you already have %d of %d in stock in your cart", available, ci.Quantity, availableStock),
						"code":  errcodes.ECartStockLimit.Code,
					})
					return
				}
				cart.Items[i].Quantity = newQty
				found = true
				break
			}
		}
		if !found {
			cart.Items = append(cart.Items, item)
		}
		h.db.Collection("carts").UpdateOne(ctx, bson.M{"_id": cart.ID}, bson.M{"$set": bson.M{"items": cart.Items, "updated_at": time.Now()}})
	}

	log.Info("ADD", "Item added to cart", "user_id", userID, "product_id", req.ProductID, "quantity", req.Quantity, "price", price)
	c.JSON(http.StatusOK, gin.H{"message": "Item added to cart"})
}

func (h *Handler) RemoveItem(c *gin.Context) {
	userID := c.GetString("user_id")
	productID := c.Param("productId")
	variantID := c.Query("variant_id")
	itemFilter := bson.M{"product_id": productID}
	if variantID != "" {
		itemFilter["variant_id"] = variantID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.db.Collection("carts").UpdateOne(ctx, bson.M{"user_id": userID},
		bson.M{"$pull": bson.M{"items": itemFilter}, "$set": bson.M{"updated_at": time.Now()}})

	log.Info("REMOVE", "Item removed from cart", "user_id", userID, "product_id", productID)
	c.JSON(http.StatusOK, gin.H{"message": "Item removed"})
}

func (h *Handler) UpdateQuantity(c *gin.Context) {
	userID := c.GetString("user_id")
	productID := c.Param("productId")
	var body struct {
		Quantity  int    `json:"quantity"`
		VariantID string `json:"variant_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Quantity < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Quantity must be >= 1"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var product models.Product
	if err := h.db.Collection("products").FindOne(ctx, bson.M{"_id": productID, "is_active": true}).Decode(&product); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found", "code": errcodes.ECartProdNotFound.Code})
		return
	}
	availableStock := product.Stock
	if body.VariantID != "" {
		variantFound := false
		for _, variant := range product.Variants {
			if variant.ID == body.VariantID {
				variantFound = true
				if variant.Stock < availableStock {
					availableStock = variant.Stock
				}
				break
			}
		}
		if !variantFound {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Selected product variant is unavailable", "code": errcodes.ECartProdNotFound.Code})
			return
		}
	}
	if body.Quantity > availableStock {
		log.WarnWithCode("UPDATE_QTY", errcodes.ECartStockLimit.Code, "Requested quantity exceeds stock", "user_id", userID, "product_id", productID, "variant_id", body.VariantID, "stock", availableStock, "requested", body.Quantity)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Only %d in stock", availableStock), "code": errcodes.ECartStockLimit.Code})
		return
	}

	itemFilter := bson.M{"item.product_id": productID}
	if body.VariantID != "" {
		itemFilter["item.variant_id"] = body.VariantID
	}
	result, err := h.db.Collection("carts").UpdateOne(ctx,
		bson.M{"user_id": userID},
		bson.M{"$set": bson.M{"items.$[item].quantity": body.Quantity, "updated_at": time.Now()}},
		options.Update().SetArrayFilters(options.ArrayFilters{Filters: []interface{}{itemFilter}}),
	)
	if err != nil || result.ModifiedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Cart item not found"})
		return
	}

	log.Debug("UPDATE_QTY", "Cart quantity updated", "user_id", userID, "product_id", productID, "variant_id", body.VariantID, "quantity", body.Quantity)
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
