package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRuntimeBridgeBlocksUnsupportedOperationsBeforeHandler(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{http.MethodPost, "/sandboxes", true},
		{http.MethodGet, "/v2/sandboxes", true},
		{http.MethodGet, "/sandboxes/sandbox01", true},
		{http.MethodDelete, "/sandboxes/sandbox01", true},
		{http.MethodPost, "/sandboxes/sandbox01/timeout", true},
		{http.MethodPost, "/sandboxes/sandbox01/pause", false},
		{http.MethodPost, "/sandboxes/sandbox01/fork", false},
		{http.MethodPost, "/sandboxes/sandbox01/connect", false},
		{http.MethodPost, "/v2/templates/template/builds/build", false},
		{http.MethodPost, "/volumes", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			router := gin.New()
			router.Use(RuntimeBridgePOC())
			called := false
			router.Any("/*path", func(c *gin.Context) { called = true; c.Status(http.StatusNoContent) })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))
			if called != tc.allowed {
				t.Fatalf("handler invoked=%v, expected %v", called, tc.allowed)
			}
			if !tc.allowed && response.Code != http.StatusNotImplemented {
				t.Fatal(response.Code)
			}
		})
	}
}
