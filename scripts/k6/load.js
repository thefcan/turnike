// load.js — the one parameterized k6 scenario for scripts/load_test.sh.
//
// Two shapes, selected by SCENARIO:
//   sustained — constant arrival RATE for DURATION against a route whose
//               quota is far above arrival: nothing is denied, so the
//               percentiles measure the pure allow path.
//   burst     — same executor, but setup() first busy-waits until
//               START_EPOCH_S on this container's clock (the same VM
//               kernel clock redis TIME reads, so the wait is on the
//               clock that decides windows); the shell computes that
//               instant from redis TIME so the ~6s burst straddles a
//               fixed_window epoch-grid boundary.
//
// Numbers discipline: this script never aggregates away anything —
// handleSummary dumps k6's full end-of-test summary as JSON into /out,
// and the per-status Counters below give the runner exact admitted /
// denied counts to assert against. 429 is an EXPECTED status (denial is
// the system working), so it must not count into http_req_failed.
import http from 'k6/http';
import { sleep } from 'k6';
import { Counter } from 'k6/metrics';

const route = __ENV.ROUTE;
const rate = parseInt(__ENV.RATE || '0', 10);
const duration = __ENV.DURATION;
const startEpochS = parseInt(__ENV.START_EPOCH_S || '0', 10);
const baseURL = __ENV.BASE_URL || 'http://nginx:8080';
const apiKey = __ENV.API_KEY || 'load-key';
const name = __ENV.NAME || 'run';

if (!route || !rate || !duration) {
  throw new Error('ROUTE, RATE and DURATION are required');
}

// 200 = admitted, 429 = denied: both are the gateway answering as
// designed. Anything else (a fail_closed 503, a 502) is a broken run —
// counted in status_other and failed by the runner's assertions.
http.setResponseCallback(http.expectedStatuses(200, 429));

const status200 = new Counter('status_200');
const status429 = new Counter('status_429');
const statusOther = new Counter('status_other');

export const options = {
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    load: {
      executor: 'constant-arrival-rate',
      rate: rate,
      timeUnit: '1s',
      duration: duration,
      // Well under nginx's worker_connections 256: each in-flight
      // request holds two nginx connections (client + upstream) on
      // top of idle keepalives. dropped_iterations == 0 is asserted
      // by the runner, so an undersized pool fails the run loudly
      // instead of silently thinning the arrival rate.
      preAllocatedVUs: parseInt(__ENV.PRE_VUS || '60', 10),
      maxVUs: parseInt(__ENV.MAX_VUS || '100', 10),
      gracefulStop: '10s',
    },
  },
};

export function setup() {
  // Burst timing: wait INSIDE the running process, so docker
  // create/init latency spends none of the straddle budget. The
  // scenario's arrival schedule starts right after setup returns.
  if (startEpochS > 0) {
    while (Date.now() < startEpochS * 1000) {
      sleep(0.02);
    }
  }
  // The runner greps this line from k6's log and asserts the actual
  // start landed within tolerance of the redis-TIME-derived target —
  // the recorded clock-alignment evidence for the boundary straddle.
  console.log(`scenario_start name=${name} target_epoch_s=${startEpochS} actual_start_ms=${Date.now()}`);
}

export default function () {
  const res = http.get(`${baseURL}${route}hello`, {
    headers: { 'X-API-Key': apiKey },
  });
  if (res.status === 200) {
    status200.add(1);
  } else if (res.status === 429) {
    status429.add(1);
  } else {
    statusOther.add(1);
  }
}

export function handleSummary(data) {
  return {
    [`/out/${name}.summary.json`]: JSON.stringify(data, null, 2),
  };
}
