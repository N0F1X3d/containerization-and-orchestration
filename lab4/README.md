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
