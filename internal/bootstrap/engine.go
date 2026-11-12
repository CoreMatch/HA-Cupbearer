package bootstrap

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 步骤名（运行记录与计划共用，顺序即执行序）。
// infra 在最前：它产出的管理口凭据/连接事实是建库与配置渲染的前提。
const (
	StepInfra     = "infra"
	StepSecrets   = "secrets"
	StepDatabases = "databases"
	StepKeys      = "signing-keys"
	StepEnvFile   = "env-file"
	StepRender    = "render-configs"
)

var stepOrder = []string{StepInfra, StepSecrets, StepDatabases, StepKeys, StepEnvFile, StepRender}

// PlanItem 是计划里的一行（向导预览用）。机密值只以指纹出现。
type PlanItem struct {
	Step   string `json:"step"`
	Action string `json:"action"` // generate | reuse | write | merge | skip-exists | warn-drift | execute | blocker
	Target string `json:"target"`
	Detail string `json:"detail"`
}

// PlanReport 是一次 dry-run 的完整计划。
type PlanReport struct {
	Items   []PlanItem        `json:"items"`
	Secrets map[string]string `json:"secrets"` // 键名 -> 指纹（已存在的）或 "(will-generate)"
}

// Engine 是初始化层执行器。
type Engine struct {
	store       *Store
	templateDir string
	dk          InfraDocker // 基础设施容器操作面（nil = 不允许拉起容器）
	infraFile   string      // 基础设施 compose 文件（默认 deploy/infra.yml）

	mu      sync.Mutex
	current *Run
}

// NewEngine 以 store 为状态根、templateDir 为模板目录（默认 deploy/config）。
// dk 为 nil 时基础设施只接管不拉起。
func NewEngine(store *Store, templateDir string, dk InfraDocker) *Engine {
	if templateDir == "" {
		templateDir = DefaultTemplateDir
	}
	return &Engine{store: store, templateDir: templateDir, dk: dk, infraFile: DefaultInfraCompose}
}

// Store 暴露状态存取（状态端点用）。
func (e *Engine) Store() *Store { return e.store }

// EnsureMySQLOnly 一键确保 MySQL 可用（探测→接管→必要时拉起容器，等健康）。
// 供"一键拉起 MySQL"端点单独调用；派生的管理口事实写回 in。
func (e *Engine) EnsureMySQLOnly(ctx context.Context, in *Inputs) (*InfraInstance, error) {
	if e.dk == nil {
		return nil, fmt.Errorf("未装配容器运行时，无法一键拉起 MySQL（可自行提供既有实例连接）")
	}
	in.Prepare()
	return ensureMySQL(ctx, in, e.store, e.dk, e.infraFile)
}

// EnsureRedisOnly 一键确保 Redis 可用（探测→接管→必要时拉起容器，等健康）。
func (e *Engine) EnsureRedisOnly(ctx context.Context, in *Inputs) (*InfraInstance, error) {
	if e.dk == nil {
		return nil, fmt.Errorf("未装配容器运行时，无法一键拉起 Redis（可自行提供既有实例连接）")
	}
	in.Prepare()
	return ensureRedis(ctx, in, e.store, e.dk, e.infraFile)
}

// ProbeInfra 只读探测 MySQL/Redis 现状（不拉起、不落盘），供状态端点展示
// "这实例被哪条链路用、两种连接地址各是什么"。
func (e *Engine) ProbeInfra(ctx context.Context, in *Inputs) *InfraFacts {
	facts := &InfraFacts{}
	facts.MySQL = probeInfraOne(ctx, "mysql", in.Infra.MySQLHostPort, e.dk)
	facts.Redis = probeInfraOne(ctx, "redis", in.Infra.RedisHostPort, e.dk)
	return facts
}

// CredentialHints 计算凭据预填提示（契约 1.7.0；只读，唯一写面是审计日志）：
// 对 MySQL 管理口 / 应用账号 / Redis 三个目标，按三类可追溯来源收集候选，
// 逐个对目标验证，仅第一个验证通过者回显明文并记审计。外部口令绝不回显。
// remoteAddr 是调用方地址（审计"谁取用"）。
func (e *Engine) CredentialHints(ctx context.Context, in *Inputs, remoteAddr string) (*CredentialHintsReport, error) {
	in.fillDefaults()
	var hd HintDocker
	if e.dk != nil {
		hd, _ = e.dk.(HintDocker)
	}
	collector := NewHintCollector(e.store, hd, in.Paths.EnvFile)
	// 基础设施现状（只读探测）只用于无预填值时的情境化文案。
	mysqlMode := probeInfraOne(ctx, "mysql", in.Infra.MySQLHostPort, e.dk).Mode
	redisMode := probeInfraOne(ctx, "redis", in.Infra.RedisHostPort, e.dk).Mode
	return collector.Collect(ctx, in, mysqlMode, redisMode, remoteAddr), nil
}

func probeInfraOne(ctx context.Context, kind string, hostPort int, dk InfraDocker) InfraInstance {
	container := MySQLContainerName
	composeAddr := MySQLComposeAddress
	volume := "ha-infra-mysql-data"
	containerPort := mysqlContainerPort
	probe := func() bool { return probeMySQL(ctx, "127.0.0.1", hostPort) }
	if kind == "redis" {
		container = RedisContainerName
		composeAddr = RedisComposeAddress
		volume = "ha-infra-redis-data"
		containerPort = redisContainerPort
		probe = func() bool { return probeRedis(ctx, "127.0.0.1", hostPort) }
	}
	inst := InfraInstance{
		Kind:             kind,
		ComposeService:   kind,
		ContainerName:    container,
		HostPort:         hostPort,
		ContainerPort:    containerPort,
		BaremetalAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)),
		ComposeAddress:   composeAddr,
		DataVolume:       volume,
	}
	if dk != nil {
		if st, err := dk.Inspect(ctx, container); err == nil && st != nil {
			inst.Mode = "container"
			inst.Image = st.Image
			inst.Healthy = st.Running
			inst.Detail = "受管容器（状态 " + st.State + "）"
			return inst
		}
	}
	if probe() {
		inst.Mode = "external"
		inst.Healthy = true
		inst.ContainerName = ""
		inst.Detail = "既有实例（宿主安装或外部容器），接管复用"
		return inst
	}
	inst.Mode = "absent"
	inst.Detail = "未检测到可用实例"
	return inst
}

