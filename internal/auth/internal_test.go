package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestInternalServiceToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequireInternalServiceToken("0123456789abcdef0123456789abcdef"))
	router.GET("/internal", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for _, test := range []struct {
		header string
		want   int
	}{
		{want: http.StatusUnauthorized},
		{header: "Bearer wrong", want: http.StatusUnauthorized},
		{header: "Bearer 0123456789abcdef0123456789abcdef", want: http.StatusNoContent},
	} {
		request := httptest.NewRequest(http.MethodGet, "/internal", nil)
		request.Header.Set("Authorization", test.header)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("header %q status = %d, want %d", test.header, response.Code, test.want)
		}
	}
}
