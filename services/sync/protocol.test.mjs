import assert from "node:assert/strict";
import { createHash, randomBytes } from "node:crypto";
import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { Pool } from "pg";
import { InvalidRequest, parsePage, sync } from "./protocol.mjs";

const digest = text => createHash("sha256").update(text).digest("hex");
const feedURL = "https://example.test/feed";
const feedID = digest(feedURL);
const now = Math.floor(Date.now()/1000);
const item = (id, status = 0) => ({
  id, feedId:feedID, title:"Release", snippet:"Notes", url:"", published:now, status, priority:3
});
const op = (kind, entity, data, base = 0) => ({ id:randomBytes(16).toString("hex"), kind, entity, data, base });
const feed = (name, base=0, removed=false) => op("feed", feedID, {id:feedID,url:feedURL,name,paused:false,removed}, base);
const page = (cursor, ...operations) => parsePage({ cursor, operations });

test("request parser bounds pages and rejects malformed URLs and payloads", () => {
  assert.equal(page("s:34").cursor,34);
  assert.equal(page("s:34").snapshot,true);
  assert.throws(() => page("-1"), InvalidRequest);
  assert.throws(() => page("9999999999999999"), InvalidRequest);
  assert.throws(() => page("0",op("item",digest("item"),{ ...item(digest("item")), url:"javascript:alert(1)" })), InvalidRequest);
  assert.throws(() => page("0",...Array.from({length:51},() => feed("Feed"))), InvalidRequest);
  const duplicate = feed("Feed");
  assert.throws(() => page("0",duplicate,duplicate), InvalidRequest);
});

const dbURL = process.env.TEST_DATABASE_URL;
const pgliteModule = process.env.TEST_PGLITE_MODULE;

test("Postgres sync handles retries, concurrent feed edits, archives and account isolation", {
  skip: !dbURL && !pgliteModule && "set TEST_DATABASE_URL to an isolated Postgres database"
}, async () => {
  let pool, runSQL, stop;
  if (dbURL) {
    pool = new Pool({connectionString:dbURL, max:4});
    runSQL = (...args) => pool.query(...args);
    stop = () => pool.end();
  } else {
    const { PGlite } = await import(pgliteModule);
    const db = new PGlite();
    await db.waitReady;
    let previous = Promise.resolve();
    pool = {async connect() {
      const wait = previous;
      let release;
      previous = new Promise(resolve => { release = resolve; });
      await wait;
      return {query:(sql,args) => db.query(sql,args),release};
    }};
    runSQL = (sql,args) => args ? db.query(sql,args) : db.exec(sql);
    stop = () => db.close();
  }
  const suffix = randomBytes(8).toString("hex");
  const a = "account-a-"+suffix;
  const b = "account-b-"+suffix;
  const c = "account-c-"+suffix;
  try {
    await runSQL(await readFile(new URL("./schema.sql", import.meta.url), "utf8"));
    const add = feed("Original");
    const id = digest("first");
    const initial = await sync(pool,a,page("0",add,op("item",id,item(id))));
    assert.equal(initial.cursor,"2");
    assert.equal(initial.changes.length,2);
    assert.deepEqual((await sync(pool,b,page("0"))).changes,[]);
    assert.equal((await sync(pool,a,page("2",add))).acks[0].feed.revision,1);
    await assert.rejects(sync(pool,a,page("2", { ...add, data:{ ...add.data,name:"Different" } })), InvalidRequest);

    const simultaneous = await Promise.all([
      sync(pool,a,page("2",feed("Edit from A",1))),
      sync(pool,a,page("2",feed("Edit from B",1))),
    ]);
    assert.deepEqual(simultaneous.map(result => result.acks[0].conflict).sort(),[false,true]);
    const success = simultaneous.find(result => !result.acks[0].conflict);
    const removed = await sync(pool,a,page("3",feed("Removed",success.acks[0].feed.revision,true)));
    assert.equal(removed.acks[0].feed.removed,true);
    assert.equal((await sync(pool,a,page("4",feed("Stale edit",1)))).acks[0].conflict,true);

    const archive = op("status",id,{status:2});
    const archived = await sync(pool,a,page("4",archive,op("status",id,{status:1}),op("item",id,item(id))));
    assert.equal(archived.changes[0].data.status,2);
    const replay = await sync(pool,a,page(archived.cursor,archive));
    assert.deepEqual(replay.changes,[]);
    assert.equal((await sync(pool,b,page("0",op("item",id,item(id))))).changes[0].data.status,0);

    const batch = n => Array.from({length:n},(_,i) => {
      const id = digest("item-"+n+"-"+i);
      return op("item",id,item(id));
    });
    const start = await sync(pool,c,page("0",...batch(50)));
    await sync(pool,c,page(start.cursor,...batch(2)));
    await runSQL("UPDATE focusgate_accounts SET retained_from=revision WHERE account=$1",[c]);
    const restored = await sync(pool,c,page("1"));
    assert.equal(restored.more,true);
    assert.match(restored.cursor,/^s:/);
    assert.equal(restored.changes.length,50);
    const rest = await sync(pool,c,page(restored.cursor));
    assert.equal(rest.more,false);
    assert.equal(rest.changes.length,2);
    assert.equal(rest.cursor,"52");
  } finally {
    await runSQL("DELETE FROM focusgate_accounts WHERE account IN ($1,$2,$3)",[a,b,c]).catch(() => {});
    await stop();
  }
});
