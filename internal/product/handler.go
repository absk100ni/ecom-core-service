package product

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/cache"
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

// Cache TTLs — short enough that admin edits show up quickly, long enough to absorb the
// storefront read storm. Writes also bust the relevant cache (see invalidate*).
const (
	productListTTL = 60 * time.Second
	categoryTTL    = 5 * time.Minute
)

type Handler struct {
	db    *mongo.Database
	cache *cache.Cache
}

func NewHandler(db *mongo.Database, c *cache.Cache) *Handler { return &Handler{db: db, cache: c} }

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	re := regexp.MustCompile(`[^a-z0-9]+`)
	return strings.Trim(re.ReplaceAllString(s, "-"), "-")
}

func generateSKU(category, name string) string {
	catPart := strings.ToUpper(slugify(category))
	if len(catPart) > 3 {
		catPart = catPart[:3]
	}
	namePart := strings.ToUpper(slugify(name))
	if len(namePart) > 5 {
		namePart = namePart[:5]
	}
	return fmt.Sprintf("%s-%s-%04d", catPart, namePart, rand.Intn(10000))
}

// ==================== PUBLIC ENDPOINTS ====================

// List — GET /products?category=&search=&page=&limit=&sort=
func (h *Handler) List(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Serve cached storefront listings keyed by the full query string. Busted on any
	// product write (see invalidateProductCache).
	cacheKey := "products:list:" + c.Request.URL.RawQuery
	if cached, ok := h.cache.Get(ctx, cacheKey); ok {
		c.Header("X-Cache", "HIT")
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(cached))
		return
	}

	filter := bson.M{"is_active": true}
	if cat := c.Query("category"); cat != "" {
		// Look up the category to check if it has children — include descendants
		var matchedCat models.Category
		catFilter := bson.M{"$or": []bson.M{{"slug": cat}, {"_id": cat}}, "is_active": true}
		if err := h.db.Collection("categories").FindOne(ctx, catFilter).Decode(&matchedCat); err == nil {
			slugs := CollectDescendantSlugs(ctx, h.db, matchedCat)
			if len(slugs) > 1 {
				filter["category"] = bson.M{"$in": slugs}
			} else {
				filter["category"] = matchedCat.Slug
			}
		} else {
			filter["category"] = cat
		}
	}
	isTextSearch := false
	if search := c.Query("search"); search != "" {
		// Indexed full-text search via the "product_text" index, instead of an
		// un-indexable case-insensitive $regex collection scan.
		filter["$text"] = bson.M{"$search": search}
		isTextSearch = true
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
	if len(priceFilter) > 0 {
		filter["price"] = priceFilter
	}

	// Pagination
	page, limit := 1, 12
	if p := c.Query("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if l := c.Query("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 12
	}
	skip := int64((page - 1) * limit)

	total, _ := h.db.Collection("products").CountDocuments(ctx, filter)

	// Sort. An explicit sort= always wins; otherwise a text search defaults to relevance
	// (text score) ordering, and a plain listing defaults to newest-first.
	sortExplicit := c.Query("sort")
	var sortField bson.D
	switch sortExplicit {
	case "price_asc":
		sortField = bson.D{{Key: "price", Value: 1}}
	case "price_desc":
		sortField = bson.D{{Key: "price", Value: -1}}
	case "name_asc":
		sortField = bson.D{{Key: "name", Value: 1}}
	case "name_desc":
		sortField = bson.D{{Key: "name", Value: -1}}
	case "newest":
		sortField = bson.D{{Key: "created_at", Value: -1}}
	case "oldest":
		sortField = bson.D{{Key: "created_at", Value: 1}}
	default:
		if isTextSearch {
			sortField = bson.D{{Key: "score", Value: bson.M{"$meta": "textScore"}}}
		} else {
			sortField = bson.D{{Key: "created_at", Value: -1}}
		}
	}

	opts := options.Find().SetSort(sortField).SetSkip(skip).SetLimit(int64(limit))
	if isTextSearch {
		// Required to sort by the computed relevance score.
		opts.SetProjection(bson.M{"score": bson.M{"$meta": "textScore"}})
	}
	cursor, err := h.db.Collection("products").Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch products"})
		return
	}
	defer cursor.Close(ctx)

	var products []models.Product
	cursor.All(ctx, &products)
	if products == nil {
		products = []models.Product{}
	}

	totalPages := int(total) / limit
	if int(total)%limit > 0 {
		totalPages++
	}

	resp := gin.H{
		"products": products, "total": total,
		"page": page, "limit": limit, "total_pages": totalPages,
	}
	if payload, err := json.Marshal(resp); err == nil {
		h.cache.Set(ctx, cacheKey, string(payload), productListTTL)
		c.Header("X-Cache", "MISS")
		c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
		return
	}
	c.JSON(http.StatusOK, resp)
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

	const cacheKey = "categories:active"
	if cached, ok := h.cache.Get(ctx, cacheKey); ok {
		c.Header("X-Cache", "HIT")
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(cached))
		return
	}

	opts := options.Find().SetSort(bson.D{{Key: "sort_order", Value: 1}, {Key: "name", Value: 1}})
	cursor, err := h.db.Collection("categories").Find(ctx, bson.M{"is_active": true}, opts)
	if err != nil {
		log.ErrorWithCode("LIST_CATEGORIES", errcodes.ECatListFailed.Code, "Categories find failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch categories"})
		return
	}
	defer cursor.Close(ctx)
	var cats []models.Category
	cursor.All(ctx, &cats)
	if cats == nil {
		cats = []models.Category{}
	}

	resp := gin.H{"categories": cats}
	if payload, err := json.Marshal(resp); err == nil {
		h.cache.Set(ctx, cacheKey, string(payload), categoryTTL)
		c.Header("X-Cache", "MISS")
		c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// invalidateProductCache clears cached product listings after a write. Listings vary by
// query string, so we delete by prefix; categories have a single key.
func (h *Handler) invalidateProductCache(ctx context.Context) {
	h.cache.DeleteByPrefix(ctx, "products:list:")
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
	if product.CompareAt < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Compare-at price (MRP) cannot be negative"})
		return
	}
	if product.CompareAt > 0 && product.CompareAt < product.Price {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Compare-at price must be greater than selling price"})
		return
	}

	product.ID = uuid.New().String()
	if product.Slug == "" {
		product.Slug = slugify(product.Name)
	}
	if product.SKU == "" {
		product.SKU = generateSKU(product.Category, product.Name)
	}
	product.IsActive = true
	product.CreatedAt = time.Now()
	product.UpdatedAt = time.Now()
	if product.Images == nil {
		product.Images = []string{}
	}
	if product.Variants == nil {
		product.Variants = []models.Variant{}
	}
	if product.Tags == nil {
		product.Tags = []string{}
	}

	// Generate variant IDs if missing
	for i := range product.Variants {
		if product.Variants[i].ID == "" {
			product.Variants[i].ID = uuid.New().String()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check slug uniqueness
	count, _ := h.db.Collection("products").CountDocuments(ctx, bson.M{"slug": product.Slug})
	if count > 0 {
		product.Slug = product.Slug + "-" + uuid.New().String()[:4]
	}

	if _, err := h.db.Collection("products").InsertOne(ctx, product); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create product"})
		return
	}
	h.invalidateProductCache(ctx)
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
	if ca, ok := updates["compare_at_price"]; ok {
		if v, ok := ca.(float64); ok && v < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Compare-at price (MRP) cannot be negative"})
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
	h.invalidateProductCache(ctx)
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
	h.invalidateProductCache(ctx)
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
	if cat.Slug == "" {
		cat.Slug = slugify(cat.Name)
	}
	cat.IsActive = true
	cat.CreatedAt = time.Now()
	cat.UpdatedAt = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Hierarchy: compute level and path from parent
	if cat.ParentID != "" {
		var parent models.Category
		err := h.db.Collection("categories").FindOne(ctx, bson.M{"_id": cat.ParentID}).Decode(&parent)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Parent category not found", "code": errcodes.ECatNotFound.Code})
			return
		}
		if !parent.IsActive {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Parent category is not active", "code": errcodes.ECatNotFound.Code})
			return
		}
		cat.Level = parent.Level + 1
		if cat.Level > 3 {
			c.JSON(http.StatusBadRequest, gin.H{"error": errcodes.ECatMaxDepth.Message, "code": errcodes.ECatMaxDepth.Code})
			return
		}
		if parent.Path == "" {
			cat.Path = parent.ID
		} else {
			cat.Path = parent.Path + "/" + parent.ID
		}
	} else {
		cat.Level = 1
		cat.Path = ""
	}

	// Duplicate check
	count, _ := h.db.Collection("categories").CountDocuments(ctx, bson.M{"slug": cat.Slug})
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Category already exists", "code": errcodes.ECatSlugConflict.Code})
		return
	}

	h.db.Collection("categories").InsertOne(ctx, cat)
	h.invalidateCategoryCache(ctx)
	c.JSON(http.StatusCreated, cat)
}