// Plan 计算完整计划（只读：查状态、查盘上文件、查数据库存在性），不落盘。
func (e *Engine) Plan(ctx context.Context, in *Inputs) (*PlanReport, error) {
	rep := &PlanReport{Secrets: map[string]string{}}

	// 1. 机密：既有目标值优先收编（预览不写状态，只报将发生的收编/复用/生成）。
	st, err := e.store.LoadState()
	if err != nil {
		return nil, err
	}
	envVals := map[string]string{}
	if in.Targets.Container {
		envAbs, _ := filepath.Abs(in.Paths.EnvFile)
		envVals = ReadEnvFileValues(envAbs)
	}
	projected := map[string]string{} // 预览用的推定值（占位符代表将生成）
	for _, key := range secretKeys {
		stateVal := st.Secrets[key]
		envVal := envVals[key]
		switch {
		case envVal != "" && stateVal == "":
			projected[key] = envVal
			rep.Secrets[key] = Mask(envVal)
			rep.Items = append(rep.Items, PlanItem{Step: StepSecrets, Action: "reuse",
				Target: key, Detail: "收编 .env 中已有的值（不生成新值；" + Mask(envVal) + "）"})
		case envVal != "" && stateVal != envVal:
			projected[key] = envVal
			rep.Secrets[key] = Mask(envVal)
			rep.Items = append(rep.Items, PlanItem{Step: StepSecrets, Action: "warn-drift",
				Target: key, Detail: ".env 已有值与状态不同：已有值胜出（状态将改写为已有值，.env 本身绝不改动）"})
		case stateVal != "":
			projected[key] = stateVal
			rep.Secrets[key] = Mask(stateVal)
			rep.Items = append(rep.Items, PlanItem{Step: StepSecrets, Action: "reuse",
				Target: key, Detail: "状态中已存在，原样复用（" + Mask(stateVal) + "）"})
		default:
			projected[key] = ""
			rep.Secrets[key] = "(will-generate)"
			rep.Items = append(rep.Items, PlanItem{Step: StepSecrets, Action: "generate",
				Target: key, Detail: "将生成 64 位十六进制随机值并写入本地状态（0600，不进仓库）"})
		}
	}

	// 1.5 基础设施：探测现状，报"接管复用 / 将拉起 / 端口冲突 / 需补口令"。
	rep.Items = append(rep.Items, e.planInfra(ctx, in)...)

	// 2. 数据库：先探测后报——应用账号已存在且口令不一致时，把冲突与所选
	// 处置的结局报出来；整步将被 skip 或冲突未决时不再报逐库细节（避免
	// "写着将创建、实际不会跑"的误导）。管理口令留空且允许拉起基础设施时，
	// 库检查推迟到 infra 之后（口令为生成值）。
	var dbRes dbResolution
	if in.Database.AdminPassword == "" && e.dk != nil && in.Infra.InfraLaunch() {
		rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "execute",
			Target: "(基础设施拉起后)", Detail: "管理口令将为生成值；建库检查在 MySQL 就绪后执行"})
	} else if db, derr := openAdmin(ctx, in); derr != nil {
		rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "blocker",
			Target: fmt.Sprintf("%s:%d", in.Database.AdminHost, in.Database.AdminPort),
			Detail: "MySQL 管理口不可达：" + derr.Error()})
	} else {
		suppress := false
		if in.Database.ManageUser != nil && *in.Database.ManageUser {
			suppress, dbRes = e.planAppUser(rep, ctx, in, db, st)
		}
		if !suppress {
			for _, name := range []string{in.Database.Names.HRPAuth, in.Database.Names.YGG,
				in.Database.Names.HASkinLib, in.Database.Names.MCA} {
				exists, err := databaseExists(ctx, db, name)
				if err != nil {
					rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "blocker",
						Target: name, Detail: err.Error()})
					continue
				}
				if exists {
					rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "skip-exists",
						Target: name, Detail: "库已存在，原样保留（不改动任何数据）"})
				} else {
					rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "execute",
						Target: name, Detail: "将执行 CREATE DATABASE IF NOT EXISTS（utf8mb4）"})
				}
			}
		}
		db.Close()
	}

	// 3. 签名密钥：按盘上现状报。
	secAbs, _ := filepath.Abs(in.Paths.SecretsDir)
	keyPlan, kerr := planKeys(secAbs)
	if kerr != nil {
		rep.Items = append(rep.Items, PlanItem{Step: StepKeys, Action: "blocker",
			Target: secAbs, Detail: kerr.Error()})
	} else {
		rep.Items = append(rep.Items, keyPlan...)
	}

	// 4. 容器 .env：报将合并/保留/漂移的键（机密键只报键名）。
	// DB_PASSWORD 随 databases 结论预览：skip 不写入；接管/沿用将就地更新
	// 陈旧值（"已有值绝不覆盖"铁律的唯一例外，见 MergeEnvFile）。
	if in.Targets.Container {
		envAbs, _ := filepath.Abs(in.Paths.EnvFile)
		existingVals := ReadEnvFileValues(envAbs)
		desired := EnvForContainer(in, projected, nil)
		if dbRes == dbResSkip {
			delete(desired, KeyAppDBPassword)
			if _, ok := existingVals[KeyAppDBPassword]; !ok {
				rep.Items = append(rep.Items, PlanItem{Step: StepEnvFile, Action: "skip-exists",
					Target: KeyAppDBPassword,
					Detail: "skip 处置：凭据未验证，DB_PASSWORD 不写入 .env（容器栈请自行提供）"})
			}
		}
		resolvedCred := ""
		switch dbRes {
		case dbResTakeover:
			resolvedCred = projected[KeyAppDBPassword]
		case dbResReuse:
			resolvedCred = in.Database.ExistingPassword
		}
		var add, keep, drift []string
		for k, v := range desired {
			if cur, ok := existingVals[k]; ok {
				if k == KeyAppDBPassword && (dbRes == dbResTakeover || dbRes == dbResReuse) {
					continue // 将按验证凭据就地更新，不计保留/漂移
				}
				keep = append(keep, k)
				if cur != v && cur != "" {
					drift = append(drift, k)
				}
			} else {
				add = append(add, k)
			}
		}
		rep.Items = append(rep.Items, PlanItem{Step: StepEnvFile, Action: "merge",
			Target: envAbs,
			Detail: fmt.Sprintf("将补齐 %d 个缺失变量，保留 %d 个已有值（已有值绝不覆盖）",
				len(add), len(keep))})
		if dbRes == dbResTakeover || dbRes == dbResReuse {
			if cur, ok := existingVals[KeyAppDBPassword]; ok {
				if resolvedCred != "" && cur == resolvedCred {
					rep.Items = append(rep.Items, PlanItem{Step: StepEnvFile, Action: "skip-exists",
						Target: KeyAppDBPassword, Detail: "已有值与将验证的凭据一致，原样保留"})
				} else {
					label := "（凭据将生成）"
					if resolvedCred != "" {
						label = "（" + Mask(resolvedCred) + "）"
					}
					rep.Items = append(rep.Items, PlanItem{Step: StepEnvFile, Action: "execute",
						Target: KeyAppDBPassword,
						Detail: "已有值与将验证的凭据不一致（陈旧值）：将就地更新 " + label})
				}
			}
		}
		if len(drift) > 0 {
			rep.Items = append(rep.Items, PlanItem{Step: StepEnvFile, Action: "warn-drift",
				Target: envAbs,
				Detail: "以下变量已有值且与推导值不同：保留现有值，仅告警——" +
					strings.Join(uniqueStrings(drift), ", ")})
		}
	}

	// 5. 裸金属渲染：逐服务报"将渲染/已存在跳过"。
	if in.Targets.Baremetal {
		for _, svc := range sortedOutputServices(in) {
			out := in.Paths.BaremetalOutputs[svc]
			tmpl := templateFileFor(svc)
			if tmpl == "" {
				rep.Items = append(rep.Items, PlanItem{Step: StepRender, Action: "blocker",
					Target: svc, Detail: "没有对应的配置模板，无法渲染"})
				continue
			}
			if fileExists(out) {
				rep.Items = append(rep.Items, PlanItem{Step: StepRender, Action: "skip-exists",
					Target: out, Detail: "目标配置已存在，绝不覆盖（如需重渲染请先人工移走该文件）"})
			} else {
				rep.Items = append(rep.Items, PlanItem{Step: StepRender, Action: "write",
					Target: out, Detail: "将由 " + tmpl + " 渲染（值来自统一输入与生成机密）"})
			}
		}
		if len(in.Paths.BaremetalOutputs) == 0 {
			rep.Items = append(rep.Items, PlanItem{Step: StepRender, Action: "skip-exists",
				Target: "(未指定输出)", Detail: "未给出任何裸金属渲染输出路径，本步骤跳过"})
		}
	}

	return rep, nil
}

