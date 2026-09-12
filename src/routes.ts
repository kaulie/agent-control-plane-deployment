import type { FastifyInstance } from "fastify";
import { randomUUID } from "node:crypto";
import type { Config } from "./config.js";
import type { Store } from "./db.js";
import {
  assertPackage,
  type DeployWorker,
  normalizeDeploymentTag,
} from "./worker.js";

export async function registerRoutes(
  app: FastifyInstance,
  opts: {
    store: Store;
    config: Config;
    worker: DeployWorker;
  },
): Promise<void> {
  const { store, config, worker } = opts;

  app.get("/health", async () => ({
    ok: true,
    service: "agent-control-plane-deployment",
    home: config.home,
    time: new Date().toISOString(),
  }));

  app.get("/api/services", async () => ({
    services: store.listServices(),
  }));

  app.get<{ Params: { serviceId: string } }>(
    "/api/services/:serviceId",
    async (req, reply) => {
      const svc = store.getService(req.params.serviceId);
      if (!svc) return reply.code(404).send({ error: "service not found" });
      return svc;
    },
  );

  app.put<{
    Params: { serviceId: string };
    Body: {
      name?: string;
      runtimeDir?: string;
      healthUrl?: string;
      startCmd?: string;
      stopCmd?: string;
      restartCmd?: string;
      watchdogEnabled?: boolean;
    };
  }>("/api/services/:serviceId", async (req, reply) => {
    const body = req.body ?? {};
    const serviceId = req.params.serviceId.trim();
    if (!serviceId) return reply.code(400).send({ error: "serviceId required" });
    const existing = store.getService(serviceId);
    const name = body.name?.trim() || existing?.name || serviceId;
    const runtimeDir = body.runtimeDir?.trim() || existing?.runtimeDir;
    const healthUrl = body.healthUrl?.trim() || existing?.healthUrl;
    const startCmd = body.startCmd?.trim() || existing?.startCmd;
    const stopCmd = body.stopCmd?.trim() || existing?.stopCmd;
    const restartCmd = body.restartCmd?.trim() || existing?.restartCmd;
    if (!runtimeDir || !healthUrl || !startCmd || !stopCmd || !restartCmd) {
      return reply.code(400).send({
        error:
          "runtimeDir, healthUrl, startCmd, stopCmd, restartCmd are required (or update an existing service)",
      });
    }
    const svc = store.upsertService({
      serviceId,
      name,
      runtimeDir,
      healthUrl,
      startCmd,
      stopCmd,
      restartCmd,
      watchdogEnabled:
        body.watchdogEnabled ?? existing?.watchdogEnabled ?? true,
    });
    return reply.code(existing ? 200 : 201).send(svc);
  });

  app.delete<{ Params: { serviceId: string } }>(
    "/api/services/:serviceId",
    async (req, reply) => {
      if (!store.deleteService(req.params.serviceId)) {
        return reply.code(404).send({ error: "service not found" });
      }
      return { ok: true };
    },
  );

  /**
   * Enqueue a deploy against a registered service contract.
   * Body: { serviceId, deployment, requestId? }
   */
  app.post<{
    Body: {
      serviceId?: string;
      deployment?: string;
      hash?: string;
      requestId?: string;
    };
  }>("/api/deploys", async (req, reply) => {
    const serviceId = req.body?.serviceId?.trim() || "";
    const raw =
      req.body?.deployment?.trim() || req.body?.hash?.trim() || "";
    if (!serviceId) {
      return reply.code(400).send({ error: "serviceId is required" });
    }
    if (!raw) {
      return reply.code(400).send({ error: "deployment or hash is required" });
    }
    if (!store.getService(serviceId)) {
      return reply.code(404).send({ error: `service not found: ${serviceId}` });
    }
    let deployment: string;
    try {
      deployment = assertPackage(config.packagesDir, raw);
    } catch (err) {
      return reply
        .code(400)
        .send({ error: err instanceof Error ? err.message : String(err) });
    }
    const requestId =
      req.body?.requestId?.trim() || `deploy-req-${randomUUID().slice(0, 8)}`;
    if (store.getDeploy(requestId)) {
      return reply.code(409).send({ error: `request already exists: ${requestId}` });
    }
    const job = store.createDeploy({
      requestId,
      serviceId,
      deployment,
      message: "queued for deployment worker",
    });
    worker.kick();
    return reply.code(202).send({
      ...job,
      poll: `/api/deploys/${requestId}`,
    });
  });

  app.get("/api/deploys", async (req) => {
    const q = req.query as { limit?: string };
    const limit = Math.min(200, Math.max(1, Number(q.limit || 50)));
    return { deploys: store.listDeploys(limit) };
  });

  app.get<{ Params: { requestId: string } }>(
    "/api/deploys/:requestId",
    async (req, reply) => {
      const job = store.getDeploy(req.params.requestId);
      if (!job) return reply.code(404).send({ error: "deploy not found" });
      return job;
    },
  );

  app.get("/api/meta", async () => ({
    home: config.home,
    packagesDir: config.packagesDir,
    port: config.port,
    normalizeExample: normalizeDeploymentTag("abc12345"),
  }));
}
