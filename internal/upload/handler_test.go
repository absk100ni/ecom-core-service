package upload

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ecom-core-service/internal/config"

	"github.com/gin-gonic/gin"
)

func TestProductionPresignRequiresPrivateDeliveryOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(&config.Config{
		Environment: "production",
		S3Bucket:    "catalog-images",
		S3Region:    "ap-south-1",
	})
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/admin/uploads/presign", strings.NewReader(`{"filename":"light.jpg","content_type":"image/jpeg"}`))
	context.Request.Header.Set("Content-Type", "application/json")

	handler.PresignedURL(context)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "S3_PUBLIC_BASE_URL") {
		t.Fatalf("response must explain the missing private delivery origin: %s", recorder.Body.String())
	}
}