// StartRun 异步执行一次初始化（单飞）。返回运行快照，进度经
// CurrentRun/GetRun 查询；每一步落盘，重跑即续跑。
func (e *Engine) StartRun(ctx context.Context, in *Inputs) (*Run, error) {
	e.mu.Lock()
	if e.current != nil && e.current.State == "running" {
		e.mu.Unlock()
		return nil, ErrRunInProgress
	}
	run := &Run{
		ID:        e.store.NextRunID(),
		State:     "running",
		Steps:     map[string]StepRecord{},
		StepOrder: append([]string(nil), stepOrder...),
		Secrets:   map[string]string{},
		StartedAt: time.Now(),
	}
	e.current = run
	e.mu.Unlock()
	_ = e.store.SaveRun(run)

	snap := snapshotRun(run)
	go e.execute(context.Background(), run, in)
	return snap, nil
}

// execute 逐步执行；每步完成即落盘（中断后续跑的依据）。
// run 的全部字段写入都在 e.mu 下进行（与 CurrentRun 的快照读互斥）。
func (e *Engine) execute(ctx context.Context, run *Run, in *Inputs) {
	mutate := func(fn func()) {
		e.mu.Lock()
		fn()
		_ = e.store.SaveRun(run)
		e.mu.Unlock()
	}
	setStep := func(name string, status StepStatus, detail string) {
		mutate(func() {
			run.Steps[name] = StepRecord{Name: name, Status: status, Detail: detail, UpdatedAt: time.Now()}
		})
	}
	addNote := func(note string) {
		mutate(func() { run.Notes = append(run.Notes, note) })
	}

	// 0. 基础设施：探测/接管/一键拉起 MySQL 与 Redis（按所选路径各自成立）。
	// 派生的管理口凭据与连接事实写回 in，供后续建库与配置渲染消费。
	var facts *InfraFacts
	if e.dk != nil {
		f, ierr := EnsureInfra(ctx, in, e.store, e.dk, e.infraFile)
		if ierr != nil {
			setStep(StepInfra, StepFailed, ierr.Error())
			e.finish(run)
			return
		}
		facts = f
		setStep(StepInfra, StepOK, infraStepDetail(f, in.Targets))
	} else {
		setStep(StepInfra, StepSkipped, "未装配容器运行时，跳过基础设施拉起（使用输入给定的既有 MySQL/Redis）")
	}

	// 1. 机密：先收编既有 .env 中的活值（已存在的凭据一律保留），
	// 再幂等取或生成缺失值。
	if in.Targets.Container {
		envAbs, _ := filepath.Abs(in.Paths.EnvFile)
		adopted, drifted, aerr := e.store.AdoptSecrets(ReadEnvFileValues(envAbs))
		if aerr != nil {
			setStep(StepSecrets, StepFailed, aerr.Error())
			e.finish(run)
			return
		}
		if len(adopted) > 0 {
			addNote("收编 .env 已有机密值：" + strings.Join(adopted, ", "))
		}
		if len(drifted) > 0 {
			addNote("机密漂移（.env 已有值胜出，状态已对齐；.env 未改动）：" +
				strings.Join(drifted, ", "))
		}
	}
	secrets, created, err := e.store.EnsureSecrets()
	if err != nil {
		setStep(StepSecrets, StepFailed, err.Error())
		e.finish(run)
		return
	}
	mutate(func() {
		for k, v := range secrets {
			run.Secrets[k] = Mask(v)
		}
	})
	if len(created) == 0 {
		setStep(StepSecrets, StepSkipped, "全部机密已在状态中，原样复用（不重新生成）")
	} else {
		setStep(StepSecrets, StepOK, fmt.Sprintf("新生成 %d 项：%s（其余复用；值仅以指纹记录）",
			len(created), strings.Join(created, ", ")))
	}

	// 2. 数据库：先探测后动手。应用账号已存在且口令与本次凭据不一致时，
	// 按所选处置显式处理（takeover/reuse/skip）；冲突而未给处置 = 显式失败。
	// 任何处置都不允许半途改一半：改口令/验证既有口令都在建库之前完成，
	// 失败即整体退出（重跑探测到的仍是同一冲突，收敛不悬挂）。
	manageUser := in.Database.ManageUser == nil || *in.Database.ManageUser
	appPassword := secrets[KeyAppDBPassword]
	action := DBActionProceed
	prefix := ""
	dbCredsVerified := false // 应用账号逐库验证通过（.env 覆写陈旧口令的依据）
	if manageUser {
		var probeNote string
		action, probeNote, err = probeDBStep(ctx, in, appPassword)
		if err != nil {
			setStep(StepDatabases, StepFailed, err.Error())
			e.finish(run)
			return
		}
		addNote(probeNote)
	}
	switch action {
	case DBActionSkip:
		setStep(StepDatabases, StepSkipped, fmt.Sprintf(
			"应用账号 %s 已存在且口令与本次凭据不一致：按 skip 处置保留现状——不建库、不改账号、不验证连通，只补其余步骤。如是误选，请改选 takeover 或 reuse 重跑",
			in.Database.AppUser))
	case DBActionConflict:
		setStep(StepDatabases, StepFailed, fmt.Sprintf(
			"应用账号 %s 已存在且口令与本次凭据不一致（本次候选凭据指纹 %s）：必须选择处置后重跑——takeover（改口令到本次凭据）/ reuse（沿用既有口令，需提供当前口令）/ skip（保留现状只补其余步骤）。本次运行未改动任何数据库对象",
			in.Database.AppUser, Mask(appPassword)))
		e.finish(run)
		return
	case DBActionTakeover:
		if aerr := AlterAppPassword(ctx, in, appPassword); aerr != nil {
			setStep(StepDatabases, StepFailed, "takeover 改口令失败："+aerr.Error())
			e.finish(run)
			return
		}
		addNote(fmt.Sprintf("takeover：既有账号 %s 的口令已改为本次凭据（指纹 %s）",
			in.Database.AppUser, Mask(appPassword)))
		prefix = "takeover：既有账号口令已改为本次凭据。"
	case DBActionReuse:
		ok, cerr := CheckAppPassword(ctx, in, in.Database.ExistingPassword)
		if cerr != nil {
			setStep(StepDatabases, StepFailed, "验证 existing_password 失败："+cerr.Error())
			e.finish(run)
			return
		}
		if !ok {
			setStep(StepDatabases, StepFailed,
				"policy=reuse 但提供的 existing_password 与既有账号不匹配：本次运行未改动任何数据库对象，请核对口令后重跑")
			e.finish(run)
			return
		}
		adopted, drifted, aerr := e.store.AdoptSecrets(map[string]string{KeyAppDBPassword: in.Database.ExistingPassword})
		if aerr != nil {
			setStep(StepDatabases, StepFailed, "收编既有口令进状态失败："+aerr.Error())
			e.finish(run)
			return
		}
		appPassword = in.Database.ExistingPassword
		secrets[KeyAppDBPassword] = appPassword // 后续 .env/渲染步沿用活值
		mutate(func() { run.Secrets[KeyAppDBPassword] = Mask(appPassword) })
		if len(adopted) > 0 {
			addNote("reuse：既有账号口令已收编进状态（DB_PASSWORD）")
		}
		if len(drifted) > 0 {
			addNote("reuse：既有账号口令与状态原值不同，活值胜出、状态已改写（指纹 " + Mask(appPassword) + "）")
		}
		prefix = "reuse：沿用既有账号口令（已收编进状态）。"
	case DBActionProceed:
		// 无冲突：照常创建或幂等无操作。
	}

	if action != DBActionSkip {
		dbResults, err := EnsureDatabases(ctx, in)
		if err != nil {
			setStep(StepDatabases, StepFailed, err.Error())
			e.finish(run)
			return
		}
		var createdDBs, keptDBs []string
		for _, r := range dbResults {
			if r.Created {
				createdDBs = append(createdDBs, r.Name)
			} else {
				keptDBs = append(keptDBs, r.Name)
			}
		}
		userDetail := ""
		if manageUser {
			userCreated, uerr := EnsureAppUser(ctx, in, appPassword)
			if uerr != nil {
				setStep(StepDatabases, StepFailed, uerr.Error())
				e.finish(run)
				return
			}
			if userCreated {
				userDetail = fmt.Sprintf("；创建应用账号 %s 并授权", in.Database.AppUser)
			} else {
				userDetail = fmt.Sprintf("；应用账号 %s 已存在，仅补齐授权", in.Database.AppUser)
			}
			if verr := VerifyAppAccess(ctx, in, appPassword); verr != nil {
				setStep(StepDatabases, StepFailed, "建库完成但应用账号连通性验证失败："+verr.Error())
				e.finish(run)
				return
			}
			dbCredsVerified = true
			userDetail += "；应用账号逐库连通性验证通过"
		} else {
			userDetail = "；manage_user=false：跳过账号创建（既有账号由运维保证口令一致）"
		}
		status := StepOK
		detail := fmt.Sprintf("建库 %d 个（%s），已存在保留 %d 个（%s）%s",
			len(createdDBs), strings.Join(createdDBs, ", "),
			len(keptDBs), strings.Join(keptDBs, ", "), userDetail)
		if len(createdDBs) == 0 {
			status = StepSkipped
			detail = fmt.Sprintf("四个库均已存在，原样保留（%s）%s", strings.Join(keptDBs, ", "), userDetail)
		}
		setStep(StepDatabases, status, prefix+detail)
	}

	// 3. 签名密钥。
	secAbs, _ := filepath.Abs(in.Paths.SecretsDir)
	keyRes, kerr := EnsureSigningKeys(secAbs)
	if kerr != nil {
		setStep(StepKeys, StepFailed, kerr.Error())
		e.finish(run)
		return
	}
	if err := e.store.SetSigningKeyFingerprint(keyRes.Fingerprint); err != nil {
		addNote("密钥指纹写入状态失败：" + err.Error())
	}
	switch {
	case keyRes.Generated:
		setStep(StepKeys, StepOK, fmt.Sprintf("生成 RSA-4096 密钥对：%s（私钥 0600，指纹 %s）",
			secAbs, keyRes.Fingerprint))
	case keyRes.PublicDerived:
		setStep(StepKeys, StepWarn, fmt.Sprintf("私钥已在、公钥缺失：由私钥推导补齐 %s（指纹 %s）",
			filepath.Join(secAbs, "public_key.pem"), keyRes.Fingerprint))
	default:
		setStep(StepKeys, StepSkipped, fmt.Sprintf("密钥对已存在，原样保留（指纹 %s）", keyRes.Fingerprint))
	}

	// 4. 容器 .env。DB_PASSWORD 随 databases 步的结局收敛：
	//   - skip：凭据未验证，绝不写入（写未验证凭据会污染后续运行）；
	//   - 验证通过（照常/接管/沿用）：既有值若不同则可证明是陈旧值
	//    （服务用它连不上真实账号），就地更新为验证过的凭据并如实报告。
	if in.Targets.Container {
		envAbs, _ := filepath.Abs(in.Paths.EnvFile)
		desired := EnvForContainer(in, secrets, facts)
		overwrite := map[string]bool{}
		if manageUser && action == DBActionSkip {
			delete(desired, KeyAppDBPassword)
			addNote("skip：DB_PASSWORD 未写入 .env（凭据未验证；容器栈请自行提供）")
		} else if dbCredsVerified {
			overwrite[KeyAppDBPassword] = true
		}
		added, kept, drifted, overwritten, merr := MergeEnvFile(envAbs, desired, overwrite)
		if merr != nil {
			setStep(StepEnvFile, StepFailed, merr.Error())
			e.finish(run)
			return
		}
		detail := fmt.Sprintf("%s：补齐 %d 个变量，保留 %d 个已有值", envAbs, len(added), len(kept))
		status := StepOK
		if len(added) == 0 && len(overwritten) == 0 {
			status = StepSkipped
			detail = fmt.Sprintf("%s：变量已齐，无需改动", envAbs)
		}
		if len(overwritten) > 0 {
			status = StepWarn
			detail += "；更新陈旧值：" + strings.Join(overwritten, ", ") +
				"（.env 原值与真实账号口令不一致，已更新为本次验证通过的凭据）"
		}
		if len(drifted) > 0 {
			status = StepWarn
			detail += "；漂移（保留现有值，仅告警）：" + strings.Join(uniqueStrings(drifted), ", ")
		}
		setStep(StepEnvFile, status, detail)
	} else {
		setStep(StepEnvFile, StepSkipped, "未选择容器路径，跳过 .env 合并")
	}

	// 5. 裸金属渲染。
	if in.Targets.Baremetal {
		var written, skipped []string
		var failErr error
		for _, svc := range sortedOutputServices(in) {
			out := in.Paths.BaremetalOutputs[svc]
			tmpl := templateFileFor(svc)
			if tmpl == "" {
				failErr = fmt.Errorf("服务 %s 没有对应配置模板", svc)
				break
			}
			vars := EnvForBaremetal(in, secrets, svc, secAbs, facts)
			content, rerr := RenderTemplate(filepath.Join(e.templateDir, tmpl), vars)
			if rerr != nil {
				failErr = rerr
				break
			}
			didWrite, werr := WriteIfAbsent(out, content)
			if werr != nil {
				failErr = werr
				break
			}
			if didWrite {
				written = append(written, svc+" -> "+out)
			} else {
				skipped = append(skipped, svc+" -> "+out)
			}
		}
		if failErr != nil {
			setStep(StepRender, StepFailed, failErr.Error())
			e.finish(run)
			return
		}
		status, detail := StepOK, ""
		if len(written) > 0 {
			detail += "渲染 " + strings.Join(written, "；")
		}
		if len(skipped) > 0 {
			if detail != "" {
				detail += "。"
			}
			detail += "已存在跳过（绝不覆盖）：" + strings.Join(skipped, "；")
			status = StepSkipped
			if len(written) > 0 {
				status = StepWarn
			}
		}
		if len(written) == 0 && len(skipped) == 0 {
			status = StepSkipped
			detail = "未给出任何裸金属渲染输出路径，本步骤跳过"
		}
		setStep(StepRender, status, detail)
		if (action == DBActionTakeover || action == DBActionReuse) && len(skipped) > 0 {
			addNote("注意：takeover/reuse 改变了生效的 DB 凭据，以下已存在的配置文件保留原值（绝不覆盖），请人工核对其中 DB 口令：" +
				strings.Join(skipped, "；"))
		}
	} else {
		setStep(StepRender, StepSkipped, "未选择裸金属路径，跳过配置渲染")
	}

	e.finish(run)
}

