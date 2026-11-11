package docker

import (
	"errors"
	"strings"
	"testing"
)

// ClassifyPullError 必须把不同种类的 registry 拒绝映射成可读的、
// 互不混淆的原因（401/403/404/429 各是各的问题）。
func TestClassifyPullError(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"unauthorized", `docker pull limeight/ha-x: exit status 1: unauthorized: authentication required`, "认证失败（401）"},
		{"denied", `docker pull limeight/ha-x: exit status 1: pull access denied for limeight/ha-x`, "没有权限拉取"},
		{"forbidden", `requested access to the resource is denied`, "没有权限拉取"},
		{"manifest unknown", `manifest unknown`, "不存在"},
		{"not found", `Error response from daemon: repository limeight/ha-x not found: does not exist`, "不存在"},
		{"rate limit", `toomanyrequests: You have reached your pull rate limit`, "限流（429）"},
		{"other", `connection reset by peer`, "拉取"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ClassifyPullError("limeight/ha-x", errors.New(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("classify(%q) = %v, want contain %q", tc.raw, err, tc.want)
			}
			// 原始错误必须保留（%w 链），不许吞掉细节。
			if !strings.Contains(err.Error(), tc.raw) {
				t.Fatalf("classified error must keep the raw detail: %v", err)
			}
		})
	}
	if ClassifyPullError("x", nil) != nil {
		t.Fatal("nil error must classify to nil")
	}
}
