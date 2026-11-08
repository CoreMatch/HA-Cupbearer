package orchestrator

// 部署后冒烟验证：健康探测（/health 200）只说明进程活着、路由挂上了，
// 不说明服务能发令牌、能读配置、能连数据库。声明了 facts.smoke 的项目
// 在健康判定通过后还要跑一遍真实请求；冒烟失败 = 部署失败，走同样的
// 自动回滚。未声明 smoke 的项目维持现状只看健康，不强制。
//
// 密钥边界：冒烟请求可以带固定头/体，但密钥值只以 ${secret:<name>}
// 形态出现在 Manifest，实际值从凭据文件 smoke_secrets 解析——
// 绝不写进 Manifest。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"HA-Services-Deployer/internal/manifest"
)

const (
	smokeDefaultTimeoutSeconds = 5
	// smokeBodyLimit 限制断言读取的响应体大小，防异常响应撑爆内存。
	smokeBodyLimit = 4 << 20
)

// smokeSecretRefRe 与 Manifest 校验侧同形：${secret:<name>}。
var smokeSecretRefRe = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_-]+)\}`)

// smokeLabel 给出检查的可读名字（未命名时按序号）。
func smokeLabel(chk *manifest.SmokeCheck, idx int) string {
	if chk.Name != "" {
		return chk.Name
	}
	return fmt.Sprintf("smoke[%d]", idx)
}

// runSmoke 按声明顺序执行全部冒烟检查；任一条失败即整体失败，
// 错误信息指明是哪一条、失败在哪。
func (o *Orchestrator) runSmoke(ctx context.Context, m *manifest.Manifest, portOverride int) error {
	for i := range m.Facts.Smoke {
		chk := &m.Facts.Smoke[i]
		if err := o.smokeOne(ctx, m, chk, portOverride); err != nil {
			return fmt.Errorf("smoke %q: %w", smokeLabel(chk, i), err)
		}
	}
	return nil
}

// smokeOne 执行单条冒烟请求并核对状态码与响应体断言。
func (o *Orchestrator) smokeOne(ctx context.Context, m *manifest.Manifest, chk *manifest.SmokeCheck, portOverride int) error {
	url, err := o.smokeURL(m, chk, portOverride)
	if err != nil {
		return err
	}

	method := strings.ToUpper(chk.Method)
	if method == "" {
		method = http.MethodGet
	}

	body := chk.Body
	if body != "" {
		if body, err = o.resolveSmokeSecrets(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	for _, k := range sortedHeaderKeys(chk.Headers) {
		v, err := o.resolveSmokeSecrets(chk.Headers[k])
		if err != nil {
			return err
		}
		req.Header.Set(k, v)
	}
	if body != "" {
		ct := chk.ContentType
		if ct == "" {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}

	timeout := chk.TimeoutSeconds
	if timeout <= 0 {
		timeout = smokeDefaultTimeoutSeconds
	}
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil || isTimeoutErr(err) {
			return fmt.Errorf("timeout after %ds: %v", timeout, err)
		}
		return fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	expect := chk.ExpectStatus
	if expect == 0 {
		expect = http.StatusOK
	}
	if resp.StatusCode != expect {
		return fmt.Errorf("expect status %d, got %d", expect, resp.StatusCode)
	}

	// content-type 断言先于响应体断言：SPA 静态回退会对未知路径一律
	// 回 200 + text/html，伪造出"服务活着"的假象——状态码与子串都
	// 可能被回退糊弄，content-type 是区分真实端点与静态回退的第一道闸。
	if want := chk.Assert.ExpectContentType; want != "" {
		if got := resp.Header.Get("Content-Type"); !contentTypeMatches(got, want) {
			return fmt.Errorf("expect content-type %q, got %q (static/SPA fallback responses are typically text/html — the endpoint likely does not exist in the running binary)", want, got)
		}
	}

	return assertSmokeBody(resp.Body, &chk.Assert)
}

// contentTypeMatches 比较 Content-Type 的媒体类型部分：
// 忽略 "; charset=..." 等参数，大小写不敏感。
func contentTypeMatches(got, want string) bool {
	if i := strings.IndexByte(got, ';'); i >= 0 {
		got = got[:i]
	}
	return strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(want))
}

// smokeURL 解析冒烟请求地址：未声明 port_name 时沿用 health 端口；
// 部署上下文的 health_port 覆盖同样作用于冒烟（仅对 health 端口生效）。
func (o *Orchestrator) smokeURL(m *manifest.Manifest, chk *manifest.SmokeCheck, portOverride int) (string, error) {
	healthPortName := ""
	if m.Facts.Health.HTTP != nil {
		healthPortName = m.Facts.Health.HTTP.PortName
	}
	portName := chk.PortName
	if portName == "" {
		portName = healthPortName
	}
	if portName == "" {
		return "", fmt.Errorf("no port declared for smoke (facts.ports empty)")
	}

	port := 0
	if portOverride > 0 && portName == healthPortName {
		port = portOverride
	} else {
		for _, p := range m.Facts.Ports {
			if p.Name == portName {
				port = p.Port
				break
			}
		}
	}
	if port == 0 {
		return "", fmt.Errorf("smoke port %q not declared in facts.ports", portName)
	}

	host := o.cfg.Orchestration.ProbeHost
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d%s", host, port, chk.Path), nil
}

// resolveSmokeSecrets 把文本中的 ${secret:<name>} 全部替换为凭据文件里的值；
// 任一缺失即报错并点名，不静默放过。
func (o *Orchestrator) resolveSmokeSecrets(text string) (string, error) {
	if !strings.Contains(text, "${") {
		return text, nil
	}
	var missing []string
	out := smokeSecretRefRe.ReplaceAllStringFunc(text, func(ref string) string {
		name := smokeSecretRefRe.FindStringSubmatch(ref)[1]
		if o.creds != nil && o.creds.SmokeSecrets != nil {
			if v, ok := o.creds.SmokeSecrets[name]; ok && v != "" {
				return v
			}
		}
		missing = append(missing, name)
		return ref
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("smoke secret(s) %v not configured in credentials file (smoke_secrets)", missing)
	}
	return out, nil
}

// smokeSecretNames 列出文本引用的全部密钥名（preflight 用）。
func smokeSecretNames(text string) []string {
	matches := smokeSecretRefRe.FindAllStringSubmatch(text, -1)
	names := make([]string, 0, len(matches))
	for _, mt := range matches {
		names = append(names, mt[1])
	}
	return names
}

func sortedHeaderKeys(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isTimeoutErr(err error) bool {
	type timeout interface{ Timeout() bool }
	for e := err; e != nil; {
		if t, ok := e.(timeout); ok && t.Timeout() {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return false
}

// assertSmokeBody 执行响应体断言（contains / json_field / json_not_empty）。
func assertSmokeBody(body io.Reader, assert *manifest.SmokeAssert) error {
	if assert.Contains == "" && assert.JSONField == "" && assert.JSONNotEmpty == "" {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(body, smokeBodyLimit))
	if err != nil {
		return fmt.Errorf("read response body: %v", err)
	}

	if assert.Contains != "" && !strings.Contains(string(data), assert.Contains) {
		return fmt.Errorf("body assertion failed: response does not contain %q", assert.Contains)
	}

	if assert.JSONField != "" {
		if _, ok := jsonLookup(data, assert.JSONField); !ok {
			return fmt.Errorf("body assertion failed: json field %q not present", assert.JSONField)
		}
	}

	if assert.JSONNotEmpty != "" {
		v, ok := jsonLookup(data, assert.JSONNotEmpty)
		if !ok {
			return fmt.Errorf("body assertion failed: json field %q not present", assert.JSONNotEmpty)
		}
		if !jsonNotEmpty(v) {
			return fmt.Errorf("body assertion failed: json field %q is empty", assert.JSONNotEmpty)
		}
	}
	return nil
}

// jsonLookup 按点路径（如 data.version）在 JSON 文档中查找值。
func jsonLookup(data []byte, path string) (interface{}, bool) {
	var root interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false
	}
	cur := root
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		v, ok := obj[part]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func jsonNotEmpty(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return false
	case []interface{}:
		return len(t) > 0
	case map[string]interface{}:
		return len(t) > 0
	case string:
		return t != ""
	default:
		return true // 数值/布尔：存在即视为非空
	}
}