func (e *Engine) finish(run *Run) {
	now := time.Now()
	e.mu.Lock()
	run.FinishedAt = &now
	run.State = "done"
	for _, step := range run.Steps {
		if step.Status == StepFailed {
			run.State = "failed"
			break
		}
	}
	_ = e.store.SaveRun(run)
	if e.current == run {
		e.current = run
	}
	e.mu.Unlock()
}

// CurrentRun / GetRun / ListRuns 暴露运行记录。
func (e *Engine) CurrentRun() *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	return snapshotRun(e.current)
}

func (e *Engine) GetRun(id string) (*Run, error) {
	if cur := e.CurrentRun(); cur != nil && cur.ID == id {
		return cur, nil
	}
	return e.store.LoadRun(id)
}

func (e *Engine) ListRuns() ([]RunSummary, error) { return e.store.ListRuns() }

func snapshotRun(run *Run) *Run {
	if run == nil {
		return nil
	}
	clone := *run
	clone.Steps = make(map[string]StepRecord, len(run.Steps))
	for k, v := range run.Steps {
		clone.Steps[k] = v
	}
	clone.StepOrder = append([]string(nil), run.StepOrder...)
	clone.Secrets = make(map[string]string, len(run.Secrets))
	for k, v := range run.Secrets {
		clone.Secrets[k] = v
	}
	clone.Notes = append([]string(nil), run.Notes...)
	return &clone
}

