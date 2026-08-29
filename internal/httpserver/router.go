package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/config"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/rewards"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const requestIDHeader = "X-Request-ID"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type WalletReader interface {
	GetAccount(context.Context, uuid.UUID) (ledger.Account, error)
	ListEntries(context.Context, uuid.UUID, int, *ledger.EntryCursor) (ledger.EntryPage, error)
}

type RewardReader interface {
	ListActive(context.Context, *int64, int, time.Time) ([]rewards.Reward, error)
	ResolveActive(context.Context, uuid.UUID, int64, time.Time) (rewards.Reward, rewards.FixedDiscount, error)
}

type ReservationWriter interface {
	FindReservationByIdempotency(context.Context, uuid.UUID, string) (ledger.Reservation, bool, error)
	ReservePoints(context.Context, ledger.ReserveCommand) (ledger.ReservationResult, error)
	ReleaseReservation(context.Context, ledger.ReservationTransitionCommand) (ledger.ReservationResult, error)
	CaptureReservation(context.Context, ledger.ReservationTransitionCommand) (ledger.ReservationResult, error)
}

type Dependencies struct {
	WalletReader      WalletReader
	RewardReader      RewardReader
	ReservationWriter ReservationWriter
	Readiness         func(context.Context) error
}

func NewRouter(cfg config.Config, logger *slog.Logger, dependencies ...Dependencies) (*gin.Engine, error) {
	if len(dependencies) > 1 {
		return nil, errors.New("only one dependency set may be supplied")
	}
	if cfg.Environment == "development" && cfg.LogLevel == "DEBUG" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	var dependency Dependencies
	if len(dependencies) > 0 {
		dependency = dependencies[0]
	}
	if cfg.Features.ReadAPI && dependency.WalletReader == nil {
		return nil, errors.New("wallet reader is required when the loyalty read API is enabled")
	}
	if cfg.Features.ReadAPI && dependency.RewardReader == nil {
		return nil, errors.New("reward reader is required when the loyalty read API is enabled")
	}
	if cfg.Features.Redemption && (dependency.RewardReader == nil || dependency.ReservationWriter == nil) {
		return nil, errors.New("reward reader and reservation writer are required when redemption is enabled")
	}

	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		panic("disable trusted proxies: " + err.Error())
	}

	router.Use(gin.Recovery())
	router.Use(requestID())
	router.Use(securityHeaders())
	router.Use(accessLog(logger))

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service": "loyalty-service",
			"status":  "ok",
		})
	})

	router.GET("/ready", func(c *gin.Context) {
		if dependency.Readiness != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
			defer cancel()
			if err := dependency.Readiness(ctx); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"service": "loyalty-service",
					"status":  "not_ready",
				})
				return
			}
		}
		mode := "foundation"
		switch {
		case cfg.Features.ReadAPI && cfg.Features.Redemption:
			mode = "read_redemption"
		case cfg.Features.ReadAPI && cfg.Features.Earning:
			mode = "read_earning"
		case cfg.Features.ReadAPI && cfg.Features.EventConsumption:
			mode = "read_consumer"
		case cfg.Features.Earning:
			mode = "earning"
		case cfg.Features.EventConsumption:
			mode = "consumer"
		case cfg.Features.Redemption:
			mode = "redemption"
		case cfg.Features.ReadAPI:
			mode = "read_only"
		}
		c.JSON(http.StatusOK, gin.H{
			"mode":    mode,
			"service": "loyalty-service",
			"status":  "ready",
		})
	})

	if cfg.Features.ReadAPI {
		registerCustomerReadRoutes(router, dependency.WalletReader, dependency.RewardReader, cfg.CustomerJWTSecret)
	}
	if cfg.Features.Redemption {
		registerInternalRedemptionRoutes(router, dependency.RewardReader, dependency.ReservationWriter, cfg)
	}

	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{
			"error":      "not_found",
			"request_id": c.GetString("request_id"),
		})
	})

	return router, nil
}

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		c.Set("request_id", id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

func newRequestID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(bytes)
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		headers := c.Writer.Header()
		headers.Set("Cache-Control", "no-store")
		headers.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		headers.Set("Referrer-Policy", "no-referrer")
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("X-Frame-Options", "DENY")
		headers.Del("Server")
		c.Next()
	}
}

func accessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()

		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		logger.Info("http_request",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(started).Milliseconds(),
			"request_id", c.GetString("request_id"),
		)
	}
}
