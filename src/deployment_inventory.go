package main

import (
	"context"
	"sort"
	"strings"
)

// MachineVersionRow is one machine's last known deployed version for a service.
type MachineVersionRow struct {
	MachineID    string `json:"machineId"`
	Host         string `json:"host,omitempty"`
	Port         int    `json:"port,omitempty"`
	FromRegistry bool   `json:"fromRegistry,omitempty"`
	Version      string `json:"version,omitempty"`
	Deployment   string `json:"deployment,omitempty"`
	State        string `json:"state,omitempty"`
	FinishedAt   string `json:"finishedAt,omitempty"`
	RequestID    string `json:"requestId,omitempty"`
	VersionDrift bool   `json:"versionDrift,omitempty"`
}

// ServiceMachineVersions groups machines under one service.
type ServiceMachineVersions struct {
	ServiceID        string              `json:"serviceId"`
	Name             string              `json:"name,omitempty"`
	ReferenceVersion string              `json:"referenceVersion,omitempty"`
	Machines         []MachineVersionRow `json:"machines"`
}

// DeploymentInventory is the read model for the 机器版本 tab.
type DeploymentInventory struct {
	DefaultDeployMachine string                   `json:"defaultDeployMachine"`
	Registry             RegistryStatus           `json:"registry"`
	Services             []ServiceMachineVersions `json:"services"`
}

func normalizeDeployMachine(machine, defaultMachine string) string {
	m := strings.TrimSpace(machine)
	if m == "" {
		return defaultMachine
	}
	return m
}

func deployKey(serviceID, machine, defaultMachine string) string {
	return serviceID + "\x00" + normalizeDeployMachine(machine, defaultMachine)
}

