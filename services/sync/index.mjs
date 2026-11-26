import { createRemoteJWKSet, jwtVerify } from "jose";
import { Pool } from "pg";
import { InvalidRequest, parsePage, sync } from "./protocol.mjs";

const MAX_BODY = 512 * 1024;
let services;

function getServices() {
  if (!services) {
    if (!process.env.DATABASE_URL) throw Error("database configuration missing");
    const issuer = new URL(process.env.NEON_AUTH_BASE_URL).origin;
    const jwks = createRemoteJWKSet(new URL(process.env.NEON_AUTH_JWKS_URL), { timeoutDuration:2500 });
    const pool = new Pool({ connectionString:process.env.DATABASE_URL, max:3, connectionTimeoutMillis:4000 });
    pool.on("error", () => console.error("FocusGate sync database idle connection failed"));
    services = { issuer, jwks, pool };
  }
  return services;
}

async function readJSON(request) {
  const reader = request.body?.getReader();
  if (!reader) throw new InvalidRequest("missing request body");
  const stop = AbortSignal.any([request.signal, AbortSignal.timeout(3500)]);
  const cancel = () => { reader.cancel().catch(() => {}); };
  stop.addEventListener("abort", cancel, { once:true });
  const chunks = [];
  let size = 0;
  try {
    for (;;) {
      stop.throwIfAborted();
      const {done,value} = await reader.read();
      stop.throwIfAborted();
      if (done) break;
      size += value.length;
      if (size > MAX_BODY) {
        await reader.cancel();
        throw new InvalidRequest("request body too large");
      }
      chunks.push(value);
    }
    return JSON.parse(Buffer.concat(chunks,size).toString("utf8"));
  } catch {
    throw new InvalidRequest("invalid request body");
  } finally {
    stop.removeEventListener("abort", cancel);
    reader.releaseLock();
  }
}

const response = (value, status=200) => Response.json(value, { status, headers:{ "Cache-Control":"no-store" } });
let active = 0;

export default {
  async fetch(request) {
    const path = new URL(request.url).pathname;
    if (!["/v1/identity", "/v1/sync"].includes(path)) return response({ error:"not found" },404);
    if (request.method !== "POST") return response({ error:"method not allowed" },405);
    const token = /^Bearer ([^ ]{1,8192})$/.exec(request.headers.get("Authorization") ?? "")?.[1];
    if (!token) return response({ error:"unauthorized" },401);
    if (active >= 8) return response({ error:"busy" },503);
    active++;
    try {
      const {issuer,jwks,pool} = getServices();
      let subject;
      try {
        const {payload} = await jwtVerify(token, jwks, {
          algorithms:["EdDSA"], issuer, audience:issuer, requiredClaims:["iss","aud","sub","exp"]
        });
        if (typeof payload.sub !== "string" || !payload.sub || payload.sub.length>256) throw new Error();
        subject = payload.sub;
      } catch {
        return response({ error:"unauthorized" },401);
      }
      if (path === "/v1/identity") return response({ issuer, subject });
      const account = JSON.stringify([issuer, subject]);
      return response(await sync(pool,account,parsePage(await readJSON(request))));
    } catch (error) {
      if (error instanceof InvalidRequest) return response({ error:error.message },400);
      console.error("FocusGate sync request failed");
      return response({ error:"sync unavailable" },503);
    } finally {
      active--;
    }
  }
};
