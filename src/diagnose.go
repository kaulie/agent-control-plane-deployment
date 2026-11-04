package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Diagnosis 是一条失败流水线的只读结论。材料是已经落库的错误和事件；
// 启动失败且目标是本机时，再读 runtime 里启动脚本会 tail 的日志。
type Diagnosis struct {
	RequestID   string              `json:"requestId"`
	State       string              `json:"state"`
	Stage       string              `json:"stage"`
	Category    string              `json:"category"`
	Summary     string              `json:"summary"`
	Cause       string              `json:"cause"`
	Evidence    []DiagnosisEvidence `json:"evidence"`
	Suggestions []string            `json:"suggestions"`
	DiagnosedAt string              `json:"diagnosedAt"`
}

// DiagnosisEvidence 是结论引用的一条原文。Source 为 pipeline、deploy 或 log。
type DiagnosisEvidence struct {
	Source  string `json:"source"`
	Level   string `json:"level,omitempty"`
	Ts      string `json:"ts,omitempty"`
	Message string `json:"message"`
}

type diagEvent struct {
	Source  string
	Level   string
	Ts      string
	Message string
}

type diagInput struct {
	Job        PipelineJob
	Events     []diagEvent
	RuntimeDir string
	// ReadLogs 为真时才读本机 runtime。远端部署的目录不在这台机器上。
	ReadLogs bool
}

func (s *apiServer) diagnosePipeline(requestID string) (Diagnosis, int, string) {
	job, err := s.store.GetPipeline(requestID)
	if err != nil {
		return Diagnosis{}, 500, err.Error()
	}
	if job == nil {
		return Diagnosis{}, 404, "pipeline not found"
	}
	if job.State != PipelineFailed {
		return Diagnosis{}, 409, "只有失败的流水线可以诊断"
	}
	in := diagInput{Job: *job}
	if evs, err := s.store.ListPipelineEvents(requestID); err == nil {
		for _, ev := range evs {
			in.Events = append(in.Events, diagEvent{
				Source: "pipeline", Level: ev.Level, Ts: ev.Ts, Message: ev.Message,
			})
		}
	}
	if job.DeployRequestID != "" {
		if dep, err := s.store.GetDeploy(job.DeployRequestID); err == nil && dep != nil {
			if job.Error == "" {
				in.Job.Error = dep.Error
			}
			if evs, err := s.store.ListDeployEvents(job.DeployRequestID); err == nil {
				for _, ev := range evs {
					in.Events = append(in.Events, diagEvent{
						Source: "deploy", Level: ev.Level, Ts: ev.Ts, Message: ev.Message,
					})
				}
			}
		}
	}
	in.ReadLogs, in.RuntimeDir = localRuntimeForDiagnosis(s.store, *job)
	return diagnoseFrom(in), 200, ""
}

func localRuntimeForDiagnosis(store *Store, job PipelineJob) (bool, string) {
	machine := strings.TrimSpace(job.TargetMachine)
	// 只有本机部署才读 runtime。远端机器上的日志不在控制面这台机器上。
	if machine != "" && machine != "local" {
		return false, ""
	}
	if store == nil {
		return false, ""
	}
	svc, err := store.GetService(job.ServiceID)
	if err != nil || svc == nil {
		return false, ""
	}
	dir := strings.TrimSpace(svc.LocalRuntimeDir())
	if dir == "" {
		dir = strings.TrimSpace(svc.RuntimeDir)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false, ""
	}
	return true, dir
}

func diagnoseFrom(in diagInput) Diagnosis {
	errText := strings.TrimSpace(in.Job.Error)
	if errText == "" {
		errText = lastErrorEvent(in.Events)
	}
	d := classifyFailure(errText, in.Job.Message, in.Job.UseProxy)
	d.RequestID = in.Job.RequestID
	d.State = string(in.Job.State)
	d.DiagnosedAt = nowISO()
	d.Evidence = failureEvidence(in.Events)
	if d.Category == "start_failed" || d.Category == "restart_failed" {
		refineStartFailure(&d, errText, in)
	}
	if len(d.Evidence) > 12 {
		d.Evidence = d.Evidence[:12]
	}
	if d.Evidence == nil {
		d.Evidence = []DiagnosisEvidence{}
	}
	if d.Suggestions == nil {
		d.Suggestions = []string{}
	}
	return d
}

