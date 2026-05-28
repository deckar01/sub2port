package tests

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testsDir string

var httpClient = &http.Client{Timeout: 5 * time.Second}

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(os.Stderr, "docker not found, skipping integration tests")
		os.Exit(0)
	}

	var err error
	testsDir, err = os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "getwd: %v\n", err)
		os.Exit(1)
	}

	root := filepath.Dir(testsDir)
	cmd := exec.Command("docker", "build", "-t", "sub2port", root)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "docker build failed: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// compose helpers

func composeUp(t *testing.T, file, project string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", file, "-p", project, "up", "-d")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}
}

func composeDown(file, project string) {
	exec.Command("docker", "compose", "-f", file, "-p", project,
		"down", "-v", "--remove-orphans", "-t", "5").Run()
}

func composeLogs(file, project string) string {
	cmd := exec.Command("docker", "compose", "-f", file, "-p", project,
		"logs", "--no-color", "sub2port")
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// assertion helpers

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func waitForLogs(t *testing.T, file, project string, expected []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		logs := composeLogs(file, project)
		if containsAll(logs, expected) {
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for logs\nwant: %v\ngot:\n%s", expected, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func assertLogSequence(t *testing.T, logs string, seq []string) {
	t.Helper()
	pos := 0
	for _, sub := range seq {
		idx := strings.Index(logs[pos:], sub)
		if idx < 0 {
			t.Fatalf("log sequence broken: %q not found after position %d\nlogs:\n%s", sub, pos, logs)
		}
		pos += idx + len(sub)
	}
}

func get(t *testing.T, port int, host string) (int, string) {
	t.Helper()
	addr := fmt.Sprintf("http://127.0.0.1:%d/", port)
	var lastErr error
	for range 10 {
		req, _ := http.NewRequest("GET", addr, nil)
		req.Host = host
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(body)
	}
	t.Fatalf("GET %s via port %d failed after retries: %v", host, port, lastErr)
	return 0, ""
}

func whoamiHostname(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Hostname:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "Hostname:"))
		}
	}
	return ""
}

func setup(t *testing.T, yml string, logs []string) string {
	t.Helper()
	file := filepath.Join(testsDir, yml)
	project := "sub2port-test-" + strings.TrimSuffix(yml, ".yml")

	composeDown(file, project)
	t.Cleanup(func() { composeDown(file, project) })

	composeUp(t, file, project)
	return waitForLogs(t, file, project, logs, 300*time.Second)
}

// tests

func TestSingleHost(t *testing.T) {
	seq := []string{
		"# using network",
		"# listening on",
		"+ app.test (1)",
	}
	logs := setup(t, "single-host.yml", seq)
	assertLogSequence(t, logs, seq)

	code, body := get(t, 18081, "app.test")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !strings.Contains(body, "Host: app.test") {
		t.Fatalf("response missing expected Host header\n%s", body)
	}
}

func TestOverride(t *testing.T) {
	seq := []string{
		"# using network",
		"# listening on",
		"+ app.test (1)",
		"+ app.test (2)",
	}
	logs := setup(t, "override.yml", seq)
	assertLogSequence(t, logs, seq)

	code1, body1 := get(t, 18082, "app.test")
	code2, body2 := get(t, 18082, "app.test")
	if code1 != 200 || code2 != 200 {
		t.Fatalf("expected 200 for both requests, got %d and %d", code1, code2)
	}

	if !strings.Contains(body1, "Host: app.test") {
		t.Fatalf("override failed: stale container %s", body1)
	}

	if !strings.Contains(body2, "Host: app.test") {
		t.Fatalf("override failed: stale container %s", body2)
	}

	hostname1 := whoamiHostname(body1)
	hostname2 := whoamiHostname(body2)
	if hostname1 == "" || hostname2 == "" {
		t.Fatalf("override failed: missing Hostname field\nbody1:\n%s\nbody2:\n%s", body1, body2)
	}
	if hostname1 != hostname2 {
		t.Fatalf("override failed: requests routed to different containers: %s != %s", hostname1, hostname2)
	}
}

