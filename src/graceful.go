package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

const gracefulPollInterval = 15 * time.Second

// gracefulHTTPClient is used for restart notify/poll against the project's
// own runtime. It disables HTTP keep-alives so no idle TCP connection to the
// project port lingers after the call. This matters because some projects'
// stop.sh clean up their port with `kill $(lsof -ti:$PORT)`, which would
// otherwise also kill THIS deployment process (which holds an idle keep-alive
// connection to that port from polling). With keep-alives disabled the
// connection closes as soon as each request finishes, so the project's
// port-cleanup no longer sweeps the deployment service up.
var gracefulHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DisableKeepAlives:   true,
		MaxIdleConnsPerHost: -1,
	},
}

// SupportsGracefulRestart reports whether the service registered both
// project-provided notify + poll endpoints.
func (s ServiceContract) SupportsGracefulRestart() bool {
	return strings.TrimSpace(s.RestartNotifyURL) != "" &&
		strings.TrimSpace(s.RestartPollURL) != ""
}

func (s ServiceContract) gracefulMaxWait(cfg Config) time.Duration {
	if s.GracefulMaxWaitMs > 0 {
		return time.Duration(s.GracefulMaxWaitMs) * time.Millisecond
	}
	return cfg.GracefulMaxWait
}

type restartNotifyBody struct {
	ServiceID  string `json:"serviceId"`
	RequestID  string `json:"requestId"`
	Deployment string `json:"deployment"`
	Version    string `json:"version,omitempty"`
	Message    string `json:"message"`
}

type restartPollStatus struct {
	CanRestart bool `json:"canRestart"`
	CanDeploy  bool `json:"canDeploy"`
	Ready      bool `json:"ready"`
}

func pollAllowsDeploy(st restartPollStatus) bool {
	return st.CanRestart || st.CanDeploy || st.Ready
}

func postRestartNotify(notifyURL string, body restartNotifyBody) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	client := gracefulHTTPClient
	req, err := http.NewRequest(http.MethodPost, notifyURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Close = true
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("notify returned HTTP %d", res.StatusCode)
	}
	return nil
}

// gracefulTransport 负责把「通知」与「轮询」发出去：本机直接 HTTP（localTransport），
// 远端部署则在**远端**跑 curl（restartNotifyUrl/restartPollUrl 是 127.0.0.1，只有站在
// 那台机器上问才对）。
type gracefulTransport interface {
	notify(notifyURL string, body restartNotifyBody) error
	poll(pollURL string) (restartPollStatus, error)
}

type localTransport struct{}

func (localTransport) notify(notifyURL string, body restartNotifyBody) error {
	return postRestartNotify(notifyURL, body)
}

func (localTransport) poll(pollURL string) (restartPollStatus, error) {
	return getRestartPollStatus(pollURL)
}

// remoteTransport 在远端机器上发同样的两个请求（用远端自己的 127.0.0.1 地址）。
type remoteTransport struct {
	target MachineTarget
	runner RemoteRunner
}

func (t remoteTransport) notify(notifyURL string, body restartNotifyBody) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	code, out := t.runner.HTTP(t.target, http.MethodPost, notifyURL, string(payload), 10)
	if code < 200 || code >= 300 {
		return fmt.Errorf("远端 notify 返回 %d %s", code, strings.TrimSpace(out))
	}
	return nil
}

func (t remoteTransport) poll(pollURL string) (restartPollStatus, error) {
	var st restartPollStatus
	code, out := t.runner.HTTP(t.target, http.MethodGet, pollURL, "", 10)
	if code < 200 || code >= 300 {
		return st, fmt.Errorf("远端 poll 返回 %d %s", code, strings.TrimSpace(out))
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return st, fmt.Errorf("远端 poll JSON: %w", err)
	}
	return st, nil
}

func getRestartPollStatus(pollURL string) (restartPollStatus, error) {
	var st restartPollStatus
	client := gracefulHTTPClient
	req, err := http.NewRequest(http.MethodGet, pollURL, nil)
	if err != nil {
		return st, err
	}
	req.Header.Set("Cache-Control", "no-store")
	req.Close = true
	res, err := client.Do(req)
	if err != nil {
		return st, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return st, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return st, fmt.Errorf("poll returned HTTP %d", res.StatusCode)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("poll JSON: %w", err)
	}
	return st, nil
}

