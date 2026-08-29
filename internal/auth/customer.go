package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
)

const customerIDContextKey = "platform_user_id"

type customerClaims struct {
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
	Type     string `json:"type"`
	UserType string `json:"user_type"`
	jwt.RegisteredClaims
}

func RequireCustomerJWT(secret string) gin.HandlerFunc {
	key := []byte(secret)
	return func(c *gin.Context) {
		tokenString, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			unauthorized(c)
			return
		}

		claims := &customerClaims{}
		token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, jwt.ErrSignatureInvalid
			}
			return key, nil
		}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
		if err != nil || !token.Valid || claims.ExpiresAt == nil || claims.IssuedAt == nil {
			unauthorized(c)
			return
		}
		if claims.IssuedAt.Time.After(time.Now().UTC().Add(time.Minute)) {
			unauthorized(c)
			return
		}
		if !strings.EqualFold(claims.Role, "customer") ||
			(!strings.EqualFold(claims.Type, "customer") && !strings.EqualFold(claims.UserType, "customer")) {
			unauthorized(c)
			return
		}

		subjectID, err := uuid.Parse(claims.Subject)
		if err != nil {
			unauthorized(c)
			return
		}
		userID, err := uuid.Parse(claims.UserID)
		if err != nil || userID != subjectID {
			unauthorized(c)
			return
		}
		c.Set(customerIDContextKey, userID)
		c.Next()
	}
}

func CustomerID(c *gin.Context) (uuid.UUID, error) {
	value, ok := c.Get(customerIDContextKey)
	if !ok {
		return uuid.Nil, errors.New("customer identity is absent")
	}
	id, ok := value.(uuid.UUID)
	if !ok || id == uuid.Nil {
		return uuid.Nil, errors.New("customer identity is invalid")
	}
	return id, nil
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func unauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error":      "unauthorized",
		"request_id": c.GetString("request_id"),
	})
}
