// Package bootstrap 是 Cupbearer 的首次运行初始化层：把一台全新机器变成
// 可运行的 HA 平台，不需要人手工敲 openssl、手工建库、手工拼 .env。
//
// 设计边界（与编排本体的关系）：
//   - 编排本体（版本目录+软链、Tinyvisor 重启、健康判定、一键触发）不动；
//     初始化是新增的一层，不依赖 orchestration.enabled；
//   - 幂等：重复执行不覆盖已有值、不重复建库建行；已存在的凭据一律保留，
//     漂移只告警不覆盖；
//   - 不泄密：生成的值只落本地状态文件（0600，不进仓库），任何响应/计划/
//     运行记录里只出现掩码或指纹，绝不出现明文；
//   - 断点续跑：每一步幂等 + 逐项落运行记录，重跑即续跑。
//
// 契约：HA-Contract docs/api/openapi/ha-cupbearer.yaml v1.3.0
// （/api/v1/deploy/bootstrap/*）。
package bootstrap

import (
	"fmt"
	"net/url"
	"strings"
)

// Inputs 是向导/调用方提交的统一输入。只有后端推不出来的值才要求人提供
// （数据库管理口连接、对外地址）；其余全有默认值，全部密钥由本层生成。
// 契约形状见 BootstrapInputs。
type Inputs struct {
	Targets      Targets      `json:"targets"`
	Infra        Infra        `json:"infra"`
	Database     Database     `json:"database"`
	Redis        Redis        `json:"redis"`
	PublicURLs   PublicURLs   `json:"public_urls"`
	InternalURLs InternalURLs `json:"internal_urls"`
	SMTP         SMTP         `json:"smtp"`
	ClientIDs    ClientIDs    `json:"client_ids"`
	Paths        Paths        `json:"paths"`
}

// Infra 描述 MySQL/Redis 基础设施。语义（作者边界）：按所选部署路径各自
// 成立——裸金属编排时是为裸金属服务的本地实例，容器编排时归 compose 一侧；
// 已存在就接管复用，不强行再造一个。本层负责探测、接管、必要时一键拉起
// 容器，并按目标路径如实报告对应的连接形态。
type Infra struct {
	// Launch 允许在探测不到可用实例时一键拉起容器（默认 true）。
	// 置 false 则只接管已有实例，探测不到即报缺。
	Launch        *bool `json:"launch"`
	MySQLHostPort int   `json:"mysql_host_port"` // 宿主发布口，默认 3306
	RedisHostPort int   `json:"redis_host_port"` // 宿主发布口，默认 6379
}

// InfraLaunch 报告是否允许拉起容器（nil = 允许）。
func (i *Infra) InfraLaunch() bool { return i.Launch == nil || *i.Launch }

// Targets 选择初始化覆盖哪条部署路径（可同时选）。
type Targets struct {
	// Container：合并容器 .env + 生成签名密钥（模板仍由容器 entrypoint
	// 在启动时渲染——既有设计不变，本层只供给值）。
	Container bool `json:"container"`
	// Baremetal：把 deploy/config/*.tmpl 渲染到各自输出路径（裸金属路径
	// 没有 entrypoint，渲染必须由本层完成）。
	Baremetal bool `json:"baremetal"`
}

// 数据库冲突的三种处置（见 Database.Policy）。
const (
	PolicyTakeover = "takeover" // 把既有账号口令改为本次凭据，继续
	PolicyReuse    = "reuse"    // 不改口令，验证并沿用既有口令（收编进状态）
	PolicySkip     = "skip"     // 保留现状：databases 步整体不动，只补其余步骤
)

// Database 描述 MySQL 管理口连接与库规划。
type Database struct {
	AdminHost     string `json:"admin_host"`
	AdminPort     int    `json:"admin_port"`
	AdminUser     string `json:"admin_user"`
	AdminPassword string `json:"admin_password"`
	AppUser       string `json:"app_user"`    // 默认 hrpa；口令由本层生成
	ManageUser    *bool  `json:"manage_user"` // 默认 true：建账号并授权
	// Policy 是"应用账号已存在且口令与本次凭据不一致"时的处置：
	// takeover / reuse / skip。只在探测到该冲突时生效——无冲突（账号不存在
	// 或口令本就一致）时一律忽略；探测到冲突而未给政策则显式失败（fail-closed，
	// 绝不半途改一半）。
	Policy string `json:"policy"`
	// ExistingPassword 是既有账号的当前口令；policy=reuse 时必填，
	// 先验证后收编进状态，任何响应中绝不回显。
	ExistingPassword string  `json:"existing_password"`
	Names            DBNames `json:"names"`
}

