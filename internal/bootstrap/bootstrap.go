// Package bootstrap wires the application together: database, repository,
// handlers, HTTP server and worker. Both cmd/api and the end-to-end tests
// build the system through this package.
package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/platform/clock"
	"github.com/ahsanulks/social-scheduler/internal/platform/idgen"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/httpapi"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/sqlite"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/scheduler"
)

type Config struct {
	DatabasePath         string
	APIToken             string
	DefaultPlatform      string
	Publishers           []app.Publisher
	Tick                 time.Duration
	Batch                int
	Lease                time.Duration
	MaxAttempts          int
	IdempotencyRetention time.Duration
	Logger               *slog.Logger
}

type App struct {
	Handler  http.Handler
	Worker   *scheduler.Worker
	DB       *sql.DB
	Registry *app.Registry
}

func New(ctx context.Context, cfg Config) (*App, error) {
	if cfg.APIToken == "" {
		return nil, errors.New("bootstrap: API token is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 5 * time.Second
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 10
	}
	if cfg.Lease <= 0 {
		cfg.Lease = domain.DefaultLease
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.DefaultPlatform == "" {
		cfg.DefaultPlatform = "threads"
	}

	db, err := sqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	if err := sqlite.Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	registry, err := app.NewRegistry(cfg.Publishers...)
	if err != nil {
		db.Close()
		return nil, err
	}

	clk := clock.System{}
	ids := idgen.UUIDv7{}
	repo := sqlite.NewRepo(db)
	read := sqlite.NewReadRepo(db)

	create := app.NewSchedulePostHandler(repo, ids, registry, clk)
	update := app.NewUpdateScheduleHandler(repo, registry, clk)
	cancel := app.NewCancelScheduleHandler(repo, clk)
	get := app.NewGetScheduleHandler(read)
	list := app.NewListSchedulesHandler(read)

	idempotency := sqlite.NewIdempotencyStore(db)
	server := httpapi.New(httpapi.Deps{
		Idempotency:     idempotency,
		Create:          create,
		Update:          update,
		Cancel:          cancel,
		Get:             get,
		List:            list,
		Registry:        registry,
		Clock:           clk,
		Token:           cfg.APIToken,
		DefaultPlatform: cfg.DefaultPlatform,
		Logger:          log,
		Ready:           func(ctx context.Context) error { return db.PingContext(ctx) },
	})

	publish := app.NewPublishPostHandler(repo, registry, clk, log, cfg.Lease, cfg.MaxAttempts)
	recover := app.NewRecoverExpiredLeasesHandler(repo, clk, log, 100)
	worker := scheduler.New(repo, registry, recover, publish, clk,
		idempotency, cfg.IdempotencyRetention, cfg.Tick, cfg.Batch, log)

	return &App{
		Handler:  server.Handler(),
		Worker:   worker,
		DB:       db,
		Registry: registry,
	}, nil
}

func (a *App) Close() error {
	if a.DB == nil {
		return nil
	}
	return a.DB.Close()
}
