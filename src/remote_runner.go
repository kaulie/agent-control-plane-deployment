package main

import (
	"fmt"
	"strconv"
	"strings"
)

// RemoteRunner 是「在远端机器上干活」的最小接口：远端执行一条 shell、把目录推过去、
// 以及用**远端自己的** 127.0.0.1 URL 发一次 HTTP（graceful 通知/轮询、健康检查）。
//
// 抽成接口是为了测试能注入假实现（真实实现见 remote_ssh.go：ssh / rsync / ssh+curl）。
type RemoteRunner interface {
	// Run 在远端执行一段 shell，返回退出码与输出。
	Run(target MachineTarget, script string, timeoutSec int) (code int, output string)
	// PushDir 把本地目录的内容同步到远端目录（deleteExtra = 是否 --delete）。
	PushDir(target MachineTarget, src, dst string, deleteExtra bool, timeoutSec int) (code int, output string)
	// HTTP 用远端的 127.0.0.1 地址发一次请求（在远端跑 curl），返回状态码与响应体。
	HTTP(target MachineTarget, method, rawURL, body string, timeoutSec int) (code int, respBody string)
}

// sshRemoteRunner 是 RemoteRunner 的真实实现：本机 ssh / rsync / ssh+curl。
//
// 前置要求（目标机器）：能免密 ssh 登录、装有 curl。两条都在部署前用一次 preflight
// 检查（见 worker.go 的 remotePreflight），失败会带着原因直接判这次部署失败。
type sshRemoteRunner struct {
	// home 是本地工作目录（runShell 的 cwd）。
	home string
}

func (r sshRemoteRunner) sshArgs(target MachineTarget) []string {
	args := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if target.SSHPort > 0 {
		args = append(args, "-p", strconv.Itoa(target.SSHPort))
	}
	return append(args, target.SSHDest())
}

func (r sshRemoteRunner) Run(target MachineTarget, script string, timeoutSec int) (int, string) {
	cmd := strings.Join(append(r.sshArgs(target), shellQuote(script)), " ")
	res := runShell(cmd, r.home, nil, timeoutSec)
	return res.Code, res.Output
}

func (r sshRemoteRunner) PushDir(target MachineTarget, src, dst string, deleteExtra bool, timeoutSec int) (int, string) {
	sshCmd := "ssh -o BatchMode=yes -o ConnectTimeout=10"
	if target.SSHPort > 0 {
		sshCmd += " -p " + strconv.Itoa(target.SSHPort)
	}
	parts := []string{"rsync", "-a"}
	if deleteExtra {
		parts = append(parts, "--delete")
	}
	parts = append(parts,
		// 远端同样要保住它自己的运行态（与本地部署同一套豁免）。
		"--filter='P backend/.env'",
		"--filter='P backend/data/'",
		"--filter='P backend/runtime.pid'",
		"--filter='P backend/server.log'",
		"--filter='P backend/.watchdog-paused'",
		"--filter='P data/'",
		"--filter='P logs/'",
		"--filter='P packages/'",
		"--exclude='.git/'",
		"-e", shellQuote(sshCmd),
		fmt.Sprintf("%q/", src),
		shellQuote(target.SSHDest()+":"+dst+"/"),
	)
	res := runShell(strings.Join(parts, " "), r.home, nil, timeoutSec)
	return res.Code, res.Output
}

func (r sshRemoteRunner) HTTP(target MachineTarget, method, rawURL, body string, timeoutSec int) (int, string) {
	if timeoutSec <= 0 {
		timeoutSec = 5
	}
	curl := []string{"curl", "-sS", "-m", strconv.Itoa(timeoutSec), "-X", method}
	if body != "" {
		curl = append(curl, "-H", shellQuote("content-type: application/json"), "-d", shellQuote(body))
	}
	curl = append(curl, "-w", shellQuote("\n%{http_code}"), shellQuote(rawURL))
	script := strings.Join(curl, " ")
	code, out := r.Run(target, script, timeoutSec+10)
	if code != 0 {
		// curl 自身失败（连不上等）：把退出码与输出原样带回去，由调用方判定。
		return 0, strings.TrimSpace(out)
	}
	// 最后一行是 HTTP 状态码，其余部分是响应体。
	trimmed := strings.TrimRight(out, "\n")
	idx := strings.LastIndexByte(trimmed, '\n')
	if idx < 0 {
		status, err := strconv.Atoi(strings.TrimSpace(trimmed))
		if err != nil {
			return 0, strings.TrimSpace(out)
		}
		return status, ""
	}
	status, err := strconv.Atoi(strings.TrimSpace(trimmed[idx+1:]))
	if err != nil {
		return 0, strings.TrimSpace(out)
	}
	return status, strings.TrimSpace(trimmed[:idx])
}

// shellQuote 用单引号包住一段脚本/参数（内部的单引号按 POSIX 的转义写法处理）。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
