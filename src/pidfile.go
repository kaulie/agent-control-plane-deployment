package main

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

// runtimePidRel 是约定的 pidfile，相对 runtime 目录。
// start.sh：nohup 服务 &；echo $! > "$RUNTIME_DIR/backend/runtime.pid"（不要 double-fork）。
const runtimePidRel = "backend/runtime.pid"

func runtimePidPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, filepath.FromSlash(runtimePidRel))
}

func parsePIDLine(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// 只取第一行、纯数字，避免把日志误当成 pid。
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return ""
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return ""
		}
	}
	return s
}

func readRuntimePIDLocal(runtimeDir string) string {
	b, err := os.ReadFile(runtimePidPath(runtimeDir))
	if err != nil {
		return ""
	}
	return parsePIDLine(string(b))
}

func readRuntimePIDScript(runtimeDir string) string {
	pf := shellQuote(runtimePidPath(runtimeDir))
	return "if [ -f " + pf + " ]; then tr -d '[:space:]' < " + pf + "; fi"
}

// platformStopScript 按约定停进程：先 pidfile，再 SERVICE_PORT 上的 LISTEN。
// 退出码恒 0（没有进程也算成功，部署不因此失败）。
func platformStopScript(runtimeDir, port string) string {
	rd := shellQuote(runtimeDir)
	p := shellQuote(port)
	return strings.Join([]string{
		"rd=" + rd,
		"port=" + p,
		`pidfile="$rd/` + runtimePidRel + `"`,
		`kill_pid() {`,
		`  local p="$1"`,
		`  [ -n "$p" ] || return 0`,
		`  kill -0 "$p" 2>/dev/null || return 0`,
		`  kill "$p" 2>/dev/null || true`,
		`  local i`,
		`  for i in $(seq 1 10); do`,
		`    kill -0 "$p" 2>/dev/null || return 0`,
		`    sleep 0.2`,
		`  done`,
		`  kill -9 "$p" 2>/dev/null || true`,
		`}`,
		`stopped=""`,
		`if [ -f "$pidfile" ]; then`,
		`  pid="$(tr -d '[:space:]' < "$pidfile" || true)"`,
		`  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then`,
		`    kill_pid "$pid"`,
		`    echo "stopped pidfile pid=$pid"`,
		`    stopped=1`,
		`  else`,
		`    echo "stale pidfile pid=${pid:-?}"`,
		`  fi`,
		`  rm -f "$pidfile"`,
		`fi`,
		`if command -v lsof >/dev/null 2>&1 && [ -n "$port" ]; then`,
		`  lp="$(lsof -nP -iTCP:$port -sTCP:LISTEN -t 2>/dev/null | head -1 || true)"`,
		`  if [ -n "$lp" ] && kill -0 "$lp" 2>/dev/null; then`,
		`    echo "stopped listener pid=$lp port=$port"`,
		`    kill_pid "$lp"`,
		`    stopped=1`,
		`  fi`,
		`fi`,
		`if [ -z "$stopped" ]; then`,
		`  echo "not running"`,
		`fi`,
	}, "\n")
}

func runPlatformStop(remote RemoteRunner, target MachineTarget, runtimeDir, port string, timeoutSec int) (int, string) {
	script := platformStopScript(runtimeDir, port)
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	if target.Remote() {
		return remote.Run(target, script, timeoutSec)
	}
	res := runShell(script, runtimeDir, nil, timeoutSec)
	return res.Code, res.Output
}

func readRuntimePID(remote RemoteRunner, target MachineTarget, runtimeDir string) string {
	if target.Remote() {
		_, out := remote.Run(target, readRuntimePIDScript(runtimeDir), 10)
		return parsePIDLine(out)
	}
	return readRuntimePIDLocal(runtimeDir)
}

func reportStartedPID(store *Store, job DeployJob, target MachineTarget, remote RemoteRunner, runtimeDir string) {
	pid := readRuntimePID(remote, target, runtimeDir)
	if pid == "" {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Warn,
			"start 未写入 "+runtimePidRel+"（请用 nohup 服务 &；echo $! > 该文件，不要 double-fork）。下次平台 stop 将只靠 SERVICE_PORT")
		return
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
		"服务已启动 pid="+pid+"（"+runtimePidRel+"）")
}