func classifyFailure(errText, message string, useProxy bool) Diagnosis {
	e := strings.TrimSpace(errText)
	low := strings.ToLower(e)
	switch {
	case strings.Contains(e, "unknown service"):
		return Diagnosis{
			Stage: "precheck", Category: "unknown_service",
			Summary: "流水线在预检阶段失败：服务不存在。",
			Cause:   "控制面找不到这个 serviceId 的部署配置。",
			Suggestions: []string{
				"到「服务契约」确认 serviceId 已经配置。",
			},
		}
	case strings.Contains(e, "gitRepoUrl"):
		return Diagnosis{
			Stage: "precheck", Category: "no_repo",
			Summary: "打包前发现服务没有 git 仓库地址。",
			Cause:   "gitRepoUrl 只来自 service_registry，本机配置改不了它。",
			Suggestions: []string{
				"到 service_registry 给这个服务登记 gitRepoUrl。",
			},
		}
	case strings.Contains(e, "runtimeDir（"):
		return Diagnosis{
			Stage: "precheck", Category: "runtime_dir",
			Summary: "目标机器还缺运行目录。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"按错误里的链接，在部署配置页补上该操作系统的绝对路径。",
			},
		}
	case strings.HasPrefix(e, "package failed:"):
		return classifyPackage(e, useProxy)
	case strings.HasPrefix(e, "enqueue deploy failed:"):
		return Diagnosis{
			Stage: "enqueue", Category: "enqueue",
			Summary: "包已经打好，但部署任务没有入队。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"看这条错误里的数据库或参数原文，修好后再重新发起流水线。",
			},
		}
	case strings.Contains(e, "no deploy channel"):
		return Diagnosis{
			Stage: "deploy", Category: "no_channel",
			Summary: "部署机器没有可用的部署通道。",
			Cause:   "机器可以出现在列表里，但没有配置 DEPLOY_MACHINE_TARGETS，平台不会真的部署过去。",
			Suggestions: []string{
				"为这台机器配置部署通道，或改选一台已有通道的机器后重新发起。",
			},
		}
	case strings.Contains(e, "no runtimeDir"):
		return Diagnosis{
			Stage: "deploy", Category: "no_runtime_dir",
			Summary: "部署契约里没有 runtimeDir。",
			Cause:   "没有运行目录，平台无法把制品同步过去。",
			Suggestions: []string{
				"在「服务契约」里补上 runtimeDir。",
			},
		}
	case strings.Contains(low, "download package"):
		return Diagnosis{
			Stage: "deploy", Category: "download",
			Summary: "制品下载失败。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"核对制品 tag 是否还在当前存储后端（GitHub Release 或阿里云）上。",
			},
		}
	case strings.Contains(low, "rsync failed") || strings.Contains(low, "remote stamp failed"):
		return Diagnosis{
			Stage: "deploy", Category: "rsync",
			Summary: "制品没有同步到目标机器。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"核对目标目录是否可写，远端的话再核对 SSH 通道。",
			},
		}
	case strings.Contains(e, "health check failed"):
		return Diagnosis{
			Stage: "deploy", Category: "health_check",
			Summary: "进程重启命令已经返回，但健康检查没有通过。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"核对契约里的 healthUrl、端口，以及启动命令是否真的把服务拉起来了。",
			},
		}
	case strings.Contains(low, "graceful"):
		return Diagnosis{
			Stage: "deploy", Category: "graceful",
			Summary: "优雅重启阶段失败。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"看部署事件里 notify / poll 的原文，确认业务侧 drain 是否超时。",
			},
		}
	case strings.Contains(e, "acp-upgrader"):
		return Diagnosis{
			Stage: "deploy", Category: "upgrader",
			Summary: "控制面自身升级没有交出去。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"先确认 acp-upgrader 进程在跑，再重新部署。",
			},
		}
	case strings.Contains(e, "VERSION(") && strings.Contains(e, "!= hash"):
		return Diagnosis{
			Stage: "deploy", Category: "version_mismatch",
			Summary: "装上去的版本和这次制品的 hash 不一致。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"核对包里的 VERSION 文件是不是这次构建写进去的 hash。",
			},
		}
	case strings.Contains(e, "package incomplete") || strings.Contains(e, "missing executable"):
		return Diagnosis{
			Stage: "deploy", Category: "package_incomplete",
			Summary: "制品不完整，平台无法启动。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"确认构建产物里有约定的可执行文件，或契约里写了 restartCmd。",
			},
		}
	case strings.HasPrefix(e, "start failed:") || strings.Contains(e, "start 失败"):
		return Diagnosis{
			Stage: "deploy", Category: "start_failed",
			Summary: "部署停在启动命令：start 脚本以非 0 退出。",
			Cause:   "启动命令的输出里没有单独的失败说明。",
			Suggestions: []string{
				"看该服务 runtime 下 backend/server.log，以及启动脚本失败时 tail 的业务日志。",
			},
		}
	case strings.HasPrefix(e, "restart failed:") || strings.Contains(e, "restart 失败"):
		return Diagnosis{
			Stage: "deploy", Category: "restart_failed",
			Summary: "部署停在重启命令：脚本以非 0 退出。",
			Cause:   "重启命令的输出里没有单独的失败说明。",
			Suggestions: []string{
				"看该服务 runtime 下 backend/server.log，以及脚本失败时打出的错误行。",
			},
		}
	default:
		stage := "deploy"
		if message == "pipeline failed" || strings.HasPrefix(e, "package failed:") {
			stage = "packaging"
		}
		summary := "这条失败没有落到已知类别。"
		if e == "" {
			summary = "流水线是失败状态，但没有留下错误原文。"
		}
		return Diagnosis{
			Stage: stage, Category: "uncategorized",
			Summary: summary,
			Cause:   firstNonEmpty(clipText(e, 400), "没有可引用的错误原文。"),
			Suggestions: []string{
				"看事件日志里最后一条 error 的原文。",
			},
		}
	}
}

