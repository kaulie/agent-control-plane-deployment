package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

// envExampleCandidates 是首次部署时用来生成 backend/.env 的模板（按优先级）。
// 制品 rsync 永远 --exclude backend/.env（保护远端密钥），所以新机器上这个文件
// 一开始不存在；服务的 start.sh 往往因此直接失败。模板在包里、会被 rsync 带过去。
var envExampleCandidates = []string{
	"backend/.env.example",
	"server/.env.example",
	".env.example",
}

// seedMissingBackendEnv 仅在 backend/.env 不存在时，从包里的 example 拷一份过去。
// 已有文件绝不覆盖（那是这台机器自己的密钥）。返回用上的模板相对路径；没拷则 "".
func seedMissingBackendEnv(runtimeDir string) (string, error) {
	runtimeDir = strings.TrimSpace(runtimeDir)
	if runtimeDir == "" {
		return "", nil
	}
	dest := filepath.Join(runtimeDir, "backend", ".env")
	if _, err := os.Stat(dest); err == nil {
		return "", nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	for _, cand := range envExampleCandidates {
		src := filepath.Join(runtimeDir, filepath.FromSlash(cand))
		st, err := os.Stat(src)
		if err != nil || st.IsDir() {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", err
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return "", err
		}
		return cand, nil
	}
	return "", nil
}

// seedMissingBackendEnvRemoteScript 在远端做同一件事：backend/.env 已在就跳过，
// 否则从 example 拷一份。退出码恒为 0（没有模板也不算失败，交给 start.sh 自己报）。
func seedMissingBackendEnvRemoteScript(runtimeDir string) string {
	rd := shellQuote(runtimeDir)
	lines := []string{
		"rd=" + rd,
		`if [ -f "$rd/backend/.env" ]; then echo 'backend/.env already present'; exit 0; fi`,
		`mkdir -p "$rd/backend"`,
		`for cand in ` + strings.Join(envExampleCandidates, " ") + `; do`,
		`  if [ -f "$rd/$cand" ]; then`,
		`    cp "$rd/$cand" "$rd/backend/.env"`,
		`    chmod 600 "$rd/backend/.env" 2>/dev/null || true`,
		`    echo "seeded backend/.env from $cand (first deploy)"`,
		`    exit 0`,
		`  fi`,
		`done`,
		`echo 'no env template to seed'`,
	}
	return strings.Join(lines, "\n")
}

// seedRuntimeEnvIfMissing 在 rsync 之后、restart 之前跑：新机器上 backend/.env
// 还不存在时，用包里的 example 生成一份。已有文件不动。失败只记 warn，不阻断部署。
func seedRuntimeEnvIfMissing(store *Store, job DeployJob, target MachineTarget, remote RemoteRunner, runtimeDir string) {
	if target.Remote() {
		code, out := remote.Run(target, seedMissingBackendEnvRemoteScript(runtimeDir), 30)
		out = strings.TrimSpace(out)
		if code != 0 {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Warn,
				"首次部署补 backend/.env 失败（继续）："+out)
			return
		}
		if strings.Contains(out, "seeded backend/.env") {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
				"首次部署：runtime 还没有 backend/.env，已从模板写入（"+out+"）")
		}
		return
	}
	src, err := seedMissingBackendEnv(runtimeDir)
	if err != nil {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Warn,
			"首次部署补 backend/.env 失败（继续）："+err.Error())
		return
	}
	if src != "" {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
			"首次部署：runtime 还没有 backend/.env，已从 "+src+" 拷贝一份（请按需补密钥）")
	}
}