// infraStepDetail 按所选部署路径报告连接形态（各路径各自成立，不做
// "跨路径共用"表述）：选裸金属报宿主地址，选容器报 compose 服务名，
// 两者都选则分别列出。
func infraStepDetail(f *InfraFacts, tg Targets) string {
	addr := func(inst InfraInstance) string {
		switch {
		case tg.Baremetal && tg.Container:
			return fmt.Sprintf("裸金属编排连 %s；容器编排连 %s", inst.BaremetalAddress, inst.ComposeAddress)
		case tg.Container:
			return fmt.Sprintf("容器编排连 %s（宿主发布口 %s 供本机工具）", inst.ComposeAddress, inst.BaremetalAddress)
		default:
			return fmt.Sprintf("裸金属编排连 %s", inst.BaremetalAddress)
		}
	}
	return fmt.Sprintf("MySQL：%s（%s，%s）。Redis：%s（%s，%s）",
		f.MySQL.Mode, f.MySQL.Detail, addr(f.MySQL),
		f.Redis.Mode, f.Redis.Detail, addr(f.Redis))
}

// --- 辅助 ---

// planInfra 探测 MySQL/Redis 现状并给出预览结论（不落盘、不起容器）。
func (e *Engine) planInfra(ctx context.Context, in *Inputs) []PlanItem {
	var items []PlanItem
	if e.dk == nil {
		items = append(items, PlanItem{Step: StepInfra, Action: "skip-exists",
			Target: "mysql/redis", Detail: "未装配容器运行时：仅使用输入给定的既有实例"})
		return items
	}
	items = append(items, planInfraOne(ctx, "mysql", in.Infra.MySQLHostPort, e.dk, in.Database.AdminPassword != "", in.Infra.InfraLaunch()))
	items = append(items, planInfraOne(ctx, "redis", in.Infra.RedisHostPort, e.dk, true, in.Infra.InfraLaunch()))
	return items
}

