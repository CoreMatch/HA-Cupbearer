// Package tinyvisor 是 Deployer 访问目标机 Tinyvisor 的 HTTP 客户端。
// 编排器通过它完成脚本登记（按名幂等 upsert）、原子重启与状态查询；
// 进程生命周期一律经由 Tinyvisor，编排器自身不拉起任何进程。
package tinyvisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotFound 表示按名查询无对应脚本。
var ErrNotFound = errors.New("tinyvisor: script not found")

// Script 是 Tinyvisor 脚本记录的视图（字段与其 API 对齐）。
type Script struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	WorkDir       string `json:"workDir"`
	Content       string `json:"content"`
	AutoStart     bool   `json:"autoStart"`
	RestartPolicy string `json:"restartPolicy"`
	Status        string `json:"status"`
	PID           int    `json:"pid,omitempty"`
}

// ScriptPayload 为登记/更新脚本的请求体。
type ScriptPayload struct {
	Name          string `json:"name"`
	WorkDir       string `json:"workDir"`
	Content       string `json:"content"`
	AutoStart     bool   `json:"autoStart"`
	RestartPolicy string `json:"restartPolicy"`
}

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Minivisor-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("tinyvisor: %s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}

	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("tinyvisor: decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// Ping 检查 Tinyvisor 可达（/ping 无需鉴权）。
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tinyvisor: ping -> %d", resp.StatusCode)
	}
	return nil
}

// GetByName 按名解析脚本；不存在返回 ErrNotFound。
func (c *Client) GetByName(ctx context.Context, name string) (*Script, error) {
	var out struct {
		Script Script `json:"script"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/scripts/by-name/"+name, nil, &out); err != nil {
		return nil, err
	}
	return &out.Script, nil
}

// Upsert 按名幂等登记脚本：不存在则创建，存在则更新。
// 返回最新脚本与是否本次创建。
func (c *Client) Upsert(ctx context.Context, payload ScriptPayload) (*Script, bool, error) {
	var out struct {
		Script  Script `json:"script"`
		Created bool   `json:"created"`
	}
	if err := c.do(ctx, http.MethodPut, "/api/scripts/by-name/"+payload.Name, payload, &out); err != nil {
		return nil, false, err
	}
	return &out.Script, out.Created, nil
}

// RestartByName 原子重启（运行中=停止后启动；未运行=直接启动）。
func (c *Client) RestartByName(ctx context.Context, name string) (*Script, error) {
	var out struct {
		Script Script `json:"script"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/scripts/by-name/"+name+"/restart", nil, &out); err != nil {
		return nil, err
	}
	return &out.Script, nil
}

// List 列出全部脚本（状态可见性用）。
func (c *Client) List(ctx context.Context) ([]Script, error) {
	var out struct {
		Scripts []Script `json:"scripts"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/scripts", nil, &out); err != nil {
		return nil, err
	}
	return out.Scripts, nil
}
