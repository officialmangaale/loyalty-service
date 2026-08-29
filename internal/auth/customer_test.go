package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
)

func TestRequireCustomerJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-with-at-least-thirty-two-characters"
	userID := uuid.New()
	now := time.Now().UTC()

	tests := []struct {
		name       string
		claims     jwt.MapClaims
		signingKey string
		wantStatus int
	}{
		{
			name: "valid customer",
			claims: jwt.MapClaims{
				"sub": userID.String(), "user_id": userID.String(),
				"role": "customer", "type": "customer",
				"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			},
			signingKey: secret,
			wantStatus: http.StatusNoContent,
		},
		{
			name: "mismatched user ID",
			claims: jwt.MapClaims{
				"sub": userID.String(), "user_id": uuid.New().String(),
				"role": "customer", "type": "customer",
				"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			},
			signingKey: secret,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "expired",
			claims: jwt.MapClaims{
				"sub": userID.String(), "user_id": userID.String(),
				"role": "customer", "type": "customer",
				"iat": now.Add(-2 * time.Hour).Unix(), "exp": now.Add(-time.Hour).Unix(),
			},
			signingKey: secret,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "wrong signature",
			claims: jwt.MapClaims{
				"sub": userID.String(), "user_id": userID.String(),
				"role": "customer", "type": "customer",
				"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			},
			signingKey: "different-secret-with-at-least-thirty-two-chars",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token := jwt.NewWithClaims(jwt.SigningMethodHS256, test.claims)
			signed, err := token.SignedString([]byte(test.signingKey))
			if err != nil {
				t.Fatalf("sign token: %v", err)
			}
			router := gin.New()
			router.Use(RequireCustomerJWT(secret))
			router.GET("/protected", func(c *gin.Context) {
				id, err := CustomerID(c)
				if err != nil || id != userID {
					c.Status(http.StatusInternalServerError)
					return
				}
				c.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodGet, "/protected", nil)
			request.Header.Set("Authorization", "Bearer "+signed)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestRequireCustomerJWTRejectsMissingHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequireCustomerJWT("test-secret-with-at-least-thirty-two-characters"))
	router.GET("/protected", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/protected", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}
