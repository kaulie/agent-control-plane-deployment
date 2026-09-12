import { DatabaseSync } from "node:sqlite";
import fs from "node:fs";
import path from "node:path";

export type DeployState =
  | "queued"
  | "running"
  | "succeeded"
  | "failed"
  | "cancelled";

export interface ServiceContract {
  serviceId: string;
  name: string;
  runtimeDir: string;
  healthUrl: string;
  startCmd: string;
  stopCmd: string;
  restartCmd: string;
  watchdogEnabled: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface DeployJob {
  requestId: string;
  serviceId: string;
  deployment: string;
  state: DeployState;
  requestedAt: string;
  startedAt?: string;
  finishedAt?: string;
  version?: string;
  error?: string;
  message?: string;
}

function nowIso(): string {
  return new Date().toISOString();
}

export class Store {
  private db: DatabaseSync;

  constructor(dbPath: string) {
    fs.mkdirSync(path.dirname(dbPath), { recursive: true });
    this.db = new DatabaseSync(dbPath);
    this.db.exec("PRAGMA journal_mode = WAL;");
    this.migrate();
  }

  private migrate(): void {
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS services (
        service_id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        runtime_dir TEXT NOT NULL,
        health_url TEXT NOT NULL,
        start_cmd TEXT NOT NULL,
        stop_cmd TEXT NOT NULL,
        restart_cmd TEXT NOT NULL,
        watchdog_enabled INTEGER NOT NULL DEFAULT 1,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS deploys (
        request_id TEXT PRIMARY KEY,
        service_id TEXT NOT NULL,
        deployment TEXT NOT NULL,
        state TEXT NOT NULL,
        requested_at TEXT NOT NULL,
        started_at TEXT,
        finished_at TEXT,
        version TEXT,
        error TEXT,
        message TEXT
      );

      CREATE INDEX IF NOT EXISTS idx_deploys_state ON deploys(state);
    `);
  }

  upsertService(
    input: Omit<ServiceContract, "createdAt" | "updatedAt"> & {
      createdAt?: string;
      updatedAt?: string;
    },
  ): ServiceContract {
    const existing = this.getService(input.serviceId);
    const ts = nowIso();
    const row: ServiceContract = {
      serviceId: input.serviceId,
      name: input.name,
      runtimeDir: input.runtimeDir,
      healthUrl: input.healthUrl,
      startCmd: input.startCmd,
      stopCmd: input.stopCmd,
      restartCmd: input.restartCmd,
      watchdogEnabled: input.watchdogEnabled,
      createdAt: existing?.createdAt ?? input.createdAt ?? ts,
      updatedAt: ts,
    };
    this.db
      .prepare(
        `INSERT INTO services (
           service_id, name, runtime_dir, health_url,
           start_cmd, stop_cmd, restart_cmd, watchdog_enabled,
           created_at, updated_at
         ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(service_id) DO UPDATE SET
           name = excluded.name,
           runtime_dir = excluded.runtime_dir,
           health_url = excluded.health_url,
           start_cmd = excluded.start_cmd,
           stop_cmd = excluded.stop_cmd,
           restart_cmd = excluded.restart_cmd,
           watchdog_enabled = excluded.watchdog_enabled,
           updated_at = excluded.updated_at`,
      )
      .run(
        row.serviceId,
        row.name,
        row.runtimeDir,
        row.healthUrl,
        row.startCmd,
        row.stopCmd,
        row.restartCmd,
        row.watchdogEnabled ? 1 : 0,
        row.createdAt,
        row.updatedAt,
      );
    return row;
  }

  getService(serviceId: string): ServiceContract | undefined {
    const r = this.db
      .prepare(`SELECT * FROM services WHERE service_id = ?`)
      .get(serviceId) as Record<string, unknown> | undefined;
    return r ? this.mapService(r) : undefined;
  }

  listServices(): ServiceContract[] {
    const rows = this.db
      .prepare(`SELECT * FROM services ORDER BY name ASC`)
      .all() as Array<Record<string, unknown>>;
    return rows.map((r) => this.mapService(r));
  }

  deleteService(serviceId: string): boolean {
    const res = this.db
      .prepare(`DELETE FROM services WHERE service_id = ?`)
      .run(serviceId);
    return Number(res.changes) > 0;
  }

  createDeploy(input: {
    requestId: string;
    serviceId: string;
    deployment: string;
    message?: string;
  }): DeployJob {
    const requestedAt = nowIso();
    const job: DeployJob = {
      requestId: input.requestId,
      serviceId: input.serviceId,
      deployment: input.deployment,
      state: "queued",
      requestedAt,
      ...(input.message ? { message: input.message } : {}),
    };
    this.db
      .prepare(
        `INSERT INTO deploys (
           request_id, service_id, deployment, state, requested_at, message
         ) VALUES (?, ?, ?, ?, ?, ?)`,
      )
      .run(
        job.requestId,
        job.serviceId,
        job.deployment,
        job.state,
        job.requestedAt,
        job.message ?? null,
      );
    return job;
  }

  getDeploy(requestId: string): DeployJob | undefined {
    const r = this.db
      .prepare(`SELECT * FROM deploys WHERE request_id = ?`)
      .get(requestId) as Record<string, unknown> | undefined;
    return r ? this.mapDeploy(r) : undefined;
  }

  listDeploys(limit = 50): DeployJob[] {
    const rows = this.db
      .prepare(`SELECT * FROM deploys ORDER BY requested_at DESC LIMIT ?`)
      .all(limit) as Array<Record<string, unknown>>;
    return rows.map((r) => this.mapDeploy(r));
  }

  listDeploysByState(state: DeployState): DeployJob[] {
    const rows = this.db
      .prepare(
        `SELECT * FROM deploys WHERE state = ? ORDER BY requested_at ASC`,
      )
      .all(state) as Array<Record<string, unknown>>;
    return rows.map((r) => this.mapDeploy(r));
  }

  claimNextQueued(): DeployJob | undefined {
    const row = this.db
      .prepare(
        `SELECT * FROM deploys WHERE state = 'queued' ORDER BY requested_at ASC LIMIT 1`,
      )
      .get() as Record<string, unknown> | undefined;
    if (!row) return undefined;
    const requestId = String(row.request_id);
    const startedAt = nowIso();
    const res = this.db
      .prepare(
        `UPDATE deploys SET state = 'running', started_at = ? WHERE request_id = ? AND state = 'queued'`,
      )
      .run(startedAt, requestId);
    if (Number(res.changes) === 0) return undefined;
    return this.getDeploy(requestId);
  }

  finishDeploy(
    requestId: string,
    patch: {
      state: "succeeded" | "failed" | "cancelled";
      version?: string;
      error?: string;
      message?: string;
    },
  ): DeployJob | undefined {
    this.db
      .prepare(
        `UPDATE deploys SET
           state = ?,
           finished_at = ?,
           version = COALESCE(?, version),
           error = ?,
           message = COALESCE(?, message)
         WHERE request_id = ?`,
      )
      .run(
        patch.state,
        nowIso(),
        patch.version ?? null,
        patch.error ?? null,
        patch.message ?? null,
        requestId,
      );
    return this.getDeploy(requestId);
  }

  private mapService(r: Record<string, unknown>): ServiceContract {
    return {
      serviceId: String(r.service_id),
      name: String(r.name),
      runtimeDir: String(r.runtime_dir),
      healthUrl: String(r.health_url),
      startCmd: String(r.start_cmd),
      stopCmd: String(r.stop_cmd),
      restartCmd: String(r.restart_cmd),
      watchdogEnabled: Number(r.watchdog_enabled) === 1,
      createdAt: String(r.created_at),
      updatedAt: String(r.updated_at),
    };
  }

  private mapDeploy(r: Record<string, unknown>): DeployJob {
    return {
      requestId: String(r.request_id),
      serviceId: String(r.service_id),
      deployment: String(r.deployment),
      state: r.state as DeployState,
      requestedAt: String(r.requested_at),
      ...(r.started_at ? { startedAt: String(r.started_at) } : {}),
      ...(r.finished_at ? { finishedAt: String(r.finished_at) } : {}),
      ...(r.version ? { version: String(r.version) } : {}),
      ...(r.error ? { error: String(r.error) } : {}),
      ...(r.message ? { message: String(r.message) } : {}),
    };
  }
}
