package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func postRestartNotify(notifyURL string, body restartNotifyBody) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodPost, notifyURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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

func getRestartPollStatus(pollURL string) (restartPollStatus, error) {
	var st restartPollStatus
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, pollURL, nil)
	if err != nil {
		return st, err
	}
	req.Header.Set("Cache-Control", "no-store")
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
// Returns whether the wait ended by force timeout.
func waitForGracefulRestart(
	service ServiceContract,
	cfg Config,
	job DeployJob,
	version string,
) (forced bool) {
	notifyURL := strings.TrimSpace(service.RestartNotifyURL)
	pollURL := strings.TrimSpace(service.RestartPollURL)
	maxWait := service.gracefulMaxWait(cfg)
	deadline := time.Now().Add(maxWait)

	fmt.Printf("[deploy] %s graceful: POST notify %s\n", job.RequestID, notifyURL)
	if err := postRestartNotify(notifyURL, restartNotifyBody{
		ServiceID:  service.ServiceID,
		RequestID:  job.RequestID,
		Deployment: job.Deployment,
		Version:    version,
		Message:    "deployment service will restart this runtime after graceful wait",
	}); err != nil {
		fmt.Printf("[deploy] %s graceful notify failed (%v); continuing to poll\n", job.RequestID, err)
	}

	attempt := 0
	for {
		attempt++
		st, err := getRestartPollStatus(pollURL)
		if err != nil {
			fmt.Printf("[deploy] %s graceful poll #%d failed: %v\n", job.RequestID, attempt, err)
		} else if pollAllowsDeploy(st) {
			fmt.Printf("[deploy] %s graceful: project ready (poll #%d)\n", job.RequestID, attempt)
			return false
		} else {
			fmt.Printf("[deploy] %s graceful: not ready yet (poll #%d)\n", job.RequestID, attempt)
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			fmt.Printf("[deploy] %s graceful: max wait %s elapsed; forcing restart\n",
				job.RequestID, maxWait)
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
			return true
		}
	}
}
