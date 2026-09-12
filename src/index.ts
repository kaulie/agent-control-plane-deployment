import Fastify from "fastify";
import cors from "@fastify/cors";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { loadConfig } from "./config.js";
import { Store } from "./db.js";
import { registerRoutes } from "./routes.js";
import {
  DeployPause,
  DeployWorker,
  Watchdog,
  reconcileOrphanDeploys,
} from "./worker.js";

const config = loadConfig();
const store = new Store(config.dbPath);

/** Seed the gateway service contract if missing (B: registry in SQLite). */
function seedDefaultService(): void {
  if (store.getService("web-cursor")) return;
  const runtimeDir = path.join(os.homedir(), "runtime", "web-cursor");
  store.upsertService({
    serviceId: "web-cursor",
    name: "Web Cursor Agent Gateway",
    runtimeDir,
    healthUrl: "http://127.0.0.1:4211/health",
    startCmd: `bash "${path.join(runtimeDir, "scripts", "start.sh")}"`,
    stopCmd: `bash "${path.join(runtimeDir, "scripts", "stop.sh")}"`,
    restartCmd: `bash "${path.join(runtimeDir, "scripts", "restart.sh")}"`,
    watchdogEnabled: true,
  });
  console.log("[seed] registered service web-cursor →", runtimeDir);
}

seedDefaultService();

const app = Fastify({ logger: true });
await app.register(cors, { origin: true });

const pause = new DeployPause();
const worker = new DeployWorker(store, config, pause);
const watchdog = new Watchdog(store, config, pause);

await registerRoutes(app, { store, config, worker });

const pidFile = path.join(config.home, "deployment.pid");
fs.writeFileSync(pidFile, `${process.pid}\n`);

const shutdown = async (): Promise<void> => {
  worker.stop();
  watchdog.stop();
  try {
    fs.unlinkSync(pidFile);
  } catch {
    /* ignore */
  }
  await app.close();
  process.exit(0);
};
process.on("SIGINT", () => void shutdown());
process.on("SIGTERM", () => void shutdown());

await app.listen({ host: config.host, port: config.port });

const reconciled = await reconcileOrphanDeploys(store);
if (reconciled > 0) {
  app.log.info(`reconciled ${reconciled} orphan deploy(s) left running`);
}

worker.start();
watchdog.start();
app.log.info(
  `deployment service on http://${config.host}:${config.port} home=${config.home}`,
);
