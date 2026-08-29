package httpserver

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/auth"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/gin-gonic/gin"
)

const (
	defaultTransactionLimit = 50
	maximumTransactionLimit = 100
)

type walletResponse struct {
	Status               string    `json:"status"`
	Timezone             string    `json:"timezone"`
	AvailablePoints      int64     `json:"available_points"`
	PendingPoints        int64     `json:"pending_points"`
	ReservedPoints       int64     `json:"reserved_points"`
	DebtPoints           int64     `json:"debt_points"`
	LifetimeEarnedPoints int64     `json:"lifetime_earned_points"`
	UpdatedAt            time.Time `json:"updated_at,omitempty"`
}

type transactionResponse struct {
	LedgerID       string               `json:"ledger_id"`
	Type           ledger.EntryType     `json:"type"`
	AvailableDelta int64                `json:"available_delta"`
	PendingDelta   int64                `json:"pending_delta"`
	ReservedDelta  int64                `json:"reserved_delta"`
	DebtDelta      int64                `json:"debt_delta"`
	ReferenceType  string               `json:"reference_type"`
	ReferenceID    string               `json:"reference_id"`
	OrderID        *string              `json:"order_id,omitempty"`
	RestaurantID   *int64               `json:"restaurant_id,omitempty"`
	CampaignID     *string              `json:"campaign_id,omitempty"`
	RewardID       *string              `json:"reward_id,omitempty"`
	FundingSource  ledger.FundingSource `json:"funding_source"`
	PolicyVersion  string               `json:"policy_version"`
	ExpiresAt      *time.Time           `json:"expires_at,omitempty"`
	OccurredAt     time.Time            `json:"occurred_at"`
	CreatedAt      time.Time            `json:"created_at"`
}

func registerCustomerReadRoutes(router *gin.Engine, reader WalletReader, rewardReader RewardReader, secret string) {
	routes := router.Group("/v1/loyalty")
	routes.Use(auth.RequireCustomerJWT(secret))
	routes.GET("/wallet", getWallet(reader))
	routes.GET("/transactions", listTransactions(reader))
	routes.GET("/rewards", listRewards(rewardReader))
}

func getWallet(reader WalletReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, err := auth.CustomerID(c)
		if err != nil {
			writeError(c, http.StatusUnauthorized, "unauthorized")
			return
		}

		account, err := reader.GetAccount(c.Request.Context(), customerID)
		if errors.Is(err, ledger.ErrNotFound) {
			c.JSON(http.StatusOK, walletResponse{Status: "NOT_ENROLLED", Timezone: "UTC"})
			return
		}
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "temporarily_unavailable")
			return
		}

		c.JSON(http.StatusOK, walletResponse{
			Status:               account.Status,
			Timezone:             account.Timezone,
			AvailablePoints:      account.AvailablePoints,
			PendingPoints:        account.PendingPoints,
			ReservedPoints:       account.ReservedPoints,
			DebtPoints:           account.DebtPoints,
			LifetimeEarnedPoints: account.LifetimeEarnedPoints,
			UpdatedAt:            account.UpdatedAt,
		})
	}
}

func listTransactions(reader WalletReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, err := auth.CustomerID(c)
		if err != nil {
			writeError(c, http.StatusUnauthorized, "unauthorized")
			return
		}

		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			writeError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		cursor, err := ledger.ParseEntryCursor(c.Query("cursor"))
		if err != nil {
			writeError(c, http.StatusBadRequest, "invalid_request")
			return
		}

		page, err := reader.ListEntries(c.Request.Context(), customerID, limit, cursor)
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "temporarily_unavailable")
			return
		}
		entries := make([]transactionResponse, 0, len(page.Entries))
		for _, entry := range page.Entries {
			entries = append(entries, publicTransaction(entry))
		}
		c.JSON(http.StatusOK, gin.H{
			"transactions": entries,
			"next_cursor":  ledger.FormatEntryCursor(page.NextCursor),
		})
	}
}

func parseLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultTransactionLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maximumTransactionLimit {
		return 0, ledger.ErrInvalidCommand
	}
	return limit, nil
}

func listRewards(reader RewardReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			writeError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		var restaurantID *int64
		if raw := strings.TrimSpace(c.Query("restaurant_id")); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value <= 0 {
				writeError(c, http.StatusBadRequest, "invalid_request")
				return
			}
			restaurantID = &value
		}
		rewardList, err := reader.ListActive(c.Request.Context(), restaurantID, limit, time.Now().UTC())
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "temporarily_unavailable")
			return
		}
		items := make([]gin.H, 0, len(rewardList))
		for _, reward := range rewardList {
			items = append(items, gin.H{
				"reward_id": reward.RewardID, "code": reward.Code, "name": reward.Name,
				"description": reward.Description, "type": reward.Type, "scope": reward.Scope,
				"restaurant_id": reward.RestaurantID, "points_cost": reward.PointsCost,
				"effect": reward.Effect, "funding_source": reward.FundingSource,
				"policy_version": reward.PolicyVersion, "starts_at": reward.StartsAt, "ends_at": reward.EndsAt,
			})
		}
		c.JSON(http.StatusOK, gin.H{"rewards": items})
	}
}

func publicTransaction(entry ledger.Entry) transactionResponse {
	response := transactionResponse{
		LedgerID:       entry.LedgerID.String(),
		Type:           entry.Type,
		AvailableDelta: entry.AvailableDelta,
		PendingDelta:   entry.PendingDelta,
		ReservedDelta:  entry.ReservedDelta,
		DebtDelta:      entry.DebtDelta,
		ReferenceType:  entry.ReferenceType,
		ReferenceID:    entry.ReferenceID,
		OrderID:        entry.OrderID,
		RestaurantID:   entry.RestaurantID,
		FundingSource:  entry.FundingSource,
		PolicyVersion:  entry.PolicyVersion,
		ExpiresAt:      entry.ExpiresAt,
		OccurredAt:     entry.OccurredAt,
		CreatedAt:      entry.CreatedAt,
	}
	if entry.CampaignID != nil {
		value := entry.CampaignID.String()
		response.CampaignID = &value
	}
	if entry.RewardID != nil {
		value := entry.RewardID.String()
		response.RewardID = &value
	}
	return response
}

func writeError(c *gin.Context, status int, code string) {
	c.JSON(status, gin.H{
		"error":      code,
		"request_id": c.GetString("request_id"),
	})
}
