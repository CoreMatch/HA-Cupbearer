package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helper：构造临时引擎（无 docker，状态根在 t.TempDir）。
func newTestEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "state"))
	eng := NewEngine(store, filepath.Join(dir, "templates"), nil)
	return eng, dir
}

func validInputs(t *testing.T, dir string) *Inputs {
	t.Helper()
	in := &Inputs{}
	in.PublicURLs.HRPAuth = "https://ha.example.com"
	in.PublicURLs.Frontend = "https://auth.example.com"
	in.PublicURLs.YGG = "https://ha.example.com/yggdrasil-api"
	in.PublicURLs.HASkinProxy = "https://skin.example.com"
	in.PublicURLs.WinnerProxy = "https://winner.example.com"
	in.PublicURLs.MaintenanceView = "https://mv.example.com"
	in.Database.AdminHost = "127.0.0.1"
	in.Database.AdminPort = 1 // 故意不可达，避免依赖真实 MySQL
	in.Database.AdminUser = "root"
	in.Database.AdminPassword = "irrelevant-for-these-tests"
	in.Paths.EnvFile = filepath.Join(dir, "deploy", ".env")
	in.Paths.SecretsDir = filepath.Join(dir, "deploy", "secrets")
	in.Targets.Container = true
	in.Targets.Baremetal = false
	return in
}

// --- 机密：幂等、收编、不覆盖 ---

func TestEnsureSecretsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	first, created, err := store.EnsureSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != len(secretKeys) {
		t.Fatalf("first run should create all %d secrets, created=%d", len(secretKeys), len(created))
	}
	second, created2, err := store.EnsureSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(created2) != 0 {
		t.Fatalf("second run must create nothing, created=%v", created2)
	}
	for _, k := range secretKeys {
		if first[k] != second[k] {
			t.Fatalf("secret %s changed across runs", k)
		}
		if first[k] == "" {
			t.Fatalf("secret %s empty", k)
		}
	}
}

func TestAdoptSecretsKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	// 预置一个"既有 .env 值"：收编后必须原样保留，绝不重新生成。
	existing := map[string]string{KeyManageToken: "pre-existing-value"}
	adopted, drifted, err := store.AdoptSecrets(existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 || adopted[0] != KeyManageToken {
		t.Fatalf("adopted=%v", adopted)
	}
	if len(drifted) != 0 {
		t.Fatalf("drifted=%v", drifted)
	}
	st, _ := store.LoadState()
	if st.Secrets[KeyManageToken] != "pre-existing-value" {
		t.Fatalf("adopted value not kept: %q", st.Secrets[KeyManageToken])
	}
	// 再次收编同一值：不应报漂移。
	_, drifted2, _ := store.AdoptSecrets(existing)
	if len(drifted2) != 0 {
		t.Fatalf("re-adopt identical value must not drift, got %v", drifted2)
	}
}

func TestAdoptSecretsDriftTargetWins(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	// 状态里先有一个值。
	if _, _, err := store.EnsureSecrets(); err != nil {
		t.Fatal(err)
	}
	before, _ := store.LoadState()
	old := before.Secrets[KeyManageToken]
	// 目标（.env）值不同：目标胜出，状态改写为目标值。
	_, drifted, err := store.AdoptSecrets(map[string]string{KeyManageToken: "target-live-value"})
	if err != nil {
		t.Fatal(err)
	}
	if len(drifted) != 1 {
		t.Fatalf("drifted=%v", drifted)
	}
	after, _ := store.LoadState()
	if after.Secrets[KeyManageToken] != "target-live-value" {
		t.Fatalf("target value must win, got %q", after.Secrets[KeyManageToken])
	}
	if after.Secrets[KeyManageToken] == old {
		t.Fatal("state must be rewritten to the live target value")
	}
}

// --- .env 合并：保留已有、补缺、幂等 ---

