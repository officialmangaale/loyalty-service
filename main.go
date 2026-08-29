package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/config"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/httpserver"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/maintenance"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/orderevents"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/rewards"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/migrations"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	_ "github.com/lib/pq"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if err := cfg.ValidateRuntime(); err != nil {
		slog.Error("unsafe runtime configuration refused", "error", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	dependency, db, closeDatabase, err := runtimeDependencies(cfg)
	if err != nil {
		logger.Error("initialize runtime dependencies", "error", err)
		os.Exit(1)
	}
	defer closeDatabase()
	stopConsumer, err := startOrderEventConsumer(cfg, db, logger)
	if err != nil {
		logger.Error("initialize order event consumer", "error", err)
		os.Exit(1)
	}
	defer stopConsumer()
	stopExpiry, err := startExpiryWorker(cfg, db, logger)
	if err != nil {
		logger.Error("initialize expiry worker", "error", err)
		os.Exit(1)
	}
	defer stopExpiry()

	router, err := httpserver.NewRouter(cfg, logger, dependency)
	if err != nil {
		logger.Error("initialize HTTP router", "error", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           router,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	serverErrors := make(chan error, 1)
	go func() {
		mode := runtimeMode(cfg)
		logger.Info("loyalty service started",
			"address", server.Addr,
			"environment", cfg.Environment,
			"mode", mode,
		)
		serverErrors <- server.ListenAndServe()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	select {
	case received := <-signals:
		logger.Info("shutdown signal received", "signal", received.String())
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("loyalty service stopped")
}

func runtimeMode(cfg config.Config) string {
	switch {
	case cfg.Features.ReadAPI && cfg.Features.Redemption:
		return "read_redemption"
	case cfg.Features.ReadAPI && cfg.Features.Earning:
		return "read_earning"
	case cfg.Features.ReadAPI && cfg.Features.EventConsumption:
		return "read_consumer"
	case cfg.Features.Earning:
		return "earning"
	case cfg.Features.EventConsumption:
		return "consumer"
	case cfg.Features.Redemption:
		return "redemption"
	case cfg.Features.ReadAPI:
		return "read_only"
	default:
		return "foundation"
	}
}

func runtimeDependencies(cfg config.Config) (httpserver.Dependencies, *sql.DB, func(), error) {
	if !cfg.Features.ReadAPI && !cfg.Features.EventConsumption && !cfg.Features.InternalAPI {
		return httpserver.Dependencies{}, nil, func() {}, nil
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return httpserver.Dependencies{}, nil, func() {}, fmt.Errorf("open loyalty database: %w", err)
	}
	closeDatabase := func() { _ = db.Close() }
	db.SetMaxOpenConns(cfg.DatabaseMaxOpenConns)
	db.SetMaxIdleConns(cfg.DatabaseMaxIdleConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		closeDatabase()
		return httpserver.Dependencies{}, nil, func() {}, fmt.Errorf("ping loyalty database: %w", err)
	}
	statuses, err := migrations.Current(ctx, db)
	if err != nil {
		closeDatabase()
		return httpserver.Dependencies{}, nil, func() {}, err
	}
	if err := migrations.ValidateCurrent(statuses); err != nil {
		closeDatabase()
		return httpserver.Dependencies{}, nil, func() {}, fmt.Errorf("loyalty schema is not current: %w", err)
	}

	store := ledger.NewStore(db)
	dependency := httpserver.Dependencies{
		Readiness: db.PingContext,
	}
	if cfg.Features.ReadAPI {
		dependency.WalletReader = store
		dependency.RewardReader = rewards.NewStore(db)
	}
	if cfg.Features.Redemption {
		dependency.RewardReader = rewards.NewStore(db)
		dependency.ReservationWriter = store
	}
	return dependency, db, closeDatabase, nil
}

func startOrderEventConsumer(cfg config.Config, db *sql.DB, logger *slog.Logger) (func(), error) {
	if !cfg.Features.EventConsumption {
		return func() {}, nil
	}
	if db == nil {
		return func() {}, errors.New("event consumer database is absent")
	}
	ctx, cancelLoad := context.WithTimeout(context.Background(), 10*time.Second)
	awsConfiguration, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	cancelLoad()
	if err != nil {
		return func() {}, fmt.Errorf("load AWS configuration: %w", err)
	}
	processor, err := orderevents.NewProcessor(db, ledger.NewStore(db), cfg.Features.Earning)
	if err != nil {
		return func() {}, err
	}
	consumer, err := orderevents.NewSQSConsumer(
		sqs.NewFromConfig(awsConfiguration), cfg.OrderEventsQueueURL, processor, logger,
	)
	if err != nil {
		return func() {}, err
	}
	consumerContext, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(consumerContext)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(12 * time.Second):
			logger.Warn("order event consumer shutdown timed out")
		}
	}, nil
}

func startExpiryWorker(cfg config.Config, db *sql.DB, logger *slog.Logger) (func(), error) {
	if !cfg.Features.Earning && !cfg.Features.Redemption {
		return func() {}, nil
	}
	if db == nil {
		return func() {}, errors.New("expiry worker database is absent")
	}
	worker, err := maintenance.NewExpiryWorker(ledger.NewStore(db), logger)
	if err != nil {
		return func() {}, err
	}
	workerContext, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(workerContext)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			logger.Warn("expiry worker shutdown timed out")
		}
	}, nil
}

func newLogger(level string) *slog.Logger {
	logLevel := slog.LevelInfo
	switch strings.ToUpper(level) {
	case "DEBUG":
		logLevel = slog.LevelDebug
	case "WARN":
		logLevel = slog.LevelWarn
	case "ERROR":
		logLevel = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
}