func (s *Store) listSucceededDeploysNewestFirst() ([]DeployJob, error) {
	rows, err := s.db.Query(`
		SELECT ` + deployColumns + `
		FROM deploys
		WHERE state = ? AND COALESCE(finished_at, '') != ''
		ORDER BY finished_at DESC`, string(StateSucceeded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeployJob
	for rows.Next() {
		job, err := scanDeployRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if out == nil {
		out = []DeployJob{}
	}
	return out, rows.Err()
}

func latestSucceededDeployMap(jobs []DeployJob, defaultMachine string) map[string]DeployJob {
	out := map[string]DeployJob{}
	for _, j := range jobs {
		key := deployKey(j.ServiceID, j.TargetMachine, defaultMachine)
		if _, seen := out[key]; seen {
			continue
		}
		out[key] = j
	}
	return out
}

func (s *Store) listSucceededPipelinesNewestFirst() ([]PipelineJob, error) {
	rows, err := s.db.Query(`
		SELECT ` + pipelineColumns + `
		FROM pipelines
		WHERE state = ? AND COALESCE(finished_at, '') != ''
		ORDER BY finished_at DESC`, string(PipelineSucceeded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PipelineJob
	for rows.Next() {
		job, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if out == nil {
		out = []PipelineJob{}
	}
	return out, rows.Err()
}

func latestSucceededPipelineMap(jobs []PipelineJob, defaultMachine string) map[string]PipelineJob {
	out := map[string]PipelineJob{}
	for _, j := range jobs {
		key := deployKey(j.ServiceID, j.TargetMachine, defaultMachine)
		if _, seen := out[key]; seen {
			continue
		}
		out[key] = j
	}
	return out
}

type instancePlacement struct {
	host string
	port int
}

func buildDeploymentInventory(ctx context.Context, store *Store, reg *ServiceRegistry, defaultMachine string) (DeploymentInventory, error) {
	out := DeploymentInventory{DefaultDeployMachine: defaultMachine}
	catalog, regStatus, err := buildServiceCatalog(ctx, store, reg)
	if err != nil {
		return out, err
	}
	out.Registry = regStatus

	deploys, err := store.listSucceededDeploysNewestFirst()
	if err != nil {
		return out, err
	}
	latestDeploy := latestSucceededDeployMap(deploys, defaultMachine)

	pipelines, err := store.listSucceededPipelinesNewestFirst()
	if err != nil {
		return out, err
	}
	latestPipeline := latestSucceededPipelineMap(pipelines, defaultMachine)

	byService := map[string]map[string]instancePlacement{}
	if reg.Enabled() {
		_, instances, snapErr := reg.Snapshot(ctx)
		if snapErr != nil {
			out.Registry.OK = false
			if out.Registry.Error == "" {
				out.Registry.Error = snapErr.Error()
			}
		} else if !out.Registry.OK {
			out.Registry.OK = true
		}
		for _, inst := range instances {
			svc := strings.TrimSpace(inst.Service)
			if svc == "" {
				continue
			}
			mid := MachineIDForInstance(inst)
			if mid == "" {
				continue
			}
			if byService[svc] == nil {
				byService[svc] = map[string]instancePlacement{}
			}
			byService[svc][mid] = instancePlacement{host: inst.Host, port: inst.Port}
		}
	}

	machineSetForService := func(serviceID string) map[string]bool {
		set := map[string]bool{}
		for m := range byService[serviceID] {
			set[m] = true
		}
		for key := range latestDeploy {
			parts := strings.SplitN(key, "\x00", 2)
			if len(parts) == 2 && parts[0] == serviceID {
				set[parts[1]] = true
			}
		}
		for key := range latestPipeline {
			parts := strings.SplitN(key, "\x00", 2)
			if len(parts) == 2 && parts[0] == serviceID {
				set[parts[1]] = true
			}
		}
		return set
	}

	fillRow := func(serviceID, machine string, fromReg bool, place instancePlacement) MachineVersionRow {
		row := MachineVersionRow{
			MachineID:    machine,
			Host:         place.host,
			Port:         place.port,
			FromRegistry: fromReg,
		}
		key := deployKey(serviceID, machine, defaultMachine)
		if job, ok := latestDeploy[key]; ok {
			row.Version = strings.TrimSpace(job.Version)
			row.Deployment = strings.TrimSpace(job.Deployment)
			row.State = string(job.State)
			row.FinishedAt = job.FinishedAt
			row.RequestID = job.RequestID
		}
		if row.Version == "" {
			if pipe, ok := latestPipeline[key]; ok {
				row.Version = strings.TrimSpace(pipe.Version)
				row.Deployment = strings.TrimSpace(pipe.Deployment)
				row.State = string(pipe.State)
				row.FinishedAt = pipe.FinishedAt
				row.RequestID = pipe.RequestID
			}
		}
		if row.State == "" {
			row.State = "unknown"
		}
		return row
	}

	services := make([]ServiceMachineVersions, 0, len(catalog))
	for _, entry := range catalog {
		sid := entry.ServiceID
		if sid == "" {
			continue
		}
		machines := machineSetForService(sid)
		rows := make([]MachineVersionRow, 0, len(machines))
		for m := range machines {
			place := instancePlacement{}
			fromReg := false
			if svcMap := byService[sid]; svcMap != nil {
				if p, ok := svcMap[m]; ok {
					place = p
					fromReg = true
				}
			}
			rows = append(rows, fillRow(sid, m, fromReg, place))
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].MachineID < rows[j].MachineID })
		ref := referenceVersion(rows)
		for i := range rows {
			v := strings.TrimSpace(rows[i].Version)
			if v != "" && ref != "" && v != ref {
				rows[i].VersionDrift = true
			}
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" && entry.Registry != nil {
			name = strings.TrimSpace(entry.Registry.Name)
		}
		services = append(services, ServiceMachineVersions{
			ServiceID:        sid,
			Name:             name,
			ReferenceVersion: ref,
			Machines:         rows,
		})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].ServiceID < services[j].ServiceID })
	out.Services = services
	return out, nil
}

func referenceVersion(rows []MachineVersionRow) string {
	counts := map[string]int{}
	for _, r := range rows {
		v := strings.TrimSpace(r.Version)
		if v == "" {
			continue
		}
		counts[v]++
	}
	if len(counts) == 0 {
		return ""
	}
	bestV := ""
	bestN := 0
	for v, n := range counts {
		if n > bestN || (n == bestN && v > bestV) {
			bestV, bestN = v, n
		}
	}
	return bestV
}
