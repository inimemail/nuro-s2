import http from "k6/http";
import { check, sleep } from "k6";

const vus = Number(__ENV.LOAD_VUS || "1000");
const duration = __ENV.LOAD_DURATION || "10m";

export const options = {
  scenarios: {
    streams: {
      executor: "constant-vus",
      vus,
      duration,
      gracefulStop: "2m",
    },
  },
  noConnectionReuse: false,
  userAgent: "sub2api-capacity-validation/1.0",
};

export default function () {
  const response = http.post(
    `${__ENV.BASE_URL}/v1/responses`,
    JSON.stringify({
      model: __ENV.LOAD_MODEL || "gpt-5.6",
      input: "Reply with ten short numbered lines.",
      stream: true,
    }),
    {
      headers: {
        Authorization: `Bearer ${__ENV.API_KEY}`,
        "Content-Type": "application/json",
        Accept: "text/event-stream",
      },
      timeout: "10m",
    },
  );
  check(response, {
    "stream accepted": (r) => r.status === 200,
    "no leaked upstream html": (r) => !/<html|cloudflare|authorization:/i.test(r.body || ""),
  });
  sleep(0.05);
}

