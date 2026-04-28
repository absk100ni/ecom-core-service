package product

import (
	"context"
	"encoding/csv"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
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

var log = logger.New("PRODUCT", "CATALOG")
var _ = errcodes.EProdNotFound

type Handler struct{ db *mongo.Database }

func NewHandler(db *mongo.Database) *Handler { return &Handler{db: db} }

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	re := regexp.MustCompile(`[^a-z0-9]+`)
	return strings.Trim(re.ReplaceAllString(s, "-"), "-")
}

func generateSKU(category, name string) string {
	catPart := strings.ToUpper(slugify(category))
	if len(catPart) > 3 { catPart = catPart[:3] }
	namePart := strings.ToUpper(slugify(name))
	if len(namePart) > 5 { namePart = namePart[:5] }
	return fmt.Sprintf("%s-%s-%04d", catPart, namePart, rand.Intn(10000))
}

// ==================== PUBLIC ENDPOINTS ====================

// List — GET /products?category=&search=&page=&limit=&sort=
func (h *Handler) List(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := bson.M{"is_active": true}
	if cat := c.Query("category"); cat != "" {
		filter["category"] = cat
	}
	if search := c.Query("search"); search != "" {
		filter["$or"] = []bson.M{
			{"name": bson.M{"$regex": search, "$options": "i"}},
			{"tags": bson.M{"$regex": search, "$options": "i"}},
			{"description": bson.M{"$regex": search, "$options": "i"}},
		}
	}
	if tag := c.Query("tag"); tag != "" {
		filter["tags"] = tag
	}

	// Price filter
	priceFilter := bson.M{}
	if minPrice := c.Query("min_price"); minPrice != "" {
		var mp int
		fmt.Sscanf(minPrice, "%d", &mp)
		priceFilter["$gte"] = mp
	}
	if maxPrice := c.Query("max_price"); maxPrice != "" {
		var mp int
		fmt.Sscanf(maxPrice, "%d", &mp)
		priceFilter["$lte"] = mp
	}
	if len(priceFilter) > 0 { filter["price"] = priceFilter }

	// Pagination
	page, limit := 1, 12
	if p := c.Query("page"); p != "" { fmt.Sscanf(p, "%d", &page) }
	if l := c.Query("limit"); l != "" { fmt.Sscanf(l, "%d", &limit) }
	if page < 1 { page = 1 }
	if limit < 1 || limit > 100 { limit = 12 }
	skip := int64((page - 1) * limit)

	total, _ := h.db.Collection("products").CountDocuments(ctx, filter)

	// Sort
	sortField := bson.M{"created_at": -1}
	switch c.Query("sort") {
	case "price_asc": sortField = bson.M{"price": 1}
	case "price_desc": sortField = bson.M{"price": -1}
	case "name_asc": sortField = bson.M{"name": 1}
	case "name_desc": sortField = bson.M{"name": -1}
	case "newest": sortField = bson.M{"created_at": -1}
	case "oldest": sortField = bson.M{"created_at": 1}
	}

	opts := options.Find().SetSort(sortField).SetSkip(skip).SetLimit(int64(limit))
	cursor, err := h.db.Collection("products").Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch products"})
		return
	}
	defer cursor.Close(ctx)

	var products []models.Product
	cursor.All(ctx, &products)
	if products == nil { products = []models.Product{} }

	totalPages := int(total) / limit
	if int(total)%limit > 0 { totalPages++ }

	c.JSON(http.StatusOK, gin.H{
		"products": products, "total": total,
		"page": page, "limit": limit, "total_pages": totalPages,
	})
}

// Get — GET /products/:id
func (h *Handler) Get(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	id := c.Param("id")
	var product models.Product
	err := h.db.Collection("products").FindOne(ctx, bson.M{"$or": []bson.M{{"_id": id}, {"slug": id}}}).Decode(&product)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}
	c.JSON(http.StatusOK, product)
}

// ListCategories — GET /categories
func (h *Handler) ListCategories(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := options.Find().SetSort(bson.M{"sort_order": 1, "name": 1})
	cursor, err := h.db.Collection("categories").Find(ctx, bson.M{"is_active": true}, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch categories"})
		return
	}
	defer cursor.Close(ctx)
	var cats []models.Category
	cursor.All(ctx, &cats)
	if cats == nil { cats = []models.Category{} }
	c.JSON(http.StatusOK, gin.H{"categories": cats})
}

