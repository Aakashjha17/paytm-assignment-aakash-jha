package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is everything the server reads from the environment.
type Config struct {
	Port            int
	DatabaseURL     string
	DBMaxConns      int32
	JWTSecret       string
	AdminAPIKey     string
	TokenTTL        time.Duration
	LogLevel        string
	ShutdownTimeout time.Duration
}

func Load() (Config, error) {
	var errs []error
	port, err := intEnv("PORT", 8080)
	errs = append(errs, err)
	maxConns, err := intEnv("DB_MAX_CONNS", 20)
	errs = append(errs, err)
	tokenTTL, err := durationEnv("TOKEN_TTL", 24*time.Hour)
	errs = append(errs, err)
	shutdown, err := durationEnv("SHUTDOWN_TIMEOUT", 10*time.Second)
	errs = append(errs, err)

	cfg := Config{
		Port:            port,
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DBMaxConns:      int32(maxConns),
		JWTSecret:       os.Getenv("JWT_SECRET"),
		AdminAPIKey:     os.Getenv("ADMIN_API_KEY"),
		TokenTTL:        tokenTTL,
		LogLevel:        os.Getenv("LOG_LEVEL"),
		ShutdownTimeout: shutdown,
	}

	// Fail closed: no default secrets, so a misconfigured deploy refuses to start
	// rather than silently accepting tokens signed with a well-known key.
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if len(cfg.JWTSecret) < 16 {
		errs = append(errs, errors.New("JWT_SECRET must be at least 16 characters"))
	}
	if len(cfg.AdminAPIKey) < 16 {
		errs = append(errs, errors.New("ADMIN_API_KEY must be at least 16 characters"))
	}
	if cfg.DBMaxConns < 1 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be >= 1"))
	}
	return cfg, errors.Join(errs...)
}

func intEnv(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
