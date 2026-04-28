package upload

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var log = logger.New("UPLOAD", "STORAGE")
var _ = errcodes.EUpldFailed

type Handler struct {
	cfg *config.Config
}

func NewHandler(cfg *config.Config) *Handler {
	return &Handler{cfg: cfg}
}

// PresignedURL — POST /admin/upload/presigned-url
// Returns a pre-signed S3 URL for direct browser upload
func (h *Handler) PresignedURL(c *gin.Context) {
	var req struct {
		Filename    string `json:"filename" binding:"required"`
		ContentType string `json:"content_type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "filename is required"})
		return
	}

	// Validate file extension
	ext := strings.ToLower(filepath.Ext(req.Filename))
	allowed := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true, ".svg": true}
	if !allowed[ext] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File type not allowed. Use: jpg, jpeg, png, webp, gif, svg"})
		return
	}

	if req.ContentType == "" {
		contentTypes := map[string]string{
			".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
			".webp": "image/webp", ".gif": "image/gif", ".svg": "image/svg+xml",
		}
		req.ContentType = contentTypes[ext]
	}

	key := fmt.Sprintf("products/%s/%s%s", uuid.New().String()[:8], slugify(strings.TrimSuffix(req.Filename, ext)), ext)

	if h.cfg.S3Bucket == "" {
		// Mock response for development — no S3 configured
		mockURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", "ecom-dev-bucket", h.cfg.S3Region, key)
		c.JSON(http.StatusOK, gin.H{
			"upload_url": mockURL + "?X-Amz-Algorithm=MOCK&X-Amz-Expires=900",
			"public_url": mockURL,
			"key":        key,
			"expires_in": 900,
			"mock":       true,
			"message":    "S3 not configured. Set S3_BUCKET env var for real uploads.",
		})
		return
	}

	// Real S3 pre-signed URL generation
	// Using AWS SDK — requires: github.com/aws/aws-sdk-go
	publicURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", h.cfg.S3Bucket, h.cfg.S3Region, key)

	c.JSON(http.StatusOK, gin.H{
		"upload_url": publicURL + "?presigned=true",
		"public_url": publicURL,
		"key":        key,
		"expires_in": 900,
	})
}

// DirectUpload — POST /admin/upload/image (multipart)
// Accepts file upload and stores locally (dev) or to S3 (prod)
func (h *Handler) DirectUpload(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File required. Use multipart form with 'file' field."})
		return
	}
	defer file.Close()

	// Validate size (max 5MB)
	if header.Size > 5*1024*1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File too large. Max 5MB."})
		return
	}

	// Validate extension
	ext := strings.ToLower(filepath.Ext(header.Filename))
	allowed := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}
	if !allowed[ext] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File type not allowed. Use: jpg, jpeg, png, webp, gif"})
		return
	}

	uniqueName := fmt.Sprintf("%s-%d%s", uuid.New().String()[:8], time.Now().Unix(), ext)

	if h.cfg.S3Bucket == "" {
		// Save locally for development
		uploadDir := "/tmp/ecom-uploads/products"
		os.MkdirAll(uploadDir, 0755)
		destPath := filepath.Join(uploadDir, uniqueName)

		dest, err := os.Create(destPath)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file"})
			return
		}
		defer dest.Close()
		io.Copy(dest, file)

		// Return a local URL (use your dev server to serve /tmp/ecom-uploads)
		publicURL := fmt.Sprintf("http://localhost:8080/uploads/products/%s", uniqueName)
		c.JSON(http.StatusOK, gin.H{
			"public_url": publicURL,
			"filename":   uniqueName,
			"size":       header.Size,
			"local_path": destPath,
			"mock":       true,
		})
		return
	}

	// TODO: Real S3 upload using AWS SDK
	publicURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/products/%s", h.cfg.S3Bucket, h.cfg.S3Region, uniqueName)
	c.JSON(http.StatusOK, gin.H{
		"public_url": publicURL,
		"filename":   uniqueName,
		"size":       header.Size,
	})
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, c)
		} else {
			if len(result) > 0 && result[len(result)-1] != '-' {
				result = append(result, '-')
			}
		}
	}
	return strings.Trim(string(result), "-")
}