// DBNames 是四个独占库的库名（schema_migrations 簿记表结构互不兼容，
// 共库必互踩——与 compose/.env.example 的既有结论一致）。
type DBNames struct {
	HRPAuth   string `json:"hrpauth"`   // 默认 hrpa
	YGG       string `json:"ygg"`       // 默认 ygg
	HASkinLib string `json:"haskinlib"` // 默认 haskinlib
	MCA       string `json:"mca"`       // 默认 matchcorearena
}

// Redis 连接（生产为面板软件，本层只登记不供给）。
type Redis struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"`
	DB       int    `json:"db"`
}

// PublicURLs 是对外地址（域名只有人知道，属必填）。
type PublicURLs struct {
	HRPAuth         string `json:"hrpauth"`
	Frontend        string `json:"frontend"`
	YGG             string `json:"ygg"`
	HASkinProxy     string `json:"haskinproxy"`
	WinnerProxy     string `json:"winnerproxy"`
	MaintenanceView string `json:"maintenanceview"`
}

// InternalURLs 是主机内服务发现地址的可选覆盖；留空时按形态推导
// （容器 = compose 网络名，裸金属 = 127.0.0.1+声明端口）。
type InternalURLs struct {
	HRPAuth   string `json:"hrpauth"`
	YGG       string `json:"ygg"`
	Cupbearer string `json:"cupbearer"`
}

// SMTP 可选：全空时服务照常启动，只是不发信。
type SMTP struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	FromEmail string `json:"from_email"`
}

// ClientIDs 可选覆盖；默认值遵循 presence 命名约定（种子 client_id 必须
// 与 presence 自报名逐字一致，否则自助注册 get-or-create 会另建第二行）。
type ClientIDs struct {
	Super       string `json:"super"`
	Public      string `json:"public"`
	WinnerProxy string `json:"winnerproxy"`
	YGG         string `json:"ygg"`
	HASkinProxy string `json:"haskinproxy"`
	SDKHandler  string `json:"sdkhandler"`
	HASkinLib   string `json:"haskinlib"`
}

// Paths 是产物落点（默认相对进程工作目录，与 compose 模板的位置约定一致）。
type Paths struct {
	EnvFile    string `json:"env_file"`    // 默认 deploy/.env
	SecretsDir string `json:"secrets_dir"` // 默认 deploy/secrets
	// BaremetalOutputs 按服务名给出渲染输出路径（如
	// {"hrpauth": "/www/wwwroot/ha/config.yaml"}）；向导侧用清单预填。
	BaremetalOutputs map[string]string `json:"baremetal_outputs"`
}

// Defaults 是与 .env.example / 模板 / presence 约定逐字对齐的默认值。
// 改这里必须同步 deploy/.env.example 与 knowledge/0015。
const (
	DefaultAppDBUser      = "hrpa"
	DefaultHRPAuthDB      = "hrpa"
	DefaultYGGDB          = "ygg"
	DefaultHASkinLibDB    = "haskinlib"
	DefaultMCADB          = "matchcorearena"
	DefaultSuperClientID  = "hrpauth-internal-super"
	DefaultPublicClientID = "hrpauth-webui"
	DefaultWinnerProxyID  = "WinnerProxy"
	DefaultYGGClientID    = "HRPAuth-Yggdrasil-API"
	DefaultSkinProxyID    = "HASkinProxy"
	DefaultSDKHandlerID   = "hrpauth-webui-sdkhandler"
	DefaultSkinLibID      = "haskinlib"
	DefaultEnvFile        = "deploy/.env"
	DefaultSecretsDir     = "deploy/secrets"
	DefaultTemplateDir    = "deploy/config"
	DefaultRedisHost      = "127.0.0.1"
	DefaultSMTPFromEmail  = "no-reply@example.com"
)

// DefaultRedisPort / DefaultSMTPPort 等数值默认。
const (
	DefaultRedisPort = 6379
	DefaultSMTPPort  = 25
	DefaultMySQLPort = 3306
)

// FieldError 是一条人话校验结论（向导当场拦截用）。
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e FieldError) Error() string { return e.Field + ": " + e.Message }

