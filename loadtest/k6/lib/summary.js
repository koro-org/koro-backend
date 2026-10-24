// handleSummary output (OPS-005): a Markdown report for the GitHub job
// summary (and stdout) plus the raw summary JSON. Pure JS, no remote jslib.
import { ENDPOINTS } from './config.js';

function ms(v) {
  if (v === undefined || v === null || Number.isNaN(v)) return '–';
  return v >= 1000 ? `${(v / 1000).toFixed(2)} s` : `${v.toFixed(v < 10 ? 1 : 0)} ms`;
}

function pct(v) {
  return v === undefined ? '–' : `${(v * 100).toFixed(2)} %`;
}

function val(m, key) {
  return m && m.values ? m.values[key] : undefined;
}

function thresholdRows(metrics) {
  const rows = [];
  for (const name of Object.keys(metrics).sort()) {
    const th = metrics[name].thresholds;
    if (!th) continue;
    for (const expr of Object.keys(th)) {
      if (expr === 'max>=0') continue; // per-endpoint table only
      rows.push(`| ${th[expr].ok ? 'geçti' : '**KALDI**'} | \`${name}\` | \`${expr}\` |`);
    }
  }
  return rows;
}

function failedChecks(group, out) {
  for (const c of group.checks || []) {
    if (c.fails > 0) out.push(`| ${c.name} | ${c.passes} | ${c.fails} |`);
  }
  for (const g of group.groups || []) failedChecks(g, out);
  return out;
}

export function markdown(data, meta) {
  const m = data.metrics || {};
  const durS = ((data.state && data.state.testRunDurationMs) || 0) / 1000;
  const reqs = val(m.http_reqs, 'count') || 0;
  const lines = [];
  const failed = thresholdRows(m).filter((r) => r.includes('KALDI')).length;
  lines.push(`## Koro yük testi — ${meta.profile} (${meta.scenarios.join(', ')})`);
  lines.push('');
  lines.push(`Sonuç: ${failed === 0 ? '**tüm eşikler geçti**' : `**${failed} eşik kaldı**`} · süre ${Math.round(durS)} s · ` +
    `${reqs} istek (${durS > 0 ? (reqs / durS).toFixed(1) : '–'} istek/s) · beklenmeyen yanıt ${pct(val(m.http_req_failed, 'rate'))} · ` +
    `check ${pct(val(m.checks, 'rate'))} · en çok ${val(m.vus_max, 'max') || val(m.vus_max, 'value') || '–'} VU` +
    `${meta.rateLimits ? ' · rate limit **açık**' : ' · rate limit kapalı'}`);
  lines.push('');
  lines.push('### Eşikler');
  lines.push('');
  lines.push('| durum | metrik | eşik |');
  lines.push('|---|---|---|');
  lines.push(...thresholdRows(m));
  lines.push('');
  lines.push('### Uçlar');
  lines.push('');
  lines.push('| uç | tür | istek | p50 | p95 | p99 | max |');
  lines.push('|---|---|---:|---:|---:|---:|---:|');
  for (const name of Object.keys(ENDPOINTS)) {
    const t = m[`http_req_duration{name:${name}}`];
    const n = val(t, 'count');
    if (!n) continue;
    lines.push(`| ${name} | ${ENDPOINTS[name]} | ${n} | ${ms(val(t, 'med'))} | ${ms(val(t, 'p(95)'))} | ${ms(val(t, 'p(99)'))} | ${ms(val(t, 'max'))} |`);
  }
  lines.push('');
  lines.push('### Akış göstergeleri');
  lines.push('');
  const counter = (k) => val(m[k], 'count') || 0;
  const email = m.koro_signup_email_ms;
  lines.push(`- Doğrulama e-postası gecikmesi (outbox → worker → SMTP): p50 ${ms(val(email, 'med'))}, p95 ${ms(val(email, 'p(95)'))}, ` +
    `max ${ms(val(email, 'max'))}; 30 sn'de gelmeyen: ${counter('koro_signup_email_missing')}`);
  lines.push(`- Tekrarlanan kayıt (409 already_registered, beklenen): ${counter('koro_registration_repeats')}`);
  lines.push(`- Check-in taraması: ${counter('koro_checkin_results')} (ilk tarama ok, sonrası already_checked_in)`);
  lines.push(`- Tükenen teklif (409 sold_out): ${counter('koro_orders_sold_out')} · keşif sonuç satırı: ${counter('koro_discovery_results')}`);
  lines.push(`- 429 (rate limit): ${counter('koro_rate_limited')}`);
  const fails = failedChecks(data.root_group || {}, []);
  if (fails.length > 0) {
    lines.push('');
    lines.push('### Kalan check\'ler');
    lines.push('');
    lines.push('| check | geçen | kalan |');
    lines.push('|---|---:|---:|');
    lines.push(...fails);
  }
  lines.push('');
  return lines.join('\n');
}
