package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"shipyard/internal/store"
)

var validAppNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

type Router struct {
	store    *store.Store
	service  *Service
	apiToken string
}

func NewRouter(s *store.Store, svc *Service, apiToken string) *Router {
	return &Router{
		store:    s,
		service:  svc,
		apiToken: apiToken,
	}
}

func (rt *Router) Routes() http.Handler {
	mux := http.NewServeMux()

	// App management
	mux.HandleFunc("POST /apps", rt.handleCreateApp)
	mux.HandleFunc("GET /apps", rt.handleListApps)
	mux.HandleFunc("GET /apps/{name}", rt.handleGetApp)
	mux.HandleFunc("DELETE /apps/{name}", rt.handleDeleteApp)

	// Deployments
	mux.HandleFunc("POST /apps/{name}/deploy", rt.handleDeployApp)
	mux.HandleFunc("GET /apps/{name}/deployments", rt.handleListDeployments)
	mux.HandleFunc("GET /deployments/{id}", rt.handleGetDeployment)
	mux.HandleFunc("GET /deployments/{id}/logs", rt.handleGetDeploymentLogs)

	// Runtime logs
	mux.HandleFunc("GET /apps/{name}/logs", rt.handleGetAppRuntimeLogs)

	// Health endpoint
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	return rt.authMiddleware(mux)
}

func (rt *Router) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		if rt.apiToken != "" {
			token := r.Header.Get("X-Api-Token")
			if token == "" {
				authHeader := r.Header.Get("Authorization")
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}

			if token != rt.apiToken {
				http.Error(w, `{"error":"unauthorized: invalid or missing API token"}`, http.StatusUnauthorized)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

type CreateAppRequest struct {
	Name       string `json:"name"`
	RepoURL    string `json:"repo_url"`
	Branch     string `json:"branch"`
	Port       int    `json:"port"`
	HealthPath string `json:"health_path"`
}

func (rt *Router) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req CreateAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request payload: "+err.Error())
		return
	}

	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
	if req.Name == "" || !validAppNameRegex.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid app name: must be 3-63 lowercase alphanumeric characters or hyphens")
		return
	}

	if req.RepoURL == "" {
		writeError(w, http.StatusBadRequest, "repo_url is required")
		return
	}

	if req.Port <= 0 {
		req.Port = 8080
	}
	if req.Branch == "" {
		req.Branch = "main"
	}
	if req.HealthPath == "" {
		req.HealthPath = "/"
	}

	app := &store.App{
		ID:         uuid.NewString(),
		Name:       req.Name,
		RepoURL:    req.RepoURL,
		Branch:     req.Branch,
		Port:       req.Port,
		HealthPath: req.HealthPath,
		CreatedAt:  time.Now().UTC(),
	}

	if err := rt.store.CreateApp(app); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, fmt.Sprintf("app '%s' already exists", req.Name))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	app.URL = fmt.Sprintf("http://%s.127.0.0.1.nip.io", app.Name)
	writeJSON(w, http.StatusCreated, app)
}

func (rt *Router) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := rt.store.ListApps()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if apps == nil {
		apps = []*store.App{}
	}
	writeJSON(w, http.StatusOK, apps)
}

func (rt *Router) handleGetApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := rt.store.GetAppByName(name)
	if err != nil {
		if errors.Is(err, store.ErrAppNotFound) {
			writeError(w, http.StatusNotFound, "app not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	live, _ := rt.store.GetLiveDeployment(app.ID)
	app.LiveDeployment = live
	app.URL = fmt.Sprintf("http://%s.127.0.0.1.nip.io", app.Name)
	writeJSON(w, http.StatusOK, app)
}

func (rt *Router) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := rt.store.GetAppByName(name)
	if err != nil {
		if errors.Is(err, store.ErrAppNotFound) {
			writeError(w, http.StatusNotFound, "app not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Stop any live container
	live, _ := rt.store.GetLiveDeployment(app.ID)
	if live != nil && live.ContainerID != "" {
		_ = rt.service.runtime.StopAndRemoveContainer(r.Context(), live.ContainerID)
	}

	if err := rt.store.DeleteApp(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message": fmt.Sprintf("app '%s' successfully deleted", name),
	})
}

func (rt *Router) handleDeployApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dep, err := rt.service.TriggerDeploy(name)
	if err != nil {
		if errors.Is(err, store.ErrAppNotFound) {
			writeError(w, http.StatusNotFound, "app not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, dep)
}

func (rt *Router) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := rt.store.GetAppByName(name)
	if err != nil {
		if errors.Is(err, store.ErrAppNotFound) {
			writeError(w, http.StatusNotFound, "app not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	deps, err := rt.store.ListDeploymentsByApp(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if deps == nil {
		deps = []*store.Deployment{}
	}
	writeJSON(w, http.StatusOK, deps)
}

func (rt *Router) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dep, err := rt.store.GetDeployment(id)
	if err != nil {
		if errors.Is(err, store.ErrDeploymentNotFound) {
			writeError(w, http.StatusNotFound, "deployment not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, dep)
}

func (rt *Router) handleGetDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dep, err := rt.store.GetDeployment(id)
	if err != nil {
		if errors.Is(err, store.ErrDeploymentNotFound) {
			writeError(w, http.StatusNotFound, "deployment not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Stream log file
	logFile, err := os.Open(dep.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "logs not yet available")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer logFile.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, logFile)
}

func (rt *Router) handleGetAppRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := rt.store.GetAppByName(name)
	if err != nil {
		if errors.Is(err, store.ErrAppNotFound) {
			writeError(w, http.StatusNotFound, "app not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	live, err := rt.store.GetLiveDeployment(app.ID)
	if err != nil || live == nil || live.ContainerID == "" {
		writeError(w, http.StatusNotFound, "no running container found for app")
		return
	}

	reader, err := rt.service.runtime.GetContainerLogs(r.Context(), live.ContainerID, false, "100")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, reader)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
