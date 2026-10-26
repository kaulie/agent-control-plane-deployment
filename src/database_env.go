package main

import (
	"fmt"
	"strings"
)

// DatabaseConnection 是部署契约里的数据库连接。启动 start/stop/restart 时，
// 平台把 DSN 注入到名为 Env 的环境变量（本机与远端同一份）。
// 两者都空表示不注入。连接串可能是密钥，部署时间线只记变量名。
type DatabaseConnection struct {
	Env string `json:"env,omitempty"`
	DSN string `json:"dsn,omitempty"`
}

// NormalizeDatabase 校验数据库连接。空连接 → 零值。只填了变量名或只填了连接串、
// 变量名非法、或连接串含换行时返回错误。
func NormalizeDatabase(in DatabaseConnection) (DatabaseConnection, error) {
	env := strings.TrimSpace(in.Env)
	dsn := strings.TrimSpace(in.DSN)
	if env == "" && dsn == "" {
		return DatabaseConnection{}, nil
	}
	if env == "" || dsn == "" {
		return DatabaseConnection{}, fmt.Errorf("数据库连接要同时填写环境变量名和连接串")
	}
	if _, err := NormalizeRuntimeEnv(map[string]string{env: dsn}); err != nil {
		return DatabaseConnection{}, fmt.Errorf("%s", strings.Replace(err.Error(), "运行时环境变量", "数据库连接", 1))
	}
	return DatabaseConnection{Env: env, DSN: dsn}, nil
}

// usableDatabase 是启动时实际注入的那一份。已保存的配置应当能通过
// NormalizeDatabase；库里的脏数据丢掉，不让一条坏连接挡住启动。
func usableDatabase(in DatabaseConnection) DatabaseConnection {
	out, err := NormalizeDatabase(in)
	if err != nil {
		return DatabaseConnection{}
	}
	return out
}

// applyDatabaseEnv 把数据库连接写入 env。调用方应在通用 runtimeEnv 之后、
// 平台键之前调用，这样连接串盖过同名的运行时环境变量，平台键仍然最后生效。
func applyDatabaseEnv(env map[string]string, service ServiceContract) {
	db := usableDatabase(service.Database)
	if db.Env == "" || isPlatformInjectedEnv(db.Env) {
		return
	}
	env[db.Env] = db.DSN
}

// databaseEnvConflict 拒绝「同一变量名既在运行时环境变量里、又是数据库连接」。
// 两处同时生效时，启动用哪一份不直观。
func databaseEnvConflict(db DatabaseConnection, runtimeEnv map[string]string) error {
	if db.Env == "" {
		return nil
	}
	if _, ok := runtimeEnv[db.Env]; ok {
		return fmt.Errorf("数据库连接的环境变量 %s 已在运行时环境变量里，请只保留一处", db.Env)
	}
	return nil
}
