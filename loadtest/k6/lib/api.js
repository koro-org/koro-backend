// HTTP helpers of the Koro API (OPS-005): mobile bearer sessions
// (X-Koro-Client: mobile; no cookie, so no CSRF), Idempotency-Key, a
// synthetic client IP per simulated user, `name`/`kind` tags and the
// statuses each call expects (everything else counts as http_req_failed).
import http from 'k6/http';
import exec from 'k6/execution';
import { sleep } from 'k6';
import { Counter } from 'k6/metrics';
import { env, ENDPOINTS } from './config.js';

// Custom metrics are declared here, in the init context of every VU, so
// thresholds on them are valid whichever scenarios run.
export const metrics = {
  rateLimited: new Counter('koro_rate_limited'),
};

let seq = 0;

// clientIP maps n to the RFC 2544 benchmarking range 198.18.0.0/15
// (131072 addresses, never routed): the X-Forwarded-For of a simulated
// client. The API trusts it only from the compose network's gateway.
export function clientIP(n) {
  const v = Math.abs(Math.floor(n)) % 131072;
  return `198.${18 + (v >> 16)}.${(v >> 8) & 255}.${v & 255}`;
}

function hex(n) {
  let s = '';
  for (let i = 0; i < n; i++) s += Math.floor(Math.random() * 16).toString(16);
  return s;
}

// uuid returns a random version 4 UUID (client_scan_id is format: uuid).
export function uuid() {
  const h = hex(32).split('');
  h[12] = '4';
  h[16] = '89ab'[Math.floor(Math.random() * 4)];
  const s = h.join('');
  return `${s.slice(0, 8)}-${s.slice(8, 12)}-${s.slice(12, 16)}-${s.slice(16, 20)}-${s.slice(20)}`;
}

// idemKey is unique per VU and call ([A-Za-z0-9_-]{8,255}).
export function idemKey() {
  seq++;
  let vu = 0;
  try {
    vu = exec.vu.idInTest;
  } catch (_) {
    vu = 0; // setup / teardown
  }
  return `lt-${env.runID}-${vu}-${seq}-${hex(8)}`;
}

// call sends one request. opts: token, body (JSON), ip (client number or
// dotted address), idem (true: a fresh Idempotency-Key), headers, expect
// (statuses that are a correct answer; default [200]), name (endpoint tag).
export function call(method, path, opts) {
  const o = opts || {};
  const name = o.name || 'other';
  const expect = (o.expect || [200]).slice();
  if (env.rateLimits && !expect.includes(429)) expect.push(429);
  const headers = Object.assign({ 'X-Koro-Client': 'mobile', Accept: 'application/json' }, o.headers || {});
  if (o.token) headers.Authorization = `Bearer ${o.token}`;
  if (o.ip !== undefined && o.ip !== null) {
    headers['X-Forwarded-For'] = typeof o.ip === 'string' ? o.ip : clientIP(o.ip);
  }
  if (o.idem) headers['Idempotency-Key'] = idemKey();
  let body = null;
  if (o.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(o.body);
  }
  const params = {
    headers,
    tags: { name, kind: ENDPOINTS[name] || o.kind || 'setup' },
    responseCallback: http.expectedStatuses(...expect),
    timeout: o.timeout || '30s',
  };
  let res = http.request(method, env.baseURL + path, body, params);
  // setup's own writes (one club admin creating every event) may exceed
  // an actor limit when rate limits are on: wait as told and retry.
  for (let i = 0; res.status === 429 && name.startsWith('setup') && i < 10; i++) {
    sleep(Math.min(60, parseInt(res.headers['Retry-After'] || '5', 10) || 5));
    if (o.idem) headers['Idempotency-Key'] = idemKey();
    res = http.request(method, env.baseURL + path, body, params);
  }
  if (res.status === 429) metrics.rateLimited.add(1, { name });
  return res;
}

// json returns the parsed body of res, or null.
export function json(res) {
  try {
    return res.body ? JSON.parse(res.body) : null;
  } catch (_) {
    return null;
  }
}

// code is the problem code of an RFC 9457 answer ("" when none).
export function code(res) {
  const b = json(res);
  return (b && b.code) || '';
}

// must fails setup with a readable message when res is not `want`.
export function must(res, want, what) {
  if (res.status !== want) {
    throw new Error(`${what}: HTTP ${res.status} (want ${want}): ${String(res.body).slice(0, 400)}`);
  }
  return json(res);
}

// login signs in as a mobile client and returns the bearer token. Setup
// passes another name, so its sign-ins stay out of the auth thresholds.
export function login(email, password, ip, name) {
  const res = call('POST', '/api/v1/auth/sessions', { body: { email, password }, ip, name: name || 'login' });
  const b = must(res, 200, `sign-in of ${email}`);
  if (!b || !b.token) throw new Error(`sign-in of ${email}: no token`);
  return b.token;
}

// pick returns a uniformly random element of list.
export function pick(list) {
  return list[Math.floor(Math.random() * list.length)];
}

// weighted picks a key of {key: weight}.
export function weighted(table) {
  let total = 0;
  for (const k in table) total += table[k];
  let r = Math.random() * total;
  for (const k in table) {
    r -= table[k];
    if (r < 0) return k;
  }
  return Object.keys(table)[0];
}
