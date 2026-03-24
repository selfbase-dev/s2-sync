//go:build e2e

package cmd

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/selfbase-dev/s2-cli/internal/client"
	s2sync "github.com/selfbase-dev/s2-cli/internal/sync"
)

// E2E tests require:
// 1. npm run dev (local S2 server on localhost:5173 or 5174)
// 2. go build -o /tmp/s2 . (CLI binary)
// 3. Test user + token in DB (see e2e/global-setup.ts)
//
// Run: go test -tags e2e ./cmd/ -v -run TestE2E -timeout 120s

func getE2EEndpoint() string {
	if ep := os.Getenv("S2_E2E_ENDPOINT"); ep != "" {
		return ep
	}
	// Try common dev server ports
	for _, port := range []string{"5173", "5174"} {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%s/s3/s2/", port))
		if err == nil {
			resp.Body.Close()
			return fmt.Sprintf("http://localhost:%s", port)
		}
	}
	return ""
}

func getE2EToken() string {
	if tok := os.Getenv("S2_E2E_TOKEN"); tok != "" {
		return tok
	}
	return ""
}

func skipIfNoServer(t *testing.T) (string, string) {
	t.Helper()
	endpoint := getE2EEndpoint()
	if endpoint == "" {
		t.Skip("E2E: no dev server running (npm run dev)")
	}
	token := getE2EToken()
	if token == "" {
		t.Skip("E2E: S2_E2E_TOKEN not set")
	}
	return endpoint, token
}

func buildCLI(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "s2")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cmd.Dir = filepath.Join("..", "..") // cli/
	// Find the cli directory
	wd, _ := os.Getwd()
	if strings.HasSuffix(wd, "/cmd") {
		cmd.Dir = filepath.Dir(wd)
	} else {
		cmd.Dir = wd
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build CLI: %v\n%s", err, out)
	}
	return binary
}

func TestE2E_WatchInitialSync(t *testing.T) {
	endpoint, token := skipIfNoServer(t)
	c := client.New(endpoint, token)

	// Setup: upload a test file via API
	testKey := fmt.Sprintf("e2e-watch-test/%d/remote.txt", time.Now().UnixNano())
	_, err := c.PutObject(testKey, strings.NewReader("remote content"), "")
	if err != nil {
		t.Fatalf("failed to upload test file: %v", err)
	}
	defer c.DeleteObject(testKey)

	// Create local dir
	localDir := t.TempDir()
	remotePrefix := filepath.Dir(testKey) + "/"

	// Build and run watch (with timeout)
	binary := buildCLI(t)
	cmd := exec.Command(binary, "watch", localDir, remotePrefix,
		"--endpoint", endpoint, "--token", token, "--poll-interval", "1s")

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start watch: %v", err)
	}

	// Wait for initial sync to complete
	deadline := time.Now().Add(15 * time.Second)
	localFile := filepath.Join(localDir, "remote.txt")
	synced := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(localFile); err == nil {
			content, _ := os.ReadFile(localFile)
			if string(content) == "remote content" {
				synced = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Stop watch
	cmd.Process.Signal(syscall.SIGINT)
	cmd.Wait()

	if !synced {
		t.Error("initial sync did not pull remote file")
	}
}

func TestE2E_WatchLocalEdit_Pushed(t *testing.T) {
	endpoint, token := skipIfNoServer(t)
	c := client.New(endpoint, token)

	localDir := t.TempDir()
	remotePrefix := fmt.Sprintf("e2e-watch-test/%d/", time.Now().UnixNano())

	// Build and run watch
	binary := buildCLI(t)
	cmd := exec.Command(binary, "watch", localDir, remotePrefix,
		"--endpoint", endpoint, "--token", token, "--poll-interval", "1s")

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start watch: %v", err)
	}

	// Wait for initial sync
	time.Sleep(3 * time.Second)

	// Create local file
	os.WriteFile(filepath.Join(localDir, "local.txt"), []byte("local content"), 0644)

	// Wait for push
	deadline := time.Now().Add(15 * time.Second)
	pushed := false
	for time.Now().Before(deadline) {
		body, _, err := c.GetObject(remotePrefix + "local.txt")
		if err == nil {
			data := make([]byte, 1024)
			n, _ := body.Read(data)
			body.Close()
			if string(data[:n]) == "local content" {
				pushed = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Cleanup
	cmd.Process.Signal(syscall.SIGINT)
	cmd.Wait()
	c.DeleteObject(remotePrefix + "local.txt")

	if !pushed {
		t.Error("local file was not pushed to remote")
	}
}

func TestE2E_WatchGracefulShutdown(t *testing.T) {
	endpoint, token := skipIfNoServer(t)

	localDir := t.TempDir()
	remotePrefix := fmt.Sprintf("e2e-watch-test/%d/", time.Now().UnixNano())

	binary := buildCLI(t)
	cmd := exec.Command(binary, "watch", localDir, remotePrefix,
		"--endpoint", endpoint, "--token", token, "--poll-interval", "1s")

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start watch: %v", err)
	}

	// Wait for initial sync
	time.Sleep(3 * time.Second)

	// Send SIGINT
	cmd.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("watch exited with: %v (expected for signal)", err)
		}
		// Check state.json exists
		state, err := s2sync.LoadState(localDir)
		if err != nil {
			t.Errorf("failed to load state after shutdown: %v", err)
		}
		if state == nil {
			t.Error("state should exist after graceful shutdown")
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Error("watch did not exit after SIGINT within 10s")
	}
}