// fillDefaults 把缺省值补齐（只在调用方没给时生效）。
func (in *Inputs) fillDefaults() {
	if in.Infra.MySQLHostPort == 0 {
		in.Infra.MySQLHostPort = DefaultMySQLPort
	}
	if in.Infra.RedisHostPort == 0 {
		in.Infra.RedisHostPort = DefaultRedisPort
	}
	if in.Database.AdminPort == 0 {
		in.Database.AdminPort = DefaultMySQLPort
	}
	if strings.TrimSpace(in.Database.AdminHost) == "" {
		// 一键拉起形态下管理口就在本机回环；接管外部实例时由调用方覆盖。
		in.Database.AdminHost = "127.0.0.1"
	}
	if strings.TrimSpace(in.Database.AdminUser) == "" {
		in.Database.AdminUser = "root"
	}
	if in.Database.AppUser == "" {
		in.Database.AppUser = DefaultAppDBUser
	}
	if in.Database.ManageUser == nil {
		t := true
		in.Database.ManageUser = &t
	}
	if in.Database.Names.HRPAuth == "" {
		in.Database.Names.HRPAuth = DefaultHRPAuthDB
	}
	if in.Database.Names.YGG == "" {
		in.Database.Names.YGG = DefaultYGGDB
	}
	if in.Database.Names.HASkinLib == "" {
		in.Database.Names.HASkinLib = DefaultHASkinLibDB
	}
	if in.Database.Names.MCA == "" {
		in.Database.Names.MCA = DefaultMCADB
	}
	if in.Redis.Host == "" {
		in.Redis.Host = DefaultRedisHost
	}
	if in.Redis.Port == 0 {
		in.Redis.Port = DefaultRedisPort
	}
	if in.SMTP.FromEmail == "" {
		in.SMTP.FromEmail = DefaultSMTPFromEmail
	}
	if in.SMTP.Host != "" && in.SMTP.Port == 0 {
		in.SMTP.Port = DefaultSMTPPort
	}
	if in.ClientIDs.Super == "" {
		in.ClientIDs.Super = DefaultSuperClientID
	}
	if in.ClientIDs.Public == "" {
		in.ClientIDs.Public = DefaultPublicClientID
	}
	if in.ClientIDs.WinnerProxy == "" {
		in.ClientIDs.WinnerProxy = DefaultWinnerProxyID
	}
	if in.ClientIDs.YGG == "" {
		in.ClientIDs.YGG = DefaultYGGClientID
	}
	if in.ClientIDs.HASkinProxy == "" {
		in.ClientIDs.HASkinProxy = DefaultSkinProxyID
	}
	if in.ClientIDs.SDKHandler == "" {
		in.ClientIDs.SDKHandler = DefaultSDKHandlerID
	}
	if in.ClientIDs.HASkinLib == "" {
		in.ClientIDs.HASkinLib = DefaultSkinLibID
	}
	if in.Paths.EnvFile == "" {
		in.Paths.EnvFile = DefaultEnvFile
	}
	if in.Paths.SecretsDir == "" {
		in.Paths.SecretsDir = DefaultSecretsDir
	}
	if !in.Targets.Container && !in.Targets.Baremetal {
		// 一个都不选 = 什么都没得干；默认两条路径都覆盖（一键的本义）。
		in.Targets.Container = true
		in.Targets.Baremetal = true
	}
}