func planInfraOne(ctx context.Context, kind string, hostPort int, dk InfraDocker, haveCreds, allowLaunch bool) PlanItem {
	container := MySQLContainerName
	probe := func() bool { return probeMySQL(ctx, "127.0.0.1", hostPort) }
	if kind == "redis" {
		container = RedisContainerName
		probe = func() bool { return probeRedis(ctx, "127.0.0.1", hostPort) }
	}
	// 受管容器已在 → 复用。
	if st, err := dk.Inspect(ctx, container); err == nil && st != nil {
		return PlanItem{Step: StepInfra, Action: "reuse", Target: kind,
			Detail: fmt.Sprintf("受管容器 %s 已在（%s），将确保其在跑并等待健康，不新建", container, st.State)}
	}
	if probe() {
		if haveCreds || kind == "redis" {
			return PlanItem{Step: StepInfra, Action: "reuse", Target: kind,
				Detail: fmt.Sprintf("127.0.0.1:%d 已有可用 %s，将接管复用（不新建容器）", hostPort, strings.ToUpper(kind))}
		}
		return PlanItem{Step: StepInfra, Action: "blocker", Target: kind,
			Detail: fmt.Sprintf("127.0.0.1:%d 已有可用 %s，但缺管理口令：请在向导填写后接管，或换空闲端口改为一键拉起", hostPort, strings.ToUpper(kind))}
	}
	if !portFree(hostPort) {
		return PlanItem{Step: StepInfra, Action: "blocker", Target: kind,
			Detail: fmt.Sprintf("端口 %d 被非 %s 进程占用：请换端口或先停掉占用者", hostPort, strings.ToUpper(kind))}
	}
	if !allowLaunch {
		return PlanItem{Step: StepInfra, Action: "blocker", Target: kind,
			Detail: fmt.Sprintf("无可用 %s 且 launch=false：不允许拉起容器", strings.ToUpper(kind))}
	}
	return PlanItem{Step: StepInfra, Action: "generate", Target: kind,
		Detail: fmt.Sprintf("将一键拉起受管 %s 容器（命名卷持久化，口令生成，仅以指纹记录），并等待其真正可用", strings.ToUpper(kind))}
}

