package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var (
	apiURL   string
	apiToken string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "shipyard",
		Short: "Shipyard — A lightweight self-hosted PaaS",
		Long: `Shipyard turns a git repository into a running application
with zero-configuration subdomain routing behind Traefik.`,
	}

	defaultAPIURL := os.Getenv("SHIPYARD_API_URL")
	if defaultAPIURL == "" {
		defaultAPIURL = "http://127.0.0.1:8888"
	}
	defaultToken := os.Getenv("SHIPYARD_API_TOKEN")

	rootCmd.PersistentFlags().StringVar(&apiURL, "api-url", defaultAPIURL, "Shipyard daemon API URL")
	rootCmd.PersistentFlags().StringVar(&apiToken, "token", defaultToken, "API authentication token")

	// Apps command group
	appsCmd := &cobra.Command{
		Use:   "apps",
		Short: "Manage applications",
	}

	// apps create
	var repoURL, branch string
	var port int
	createAppCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Register a new application",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if repoURL == "" {
				return fmt.Errorf("--repo is required")
			}

			reqBody, _ := json.Marshal(map[string]interface{}{
				"name":     name,
				"repo_url": repoURL,
				"branch":   branch,
				"port":     port,
			})

			resp, err := sendRequest("POST", "/apps", bytes.NewReader(reqBody))
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusCreated {
				return printAPIError(resp)
			}

			var app struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				URL  string `json:"url"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&app)

			fmt.Printf("✅ App '%s' created successfully!\n", app.Name)
			fmt.Printf("   URL: %s\n", app.URL)
			fmt.Printf("   Run 'shipyard deploy %s' to deploy it.\n", app.Name)
			return nil
		},
	}
	createAppCmd.Flags().StringVar(&repoURL, "repo", "", "Git repository URL or local directory path")
	createAppCmd.Flags().StringVar(&branch, "branch", "main", "Git branch to track")
	createAppCmd.Flags().IntVar(&port, "port", 8080, "Port the application listens on inside container")
	_ = createAppCmd.MarkFlagRequired("repo")

	// apps list
	listAppsCmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered applications",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := sendRequest("GET", "/apps", nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return printAPIError(resp)
			}

			var apps []struct {
				Name           string `json:"name"`
				RepoURL        string `json:"repo_url"`
				Branch         string `json:"branch"`
				Port           int    `json:"port"`
				URL            string `json:"url"`
				LiveDeployment *struct {
					ID        string `json:"id"`
					Status    string `json:"status"`
					CommitSHA string `json:"commit_sha"`
				} `json:"live_deployment"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
				return err
			}

			if len(apps) == 0 {
				fmt.Println("No apps registered yet. Create one with 'shipyard apps create <name> --repo <url>'")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATUS\tURL\tCOMMIT\tBRANCH")
			for _, a := range apps {
				status := "not deployed"
				commit := "-"
				if a.LiveDeployment != nil {
					status = a.LiveDeployment.Status
					commit = a.LiveDeployment.CommitSHA
					if len(commit) > 7 {
						commit = commit[:7]
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Name, status, a.URL, commit, a.Branch)
			}
			return w.Flush()
		},
	}

	appsCmd.AddCommand(createAppCmd, listAppsCmd)

	// Deploy command
	deployCmd := &cobra.Command{
		Use:   "deploy <name>",
		Short: "Trigger a deployment for an application",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			resp, err := sendRequest("POST", fmt.Sprintf("/apps/%s/deploy", name), nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusAccepted {
				return printAPIError(resp)
			}

			var dep struct {
				ID      string `json:"id"`
				Status  string `json:"status"`
				LogPath string `json:"log_path"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&dep)

			fmt.Printf("🚀 Deployment triggered (ID: %s)\n", dep.ID)
			fmt.Println("Streaming build logs...")

			// Poll and stream logs until deployment finishes
			return streamDeploymentLogs(dep.ID)
		},
	}

	// Logs command
	var buildDeployID string
	logsCmd := &cobra.Command{
		Use:   "logs [app-name]",
		Short: "View build or runtime logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			if buildDeployID != "" {
				// Fetch build logs for specific deployment
				resp, err := sendRequest("GET", fmt.Sprintf("/deployments/%s/logs", buildDeployID), nil)
				if err != nil {
					return err
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					return printAPIError(resp)
				}
				_, _ = io.Copy(os.Stdout, resp.Body)
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("please provide app name or use --build <deploy-id>")
			}

			appName := args[0]
			resp, err := sendRequest("GET", fmt.Sprintf("/apps/%s/logs", appName), nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return printAPIError(resp)
			}

			_, _ = io.Copy(os.Stdout, resp.Body)
			return nil
		},
	}
	logsCmd.Flags().StringVar(&buildDeployID, "build", "", "Deployment ID to view build logs for")

	rootCmd.AddCommand(appsCmd, deployCmd, logsCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func sendRequest(method, path string, body io.Reader) (*http.Response, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	url := strings.TrimRight(apiURL, "/") + path

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to Shipyard daemon at %s: %w\n(Is 'shipyardd' running?)", apiURL, err)
	}

	return resp, nil
}

func printAPIError(resp *http.Response) error {
	var errResp struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != "" {
		return fmt.Errorf("API error (%d): %s", resp.StatusCode, errResp.Error)
	}
	return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
}

func streamDeploymentLogs(depID string) error {
	var printedBytes int64 = 0

	for {
		resp, err := sendRequest("GET", fmt.Sprintf("/deployments/%s", depID), nil)
		if err != nil {
			return err
		}

		var dep struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&dep)
		resp.Body.Close()

		// Fetch and print new logs
		logResp, err := sendRequest("GET", fmt.Sprintf("/deployments/%s/logs", depID), nil)
		if err == nil {
			if logResp.StatusCode == http.StatusOK {
				data, _ := io.ReadAll(logResp.Body)
				if int64(len(data)) > printedBytes {
					os.Stdout.Write(data[printedBytes:])
					printedBytes = int64(len(data))
				}
			}
			logResp.Body.Close()
		}

		if dep.Status == "live" {
			fmt.Println("\n🎉 Deployment completed successfully!")
			return nil
		}
		if dep.Status == "failed" {
			return fmt.Errorf("\n❌ Deployment failed. Check build logs above for details")
		}

		time.Sleep(1 * time.Second)
	}
}
