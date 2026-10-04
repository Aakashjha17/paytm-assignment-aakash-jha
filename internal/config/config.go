package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is everything the server reads from the environment. Later phases
// add DB, auth and limits here; for now it only needs to know where to listen.
type Config struct {
	Port            int
	DatabaseURL     string
	ShutdownTimeout time.Duration
}

func Load() (Config, error) {
	port, err := intEnv("PORT", 8080)
	if err != nil {
		return Config{}, err
	}
	shutdown, err := durationEnv("SHUTDOWN_TIMEOUT", 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Port:            port,
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		ShutdownTimeout: shutdown,
	}, nil
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
