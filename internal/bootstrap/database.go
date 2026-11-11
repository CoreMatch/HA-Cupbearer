package bootstrap

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// 数据库步骤：建库建账号。全部使用幂等语句
//（CREATE DATABASE IF NOT EXISTS / CREATE USER IF NOT EXISTS），
// 重跑不重复建、不动已有库。迁移不在本层执行：四个服务的迁移各自
// 启动时自动跑（facts.migration.mode=auto_on_startup）——本层只保证
// "库在、账号在、连得上"，服务首次启动自然触发迁移。

// DBResult 是单个库/账号的处理结论。
type DBResult struct {
	Name    string `json:"name"`
	Created bool   `json:"created"` // false = 已存在（幂等跳过）
}

// adminDSN 组装管理口 DSN（不指定库名）。
func adminDSN(in *Inputs) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/?timeout=10s&readTimeout=15s&writeTimeout=15s",
		in.Database.AdminUser, in.Database.AdminPassword,
		net.JoinHostPort(in.Database.AdminHost, strconv.Itoa(in.Database.AdminPort)))
}

// appDSN 组装应用账号对指定库的 DSN（连通性验证用）。
func appDSN(in *Inputs, dbName, appPassword string) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?timeout=10s",
		in.Database.AppUser, appPassword,
		net.JoinHostPort(in.Database.AdminHost, strconv.Itoa(in.Database.AdminPort)), dbName)
}

// EnsureDatabases 建库（幂等）。返回逐库结论。
// 管理口连不上 = 硬失败（显式失败，不静默跳过）。
func EnsureDatabases(ctx context.Context, in *Inputs) ([]DBResult, error) {
	db, err := sql.Open("mysql", adminDSN(in))
	if err != nil {
		return nil, fmt.Errorf("open admin connection: %w", err)
	}
	defer db.Close()
	db.SetConnMaxLifetime(time.Minute)

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("MySQL 管理口不可达（%s:%d，账号 %s）：%w",
			in.Database.AdminHost, in.Database.AdminPort, in.Database.AdminUser, err)
	}

	names := []string{
		in.Database.Names.HRPAuth,
		in.Database.Names.YGG,
		in.Database.Names.HASkinLib,
		in.Database.Names.MCA,
	}
	results := make([]DBResult, 0, len(names))
	for _, name := range names {
		exists, err := databaseExists(ctx, db, name)
		if err != nil {
			return nil, fmt.Errorf("check database %s: %w", name, err)
		}
		if !exists {
			// 库名已过 validIdentifier 校验（字母开头、仅字母数字下划线），
			// 反引号包裹防意外；绝不拼接未校验输入。
			stmt := fmt.Sprintf(
				"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", name)
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return nil, fmt.Errorf("create database %s: %w", name, err)
			}
		}
		results = append(results, DBResult{Name: name, Created: !exists})
	}
	return results, nil
}

// EnsureAppUser 建应用账号并授权（幂等）。口令用生成的 DB_PASSWORD。
func EnsureAppUser(ctx context.Context, in *Inputs, appPassword string) (created bool, err error) {
	db, err := sql.Open("mysql", adminDSN(in))
	if err != nil {
		return false, fmt.Errorf("open admin connection: %w", err)
	}
	defer db.Close()

	user := in.Database.AppUser
	exists, err := userExists(ctx, db, user)
	if err != nil {
		return false, fmt.Errorf("check user %s: %w", user, err)
	}
	if !exists {
		stmt := fmt.Sprintf("CREATE USER IF NOT EXISTS `%s`@`%%` IDENTIFIED BY '%s'",
			user, escapeSQLString(appPassword))
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return false, fmt.Errorf("create user %s: %w", user, err)
		}
	}
	// 授权恒做（GRANT 幂等）：已有账号可能缺新库的权限。
	for _, name := range []string{
		in.Database.Names.HRPAuth, in.Database.Names.YGG,
		in.Database.Names.HASkinLib, in.Database.Names.MCA,
	} {
		stmt := fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO `%s`@`%%`", name, user)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return false, fmt.Errorf("grant %s to %s: %w", name, user, err)
		}
	}
	if _, err := db.ExecContext(ctx, "FLUSH PRIVILEGES"); err != nil {
		return false, fmt.Errorf("flush privileges: %w", err)
	}
	return !exists, nil
}

// VerifyAppAccess 用应用账号逐库验证连通（建完即验，不等到服务启动才暴露问题）。
func VerifyAppAccess(ctx context.Context, in *Inputs, appPassword string) error {
	for _, name := range []string{
		in.Database.Names.HRPAuth, in.Database.Names.YGG,
		in.Database.Names.HASkinLib, in.Database.Names.MCA,
	} {
		db, err := sql.Open("mysql", appDSN(in, name, appPassword))
		if err != nil {
			return fmt.Errorf("open %s as app user: %w", name, err)
		}
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return fmt.Errorf("应用账号 %s 连不上库 %s：%w", in.Database.AppUser, name, err)
		}
		db.Close()
	}
	return nil
}

func databaseExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRowContext(ctx,
		"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).
		Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func userExists(ctx context.Context, db *sql.DB, user string) (bool, error) {
	var found string
	err := db.QueryRowContext(ctx,
		"SELECT User FROM mysql.user WHERE User = ?", user).
		Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// escapeSQLString 兜底转义（口令为随机十六进制，正常不会含特殊字符）。
func escapeSQLString(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '\\':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