// DBAction 是 databases 步经探测决定的动作。
type DBAction string

const (
	DBActionProceed  DBAction = "proceed"  // 无冲突：照常创建或幂等无操作
	DBActionConflict DBAction = "conflict" // 账号已存在且口令不一致、未给处置：显式失败
	DBActionTakeover DBAction = "takeover" // 冲突 + takeover：改口令到本次凭据
	DBActionReuse    DBAction = "reuse"    // 冲突 + reuse：验证并沿用既有口令
	DBActionSkip     DBAction = "skip"     // 冲突 + skip：整步保留现状
)

// decideDBAction 由纯事实推导动作（可单测，不依赖 MySQL）：处置只在
// "账号已存在且候选口令验证不过"时生效；否则照常（创建或幂等无操作）。
func decideDBAction(userExists, candidateMatches bool, policy string) DBAction {
	if !userExists || candidateMatches {
		return DBActionProceed
	}
	switch policy {
	case PolicyTakeover:
		return DBActionTakeover
	case PolicyReuse:
		return DBActionReuse
	case PolicySkip:
		return DBActionSkip
	default:
		return DBActionConflict
	}
}

// probeDBStep 探测 databases 步现状（管理口可达性、应用账号存在性、候选
// 口令匹配性），返回动作与人话探测结论。探测本身只读，不改任何对象。
func probeDBStep(ctx context.Context, in *Inputs, candidatePassword string) (DBAction, string, error) {
	db, err := openAdmin(ctx, in)
	if err != nil {
		return "", "", fmt.Errorf("MySQL 管理口不可达（%s:%d，账号 %s）：%v",
			in.Database.AdminHost, in.Database.AdminPort, in.Database.AdminUser, err)
	}
	defer db.Close()
	exists, err := userExists(ctx, db, in.Database.AppUser)
	if err != nil {
		return "", "", fmt.Errorf("探测应用账号 %s 是否存在失败：%w", in.Database.AppUser, err)
	}
	if !exists {
		note := fmt.Sprintf("探测：应用账号 %s 不存在，将创建并授权", in.Database.AppUser)
		if in.Database.Policy != "" {
			note += fmt.Sprintf("（处置 %s 在无冲突时不生效）", in.Database.Policy)
		}
		return DBActionProceed, note, nil
	}
	ok, err := CheckAppPassword(ctx, in, candidatePassword)
	if err != nil {
		return "", "", fmt.Errorf("探测应用账号 %s 口令匹配性失败：%w", in.Database.AppUser, err)
	}
	if ok {
		note := fmt.Sprintf("探测：应用账号 %s 已存在且本次凭据验证通过（幂等无操作）", in.Database.AppUser)
		if in.Database.Policy != "" {
			note += fmt.Sprintf("（处置 %s 在无冲突时不生效）", in.Database.Policy)
		}
		return DBActionProceed, note, nil
	}
	action := decideDBAction(true, false, in.Database.Policy)
	return action, fmt.Sprintf("探测：应用账号 %s 已存在且口令与本次凭据不一致（冲突；处置：%s）",
		in.Database.AppUser, policyLabel(in.Database.Policy)), nil
}

func policyLabel(policy string) string {
	if policy == "" {
		return "未选择——将显式失败"
	}
	return policy
}

// dbResolution 是计划对 databases 步的结论（驱动 .env 预览：skip 时
// DB_PASSWORD 不落盘，接管/沿用时预告陈旧值更新）。
type dbResolution string

const (
	dbResUnknown  dbResolution = ""         // 管理口不可达/推迟/不管账号：无法断言
	dbResProceed  dbResolution = "proceed"  // 无冲突：凭据将被验证
	dbResTakeover dbResolution = "takeover" // 冲突已决：口令将改为本次凭据
	dbResReuse    dbResolution = "reuse"    // 冲突已决：将沿用既有口令
	dbResSkip     dbResolution = "skip"     // 冲突 + skip：整步保留现状
	dbResBlocked  dbResolution = "blocked"  // 冲突未决：运行会被拒绝
)

