package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/config"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/auth"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/rewards"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type reserveRewardRequest struct {
	PlatformUserID    uuid.UUID `json:"platform_user_id"`
	RewardID          uuid.UUID `json:"reward_id"`
	RestaurantID      int64     `json:"restaurant_id"`
	CheckoutReference string    `json:"checkout_reference"`
	OrderID           *string   `json:"order_id,omitempty"`
}

type transitionReservationRequest struct {
	OrderID *string `json:"order_id,omitempty"`
}

func registerInternalRedemptionRoutes(
	router *gin.Engine,
	rewardReader RewardReader,
	writer ReservationWriter,
	cfg config.Config,
) {
	routes := router.Group("/internal/v1/loyalty")
	routes.Use(auth.RequireInternalServiceToken(cfg.InternalServiceToken))
	routes.POST("/reservations", reserveCatalogReward(rewardReader, writer, cfg.ReservationTTL))
	routes.POST("/reservations/:reservation_id/release", transitionReservation(writer, false))
	routes.POST("/reservations/:reservation_id/capture", transitionReservation(writer, true))
}

func reserveCatalogReward(reader RewardReader, writer ReservationWriter, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request reserveRewardRequest
		if err := decodeStrictBody(c, &request); err != nil || request.PlatformUserID == uuid.Nil ||
			request.RewardID == uuid.Nil || request.RestaurantID <= 0 ||
			strings.TrimSpace(request.CheckoutReference) == "" || len(request.CheckoutReference) > 200 ||
			!validOptionalOrderID(request.OrderID) {
			writeError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
		if idempotencyKey == "" || len(idempotencyKey) > 200 {
			writeError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		existing, found, err := writer.FindReservationByIdempotency(
			c.Request.Context(), request.PlatformUserID, idempotencyKey,
		)
		if err != nil {
			writeRedemptionError(c, err)
			return
		}
		if found {
			if !sameReserveHTTPRequest(existing, request) {
				writeRedemptionError(c, ledger.ErrIdempotencyConflict)
				return
			}
			c.JSON(http.StatusOK, reservationResponse(ledger.ReservationResult{
				Reservation: existing,
				Replayed:    true,
			}))
			return
		}
		now := time.Now().UTC()
		reward, effect, err := reader.ResolveActive(c.Request.Context(), request.RewardID, request.RestaurantID, now)
		if err != nil {
			writeRedemptionError(c, err)
			return
		}
		metadata, _ := json.Marshal(map[string]string{"request_id": c.GetString("request_id")})
		restaurantID := request.RestaurantID
		result, err := writer.ReservePoints(c.Request.Context(), ledger.ReserveCommand{
			PlatformUserID: request.PlatformUserID, CheckoutReference: request.CheckoutReference,
			OrderID: request.OrderID, RestaurantID: &restaurantID, Points: reward.PointsCost,
			DiscountMinor: effect.DiscountMinor, Currency: effect.Currency, RewardID: &reward.RewardID,
			PolicyVersion: reward.PolicyVersion, FundingSource: reward.FundingSource,
			IdempotencyKey: idempotencyKey, ExpiresAt: now.Add(ttl), OccurredAt: now,
			Metadata: metadata, BenefitSnapshot: reward.Effect,
		})
		if err != nil {
			writeRedemptionError(c, err)
			return
		}
		c.JSON(http.StatusOK, reservationResponse(result))
	}
}

func sameReserveHTTPRequest(existing ledger.Reservation, request reserveRewardRequest) bool {
	return existing.CheckoutReference == request.CheckoutReference &&
		existing.RewardID != nil && *existing.RewardID == request.RewardID &&
		existing.RestaurantID != nil && *existing.RestaurantID == request.RestaurantID &&
		sameOptionalString(existing.OrderID, request.OrderID)
}

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return strings.TrimSpace(*left) == strings.TrimSpace(*right)
}

func transitionReservation(writer ReservationWriter, capture bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		reservationID, err := uuid.Parse(c.Param("reservation_id"))
		idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
		if err != nil || reservationID == uuid.Nil || idempotencyKey == "" || len(idempotencyKey) > 200 {
			writeError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		var request transitionReservationRequest
		if c.Request.ContentLength != 0 {
			if err := decodeStrictBody(c, &request); err != nil || !validOptionalOrderID(request.OrderID) {
				writeError(c, http.StatusBadRequest, "invalid_request")
				return
			}
		}
		if capture && request.OrderID == nil {
			writeError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		command := ledger.ReservationTransitionCommand{
			ReservationID: reservationID, OrderID: request.OrderID, IdempotencyKey: idempotencyKey,
			OccurredAt: time.Now().UTC(), ActorType: ledger.ActorSystem,
			Metadata: json.RawMessage(`{"source":"restaurant_service"}`),
		}
		var result ledger.ReservationResult
		if capture {
			result, err = writer.CaptureReservation(c.Request.Context(), command)
		} else {
			result, err = writer.ReleaseReservation(c.Request.Context(), command)
		}
		if err != nil {
			writeRedemptionError(c, err)
			return
		}
		c.JSON(http.StatusOK, reservationResponse(result))
	}
}

func reservationResponse(result ledger.ReservationResult) gin.H {
	return gin.H{
		"reservation_id": result.Reservation.ReservationID,
		"status":         result.Reservation.Status, "points": result.Reservation.Points,
		"discount_minor": result.Reservation.DiscountMinor, "currency": result.Reservation.Currency,
		"reward_id": result.Reservation.RewardID, "restaurant_id": result.Reservation.RestaurantID,
		"order_id": result.Reservation.OrderID, "funding_source": result.Reservation.FundingSource,
		"policy_version": result.Reservation.PolicyVersion, "expires_at": result.Reservation.ExpiresAt,
		"replayed": result.Replayed,
	}
}

func decodeStrictBody(c *gin.Context, target any) error {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func validOptionalOrderID(value *string) bool {
	return value == nil || (strings.TrimSpace(*value) != "" && len(*value) <= 200)
}

func writeRedemptionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, rewards.ErrNotFound), errors.Is(err, ledger.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found")
	case errors.Is(err, rewards.ErrUnsupportedEffect), errors.Is(err, ledger.ErrRewardUnavailable):
		writeError(c, http.StatusUnprocessableEntity, "reward_unavailable")
	case errors.Is(err, ledger.ErrInsufficientPoints):
		writeError(c, http.StatusConflict, "insufficient_points")
	case errors.Is(err, ledger.ErrUsageLimitReached):
		writeError(c, http.StatusConflict, "usage_limit_reached")
	case errors.Is(err, ledger.ErrAccountRestricted):
		writeError(c, http.StatusConflict, "account_restricted")
	case errors.Is(err, ledger.ErrIdempotencyConflict), errors.Is(err, ledger.ErrInvalidTransition):
		writeError(c, http.StatusConflict, "conflict")
	case errors.Is(err, ledger.ErrInvalidCommand):
		writeError(c, http.StatusBadRequest, "invalid_request")
	default:
		writeError(c, http.StatusServiceUnavailable, "temporarily_unavailable")
	}
}
