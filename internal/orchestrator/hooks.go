package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"HA-Cupbearer/internal/manifest"
)

const (
	hookDefaultTimeoutSeconds = 30
	hookOutputLimit           = 2048
	hookStateFile             = ".cupbearer-after-file-update"
)

type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := hookOutputLimit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		_, _ = b.buf.Write(p)
	} else if len(p) > 0 {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	out := b.buf.String()
	if b.truncated {
		out += "… [truncated]"
	}
	return out
}

func hookScriptPath(m *manifest.Manifest, action *manifest.FileUpdateAction) (string, error) {
	path, err := m.ResolveHookScript(action.Script)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(filepath.Dir(m.SourcePath))
	if err != nil {
		return "", fmt.Errorf("resolve manifest directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve hook script %s: %w", path, err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("hook script %s escapes manifest directory via symlink", path)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat hook script %s: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("hook script %s is not a regular file", resolved)
	}
	return resolved, nil
}

func hookFingerprint(m *manifest.Manifest, sha string) (string, error) {
	payload := struct {
		Version int                         `json:"version"`
		SHA256  string                      `json:"sha256"`
		Actions []manifest.FileUpdateAction `json:"actions"`
	}{Version: 1, SHA256: sha, Actions: m.Hooks.AfterFileUpdate}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (o *Orchestrator) runAfterFileUpdate(ctx context.Context, m *manifest.Manifest, releaseDir, version, sha string, changed bool, res *ProjectResult) error {
	if len(m.Hooks.AfterFileUpdate) == 0 {
		return nil
	}
	fingerprint, err := hookFingerprint(m, sha)
	if err != nil {
		return err
	}
	statePath := filepath.Join(releaseDir, hookStateFile)
	if changed {
		if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
			res.Steps = append(res.Steps, StepRecord{Name: "after-file-update", OK: false, Detail: "clear stale hook state: " + err.Error()})
			return err
		}
	} else {
		data, err := os.ReadFile(statePath)
		if err != nil && !os.IsNotExist(err) {
			res.Steps = append(res.Steps, StepRecord{Name: "after-file-update", OK: false, Detail: "read hook state: " + err.Error()})
			return err
		}
		if strings.TrimSpace(string(data)) == fingerprint {
			res.Steps = append(res.Steps, StepRecord{Name: "after-file-update", OK: true, Detail: "skipped: artifact unchanged and hooks already completed"})
			return nil
		}
	}

	for i := range m.Hooks.AfterFileUpdate {
		action := &m.Hooks.AfterFileUpdate[i]
		name := action.Name
		if name == "" {
			name = fmt.Sprintf("action[%d]", i+1)
		}
		detail, err := o.runFileUpdateAction(ctx, m, action, releaseDir, version, sha)
		step := StepRecord{Name: fmt.Sprintf("after-file-update %s (%s)", name, action.Type), OK: err == nil, Detail: detail}
		if err != nil {
			step.Detail += ": " + o.redactHookOutput(err.Error())
		}
		res.Steps = append(res.Steps, step)
		if err != nil {
			return fmt.Errorf("after-file-update action %q: %w", name, err)
		}
	}
	if err := atomicWrite(statePath, []byte(fingerprint+"\n")); err != nil {
		res.Steps = append(res.Steps, StepRecord{Name: "after-file-update", OK: false, Detail: "persist hook completion state: " + err.Error()})
		return err
	}
	return nil
}

func (o *Orchestrator) runFileUpdateAction(ctx context.Context, m *manifest.Manifest, action *manifest.FileUpdateAction, releaseDir, version, sha string) (string, error) {
	timeout := action.TimeoutSeconds
	if timeout == 0 {
		timeout = hookDefaultTimeoutSeconds
	}
	actionCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	if action.Type == "shell" {
		return o.runShellHook(actionCtx, m, action, releaseDir, version, sha, timeout)
	}
	return o.runHTTPHook(actionCtx, action, timeout)
}

func (o *Orchestrator) runShellHook(ctx context.Context, m *manifest.Manifest, action *manifest.FileUpdateAction, releaseDir, version, sha string, timeout int) (string, error) {
	script, err := hookScriptPath(m, action)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{script}, action.Args...)...)
	cmd.Dir = releaseDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = hookBaseEnv()
	cmd.Env = append(cmd.Env,
		"HA_PROJECT="+m.Project,
		"HA_SERVICE_VERSION="+m.ServiceVersion,
		"HA_DEPLOY_VERSION="+version,
		"HA_RELEASE_DIR="+releaseDir,
		"HA_ARTIFACT_PATH="+filepath.Join(releaseDir, m.Baremetal.Install.BinaryName),
		"HA_ARTIFACT_SHA256="+sha,
	)
	for _, key := range sortedHeaderKeys(action.Env) {
		value, err := o.resolveDeploymentSecrets(action.Env[key])
		if err != nil {
			return "", err
		}
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timeout after %ds", timeout)
		} else if ctx.Err() != nil {
			err = fmt.Errorf("canceled: %w", ctx.Err())
		}
	}
	detail := "exit 0"
	if err != nil {
		detail = "shell failed"
	}
	if out := strings.TrimSpace(stdout.String()); out != "" {
		detail += "; stdout: " + o.redactHookOutput(out)
	}
	if out := strings.TrimSpace(stderr.String()); out != "" {
		detail += "; stderr: " + o.redactHookOutput(out)
	}
	return detail, err
}

