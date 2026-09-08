package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"avito-kitchen/internal/httpapi"
	"avito-kitchen/internal/store"
	"avito-kitchen/migrations"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log.Logger = zerolog.New(os.Stdout).Level(zerolog.InfoLevel).With().Timestamp().Str("service", "api").Logger()
	if err := run(); err != nil {
		log.Error().Err(err).Msg("application stopped")
		os.Exit(1)
	}
}

func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(env("HEALTHCHECK_URL", "http://127.0.0.1:8080/healthz"))
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			return fmt.Errorf("healthcheck: HTTP %d", resp.StatusCode)
		}
		return nil
	}
	if command != "serve" && command != "migrate" && command != "provision" {
		return fmt.Errorf("usage: api [serve|migrate up|migrate down|provision|healthcheck]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	s, err := store.Open(connectCtx, dsn)
	cancel()
	if err != nil {
		return fmt.Errorf("database connection failed")
	}
	defer func() { _ = s.DB.Close() }()
	switch command {
	case "migrate":
		direction := "up"
		if len(os.Args) > 2 {
			direction = os.Args[2]
		}
		migrationCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		return migrations.Run(migrationCtx, s.DB, direction)
	case "provision":
		p, err := s.Provision(ctx, env("RESTAURANT_EXTERNAL_ID", "demo-cafe"), env("RESTAURANT_NAME", "Кухня на районе"), env("RESTAURANT_CITY", "Москва"), env("RESTAURANT_CUISINE", "Домашняя кухня"), os.Getenv("RESTAURANT_API_KEY"))
		if err != nil {
			return err
		}
		log.Info().Str("restaurant_id", p.ID).Str("external_id", p.ExternalID).Msg("restaurant provisioned")
		return nil
	}
	ttl, err := time.ParseDuration(env("ORDER_TTL", "2m"))
	if err != nil || ttl < time.Second || ttl > 24*time.Hour {
		return fmt.Errorf("ORDER_TTL must be between 1s and 24h")
	}
	var version int
	if err = s.DB.QueryRowContext(ctx, `SELECT coalesce(max(version),0) FROM schema_migrations`).Scan(&version); err != nil || version != migrations.LatestVersion {
		return fmt.Errorf("database is not migrated; run api migrate up")
	}
	gatewayKey := os.Getenv("GATEWAY_API_KEY")
	if len(gatewayKey) < 32 || len(gatewayKey) > 512 {
		return fmt.Errorf("GATEWAY_API_KEY must contain 32..512 bytes")
	}
	if err := s.CheckRuntimeRole(ctx); err != nil {
		return err
	}
	server := &http.Server{Addr: env("HTTP_ADDR", ":8080"), Handler: httpapi.New(s, ttl, gatewayKey), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); expire(ctx, s) }()
	errCh := make(chan error, 1)
	go func() {
		log.Info().Str("address", server.Addr).Msg("API listening")
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
	case err = <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			<-workerDone
			return err
		}
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = server.Shutdown(shutdownCtx)
	<-workerDone
	return err
}

func expire(ctx context.Context, s *store.Store) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for range 100 {
			workCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			changed, err := s.ExpireOne(workCtx)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					log.Error().Msg("expiration failed")
				}
				break
			}
			if !changed {
				break
			}
		}
	}
}
