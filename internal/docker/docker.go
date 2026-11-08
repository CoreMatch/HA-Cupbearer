// Package docker 是容器运行时的最小驱动层：shell 出 docker CLI，
// 不引入 SDK 依赖。CLI 天然尊重 DOCKER_HOST 与当前 context，
// rootless（用户单元 socket）与特权守护进程之间切换零成本。
// 测试注入 fake 驱动，不依赖真实 daemon。
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotFound 表示容器/镜像不存在。
var ErrNotFound = errors.New("docker: not found")

// LabelProject 是托管容器的项目标签键（孤儿识别依据）。
const LabelProject = "ha-services-deployer.project"

// LabelManagedBy 标记"归本编排器管"的容器。
const LabelManagedBy = "ha-services-deployer.managed-by"

// PortMapping 是一条端口发布：宿主口（可绑指定地址）→ 容器口。
type PortMapping struct {
	HostIP        string // 空 = 默认地址；loopback 声明传 127.0.0.1
	HostPort      int
	ContainerPort int
}

// VolumeMount 是一条卷挂载。
type VolumeMount struct {
	Host      string
	Container string
}

// CreateOptions 是容器创建参数（单项目单实例，名字即项目名）。
type CreateOptions struct {
	Name          string
	Image         string
	Publish       []PortMapping
	Volumes       []VolumeMount
	RestartPolicy string // 如 unless-stopped
	Labels        map[string]string
	WorkDir       string
}

// ContainerState 是一次 inspect 的关键字段。
type ContainerState struct {
	ID      string
	Name    string
	Image   string // 创建时使用的镜像引用（标签）
	ImageID string // 镜像内容摘要（sha256:...），回滚对照证据
	State   string // created | running | exited | ...
	Running bool
}

// Driver 是编排器需要的容器运行时操作面。
type Driver interface {
	// Info 验证 daemon 可达与当前用户权限。
	Info(ctx context.Context) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	// ImageID 返回镜像内容摘要；本地构建/加载的镜像没有 RepoDigests，
	// 用 .Id 作为版本对照的证据。
	ImageID(ctx context.Context, ref string) (string, error)
	Build(ctx context.Context, tag, contextDir, dockerfile string) error
	// LoadAndTag 装载 docker-save 压缩包并打成目标标签。
	LoadAndTag(ctx context.Context, tarball, tag string) error
	Create(ctx context.Context, opts CreateOptions) (string, error)
	Start(ctx context.Context, name string) error
	// Remove 强制删除（含停止）；容器不存在视为成功（幂等）。
	Remove(ctx context.Context, name string) error
	// Inspect 查询容器；不存在返回 ErrNotFound。
	Inspect(ctx context.Context, name string) (*ContainerState, error)
	// ListManaged 列出带本编排器托管标签的全部容器（孤儿识别依据）。
	ListManaged(ctx context.Context) ([]ManagedContainer, error)
	// ImageTags 列出某仓库名下的全部标签（prune 依据）。
	ImageTags(ctx context.Context, repo string) ([]string, error)
	// RemoveImage 删除镜像标签；被占用时返回错误由调用方记录。
	RemoveImage(ctx context.Context, ref string) error
}

// CLI 经 docker 命令行实现的 Driver。
type CLI struct {
	// host 为 docker 端点（如 unix:///run/user/1000/docker.sock）；
	// 空串表示沿用环境（DOCKER_HOST / CLI context）。
	host string
}

// NewCLI 构造 CLI 驱动。
func NewCLI(host string) *CLI { return &CLI{host: host} }

// DetectHost 按序探测 docker 端点，供配置缺省时使用：
// 环境变量 DOCKER_HOST → $XDG_RUNTIME_DIR/docker.sock（rootless 用户单元）
// → /var/run/docker.sock。不写死任何具体路径。
func DetectHost() string {
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		return v
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		p := filepath.Join(xdg, "docker.sock")
		if _, err := os.Stat(p); err == nil {
			return "unix://" + p
		}
	}
	return "unix:///var/run/docker.sock"
}

func (c *CLI) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if c.host != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+c.host)
	}
	return cmd
}

