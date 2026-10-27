// Koro load test (OPS-005): k6 entry point. Run by
// .github/workflows/loadtest.yml against loadtest/compose.yaml; see
// loadtest/README.md for the scenarios, the thresholds and how to read the
// results. Configuration: lib/config.js (k6 -e variables).
import { env, enabledScenarios, scenarios, thresholds } from './lib/config.js';
import { markdown } from './lib/summary.js';

export { setup } from './lib/setup.js';
export { browse, signup, register, checkin, orders, discovery } from './lib/scenarios.js';

export const options = {
  scenarios: scenarios(),
  thresholds: thresholds(),
  // setup signs in every demo student (argon2id) and creates the events.
  setupTimeout: '10m',
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)', 'count'],
  // a reused connection per VU, as a mobile client keeps one.
  noConnectionReuse: false,
  discardResponseBodies: false,
  userAgent: 'koro-loadtest/OPS-005 (k6)',
};

export function handleSummary(data) {
  const md = markdown(data, {
    profile: env.profile, scenarios: enabledScenarios(), rateLimits: env.rateLimits,
    setupFailed: data.setup_data === undefined || data.setup_data === null,
  });
  // setup_data holds session tokens of the throwaway environment: keep it
  // out of the uploaded summary anyway.
  const clean = Object.assign({}, data);
  delete clean.setup_data;
  return {
    stdout: `\n${md}\n`,
    [`${env.summaryDir}/OPS-005-summary.md`]: md,
    [`${env.summaryDir}/OPS-005-summary.json`]: JSON.stringify(clean, null, 2),
  };
}
