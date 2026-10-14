package builder

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

type Builder struct {
	dockerCli *client.Client
	logDir    string
}

func NewBuilder(dockerCli *client.Client, logDir string) (*Builder, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log dir: %w", err)
	}
	return &Builder{
		dockerCli: dockerCli,
		logDir:    logDir,
	}, nil
}

// CloneRepo clones a repository into destDir and returns the commit SHA.
// Supports both git remote URLs and local filesystem paths.
func (b *Builder) CloneRepo(ctx context.Context, repoURL string, branch string, destDir string) (commitSHA string, shortSHA string, err error) {
	if branch == "" {
		branch = "main"
	}

	// Case 1: Local directory path provided
	if fi, statErr := os.Stat(repoURL); statErr == nil && fi.IsDir() {
		// Copy directory content to destDir, excluding .git
		if err := copyDir(repoURL, destDir); err != nil {
			return "", "", fmt.Errorf("failed to copy local repo: %w", err)
		}
		// Try to read commit hash if it's a git repo
		commit := "local0000000000000000000000000000000"
		if gitHead, err := exec.CommandContext(ctx, "git", "-C", repoURL, "rev-parse", "HEAD").Output(); err == nil {
			commit = strings.TrimSpace(string(gitHead))
		}
		short := commit
		if len(short) > 7 {
			short = short[:7]
		}
		return commit, short, nil
	}

	// Case 2: Git URL (https / ssh / etc.)
	// Ensure URL has protocol if omitted (e.g. github.com/user/repo -> https://github.com/user/repo.git)
	normalizedURL := repoURL
	if !strings.HasPrefix(normalizedURL, "http://") &&
		!strings.HasPrefix(normalizedURL, "https://") &&
		!strings.HasPrefix(normalizedURL, "git@") &&
		!strings.HasPrefix(normalizedURL, "file://") {
		normalizedURL = "https://" + normalizedURL
	}
	if !strings.HasSuffix(normalizedURL, ".git") && !strings.HasPrefix(normalizedURL, "file://") {
		normalizedURL += ".git"
	}

	cloneOptions := &git.CloneOptions{
		URL:           normalizedURL,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
		Depth:         1,
		Progress:      nil,
	}

	r, err := git.PlainCloneContext(ctx, destDir, false, cloneOptions)
	if err != nil {
		// Fallback to git CLI if available (handles system SSH keys, credential helpers, etc.)
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", branch, normalizedURL, destDir)
		if cliErr := cmd.Run(); cliErr != nil {
			return "", "", fmt.Errorf("git clone failed via go-git (%v) and git CLI (%w)", err, cliErr)
		}
		// Get commit SHA via CLI
		out, cliErr := exec.CommandContext(ctx, "git", "-C", destDir, "rev-parse", "HEAD").Output()
		if cliErr != nil {
			return "", "", fmt.Errorf("failed to get commit sha: %w", cliErr)
		}
		commit := strings.TrimSpace(string(out))
		short := commit
		if len(short) > 7 {
			short = short[:7]
		}
		return commit, short, nil
	}

	ref, err := r.Head()
	if err != nil {
		return "", "", fmt.Errorf("failed to get HEAD: %w", err)
	}

	commit := ref.Hash().String()
	short := commit
	if len(short) > 7 {
		short = short[:7]
	}
	return commit, short, nil
}

// BuildImage creates a tar context from contextDir and invokes Docker Engine SDK ImageBuild.
func (b *Builder) BuildImage(ctx context.Context, contextDir string, imageTag string, logFile *os.File) error {
	dockerfilePath := filepath.Join(contextDir, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		return fmt.Errorf("no Dockerfile found in repository root: %s", dockerfilePath)
	}

	// Create pipe for streaming the tar archive into Docker daemon
	pr, pw := io.Pipe()

	go func() {
		err := createTarContext(contextDir, pw)
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}()

	buildOpts := types.ImageBuildOptions{
		Dockerfile: "Dockerfile",
		Tags:       []string{imageTag},
		Remove:     true,
		NoCache:    false,
	}

	resp, err := b.dockerCli.ImageBuild(ctx, pr, buildOpts)
	if err != nil {
		return fmt.Errorf("docker build API failed: %w", err)
	}
	defer resp.Body.Close()

	// Parse JSON lines from Docker daemon and stream to log file
	scanner := bufio.NewScanner(resp.Body)
	type buildLine struct {
		Stream      string `json:"stream"`
		Status      string `json:"status"`
		Error       string `json:"error"`
		ErrorDetail struct {
			Message string `json:"message"`
		} `json:"errorDetail"`
	}

	for scanner.Scan() {
		var line buildLine
		text := scanner.Bytes()
		if err := json.Unmarshal(text, &line); err == nil {
			if line.Error != "" {
				errMsg := line.Error
				if line.ErrorDetail.Message != "" {
					errMsg = line.ErrorDetail.Message
				}
				fmt.Fprintf(logFile, "\n[BUILD ERROR]: %s\n", errMsg)
				return errors.New(errMsg)
			}
			if line.Stream != "" {
				fmt.Fprint(logFile, line.Stream)
			} else if line.Status != "" {
				fmt.Fprintf(logFile, "%s\n", line.Status)
			}
		} else {
			fmt.Fprintln(logFile, string(text))
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading build output stream: %w", err)
	}

	return nil
}

func createTarContext(srcDir string, writer io.Writer) error {
	tw := tar.NewWriter(writer)
	defer tw.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		// Skip .git directory to keep context lightweight
		if relPath == ".git" || strings.HasPrefix(relPath, ".git"+string(filepath.Separator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if relPath == "." {
			return nil
		}

		// Resolve symlinks if needed or write header
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}

		// Ensure tar paths use forward slashes
		hdr.Name = filepath.ToSlash(relPath)

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(tw, file)
		return err
	})
}

func copyDir(src string, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		if relPath == ".git" || strings.HasPrefix(relPath, ".git"+string(filepath.Separator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		targetPath := filepath.Join(dst, relPath)
		if info.IsDir() {
			return os.MkdirAll(targetPath, info.Mode())
		}

		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		dstFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer dstFile.Close()

		_, err = io.Copy(dstFile, srcFile)
		return err
	})
}
