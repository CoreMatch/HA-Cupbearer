// Package mvmanager 负责"Cupbearer 运行时自动拉起 MaintenanceView"：
// 用户不需要单独部署 MV。语义边界：
//
//   - 已有一份健康的 MV 在配置地址上 → 识别并复用，绝不起第二份；
//   - 否则找 MV 二进制，生成 MV 配置（Cupbearer 地址/端口/凭据全部自动推导），
//     作为受管子进程拉起并监督（有界重启、退出留痕、Cupbearer 退出时优雅停止）；
//   - 二进制找不到 → 如实报 unavailable（不致命：Cupbearer 本体照常运行，
//     容器形态里 MV 本来就是独立 compose 服务）。
//
// MV 配置里写入 loopback_session: true（仅 Cupbearer 自启形态）：面板绑定
// 回环且请求来自回环时免登录；部署凭据仍由 MV 服务端持有并转发，浏览器
// 永远接触不到（与 knowledge/0007 的凭据边界一致）。
package mvmanager

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config 是 MV 监督的配置（Cupbearer config v3 maintenance_view 段）。
type Config struct {
	Enabled    bool   `yaml:"enabled"`
	BinaryPath string `yaml:"binary_path"` // 空 = 自动探测候选位置
	Listen     string `yaml:"listen"`      // 默认 127.0.0.1:2771
	WorkDir    string `yaml:"work_dir"`    // 默认 ./data/maintenance-view
}

const (
	DefaultListen     = "127.0.0.1:2771"
	DefaultWorkDir    = "./data/maintenance-view"
	maxPortProbe      = 10
	healthTimeout     = 3 * time.Second
	maxFastRestarts   = 5
	fastRestartWindow = 2 * time.Minute
)

// Manager 监督至多一个 MV 实例。
type Manager struct {
	cfg          Config
	cupbearerURL string
	deployToken  string

	mu       sync.Mutex
	mode     string // managed | reused | disabled | unavailable
	address  string
	pid      int
	detail   string
	cmd      *exec.Cmd
	logFile  *os.File
	stopping bool
	stopCh   chan struct{}
}

// New 构造监督器。cupbearerURL 是 MV 要回连的 Cupbearer 地址，
// deployToken 注入 MV 配置（服务端持有，绝不下发浏览器）。
func New(cfg Config, cupbearerURL, deployToken string) *Manager {
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = DefaultWorkDir
	}
	return &Manager{cfg: cfg, cupbearerURL: cupbearerURL, deployToken: deployToken, mode: "disabled"}
}

// StatusView 是只读状态视图（契约：GET /api/v1/deploy/maintenance-view）。
type StatusView struct {
	Mode       string `json:"mode"`
	Address    string `json:"address,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Healthy    *bool  `json:"healthy,omitempty"`
	BinaryPath string `json:"binary_path,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// Start 执行识别/拉起流程（Cupbearer 启动时调用一次）。
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.cfg.Enabled {
		m.mode = "disabled"
		m.detail = "maintenance_view.enabled=false"
		return
	}

	// 1. 复用探测：配置地址已有健康的 MV → 绝不起第二份。
	if probeMV(ctx, "http://"+m.cfg.Listen) {
		m.mode = "reused"
		m.address = m.cfg.Listen
		m.detail = "已有健康的 MaintenanceView 在运行，直接复用（不起第二份）"
		log.Printf("mvmanager: reusing existing MaintenanceView at %s", m.cfg.Listen)
		return
	}

	// 2. 找二进制。
	binary := m.findBinary()
	if binary == "" {
		m.mode = "unavailable"
		m.detail = "未找到 MaintenanceView 二进制：请把二进制放到 Cupbearer 同目录，" +
			"或在配置 maintenance_view.binary_path 指定路径（容器形态可忽略：MV 是独立 compose 服务）"
		log.Printf("mvmanager: %s", m.detail)
		return
	}

	// 3. 选端口：配置口被非 MV 进程占用时顺延找空闲口；顺延口上已有
	// 健康的 MV 同样复用（不起第二份）。
	listen, reused := m.pickListen()
	if reused {
		return
	}
	if listen == "" {
		m.mode = "unavailable"
		m.detail = fmt.Sprintf("从 %s 起 %d 个端口均被占用且都不是 MV，放弃自动拉起", m.cfg.Listen, maxPortProbe)
		log.Printf("mvmanager: %s", m.detail)
		return
	}

	// 4. 生成 MV 配置并拉起受管子进程。
	if err := m.writeMVConfig(listen); err != nil {
		m.mode = "unavailable"
		m.detail = "生成 MaintenanceView 配置失败：" + err.Error()
		log.Printf("mvmanager: %s", m.detail)
		return
	}
	if err := m.spawn(binary, listen); err != nil {
		m.mode = "unavailable"
		m.detail = "拉起 MaintenanceView 失败：" + err.Error()
		log.Printf("mvmanager: %s", m.detail)
		return
	}
	m.mode = "managed"
	m.address = listen
	m.detail = "受管子进程（配置自动生成，凭据服务端持有）"
	log.Printf("mvmanager: started MaintenanceView (pid %d) at %s", m.pid, listen)
	go m.supervise(binary)
}

