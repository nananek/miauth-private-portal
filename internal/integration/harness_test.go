package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// repoRoot resolves the repository root from this file's own source
// location, so these tests can `go build` cmd/server and cmd/miauthctl
// regardless of the working directory `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this file's own path")
	}
	// This file lives at <repoRoot>/internal/integration/harness_test.go.
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// buildBinary compiles pkgPath (a package path relative to the repo
// root, e.g. "./cmd/server") into a fresh temporary directory and
// returns the resulting executable's path. Building through `go build`
// rather than requiring a pre-built bin/ output keeps `go test ./...`
// alone sufficient to exercise the real binary's startup wiring.
func buildBinary(t *testing.T, pkgPath, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, pkgPath)
	cmd.Dir = repoRoot(t)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkgPath, err, output)
	}
	return out
}

// freePort asks the OS for an unused loopback TCP port and releases it
// immediately. The release-then-bind gap is an inherent, narrow race,
// but is standard practice for this kind of test and mirrors what
// scripts/run-contract-tests.sh's fixed test port was already trying to
// avoid colliding with.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// syncBuffer is a concurrency-safe io.Writer/String() pair for capturing
// a subprocess's combined stdout+stderr while the test goroutine may
// read it (for a failure message) at the same time the OS is still
// writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// mergeEnv builds a subprocess environment starting from the current
// process's own environment (config.Load's doc comment guarantees it
// only ever reads its own known keys by name, so ambient variables like
// PATH/HOME/GOCACHE can never affect it) with overrides applied on top,
// replacing rather than duplicating any key that also appears in base.
func mergeEnv(overrides map[string]string) []string {
	base := os.Environ()
	result := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			key = kv[:idx]
		}
		if _, overridden := overrides[key]; overridden {
			continue
		}
		result = append(result, kv)
	}
	for k, v := range overrides {
		result = append(result, k+"="+v)
	}
	return result
}

// testServer is a running cmd/server subprocess plus the state needed to
// approve a MiAuth session against it, call its API, and shut it down.
type testServer struct {
	cmd     *exec.Cmd
	baseURL string
	dbPath  string
	output  *syncBuffer
	exited  chan struct{}
	waitErr error
}

// startServer launches serverBin as a subprocess in a fresh temporary
// working directory (so cmd/server's default "./.env" lookup can never
// pick up a real developer .env) with a minimal required baseline
// environment; extraEnv overrides or extends that baseline, e.g. to
// reuse an existing DB_PATH across a restart or to enable LLM_*.
func startServer(t *testing.T, serverBin string, extraEnv map[string]string) *testServer {
	t.Helper()
	workDir := t.TempDir()
	port := freePort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	env := map[string]string{
		"APP_ENV":      "development",
		"HTTP_HOST":    "127.0.0.1",
		"HTTP_PORT":    fmt.Sprintf("%d", port),
		"DB_PATH":      filepath.Join(t.TempDir(), "portal.db"),
		"LOCAL_ORIGIN": baseURL,
	}
	for k, v := range extraEnv {
		env[k] = v
	}

	cmd := exec.Command(serverBin)
	cmd.Dir = workDir
	cmd.Env = mergeEnv(env)
	output := &syncBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", serverBin, err)
	}

	ts := &testServer{cmd: cmd, baseURL: baseURL, dbPath: env["DB_PATH"], output: output, exited: make(chan struct{})}
	go func() {
		ts.waitErr = cmd.Wait()
		close(ts.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-ts.exited:
			return
		default:
		}
		_ = cmd.Process.Kill()
		<-ts.exited
	})
	return ts
}

// waitForReady polls /readyz until it returns 200, failing the test if
// the process exits first or the deadline passes.
func (ts *testServer) waitForReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ts.exited:
			t.Fatalf("server exited before becoming ready (err=%v):\n%s", ts.waitErr, ts.output.String())
		default:
		}
		resp, err := client.Get(ts.baseURL + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server never became ready within %s:\n%s", timeout, ts.output.String())
}

// terminateAndWait sends SIGTERM (the same signal cmd/server's own
// signal.NotifyContext listens for) and waits for the process to exit,
// failing the test if it does not do so within timeout.
func (ts *testServer) terminateAndWait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	if err := ts.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case <-ts.exited:
		return ts.waitErr
	case <-time.After(timeout):
		t.Fatalf("server did not exit within %s after SIGTERM:\n%s", timeout, ts.output.String())
		return nil
	}
}

// approveMiAuthSession drives the same host-local approval flow an
// operator uses (README's "Approving Aria sign-ins"): it opens a pending
// MiAuth session the way Aria's browser/deep-link step would, approves
// it non-interactively through the real miauthctl binary, then completes
// the check to obtain a local API token.
func approveMiAuthSession(t *testing.T, miauthctlBin string, ts *testServer, permission string) string {
	t.Helper()
	sessionID := fmt.Sprintf("it-%d", time.Now().UnixNano())

	startURL := fmt.Sprintf("%s/miauth/%s?permission=%s", ts.baseURL, sessionID, url.QueryEscape(permission))
	resp, err := http.Get(startURL)
	if err != nil {
		t.Fatalf("GET %s: %v", startURL, err)
	}
	resp.Body.Close()

	approveCmd := exec.Command(miauthctlBin, "approve", "--yes", sessionID)
	approveCmd.Env = mergeEnv(map[string]string{
		"APP_ENV":      "development",
		"DB_PATH":      ts.dbPath,
		"LOCAL_ORIGIN": ts.baseURL,
	})
	if out, err := approveCmd.CombinedOutput(); err != nil {
		t.Fatalf("miauthctl approve %s: %v\n%s", sessionID, err, out)
	}

	var check struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	checkResp := postJSON(t, fmt.Sprintf("%s/api/miauth/%s/check", ts.baseURL, sessionID), map[string]any{}, &check)
	if checkResp.StatusCode != http.StatusOK || !check.OK || check.Token == "" {
		t.Fatalf("miauth check for %s did not return an ok token (status=%d)", sessionID, checkResp.StatusCode)
	}
	return check.Token
}

// postJSON POSTs reqBody as JSON to targetURL and, if respBody is
// non-nil, decodes the response body into it. It returns the response
// with its body already drained/closed, so only its status/headers
// remain usable afterward.
func postJSON(t *testing.T, targetURL string, reqBody any, respBody any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request body for %s: %v", targetURL, err)
	}
	resp, err := http.Post(targetURL, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("POST %s: %v", targetURL, err)
	}
	defer resp.Body.Close()
	if respBody != nil {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			t.Fatalf("decode response from %s: %v", targetURL, err)
		}
	}
	return resp
}
