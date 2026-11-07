// Package probe 提供应用级健康探测。
// 分工：Tinyvisor 负责进程存活与崩溃重启；编排器负责按 Manifest 的
// facts.health 做应用级探测，用于部署门控、自动回滚与状态可见性。
package probe

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// CheckHTTP 执行一次 HTTP 探测，状态码与期望一致才算通过。
func CheckHTTP(ctx context.Context, url string, expectStatus int, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != expectStatus {
		return fmt.Errorf("expect status %d, got %d", expectStatus, resp.StatusCode)
	}
	return nil
}

// Poll 按间隔重试探测，直到成功或耗尽重试次数。
// 返回值包含最后一次失败原因（成功时为 nil）。
func Poll(ctx context.Context, url string, expectStatus int, timeout, interval time.Duration, retries int) error {
	var lastErr error
	for i := 0; i < retries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
		lastErr = CheckHTTP(ctx, url, expectStatus, timeout)
		if lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("health probe failed after %d attempts: %w", retries, lastErr)
}