func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := c.cmd(ctx, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "No such object") || strings.Contains(msg, "No such image") ||
			strings.Contains(msg, "No such container") || strings.Contains(msg, "could not be found") {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (c *CLI) Info(ctx context.Context) error {
	out, err := c.run(ctx, "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		return err
	}
	if out == "" {
		return errors.New("docker info returned no server version")
	}
	return nil
}

func (c *CLI) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := c.run(ctx, "image", "inspect", ref)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *CLI) ImageID(ctx context.Context, ref string) (string, error) {
	return c.run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
}

func (c *CLI) Build(ctx context.Context, tag, contextDir, dockerfile string) error {
	args := []string{"build", "-t", tag}
	if dockerfile != "" {
		args = append(args, "-f", filepath.Join(contextDir, dockerfile))
	}
	args = append(args, contextDir)
	_, err := c.run(ctx, args...)
	return err
}

func (c *CLI) LoadAndTag(ctx context.Context, tarball, tag string) error {
	out, err := c.run(ctx, "load", "-i", tarball)
	if err != nil {
		return err
	}
	// 输出形如 "Loaded image: repo:tag"（可能多行，取第一个）。
	var loaded string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Loaded image: "); ok {
			loaded = rest
			break
		}
	}
	if loaded == "" {
		return fmt.Errorf("docker load: no loaded image reported in output: %s", out)
	}
	if loaded == tag {
		return nil
	}
	_, err = c.run(ctx, "tag", loaded, tag)
	return err
}

func (c *CLI) Create(ctx context.Context, opts CreateOptions) (string, error) {
	args := []string{"create", "--name", opts.Name}
	for _, p := range opts.Publish {
		spec := fmt.Sprintf("%d:%d", p.HostPort, p.ContainerPort)
		if p.HostIP != "" {
			spec = fmt.Sprintf("%s:%s", p.HostIP, spec)
		}
		args = append(args, "--publish", spec)
	}
	for _, v := range opts.Volumes {
		args = append(args, "--volume", fmt.Sprintf("%s:%s", v.Host, v.Container))
	}
	if opts.RestartPolicy != "" {
		args = append(args, "--restart", opts.RestartPolicy)
	}
	for k, v := range opts.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}
	if opts.WorkDir != "" {
		args = append(args, "--workdir", opts.WorkDir)
	}
	args = append(args, opts.Image)
	return c.run(ctx, args...)
}

func (c *CLI) Start(ctx context.Context, name string) error {
	_, err := c.run(ctx, "start", name)
	return err
}

func (c *CLI) Remove(ctx context.Context, name string) error {
	_, err := c.run(ctx, "rm", "-f", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *CLI) Inspect(ctx context.Context, name string) (*ContainerState, error) {
	// 必须用 container inspect 消歧：项目名同时是镜像仓库名，
	// 裸 `docker inspect` 在同名镜像存在时会返回镜像 JSON（无 .State）。
	// 注意 inspect JSON 里运行状态字段是 .State.Status（不是 .State.State）。
	const tmpl = "{{.Id}}\n{{.Name}}\n{{.Config.Image}}\n{{.Image}}\n{{.State.Status}}\n{{.State.Running}}"
	out, err := c.run(ctx, "container", "inspect", "--format", tmpl, name)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 6 {
		return nil, fmt.Errorf("docker inspect: unexpected output for %s", name)
	}
	return &ContainerState{
		ID:      lines[0],
		Name:    strings.TrimPrefix(lines[1], "/"),
		Image:   lines[2],
		ImageID: lines[3],
		State:   lines[4],
		Running: lines[5] == "true",
	}, nil
}

// ManagedContainer 是一个托管容器的最小标识。
type ManagedContainer struct {
	Name    string
	Project string
}

func (c *CLI) ListManaged(ctx context.Context) ([]ManagedContainer, error) {
	out, err := c.run(ctx, "ps", "-a", "--filter", "label="+LabelManagedBy,
		"--format", "{{.Names}}|{{.Label \""+LabelProject+"\"}}")
	if err != nil {
		return nil, err
	}
	var list []ManagedContainer
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		name, project, _ := strings.Cut(line, "|")
		list = append(list, ManagedContainer{Name: name, Project: project})
	}
	return list, nil
}

func (c *CLI) ImageTags(ctx context.Context, repo string) ([]string, error) {
	out, err := c.run(ctx, "images", repo, "--format", "{{.Tag}}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var tags []string
	for _, t := range strings.Split(out, "\n") {
		if t != "" && t != "<none>" {
			tags = append(tags, t)
		}
	}
	return tags, nil
}

func (c *CLI) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.run(ctx, "rmi", ref)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
