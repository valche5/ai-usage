// Command ai-usage-web runs the single-user homelab dashboard.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/valche5/ai-usage/internal/connection"
	"github.com/valche5/ai-usage/internal/httpx"
	"github.com/valche5/ai-usage/internal/oauthflow"
	"github.com/valche5/ai-usage/internal/webapp"
)

var version = "dev"

func main() {
	httpx.ScrubEnv()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if zone := os.Getenv("TZ"); zone != "" {
		location, err := time.LoadLocation(zone)
		if err != nil {
			return errors.New("TZ invalide: " + zone)
		}
		time.Local = location
	}
	dataDir := env("AI_USAGE_DATA_DIR", ".ai-usage-web")
	encryptionKey, err := secretEnv("AI_USAGE_ENCRYPTION_KEY")
	if err != nil {
		return err
	}
	store, err := connection.Open(dataDir, encryptionKey)
	if err != nil {
		return err
	}
	timeout, err := durationEnv("AI_USAGE_HTTP_TIMEOUT", 10*time.Second)
	if err != nil {
		return err
	}
	refreshEvery, err := durationEnv("AI_USAGE_REFRESH_INTERVAL", time.Minute)
	if err != nil {
		return err
	}
	sessionTTL, err := durationEnv("AI_USAGE_SESSION_TTL", 7*24*time.Hour)
	if err != nil {
		return err
	}
	client := httpx.Client(timeout)
	oauthConfig := oauthflow.DefaultConfig(client)
	oauthConfig.ChatGPTClientID = env("AI_USAGE_OPENAI_CLIENT_ID", oauthConfig.ChatGPTClientID)
	oauthConfig.XAIClientID = env("AI_USAGE_XAI_CLIENT_ID", oauthConfig.XAIClientID)
	oauthConfig.GitHubClientID = env("AI_USAGE_GITHUB_CLIENT_ID", oauthConfig.GitHubClientID)
	oauthConfig.GitHubAPI = env("AI_USAGE_GITHUB_API", oauthConfig.GitHubAPI)
	oauth := oauthflow.New(oauthConfig, store)
	migrationCtx, cancelMigration := context.WithTimeout(context.Background(), timeout)
	if err := oauth.MigrateLegacyCopilot(migrationCtx); err != nil {
		log.Printf("migration de l'ancien compte Copilot reportée: %v", err)
	}
	cancelMigration()
	password, err := secretEnv("AI_USAGE_PASSWORD")
	if err != nil {
		return err
	}
	apiToken, err := secretEnv("AI_USAGE_API_TOKEN")
	if err != nil {
		return err
	}
	app, err := webapp.New(webapp.Config{
		Password:        password,
		APIToken:        apiToken,
		SessionTTL:      sessionTTL,
		RefreshInterval: refreshEvery,
		HTTPTimeout:     timeout,
	}, store, oauth, client)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app.Run(ctx)
	server := &http.Server{
		Addr:              env("AI_USAGE_ADDR", ":8080"),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("ai-usage-web %s écoute sur %s", version, server.Addr)
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func secretEnv(key string) (string, error) {
	if path := os.Getenv(key + "_FILE"); path != "" {
		value, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("lire " + key + "_FILE: " + err.Error())
		}
		return strings.TrimSpace(string(value)), nil
	}
	return os.Getenv(key), nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, errors.New(key + " doit être une durée positive (ex: 1m)")
	}
	return duration, nil
}