// waitForGracefulRestart notifies the project, then polls every 15s until
// the project reports deploy is allowed, or maxWait elapses (force).
// Returns whether the wait ended by force timeout. Deploy events are recorded
// for the notify + each poll attempt + the outcome.
// waitForGracefulRestart 是本机部署的入口（本机 HTTP 通知+轮询）。
func waitForGracefulRestart(store *Store, service ServiceContract, cfg Config, job DeployJob, version string) bool {
	return waitForGracefulRestartVia(store, service, cfg, job, version, localTransport{})
}

// waitForGracefulRestartVia：通知 + 轮询等对方就绪；transport 决定这些请求从这里发还是
// 在远端机器上发。
func waitForGracefulRestartVia(
	store *Store,
	service ServiceContract,
	cfg Config,
	job DeployJob,
	version string,
	transport gracefulTransport,
) (forced bool) {
	// 契约里只存路径，这里按目标机器的端口拼出真地址（本机 → 127.0.0.1:port；
	// 远端 → 同一个字符串，但由 remoteTransport 在那台机器上执行）。
	notifyURL := serviceNotifyURL(service)
	pollURL := servicePollURL(service)
	maxWait := service.gracefulMaxWait(cfg)
	deadline := time.Now().Add(maxWait)

	recordDeployEvent(store, job.RequestID, eventlevel.Info,
		fmt.Sprintf("graceful：已启用通知+轮询（最长 %s）", maxWait))
	fmt.Printf("[deploy] %s graceful: POST notify %s\n", job.RequestID, notifyURL)
	recordDeployEvent(store, job.RequestID, eventlevel.Info, "graceful：发送通知 POST "+notifyURL)
	if err := transport.notify(notifyURL, restartNotifyBody{
		ServiceID:  service.ServiceID,
		RequestID:  job.RequestID,
		Deployment: job.Deployment,
		Version:    version,
		Message:    "deployment service will restart this runtime after graceful wait",
	}); err != nil {
		fmt.Printf("[deploy] %s graceful notify failed (%v); continuing to poll\n", job.RequestID, err)
		recordDeployEvent(store, job.RequestID, eventlevel.Warn,
			"graceful：通知失败（继续轮询）："+err.Error())
	} else {
		recordDeployEvent(store, job.RequestID, eventlevel.Success, "graceful：通知已送达")
	}

	attempt := 0
	for {
		attempt++
		st, err := transport.poll(pollURL)
		if err != nil {
			fmt.Printf("[deploy] %s graceful poll #%d failed: %v\n", job.RequestID, attempt, err)
			recordDeployEvent(store, job.RequestID, eventlevel.Warn,
				fmt.Sprintf("graceful 轮询 #%d 失败：%v", attempt, err))
		} else if pollAllowsDeploy(st) {
			fmt.Printf("[deploy] %s graceful: project ready (poll #%d)\n", job.RequestID, attempt)
			recordDeployEvent(store, job.RequestID, eventlevel.Success,
				fmt.Sprintf("graceful 轮询 #%d：项目就绪，继续部署", attempt))
			return false
		} else {
			fmt.Printf("[deploy] %s graceful: not ready yet (poll #%d)\n", job.RequestID, attempt)
			recordDeployEvent(store, job.RequestID, eventlevel.Info,
				fmt.Sprintf("graceful 轮询 #%d：尚未就绪", attempt))
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			fmt.Printf("[deploy] %s graceful: max wait %s elapsed; forcing restart\n",
				job.RequestID, maxWait)
			recordDeployEvent(store, job.RequestID, eventlevel.Warn,
				fmt.Sprintf("graceful：等待超时（%s），强制重启", maxWait))
			return true
		}
		sleep := gracefulPollInterval
		if sleep > remaining {
			sleep = remaining
		}
		time.Sleep(sleep)
		if time.Now().After(deadline) {
			fmt.Printf("[deploy] %s graceful: max wait %s elapsed; forcing restart\n",
				job.RequestID, maxWait)
			recordDeployEvent(store, job.RequestID, eventlevel.Warn,
				fmt.Sprintf("graceful：等待超时（%s），强制重启", maxWait))
			return true
		}
	}
}
