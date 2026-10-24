// Нагрузка по пути заказа: POST /order + GET /orders с постоянной интенсивностью.
//   k6 run -e BASE_URL=http://<IP ноды>:30080 -e RATE=20 -e DURATION=5m load/order.js
import http from "k6/http";
import { check } from "k6";

const BASE = __ENV.BASE_URL || "http://172.18.0.2:30080";

export const options = {
  scenarios: {
    orders: {
      // Постоянная интенсивность, а не «сколько успеют»: под нагрузкой запросов не становится меньше.
      executor: "constant-arrival-rate",
      rate: Number(__ENV.RATE || 20),        // заказов в секунду
      timeUnit: "1s",
      duration: __ENV.DURATION || "3m",
      preAllocatedVUs: 20,
      maxVUs: 100,
    },
  },
  // SLA: 95% заказов быстрее 300 мс и меньше 1% ошибок. k6 сам скажет, выполнен ли.
  thresholds: {
    "http_req_duration{name:create_order}": ["p(95)<300"],
    http_req_failed: ["rate<0.01"],
  },
};

export default function () {
  const r = http.post(`${BASE}/order?item=load-${__VU}-${__ITER}`, null, { tags: { name: "create_order" } });
  check(r, { "order created": (res) => res.status === 201 });
  if (__ITER % 5 === 0) {
    http.get(`${BASE}/orders`, { tags: { name: "list_orders" } });
  }
}