// UpdateCategory — PUT /admin/categories/:id
func (h *Handler) UpdateCategory(c *gin.Context) {
	id := c.Param("id")
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

	// If parent_id is being changed, validate hierarchy constraints
	if newParentID, ok := updates["parent_id"]; ok {
		var current models.Category
		if err := h.db.Collection("categories").FindOne(ctx, bson.M{"_id": id}).Decode(&current); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Category not found", "code": errcodes.ECatNotFound.Code})
			return
		}

		parentIDStr, _ := newParentID.(string)

		// Cycle check: parent cannot be self
		if parentIDStr == id {
			c.JSON(http.StatusBadRequest, gin.H{"error": errcodes.ECatCycle.Message, "code": errcodes.ECatCycle.Code})
			return
		}

		if parentIDStr != "" {
			var parent models.Category
			if err := h.db.Collection("categories").FindOne(ctx, bson.M{"_id": parentIDStr}).Decode(&parent); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Parent category not found", "code": errcodes.ECatNotFound.Code})
				return
			}
			if !parent.IsActive {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Parent category is not active"})
				return
			}

			// Cycle check: parent cannot be a descendant of this category
			if isDescendant(parent, id) {
				c.JSON(http.StatusBadRequest, gin.H{"error": errcodes.ECatCycle.Message, "code": errcodes.ECatCycle.Code})
				return
			}

			newLevel := parent.Level + 1
			if newLevel > 3 {
				c.JSON(http.StatusBadRequest, gin.H{"error": errcodes.ECatMaxDepth.Message, "code": errcodes.ECatMaxDepth.Code})
				return
			}

			// Check that existing descendants won't exceed depth 3
			maxDescendantDepth := h.maxDescendantLevel(ctx, id)
			depthBelow := maxDescendantDepth - current.Level
			if newLevel+depthBelow > 3 {
				c.JSON(http.StatusBadRequest, gin.H{"error": errcodes.ECatMaxDepth.Message, "code": errcodes.ECatMaxDepth.Code})
				return
			}

			var newPath string
			if parent.Path == "" {
				newPath = parent.ID
			} else {
				newPath = parent.Path + "/" + parent.ID
			}
			updates["level"] = newLevel
			updates["path"] = newPath
		} else {
			// Moving to root
			updates["level"] = 1
			updates["path"] = ""
		}
	}

	result, err := h.db.Collection("categories").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": updates})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Category not found", "code": errcodes.ECatNotFound.Code})
		return
	}
	h.invalidateCategoryCache(ctx)
	c.JSON(http.StatusOK, gin.H{"message": "Category updated"})
}