func classifyPackage(e string, useProxy bool) Diagnosis {
	low := strings.ToLower(e)
	network := containsAny(low,
		"timeout", "timed out", "connection reset", "connection refused",
		"http2", "framing", "proxy", "i/o timeout", "could not resolve",
		"network is unreachable", "dial tcp", "tls handshake")
	git := containsAny(low,
		"git ", "git [", "fetch", "clone", "authentication",
		"repository not found", "could not read from remote")
	switch {
	case network && !useProxy:
		return Diagnosis{
			Stage: "packaging", Category: "proxy_off",
			Summary: "打包拉取仓库时网络失败，而且这次没有走本机代理。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"重新发起流水线，并勾选「打包走本机代理」。",
			},
		}
	case network && useProxy:
		return Diagnosis{
			Stage: "packaging", Category: "proxy_on",
			Summary: "打包已经走了本机代理，拉取仍然失败。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"检查 data/proxy.env 里的代理地址，以及本机代理进程是否在监听。",
			},
		}
	case git:
		return Diagnosis{
			Stage: "packaging", Category: "git_fetch",
			Summary: "打包时没有把 git ref 拉下来。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"核对 service_registry 里的仓库地址、访问权限，以及这次填写的 ref。",
			},
		}
	default:
		return Diagnosis{
			Stage: "packaging", Category: "build",
			Summary: "打包命令失败。",
			Cause:   clipText(e, 400),
			Suggestions: []string{
				"看这条错误里构建输出的结尾，修构建后再重新发起。",
			},
		}
	}
}

