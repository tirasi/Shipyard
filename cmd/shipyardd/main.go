package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/docker/docker/client"

	"shipyard/internal/builder"
	"shipyard/internal/runtime"
	"shipyard/internal/server"
	"shipyard/internal/store"
)

func main() {
	log.Println("🚢 Starting Shipyard Daemon (shipyardd)...")

	// Determine data directory
	dataDir := os.Getenv("SHIPYARD_DATA_DIR")
	if dataDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			dataDir = "./data"
		} else {
			dataDir = filepath.Join(homeDir, ".shipyard")
		}
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("Failed to create data directory %s: %v", dataDir, err)
	}

	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Fatalf("Failed to create logs directory %s: %v", logDir, err)
	}

	// Initialize SQLite Store
	dbPath := filepath.Join(dataDir, "shipyard.db")
	dbStore, err := store.NewStore(dbPath)
	if err != nil {
		log.Fatalf("Failed to initialize database at %s: %v", dbPath, err)
	}
	defer dbStore.Close()
	log.Printf("Connected to SQLite database at %s", dbPath)

	// Ensure Docker host socket is detected on macOS (Docker Desktop / OrbStack / Colima)
	if os.Getenv("DOCKER_HOST") == "" {
		if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
			homeDir, _ := os.UserHomeDir()
			macSocket := filepath.Join(homeDir, ".docker", "run", "docker.sock")
			if _, err := os.Stat(macSocket); err == nil {
				os.Setenv("DOCKER_HOST", "unix://"+macSocket)
				log.Printf("Detected macOS Docker socket at %s", macSocket)
			}
		}
	}

	// Initialize Docker SDK Client
	dockerCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Failed to initialize Docker SDK client: %v", err)
	}
	defer dockerCli.Close()

	// Verify Docker daemon connectivity
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()
	ping, err := dockerCli.Ping(pingCtx)
	if err != nil {
		log.Fatalf("Failed to ping Docker daemon: %v. Please ensure Docker is running.", err)
	}
	log.Printf("Connected to Docker Engine (API Version: %s, OS: %s)", ping.APIVersion, ping.OSType)

	// Initialize Builder & Runtime
	appBuilder, err := builder.NewBuilder(dockerCli, logDir)
	if err != nil {
		log.Fatalf("Failed to initialize builder: %v", err)
	}
	appRuntime := runtime.NewRuntime(dockerCli)

	// Initialize Service & Router
	apiToken := os.Getenv("SHIPYARD_API_TOKEN")
	svc := server.NewService(dbStore, appBuilder, appRuntime, dataDir)
	router := server.NewRouter(dbStore, svc, apiToken)

	// Server Port
	port := os.Getenv("SHIPYARD_PORT")
	if port == "" {
		port = "8888"
	}
	addr := fmt.Sprintf(":%s", port)

	httpServer := &http.Server{
		Addr:         addr,
		Handler:      router.Routes(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown handling
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("🚀 Shipyard API server listening on http://127.0.0.1%s", addr)
		if apiToken != "" {
			log.Println("🔒 API authentication enabled (token required)")
		} else {
			log.Println("🔓 Running in local mode without API token authentication")
		}
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-stopChan
	log.Println("\nShutting down Shipyard Daemon gracefully...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}

	log.Println("Shipyard daemon stopped.")
}