// planAppUser 在计划中报应用账号的探测结论与处置结局。返回 (suppress,
// resolution)：suppress=true 表示 databases 步整体将被跳过或冲突未决
// （此时不再报逐库细节）。
func (e *Engine) planAppUser(rep *PlanReport, ctx context.Context, in *Inputs, db *sql.DB, st *State) (bool, dbResolution) {
	user := in.Database.AppUser
	uexists, err := userExists(ctx, db, user)
	if err != nil {
		rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "blocker",
			Target: user, Detail: err.Error()})
		return false, dbResUnknown
	}
	if !uexists {
		detail := "将创建应用账号并授权四个库（口令为生成值）"
		if in.Database.Policy != "" {
			detail = fmt.Sprintf("应用账号不存在：将创建并授权（处置 %s 在无冲突时不生效）", in.Database.Policy)
		}
		rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "execute",
			Target: user, Detail: detail})
		return false, dbResProceed
	}
	// 账号已在：以状态中的凭据为候选探测匹配性（无状态值 = 将生成，必冲突）。
	candidate := st.Secrets[KeyAppDBPassword]
	if candidate != "" {
		ok, cerr := CheckAppPassword(ctx, in, candidate)
		if cerr != nil {
			rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "blocker",
				Target: user, Detail: "探测既有账号口令匹配性失败：" + cerr.Error()})
			return true, dbResUnknown
		}
		if ok {
			rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "skip-exists",
				Target: user, Detail: "应用账号已存在且本状态凭据验证通过：幂等无操作（仅补齐 GRANT）"})
			return false, dbResProceed
		}
	}
	detail := "应用账号已存在且口令与本次凭据不一致"
	if candidate == "" {
		detail = "应用账号已存在；本次将生成新凭据，必与既有口令不一致"
	}
	switch in.Database.Policy {
	case PolicyTakeover:
		rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "execute",
			Target: user, Detail: detail + "；处置 takeover：将把账号口令改为本次凭据，补齐授权并逐库验证连通"})
		return false, dbResTakeover
	case PolicyReuse:
		if strings.TrimSpace(in.Database.ExistingPassword) == "" {
			rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "blocker",
				Target: user, Detail: detail + "；处置 reuse 需提供 existing_password（既有账号当前口令）"})
			return true, dbResBlocked
		}
		ok, cerr := CheckAppPassword(ctx, in, in.Database.ExistingPassword)
		if cerr != nil {
			rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "blocker",
				Target: user, Detail: "验证提供的 existing_password 失败：" + cerr.Error()})
			return true, dbResBlocked
		}
		if !ok {
			rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "blocker",
				Target: user, Detail: detail + "；提供的 existing_password 与既有账号不匹配，请核对口令"})
			return true, dbResBlocked
		}
		rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "reuse",
			Target: user, Detail: detail + "；处置 reuse：existing_password 验证通过，将沿用并收编进状态（不改口令）"})
		return false, dbResReuse
	case PolicySkip:
		rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "skip-exists",
			Target: "(整步)", Detail: detail + "；处置 skip：databases 步整体保留现状（不建库、不改账号），只补其余步骤"})
		return true, dbResSkip
	default:
		rep.Items = append(rep.Items, PlanItem{Step: StepDatabases, Action: "blocker",
			Target: user, Detail: detail + "；须选择处置后继续：takeover（改口令到本次凭据）/ reuse（沿用既有口令）/ skip（保留本步现状）"})
		return true, dbResBlocked
	}
}

// ErrRunInProgress 表示已有运行中的初始化任务（单飞）。
var ErrRunInProgress = fmt.Errorf("a bootstrap run is already in progress")

func openAdmin(ctx context.Context, in *Inputs) (*sql.DB, error) {
	db, err := sql.Open("mysql", adminDSN(in))
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func sortedOutputServices(in *Inputs) []string {
	names := make([]string, 0, len(in.Paths.BaremetalOutputs))
	for svc := range in.Paths.BaremetalOutputs {
		names = append(names, svc)
	}
	sortStrings(names)
	return names
}

func templateFileFor(service string) string {
	for file, svc := range TemplateServices {
		if svc == service {
			return file
		}
	}
	return ""
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func planKeys(dir string) ([]PlanItem, error) {
	privErr := fileAccessible(filepath.Join(dir, "private_key.pem"))
	pubErr := fileAccessible(filepath.Join(dir, "public_key.pem"))
	switch {
	case privErr == nil && pubErr == nil:
		fp, err := privateKeyFingerprint(filepath.Join(dir, "private_key.pem"))
		if err != nil {
			return nil, err
		}
		return []PlanItem{{Step: StepKeys, Action: "skip-exists", Target: dir,
			Detail: "密钥对已存在，原样保留（指纹 " + fp + "）"}}, nil
	case privErr != nil && pubErr == nil:
		return []PlanItem{{Step: StepKeys, Action: "blocker", Target: dir,
			Detail: "只有公钥没有私钥：无法安全恢复，需人工核对后再继续"}}, nil
	case privErr == nil:
		return []PlanItem{{Step: StepKeys, Action: "write", Target: filepath.Join(dir, "public_key.pem"),
			Detail: "私钥已在、公钥缺失：将由私钥推导补齐"}}, nil
	default:
		return []PlanItem{{Step: StepKeys, Action: "generate", Target: dir,
			Detail: "将生成 RSA-4096 密钥对（私钥 0600 / 公钥 0644，与 YGG generateKeyPair 同形态）"}}, nil
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
