// k6 setup (OPS-005): prepares, through the real API, what the scenarios
// share. Only the parts of the enabled scenarios run. Every request here
// has a `setup_*` name, so it stays out of the per-kind thresholds.
//
//   - sessions: the demo students (STUDENTS_FILE), the club admin, the
//     company owner (TOTP enrolled: orders need an MFA-verified
//     company_admin) and the recruiter (discovery, no MFA needed);
//   - events of the main club: registration events with a large capacity
//     (enough distinct student × event pairs for the whole run) and one
//     check-in event with scanner staff and pre-issued tickets;
//   - a part of the students opts in to discovery (PATCH /me/privacy with
//     the consent text loadtest/prep.sql published);
//   - the ids the reads pick from (public events, universities).
import http from 'k6/http';
import { sleep } from 'k6';
import { SharedArray } from 'k6/data';
import { env, profile, enabledScenarios, totalSeconds } from './config.js';
import { call, clientIP, idemKey, json, login, must } from './api.js';
import { totp } from './totp.js';

// Demo accounts of db/seed/seed.go (synthetic, @example.test).
const CLUB_ADMIN = 'demo.kulup@example.test';
const COMPANY_OWNER = 'demo.sirket@example.test';
const RECRUITER = 'demo.ik@example.test';

// Client numbers of clientIP(): setup sign-ins use their own range.
const SETUP_IP = 100000;

const students = new SharedArray('students', () => {
  if (!env.studentsFile) return [];
  try {
    const list = JSON.parse(open(env.studentsFile));
    return Array.isArray(list) ? list.filter((e) => typeof e === 'string' && e.endsWith('@example.test')) : [];
  } catch (e) {
    console.warn(`STUDENTS_FILE ${env.studentsFile}: ${e}`);
    return [];
  }
});

const SCANNERS = 3; // door staff of the check-in event
const MAX_TICKETS = 250; // pre-issued tickets of the check-in event
const DISCOVERABLE = 60; // students that opt in to discovery
const MAX_REG_EVENTS = 60;

function waitReady() {
  for (let i = 0; i < 60; i++) {
    const res = http.get(`${env.baseURL}/readyz`, { tags: { name: 'setup_readyz', kind: 'setup' } });
    if (res.status === 200) return;
    sleep(2);
  }
  throw new Error('the API did not become ready (GET /readyz) in 120 s');
}

// batch runs reqs ([method, path, opts]) in parallel groups of `width`.
function batch(reqs, width) {
  const out = [];
  for (let i = 0; i < reqs.length; i += width) {
    const group = reqs.slice(i, i + width).map(([method, path, o]) => {
      const headers = { 'X-Koro-Client': 'mobile', Accept: 'application/json' };
      if (o.token) headers.Authorization = `Bearer ${o.token}`;
      if (o.ip !== undefined) headers['X-Forwarded-For'] = clientIP(o.ip);
      if (o.idem) headers['Idempotency-Key'] = idemKey();
      let body = null;
      if (o.body !== undefined) {
        headers['Content-Type'] = 'application/json';
        body = JSON.stringify(o.body);
      }
      return { method, url: env.baseURL + path, body, params: { headers, tags: { name: o.name, kind: 'setup' }, timeout: '60s' } };
    });
    for (const r of http.batch(group)) out.push(r);
  }
  return out;
}

function signInStudents(emails) {
  const res = batch(emails.map((email, i) => ['POST', '/api/v1/auth/sessions', {
    body: { email, password: env.demoPassword }, ip: SETUP_IP + i, name: 'setup_login',
  }]), 8);
  const out = [];
  res.forEach((r, i) => {
    const b = json(r);
    if (r.status === 200 && b && b.token) {
      out.push({ email: emails[i], token: b.token });
    } else {
      console.warn(`sign-in of a demo student failed: HTTP ${r.status}`);
    }
  });
  return out;
}

function membership(token, role) {
  const me = must(call('GET', '/api/v1/me', { token, name: 'setup_me' }), 200, 'GET /me');
  const m = (me.memberships || []).find((x) => x.role === role);
  if (!m) throw new Error(`no ${role} membership in /me`);
  return m.org_id;
}

