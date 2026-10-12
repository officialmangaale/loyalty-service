package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPort            = 8086
	defaultShutdownTimeout = 10 * time.Second
)

var ErrFoundationFlagEnabled = errors.New("loyalty foundation cannot start with business or integration flags enabled")
var ErrFeatureNotImplemented = errors.New("configured loyalty feature is not implemented in this release")

// FeatureFlags are global kill switches. Missing values always resolve false.
// Per-restaurant and cohort entitlements will be added only with an approved
// persistence model; they must never override a false global switch.
type FeatureFlags struct {
	ReadAPI          bool
	InternalAPI      bool
	EventConsumption bool
	Earning          bool
	Redemption       bool
	Referrals        bool
	Birthday         bool
	CouponAdapter    bool
}

func (f FeatureFlags) AnyEnabled() bool {
	return f.ReadAPI ||
		f.InternalAPI ||
		f.EventConsumption ||
		f.Earning ||
		f.Redemption ||
		f.Referrals ||
		f.Birthday ||
		f.CouponAdapter
}

type Config struct {
	Port                 int
	Environment          string
	LogLevel             string
	ReadHeaderTimeout    time.Duration
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	IdleTimeout          time.Duration
	ShutdownTimeout      time.Duration
	Features             FeatureFlags
	DatabaseURL          string
	CustomerJWTSecret    string
	InternalServiceToken string
	AWSRegion            string
	OrderEventsQueueURL  string
	DatabaseMaxOpenConns int
	DatabaseMaxIdleConns int
	ReservationTTL       time.Duration
}

func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	port, err := readPort(lookup)
	if err != nil {
		return Config{}, err
	}

	flags, err := readFeatureFlags(lookup)
	if err != nil {
		return Config{}, err
	}

	environment := readString(lookup, "ENVIRONMENT", "development")
	logLevel := strings.ToUpper(readString(lookup, "LOG_LEVEL", "INFO"))
	if logLevel != "DEBUG" && logLevel != "INFO" && logLevel != "WARN" && logLevel != "ERROR" {
		return Config{}, fmt.Errorf("LOG_LEVEL must be DEBUG, INFO, WARN, or ERROR")
	}
	reservationTTL, err := readDuration(lookup, "LOYALTY_RESERVATION_TTL", 2*time.Minute, 30*time.Second, 15*time.Minute)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Port:                 port,
		Environment:          environment,
		LogLevel:             logLevel,
		ReadHeaderTimeout:    5 * time.Second,
		ReadTimeout:          10 * time.Second,
		WriteTimeout:         15 * time.Second,
		IdleTimeout:          60 * time.Second,
		ShutdownTimeout:      defaultShutdownTimeout,
		Features:             flags,
		DatabaseURL:          readString(lookup, "DATABASE_URL", ""),
		CustomerJWTSecret:    readString(lookup, "CUSTOMER_JWT_SECRET", ""),
		InternalServiceToken: readString(lookup, "INTERNAL_SERVICE_TOKEN", ""),
		AWSRegion:            readString(lookup, "AWS_REGION", ""),
		OrderEventsQueueURL:  readString(lookup, "LOYALTY_ORDER_EVENTS_QUEUE_URL", ""),
		DatabaseMaxOpenConns: 20,
		DatabaseMaxIdleConns: 5,
		ReservationTTL:       reservationTTL,
	}, nil
}

func (c Config) ValidateFoundationMode() error {
	if c.Features.AnyEnabled() {
		return ErrFoundationFlagEnabled
	}
	return nil
}

func (c Config) ValidateRuntime() error {
	unsupported := c.Features.Referrals || c.Features.Birthday || c.Features.CouponAdapter
	if unsupported {
		return ErrFeatureNotImplemented
	}
	if c.Features.ReadAPI || c.Features.EventConsumption || c.Features.InternalAPI {
		if strings.TrimSpace(c.DatabaseURL) == "" {
			return fmt.Errorf("DATABASE_URL is required when a database-backed loyalty feature is enabled")
		}
	}
	if c.Features.ReadAPI {
		if strings.TrimSpace(c.CustomerJWTSecret) == "" {
			return fmt.Errorf("CUSTOMER_JWT_SECRET is required when the read API is enabled")
		}
	}
	if c.Features.EventConsumption {
		if strings.TrimSpace(c.AWSRegion) == "" || strings.TrimSpace(c.OrderEventsQueueURL) == "" {
			return fmt.Errorf("AWS_REGION and LOYALTY_ORDER_EVENTS_QUEUE_URL are required when event consumption is enabled")
		}
	}
	if c.Features.Earning && !c.Features.EventConsumption {
		return fmt.Errorf("LOYALTY_EVENT_CONSUMPTION_ENABLED must be true when earning is enabled")
	}
	if c.Features.InternalAPI {
		if len(c.InternalServiceToken) < 32 {
			return fmt.Errorf("INTERNAL_SERVICE_TOKEN must contain at least 32 characters when the internal API is enabled")
		}
	}
	if c.Features.Redemption && !c.Features.InternalAPI {
		return fmt.Errorf("LOYALTY_INTERNAL_API_ENABLED must be true when redemption is enabled")
	}
	return nil
}

func readPort(lookup func(string) (string, bool)) (int, error) {
	raw, ok := lookup("PORT")
	if !ok || strings.TrimSpace(raw) == "" {
		return defaultPort, nil
	}

	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("PORT must be an integer between 1 and 65535")
	}
	return port, nil
}

func readFeatureFlags(lookup func(string) (string, bool)) (FeatureFlags, error) {
	values := []struct {
		name   string
		target *bool
	}{
		{name: "LOYALTY_READ_API_ENABLED"},
		{name: "LOYALTY_INTERNAL_API_ENABLED"},
		{name: "LOYALTY_EVENT_CONSUMPTION_ENABLED"},
		{name: "LOYALTY_EARNING_ENABLED"},
		{name: "LOYALTY_REDEMPTION_ENABLED"},
		{name: "LOYALTY_REFERRALS_ENABLED"},
		{name: "LOYALTY_BIRTHDAY_ENABLED"},
		{name: "LOYALTY_COUPON_ADAPTER_ENABLED"},
	}

	flags := FeatureFlags{}
	values[0].target = &flags.ReadAPI
	values[1].target = &flags.InternalAPI
	values[2].target = &flags.EventConsumption
	values[3].target = &flags.Earning
	values[4].target = &flags.Redemption
	values[5].target = &flags.Referrals
	values[6].target = &flags.Birthday
	values[7].target = &flags.CouponAdapter

	for _, value := range values {
		parsed, err := readBool(lookup, value.name)
		if err != nil {
			return FeatureFlags{}, err
		}
		*value.target = parsed
	}

	return flags, nil
}

func readBool(lookup func(string) (string, bool), name string) (bool, error) {
	raw, ok := lookup(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return false, nil
	}

	parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return parsed, nil
}

func readString(lookup func(string) (string, bool), name, fallback string) string {
	raw, ok := lookup(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	return strings.TrimSpace(raw)
}

func readDuration(lookup func(string) (string, bool), name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw, ok := lookup(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be a duration between %s and %s", name, minimum, maximum)
	}
	return value, nil
}
