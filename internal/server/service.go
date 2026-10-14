package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"shipyard/internal/builder"
	"shipyard/internal/runtime"
	"shipyard/internal/store"
)

type Service struct {
	store   *store.Store
	builder *builder.Builder
	runtime *runtime.Runtime
	dataDir string
}

func NewService(s *store.Store, b *builder.Builder, r *runtime.Runtime, dataDir string) *Service {
	return &Service{
		store:   s,
		builder: b,
		runtime: r,
		dataDir: dataDir,
	}
}

// TriggerDeploy initiates a new deployment for the named app and executes it in a background goroutine.
func (svc *Service) TriggerDeploy(appName string) (*store.Deployment, error) {
	app, err := svc.store.GetAppByName(appName)
	if err != nil {
		return nil, err
	}

	depID := uuid.NewString()
	logPath := filepath.Join(svc.dataDir, "logs", fmt.Sprintf("%s.log", depID))

	dep := &store.Deployment{
		ID:        depID,
		AppID:     app.ID,
		CommitSHA: "pending",
		ImageTag:  fmt.Sprintf("shipyard/%s:building", app.Name),
		Status:    store.StatusQueued,
		LogPath:   logPath,
		CreatedAt: time.Now().UTC(),
	}

	if err := svc.store.CreateDeployment(dep); err != nil {
		return nil, fmt.Errorf("failed to save deployment record: %w", err)
	}

	// Run build and deploy pipeline in background
	go svc.runDeploymentPipeline(app, dep)

	return dep, nil
}

func (svc *Service) runDeploymentPipeline(app *store.App, dep *store.Deployment) {
	// Create log file
	logFile, err := os.OpenFile(dep.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("[ERROR] Failed to open log file %s: %v", dep.LogPath, err)
		_ = svc.store.UpdateDeploymentStatus(dep.ID, store.StatusFailed)
		return
	}
	defer logFile.Close()

	fmt.Fprintf(logFile, "=== Starting Shipyard deployment %s for app %s ===\n", dep.ID, app.Name)
	fmt.Fprintf(logFile, "Repo: %s (branch: %s)\n", app.RepoURL, app.Branch)

	ctx := context.Background()

	// Step 1: Update status to building
	_ = svc.store.UpdateDeploymentStatus(dep.ID, store.StatusBuilding)
	fmt.Fprintln(logFile, "\n[1/3] Cloning repository...")

	tempDir, err := os.MkdirTemp("", fmt.Sprintf("shipyard-build-%s-*", app.Name))
	if err != nil {
		fmt.Fprintf(logFile, "Failed to create temp build dir: %v\n", err)
		_ = svc.store.UpdateDeploymentStatus(dep.ID, store.StatusFailed)
		return
	}
	defer os.RemoveAll(tempDir)

	commitSHA, shortSHA, err := svc.builder.CloneRepo(ctx, app.RepoURL, app.Branch, tempDir)
	if err != nil {
		fmt.Fprintf(logFile, "Clone failed: %v\n", err)
		_ = svc.store.UpdateDeploymentStatus(dep.ID, store.StatusFailed)
		return
	}

	imageTag := fmt.Sprintf("shipyard/%s:%s", app.Name, shortSHA)
	dep.CommitSHA = commitSHA
	dep.ImageTag = imageTag
	fmt.Fprintf(logFile, "Cloned commit %s (tag: %s)\n", commitSHA, imageTag)

	// Step 2: Build Docker image
	fmt.Fprintln(logFile, "\n[2/3] Building Docker image...")
	if err := svc.builder.BuildImage(ctx, tempDir, imageTag, logFile); err != nil {
		fmt.Fprintf(logFile, "\nDocker build failed: %v\n", err)
		_ = svc.store.UpdateDeploymentStatus(dep.ID, store.StatusFailed)
		return
	}
	fmt.Fprintln(logFile, "Docker build completed successfully.")

	// Step 3: Start container and route traffic
	_ = svc.store.UpdateDeploymentStatus(dep.ID, store.StatusStarting)
	fmt.Fprintln(logFile, "\n[3/3] Starting container and configuring Traefik routing...")

	// Lookup previous live deployment so we can stop it once the new one is up
	prevLive, _ := svc.store.GetLiveDeployment(app.ID)

	containerID, err := svc.runtime.StartAppContainer(ctx, app, dep.ID, imageTag, nil)
	if err != nil {
		fmt.Fprintf(logFile, "Failed to start container: %v\n", err)
		_ = svc.store.UpdateDeploymentStatus(dep.ID, store.StatusFailed)
		return
	}

	// Update deployment status to LIVE
	dep.ContainerID = containerID
	if err := svc.store.UpdateDeploymentContainer(dep.ID, containerID, store.StatusLive); err != nil {
		fmt.Fprintf(logFile, "Warning: failed to update deployment container in db: %v\n", err)
	}

	// Mark previous deployment as superseded and stop its old container
	if prevLive != nil && prevLive.ContainerID != "" && prevLive.ID != dep.ID {
		fmt.Fprintf(logFile, "Stopping superseded container %s...\n", prevLive.ContainerID)
		_ = svc.runtime.StopAndRemoveContainer(ctx, prevLive.ContainerID)
		_ = svc.store.MarkPreviousDeploymentsSuperseded(app.ID, dep.ID)
	}

	fmt.Fprintf(logFile, "\nDeployment successful! App is live at: http://%s.127.0.0.1.nip.io\n", app.Name)
}
