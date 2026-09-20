package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type storageState bool

func (s storageState) StorageReady() bool { return bool(s) }

func TestHealthEndpoints(t *testing.T) {
	for _, storage := range []StorageHealth{nil, storageState(false), storageState(true)} {
		r := gin.New()
		registerHealth(r, storage)
		for _, tc := range []struct {
			path string
			code int
			body string
		}{
			{"/health/live", http.StatusOK, "live"},
			{"/health/ready/accept", http.StatusServiceUnavailable, "MQ_ACCEPTANCE_NOT_CONFIGURED"},
			{"/health/ready/storage", http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE"},
		} {
			if tc.path == "/health/ready/storage" && storage != nil && storage.StorageReady() {
				tc.code, tc.body = http.StatusOK, `"status":"ready"`
			}
			response := httptest.NewRecorder()
			r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if response.Code != tc.code || !strings.Contains(response.Body.String(), tc.body) {
				t.Fatalf("%s: %d %s", tc.path, response.Code, response.Body.String())
			}
			if strings.Contains(tc.path, "/ready/") && response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("readiness must not be cached")
			}
		}
	}
}