func TestMergeEnvFileKeepsAndFills(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	// 预置一个已有值（与期望不同 → 漂移，但绝不覆盖）。
	if err := os.WriteFile(envPath, []byte("MANAGE_TOKEN=keep-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	desired := map[string]string{
		"MANAGE_TOKEN": "generated-value",
		"DB_USER":      "hrpa",
	}
	added, kept, drifted, err := MergeEnvFile(envPath, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "DB_USER" {
		t.Fatalf("added=%v", added)
	}
	if len(kept) != 1 || kept[0] != "MANAGE_TOKEN" {
		t.Fatalf("kept=%v", kept)
	}
	if len(drifted) != 1 || drifted[0] != "MANAGE_TOKEN" {
		t.Fatalf("drifted=%v", drifted)
	}
	data, _ := os.ReadFile(envPath)
	if !strings.Contains(string(data), "MANAGE_TOKEN=keep-me") {
		t.Fatal("existing value must be preserved (never overwritten)")
	}
	if !strings.Contains(string(data), "DB_USER=hrpa") {
		t.Fatal("missing key must be appended")
	}

	// 第二次合并：无新增（幂等）。
	added2, _, _, err := MergeEnvFile(envPath, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(added2) != 0 {
		t.Fatalf("second merge must add nothing, added=%v", added2)
	}
}

// --- 模板渲染：替换、缺变量硬失败 ---

func TestRenderTemplate(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "x.yaml.tmpl")
	os.WriteFile(tmpl, []byte("a: ${FOO}\nb: ${BAR}\n"), 0o644)

	out, err := RenderTemplate(tmpl, map[string]string{"FOO": "1", "BAR": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "a: 1\nb: 2\n" {
		t.Fatalf("got %q", out)
	}

	// 缺变量 → 硬失败并点名。
	_, err = RenderTemplate(tmpl, map[string]string{"FOO": "1"})
	if err == nil || !strings.Contains(err.Error(), "BAR") {
		t.Fatalf("expected missing-var error naming BAR, got %v", err)
	}
}

func TestWriteIfAbsentNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte("original"), 0o644)
	written, err := WriteIfAbsent(path, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Fatal("must not overwrite existing file")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Fatalf("content changed: %q", data)
	}
	// 目标不存在 → 写入。
	path2 := filepath.Join(dir, "new.yaml")
	written2, err := WriteIfAbsent(path2, []byte("fresh"))
	if err != nil || !written2 {
		t.Fatalf("written=%v err=%v", written2, err)
	}
}

// --- 签名密钥：生成/复用/推导/错配拦截 ---

func TestEnsureSigningKeysLifecycle(t *testing.T) {
	dir := t.TempDir()
	res, err := EnsureSigningKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Generated {
		t.Fatal("first call must generate")
	}
	// 复用：两文件都在 → 原样保留。
	res2, err := EnsureSigningKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Reused {
		t.Fatal("second call must reuse")
	}
	if res2.Fingerprint != res.Fingerprint {
		t.Fatal("fingerprint must be stable")
	}
	// 缺公钥 → 由私钥推导补齐。
	os.Remove(filepath.Join(dir, "public_key.pem"))
	res3, err := EnsureSigningKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res3.PublicDerived {
		t.Fatal("missing public key must be derived from private")
	}
	// 只有公钥没有私钥 → 硬失败（绝不另造一对造成错配）。
	os.Remove(filepath.Join(dir, "private_key.pem"))
	if _, err := EnsureSigningKeys(dir); err == nil {
		t.Fatal("public-only must fail loud")
	}
}

// --- 输入校验：空提交当场拦截 ---

func TestValidateEmptyInputsBlocked(t *testing.T) {
	in := &Inputs{}
	in.fillDefaults()
	errs := in.Validate()
	if len(errs) == 0 {
		t.Fatal("empty submission must be blocked with field errors")
	}
	// 必须点名对外地址缺失。
	var fields []string
	for _, e := range errs {
		fields = append(fields, e.Field)
	}
	joined := strings.Join(fields, ",")
	for _, want := range []string{"public_urls.hrpauth", "public_urls.frontend"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected field error %s, got %v", want, fields)
		}
	}
}

func TestValidateRejectsBadDBName(t *testing.T) {
	in := validInputs(t, t.TempDir())
	in.Database.Names.HRPAuth = "bad-name;drop"
	in.fillDefaults()
	errs := in.Validate()
	found := false
	for _, e := range errs {
		if e.Field == "database.names.hrpauth" {
			found = true
		}
	}
	if !found {
		t.Fatal("invalid db name must be rejected")
	}
}

// --- dry-run 不落盘 ---

func TestPlanWritesNothing(t *testing.T) {
	eng, dir := newTestEngine(t)
	in := validInputs(t, dir)

	if _, err := eng.Plan(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	// dry-run 之后：.env 不存在、状态文件不存在、无任何机密落盘。
	if _, err := os.Stat(in.Paths.EnvFile); !os.IsNotExist(err) {
		t.Fatal("plan must not create .env")
	}
	st, _ := eng.Store().LoadState()
	if len(st.Secrets) != 0 {
		t.Fatal("plan must not persist secrets")
	}
}

// --- 运行：机密落盘 + 断点续跑（机密步复用） ---

func TestRunPersistsSecretsAndResumes(t *testing.T) {
	eng, dir := newTestEngine(t)
	in := validInputs(t, dir)
	// 无真实 MySQL：database 步会失败，但机密步必须先落盘——这正是
	// "中途失败可重试、从断点继续" 的被测面。

	run1, err := eng.StartRun(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, eng, run1.ID)
	r1, _ := eng.GetRun(run1.ID)
	if r1.Steps[StepSecrets].Status != StepOK {
		t.Fatalf("first run secrets step = %s (%s)", r1.Steps[StepSecrets].Status, r1.Steps[StepSecrets].Detail)
	}

	// 第二次：机密步必须复用（不重新生成）。
	run2, err := eng.StartRun(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, eng, run2.ID)
	r2, _ := eng.GetRun(run2.ID)
	if r2.Steps[StepSecrets].Status != StepSkipped {
		t.Fatalf("second run secrets step = %s, want skipped (resume)", r2.Steps[StepSecrets].Status)
	}
	// 两次运行的机密指纹必须一致。
	for k, v := range r1.Secrets {
		if r2.Secrets[k] != v {
			t.Fatalf("secret %s fingerprint changed across runs", k)
		}
	}
}

func waitRun(t *testing.T, eng *Engine, id string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		r, err := eng.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != "running" {
			return
		}
		// 轮询间隔短；用 os 睡眠避免引入 time 依赖外的复杂度。
		sleepMillis(25)
	}
	t.Fatal("run did not finish in time")
}

func sleepMillis(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
