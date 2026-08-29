package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

func RequireInternalServiceToken(expected string) gin.HandlerFunc {
	expectedHash := sha256.Sum256([]byte(expected))
	return func(c *gin.Context) {
		presented, ok := bearerToken(c.GetHeader("Authorization"))
		presentedHash := sha256.Sum256([]byte(presented))
		if !ok || subtle.ConstantTimeCompare(expectedHash[:], presentedHash[:]) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":      "unauthorized",
				"request_id": c.GetString("request_id"),
			})
			return
		}
		c.Next()
	}
}
