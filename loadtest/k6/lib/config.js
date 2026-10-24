// Run configuration (OPS-005): profiles, scenario selection and thresholds.
// Everything comes from k6 -e variables (the workflow's inputs):
//
//   PROFILE      smoke | pilot | soak | stress (default pilot)
//   SCENARIOS    "all" or a comma list of: browse, signup, register,
//                checkin, orders, discovery
//   VUS          peak virtual users of browse (default: the profile's)
//   DURATION     steady-state duration, e.g. 10m (default: the profile's)
//   RATE_LIMITS  "on" when the API runs with RATE_LIMIT_ENABLED=true: a 429
//                is then an expected answer, counted in koro_rate_limited
//   BASE_URL     API base (default http://127.0.0.1:8080)
//   MAILPIT_URL  Mailpit API base (default http://127.0.0.1:8025)
//   DEMO_PASSWORD  the `koro seed demo -password` value
//   STUDENTS_FILE  JSON array of the demo students' e-mail addresses
//                  (loadtest/students.sql)
//   SUMMARY_DIR  where handleSummary writes OPS-005-summary.{json,md}

export const ALL_SCENARIOS = ['browse', 'signup', 'register', 'checkin', 'orders', 'discovery'];

export const env = {
  profile: (__ENV.PROFILE || 'pilot').trim(),
  baseURL: (__ENV.BASE_URL || 'http://127.0.0.1:8080').replace(/\/+$/, ''),
  mailpitURL: (__ENV.MAILPIT_URL || 'http://127.0.0.1:8025').replace(/\/+$/, ''),
  demoPassword: __ENV.DEMO_PASSWORD || 'demo-demo-demo-demo-2026',
  rateLimits: (__ENV.RATE_LIMITS || 'off').trim() === 'on',
  studentsFile: __ENV.STUDENTS_FILE || '',
  summaryDir: (__ENV.SUMMARY_DIR || '.').replace(/\/+$/, ''),
  runID: (__ENV.RUN_ID || String(Date.now())).replace(/[^A-Za-z0-9]/g, '').slice(-12),
};

// Pilot scale (docs/reports/teknik-rapor §8.5 targets; pilot: 2–3
// universities, 10–20 clubs, a few thousand students): ~500 concurrent
// users at peak. browse is a closed model (VUs with 3–7 s think time,
// ~0.2 requests/s each); the write flows are open models (arrival rates)
// so a slow API cannot slow down the offered load.
//
// rates are iterations per second; stress ramps every rate ×5 and browse
// to 1000 VUs in steps to find the knee.
const PROFILES = {
  smoke: {
    ramp: '15s', steady: '1m', down: '10s', browseVUs: 10,
    rates: { signup: 0.2, register: 1, checkin: 0.5, orders: 0.1, discovery: 0.2 },
  },
  pilot: {
    ramp: '2m', steady: '10m', down: '30s', browseVUs: 400,
    // signup: a launch day (~1000 sign-ups, peaks of ~1/s); register: a
    // popular event announced to the whole university (300 seats in a
    // minute: 5/s); checkin: 300 people through 3 doors in ~3 minutes
    // (2/s); orders: rare, kept above zero to exercise the path; discovery:
    // a few recruiters paging through results.
    rates: { signup: 1, register: 5, checkin: 2, orders: 0.2, discovery: 1 },
  },
  soak: {
    // the report's 30-minute endurance test at pilot load.
    ramp: '2m', steady: '30m', down: '30s', browseVUs: 400,
    rates: { signup: 1, register: 5, checkin: 2, orders: 0.2, discovery: 1 },
  },
  stress: {
    ramp: '2m', steady: '2m', down: '30s', browseVUs: 1000, steps: 5,
    rates: { signup: 5, register: 25, checkin: 10, orders: 1, discovery: 5 },
  },
};

export function profile() {
  const p = PROFILES[env.profile];
  if (!p) {
    throw new Error(`PROFILE=${env.profile}: want one of ${Object.keys(PROFILES).join(', ')}`);
  }
  const out = JSON.parse(JSON.stringify(p));
  if (__ENV.VUS) {
    const v = parseInt(__ENV.VUS, 10);
    if (!(v > 0 && v <= 5000)) throw new Error(`VUS=${__ENV.VUS}: want 1..5000`);
    out.browseVUs = v;
  }
  if (__ENV.DURATION) {
    if (!/^[0-9]+(s|m|h)$/.test(__ENV.DURATION)) throw new Error(`DURATION=${__ENV.DURATION}: want e.g. 90s, 10m, 1h`);
    out.steady = __ENV.DURATION;
  }
  return out;
}

export function enabledScenarios() {
  const raw = (__ENV.SCENARIOS || 'all').trim();
  if (raw === '' || raw === 'all') return ALL_SCENARIOS.slice();
  const list = raw.split(',').map((s) => s.trim()).filter((s) => s !== '');
  for (const s of list) {
    if (!ALL_SCENARIOS.includes(s)) throw new Error(`SCENARIOS: unknown scenario "${s}" (want all or ${ALL_SCENARIOS.join(',')})`);
  }
  return list;
}

