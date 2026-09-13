package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const gracefulPollInterval = 15 * time.Second

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

func postRestartNotify(notifyURL string, body restartNotifyBody) (status int, respBody string, err error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, "", err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodPost, notifyURL, bytes.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	status = res.StatusCode
	respBody = string(raw)
	if status < 200 || status >= 300 {
		return status, respBody, fmt.Errorf("notify returned HTTP %d", status)
	}
	return status, respBody, nil
}

func getRestartPollStatus(pollURL string) (st restartPollStatus, status int, body string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, pollURL, nil)
	if err != nil {
		return st, 0, "", err
	}
	req.Header.Set("Cache-Control", "no-store")
	res, err := client.Do(req)
	if err != nil {
		return st, 0, "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return st, res.StatusCode, "", err
	}
	status = res.StatusCode
	body = string(raw)
	if status < 200 || status >= 300 {
		return st, status, body, fmt.Errorf("poll returned HTTP %d", status)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, status, body, fmt.Errorf("poll JSON: %w", err)
	}
	return st, status, body, nil
}

// clipTrailing trims s to at most n bytes, appending an ellipsis if truncated.
func clipTrailing(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// waitForGracefulRestart notifies the project, then polls every 15s until
// the project reports deploy is allowed, or maxWait elapses (force).
// Returns whether the wait ended by force timeout. Deploy events are recorded
// for the notify + each poll attempt + the outcome.
func waitForGracefulRestart(
	store *Store,
	service ServiceContract,
	cfg Config,
	job DeployJob,
	version string,
) (forced bool) {
	notifyURL := strings.TrimSpace(service.RestartNotifyURL)
	pollURL := strings.TrimSpace(service.RestartPollURL)
	maxWait := service.gracefulMaxWait(cfg)
	deadline := time.Now().Add(maxWait)

	recordDeployEvent(store, job.RequestID, "info",
		fmt.Sprintf("graceful：已启用通知+轮询（最长 %s）", maxWait))
	fmt.Printf("[deploy] %s graceful: POST notify %s\n", job.RequestID, notifyURL)
	notifyBody := restartNotifyBody{
		ServiceID:  service.ServiceID,
		RequestID:  job.RequestID,
		Deployment: job.Deployment,
		Version:    version,
		Message:    "deployment service will restart this runtime after graceful wait",
	}
	reqJSON, _ := json.Marshal(notifyBody)
	status, respBody, nerr := postRestartNotify(notifyURL, notifyBody)
	if nerr != nil {
		fmt.Printf("[deploy] %s graceful notify failed (%v); continuing to poll\n", job.RequestID, nerr)
		recordDeployEvent(store, job.RequestID, "warn",
			"graceful 通知：POST "+notifyURL+
				"\n请求体: "+string(reqJSON)+
				"\n响应: HTTP "+strconv.Itoa(status)+"\n"+clipTrailing(respBody, 1000)+
				"\n错误: "+nerr.Error())
	} else {
		recordDeployEvent(store, job.RequestID, "ok",
			"graceful 通知：POST "+notifyURL+
				"\n请求体: "+string(reqJSON)+
				"\n响应: HTTP "+strconv.Itoa(status)+"\n"+clipTrailing(respBody, 1000))
	}

	attempt := 0
	for {
		attempt++
		st, pstatus, pbody, perr := getRestartPollStatus(pollURL)
		if perr != nil {
			fmt.Printf("[deploy] %s graceful poll #%d failed: %v\n", job.RequestID, attempt, perr)
			recordDeployEvent(store, job.RequestID, "warn",
				fmt.Sprintf("graceful 轮询 #%d：GET %s\n响应: HTTP %d\n%s\n错误: %v",
					attempt, pollURL, pstatus, clipTrailing(pbody, 1000), perr))
		} else if pollAllowsDeploy(st) {
			fmt.Printf("[deploy] %s graceful: project ready (poll #%d)\n", job.RequestID, attempt)
			recordDeployEvent(store, job.RequestID, "ok",
				fmt.Sprintf("graceful 轮询 #%d：GET %s\n响应: HTTP %d\n%s\n解析: ready=%v canRestart=%v canDeploy=%v → 就绪，继续部署",
					attempt, pollURL, pstatus, clipTrailing(pbody, 1000),
					st.Ready, st.CanRestart, st.CanDeploy))
			return false
		} else {
			fmt.Printf("[deploy] %s graceful: not ready yet (poll #%d)\n", job.RequestID, attempt)
			recordDeployEvent(store, job.RequestID, "info",
				fmt.Sprintf("graceful 轮询 #%d：GET %s\n响应: HTTP %d\n%s\n解析: ready=%v canRestart=%v canDeploy=%v → 尚未就绪",
					attempt, pollURL, pstatus, clipTrailing(pbody, 1000),
					st.Ready, st.CanRestart, st.CanDeploy))
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			fmt.Printf("[deploy] %s graceful: max wait %s elapsed; forcing restart\n",
				job.RequestID, maxWait)
			recordDeployEvent(store, job.RequestID, "warn",
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
			recordDeployEvent(store, job.RequestID, "warn",
				fmt.Sprintf("graceful：等待超时（%s），强制重启", maxWait))
			return true
		}
	}
}
