package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_AppAndDeploymentLifecycle(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "shipyard-store-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "test_shipyard.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer s.Close()

	// 1. Create App
	app := &App{
		ID:         "app-123",
		Name:       "todo-api",
		RepoURL:    "https://github.com/example/todo-api.git",
		Branch:     "main",
		Port:       8080,
		HealthPath: "/health",
		CreatedAt:  time.Now().UTC(),
	}

	if err := s.CreateApp(app); err != nil {
		t.Fatalf("failed to create app: %v", err)
	}

	// 2. Fetch App by Name & ID
	fetched, err := s.GetAppByName("todo-api")
	if err != nil {
		t.Fatalf("failed to get app by name: %v", err)
	}
	if fetched.ID != app.ID || fetched.Name != app.Name || fetched.Port != 8080 {
		t.Errorf("fetched app mismatch: %+v", fetched)
	}

	// 3. Create Deployment
	dep := &Deployment{
		ID:        "dep-001",
		AppID:     app.ID,
		CommitSHA: "abcdef123456",
		ImageTag:  "shipyard/todo-api:abcdef1",
		Status:    StatusQueued,
		LogPath:   filepath.Join(tempDir, "dep-001.log"),
		CreatedAt: time.Now().UTC(),
	}

	if err := s.CreateDeployment(dep); err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}

	// 4. Update Deployment status to building, then live
	if err := s.UpdateDeploymentStatus(dep.ID, StatusBuilding); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	if err := s.UpdateDeploymentContainer(dep.ID, "container-xyz", StatusLive); err != nil {
		t.Fatalf("failed to update container: %v", err)
	}

	// 5. Verify live deployment
	live, err := s.GetLiveDeployment(app.ID)
	if err != nil {
		t.Fatalf("failed to get live deployment: %v", err)
	}
	if live == nil || live.ContainerID != "container-xyz" || live.Status != StatusLive {
		t.Fatalf("expected live deployment with container-xyz, got: %+v", live)
	}

	// 6. Test App listing
	apps, err := s.ListApps()
	if err != nil {
		t.Fatalf("failed to list apps: %v", err)
	}
	if len(apps) != 1 || apps[0].LiveDeployment == nil {
		t.Fatalf("expected 1 app with live deployment, got: %d", len(apps))
	}

	// 7. Delete App
	if err := s.DeleteApp(app.Name); err != nil {
		t.Fatalf("failed to delete app: %v", err)
	}

	_, err = s.GetAppByName(app.Name)
	if err != ErrAppNotFound {
		t.Fatalf("expected ErrAppNotFound, got: %v", err)
	}
}