func TestOverrideRestartOrder(t *testing.T) {
	seq := []string{
		"# using network",
		"# listening on",
		"+ app.test (1)",
		"+ app.test (2)",
	}
	setup(t, "override.yml", seq)

	// Check the initial container ID
	code, body := get(t, 18082, "app.test")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	before := whoamiHostname(body)
	if before == "" {
		t.Fatalf("response missing Hostname field\n%s", body)
	}

	// Restart sub2port
	file := filepath.Join(testsDir, "override.yml")
	project := "sub2port-test-override"
	cmd := exec.Command("docker", "compose", "-f", file, "-p", project, "restart", "sub2port")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose restart sub2port: %v\n%s", err, out)
	}

	// The container should not change
	code, body = get(t, 18082, "app.test")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	after := whoamiHostname(body)
	if after == "" {
		t.Fatalf("response missing Hostname field\n%s", body)
	}
	if before != after {
		t.Fatalf("container changed after restart: before=%s after=%s", before, after)
	}
}

func TestMultiHost(t *testing.T) {
	wait := []string{
		"# using network",
		"# listening on",
		"+ a.test (1)",
		"+ b.test (1)",
	}
	logs := setup(t, "multi-host.yml", wait)
	assertLogSequence(t, logs, []string{"# using network", "# listening on"})

	code, body := get(t, 18083, "a.test")
	if code != 200 {
		t.Fatalf("a.test: expected 200, got %d", code)
	}
	if !strings.Contains(body, "Host: a.test") {
		t.Fatalf("a.test response missing expected Host header\n%s", body)
	}

	code, body = get(t, 18083, "b.test")
	if code != 200 {
		t.Fatalf("b.test: expected 200, got %d", code)
	}
	if !strings.Contains(body, "Host: b.test") {
		t.Fatalf("b.test response missing expected Host header\n%s", body)
	}
}

func TestCustomPort(t *testing.T) {
	seq := []string{
		"# using network",
		"# listening on",
		"+ app.test (1)",
	}
	logs := setup(t, "custom-port.yml", seq)
	assertLogSequence(t, logs, seq)

	if !strings.Contains(logs, ":8080") {
		t.Fatalf("expected route to port 8080\nlogs:\n%s", logs)
	}

	code, body := get(t, 18084, "app.test")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !strings.Contains(body, "Host: app.test") {
		t.Fatalf("response missing expected Host header\n%s", body)
	}
}

func TestStopContainer(t *testing.T) {
	seq := []string{
		"# using network",
		"# listening on",
		"+ app.test (1)",
		"+ app.test (2)",
	}
	logs := setup(t, "stop-container.yml", seq)
	assertLogSequence(t, logs, seq)

	// Stop one of the two backends.
	file := filepath.Join(testsDir, "stop-container.yml")
	project := "sub2port-test-stop-container"
	cmd := exec.Command("docker", "compose", "-f", file, "-p", project, "stop", "app2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose stop app2: %v\n%s", err, out)
	}

	// Wait for the removal log line.
	waitForLogs(t, file, project, []string{"- app.test (1)"}, 30*time.Second)

	// The remaining backend should still serve requests.
	code, body := get(t, 18086, "app.test")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !strings.Contains(body, "Host: app.test") {
		t.Fatalf("response missing expected Host header\n%s", body)
	}
}

func TestDefaultPort(t *testing.T) {
	seq := []string{
		"# using network",
		"# listening on",
		"+ app.test (1)",
	}
	logs := setup(t, "default-port.yml", seq)
	assertLogSequence(t, logs, seq)

	code, body := get(t, 18085, "app.test")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !strings.Contains(body, "Host: app.test") {
		t.Fatalf("response missing expected Host header\n%s", body)
	}
}