// DeleteCategory — DELETE /admin/categories/:id (soft)
func (h *Handler) DeleteCategory(c *gin.Context) {
	id := c.Param("id")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Reject if category has active children
	childCount, _ := h.db.Collection("categories").CountDocuments(ctx, bson.M{
		"parent_id": id,
		"is_active": true,
	})
	if childCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": errcodes.ECatHasChildren.Message, "code": errcodes.ECatHasChildren.Code})
		return
	}

	result, err := h.db.Collection("categories").UpdateOne(ctx, bson.M{"_id": id},
		bson.M{"$set": bson.M{"is_active": false, "updated_at": time.Now()}})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Category not found", "code": errcodes.ECatNotFound.Code})
		return
	}
	h.invalidateCategoryCache(ctx)
	c.JSON(http.StatusOK, gin.H{"message": "Category deleted"})
}

// ==================== CATEGORY TREE ====================

// CategoryTree — GET /categories/tree (nested JSON)
func (h *Handler) CategoryTree(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const cacheKey = "categories:tree"
	if cached, ok := h.cache.Get(ctx, cacheKey); ok {
		c.Header("X-Cache", "HIT")
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(cached))
		return
	}

	opts := options.Find().SetSort(bson.D{{Key: "sort_order", Value: 1}, {Key: "name", Value: 1}})
	cursor, err := h.db.Collection("categories").Find(ctx, bson.M{"is_active": true}, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch categories", "code": errcodes.ECatTreeFailed.Code})
		return
	}
	defer cursor.Close(ctx)

	var cats []models.Category
	cursor.All(ctx, &cats)
	if cats == nil {
		cats = []models.Category{}
	}

	tree := BuildCategoryTree(cats)
	resp := gin.H{"categories": tree}
	if payload, err := json.Marshal(resp); err == nil {
		h.cache.Set(ctx, cacheKey, string(payload), categoryTTL)
		c.Header("X-Cache", "MISS")
		c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// CategoryTreeNode is the nested response shape for GET /categories/tree.
type CategoryTreeNode struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	Slug     string             `json:"slug"`
	Image    string             `json:"image,omitempty"`
	Level    int                `json:"level"`
	Children []CategoryTreeNode `json:"children"`
}

