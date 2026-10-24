# Лаба 4 — SLA критического пути под перегрузкой

Критический путь `shop` — оформление заказа (`api → postgres`). Второстепенное — `batch` («ночная аналитика»), им под давлением жертвуем. Вся настройка — через Helm-чарт `shop` из лабы 3 (скопирован в [`charts/shop`](charts/shop) и доработан): значения и шаблоны, а не разовые `kubectl`-патчи. Каждая часть — свой файл-надстройка над values в [`values/`](values), применяется `helm upgrade -f`.

Что где лежит:

- [`kind.yaml`](kind.yaml) — кластер на 2 ноды;
- [`services/`](services) — `api`, `worker` (из лабы 3) и новый `batch`;
- [`charts/shop/`](charts/shop) — чарт: разнесение, PriorityClass, PDB, `batch`, мониторинг;
- [`values/`](values) — надстройки по частям;
- [`load/order.js`](load/order.js) — нагрузка k6 по пути заказа;
- [`helm/`](helm), [`policies/`](policies) — стек мониторинга (из лабы 2) и гвардрейлы Kyverno (из лабы 3).

## Часть 0 — сервис batch

[`services/cmd/batch`](services/cmd/batch/main.go) — в бесконечном цикле крутит CPU (`BATCH_CPU_WORKERS` горутин) и держит `BATCH_MEMORY_MB` памяти. Память набирает постепенно, по 16 МБ в секунду, — чтобы давление на ноду росло плавно, а не скачком. Пишет в каждую страницу памяти, иначе ядро реально ничего не выделит (то же, что в `/eat` лабы 1). Тот же [`Dockerfile`](services/Dockerfile), что для api и worker (`APP=batch`).

В `api` (версия `1.2`) добавил гистограмму `http_request_duration_seconds` — без неё не построить график задержки для SLA (часть 8).

## Часть 1 — механизм разнесения

В [`values.yaml`](charts/shop/values.yaml) — параметр `placement.mode`: `none` / `antiAffinity` / `topologySpread` (+ `hard` и `maxSkew`). Шаблоны — блок `shop.placement` в [`_helpers.tpl`](charts/shop/templates/_helpers.tpl).

| | `podAntiAffinity` | `topologySpreadConstraints` |
|---|---|---|
| правило | «не ставь под рядом с подом с такой-то меткой» (на одну ноду, в одну зону) | «держи разницу в числе подов между доменами (нодами/зонами) не больше `maxSkew`» |
| 3 реплики на 2 нодах, жёсткий режим | третьей встать некуда — обе ноды уже заняты репликой `api` → **Pending** | 2 + 1 — разница 1 ≤ `maxSkew: 1` → **встаёт** |
| масштабирование | жёсткий режим работает, только пока реплик ≤ нод | держит равновесие при любом числе реплик |
| мягкий режим | `preferred...` — по возможности, без гарантий | `ScheduleAnyway` — то же |

Выбрал **`topologySpread`** (`maxSkew: 1`, `DoNotSchedule`): у нас 3 реплики `api` и всего 2 ноды. Жёсткий `antiAffinity` оставил бы одну реплику в Pending навсегда, мягкий — не даёт гарантий. `topologySpread` гарантирует ровно то, что нужно: реплики распределены максимально равномерно (2 + 1), и потеря любой ноды забирает не больше двух из трёх.

Ещё одна деталь, которую увидел на прогоне: правило разнесения проверяется **только при планировании**. Если реплики уже съехали на одну ноду (например, после выселения с другой), сами они не переедут — нужен перезапуск (`rollout restart`) или descheduler.

## Часть 2 — тесная площадка

Кластер kind строго на 2 ноды — [`kind.yaml`](kind.yaml). Обе ноды kind живут в одной VM (4 CPU, 8 ГБ) и по умолчанию каждая считает, что ей принадлежат все 4 ядра. `systemReserved` в конфиге kubelet «отрезает» у каждой ноды 2 CPU и 2 ГБ — под поды остаётся 2 CPU и ~5.7 ГБ на ноду, и планировщик быстро упирается в нехватку. С control-plane снял taint `NoSchedule`, чтобы обе ноды принимали обычные поды.

![ноды](screenshots/01_part2_nodes.png)

Заново поставил на этот кластер стек из лабы 2 (Prometheus, Grafana, Alertmanager, Karma — Loki/Jaeger тут не нужны), оператор CloudNativePG и Kyverno с политиками лабы 3 — гвардрейлы действуют без исключений.

`batch` — новый шаблон [`batch.yaml`](charts/shop/templates/batch.yaml), включается [`values/part2-batch.yaml`](values/part2-batch.yaml):

