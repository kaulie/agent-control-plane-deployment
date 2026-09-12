import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Config } from "./config.js";
import type { ServiceContract, Store } from "./db.js";

export function normalizeDeploymentTag(raw: string): string {
  const s = raw.trim();
  if (!s) throw new Error("deployment is required");
  if (s.startsWith("deployment-")) return s;
  return `deployment-${s.replace(/^deployment-/, "")}`;
}

/**
 * Run a shell command and wait for exit.
 * `detached: true` so timeout can `kill(-pid)` the child tree only.
 */
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
      detached: true,
    });
    let output = "";
    const append = (buf: Buffer) => {
      output += buf.toString("utf8");
      if (output.length > 200_000) output = output.slice(-200_000);
    };
    child.stdout?.on("data", append);
    child.stderr?.on("data", append);

    const killTree = () => {
      if (child.pid == null) return;
      try {
        process.kill(-child.pid, "SIGKILL");
      } catch {
        try {
          child.kill("SIGKILL");
        } catch {
          /* ignore */
        }
      }
    };

    const timer = setTimeout(killTree, timeoutSec * 1000);
    child.on("close", (code) => {
      clearTimeout(timer);
      resolve({ code: code ?? 1, output });
    });
    child.on("error", (err) => {
      clearTimeout(timer);
      resolve({ code: 1, output: `${output}\n${err.message}` });
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

/** App listen port from contract healthUrl — must override ACP's own PORT=4220. */
function portFromHealthUrl(healthUrl: string): string {
  try {
    const u = new URL(healthUrl);
    if (u.port) return u.port;
    return u.protocol === "https:" ? "443" : "80";
  } catch {
    return "4211";
  }
}

/**
 * Env for app start/stop/restart.
 * Critical: never leak deployment-service PORT/HOST (that made stop.sh kill :4220).
 */
function serviceCmdEnv(
  service: ServiceContract,
  extra: Record<string, string> = {},
): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env, ...extra };
  env.RUNTIME_DIR = service.runtimeDir;
  env.PORT = portFromHealthUrl(service.healthUrl);
  delete env.HOST;
  delete env.DEPLOYMENT_HOME;
  return env;
}

/**
 * Short pause for external ops watchdog during restart only.
 * Keep TTL tight: if this process dies mid-restart, leftover pause must not
 * block auto-recovery for minutes.
 */
function externalWatchdogPausePath(runtimeDir: string): string {
  const name = path.basename(runtimeDir.replace(/\/+$/, "") || runtimeDir);
  return path.join(os.homedir(), "deployment", name, "ops", "watchdog-pause-until");
}

function setExternalWatchdogPause(runtimeDir: string, sec: number): void {
  const file = externalWatchdogPausePath(runtimeDir);
  try {
    fs.mkdirSync(path.dirname(file), { recursive: true });
    const until = Math.floor(Date.now() / 1000) + Math.max(15, sec);
    fs.writeFileSync(file, `${until}\n`);
  } catch (err) {
    console.warn(
      "[deploy] watchdog-pause-until write failed:",
      err instanceof Error ? err.message : err,
    );
  }
}

function clearExternalWatchdogPause(runtimeDir: string): void {
  const file = externalWatchdogPausePath(runtimeDir);
  try {
    fs.unlinkSync(file);
  } catch {
    /* ignore */
  }
}

/** After a crash mid-deploy, unblock external ops watchdogs immediately. */
export function clearStaleDeployPauses(store: Store): number {
  let n = 0;
  for (const svc of store.listServices()) {
    const file = externalWatchdogPausePath(svc.runtimeDir);
    if (!fs.existsSync(file)) continue;
    clearExternalWatchdogPause(svc.runtimeDir);
    // stop.sh may have left this; start.sh normally clears it — do it on boot
    // so ops watchdog is not stuck forever after a mid-restart crash.
    try {
      fs.unlinkSync(path.join(svc.runtimeDir, "backend", ".watchdog-paused"));
    } catch {
      /* ignore */
    }
    console.log(`[deploy] cleared stale pause flags for ${svc.serviceId}`);
    n += 1;
  }
  return n;
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

  // rsync while app is still up — do not pause ops watchdog yet.
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
  // Pause only for the restart window; cap TTL so a crash cannot block recovery long.
  const pauseSec = Math.min(90, opts.config.deployMaxSec);
  setExternalWatchdogPause(service.runtimeDir, pauseSec);
  try {
    console.log(
      `[deploy] ${job.requestId} restart via contract (PORT=${portFromHealthUrl(service.healthUrl)}): ${restartCmd}`,
    );
    const restart = await runShell(
      restartCmd,
      service.runtimeDir,
      serviceCmdEnv(service, { APP_VERSION: hash }),
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
  } finally {
    clearExternalWatchdogPause(service.runtimeDir);
  }
}

/**
 * If this process died mid-restart, jobs can be stuck in `running`.
 * On boot: health+VERSION match → succeeded; otherwise → failed.
 */
export async function reconcileOrphanDeploys(store: Store): Promise<number> {
  const orphans = store.listDeploysByState("running");
  let n = 0;
  for (const job of orphans) {
    const service = store.getService(job.serviceId);
    if (!service) {
      store.finishDeploy(job.requestId, {
        state: "failed",
        error: "deployment service restarted; service contract missing",
        message: "reconciled after deployment service restart",
      });
      n += 1;
      continue;
    }
    const hash = normalizeDeploymentTag(job.deployment).replace(
      /^deployment-/,
      "",
    );
    let versionOnDisk = "";
    try {
      versionOnDisk = fs
        .readFileSync(path.join(service.runtimeDir, "VERSION"), "utf8")
        .trim();
    } catch {
      /* ignore */
    }
    const ok = await healthOk(service.healthUrl);
    if (ok && versionOnDisk === hash) {
      store.finishDeploy(job.requestId, {
        state: "succeeded",
        version: hash,
        message:
          "deploy succeeded (status reconciled after deployment service died mid-restart; gateway health confirmed)",
      });
      console.log(
        `[deploy] reconciled ${job.requestId} → succeeded (health ok, version=${hash})`,
      );
    } else {
      store.finishDeploy(job.requestId, {
        state: "failed",
        version: versionOnDisk || undefined,
        error: ok
          ? `deployment service restarted mid-deploy; VERSION=${versionOnDisk || "?"} expected=${hash}`
          : `deployment service restarted mid-deploy; health check failed: ${service.healthUrl}`,
        message: "reconciled after deployment service restart",
      });
      console.warn(`[deploy] reconciled ${job.requestId} → failed`);
    }
    n += 1;
  }
  return n;
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

export function assertPackage(packagesDir: string, deployment: string): string {
  const tag = normalizeDeploymentTag(deployment);
  const snap = path.join(packagesDir, tag);
  if (!fs.existsSync(path.join(snap, "VERSION"))) {
    throw new Error(`deployment package not found: ${snap}`);
  }
  return tag;
}

export type { ServiceContract };