// BuildCategoryTree assembles a nested tree from a flat list in one pass (exported for tests).
func BuildCategoryTree(cats []models.Category) []CategoryTreeNode {
	nodeMap := make(map[string]*CategoryTreeNode, len(cats))
	var roots []CategoryTreeNode

	// Create nodes
	for i := range cats {
		nodeMap[cats[i].ID] = &CategoryTreeNode{
			ID: cats[i].ID, Name: cats[i].Name, Slug: cats[i].Slug,
			Image: cats[i].Image, Level: cats[i].Level, Children: []CategoryTreeNode{},
		}
	}

	// Link children to parents
	for i := range cats {
		node := nodeMap[cats[i].ID]
		if cats[i].ParentID == "" || nodeMap[cats[i].ParentID] == nil {
			roots = append(roots, *node)
		} else {
			parent := nodeMap[cats[i].ParentID]
			parent.Children = append(parent.Children, *node)
		}
	}

	// The above creates copies; rebuild with correct children by doing a second pass
	// Actually, since Go copies, we need a pointer-based approach. Let's redo properly.
	type ptrNode struct {
		CategoryTreeNode
		children []*ptrNode
	}
	ptrMap := make(map[string]*ptrNode, len(cats))
	for i := range cats {
		ptrMap[cats[i].ID] = &ptrNode{
			CategoryTreeNode: CategoryTreeNode{
				ID: cats[i].ID, Name: cats[i].Name, Slug: cats[i].Slug,
				Image: cats[i].Image, Level: cats[i].Level, Children: []CategoryTreeNode{},
			},
		}
	}
	var rootPtrs []*ptrNode
	for i := range cats {
		pn := ptrMap[cats[i].ID]
		if cats[i].ParentID == "" || ptrMap[cats[i].ParentID] == nil {
			rootPtrs = append(rootPtrs, pn)
		} else {
			ptrMap[cats[i].ParentID].children = append(ptrMap[cats[i].ParentID].children, pn)
		}
	}

	var flatten func(pn *ptrNode) CategoryTreeNode
	flatten = func(pn *ptrNode) CategoryTreeNode {
		node := pn.CategoryTreeNode
		node.Children = make([]CategoryTreeNode, 0, len(pn.children))
		for _, ch := range pn.children {
			node.Children = append(node.Children, flatten(ch))
		}
		return node
	}

	result := make([]CategoryTreeNode, 0, len(rootPtrs))
	for _, rp := range rootPtrs {
		result = append(result, flatten(rp))
	}
	if result == nil {
		result = []CategoryTreeNode{}
	}
	return result
}