// ==================== ADMIN PRODUCT ENDPOINTS ====================

// Create — POST /admin/products
func (h *Handler) Create(c *gin.Context) {
	var product models.Product
	if err := c.ShouldBindJSON(&product); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid product data", "details": err.Error()})
		return
	}

	// Validation
	if product.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Product name is required"})
		return
	}
	if product.Price <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Price must be greater than 0 (in paise)"})
		return
	}
	if product.Stock < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Stock cannot be negative"})
		return
	}
	if product.Category == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Category is required"})
		return
	}
	if product.CompareAt > 0 && product.CompareAt < product.Price {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Compare-at price must be greater than selling price"})
		return
	}

	product.ID = uuid.New().String()
	if product.Slug == "" { product.Slug = slugify(product.Name) }
	if product.SKU == "" { product.SKU = generateSKU(product.Category, product.Name) }
	product.IsActive = true
	product.CreatedAt = time.Now()
	product.UpdatedAt = time.Now()
	if product.Images == nil { product.Images = []string{} }
	if product.Variants == nil { product.Variants = []models.Variant{} }
	if product.Tags == nil { product.Tags = []string{} }

	// Generate variant IDs if missing
	for i := range product.Variants {
		if product.Variants[i].ID == "" { product.Variants[i].ID = uuid.New().String() }
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check slug uniqueness
	count, _ := h.db.Collection("products").CountDocuments(ctx, bson.M{"slug": product.Slug})
	if count > 0 { product.Slug = product.Slug + "-" + uuid.New().String()[:4] }

	if _, err := h.db.Collection("products").InsertOne(ctx, product); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create product"})
		return
	}
	c.JSON(http.StatusCreated, product)
}

// Update — PUT /admin/products/:id
func (h *Handler) Update(c *gin.Context) {
	id := c.Param("id")
	var updates map[string]interface{}
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid data"})
		return
	}
	updates["updated_at"] = time.Now()
	delete(updates, "_id")
	delete(updates, "id")

	// Validate price if provided
	if price, ok := updates["price"]; ok {
		if p, ok := price.(float64); ok && p <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Price must be greater than 0"})
			return
		}
	}
	if stock, ok := updates["stock"]; ok {
		if s, ok := stock.(float64); ok && s < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Stock cannot be negative"})
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := h.db.Collection("products").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": updates})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Product updated"})
}

// Delete — DELETE /admin/products/:id (soft delete)
func (h *Handler) Delete(c *gin.Context) {
	id := c.Param("id")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := h.db.Collection("products").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"is_active": false, "updated_at": time.Now()}})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Product deleted (soft)"})
}

// ==================== ADMIN CATEGORY ENDPOINTS ====================

// CreateCategory — POST /admin/categories
func (h *Handler) CreateCategory(c *gin.Context) {
	var cat models.Category
	if err := c.ShouldBindJSON(&cat); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Category name required", "details": err.Error()})
		return
	}
	cat.ID = uuid.New().String()
	if cat.Slug == "" { cat.Slug = slugify(cat.Name) }
	cat.IsActive = true
	cat.CreatedAt = time.Now()
	cat.UpdatedAt = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Duplicate check
	count, _ := h.db.Collection("categories").CountDocuments(ctx, bson.M{"slug": cat.Slug})
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Category already exists"})
		return
	}

	h.db.Collection("categories").InsertOne(ctx, cat)
	c.JSON(http.StatusCreated, cat)
}

// UpdateCategory — PUT /admin/categories/:id
func (h *Handler) UpdateCategory(c *gin.Context) {
	var updates map[string]interface{}
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid data"})
		return
	}
	updates["updated_at"] = time.Now()
	delete(updates, "_id")
	delete(updates, "id")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := h.db.Collection("categories").UpdateOne(ctx, bson.M{"_id": c.Param("id")}, bson.M{"$set": updates})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Category not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Category updated"})
}

