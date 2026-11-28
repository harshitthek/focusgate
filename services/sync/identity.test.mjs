import assert from "node:assert/strict";
import { createServer } from "node:http";
import { once } from "node:events";
import { test } from "node:test";
import { exportJWK, generateKeyPair, SignJWT } from "jose";
import app from "./index.mjs";

test("identity verifies Neon issuer, audience, signature and expiry", async () => {
  const { publicKey, privateKey } = await generateKeyPair("EdDSA");
  const jwk = { ...await exportJWK(publicKey), kid:"staging-key", alg:"EdDSA" };
  const issuer = "https://auth.example.test";
  const server = createServer((_, res) => {
    res.setHeader("Content-Type","application/json");
    res.end(JSON.stringify({keys:[jwk]}));
  }).listen(0,"127.0.0.1");
  await once(server,"listening");
  process.env.NEON_AUTH_JWKS_URL = "http://127.0.0.1:"+server.address().port+"/jwks";
  process.env.NEON_AUTH_BASE_URL = issuer+"/neondb/auth";
  process.env.DATABASE_URL = "postgres://unused@127.0.0.1/focusgate";
  const issue = (audience, expiry) => new SignJWT({})
    .setProtectedHeader({alg:"EdDSA",kid:"staging-key"})
    .setIssuer(issuer).setAudience(audience).setSubject("account").setExpirationTime(expiry).sign(privateKey);
  const identify = token => app.fetch(new Request("https://sync.example.test/v1/identity", {
    method:"POST", headers:{ Authorization:"Bearer "+token }
  }));
  try {
    const valid = await identify(await issue(issuer,"5m"));
    assert.equal(valid.status,200);
    assert.deepEqual(await valid.json(), { issuer,subject:"account" });
    assert.equal((await identify(await issue("https://other.example.test","5m"))).status,401);
    assert.equal((await identify(await issue(issuer,1))).status,401);
    assert.equal((await identify("malformed")).status,401);
  } finally {
    server.close();
    await once(server,"close");
  }
});