// seconds parses a k6 duration of the profile ("90s", "10m", "1h").
export function seconds(d) {
  const m = /^([0-9]+)(s|m|h)$/.exec(d);
  if (!m) throw new Error(`bad duration ${d}`);
  return parseInt(m[1], 10) * { s: 1, m: 60, h: 3600 }[m[2]];
}

// totalSeconds is how long the open-model scenarios offer load.
export function totalSeconds(p) {
  const steps = p.steps || 1;
  return seconds(p.ramp) + (steps - 1) * 30 + steps * seconds(p.steady) + seconds(p.down);
}

function browseScenario(p) {
  const stages = [];
  if (p.steps) {
    for (let i = 1; i <= p.steps; i++) {
      const target = Math.round((p.browseVUs * i) / p.steps);
      stages.push({ duration: i === 1 ? p.ramp : '30s', target }, { duration: p.steady, target });
    }
  } else {
    stages.push({ duration: p.ramp, target: p.browseVUs }, { duration: p.steady, target: p.browseVUs });
  }
  stages.push({ duration: p.down, target: 0 });
  return { executor: 'ramping-vus', exec: 'browse', startVUs: 0, stages, gracefulRampDown: '20s', gracefulStop: '30s' };
}

// arrival returns an open-model scenario at `rate` iterations per second
// (stress ramps from a fifth of it up to it in steps).
function arrival(name, rate, p, maxVUs) {
  // timeUnit keeps fractional rates integral (0.2/s = 12/min).
  const perMin = Math.max(1, Math.round(rate * 60));
  const base = { exec: name, timeUnit: '1m', preAllocatedVUs: Math.min(maxVUs, Math.ceil(rate * 5) + 2), maxVUs, gracefulStop: '30s' };
  if (p.steps) {
    const stages = [];
    for (let i = 1; i <= p.steps; i++) {
      const target = Math.max(1, Math.round((perMin * i) / p.steps));
      stages.push({ duration: i === 1 ? p.ramp : '30s', target }, { duration: p.steady, target });
    }
    stages.push({ duration: p.down, target: 0 });
    return Object.assign(base, { executor: 'ramping-arrival-rate', startRate: 0, stages });
  }
  return Object.assign(base, {
    executor: 'ramping-arrival-rate', startRate: 0,
    stages: [{ duration: p.ramp, target: perMin }, { duration: p.steady, target: perMin }, { duration: p.down, target: 0 }],
  });
}

export function scenarios() {
  const p = profile();
  const out = {};
  for (const name of enabledScenarios()) {
    if (name === 'browse') {
      out.browse = browseScenario(p);
      continue;
    }
    // signup waits for its e-mail (seconds) and needs more VUs in flight.
    const maxVUs = { signup: 100, register: 100, checkin: 60, orders: 20, discovery: 40 }[name];
    out[name] = arrival(name, p.rates[name], p, maxVUs);
  }
  return out;
}

// Endpoint names (the `name` tag of every request). A threshold per name
// (a no-op `max>=0` unless it has a target) makes k6 keep a submetric for
// it, so the summary can show a per-endpoint table.
export const ENDPOINTS = {
  events_list: 'read', event_detail: 'read', feed: 'read', clubs_list: 'read', me: 'read',
  my_registrations: 'read', checkin_stats: 'read', payment_instructions: 'read', order_detail: 'read',
  discovery_search: 'read', discovery_profile: 'read',
  register_account: 'auth', confirm_email: 'auth', login: 'auth',
  event_registration: 'write', my_ticket: 'read', checkin: 'write', order_create: 'write', order_cancel: 'write',
};

// Thresholds (README "Eşikler"). The technical report's pilot targets:
// reads p95 < 300 ms, writes p95 < 800 ms, errors < 1 %, measured with
// the whole stack and k6 on one 4-vCPU runner (a pessimistic setup).
export function thresholds() {
  const t = {
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
    'http_req_duration{kind:read}': ['p(95)<300', 'p(99)<1000'],
    'http_req_duration{kind:write}': ['p(95)<800', 'p(99)<2000'],
    // argon2id is slow on purpose (and bounded by PASSWORD_HASH_CONCURRENCY):
    // sign-up and sign-in may take longer than other writes.
    'http_req_duration{kind:auth}': ['p(95)<1500'],
    // a door scanner waits for this answer with a person in front of it.
    'http_req_duration{name:checkin}': ['p(95)<500'],
    // search is the heaviest read (candidate scan + ranking).
    'http_req_duration{name:discovery_search}': ['p(95)<800'],
    // the verification e-mail through outbox → worker → SMTP.
    koro_signup_email_ms: ['p(95)<15000'],
  };
  for (const name of Object.keys(ENDPOINTS)) {
    const key = `http_req_duration{name:${name}}`;
    if (!t[key]) t[key] = ['max>=0'];
  }
  return t;
}
