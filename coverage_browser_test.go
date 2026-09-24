package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoverageBrowser(t *testing.T) {
	if os.Getenv("CPA_BROWSER_TESTS") != "1" {
		t.Skip("set CPA_BROWSER_TESTS=1 to run the browser integration test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("browser integration requires Node.js 22 or newer")
	}
	var chrome string
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		t.Fatal("browser integration requires Chrome or Chromium")
	}
	artifacts := os.Getenv("CPA_BROWSER_ARTIFACT_DIR")
	if artifacts == "" {
		artifacts = t.TempDir()
	}
	artifacts, err = filepath.Abs(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(filepath.Join(t.TempDir(), "browser.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = seedPrices(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	seedCoverageUsage(t, s, "a-mixed", true, true)
	seedCoverageUsage(t, s, "b-cpa", true, true)
	if err=s.addDroppedUsageCount(context.Background(),2);err!=nil{t.Fatal(err)}
	a := &app{cfg: defaultConfig(), store: s}
	var failWrite, delayRead, failRefresh atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/test/fail-write" {
			failWrite.Store(true)
			return
		}
		if r.URL.Path == "/test/delay-read" {
			delayRead.Store(true)
			return
		}
		if r.URL.Path == "/test/fail-refresh" {
			failRefresh.Store(true)
			return
		}
		if r.URL.Path == "/v0/management/auth-files" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"files":[{"name":"a-mixed","provider":"codex","plan_type":"pro"},{"name":"b-cpa","provider":"codex","plan_type":"pro"}]}`)
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/dashboard" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(dashboardHTML)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/coverage-settings") && r.Method == "POST" && failWrite.Swap(false) {
			http.Error(w, "injected save failure", 500)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/summary") && failRefresh.Swap(false) {
			http.Error(w, "injected refresh failure", 500)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/summary") && r.URL.Query().Get("account") == "a-mixed" && delayRead.Swap(false) {
			time.Sleep(400 * time.Millisecond)
		}
		body, _ := io.ReadAll(r.Body)
		response := a.handleManagement(managementRequest{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/v0/management"), Query: r.URL.Query(), Body: body})
		for name, values := range response.Headers {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		w.Write(response.Body)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "web/coverage.browser-test.mjs", server.URL, chrome, artifacts)
	output, err := command.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatal(err)
	}
}