```bash
helm install shop charts/shop -n shop --set monitoring.enabled=true --wait
helm upgrade shop charts/shop -n shop --reuse-values -f values/part2-batch.yaml --wait
```

У `batch` всё по гвардрейлам: метки, `requests: 250m / 128Mi`, `limits: 500m / 256Mi`, образ из `registry.lab.local`, не privileged — Kyverno пропустил.

![shop с batch](screenshots/02_part2_shop_with_batch.png)

Без правил разнесения планировщик положил 2 из 3 подов `api` на `lab-worker`. Упадёт эта нода — пропадут сразу две трети `api`. Это «до» для части 4.

## Часть 3 — реальные числа вместо угадывания

Сначала нагрузка по пути заказа — k6, 5 минут по 20 заказов в секунду через NodePort ([`load/order.js`](load/order.js)):

```bash
k6 run -e BASE_URL=http://<IP ноды>:30080 -e RATE=20 -e DURATION=5m load/order.js
```

![k6 без перегрузки](screenshots/03_part3_k6_baseline.png)

Это заодно базовая линия SLA **без** перегрузки: p95 оформления заказа 0.76 мс при пороге 300 мс, 6000 заказов, 0% ошибок.

Потом [krr](https://github.com/robusta-dev/krr) против namespace `shop` (готовая сборка krr под macOS оказалась сломана — ошибка PyInstaller, — поставил из исходников; запускал на маке, к кластеру и Prometheus — через проброшенные порты):

```bash
krr simple -k ~/.kube/kind-lab.yaml -p http://localhost:9090 -n shop --history_duration 1 --points_required 10
```

![krr](screenshots/04_part3_krr.png)

Полный отчёт — [`krr/report.yaml`](krr/report.yaml). Стратегия `simple`: CPU request = p95 потребления, CPU limit — не ставить; память request и limit = максимум + 15%.

### Догадка против krr против реальности

| | моя догадка (req / lim) | krr | реально (за 1 ч с нагрузкой) |
|---|---|---|---|
| api CPU | 20m / 200m | **10m** / нет лимита | p95 2m, max 2.5m |
| api память | 32Mi / 64Mi | **100Mi** / 100Mi | max 11 МиБ |
| worker CPU | 10m / 100m | 10m / нет лимита | p95 0.6m |
| worker память | 32Mi / 64Mi | 100Mi / 100Mi | max 11 МиБ |
| batch CPU | 250m / 500m | **513m** / нет лимита | упирается в limit 500m |
| batch память | 128Mi / 256Mi | 100Mi / 100Mi | max 67 МиБ |
| postgres CPU | 50m / 500m | krr не видит | p95 10m |
| postgres память | 128Mi / 256Mi | krr не видит | max 82 МиБ |

Почему расходится:

- **Память api/worker — 100Mi при реальных 11 МиБ.** Это не измерение, а **минимальные значения krr** (`memory_min_value: 100` МБ, `cpu_min_value: 10m` в его конфиге). Реальное потребление ниже — krr подтягивает до своего пола. CPU по той же причине 10m.
- **CPU и память считаются по-разному.** CPU — p95: упёрся в CPU — процесс просто притормаживают (throttling, лаба 1 часть 3). Память — максимум + 15%: упёрся в лимит памяти — OOMKill. Ошибка по памяти дороже, поэтому берут максимум с запасом.
- **batch — 513m.** batch жрёт столько CPU, сколько дадут, и упирается в свой limit — krr честно говорит «ему нужно больше». Специально не поднимаю: batch — фоновая жертва, его requests держим низкими (часть 5).
- **postgres krr не видит** — его поды принадлежат объекту `Cluster` оператора, а krr знает только Deployment/StatefulSet/DaemonSet/Job и т.п. Посчитал тем же методом напрямую из Prometheus: p95 CPU 10m, память max 82 МиБ → +15% → 95 МиБ → минимум 100Mi. **Но** `shared_buffers` у postgres = 128MB: по мере работы база заполнит буфер и упрётся в 100Mi — OOMKill. История в 1 час этого просто не видела, как не видела бы ночную аналитику или пиковый день месяца. Поэтому для postgres оставил 256Mi: krr — хорошая отправная точка, но не истина, её надо проверять пониманием того, как устроено приложение.

Применил через [`values/part3-krr.yaml`](values/part3-krr.yaml) (откуда каждое число — в комментариях; `null` убирает CPU limit, как советует krr):

```bash
helm upgrade shop charts/shop -n shop --reuse-values -f values/part3-krr.yaml --wait
```

![применено](screenshots/05_part3_applied.png)

Пока у всех подов QoS `Burstable` — исправлю в части 5.
