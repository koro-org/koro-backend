// Scenario functions (OPS-005; loadtest/README.md "Senaryolar"). Each is
// one iteration of one simulated user; `data` is what setup() returned.
import http from 'k6/http';
import exec from 'k6/execution';
import { check, sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { env } from './config.js';
import { call, code, json, pick, uuid, weighted } from './api.js';

export const custom = {
  signupEmail: new Trend('koro_signup_email_ms', true),
  signupEmailMissing: new Counter('koro_signup_email_missing'),
  registrationRepeats: new Counter('koro_registration_repeats'),
  checkinResults: new Counter('koro_checkin_results'),
  ordersSoldOut: new Counter('koro_orders_sold_out'),
  discoveryHits: new Counter('koro_discovery_results'),
};

function rfc3339(d) {
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

// --- browse: reading students and anonymous visitors (closed model) ---

const EVENT_FILTERS = ['', '', '&category=workshop', '&city=Ankara', '&q=kariyer'];

function eventsList(token, ip) {
  const res = call('GET', `/api/v1/events?limit=20${pick(EVENT_FILTERS)}`, { token, ip, name: 'events_list' });
  check(res, { 'events list 200': (r) => r.status === 200 });
  const b = json(res);
  if (b && b.next_cursor && Math.random() < 0.2) {
    call('GET', `/api/v1/events?limit=20&cursor=${encodeURIComponent(b.next_cursor)}`, { token, ip, name: 'events_list' });
  }
}

function eventDetail(data, token, ip) {
  if (!data.browseEvents || data.browseEvents.length === 0) return eventsList(token, ip);
  const res = call('GET', `/api/v1/events/${pick(data.browseEvents)}`, { token, ip, name: 'event_detail' });
  check(res, { 'event detail 200': (r) => r.status === 200 });
}

export function browse(data) {
  const vu = exec.vu.idInTest;
  const ip = vu;
  // every fifth visitor is not signed in (public event pages).
  const student = vu % 5 === 0 || data.students.length === 0 ? null : data.students[vu % data.students.length];
  if (!student) {
    if (Math.random() < 0.6) eventsList(null, ip);
    else eventDetail(data, null, ip);
  } else {
    const token = student.token;
    switch (weighted({ feed: 35, events_list: 20, event_detail: 20, my_registrations: 10, clubs_list: 8, me: 7 })) {
      case 'feed': {
        const res = call('GET', '/api/v1/feed?limit=20', { token, ip, name: 'feed' });
        check(res, { 'feed 200': (r) => r.status === 200 });
        const b = json(res);
        if (b && b.next_cursor && Math.random() < 0.25) {
          call('GET', `/api/v1/feed?limit=20&cursor=${encodeURIComponent(b.next_cursor)}`, { token, ip, name: 'feed' });
        }
        break;
      }
      case 'events_list':
        eventsList(token, ip);
        break;
      case 'event_detail':
        eventDetail(data, token, ip);
        break;
      case 'my_registrations':
        check(call('GET', '/api/v1/me/registrations?limit=20', { token, ip, name: 'my_registrations' }),
          { 'my registrations 200': (r) => r.status === 200 });
        break;
      case 'clubs_list':
        check(call('GET', '/api/v1/clubs?limit=20', { token, ip, name: 'clubs_list' }), { 'clubs 200': (r) => r.status === 200 });
        break;
      default:
        check(call('GET', '/api/v1/me', { token, ip, name: 'me' }), { 'me 200': (r) => r.status === 200 });
    }
  }
  sleep(3 + Math.random() * 4); // think time: 3–7 s
}

// --- signup: register → verification e-mail → confirm → sign in ---

const FIRST = ['Deniz', 'Ege', 'Ada', 'Kaan', 'Elif', 'Mert', 'Zeynep', 'Can'];
const LAST = ['Yılmaz', 'Kaya', 'Demir', 'Şahin', 'Çelik', 'Aydın'];
const TOKEN_RE = /#token=([A-Za-z0-9_-]+)/;

function mailpitGet(path) {
  return http.get(env.mailpitURL + path, { tags: { name: 'mailpit', kind: 'mailpit' }, timeout: '10s' });
}

// verificationToken polls Mailpit for the link e-mail to `to` (≤ 30 s).
function verificationToken(to) {
  const query = encodeURIComponent(`to:"${to}"`);
  const deadline = Date.now() + 30000;
  while (Date.now() < deadline) {
    const list = json(mailpitGet(`/api/v1/search?query=${query}`)) || {};
    const msg = (list.messages || list.Messages || [])[0];
    if (msg) {
      const full = json(mailpitGet(`/api/v1/message/${msg.ID || msg.id}`)) || {};
      const m = TOKEN_RE.exec(full.Text || full.text || '');
      if (m) return m[1];
    }
    sleep(0.25);
  }
  return '';
}

export function signup(data) {
  const i = exec.scenario.iterationInTest;
  // one synthetic client per sign-up (auth_register is 5/hour per IP).
  const ip = 50000 + (i % 50000);
  const email = `yuk.${env.runID}.${i}@example.test`;
  // random: the policy refuses a password that contains a part of the
  // e-mail address (identity/password.go).
  const password = `Koro-Kayit-${uuid().slice(0, 23)}`;
  const reg = call('POST', '/api/v1/auth/register', {
    ip, name: 'register_account', expect: [202], body: {
      email, password, first_name: pick(FIRST), last_name: pick(LAST), birth_date: '2004-05-20', locale: 'tr-TR',
      signup_persona: 'student', privacy_notice_version_id: data.privacyNotice,
    },
  });
  if (!check(reg, { 'register 202': (r) => r.status === 202 })) return;
  const t0 = Date.now();
  const token = verificationToken(email);
  if (!check(token, { 'verification e-mail arrived': (t) => t !== '' })) {
    custom.signupEmailMissing.add(1);
    return;
  }
  custom.signupEmail.add(Date.now() - t0);
  const conf = call('POST', '/api/v1/auth/email-verifications/confirm', { ip, name: 'confirm_email', expect: [204], body: { token } });
  if (!check(conf, { 'email confirmed 204': (r) => r.status === 204 })) return;
  const login = call('POST', '/api/v1/auth/sessions', { ip, name: 'login', body: { email, password } });
  const b = json(login);
  if (!check(login, { 'login 200 with token': (r) => r.status === 200 && b && b.token })) return;
  check(call('GET', '/api/v1/me', { token: b.token, ip, name: 'me' }), { 'me 200': (r) => r.status === 200 });
}

// --- register: a student registers for an event (open model) ---

export function register(data) {
  const i = exec.scenario.iterationInTest;
  const n = data.students.length;
  const s = data.students[i % n];
  const ev = data.regEvents[Math.floor(i / n) % data.regEvents.length];
  const ip = 20000 + (i % n);
  const res = call('POST', `/api/v1/events/${ev.id}/registrations`, {
    token: s.token, ip, idem: true, name: 'event_registration', expect: [201, 409],
    body: { form_version_id: ev.form, answers: { beklenti: 'Yeni şeyler öğrenmek' } },
  });
  if (res.status === 409) {
    // the run outlived the distinct (student, event) pairs of setup.
    if (check(res, { 'repeat registration is already_registered': (r) => code(r) === 'events.already_registered' })) {
      custom.registrationRepeats.add(1);
    }
    return;
  }
  const b = json(res);
  if (!check(res, { 'registration confirmed': (r) => r.status === 201 && b && b.status === 'confirmed' })) return;
  if (Math.random() < 0.3) {
    check(call('GET', `/api/v1/me/registrations/${b.registration_id}/ticket`, { token: s.token, ip, name: 'my_ticket' }),
      { 'ticket 200': (r) => r.status === 200 });
  }
}

// --- checkin: door staff scan QR tickets (open model) ---

export function checkin(data) {
  const i = exec.scenario.iterationInTest;
  const ticket = data.tickets[i % data.tickets.length];
  const scanner = data.scanners[i % data.scanners.length];
  const ip = 70000 + (i % data.scanners.length);
  const res = call('POST', `/api/v1/events/${data.checkinEvent}/check-ins`, {
    token: scanner, ip, idem: true, name: 'checkin',
    body: { client_scan_id: uuid(), method: 'qr', token: ticket, scanned_at: rfc3339(new Date()), device_id: `kapi-${i % data.scanners.length}` },
  });
  const b = json(res);
  const result = (b && b.result) || 'none';
  // the first scan of a ticket is ok; the run re-scans tickets once all
  // were scanned (a door's double scans): already_checked_in.
  check(res, { 'scan ok or already checked in': (r) => r.status === 200 && (result === 'ok' || result === 'already_checked_in') });
  custom.checkinResults.add(1, { result });
  if (Math.random() < 0.2) {
    check(call('GET', `/api/v1/events/${data.checkinEvent}/check-ins/stats`, { token: scanner, ip, name: 'checkin_stats' }),
      { 'check-in stats 200': (r) => r.status === 200 });
  }
}

// --- orders: the company orders, reads the bank transfer reference, cancels ---

export function orders(data) {
  const c = data.company;
  const ip = 80000;
  const res = call('POST', `/api/v1/orgs/${c.org}/orders`, {
    token: c.owner, ip, idem: true, name: 'order_create', expect: [201, 409],
    body: { buyer_party_id: c.party, lines: [{ offering_id: pick(c.offerings), quantity: 1 }] },
  });
  if (res.status === 409) {
    // the demo offerings have 9 units; every order is cancelled right
    // after, so this only happens when orders overlap.
    if (check(res, { 'conflict is sold_out': (r) => code(r) === 'catalog.sold_out' })) custom.ordersSoldOut.add(1);
    return;
  }
  const b = json(res);
  const order = b && b.orders && b.orders[0];
  if (!check(res, { 'order awaiting payment': () => res.status === 201 && order && order.status === 'awaiting_payment' })) return;
  const ins = call('GET', `/api/v1/orders/${order.order_id}/payment-instructions`, { token: c.owner, ip, name: 'payment_instructions' });
  const insB = json(ins);
  check(ins, {
    'transfer reference is the order number': (r) => r.status === 200 && insB && insB.reference_code === order.order_no && !!insB.iban,
  });
  check(call('GET', `/api/v1/orders/${order.order_id}`, { token: c.owner, ip, name: 'order_detail' }), { 'order 200': (r) => r.status === 200 });
  const can = call('POST', `/api/v1/orders/${order.order_id}/cancel`, {
    token: c.owner, ip, name: 'order_cancel', body: { reason: 'Yük testi: sentetik siparişin iptali' },
  });
  check(can, { 'order cancelled': (r) => r.status === 200 && (json(r) || {}).status === 'cancelled' });
}

// --- discovery: recruiters page through anonymous student profiles ---

export function discovery(data) {
  const c = data.company;
  const token = Math.random() < 0.7 ? c.recruiter : c.owner;
  const ip = 81000 + (token === c.owner ? 1 : 0);
  let q = 'limit=20';
  const r = Math.random();
  if (data.universities.length > 0 && r < 0.6) q += `&university_id=${pick(data.universities)}`;
  if (r > 0.4) q += `&class_year=${1 + Math.floor(Math.random() * 4)}`;
  const res = call('GET', `/api/v1/orgs/${c.org}/discovery/profiles?${q}`, { token, ip, name: 'discovery_search' });
  const b = json(res);
  check(res, { 'discovery search 200': (x) => x.status === 200 });
  const items = (b && b.items) || [];
  custom.discoveryHits.add(items.length);
  if (items.length > 0 && Math.random() < 0.3) {
    check(call('GET', `/api/v1/orgs/${c.org}/discovery/profiles/${pick(items).anon_id}`, { token, ip, name: 'discovery_profile' }),
      { 'discovery profile 200': (x) => x.status === 200 });
  }
}
