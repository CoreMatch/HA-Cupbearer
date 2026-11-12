package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"HA-Cupbearer/internal/docker"
)

// fakeHintDocker 是预填收集器的容器只读面假驱动。
type fakeHintDocker struct {
	envs    map[string]map[string]string
	compose []docker.ComposeContainerRef
}

func (f *fakeHintDocker) ContainerEnv(_ context.Context, name string) (map[string]string, error) {
	if env, ok := f.envs[name]; ok {
		return env, nil
	}
	return nil, docker.ErrNotFound
}

func (f *fakeHintDocker) ListComposeContainers(_ context.Context) ([]docker.ComposeContainerRef, error) {
	return f.compose, nil
}

func hintTestInputs(t *testing.T) *Inputs {
	t.Helper()
	var in Inputs
	in.fillDefaults()
	return &in
}

// ①类来源：受管容器 env 的凭据验证通过 → 回显并标来源；审计只含指纹。
func TestHintCollectorManagedContainerEnvWins(t *testing.T) {
	store := NewStore(t.TempDir())
	dk := &fakeHintDocker{envs: map[string]map[string]string{
		MySQLContainerName: {"MYSQL_ROOT_PASSWORD": "rootpw-abc"},
	}}
	c := NewHintCollector(store, dk, filepath.Join(t.TempDir(), "deploy/.env"))
	c.verifyAdminOverride = func(_ context.Context, pw string) bool { return pw == "rootpw-abc" }
	c.verifyAppOverride = func(context.Context, string) bool { return false }
	c.verifyRedisOverride = func(context.Context, string) bool { return false }

	rep := c.Collect(context.Background(), hintTestInputs(t), "container", "container", "127.0.0.1:41000")
	var admin *CredentialHint
	for i := range rep.Hints {
		if rep.Hints[i].Target == TargetMySQLAdmin {
			admin = &rep.Hints[i]
		}
	}
	if admin == nil {
		t.Fatal("mysql_admin hint missing")
	}
	if !admin.Found || admin.Source != SourceManagedContainerEnv || admin.Value != "rootpw-abc" {
		t.Fatalf("expected managed_container_env echo, got %+v", admin)
	}
	if !strings.Contains(admin.Provenance, MySQLContainerName) {
		t.Fatalf("provenance should name the container: %s", admin.Provenance)
	}

	// 审计：一次明文回显一条记录；只含指纹，绝不含明文。
	audit, err := os.ReadFile(filepath.Join(store.dir, "credential-audit.log"))
	if err != nil {
		t.Fatalf("audit log missing: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(audit)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 audit line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], Fingerprint("rootpw-abc")) {
		t.Fatalf("audit must carry the fingerprint: %s", lines[0])
	}
	if strings.Contains(lines[0], "rootpw-abc") {
		t.Fatal("audit must never carry plaintext")
	}
	if !strings.Contains(lines[0], "127.0.0.1:41000") {
		t.Fatalf("audit must carry the caller address: %s", lines[0])
	}
}

// 验证不通过的候选一律不回显（只计数）。
func TestHintCollectorUnverifiedNeverEchoed(t *testing.T) {
	store := NewStore(t.TempDir())
	// 状态里有一个候选，但验证器拒绝一切。
	if _, _, err := store.EnsureInfraSecret(KeyMySQLRootPassword); err != nil {
		t.Fatal(err)
	}
	c := NewHintCollector(store, nil, filepath.Join(t.TempDir(), "deploy/.env"))
	c.verifyAdminOverride = func(context.Context, string) bool { return false }

	rep := c.Collect(context.Background(), hintTestInputs(t), "external", "external", "")
	c.verifyAppOverride = func(context.Context, string) bool { return false }
	c.verifyRedisOverride = func(context.Context, string) bool { return false }

	admin := rep.Hints[0]
	if admin.Found || admin.Value != "" || admin.Source != SourceNone {
		t.Fatalf("unverified candidate must never be echoed: %+v", admin)
	}
	if admin.CandidatesChecked == 0 {
		t.Fatal("candidates were present; checked count must be > 0")
	}
	if !strings.Contains(admin.Detail, "外部实例") {
		t.Fatalf("external situation must get the external copy: %s", admin.Detail)
	}
	if _, err := os.Stat(filepath.Join(store.dir, "credential-audit.log")); !os.IsNotExist(err) {
		t.Fatal("no echo happened; audit log must not exist")
	}
}

// 外部 .env（无本工具来源标记，或标记之前的键）绝不作为预填来源。
func TestHintCollectorExternalEnvNeverEchoed(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "deploy/.env")
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// 无标记：整文件按外部对待。
	if err := os.WriteFile(envPath, []byte("MYSQL_ROOT_PASSWORD=external-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewHintCollector(NewStore(t.TempDir()), nil, envPath)
	c.verifyAdminOverride = func(context.Context, string) bool { return true } // 即便验证通过也不该有候选
	c.verifyAppOverride = func(context.Context, string) bool { return false }
	c.verifyRedisOverride = func(context.Context, string) bool { return false }
	rep := c.Collect(context.Background(), hintTestInputs(t), "", "", "")
	if rep.Hints[0].Found {
		t.Fatal("marker-less .env is external; must not prefill")
	}

	// 有标记但键在标记之前（外部既有值）：同样不预填。
	content := "MYSQL_ROOT_PASSWORD=external-2\n\n# ---- 以下变量" + envFileMarker + "（幂等；已有值不覆盖） ----\nMANAGE_TOKEN=abc\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rep = c.Collect(context.Background(), hintTestInputs(t), "", "", "")
	if rep.Hints[0].Found {
		t.Fatal("keys before the provenance marker are external; must not prefill")
	}
}

// ②类来源：本实例 .env 中本工具补齐的键可预填。
func TestHintCollectorOwnEnvFileAppendedKey(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "deploy/.env")
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "EXTERNAL_KEY=whatever\n\n# ---- 以下变量" + envFileMarker + "（幂等；已有值不覆盖） ----\n" +
		"MYSQL_ROOT_PASSWORD=tool-written\nDB_USER=hrpa\nDB_PASSWORD=app-tool-written\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewHintCollector(NewStore(t.TempDir()), nil, envPath)
	c.verifyAdminOverride = func(_ context.Context, pw string) bool { return pw == "tool-written" }
	c.verifyAppOverride = func(_ context.Context, pw string) bool { return pw == "app-tool-written" }
	c.verifyRedisOverride = func(context.Context, string) bool { return false }

	rep := c.Collect(context.Background(), hintTestInputs(t), "", "", "")
	admin, app := rep.Hints[0], rep.Hints[1]
	if !admin.Found || admin.Source != SourceEnvFile || admin.Value != "tool-written" {
		t.Fatalf("own env_file admin hint wrong: %+v", admin)
	}
	if !app.Found || app.Source != SourceEnvFile || app.Value != "app-tool-written" {
		t.Fatalf("own env_file app hint wrong: %+v", app)
	}
}

// ③类来源：历史实例的 state 副本与 .env——报出"来自哪个目录/哪次初始化"。
func TestHintCollectorHistoricalSources(t *testing.T) {
	// 历史实例根：root/data/bootstrap/state.json + root/.env（带标记）。
	root := filepath.Join(t.TempDir(), "old-instance")
	if err := os.MkdirAll(filepath.Join(root, "data", "bootstrap"), 0o700); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 11, 10, 9, 30, 0, 0, time.Local)
	state := `{"version":1,"created_at":"` + created.Format(time.RFC3339) + `","updated_at":"` +
		created.Format(time.RFC3339) + `","secrets":{"DB_PASSWORD":"hist-app-pw","MYSQL_ROOT_PASSWORD":"hist-root-pw"}}`
	if err := os.WriteFile(filepath.Join(root, "data", "bootstrap", "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ScanRootsEnvVar, root)

	c := NewHintCollector(NewStore(t.TempDir()), nil, filepath.Join(t.TempDir(), "deploy/.env"))
	// 验证器只认历史值，模拟"本实例没初始化过、但机器上别处有同类账号"。
	c.verifyAdminOverride = func(_ context.Context, pw string) bool { return pw == "hist-root-pw" }
	c.verifyAppOverride = func(_ context.Context, pw string) bool { return pw == "hist-app-pw" }
	c.verifyRedisOverride = func(context.Context, string) bool { return false }

	rep := c.Collect(context.Background(), hintTestInputs(t), "container", "absent", "")
	admin, app := rep.Hints[0], rep.Hints[1]
	if !admin.Found || admin.Source != SourceHistorical || admin.Value != "hist-root-pw" {
		t.Fatalf("historical admin hint wrong: %+v", admin)
	}
	if !strings.Contains(admin.Provenance, root) || !strings.Contains(admin.Provenance, "2026-11-10") {
		t.Fatalf("historical provenance must name the directory and initialization time: %s", admin.Provenance)
	}
	if !app.Found || app.Source != SourceHistorical || app.Value != "hist-app-pw" {
		t.Fatalf("historical app hint wrong: %+v", app)
	}
}

// ③类边界：任意 JSON 不按 state 对待；无已知键的 state 不采纳。
func TestHintCollectorHistoricalRejectsForeignState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.json"),
		[]byte(`{"version":1,"secrets":{"SOME_OTHER_KEY":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ScanRootsEnvVar, root)
	c := NewHintCollector(NewStore(t.TempDir()), nil, filepath.Join(t.TempDir(), "deploy/.env"))
	c.verifyAdminOverride = func(context.Context, string) bool { return true }
	rep := c.Collect(context.Background(), hintTestInputs(t), "", "", "")
	c.verifyAppOverride = func(context.Context, string) bool { return false }
	c.verifyRedisOverride = func(context.Context, string) bool { return false }

	if rep.Hints[0].Found {
		t.Fatal("state without known keys must not be treated as this tool's state")
	}
}

// ①类来源（应用账号）：平台服务容器声明的 DB_USER 必须与目标账号一致。
func TestHintCollectorAppUserFromPlatformContainer(t *testing.T) {
	dk := &fakeHintDocker{
		envs: map[string]map[string]string{
			"ha-platform-hrpauth-1": {"DB_USER": "hrpa", "DB_PASSWORD": "live-app-pw"},
			"ha-platform-other-1":   {"DB_USER": "someone", "DB_PASSWORD": "other-pw"},
		},
		compose: []docker.ComposeContainerRef{
			{Name: "ha-platform-hrpauth-1", Project: "ha-platform"},
			{Name: "ha-platform-other-1", Project: "ha-platform"},
		},
	}
	c := NewHintCollector(NewStore(t.TempDir()), dk, filepath.Join(t.TempDir(), "deploy/.env"))
	c.verifyAppOverride = func(_ context.Context, pw string) bool { return pw == "live-app-pw" }
	c.verifyAdminOverride = func(context.Context, string) bool { return false }
	c.verifyRedisOverride = func(context.Context, string) bool { return false }

	rep := c.Collect(context.Background(), hintTestInputs(t), "container", "container", "")
	app := rep.Hints[1]
	if !app.Found || app.Source != SourceManagedContainerEnv || app.Value != "live-app-pw" {
		t.Fatalf("app user hint from platform container wrong: %+v", app)
	}
	if !strings.Contains(app.Provenance, "ha-platform-hrpauth-1") || !strings.Contains(app.Provenance, "ha-platform") {
		t.Fatalf("provenance must name container and project: %s", app.Provenance)
	}
}

// Redis 同处理：无口令实例如实说明；有口令走同一套来源/验证纪律。
func TestHintCollectorRedisNoAuthAndManaged(t *testing.T) {
	in := hintTestInputs(t)

	// 无口令：验证器接受空口令 → 报"留空即可"。
	c := NewHintCollector(NewStore(t.TempDir()), nil, filepath.Join(t.TempDir(), "deploy/.env"))
	c.verifyRedisOverride = func(_ context.Context, pw string) bool { return pw == "" }
	c.verifyAdminOverride = func(context.Context, string) bool { return false }
	c.verifyAppOverride = func(context.Context, string) bool { return false }
	rep := c.Collect(context.Background(), in, "", "external", "")
	redis := rep.Hints[2]
	if redis.Found || redis.Value != "" {
		t.Fatalf("no-auth redis must not produce a value: %+v", redis)
	}
	if !strings.Contains(redis.Detail, "不要求口令") {
		t.Fatalf("no-auth redis must say so: %s", redis.Detail)
	}

	// 受管容器 env 的口令验证通过 → 回显。
	dk := &fakeHintDocker{envs: map[string]map[string]string{
		RedisContainerName: {"REDIS_PASSWORD": "redis-pw"},
	}}
	c2 := NewHintCollector(NewStore(t.TempDir()), dk, filepath.Join(t.TempDir(), "deploy/.env"))
	c2.verifyRedisOverride = func(_ context.Context, pw string) bool { return pw == "redis-pw" }
	c.verifyAdminOverride = func(context.Context, string) bool { return false }
	c.verifyAppOverride = func(context.Context, string) bool { return false }
	rep = c2.Collect(context.Background(), in, "", "container", "")
	redis = rep.Hints[2]
	if !redis.Found || redis.Source != SourceManagedContainerEnv || redis.Value != "redis-pw" {
		t.Fatalf("redis managed hint wrong: %+v", redis)
	}
}

// 来源优先级：①受管容器 env 先于 ②state/③历史（同值多源时以①报）。
func TestHintCollectorSourcePrecedence(t *testing.T) {
	store := NewStore(t.TempDir())
	st, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.Secrets[KeyMySQLRootPassword] = "same-pw"
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}
	dk := &fakeHintDocker{envs: map[string]map[string]string{
		MySQLContainerName: {"MYSQL_ROOT_PASSWORD": "same-pw"},
	}}
	c := NewHintCollector(store, dk, filepath.Join(t.TempDir(), "deploy/.env"))
	c.verifyAdminOverride = func(context.Context, string) bool { return true }
	rep := c.Collect(context.Background(), hintTestInputs(t), "container", "container", "")
	c.verifyAppOverride = func(context.Context, string) bool { return false }
	c.verifyRedisOverride = func(context.Context, string) bool { return false }

	if rep.Hints[0].Source != SourceManagedContainerEnv {
		t.Fatalf("managed container env must take precedence: %+v", rep.Hints[0])
	}
	if rep.Hints[0].CandidatesChecked != 1 {
		t.Fatalf("first verified candidate wins immediately; checked=%d", rep.Hints[0].CandidatesChecked)
	}
}