func hookBaseEnv() []string {
	env := make([]string, 0, 6)
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (o *Orchestrator) runHTTPHook(ctx context.Context, action *manifest.FileUpdateAction, timeout int) (string, error) {
	method := strings.ToUpper(action.Method)
	if method == "" {
		method = http.MethodGet
	}
	body, err := o.resolveDeploymentSecrets(action.Body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, method, action.URL, strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	for _, key := range sortedHeaderKeys(action.Headers) {
		value, err := o.resolveDeploymentSecrets(action.Headers[key])
		if err != nil {
			return "", err
		}
		req.Header.Set(key, value)
	}
	if body != "" {
		contentType := action.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("timeout after %ds", timeout)
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("canceled: %w", ctx.Err())
		}
		return "", fmt.Errorf("request failed: %s", hookRequestError(err))
	}
	defer resp.Body.Close()
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return "", fmt.Errorf("read response: %w", readErr)
	}
	expect := action.ExpectStatus
	if expect == 0 {
		expect = http.StatusOK
	}
	address := hookHTTPAddress(req.URL)
	if resp.StatusCode != expect {
		return "", fmt.Errorf("%s %s: expect status %d, got %d", method, address, expect, resp.StatusCode)
	}
	return fmt.Sprintf("%s %s -> %d", method, address, resp.StatusCode), nil
}

func hookRequestError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}

func hookHTTPAddress(u *url.URL) string {
	return u.Host + u.Path
}

func (o *Orchestrator) redactHookOutput(text string) string {
	values := make([]string, 0)
	if o.creds != nil {
		values = append(values,
			o.creds.TinyvisorAPIKey,
			o.creds.DeployToken,
			o.creds.GithubToken,
			o.creds.GrailReadToken,
		)
		for _, value := range o.creds.SmokeSecrets {
			values = append(values, value)
		}
	}
	filtered := values[:0]
	for _, value := range values {
		if value != "" {
			filtered = append(filtered, value)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return len(filtered[i]) > len(filtered[j]) })
	if len(filtered) == 0 {
		return text
	}
	pairs := make([]string, 0, len(filtered)*2)
	for _, value := range filtered {
		pairs = append(pairs, value, "[REDACTED]")
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

func (o *Orchestrator) hookSecretValues(texts ...string) []string {
	missing := map[string]bool{}
	for _, text := range texts {
		for _, name := range smokeSecretNames(text) {
			if o.creds == nil || o.creds.SmokeSecrets == nil || o.creds.SmokeSecrets[name] == "" {
				missing[name] = true
			}
		}
	}
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