// DeleteCategory — DELETE /admin/categories/:id (soft)
func (h *Handler) DeleteCategory(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := h.db.Collection("categories").UpdateOne(ctx, bson.M{"_id": c.Param("id")},
		bson.M{"$set": bson.M{"is_active": false, "updated_at": time.Now()}})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Category not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Category deleted"})
}

// ==================== BULK OPERATIONS ====================

// BulkUploadCSV — POST /admin/products/upload-csv
func (h *Handler) BulkUploadCSV(c *gin.Context) {
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "CSV file required"})
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	headers, err := reader.Read()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid CSV"})
		return
	}

	headerMap := make(map[string]int)
	for i, h := range headers {
		headerMap[strings.TrimSpace(strings.ToLower(h))] = i
	}

	required := []string{"name", "price", "category"}
	for _, r := range required {
		if _, ok := headerMap[r]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Missing required column: " + r})
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	created, failed, errors := 0, 0, []string{}
	for {
		row, err := reader.Read()
		if err != nil { break }

		getVal := func(col string) string {
			if idx, ok := headerMap[col]; ok && idx < len(row) {
				return strings.TrimSpace(row[idx])
			}
			return ""
		}

		name := getVal("name")
		if name == "" { failed++; errors = append(errors, "Row missing name"); continue }

		price := 0
		if p := getVal("price"); p != "" { fmt.Sscanf(p, "%d", &price) }
		if price <= 0 { failed++; errors = append(errors, name+": price must be > 0"); continue }

		stock := 100
		if s := getVal("stock"); s != "" { fmt.Sscanf(s, "%d", &stock) }
		if stock < 0 { stock = 0 }

		weight := 0
		if w := getVal("weight"); w != "" { fmt.Sscanf(w, "%d", &weight) }

		compareAt := 0
		if ca := getVal("compare_at_price"); ca != "" { fmt.Sscanf(ca, "%d", &compareAt) }

		images := []string{}
		if img := getVal("images"); img != "" {
			for _, u := range strings.Split(img, "|") {
				if t := strings.TrimSpace(u); t != "" { images = append(images, t) }
			}
		}
		tags := []string{}
		if t := getVal("tags"); t != "" {
			for _, tag := range strings.Split(t, "|") {
				if tt := strings.TrimSpace(tag); tt != "" { tags = append(tags, tt) }
			}
		}

		sku := getVal("sku")
		if sku == "" { sku = generateSKU(getVal("category"), name) }

		product := models.Product{
			ID: uuid.New().String(), Name: name, Slug: slugify(name),
			Description: getVal("description"), Category: getVal("category"),
			Price: price, CompareAt: compareAt, SKU: sku,
			Stock: stock, Weight: weight, Images: images, Tags: tags,
			Thumbnail: getVal("thumbnail"), IsActive: true,
			Variants: []models.Variant{},
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}

		if _, err := h.db.Collection("products").InsertOne(ctx, product); err != nil {
			failed++
			errors = append(errors, name+": "+err.Error())
		} else {
			created++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Uploaded %d products, %d failed", created, failed),
		"created": created, "failed": failed, "errors": errors,
	})
}

// ExportCSV — GET /admin/products/export-csv
func (h *Handler) ExportCSV(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cursor, err := h.db.Collection("products").Find(ctx, bson.M{"is_active": true})
	if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed"}); return }
	defer cursor.Close(ctx)

	var products []models.Product
	cursor.All(ctx, &products)

	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", "attachment; filename=products.csv")

	writer := csv.NewWriter(c.Writer)
	writer.Write([]string{"name", "slug", "description", "category", "price", "compare_at_price", "sku", "stock", "weight", "tags", "thumbnail", "images"})

	for _, p := range products {
		writer.Write([]string{
			p.Name, p.Slug, p.Description, p.Category,
			fmt.Sprintf("%d", p.Price), fmt.Sprintf("%d", p.CompareAt),
			p.SKU, fmt.Sprintf("%d", p.Stock), fmt.Sprintf("%d", p.Weight),
			strings.Join(p.Tags, "|"), p.Thumbnail, strings.Join(p.Images, "|"),
		})
	}
	writer.Flush()
}
