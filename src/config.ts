import path from "node:path";
import fs from "node:fs";
import os from "node:os";

export interface Config {
  host: string;
  port: number;
  /** Install / data root: ~/runtime/agent-control-plane-deployment */
  home: string;
  packagesDir: string;
  dataDir: string;
  dbPath: string;
  deployMaxSec: number;
  watchdogIntervalSec: number;
}

function expandHome(p: string): string {
  if (p.startsWith("~/")) return path.join(os.homedir(), p.slice(2));
  return p;
}

export function loadConfig(): Config {
  const home = expandHome(
    process.env.DEPLOYMENT_HOME?.trim() ||
      path.join(os.homedir(), "runtime", "agent-control-plane-deployment"),
  );
  const dataDir = path.join(home, "data");
  const packagesDir = path.join(home, "packages");
  fs.mkdirSync(dataDir, { recursive: true });
  fs.mkdirSync(packagesDir, { recursive: true });
  fs.mkdirSync(path.join(home, "logs"), { recursive: true });

  return {
    host: process.env.HOST || "127.0.0.1",
    port: Number(process.env.PORT || 4220),
    home,
    packagesDir,
    dataDir,
    dbPath: path.join(dataDir, "deploy.sqlite"),
    deployMaxSec: Math.max(30, Number(process.env.DEPLOY_MAX_SEC || 120)),
    watchdogIntervalSec: Math.max(
      5,
      Number(process.env.WATCHDOG_INTERVAL_SEC || 10),
    ),
  };
}
