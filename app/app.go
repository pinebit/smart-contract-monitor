package app

import (
	"context"
	"path"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	_ "github.com/joho/godotenv/autoload"
)

type App interface {
	Run()
	Close()
}

type AppContext interface {
	Context() context.Context
	Logger(name string) *zap.SugaredLogger
	Config() *Config
	Contracts() []Contract
}

type app struct {
	ctx       context.Context
	logger    *zap.SugaredLogger
	config    *Config
	contracts []Contract
}

func NewApp(configPath string) App {
	zapLogger, _ := zap.NewProduction()
	logger := zapLogger.Sugar()

	logger.Debugf("Loading config from %s...", configPath)
	config, err := LoadConfigJSON(configPath)
	if err != nil {
		logger.Fatalf("Failed to read config file: %v", err)
	}

	logger.Debug("Configuring contracts...")
	contracts, err := LoadContracts(config, path.Dir(configPath))
	if err != nil {
		logger.Fatalf("Failed to configure contracts: %v", err)
	}

	return &app{
		logger:    logger,
		config:    config,
		contracts: contracts,
	}
}

func (a app) Context() context.Context {
	return a.ctx
}

func (a app) Logger(name string) *zap.SugaredLogger {
	return a.logger.Named(name)
}

func (a app) Config() *Config {
	return a.config
}

func (a app) Contracts() []Contract {
	return a.contracts
}

func (a app) Close() {
	a.logger.Sync()
}

func (a *app) Run() {
	ctx, cancel := context.WithCancel(context.Background())
	go ShutdownHandler(cancel)

	g, gctx := errgroup.WithContext(ctx)
	a.ctx = gctx

	outputs := NewOutputs()
	if a.config.Outputs.Console == nil || !a.config.Outputs.Console.Disabled {
		outputs.Add(NewLoggerOutput(a.logger))
	}

	handler := NewLogHandler(a.logger.Named("handler"), outputs)
	chains := NewChains(a.config, a.logger.Named("chains"), a.contracts, handler)

	if a.config.Outputs.Postgres != nil {
		db := NewDatabase(a.logger.Named("db"))
		if err := db.Connect(ctx, a.config.Outputs.Postgres.URL); err != nil {
			a.logger.Fatalw("Failed to connect Postgres", "url", a.config.Outputs.Postgres.URL)
		}
		defer db.Close(ctx)

		if err := db.MigrateSchema(ctx, chains); err != nil {
			a.logger.Fatalw("Database.CreateSchemas failed", "err", err)
		}

		outputs.Add(db)
	}

	for _, chain := range chains {
		chain := chain

		g.Go(func() error {
			chain.RunLoop(gctx)
			return nil
		})
	}

	s := NewServer(a)
	g.Go(s.Run)

	if err := g.Wait(); err != nil {
		a.logger.Fatalf("Application error: %v", err)
	}
}
