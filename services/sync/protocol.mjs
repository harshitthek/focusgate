import { createHash } from "node:crypto";

const entityID = /^[0-9a-f]{64}$/;
const operationID = /^[0-9a-f]{32}$/;
const safeRevision = n => Number.isSafeInteger(n) && n >= 0;
const oneMonth = 30 * 24 * 60 * 60;

export class InvalidRequest extends Error {}

function check(test) {
  if (!test) throw new InvalidRequest("invalid sync request");
}

function within(value, limit) {
  return typeof value === "string" && !value.includes("\0") && Array.from(value).length <= limit;
}

function httpURL(raw, required) {
  if (raw === "" && !required) return;
  check(within(raw, 2048));
  let url;
  try { url = new URL(raw); } catch { throw new InvalidRequest("invalid URL"); }
  check(["http:", "https:"].includes(url.protocol) && url.hostname && !url.username && !url.password);
}

function parseOperation(op) {
  check(op && typeof op.id === "string" && typeof op.entity === "string" &&
    operationID.test(op.id) && entityID.test(op.entity) && safeRevision(op.base));
  const d = op.data;
  check(d && typeof d === "object" && !Array.isArray(d));
  let data;
  if (op.kind === "feed") {
    httpURL(d.url, true);
    check(d.id === op.entity && createHash("sha256").update(d.url).digest("hex") === d.id &&
      within(d.name, 128) && d.name.length && typeof d.paused === "boolean" &&
      (d.removed === undefined || typeof d.removed === "boolean"));
    data = { id:d.id, url:d.url, name:d.name, paused:d.paused, removed:d.removed === true };
  } else if (op.kind === "item") {
    httpURL(d.url, false);
    check(d.id === op.entity && typeof d.feedId === "string" && entityID.test(d.feedId) &&
      within(d.title, 512) && within(d.snippet, 900) && safeRevision(d.published) &&
      d.published <= Math.floor(Date.now()/1000) + 86400 &&
      [0,1,2].includes(d.status) && [1,2,3,4,5].includes(d.priority));
    data = { id:d.id, feedId:d.feedId, title:d.title, url:d.url, snippet:d.snippet,
      published:d.published, status:d.status, priority:d.priority };
  } else {
    check(op.kind === "status" && [1,2].includes(d.status));
    data = { status:d.status };
  }
  return { id:op.id, entity:op.entity, kind:op.kind, base:op.base, data };
}

export function parsePage(body) {
  check(body && typeof body.cursor === "string" && /^(s:)?(0|[1-9][0-9]{0,15})$/.test(body.cursor) &&
    safeRevision(Number(body.cursor.replace(/^s:/,""))) && Array.isArray(body.operations) && body.operations.length <= 50);
  const snapshot = body.cursor.startsWith("s:");
  const cursor = Number(snapshot ? body.cursor.slice(2) : body.cursor);
  const operations = body.operations.map(parseOperation);
  check(new Set(operations.map(o => o.id)).size === operations.length);
  return { cursor, snapshot, operations };
}

