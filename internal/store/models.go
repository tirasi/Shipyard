package store

import (
	"time"
)

type DeploymentStatus string

const (
	StatusQueued     DeploymentStatus = "queued"
	StatusBuilding   DeploymentStatus = "building"
	StatusStarting   DeploymentStatus = "starting"
	StatusLive       DeploymentStatus = "live"
	StatusFailed     DeploymentStatus = "failed"
	StatusSuperseded DeploymentStatus = "superseded"
)

type App struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	RepoURL    string    `json:"repo_url"`
	Branch     string    `json:"branch"`
	Port       int       `json:"port"`
	HealthPath string    `json:"health_path"`
	CreatedAt  time.Time `json:"created_at"`

	// Populated for list/detail views
	LiveDeployment *Deployment `json:"live_deployment,omitempty"`
	URL            string      `json:"url,omitempty"`
}

type Deployment struct {
	ID          string           `json:"id"`
	AppID       string           `json:"app_id"`
	CommitSHA   string           `json:"commit_sha"`
	ImageTag    string           `json:"image_tag"`
	ContainerID string           `json:"container_id,omitempty"`
	Status      DeploymentStatus `json:"status"`
	LogPath     string           `json:"log_path"`
	CreatedAt   time.Time        `json:"created_at"`
	FinishedAt  *time.Time       `json:"finished_at,omitempty"`
}

type EnvVar struct {
	AppID string `json:"app_id"`
	Key   string `json:"key"`
	Value string `json:"value"`
}