// Validate 逐字段校验，一次报全（向导当场拦截，不让人提交后才发现）。
func (in *Inputs) Validate() []FieldError {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{Field: field, Message: msg}) }

	if !in.Targets.Container && !in.Targets.Baremetal {
		add("targets", "至少选择一条部署路径（容器 / 裸金属）")
	}

	// 数据库管理口：建库建账号的硬前提。注意——管理口令在"一键拉起
	// MySQL 容器"形态下由本层生成（无需人提供），因此这里只校验能静态
	// 判定的字段；口令是否必需由 infra 步骤按探测结果动态强制
	//（接管到既有实例而无口令 → 当场人话报错）。
	if strings.TrimSpace(in.Database.AdminHost) == "" {
		add("database.admin_host", "必须提供 MySQL 管理口地址（建库建账号用），例如 127.0.0.1")
	}
	if in.Database.AdminPort <= 0 || in.Database.AdminPort > 65535 {
		add("database.admin_port", "MySQL 端口必须是 1-65535 的整数")
	}
	if strings.TrimSpace(in.Database.AdminUser) == "" {
		add("database.admin_user", "必须提供 MySQL 管理口账号（通常是 root）")
	}
	if in.Infra.MySQLHostPort <= 0 || in.Infra.MySQLHostPort > 65535 {
		add("infra.mysql_host_port", "MySQL 宿主发布端口必须是 1-65535 的整数")
	}
	if in.Infra.RedisHostPort <= 0 || in.Infra.RedisHostPort > 65535 {
		add("infra.redis_host_port", "Redis 宿主发布端口必须是 1-65535 的整数")
	}
	for _, n := range []struct{ name, val string }{
		{"database.names.hrpauth", in.Database.Names.HRPAuth},
		{"database.names.ygg", in.Database.Names.YGG},
		{"database.names.haskinlib", in.Database.Names.HASkinLib},
		{"database.names.mca", in.Database.Names.MCA},
	} {
		if n.val != "" && !validIdentifier(n.val) {
			add(n.name, fmt.Sprintf("库名 %q 非法：只允许字母/数字/下划线，且以字母开头", n.val))
		}
	}
	if in.Database.AppUser != "" && !validIdentifier(in.Database.AppUser) {
		add("database.app_user", fmt.Sprintf("账号名 %q 非法：只允许字母/数字/下划线，且以字母开头", in.Database.AppUser))
	}
	switch in.Database.Policy {
	case "", PolicyTakeover, PolicyReuse, PolicySkip:
	default:
		add("database.policy", fmt.Sprintf("处置 %q 非法：只允许 takeover（接管）/ reuse（沿用）/ skip（跳过）", in.Database.Policy))
	}
	if in.Database.Policy == PolicyReuse && strings.TrimSpace(in.Database.ExistingPassword) == "" {
		add("database.existing_password", "policy=reuse 必须提供既有账号的当前口令（先验证、后收编进状态）")
	}
	if in.Database.Policy != PolicyReuse && strings.TrimSpace(in.Database.ExistingPassword) != "" {
		add("database.existing_password", "existing_password 只与 policy=reuse 搭配使用")
	}

	if in.Redis.Port < 0 || in.Redis.Port > 65535 {
		add("redis.port", "Redis 端口必须是 0-65535 的整数")
	}

	// 对外地址：compose 以 :? 强制必填的四个 + 派生必需的公开域。
	checkURL := func(field, val string, required bool) {
		if strings.TrimSpace(val) == "" {
			if required {
				add(field, "必须提供该对外地址（客户端/玩家要访问它；空值会让容器编排直接拒绝启动）")
			}
			return
		}
		u, err := url.Parse(val)
		if err != nil || u.Scheme == "" || u.Host == "" {
			add(field, fmt.Sprintf("%q 不是合法 URL：必须带协议与主机，例如 https://ha.example.com", val))
		}
	}
	checkURL("public_urls.hrpauth", in.PublicURLs.HRPAuth, true)
	checkURL("public_urls.frontend", in.PublicURLs.Frontend, true)
	checkURL("public_urls.ygg", in.PublicURLs.YGG, true)
	checkURL("public_urls.haskinproxy", in.PublicURLs.HASkinProxy, true)
	checkURL("public_urls.winnerproxy", in.PublicURLs.WinnerProxy, true)
	checkURL("public_urls.maintenanceview", in.PublicURLs.MaintenanceView, true)
	checkURL("internal_urls.hrpauth", in.InternalURLs.HRPAuth, false)
	checkURL("internal_urls.ygg", in.InternalURLs.YGG, false)
	checkURL("internal_urls.cupbearer", in.InternalURLs.Cupbearer, false)

	// 裸金属目标必须给出渲染输出（向导会用清单预填，但空值不能落盘）。
	if in.Targets.Baremetal {
		for svc, path := range in.Paths.BaremetalOutputs {
			if strings.TrimSpace(path) == "" {
				add("paths.baremetal_outputs."+svc, "裸金属渲染输出路径不能为空")
			}
		}
	}
	return errs
}

// validIdentifier 限定 MySQL 库名/账号名形态，杜绝注入与怪名。
func validIdentifier(s string) bool {
	if s == "" {
		return false
	}
	first := s[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// Prepare 补默认值并校验；错误一次报全。
func (in *Inputs) Prepare() []FieldError {
	in.fillDefaults()
	return in.Validate()
}
