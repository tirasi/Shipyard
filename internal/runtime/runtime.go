package runtime

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"shipyard/internal/store"
)

const (
	NetworkName = "shipyard"
)

type Runtime struct {
	dockerCli *client.Client
}

func NewRuntime(dockerCli *client.Client) *Runtime {
	return &Runtime{
		dockerCli: dockerCli,
	}
}

// StartAppContainer launches an application container on the 'shipyard' network with Traefik routing labels.
func (r *Runtime) StartAppContainer(ctx context.Context, app *store.App, deploymentID string, imageTag string, envVars map[string]string) (string, error) {
	shortDep := deploymentID
	if len(shortDep) > 8 {
		shortDep = shortDep[:8]
	}
	containerName := fmt.Sprintf("shipyard-%s-%s", app.Name, shortDep)

	// Build environment variable list
	var envList []string
	// Ensure default PORT is provided
	envList = append(envList, fmt.Sprintf("PORT=%d", app.Port))
	for k, v := range envVars {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	portStr := strconv.Itoa(app.Port)
	natPort := nat.Port(portStr + "/tcp")

	// Traefik labels for automatic routing
	labels := map[string]string{
		"traefik.enable": "true",
		fmt.Sprintf("traefik.http.routers.%s.rule", app.Name):                 fmt.Sprintf("Host(`%s.127.0.0.1.nip.io`)", app.Name),
		fmt.Sprintf("traefik.http.routers.%s.entrypoints", app.Name):          "web",
		fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", app.Name): portStr,
		"shipyard.managed":    "true",
		"shipyard.app":        app.Name,
		"shipyard.deployment": deploymentID,
	}

	containerConfig := &container.Config{
		Image: imageTag,
		Env:   envList,
		ExposedPorts: nat.PortSet{
			natPort: struct{}{},
		},
		Labels: labels,
	}

	hostConfig := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
	}

	netConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {
				Aliases: []string{app.Name},
			},
		},
	}

	resp, err := r.dockerCli.ContainerCreate(ctx, containerConfig, hostConfig, netConfig, nil, containerName)
	if err != nil {
		return "", fmt.Errorf("failed to create container %s: %w", containerName, err)
	}

	containerID := resp.ID

	// Start the container
	if err := r.dockerCli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = r.dockerCli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("failed to start container %s: %w", containerName, err)
	}

	// Verify the container did not immediately crash on startup
	time.Sleep(1 * time.Second)
	inspect, err := r.dockerCli.ContainerInspect(ctx, containerID)
	if err != nil {
		return containerID, nil
	}

	if !inspect.State.Running && inspect.State.ExitCode != 0 {
		return containerID, fmt.Errorf("container exited immediately with status code %d: %s", inspect.State.ExitCode, inspect.State.Error)
	}

	return containerID, nil
}

// StopAndRemoveContainer stops a running container gracefully and removes it.
func (r *Runtime) StopAndRemoveContainer(ctx context.Context, containerID string) error {
	if containerID == "" {
		return nil
	}

	timeoutSec := 5
	stopOpts := container.StopOptions{
		Timeout: &timeoutSec,
	}

	_ = r.dockerCli.ContainerStop(ctx, containerID, stopOpts)

	removeOpts := container.RemoveOptions{
		Force: true,
	}
	return r.dockerCli.ContainerRemove(ctx, containerID, removeOpts)
}

// GetContainerLogs returns a stream of container stdout/stderr.
func (r *Runtime) GetContainerLogs(ctx context.Context, containerID string, follow bool, tail string) (io.ReadCloser, error) {
	if tail == "" {
		tail = "100"
	}
	opts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tail,
	}
	return r.dockerCli.ContainerLogs(ctx, containerID, opts)
}