func refineStartFailure(d *Diagnosis, errText string, in diagInput) {
	body := errText
	for _, p := range []string{"start failed:", "restart failed:"} {
		if strings.HasPrefix(body, p) {
			body = strings.TrimSpace(strings.TrimPrefix(body, p))
		}
	}
	banners := bannerLines(body)
	from, to, dur, haveDur := startWindow(in.Events, in.Job.FinishedAt)

	var logHits []DiagnosisEvidence
	var sawListen, sawWindow bool
	if in.ReadLogs && in.RuntimeDir != "" && !to.IsZero() {
		logHits, sawListen, sawWindow = scanStartLogs(in.RuntimeDir, from, to)
		for _, hit := range logHits {
			if strings.Contains(hit.Message, "[start][错误]") || strings.Contains(hit.Message, "[restart][错误]") {
				banners = append(banners, hit.Message)
			}
		}
	}

	switch {
	case platformTimedOut(body):
		d.Summary = "启动命令超过平台等待时间，被终止。"
		d.Cause = "平台给启动命令的等待时间到了（输出以 timeout 结尾），进程被杀掉。这和脚本自己的健康检查不是同一次超时。"
		d.Suggestions = []string{
			"看启动脚本是否在前台一直不返回。约定是后台拉起后立刻写 pid 并返回。",
		}
	case len(banners) > 0:
		d.Summary = "启动脚本自己打出了失败原因。"
		d.Cause = strings.Join(uniqueStrings(banners), "；")
		d.Suggestions = startBannerSuggestions(d.Cause)
	case logTrace(logHits) != "":
		d.Summary = "启动命令失败，运行日志里有异常栈。"
		d.Cause = logTrace(logHits)
		d.Suggestions = []string{
			"按日志里的 Traceback 修服务本身，再重新部署。",
		}
	case looksLikeLogTail(body):
		d.Summary = "启动命令非 0 退出。平台只保留了输出结尾，失败说明不在里面。"
		d.Cause = clippedStartCause(haveDur, dur, sawListen, sawWindow, len(logHits) > 0)
		d.Suggestions = []string{
			"不要把这段 INFO 访问日志当成原因。它多半是失败时 tail 出来的旧日志，开头的 [start][错误] 被 2000 字截断丢掉了。",
			"看 runtime 的 backend/server.log，以及脚本里配置的业务日志，对一下从执行 start 到失败的这段时间。",
		}
		if !in.ReadLogs {
			d.Suggestions = append(d.Suggestions, "这次不是本机部署，控制面没有去读目标机器上的日志。")
		}
	default:
		if body != "" {
			d.Cause = clipText(body, 400)
		}
	}

	if len(logHits) > 0 {
		d.Evidence = append(logHits, d.Evidence...)
	}
	d.Evidence = shrinkStartEvidence(d.Evidence)
}

func clippedStartCause(haveDur bool, dur time.Duration, sawListen, sawWindow, sawLogHit bool) string {
	switch {
	case sawListen:
		return "运行日志里已经有监听记录，但启动命令仍以非 0 退出。保留下来的命令输出只是日志结尾，没有 [start][错误]。"
	case haveDur && dur >= 28*time.Second && sawWindow && !sawLogHit:
		return "从执行启动命令到失败大约 " + dur.Round(time.Second).String() +
			"。这段时间里日志没有 Traceback，也没有打出「on :端口」这样的监听行。" +
			"按启动脚本的约定，这是 30 秒内 /health 一直没就绪，脚本退出并杀掉了还没开始监听的进程。"
	case haveDur && dur >= 28*time.Second && !sawWindow:
		return "从执行启动命令到失败大约 " + dur.Round(time.Second).String() +
			"。命令输出只剩日志尾巴，没有 [start][错误]。本机日志里没有这次启动的时间窗（文件太大或已经轮转），不能从日志确认进程有没有开始监听。"
	case haveDur && dur >= 28*time.Second:
		return "从执行启动命令到失败大约 " + dur.Round(time.Second).String() +
			"。命令输出只剩日志尾巴。这个时长符合启动脚本等 /health 约 30 秒后主动退出。"
	case haveDur:
		return "启动命令在大约 " + dur.Round(time.Second).String() +
			" 后非 0 退出。输出只剩日志尾巴，没有 [start][错误] 或 Traceback。"
	default:
		return "命令输出只剩业务日志的结尾，没有 [start][错误]、Traceback 或 timeout。平台只保留最后 2000 字，真正的失败句在被丢掉的前半段。"
	}
}

func startBannerSuggestions(cause string) []string {
	switch {
	case strings.Contains(cause, "未就绪"):
		return []string{
			"进程在超时前没有把 /health 准备好。对照业务日志看它是否还在导入、还没执行到监听。",
			"启动脚本的等待大约 30 秒。启动本身接近这个时间时，会在开始监听前被脚本杀掉。",
		}
	case strings.Contains(cause, "进程已退出"):
		return []string{
			"进程在健康检查通过前就退出了。看同一次失败附带的日志里有没有 Traceback。",
		}
	case strings.Contains(cause, "端口") && strings.Contains(cause, "占用"):
		return []string{
			"约定端口上还有别的进程。先停掉占用者，再重新部署。",
		}
	default:
		return []string{
			"按启动脚本打出的这句错误处理，然后重新部署。",
		}
	}
}

