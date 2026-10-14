package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrAppNotFound        = errors.New("app not found")
	ErrDeploymentNotFound = errors.New("deployment not found")
	ErrAppNameExists      = errors.New("app name already exists")
)

type Store struct {
	db *sql.DB
}

// NewStore initializes a SQLite database connection at the specified path and runs migrations.
func NewStore(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create db directory: %w", err)
		}
	}

	// _pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS apps (
		id TEXT PRIMARY KEY,
		name TEXT UNIQUE NOT NULL,
		repo_url TEXT NOT NULL,
		branch TEXT NOT NULL DEFAULT 'main',
		port INTEGER NOT NULL DEFAULT 8080,
		health_path TEXT NOT NULL DEFAULT '/',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS deployments (
		id TEXT PRIMARY KEY,
		app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
		commit_sha TEXT NOT NULL,
		image_tag TEXT NOT NULL,
		container_id TEXT,
		status TEXT NOT NULL,
		log_path TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		finished_at TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS env_vars (
		app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		PRIMARY KEY (app_id, key)
	);

	CREATE INDEX IF NOT EXISTS idx_deployments_app_id ON deployments(app_id);
	CREATE INDEX IF NOT EXISTS idx_deployments_status ON deployments(status);
	`
	_, err := s.db.Exec(schema)
	return err
}

// CreateApp inserts a new application record.
func (s *Store) CreateApp(app *App) error {
	query := `
	INSERT INTO apps (id, name, repo_url, branch, port, health_path, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	`
	if app.CreatedAt.IsZero() {
		app.CreatedAt = time.Now().UTC()
	}
	if app.Branch == "" {
		app.Branch = "main"
	}
	if app.Port <= 0 {
		app.Port = 8080
	}
	if app.HealthPath == "" {
		app.HealthPath = "/"
	}

	_, err := s.db.Exec(query, app.ID, app.Name, app.RepoURL, app.Branch, app.Port, app.HealthPath, app.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Check for uniqueness constraint error
		return fmt.Errorf("failed to create app: %w", err)
	}
	return nil
}

// GetAppByName retrieves an app by its unique name.
func (s *Store) GetAppByName(name string) (*App, error) {
	query := `SELECT id, name, repo_url, branch, port, health_path, created_at FROM apps WHERE name = ?`
	row := s.db.QueryRow(query, name)

	var app App
	err := row.Scan(&app.ID, &app.Name, &app.RepoURL, &app.Branch, &app.Port, &app.HealthPath, &app.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query error: %w", err)
	}
	return &app, nil
}

// GetAppByID retrieves an app by its ID.
func (s *Store) GetAppByID(id string) (*App, error) {
	query := `SELECT id, name, repo_url, branch, port, health_path, created_at FROM apps WHERE id = ?`
	row := s.db.QueryRow(query, id)

	var app App
	err := row.Scan(&app.ID, &app.Name, &app.RepoURL, &app.Branch, &app.Port, &app.HealthPath, &app.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query error: %w", err)
	}
	return &app, nil
}

// ListApps retrieves all apps and includes their live deployment if any.
func (s *Store) ListApps() ([]*App, error) {
	query := `SELECT id, name, repo_url, branch, port, health_path, created_at FROM apps ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []*App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.RepoURL, &a.Branch, &a.Port, &a.HealthPath, &a.CreatedAt); err != nil {
			return nil, err
		}
		// Attach live deployment
		live, _ := s.GetLiveDeployment(a.ID)
		a.LiveDeployment = live
		a.URL = fmt.Sprintf("http://%s.127.0.0.1.nip.io", a.Name)
		apps = append(apps, &a)
	}
	return apps, rows.Err()
}

