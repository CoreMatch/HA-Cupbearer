package bootstrap

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// 生成值的键名集合（状态文件与运行记录共用）。这些键名本身不含机密，
// 可以安全地出现在任何响应里；值只以指纹形态出现。
const (
	KeySuperClientSecret     = "HRPAUTH_SUPER_CLIENT_SECRET"
	KeyWinnerProxySecret     = "WINNERPROXY_CLIENT_SECRET"
	KeyYGGSecret              = "YGG_CLIENT_SECRET"
	KeySkinProxySecret       = "HASKINPROXY_CLIENT_SECRET"
	KeySDKHandlerSecret      = "SDKHANDLER_CLIENT_SECRET"
	KeySkinLibSecret         = "HASKINLIB_CLIENT_SECRET"
	KeyManageToken           = "MANAGE_TOKEN"
	KeyMCAMaintenanceToken   = "MCA_MAINTENANCE_TOKEN"
	KeySDKHandlerInternalKey = "SDKHANDLER_INTERNAL_KEY"
	KeyAppDBPassword         = "DB_PASSWORD"
	// 基础设施机密：一键拉起 MySQL/Redis 容器时生成（按需，不进平台机密全集）。
	KeyMySQLRootPassword = "MYSQL_ROOT_PASSWORD"
	KeyRedisPassword     = "REDIS_PASSWORD"
)

// secretKeys 是全部需要生成的机密值（顺序即生成顺序）。
var secretKeys = []string{
	KeySuperClientSecret,
	KeyWinnerProxySecret,
	KeyYGGSecret,
	KeySkinProxySecret,
	KeySDKHandlerSecret,
	KeySkinLibSecret,
	KeyManageToken,
	KeyMCAMaintenanceToken,
	KeySDKHandlerInternalKey,
	KeyAppDBPassword,
}

// SecretKeyNames 返回机密键名列表（状态回显用；键名本身不含机密）。
func SecretKeyNames() []string {
	return append([]string(nil), secretKeys...)
}

// GenerateRandomToken 生成 32 字节（64 位十六进制）的随机令牌。
// 与生态内既有约定一致（HRPAuth utils.GenerateRandomToken(32) 同形态）。
func GenerateRandomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Fingerprint 是机密值的对外形态：sha256 前 16 位十六进制。
// 任何响应/计划/运行记录/日志里，机密值只以该形态出现，绝不出现明文。
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// Mask 返回机密值的展示形态：仅指纹（不泄露任何明文字符）。
func Mask(value string) string {
	if value == "" {
		return "(empty)"
	}
	return Fingerprint(value)
}

// maskMap 对一组机密值批量取指纹（运行记录/状态回显用）。
func maskMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = Mask(v)
	}
	return out
}