// Stop 优雅停止受管子进程（Cupbearer 退出时调用）。
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopping = true
	cmd := m.cmd
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
	}
	m.mu.Lock()
	m.closeLog()
	m.mu.Unlock()
	log.Printf("mvmanager: stopped supervised MaintenanceView")
}

// Status 返回当前监督状态（含一次主动健康探测）。
func (m *Manager) Status(ctx context.Context) StatusView {
	m.mu.Lock()
	view := StatusView{Mode: m.mode, Address: m.address, PID: m.pid, Detail: m.detail, BinaryPath: m.cfg.BinaryPath}
	m.mu.Unlock()
	if view.Address != "" && (view.Mode == "managed" || view.Mode == "reused") {
		healthy := probeMV(ctx, "http://"+view.Address)
		view.Healthy = &healthy
	}
	return view
}

// --- 内部 ---

func (m *Manager) findBinary() string {
	if m.cfg.BinaryPath != "" {
		if isExecutable(m.cfg.BinaryPath) {
			return m.cfg.BinaryPath
		}
		return ""
	}
	exeDir, _ := os.Getwd()
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	candidates := []string{
		filepath.Join(exeDir, "MaintenanceView"),
		filepath.Join(exeDir, "HA-Services-MaintenanceView"),
		filepath.Join(exeDir, "ha-services-maintenance-view"),
		"./MaintenanceView",
		"./HA-Services-MaintenanceView",
	}
	for _, c := range candidates {
		if isExecutable(c) {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

func (m *Manager) pickListen() (string, bool) {
	host, portStr, err := net.SplitHostPort(m.cfg.Listen)
	if err != nil {
		host, portStr = "127.0.0.1", "2771"
	}
	base, err := strconv.Atoi(portStr)
	if err != nil {
		base = 2771
	}
	for i := 0; i < maxPortProbe; i++ {
		addr := net.JoinHostPort(host, strconv.Itoa(base+i))
		if probeMV(context.Background(), "http://"+addr) {
			// 顺延口上已有健康的 MV：同样复用，不起第二份。
			m.mode = "reused"
			m.address = addr
			m.detail = fmt.Sprintf("端口 %d 已有健康的 MaintenanceView，直接复用", base+i)
			return "", true
		}
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return addr, false
		}
	}
	return "", false
}

func (m *Manager) writeMVConfig(listen string) error {
	if err := os.MkdirAll(m.cfg.WorkDir, 0o755); err != nil {
		return err
	}
	dist := m.findFrontendDist()
	var sb strings.Builder
	sb.WriteString("# 由 HA-Cupbearer 自动生成——勿手工编辑。\n")
	sb.WriteString("# Cupbearer 自启 MV 形态：配置自动推导，凭据由 MV 服务端持有并转发。\n")
	sb.WriteString("version: \"1\"\n")
	sb.WriteString("server:\n")
	sb.WriteString("  listen: \"" + listen + "\"\n")
	sb.WriteString("  public_base_url: \"http://" + listen + "\"\n")
	if dist != "" {
		sb.WriteString("  frontend_dist: \"" + dist + "\"\n")
	} else {
		sb.WriteString("  frontend_dist: \"./frontend/dist\"\n")
	}
	sb.WriteString("  loopback_session: true\n")
	sb.WriteString("cupbearer:\n")
	sb.WriteString("  base_url: \"" + m.cupbearerURL + "\"\n")
	if m.deployToken != "" {
		sb.WriteString("  token: \"" + m.deployToken + "\"\n")
	}
	sb.WriteString("state:\n")
	sb.WriteString("  projects_path: \"./data/projects.json\"\n")

	path := filepath.Join(m.cfg.WorkDir, "config.yaml")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) findFrontendDist() string {
	binaryDir := filepath.Dir(m.cfg.BinaryPath)
	candidates := []string{
		filepath.Join(binaryDir, "frontend", "dist"),
		filepath.Join(binaryDir, "dist"),
		filepath.Join(m.cfg.WorkDir, "frontend", "dist"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !info.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

func (m *Manager) spawn(binary, listen string) error {
	cmd := exec.Command(binary)
	cmd.Dir = m.cfg.WorkDir
	logPath := filepath.Join(m.cfg.WorkDir, "mv.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		lf.Close()
		return err
	}
	m.cmd = cmd
	m.logFile = lf
	m.pid = cmd.Process.Pid

	// 等待健康（启动窗口 30s）；不健康则杀掉交给监督循环重试。
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if probeMV(context.Background(), "http://"+listen) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("30 秒内未通过健康探测（日志：%s）", logPath)
}

// closeLog 关闭当前子进程日志文件（Wait 之后调用）。
func (m *Manager) closeLog() {
	if m.logFile != nil {
		m.logFile.Close()
		m.logFile = nil
	}
}

// supervise 有界重启：快速失败超过上限即放弃（响亮失败，不静默）。
func (m *Manager) supervise(binary string) {
	var fastFails []time.Time
	backoff := time.Second
	for {
		m.mu.Lock()
		if m.stopping {
			m.mu.Unlock()
			return
		}
		cmd := m.cmd
		listen := m.address
		m.mu.Unlock()

		if cmd != nil {
			_ = cmd.Wait()
			m.closeLog()
		}
		m.mu.Lock()
		if m.stopping {
			m.mu.Unlock()
			return
		}
		now := time.Now()
		fastFails = append(fastFails, now)
		cutoff := now.Add(-fastRestartWindow)
		kept := fastFails[:0]
		for _, t := range fastFails {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		fastFails = kept
		if len(fastFails) > maxFastRestarts {
			m.mode = "unavailable"
			m.pid = 0
			m.detail = fmt.Sprintf("MaintenanceView 在 %s 内连续退出超过 %d 次，停止自动重启（见工作目录日志）",
				fastRestartWindow, maxFastRestarts)
			m.mu.Unlock()
			log.Printf("mvmanager: giving up on supervised MaintenanceView: %s", m.detail)
			return
		}
		m.mu.Unlock()

		log.Printf("mvmanager: MaintenanceView exited; restarting in %s", backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}

		m.mu.Lock()
		if m.stopping {
			m.mu.Unlock()
			return
		}
		if err := m.spawn(binary, listen); err != nil {
			m.detail = "重启 MaintenanceView 失败：" + err.Error()
			log.Printf("mvmanager: restart failed: %v", err)
		} else {
			m.mode = "managed"
			m.detail = "受管子进程（发生过自动重启）"
			backoff = time.Second
		}
		m.mu.Unlock()
	}
}

// probeMV 判定地址上是否有健康的 MV（200 且信封 success=true）。
func probeMV(ctx context.Context, base string) bool {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return false
	}
	return strings.Contains(string(body), `"success":true`) ||
		strings.Contains(string(body), `"success": true`)
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