// DeleteApp deletes an app by name.
func (s *Store) DeleteApp(name string) error {
	res, err := s.db.Exec(`DELETE FROM apps WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAppNotFound
	}
	return nil
}

// CreateDeployment records a new deployment.
func (s *Store) CreateDeployment(dep *Deployment) error {
	query := `
	INSERT INTO deployments (id, app_id, commit_sha, image_tag, container_id, status, log_path, created_at, finished_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if dep.CreatedAt.IsZero() {
		dep.CreatedAt = time.Now().UTC()
	}
	if dep.Status == "" {
		dep.Status = StatusQueued
	}

	_, err := s.db.Exec(query, dep.ID, dep.AppID, dep.CommitSHA, dep.ImageTag, dep.ContainerID, dep.Status, dep.LogPath, dep.CreatedAt, dep.FinishedAt)
	return err
}

// UpdateDeploymentStatus updates status and optional finished_at.
func (s *Store) UpdateDeploymentStatus(id string, status DeploymentStatus) error {
	var finishedAt *time.Time
	if status == StatusLive || status == StatusFailed || status == StatusSuperseded {
		now := time.Now().UTC()
		finishedAt = &now
	}

	query := `UPDATE deployments SET status = ?, finished_at = COALESCE(?, finished_at) WHERE id = ?`
	res, err := s.db.Exec(query, status, finishedAt, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDeploymentNotFound
	}
	return nil
}

// UpdateDeploymentContainer updates container_id and status.
func (s *Store) UpdateDeploymentContainer(id string, containerID string, status DeploymentStatus) error {
	var finishedAt *time.Time
	if status == StatusLive || status == StatusFailed {
		now := time.Now().UTC()
		finishedAt = &now
	}

	query := `UPDATE deployments SET container_id = ?, status = ?, finished_at = COALESCE(?, finished_at) WHERE id = ?`
	res, err := s.db.Exec(query, containerID, status, finishedAt, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDeploymentNotFound
	}
	return nil
}

// MarkPreviousDeploymentsSuperseded marks all previous live deployments for an app as superseded.
func (s *Store) MarkPreviousDeploymentsSuperseded(appID string, currentDeploymentID string) error {
	query := `UPDATE deployments SET status = ? WHERE app_id = ? AND id != ? AND status = ?`
	_, err := s.db.Exec(query, StatusSuperseded, appID, currentDeploymentID, StatusLive)
	return err
}

// GetDeployment retrieves a deployment by its ID.
func (s *Store) GetDeployment(id string) (*Deployment, error) {
	query := `SELECT id, app_id, commit_sha, image_tag, container_id, status, log_path, created_at, finished_at FROM deployments WHERE id = ?`
	row := s.db.QueryRow(query, id)

	var d Deployment
	var containerID sql.NullString
	err := row.Scan(&d.ID, &d.AppID, &d.CommitSHA, &d.ImageTag, &containerID, &d.Status, &d.LogPath, &d.CreatedAt, &d.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeploymentNotFound
	}
	if err != nil {
		return nil, err
	}
	if containerID.Valid {
		d.ContainerID = containerID.String
	}
	return &d, nil
}

// GetLiveDeployment returns the currently 'live' deployment for an app.
func (s *Store) GetLiveDeployment(appID string) (*Deployment, error) {
	query := `SELECT id, app_id, commit_sha, image_tag, container_id, status, log_path, created_at, finished_at FROM deployments WHERE app_id = ? AND status = ? ORDER BY created_at DESC LIMIT 1`
	row := s.db.QueryRow(query, appID, StatusLive)

	var d Deployment
	var containerID sql.NullString
	err := row.Scan(&d.ID, &d.AppID, &d.CommitSHA, &d.ImageTag, &containerID, &d.Status, &d.LogPath, &d.CreatedAt, &d.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // No live deployment yet
	}
	if err != nil {
		return nil, err
	}
	if containerID.Valid {
		d.ContainerID = containerID.String
	}
	return &d, nil
}

// ListDeploymentsByApp retrieves all deployments for an app ordered by created_at DESC.
func (s *Store) ListDeploymentsByApp(appID string) ([]*Deployment, error) {
	query := `SELECT id, app_id, commit_sha, image_tag, container_id, status, log_path, created_at, finished_at FROM deployments WHERE app_id = ? ORDER BY created_at DESC`
	rows, err := s.db.Query(query, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deps []*Deployment
	for rows.Next() {
		var d Deployment
		var containerID sql.NullString
		if err := rows.Scan(&d.ID, &d.AppID, &d.CommitSHA, &d.ImageTag, &containerID, &d.Status, &d.LogPath, &d.CreatedAt, &d.FinishedAt); err != nil {
			return nil, err
		}
		if containerID.Valid {
			d.ContainerID = containerID.String
		}
		deps = append(deps, &d)
	}
	return deps, rows.Err()
}