func failureEvidence(events []diagEvent) []DiagnosisEvidence {
	var picked []diagEvent
	for _, level := range []string{"error", "warn"} {
		for i := len(events) - 1; i >= 0 && len(picked) < 6; i-- {
			if events[i].Level == level && strings.TrimSpace(events[i].Message) != "" {
				picked = append(picked, events[i])
			}
		}
	}
	// 恢复成时间顺序。
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	out := make([]DiagnosisEvidence, 0, len(picked))
	for _, ev := range picked {
		out = append(out, DiagnosisEvidence{
			Source:  ev.Source,
			Level:   ev.Level,
			Ts:      ev.Ts,
			Message: presentEventMessage(ev.Message),
		})
	}
	return out
}

func presentEventMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if looksLikeLogTail(msg) {
		return "命令输出已被截断，结尾是业务日志而不是错误行：" + clipText(lastNonEmptyLine(msg), 180)
	}
	return clipText(msg, 500)
}

func shrinkStartEvidence(in []DiagnosisEvidence) []DiagnosisEvidence {
	out := make([]DiagnosisEvidence, 0, len(in))
	seen := map[string]bool{}
	for _, ev := range in {
		key := ev.Source + "\n" + ev.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ev)
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func looksLikeLogTail(text string) bool {
	if strings.Contains(text, "[start][错误]") || strings.Contains(text, "[restart][错误]") || strings.Contains(text, "Traceback") {
		return false
	}
	return strings.Contains(text, "| INFO |") || strings.Contains(text, "HTTP/1.1")
}

func platformTimedOut(text string) bool {
	line := lastNonEmptyLine(text)
	return line == "timeout"
}

func bannerLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "[start][错误]") || strings.Contains(line, "[restart][错误]") {
			out = append(out, clipText(line, 300))
		}
	}
	return out
}

func logTrace(hits []DiagnosisEvidence) string {
	for _, hit := range hits {
		if strings.Contains(hit.Message, "Traceback") || strings.Contains(hit.Message, "Exception") {
			return clipText(hit.Message, 300)
		}
	}
	return ""
}

func lastErrorEvent(events []diagEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Level == "error" && strings.TrimSpace(events[i].Message) != "" {
			return events[i].Message
		}
	}
	return ""
}

func startWindow(events []diagEvent, finishedAt string) (from, to time.Time, dur time.Duration, ok bool) {
	var start time.Time
	var fail time.Time
	for _, ev := range events {
		ts, parsed := parseEventTime(ev.Ts)
		if !parsed {
			continue
		}
		if strings.Contains(ev.Message, "执行 startCmd") || strings.Contains(ev.Message, "执行 restartCmd") {
			if start.IsZero() {
				start = ts
			}
		}
		if strings.Contains(ev.Message, "start 失败") || strings.Contains(ev.Message, "restart 失败") || ev.Level == "error" {
			fail = ts
		}
	}
	if fail.IsZero() {
		if ts, parsed := parseEventTime(finishedAt); parsed {
			fail = ts
		}
	}
	if start.IsZero() || fail.IsZero() || !fail.After(start) {
		if !fail.IsZero() {
			return fail.Add(-2 * time.Minute), fail, 0, false
		}
		return time.Time{}, time.Time{}, 0, false
	}
	return start, fail, fail.Sub(start), true
}