// ==================== SEARCH SUGGEST ====================

// Suggest — GET /search/suggest?q=<term>
func (h *Handler) Suggest(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if len(q) < 2 {
		c.JSON(http.StatusOK, gin.H{"suggestions": []interface{}{}, "total": 0})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cacheKey := "suggest:" + strings.ToLower(q)
	if cached, ok := h.cache.Get(ctx, cacheKey); ok {
		c.Header("X-Cache", "HIT")
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(cached))
		return
	}

	const limit = 8
	seen := make(map[string]bool)
	var results []gin.H

	// 1. Prefix match (anchored case-insensitive regex on name)
	escaped := regexp.QuoteMeta(q)
	prefixFilter := bson.M{
		"is_active": true,
		"name":      bson.M{"$regex": "^" + escaped, "$options": "i"},
	}
	prefixOpts := options.Find().SetLimit(int64(limit)).
		SetProjection(bson.M{"_id": 1, "name": 1, "slug": 1, "price": 1, "thumbnail": 1, "category": 1})
	cursor, err := h.db.Collection("products").Find(ctx, prefixFilter, prefixOpts)
	if err == nil {
		defer cursor.Close(ctx)
		for cursor.Next(ctx) {
			var p models.Product
			cursor.Decode(&p)
			if !seen[p.ID] {
				seen[p.ID] = true
				results = append(results, gin.H{
					"id": p.ID, "name": p.Name, "slug": p.Slug,
					"price": p.Price, "image": p.Thumbnail, "category": p.Category,
				})
			}
		}
	}

	// 2. Text search fallback to fill up to 8
	if len(results) < limit {
		remaining := int64(limit - len(results))
		textFilter := bson.M{"is_active": true, "$text": bson.M{"$search": q}}
		textOpts := options.Find().SetLimit(remaining).
			SetSort(bson.D{{Key: "score", Value: bson.M{"$meta": "textScore"}}}).
			SetProjection(bson.M{"_id": 1, "name": 1, "slug": 1, "price": 1, "thumbnail": 1, "category": 1, "score": bson.M{"$meta": "textScore"}})
		cur2, err2 := h.db.Collection("products").Find(ctx, textFilter, textOpts)
		if err2 == nil {
			defer cur2.Close(ctx)
			for cur2.Next(ctx) {
				var p models.Product
				cur2.Decode(&p)
				if !seen[p.ID] {
					seen[p.ID] = true
					results = append(results, gin.H{
						"id": p.ID, "name": p.Name, "slug": p.Slug,
						"price": p.Price, "image": p.Thumbnail, "category": p.Category,
					})
				}
				if len(results) >= limit {
					break
				}
			}
		}
	}

	if results == nil {
		results = []gin.H{}
	}
	resp := gin.H{"suggestions": results, "total": len(results)}
	if payload, err := json.Marshal(resp); err == nil {
		h.cache.Set(ctx, cacheKey, string(payload), 60*time.Second)
		c.Header("X-Cache", "MISS")
		c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ==================== HIERARCHY HELPERS ====================

// isDescendant checks if a category's path contains the given ancestor ID.
func isDescendant(cat models.Category, ancestorID string) bool {
	if cat.Path == "" {
		return false
	}
	parts := strings.Split(cat.Path, "/")
	for _, p := range parts {
		if p == ancestorID {
			return true
		}
	}
	return cat.ParentID == ancestorID
}

// maxDescendantLevel returns the deepest level among descendants of the given category.
func (h *Handler) maxDescendantLevel(ctx context.Context, catID string) int {
	// Find all categories whose path contains this catID
	filter := bson.M{
		"$or": []bson.M{
			{"parent_id": catID},
			{"path": bson.M{"$regex": catID}},
		},
	}
	cursor, err := h.db.Collection("categories").Find(ctx, filter, options.Find().SetProjection(bson.M{"level": 1}))
	if err != nil {
		return 0
	}
	defer cursor.Close(ctx)
	maxLevel := 0
	for cursor.Next(ctx) {
		var c models.Category
		cursor.Decode(&c)
		if c.Level > maxLevel {
			maxLevel = c.Level
		}
	}
	return maxLevel
}

// CollectDescendantSlugs returns all descendant category slugs (including self) using path prefix match.
// Exported for use in product listing.
func CollectDescendantSlugs(ctx context.Context, db *mongo.Database, cat models.Category) []string {
	slugs := []string{cat.Slug}
	// Find categories whose path starts with (or contains) the parent's full path prefix
	var pathPrefix string
	if cat.Path == "" {
		pathPrefix = cat.ID
	} else {
		pathPrefix = cat.Path + "/" + cat.ID
	}
	filter := bson.M{
		"is_active": true,
		"$or": []bson.M{
			{"parent_id": cat.ID},
			{"path": bson.M{"$regex": "^" + regexp.QuoteMeta(pathPrefix)}},
		},
	}
	cursor, err := db.Collection("categories").Find(ctx, filter, options.Find().SetProjection(bson.M{"slug": 1}))
	if err != nil {
		return slugs
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var child models.Category
		cursor.Decode(&child)
		slugs = append(slugs, child.Slug)
	}
	return slugs
}

// invalidateCategoryCache clears both flat and tree category caches.
func (h *Handler) invalidateCategoryCache(ctx context.Context) {
	h.cache.Delete(ctx, "categories:active")
	h.cache.Delete(ctx, "categories:tree")
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
		if err != nil {
			break
		}

		getVal := func(col string) string {
			if idx, ok := headerMap[col]; ok && idx < len(row) {
				return strings.TrimSpace(row[idx])
			}
			return ""
		}

		name := getVal("name")
		if name == "" {
			failed++
			errors = append(errors, "Row missing name")
			continue
		}

		price := 0
		if p := getVal("price"); p != "" {
			fmt.Sscanf(p, "%d", &price)
		}
		if price <= 0 {
			failed++
			errors = append(errors, name+": price must be > 0")
			continue
		}

		stock := 100
		if s := getVal("stock"); s != "" {
			fmt.Sscanf(s, "%d", &stock)
		}
		if stock < 0 {
			stock = 0
		}

		weight := 0
		if w := getVal("weight"); w != "" {
			fmt.Sscanf(w, "%d", &weight)
		}

		compareAt := 0
		if ca := getVal("compare_at_price"); ca != "" {
			fmt.Sscanf(ca, "%d", &compareAt)
		}

		images := []string{}
		if img := getVal("images"); img != "" {
			for _, u := range strings.Split(img, "|") {
				if t := strings.TrimSpace(u); t != "" {
					images = append(images, t)
				}
			}
		}
		tags := []string{}
		if t := getVal("tags"); t != "" {
			for _, tag := range strings.Split(t, "|") {
				if tt := strings.TrimSpace(tag); tt != "" {
					tags = append(tags, tt)
				}
			}
		}

		sku := getVal("sku")
		if sku == "" {
			sku = generateSKU(getVal("category"), name)
		}

		product := models.Product{
			ID: uuid.New().String(), Name: name, Slug: slugify(name),
			Description: getVal("description"), Category: getVal("category"),
			Price: price, CompareAt: compareAt, SKU: sku,
			Stock: stock, Weight: weight, Images: images, Tags: tags,
			Thumbnail: getVal("thumbnail"), IsActive: true,
			Variants:  []models.Variant{},
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}

		if _, err := h.db.Collection("products").InsertOne(ctx, product); err != nil {
			failed++
			errors = append(errors, name+": "+err.Error())
		} else {
			created++
		}
	}

	if created > 0 {
		h.invalidateProductCache(ctx)
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
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed"})
		return
	}
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
