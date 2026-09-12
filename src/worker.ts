import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Config } from "./config.js";
import type { ServiceContract, Store } from "./db.js";

export function normalizeDeploymentTag(raw: string): string {
  const s = raw.trim();
  if (!s) throw new Error("deployment is required");
  if (s.startsWith("deployment-")) return s;
  return `deployment-${s.replace(/^deployment-/, "")}`;
}

function runShell(
  cmd: string,
  cwd: string,
  env: NodeJS.ProcessEnv,
  timeoutSec: number,
): Promise<{ code: number; output: string }> {
  return new Promise((resolve) => {
    const child = spawn("/bin/bash", ["-lc", cmd], {
      cwd,
      env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    const append = (buf: Buffer) => {
      output += buf.toString("utf8");
      if (output.length > 200_000) output = output.slice(-200_000);
    };
    child.stdout?.on("data", append);
    child.stderr?.on("data", append);
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
    }, timeoutSec * 1000);
    child.on("close", (code) => {
      clearTimeout(timer);
      resolve({ code: code ?? 1, output });
    });
  });
}

async function healthOk(url: string): Promise<boolean> {
  try {
    const ac = new AbortController();
    const t = setTimeout(() => ac.abort(), 3000);
    const res = await fetch(url, { signal: ac.signal, cache: "no-store" });
    clearTimeout(t);
    return res.ok;
  } catch {
    return false;
  }
}

export async function executeDeploy(opts: {
  store: Store;
  config: Config;
  requestId: string;
}): Promise<void> {
  const job = opts.store.getDeploy(opts.requestId);
  if (!job || job.state !== "running") return;

  const service = opts.store.getService(job.serviceId);
  if (!service) {
    opts.store.finishDeploy(job.requestId, {
      state: "failed",
      error: `unknown service: ${job.serviceId}`,
    });
    return;
  }

  let tag: string;
  try {
    tag = normalizeDeploymentTag(job.deployment);
  } catch (err) {
    opts.store.finishDeploy(job.requestId, {
      state: "failed",
      error: err instanceof Error ? err.message : String(err),
    });
    return;
  }

  const hash = tag.replace(/^deployment-/, "");
  const src = path.join(opts.config.packagesDir, tag);
  if (!fs.existsSync(path.join(src, "VERSION"))) {
    opts.store.finishDeploy(job.requestId, {
      state: "failed",
      error: `package not found: ${src}`,
    });
    return;
  }
  if (!fs.existsSync(path.join(src, "scripts", "restart.sh")) && !service.restartCmd) {
    opts.store.finishDeploy(job.requestId, {
      state: "failed",
      error: `package incomplete and no restartCmd: ${src}`,
    });
    return;
  }

  const snapVer = fs.readFileSync(path.join(src, "VERSION"), "utf8").trim();
  if (snapVer !== hash) {
    opts.store.finishDeploy(job.requestId, {
      state: "failed",
      error: `VERSION(${snapVer}) != hash(${hash})`,
    });
    return;
  }

  fs.mkdirSync(service.runtimeDir, { recursive: true });

  const rsyncCmd = [
    "rsync",
    "-a",
    "--delete",
    "--filter='P backend/.env'",
    "--filter='P backend/data/'",
    "--filter='P backend/runtime.pid'",
    "--filter='P backend/server.log'",
    "--filter='P backend/.watchdog-paused'",
    "--exclude='backend/.env'",
    "--exclude='backend/data/'",
    "--exclude='backend/runtime.pid'",
    "--exclude='backend/server.log'",
    "--exclude='backend/.watchdog-paused'",
    "--exclude='.git/'",
    `"${src}/"`,
    `"${service.runtimeDir}/"`,
  ].join(" ");

  console.log(`[deploy] ${job.requestId} rsync ${tag} → ${service.runtimeDir}`);
  const rsync = await runShell(
    rsyncCmd,
    opts.config.home,
    process.env,
    opts.config.deployMaxSec,
  );
  if (rsync.code !== 0) {
    opts.store.finishDeploy(job.requestId, {
      state: "failed",
      error: `rsync failed: ${rsync.output.slice(-2000)}`,
    });
    return;
  }

  fs.writeFileSync(path.join(service.runtimeDir, "VERSION"), `${hash}\n`);
  fs.writeFileSync(path.join(service.runtimeDir, "DEPLOYMENT"), `${tag}\n`);

  const restartCmd =
    service.restartCmd.trim() ||
    `bash "${path.join(service.runtimeDir, "scripts", "restart.sh")}"`;
  console.log(`[deploy] ${job.requestId} restart via contract: ${restartCmd}`);
  const restart = await runShell(
    restartCmd,
    service.runtimeDir,
    { ...process.env, APP_VERSION: hash, RUNTIME_DIR: service.runtimeDir },
    opts.config.deployMaxSec,
  );
  if (restart.code !== 0) {
    opts.store.finishDeploy(job.requestId, {
      state: "failed",
      version: hash,
      error: `restart failed: ${restart.output.slice(-2000)}`,
    });
    return;
  }

  const ok = await healthOk(service.healthUrl);
  if (!ok) {
    opts.store.finishDeploy(job.requestId, {
      state: "failed",
      version: hash,
      error: `restart finished but health check failed: ${service.healthUrl}`,
    });
    return;
  }

  opts.store.finishDeploy(job.requestId, {
    state: "succeeded",
    version: hash,
    message: "deploy succeeded",
  });
  console.log(`[deploy] ${job.requestId} ok version=${hash}`);
}

export class DeployWorker {
  private timer: NodeJS.Timeout | undefined;
  private busy = false;

  constructor(
    private store: Store,
    private config: Config,
  ) {}

  start(): void {
    if (this.timer) return;
    this.timer = setInterval(() => void this.tick(), 1000);
    this.timer.unref?.();
    void this.tick();
  }

  stop(): void {
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
  }

  kick(): void {
    void this.tick();
  }

  private async tick(): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    try {
      const job = this.store.claimNextQueued();
      if (!job) return;
      await executeDeploy({
        store: this.store,
        config: this.config,
        requestId: job.requestId,
      });
    } catch (err) {
      console.warn(
        "[deploy-worker]",
        err instanceof Error ? err.message : err,
      );
    } finally {
      this.busy = false;
    }
  }
}

export class Watchdog {
  private timer: NodeJS.Timeout | undefined;

  constructor(
    private store: Store,
    private config: Config,
  ) {}

  start(): void {
    if (this.timer) return;
    this.timer = setInterval(
      () => void this.tick(),
      this.config.watchdogIntervalSec * 1000,
    );
    this.timer.unref?.();
  }

  stop(): void {
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
  }

  private async tick(): Promise<void> {
    for (const svc of this.store.listServices()) {
      if (!svc.watchdogEnabled) continue;
      const ok = await healthOk(svc.healthUrl);
      if (ok) continue;
      console.warn(
        `[watchdog] ${svc.serviceId} unhealthy → startCmd`,
      );
      await runShell(
        svc.startCmd,
        svc.runtimeDir,
        { ...process.env, RUNTIME_DIR: svc.runtimeDir },
        this.config.deployMaxSec,
      );
    }
  }
}

export function assertPackage(packagesDir: string, deployment: string): string {
  const tag = normalizeDeploymentTag(deployment);
  const snap = path.join(packagesDir, tag);
  if (!fs.existsSync(path.join(snap, "VERSION"))) {
    throw new Error(`deployment package not found: ${snap}`);
  }
  return tag;
}

export type { ServiceContract };
