package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"HA-Cupbearer/internal/config"
	"HA-Cupbearer/internal/manifest"
	"HA-Cupbearer/internal/models"
)

func newHookTestManifest(t *testing.T) (*manifest.Manifest, string) {
	t.Helper()
	repo := t.TempDir()
	m := &manifest.Manifest{
		ManifestVersion: 1,
		Kind:            "service",
		Project:         "svc-a",
		DisplayName:     "Service A",
		Group:           "core",
		ServiceVersion:  "1.0.0",
		SourcePath:      filepath.Join(repo, manifest.FileName),
		Baremetal: manifest.Baremetal{
			Process:  manifest.ProcessSpec{Manager: "tinyvisor"},
			Install:  manifest.InstallSpec{BinaryName: "svc-a"},
			Artifact: manifest.ArtifactSpec{Source: "local_path"},
		},
	}
	return m, repo
}

func writeHookScript(t *testing.T, repo, content string) {
	t.Helper()
	path := filepath.Join(repo, "hooks", "run.sh")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestShellHookSuccess(t *testing.T) {
	t.Setenv("CUPBEARER_PRIVATE", "must-not-be-inherited")
	m, repo := newHookTestManifest(t)
	writeHookScript(t, repo, `[ -z "${CUPBEARER_PRIVATE+x}" ]; printf '%s|%s|%s|%s|%s' "$1" "$HA_PROJECT" "$HA_DEPLOY_VERSION" "$HA_ARTIFACT_SHA256" "$HOOK_TOKEN" > "$HA_RELEASE_DIR/result"`+"\n")
	releaseDir := t.TempDir()
	action := &manifest.FileUpdateAction{
		Type:   "shell",
		Script: "hooks/run.sh",
		Args:   []string{"--once"},
		Env:    map[string]string{"HOOK_TOKEN": "${secret:hook_token}"},
	}
	o := &Orchestrator{creds: &config.Credentials{SmokeSecrets: map[string]string{"hook_token": "top-secret"}}}
	detail, err := o.runFileUpdateAction(context.Background(), m, action, releaseDir, "1.0.0", "abc123")
	if err != nil {
		t.Fatalf("shell hook: %v (%s)", err, detail)
	}
	data, err := os.ReadFile(filepath.Join(releaseDir, "result"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "--once|svc-a|1.0.0|abc123|top-secret"; got != want {
		t.Fatalf("hook output = %q, want %q", got, want)
	}
	if strings.Contains(detail, "top-secret") {
		t.Fatalf("secret leaked in detail: %s", detail)
	}
}

func TestShellHookFailureRedactsAndBoundsOutput(t *testing.T) {
	m, repo := newHookTestManifest(t)
	writeHookScript(t, repo, `printf '%s' "$HOOK_TOKEN"; i=0; while [ "$i" -lt 3000 ]; do printf x; i=$((i+1)); done; exit 7`+"\n")
	action := &manifest.FileUpdateAction{Type: "shell", Script: "hooks/run.sh", Env: map[string]string{"HOOK_TOKEN": "${secret:hook_token}"}}
	o := &Orchestrator{creds: &config.Credentials{SmokeSecrets: map[string]string{"hook_token": "top-secret"}}}
	detail, err := o.runFileUpdateAction(context.Background(), m, action, t.TempDir(), "1.0.0", "abc")
	if err == nil {
		t.Fatal("expected shell failure")
	}
	if strings.Contains(detail, "top-secret") || !strings.Contains(detail, "[REDACTED]") {
		t.Fatalf("output was not redacted: %q", detail)
	}
	if !strings.Contains(detail, "[truncated]") || len(detail) > hookOutputLimit+200 {
		t.Fatalf("output was not bounded: len=%d detail=%q", len(detail), detail)
	}
}

func TestShellHookTimeout(t *testing.T) {
	m, repo := newHookTestManifest(t)
	writeHookScript(t, repo, "sleep 10\n")
	action := &manifest.FileUpdateAction{Type: "shell", Script: "hooks/run.sh", TimeoutSeconds: 1}
	o := &Orchestrator{}
	started := time.Now()
	detail, err := o.runFileUpdateAction(context.Background(), m, action, t.TempDir(), "1.0.0", "abc")
	if err == nil || !strings.Contains(detail+err.Error(), "timeout after 1s") {
		t.Fatalf("expected timeout, got detail=%q err=%v", detail, err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestHTTPHookSuccessAndStatusMismatch(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer top-secret" || string(body) != `{"token":"top-secret"}` {
			t.Errorf("unexpected request: method=%s auth=%q body=%q", r.Method, r.Header.Get("Authorization"), body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	action := &manifest.FileUpdateAction{
		Type:         "http",
		Method:       "POST",
		URL:          server.URL,
		Headers:      map[string]string{"Authorization": "Bearer ${secret:hook_token}"},
		Body:         `{"token":"${secret:hook_token}"}`,
		ExpectStatus: http.StatusNoContent,
	}
	o := &Orchestrator{creds: &config.Credentials{SmokeSecrets: map[string]string{"hook_token": "top-secret"}}}
	detail, err := o.runFileUpdateAction(context.Background(), &manifest.Manifest{}, action, t.TempDir(), "1.0.0", "abc")
	if err != nil || !strings.Contains(detail, "204") {
		t.Fatalf("HTTP hook failed: detail=%q err=%v", detail, err)
	}
	action.ExpectStatus = http.StatusOK
	if _, err := o.runFileUpdateAction(context.Background(), &manifest.Manifest{}, action, t.TempDir(), "1.0.0", "abc"); err == nil || !strings.Contains(err.Error(), "expect status 200, got 204") {
		t.Fatalf("expected status mismatch, got %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestHTTPHookTimeoutAndNoRedirect(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	o := &Orchestrator{}
	action := &manifest.FileUpdateAction{Type: "http", URL: server.URL + "/slow", TimeoutSeconds: 1}
	started := time.Now()
	if _, err := o.runFileUpdateAction(context.Background(), &manifest.Manifest{}, action, t.TempDir(), "1.0.0", "abc"); err == nil || !strings.Contains(err.Error(), "timeout after 1s") {
		t.Fatalf("expected HTTP timeout, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("HTTP timeout took too long: %v", elapsed)
	}
	action.URL = server.URL
	action.ExpectStatus = http.StatusFound
	if _, err := o.runFileUpdateAction(context.Background(), &manifest.Manifest{}, action, t.TempDir(), "1.0.0", "abc"); err != nil {
		t.Fatalf("expected redirect response to be accepted without following: %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatal("HTTP hook followed a redirect")
	}
}

func TestAfterFileUpdateSkipsCompletedAndRetriesFailed(t *testing.T) {
	m, repo := newHookTestManifest(t)
	writeHookScript(t, repo, `printf x >> "$HA_RELEASE_DIR/calls"`+"\n")
	m.Hooks.AfterFileUpdate = []manifest.FileUpdateAction{{Name: "record", Type: "shell", Script: "hooks/run.sh"}}
	o := &Orchestrator{}
	releaseDir := t.TempDir()
	res := &ProjectResult{}
	if err := o.runAfterFileUpdate(context.Background(), m, releaseDir, "1.0.0", "abc", true, res); err != nil {
		t.Fatal(err)
	}
	if err := o.runAfterFileUpdate(context.Background(), m, releaseDir, "1.0.0", "abc", false, res); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(releaseDir, "calls"))
	if string(data) != "x" {
		t.Fatalf("completed hooks repeated: %q", data)
	}
	m.Hooks.AfterFileUpdate[0].TimeoutSeconds = 31
	if err := o.runAfterFileUpdate(context.Background(), m, releaseDir, "1.0.0", "abc", false, res); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(releaseDir, "calls"))
	if string(data) != "xx" {
		t.Fatalf("changed hook configuration did not rerun: %q", data)
	}

	writeHookScript(t, repo, "printf x >> \"$HA_RELEASE_DIR/calls\"\nexit 1\n")
	failedDir := t.TempDir()
	if err := o.runAfterFileUpdate(context.Background(), m, failedDir, "1.0.0", "abc", true, &ProjectResult{}); err == nil {
		t.Fatal("expected first hook attempt to fail")
	}
	if err := o.runAfterFileUpdate(context.Background(), m, failedDir, "1.0.0", "abc", false, &ProjectResult{}); err == nil {
		t.Fatal("expected unchanged artifact with unfinished hooks to retry")
	}
	data, _ = os.ReadFile(filepath.Join(failedDir, "calls"))
	if string(data) != "xx" {
		t.Fatalf("failed hooks were not retried: %q", data)
	}
}

func TestHookScriptSymlinkEscapeRejected(t *testing.T) {
	m, repo := newHookTestManifest(t)
	outside := filepath.Join(t.TempDir(), "outside.sh")
	if err := os.WriteFile(outside, []byte("exit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "hooks", "run.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := hookScriptPath(m, &manifest.FileUpdateAction{Script: "hooks/run.sh"}); err == nil || !strings.Contains(err.Error(), "escapes manifest directory") {
		t.Fatalf("expected symlink escape rejection, got %v", err)
	}
}

func TestPreflightFileUpdateHooks(t *testing.T) {
	m, repo := newHookTestManifest(t)
	m.Hooks.AfterFileUpdate = []manifest.FileUpdateAction{{Type: "shell", Script: "hooks/run.sh", Env: map[string]string{"HOOK_TOKEN": "${secret:absent}"}}}
	o := &Orchestrator{creds: &config.Credentials{}}
	report := &PreflightReport{}
	o.checkFileUpdateHooks(m, report)
	if len(report.Blockers) != 1 || !strings.Contains(report.Blockers[0].Detail, "resolve hook script") {
		t.Fatalf("expected missing script blocker, got %+v", report.Blockers)
	}
	writeHookScript(t, repo, "exit 0\n")
	report = &PreflightReport{}
	o.checkFileUpdateHooks(m, report)
	if len(report.Blockers) != 1 || !strings.Contains(report.Blockers[0].Detail, "absent") {
		t.Fatalf("expected missing secret blocker, got %+v", report.Blockers)
	}
	o.creds.SmokeSecrets = map[string]string{"absent": "configured"}
	report = &PreflightReport{}
	o.checkFileUpdateHooks(m, report)
	if len(report.Blockers) != 0 || len(report.Results) != 1 || !report.Results[0].OK {
		t.Fatalf("expected passing hook preflight, got %+v", report)
	}
}

func newManagedHookEnv(t *testing.T, hook string) (*Orchestrator, *manifest.Manifest, string, *tinyvisorStub) {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	root := filepath.Join(base, "install")
	artifactPath := filepath.Join(base, "svc-a-linux-amd64")
	payload := []byte("binary-v1")
	if err := os.WriteFile(artifactPath, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if err := os.WriteFile(artifactPath+".sha256", []byte(hex.EncodeToString(sum[:])+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		ManifestVersion: 1,
		Kind:            "service",
		Project:         "svc-a",
		DisplayName:     "Service A",
		Group:           "core",
		ServiceVersion:  "1.0.0",
		SourcePath:      filepath.Join(repo, manifest.FileName),
		Baremetal: manifest.Baremetal{
			Process:  manifest.ProcessSpec{Manager: "tinyvisor"},
			Install:  manifest.InstallSpec{Root: root, BinaryName: "svc-a", KeepReleases: 3},
			Artifact: manifest.ArtifactSpec{Source: "local_path", LocalPath: artifactPath},
		},
	}
	writeHookScript(t, repo, hook)
	m.Hooks.AfterFileUpdate = []manifest.FileUpdateAction{{Name: "before-activation", Type: "shell", Script: "hooks/run.sh"}}
	stub := newTinyvisorStub(t, "test-key")
	cfg := &models.Config{}
	cfg.Orchestration.Tinyvisor.URL = stub.server.URL
	cfg.Orchestration.ProbeHost = "127.0.0.1"
	cfg.Orchestration.StateDir = filepath.Join(base, "state")
	return New(cfg, &config.Credentials{TinyvisorAPIKey: "test-key"}), m, root, stub
}

func TestDeployHookFailureDoesNotActivate(t *testing.T) {
	o, m, root, _ := newManagedHookEnv(t, "exit 9\n")
	if err := os.MkdirAll(releaseDir(root, "0.9.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := switchSymlink(root, "0.9.0"); err != nil {
		t.Fatal(err)
	}
	res := &ProjectResult{ActiveVersion: "0.9.0"}
	o.deployManaged(context.Background(), &Run{}, m, o.effective(m), res)
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s, want failed: %+v", res.Outcome, res)
	}
	if got := readActiveVersion(root); got != "0.9.0" {
		t.Fatalf("current activated %q after hook failure", got)
	}
	if res.ActiveVersion != "0.9.0" {
		t.Fatalf("active version = %q, want 0.9.0", res.ActiveVersion)
	}
	found := false
	for _, step := range res.Steps {
		if strings.HasPrefix(step.Name, "after-file-update") && !step.OK {
			found = true
		}
		if step.Name == "symlink" {
			t.Fatal("symlink step ran after hook failure")
		}
	}
	if !found {
		t.Fatalf("missing failed hook step: %+v", res.Steps)
	}
}

func startHookHealth(t *testing.T, m *manifest.Manifest) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	m.Facts.Ports = []manifest.Port{{Name: "http", Port: port}}
	m.Facts.Health.HTTP = &manifest.HTTPProbe{PortName: "http", Path: "/healthz", ExpectStatus: 200, TimeoutSeconds: 1, IntervalSeconds: 1, Retries: 1}
}

func TestDeployHookRunsBeforeActivation(t *testing.T) {
	o, m, root, _ := newManagedHookEnv(t, `root=$(dirname "$(dirname "$HA_RELEASE_DIR")"); [ "$(readlink "$root/current")" = "releases/0.9.0" ]; printf ok > "$HA_RELEASE_DIR/hook-ran"`+"\n")
	if err := os.MkdirAll(releaseDir(root, "0.9.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := switchSymlink(root, "0.9.0"); err != nil {
		t.Fatal(err)
	}
	startHookHealth(t, m)
	res := &ProjectResult{ActiveVersion: "0.9.0"}
	o.deployManaged(context.Background(), &Run{}, m, o.effective(m), res)
	if res.Outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s, want deployed: %+v", res.Outcome, res)
	}
	if got := readActiveVersion(root); got != "1.0.0" {
		t.Fatalf("current = %q, want 1.0.0", got)
	}
	if _, err := os.Stat(filepath.Join(releaseDir(root, "1.0.0"), "hook-ran")); err != nil {
		t.Fatalf("hook did not run before activation: %v", err)
	}
}

func TestSameVersionArtifactUpdateRunsHooksAndRestarts(t *testing.T) {
	o, m, root, stub := newManagedHookEnv(t, `printf x >> "$HA_RELEASE_DIR/calls"`+"\n")
	startHookHealth(t, m)
	first := &ProjectResult{}
	o.deployManaged(context.Background(), &Run{}, m, o.effective(m), first)
	if first.Outcome != OutcomeDeployed || stub.restartCount("svc-a") != 1 {
		t.Fatalf("first deploy failed: result=%+v restarts=%d", first, stub.restartCount("svc-a"))
	}
	payload := []byte("binary-v2")
	if err := os.WriteFile(m.Baremetal.Artifact.LocalPath, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if err := os.WriteFile(m.Baremetal.Artifact.LocalPath+".sha256", []byte(hex.EncodeToString(sum[:])+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := &ProjectResult{ActiveVersion: readActiveVersion(root)}
	o.deployManaged(context.Background(), &Run{}, m, o.effective(m), second)
	if second.Outcome != OutcomeDeployed || stub.restartCount("svc-a") != 2 {
		t.Fatalf("same-version update did not restart: result=%+v restarts=%d", second, stub.restartCount("svc-a"))
	}
	data, err := os.ReadFile(filepath.Join(releaseDir(root, "1.0.0"), "calls"))
	if err != nil || string(data) != "xx" {
		t.Fatalf("hooks did not run for both writes: data=%q err=%v", data, err)
	}
}

func TestForeignProjectDoesNotRunFileUpdateHooks(t *testing.T) {
	o, m, repo, _ := newManagedHookEnv(t, `printf ran > "$(dirname "$0")/foreign-ran"`+"\n")
	m.Baremetal.Process.Manager = "manual"
	o.cfg = &models.Config{}
	res := o.executeProject(context.Background(), &Run{Results: map[string]*ProjectResult{}}, m, nil)
	if res.Outcome != OutcomeForeign {
		t.Fatalf("outcome = %s, want foreign", res.Outcome)
	}
	if _, err := os.Stat(filepath.Join(repo, "foreign-ran")); !os.IsNotExist(err) {
		t.Fatalf("foreign hook executed: %v", err)
	}
}

func TestHookStateFingerprintTracksDeclaration(t *testing.T) {
	m, _ := newHookTestManifest(t)
	m.Hooks.AfterFileUpdate = []manifest.FileUpdateAction{{Type: "http", URL: "https://example.invalid", Headers: map[string]string{"Authorization": "${secret:token}"}}}
	first, err := hookFingerprint(m, "abc")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m.Hooks.AfterFileUpdate)
	if !strings.Contains(string(data), "${secret:token}") {
		t.Fatal("test manifest lost secret reference")
	}
	second, _ := hookFingerprint(m, "abc")
	if first != second {
		t.Fatal("same hook declaration produced different fingerprints")
	}
	m.Hooks.AfterFileUpdate[0].URL = "https://example.invalid/changed"
	third, _ := hookFingerprint(m, "abc")
	if third == first {
		t.Fatal("changed hook declaration retained old fingerprint")
	}
}
