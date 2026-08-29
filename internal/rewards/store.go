package rewards

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/google/uuid"
)

var (
	ErrNotFound          = errors.New("reward not found")
	ErrUnsupportedEffect = errors.New("reward effect is not supported for reservation")
)

type Reward struct {
	RewardID      uuid.UUID
	Code          string
	Name          string
	Description   string
	Type          string
	Scope         string
	RestaurantID  *int64
	PointsCost    int64
	Effect        json.RawMessage
	FundingSource ledger.FundingSource
	PolicyVersion string
	StartsAt      *time.Time
	EndsAt        *time.Time
}

type FixedDiscount struct {
	DiscountMinor int64  `json:"discount_minor"`
	Currency      string `json:"currency"`
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) ListActive(ctx context.Context, restaurantID *int64, limit int, asOf time.Time) ([]Reward, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if asOf.IsZero() {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT reward_id, code, name, description, reward_type, scope,
		       restaurant_id, points_cost, effect, funding_source,
		       policy_version, starts_at, ends_at
		FROM loyalty.reward_catalog
		WHERE status = 'ACTIVE'
		  AND (starts_at IS NULL OR starts_at <= $1)
		  AND (ends_at IS NULL OR ends_at > $1)
		  AND (scope = 'GLOBAL_MANGAALE' OR restaurant_id = $2)
		ORDER BY points_cost, reward_id
		LIMIT $3`, asOf, nullableInt64(restaurantID), limit)
	if err != nil {
		return nil, fmt.Errorf("list active rewards: %w", err)
	}
	defer rows.Close()
	var rewards []Reward
	for rows.Next() {
		reward, err := scanReward(rows)
		if err != nil {
			return nil, err
		}
		rewards = append(rewards, reward)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rewards, nil
}

func (s *Store) ResolveActive(ctx context.Context, rewardID uuid.UUID, restaurantID int64, asOf time.Time) (Reward, FixedDiscount, error) {
	if rewardID == uuid.Nil || restaurantID <= 0 || asOf.IsZero() {
		return Reward{}, FixedDiscount{}, ErrNotFound
	}
	reward, err := scanReward(s.db.QueryRowContext(ctx, `
		SELECT reward_id, code, name, description, reward_type, scope,
		       restaurant_id, points_cost, effect, funding_source,
		       policy_version, starts_at, ends_at
		FROM loyalty.reward_catalog
		WHERE reward_id = $1 AND status = 'ACTIVE'
		  AND (starts_at IS NULL OR starts_at <= $2)
		  AND (ends_at IS NULL OR ends_at > $2)
		  AND (scope = 'GLOBAL_MANGAALE' OR restaurant_id = $3)`, rewardID, asOf, restaurantID))
	if err != nil {
		return Reward{}, FixedDiscount{}, err
	}
	if reward.Type != "FIXED_DISCOUNT" {
		return Reward{}, FixedDiscount{}, ErrUnsupportedEffect
	}
	effect, err := decodeFixedDiscount(reward.Effect)
	if err != nil {
		return Reward{}, FixedDiscount{}, err
	}
	return reward, effect, nil
}

func decodeFixedDiscount(raw []byte) (FixedDiscount, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var effect FixedDiscount
	if err := decoder.Decode(&effect); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		effect.DiscountMinor <= 0 || !currencyPattern(effect.Currency) {
		return FixedDiscount{}, ErrUnsupportedEffect
	}
	effect.Currency = strings.ToUpper(effect.Currency)
	return effect, nil
}

func scanReward(row interface{ Scan(...any) error }) (Reward, error) {
	var reward Reward
	var restaurantID sql.NullInt64
	var startsAt, endsAt sql.NullTime
	err := row.Scan(&reward.RewardID, &reward.Code, &reward.Name, &reward.Description,
		&reward.Type, &reward.Scope, &restaurantID, &reward.PointsCost, &reward.Effect,
		&reward.FundingSource, &reward.PolicyVersion, &startsAt, &endsAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Reward{}, ErrNotFound
	}
	if err != nil {
		return Reward{}, fmt.Errorf("scan reward: %w", err)
	}
	if restaurantID.Valid {
		value := restaurantID.Int64
		reward.RestaurantID = &value
	}
	if startsAt.Valid {
		value := startsAt.Time
		reward.StartsAt = &value
	}
	if endsAt.Valid {
		value := endsAt.Time
		reward.EndsAt = &value
	}
	return reward, nil
}

func currencyPattern(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 3 {
		return false
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