func parseEventTime(ts string) (time.Time, bool) {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func scanStartLogs(runtimeDir string, from, to time.Time) ([]DiagnosisEvidence, bool, bool) {
	var hits []DiagnosisEvidence
	sawListen := false
	sawWindow := false
	for _, path := range startLogFiles(runtimeDir) {
		fileHits, listen, window := scanOneLog(path, from, to)
		if listen {
			sawListen = true
		}
		if window {
			sawWindow = true
		}
		hits = append(hits, fileHits...)
		if len(hits) >= 8 {
			hits = hits[:8]
			break
		}
	}
	return hits, sawListen, sawWindow
}

func startLogFiles(runtimeDir string) []string {
	var out []string
	add := func(path string) {
		path = filepath.Clean(path)
		if path == "" {
			return
		}
		for _, existing := range out {
			if existing == path {
				return
			}
		}
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			return
		}
		out = append(out, path)
	}
	add(filepath.Join(runtimeDir, "backend", "server.log"))
	envPath := filepath.Join(runtimeDir, "backend", ".env")
	raw, err := os.ReadFile(envPath)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || !logPathKey(key) {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if !filepath.IsAbs(val) {
			continue
		}
		st, err := os.Stat(val)
		if err != nil {
			continue
		}
		if st.IsDir() {
			preferred := filepath.Join(val, "brain.log")
			if st2, err := os.Stat(preferred); err == nil && st2.Mode().IsRegular() {
				add(preferred)
				continue
			}
			matches, _ := filepath.Glob(filepath.Join(val, "*.log"))
			best := ""
			var bestTime time.Time
			for _, m := range matches {
				st3, err := os.Stat(m)
				if err != nil || !st3.Mode().IsRegular() {
					continue
				}
				if best == "" || st3.ModTime().After(bestTime) {
					best = m
					bestTime = st3.ModTime()
				}
			}
			add(best)
			continue
		}
		add(val)
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func logPathKey(key string) bool {
	key = strings.ToUpper(strings.TrimSpace(key))
	if !strings.Contains(key, "LOG") {
		return false
	}
	return !strings.Contains(key, "LEVEL")
}

func scanOneLog(path string, from, to time.Time) ([]DiagnosisEvidence, bool, bool) {
	// 一天的业务日志可以到十几 MB。从末尾读 16MB，覆盖当次启动前后，而不是只看最后几百行。
	data := readTail(path, 16*1024*1024)
	lines := strings.Split(string(data), "\n")
	var hits []DiagnosisEvidence
	sawListen := false
	sawWindow := false
	inTrace := false
	traceLines := 0
	windowStart := from.Add(-2 * time.Second)
	windowEnd := to.Add(2 * time.Second)
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		ts, hasTS := parseLogTime(trim)
		inWindow := hasTS && !ts.Before(windowStart) && !ts.After(windowEnd)
		if inWindow {
			sawWindow = true
		}
		if hasTS && !inWindow {
			inTrace = false
			continue
		}
		if !hasTS && !inTrace {
			continue
		}
		if inWindow && listenLine(trim) {
			sawListen = true
		}
		if strings.Contains(trim, "Traceback (most recent call last)") {
			inTrace = true
			traceLines = 0
		}
		keep := false
		switch {
		case inTrace:
			keep = true
			traceLines++
			if traceLines > 8 {
				inTrace = false
			}
		case strings.Contains(trim, "[start][错误]") || strings.Contains(trim, "[restart][错误]"):
			keep = true
		case strings.Contains(trim, "未就绪") || strings.Contains(trim, "进程已退出"):
			keep = true
		case logErrorLine(trim):
			keep = true
		}
		if !keep {
			continue
		}
		ev := DiagnosisEvidence{Source: "log", Level: "error", Message: clipText(trim, 300)}
		if hasTS {
			ev.Ts = ts.Format(time.RFC3339)
		}
		if len(hits) < 8 {
			hits = append(hits, ev)
		}
	}
	return hits, sawListen, sawWindow
}

func listenLine(line string) bool {
	return strings.Contains(line, " on :") || strings.Contains(line, "Running on") || strings.Contains(line, "Listening")
}

func logErrorLine(line string) bool {
	return strings.Contains(line, "| ERROR |") ||
		strings.Contains(line, "| CRITICAL |") ||
		strings.Contains(line, " ERROR ") ||
		strings.Contains(line, "Exception:")
}

func parseLogTime(line string) (time.Time, bool) {
	if len(line) < 19 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", line[:19], time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func readTail(path string, max int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil
	}
	start := int64(0)
	if st.Size() > int64(max) {
		start = st.Size() - int64(max)
	}
	buf := make([]byte, st.Size()-start)
	_, err = f.ReadAt(buf, start)
	if err != nil && len(buf) == 0 {
		return nil
	}
	return buf
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func uniqueStrings(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func clipText(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…"
}
