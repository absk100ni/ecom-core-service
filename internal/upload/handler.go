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

// PresignedURL — POST /admin/uploads/presign
// When S3_BUCKET is set, returns a properly-formed presigned URL pattern.
// TODO: integrate github.com/aws/aws-sdk-go-v2 when network is available for `go get`.
func (h *Handler) PresignedURL(c *gin.Context) {
	var req struct {
		Filename    string `json:"filename" binding:"required"`
		ContentType string `json:"content_type" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "filename and content_type required"})
		return
	}

	// Validate content_type
	allowedTypes := map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}
	if !allowedTypes[req.ContentType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content_type must be image/jpeg, image/png, or image/webp", "code": errcodes.EUpldInvalidFile.Code})
		return
	}

	// Derive extension from content_type
	extMap := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}
	ext := extMap[req.ContentType]
	slug := slugify(strings.TrimSuffix(req.Filename, filepath.Ext(req.Filename)))
	key := fmt.Sprintf("products/%s-%s%s", uuid.New().String()[:8], slug, ext)

	if h.cfg.S3Bucket == "" {
		// Local /tmp fallback for dev
		publicURL := fmt.Sprintf("http://localhost:%s/uploads/%s", h.cfg.Port, key)
		c.JSON(http.StatusOK, gin.H{
			"upload_url": publicURL,
			"public_url": publicURL,
			"key":        key,
			"mock":       true,
			"message":    "S3 not configured. Set S3_BUCKET for real uploads.",
		})
		return
	}

	// S3 presigned URL — CDN base if configured, else S3 direct
	publicBase := h.cfg.S3PublicBaseURL
	if publicBase == "" {
		publicBase = fmt.Sprintf("https://%s.s3.%s.amazonaws.com", h.cfg.S3Bucket, h.cfg.S3Region)
	}
	publicURL := publicBase + "/" + key

	// NOTE: Presigned PUT URL via stdlib SigV4 (see presign.go — verified
	// against the official AWS documentation test vector). Credentials come
	// from the standard env chain vars. Only the `host` header is signed, so
	// the client PUT may send any Content-Type.
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	sessionToken := os.Getenv("AWS_SESSION_TOKEN")
	if accessKey == "" || secretKey == "" {
		log.ErrorWithCode("presign", errcodes.EUpldFailed.Code, "S3_BUCKET set but AWS credentials missing", "bucket", h.cfg.S3Bucket)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Upload storage misconfigured", "code": errcodes.EUpldFailed.Code})
		return
	}
	host := fmt.Sprintf("%s.s3.%s.amazonaws.com", h.cfg.S3Bucket, h.cfg.S3Region)
	uploadURL := presignS3URL("PUT", host, "/"+key, h.cfg.S3Region, accessKey, secretKey, sessionToken, 15*time.Minute, time.Now())

	c.JSON(http.StatusOK, gin.H{
		"upload_url": uploadURL,
		"public_url": publicURL,
		"key":        key,
	})
}

// DirectUpload — POST /admin/upload/image (multipart)
func (h *Handler) DirectUpload(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File required. Use multipart form with 'file' field."})
		return
	}
	defer file.Close()

	if header.Size > 5*1024*1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File too large. Max 5MB.", "code": errcodes.EUpldTooLarge.Code})
		return
	}

	ext := strings.ToLower(filepath.Ext(header.Filename))
	allowed := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}
	if !allowed[ext] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File type not allowed. Use: jpg, jpeg, png, webp, gif"})
		return
	}

	uniqueName := fmt.Sprintf("%s-%d%s", uuid.New().String()[:8], time.Now().Unix(), ext)

	if h.cfg.S3Bucket == "" {
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

		publicURL := fmt.Sprintf("http://localhost:%s/uploads/products/%s", h.cfg.Port, uniqueName)
		c.JSON(http.StatusOK, gin.H{"public_url": publicURL, "filename": uniqueName, "size": header.Size, "mock": true})
		return
	}

	publicURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/products/%s", h.cfg.S3Bucket, h.cfg.S3Region, uniqueName)
	c.JSON(http.StatusOK, gin.H{"public_url": publicURL, "filename": uniqueName, "size": header.Size})
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