export async function sync(pool, account, page) {
  const tx = await pool.connect();
  try {
    await tx.query("BEGIN");
    await tx.query("SET LOCAL statement_timeout = '4s'");
    await tx.query("SET LOCAL lock_timeout = '2s'");
    await tx.query("INSERT INTO focusgate_accounts(account) VALUES($1) ON CONFLICT DO NOTHING", [account]);
    const { rows:[head] } = await tx.query(
      "SELECT revision, retained_from FROM focusgate_accounts WHERE account=$1 FOR UPDATE", [account]);
    let revision = Number(head.revision);
    const now = Math.floor(Date.now()/1000);
    const cutoff = now - oneMonth;
    const acknowledgements = [];

    async function put(kind, id, data, published = null) {
      revision++;
      await tx.query(`
        INSERT INTO focusgate_entities(account,kind,id,revision,data,published) VALUES($1,$2,$3,$4,$5,$6)
        ON CONFLICT(account,kind,id) DO UPDATE SET revision=excluded.revision,data=excluded.data,published=excluded.published
      `, [account,kind,id,revision,data,published]);
    }

    for (const op of page.operations) {
      const fingerprint = createHash("sha256").update(JSON.stringify(op)).digest("hex");
      const {rows:[prior]} = await tx.query(
        "SELECT fingerprint,ack FROM focusgate_operations WHERE account=$1 AND id=$2", [account,op.id]);
      if (prior) {
        if (prior.fingerprint !== fingerprint) throw new InvalidRequest("operation ID already used");
        acknowledgements.push(prior.ack);
        continue;
      }
      const kind = op.kind === "feed" ? "feed" : "item";
      const {rows:[row]} = await tx.query(
        "SELECT data,revision FROM focusgate_entities WHERE account=$1 AND kind=$2 AND id=$3", [account,kind,op.entity]);
      const ack = { id:op.id, conflict:false };
      if (kind === "feed") {
        ack.conflict = (row ? Number(row.revision) : 0) !== op.base;
        if (!ack.conflict && !op.data.removed && (!row || row.data.removed)) {
          const {rows:[{count}]} = await tx.query(
            "SELECT count(*) FROM focusgate_entities WHERE account=$1 AND kind='feed' AND NOT (data->>'removed')::boolean", [account]);
          ack.conflict = Number(count) >= 8;
        }
        if (ack.conflict) {
          if (row) ack.feed = row.data;
        } else {
          ack.feed = { ...op.data, revision:revision+1 };
          await put(kind, op.entity, ack.feed);
        }
      } else if (row) {
        const status = Math.max(row.data.status, op.data.status);
        if (status !== row.data.status) await put(kind,op.entity,{ ...row.data,status },row.data.published);
      } else if (op.kind === "item" && op.data.published >= cutoff) {
        await put(kind,op.entity,op.data,op.data.published);
      }
      await tx.query(
        "INSERT INTO focusgate_operations(account,id,fingerprint,ack) VALUES($1,$2,$3,$4)", [account,op.id,fingerprint,ack]);
      acknowledgements.push(ack);
    }

    const {rows:[pruned]} = await tx.query(`
      WITH removed AS (
        DELETE FROM focusgate_entities
        WHERE account=$1 AND kind='item' AND (published<$2 OR id IN (
          SELECT id FROM focusgate_entities WHERE account=$1 AND kind='item'
          ORDER BY published DESC,id OFFSET 1000
        )) RETURNING revision
      ) SELECT coalesce(max(revision),0) AS revision FROM removed
    `, [account,cutoff]);
    await tx.query("DELETE FROM focusgate_operations WHERE account=$1 AND received<now()-interval '45 days'", [account]);
    const floor = Math.max(Number(pruned.revision), Number(head.retained_from));
    await tx.query("UPDATE focusgate_accounts SET revision=$2,retained_from=$3 WHERE account=$1", [account,revision,floor]);

    // Current rows carry revisions. A cursor outside the retained window scans
    // those rows again. During paging, the cursor is prefixed with an internal
    // snapshot marker on the wire so later pages don't reset repeatedly.
    const reset = page.cursor > revision || (!page.snapshot && (page.cursor === 0 || page.cursor < floor));
    const after = reset ? 0 : page.cursor;
    const snapshot = page.snapshot || reset;
    const {rows} = await tx.query(`
      SELECT kind,data,revision FROM focusgate_entities
      WHERE account=$1 AND revision>$2 ORDER BY revision LIMIT 51
    `, [account,after]);
    const more = rows.length > 50;
    const changes = rows.slice(0,50).map(({kind,data}) => ({kind,data}));
    const cursor = more ? String(rows[49].revision) : String(revision);
    await tx.query("COMMIT");
    return { cursor:snapshot && more ? "s:"+cursor : cursor, more, reset, acks:acknowledgements, changes };
  } catch (error) {
    await tx.query("ROLLBACK").catch(() => {});
    throw error;
  } finally {
    tx.release();
  }
}