// createEvent creates and publishes an event of org with a one-field form
// (as cmd/koro/smoke_integration_test.go) and returns {id, form}.
function createEvent(token, org, title, startsAt, capacity) {
  const ev = must(call('POST', `/api/v1/orgs/${org}/events`, {
    token, name: 'setup_event', expect: [201], body: {
      title, category: 'workshop', format: 'in_person', city: 'Ankara', location_name: 'Yük Testi Salonu',
      starts_at: startsAt.toISOString().replace(/\.\d{3}Z$/, 'Z'),
      ends_at: new Date(startsAt.getTime() + 2 * 3600 * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z'),
      capacity, checkin_enabled: true, visibility: 'public', registration_mode: 'auto',
    },
  }), 201, `create event ${title}`);
  const id = ev.event_id;
  const draft = call('GET', `/api/v1/events/${id}/form/draft`, { token, name: 'setup_event' });
  must(draft, 200, 'get form draft');
  must(call('PUT', `/api/v1/events/${id}/form/draft`, {
    token, name: 'setup_event', headers: { 'If-Match': draft.headers.Etag || draft.headers.ETag || '' }, body: {
      draft_schema: {
        schema_version: 1, fields: [{ key: 'beklenti', type: 'short_text', label: { tr: 'Etkinlikten beklentin' }, required: false }],
      },
    },
  }), 200, 'put form draft');
  const form = must(call('POST', `/api/v1/events/${id}/form/versions`, { token, name: 'setup_event', body: {}, expect: [201] }),
    201, 'publish form').form_version_id;
  const pub = must(call('POST', `/api/v1/events/${id}/publish`, { token, name: 'setup_event' }), 200, 'publish event');
  if (pub.status !== 'published') throw new Error(`publish event: status ${pub.status}`);
  return { id, form };
}

function enrolTOTP(token) {
  const enr = must(call('POST', '/api/v1/me/mfa/totp', {
    token, name: 'setup_mfa', body: { reauth: { password: env.demoPassword } },
  }), 200, 'enrol TOTP');
  must(call('POST', '/api/v1/me/mfa/totp/confirm', {
    token, name: 'setup_mfa', body: { code: totp(enr.secret_base32), reauth: { password: env.demoPassword } },
  }), 200, 'confirm TOTP');
}

export function setup() {
  const want = new Set(enabledScenarios());
  const p = profile();
  waitReady();
  const data = { profile: env.profile, scenarios: Array.from(want), students: [] };

  // --- reads: public events and universities ---
  const evs = must(call('GET', '/api/v1/events?limit=50', { name: 'setup_events' }), 200, 'list events');
  data.browseEvents = (evs.items || []).map((e) => e.event_id);
  const unis = must(call('GET', '/api/v1/universities?limit=10', { name: 'setup_unis' }), 200, 'list universities');
  data.universities = (unis.items || []).map((u) => u.university_id);

  // --- sign-up needs the current privacy notice ---
  if (want.has('signup')) {
    const n = must(call('GET', '/api/v1/notices/current?audience=student&kind=privacy_notice', { name: 'setup_notice' }),
      200, 'current privacy notice');
    if (!n.items || n.items.length === 0) throw new Error('no current privacy notice (the demo seed publishes one)');
    data.privacyNotice = n.items[0].notice_version_id;
  }

  // --- students ---
  const needStudents = ['browse', 'register', 'checkin', 'discovery'].some((s) => want.has(s));
  if (needStudents) {
    if (students.length < SCANNERS + 10) {
      throw new Error(`STUDENTS_FILE has ${students.length} students; run loadtest/students.sql first (README)`);
    }
    data.students = signInStudents(Array.from(students));
    if (data.students.length < SCANNERS + 10) throw new Error(`only ${data.students.length} demo students signed in`);
  }

  // --- club events ---
  if (want.has('register') || want.has('checkin')) {
    const club = login(CLUB_ADMIN, env.demoPassword, SETUP_IP + 90000, 'setup_login');
    const org = membership(club, 'club_admin');
    const base = Date.now();
    if (want.has('register')) {
      // enough distinct (student, event) pairs for every registration of
      // the run; past that, a pair repeats and the answer is 409
      // events.already_registered (an expected answer, counted).
      const expected = Math.ceil(p.rates.register * totalSeconds(p));
      const n = Math.min(MAX_REG_EVENTS, Math.ceil(expected / data.students.length) + 1);
      data.regEvents = [];
      for (let i = 0; i < n; i++) {
        const starts = new Date(base + (7 * 24 + i) * 3600 * 1000);
        starts.setUTCSeconds(0, 0);
        data.regEvents.push(createEvent(club, org, `Yük Testi Etkinliği ${env.runID}-${i + 1}`, starts, 100000));
      }
      data.browseEvents = data.browseEvents.concat(data.regEvents.map((e) => e.id));
    }
    if (want.has('checkin')) {
      const starts = new Date(base + 72 * 3600 * 1000);
      starts.setUTCSeconds(0, 0);
      const ev = createEvent(club, org, `Yük Testi Kapı Girişi ${env.runID}`, starts, 100000);
      data.checkinEvent = ev.id;
      const staff = data.students.slice(-SCANNERS);
      for (const s of staff) {
        must(call('POST', `/api/v1/events/${ev.id}/staff`, {
          token: club, name: 'setup_staff', expect: [201], body: { email: s.email, role: 'scanner' },
        }), 201, 'add scanner');
      }
      data.scanners = staff.map((s) => s.token);
      const holders = data.students.slice(0, Math.min(MAX_TICKETS, data.students.length - SCANNERS));
      const regs = batch(holders.map((s) => ['POST', `/api/v1/events/${ev.id}/registrations`, {
        token: s.token, idem: true, name: 'setup_register', body: { form_version_id: ev.form, answers: { beklenti: 'Kapı testi' } },
      }]), 8);
      const ids = [];
      regs.forEach((r, i) => {
        const b = json(r);
        if (r.status === 201 && b) ids.push([holders[i].token, b.registration_id]);
        else console.warn(`check-in event registration: HTTP ${r.status}`);
      });
      const tickets = batch(ids.map(([token, id]) => ['GET', `/api/v1/me/registrations/${id}/ticket`, { token, name: 'setup_ticket' }]), 8);
      data.tickets = tickets.map((r) => (r.status === 200 && json(r) ? json(r).qr_payload : null)).filter((t) => t);
      if (data.tickets.length === 0) throw new Error('no ticket of the check-in event');
    }
  }

  // --- company: orders (MFA) and discovery ---
  if (want.has('orders') || want.has('discovery')) {
    const owner = login(COMPANY_OWNER, env.demoPassword, SETUP_IP + 90001, 'setup_login');
    enrolTOTP(owner);
    const org = membership(owner, 'company_owner');
    data.company = { org, owner };
    if (want.has('orders')) {
      const parties = must(call('GET', `/api/v1/orgs/${org}/legal-parties`, { token: owner, name: 'setup_parties' }), 200, 'legal parties');
      const party = (parties.items || []).find((x) => x.kind === 'buyer_company' && x.status === 'verified');
      if (!party) throw new Error('the demo company has no verified buyer party');
      const offs = must(call('GET', '/api/v1/offerings?limit=100', { token: owner, name: 'setup_offerings' }), 200, 'offerings');
      // Model A bank-transfer offerings of the demo seed (db/seed/commerce.go).
      const codes = ['demo_targeted_post', 'demo_club_sponsorship'];
      const offerings = (offs.items || []).filter((o) => o.status === 'active' && o.service && codes.includes(o.service.code))
        .map((o) => o.offering_id);
      if (offerings.length === 0) throw new Error('no demo Model A offering on sale');
      data.company.party = party.party_id;
      data.company.offerings = offerings;
    }
    if (want.has('discovery')) {
      data.company.recruiter = login(RECRUITER, env.demoPassword, SETUP_IP + 90002, 'setup_login');
      const n = must(call('GET', '/api/v1/notices/current?audience=student&kind=consent_text&purpose_code=discovery_profile',
        { name: 'setup_notice' }), 200, 'discovery consent text');
      const text = (n.items || [])[0];
      if (!text) {
        console.warn('no discovery_profile consent text (loadtest/prep.sql): searches will find no student');
      } else {
        const res = batch(data.students.slice(0, DISCOVERABLE).map((s) => ['PATCH', '/api/v1/me/privacy', {
          token: s.token, name: 'setup_privacy', body: {
            discoverable: true, consent: { discovery_profile: { notice_version_id: text.notice_version_id, text_sha256: text.body_sha256 } },
          },
        }]), 8);
        const ok = res.filter((r) => r.status === 200).length;
        if (ok === 0) console.warn(`discovery opt-in failed: HTTP ${res[0] && res[0].status}: ${res[0] && String(res[0].body).slice(0, 300)}`);
        data.discoverable = ok;
      }
    }
  }
  console.log(`setup: ${data.students.length} students, ${(data.regEvents || []).length} registration events, ` +
    `${(data.tickets || []).length} tickets, ${data.discoverable || 0} discoverable, ${data.browseEvents.length} events to read`);
  return data;
}
