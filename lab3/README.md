# Лаба 3 — платформа для shop

Тот же kind-кластер в Lima VM, что и в лабе 2. Стек мониторинга из лабы 2 (Prometheus, Grafana, Loki, Alertmanager) продолжает работать — в части 5 подключаюсь к нему, новый не поднимаю.

Что где лежит:

- [`services/`](services) — `api` и `worker` на Go + один [`Dockerfile`](services/Dockerfile) на оба;
- [`policies/`](policies) — 5 политик Kyverno, в [`policies/bad/`](policies/bad) — 5 манифестов-нарушителей;
- [`charts/shop/`](charts/shop) — Helm-чарт `shop`: api, worker, postgres (CloudNativePG), мониторинг;
- [`helm/kyverno.yaml`](helm/kyverno.yaml) — values для Kyverno.

## Часть 0 — сервисы

Сгенерированы, на оценку не влияют:

- **api**: `GET /health` (`ok`, а при `HEALTH_FAIL=true` — 503), `POST /order` (пишет заказ в postgres), `GET /orders` (последние 50), `GET /version`, `/metrics`;
- **worker**: раз в 2 секунды берёт необработанные заказы и помечает обработанными. `FOR UPDATE SKIP LOCKED` — чтобы две реплики worker не схватили один и тот же заказ.

Оба сервиса стартуют **без базы**: HTTP и `/health` поднимаются сразу, а к postgres подключаются в фоне, когда он появится (база заводится только в части 3). Адрес базы читается из файла — Secret `shop-db-app`, который создаёт оператор, смонтирован в под томом с `optional: true`. Пока Secret нет — файла нет, под всё равно стартует; появится — kubelet сам подложит файл, перезапускать поды не нужно.

Образы с префиксом доверенного реестра (`registry.lab.local/...`, см. политику ниже), загружаю прямо в ноду kind — реестра с таким именем на самом деле нет:

```bash
for v in 1.0 1.1; do docker build -t registry.lab.local/shop/api:$v --build-arg APP=api --build-arg VERSION=$v services; done
docker build -t registry.lab.local/shop/worker:1.0 --build-arg APP=worker --build-arg VERSION=1.0 services
for i in api:1.0 api:1.1 worker:1.0; do kind load docker-image registry.lab.local/shop/$i --name lab; done
```

## Часть 1 — ограждения на кластер

### Почему Kyverno

| | Kyverno | OPA / Gatekeeper |
|---|---|---|
| язык правил | обычный YAML, как любой манифест Kubernetes | Rego — отдельный язык, надо учить |
| порог входа | низкий: паттерн «как должен выглядеть объект» | высокий |
| гибкость | хватает для типовых правил, сложное — через CEL/JMESPath | максимальная |
| сверх валидации | умеет ещё mutate/generate | в основном валидация |

Взял **Kyverno**: правила читаются как манифесты, для локального кластера и пяти типовых правил Rego — избыточен.

Kyverno — это **admission webhook**: apiserver перед сохранением объекта отправляет его в Kyverno, и если правило нарушено — отказ, до etcd объект не доходит.

```bash
helm install kyverno kyverno/kyverno --version 3.9.2 -n kyverno --create-namespace -f helm/kyverno.yaml --wait
kubectl apply -f policies/
```

### Пять правил

| Политика | Что требует | Зачем |
|---|---|---|
| [`require-resources`](policies/01-require-resources.yaml) | у каждого контейнера `requests.cpu/memory` и `limits.memory` | без requests планировщик не знает, сколько места нужно поду; без limits один под может съесть всю ноду (часть 3 лабы 1 — cgroup) |
| [`trusted-registry`](policies/02-trusted-registry.yaml) | образы только из `registry.lab.local/` и `ghcr.io/cloudnative-pg/` | случайный образ из интернета может содержать что угодно; так в кластер попадает только проверенное |
| [`require-labels`](policies/03-require-labels.yaml) | метки `app.kubernetes.io/name` и `app.kubernetes.io/part-of` | без меток не найти владельца и не выбрать поды в Service/ServiceMonitor/NetworkPolicy |
| [`disallow-privileged`](policies/04-disallow-privileged.yaml) | никаких `privileged: true` | privileged-контейнер получает все capabilities и доступ к устройствам ноды — по сути root на ноде (лаба 1, часть 4) |
| [`disallow-latest-tag`](policies/05-disallow-latest-tag.yaml) (свой) | явный тег образа, не `:latest` | `:latest` меняется без предупреждения — непонятно, что реально запущено, и нельзя откатиться на «предыдущую версию» |

Правила написаны для `Pod`, но Kyverno сам генерирует такие же для Deployment/StatefulSet/Job (autogen) — поэтому неправильный Deployment отклоняется сразу при `apply`, а не «тихо» на этапе создания подов.

Системные namespace и уже работающая инфраструктура (`kube-system`, `monitoring` со стеком из лабы 2, сам `kyverno`, оператор БД `cnpg-system`) вынесены в `exclude` — их образы не из моего реестра, и без исключения политика не дала бы им пересоздать поды.

Kyverno 1.19 предупреждает, что `ClusterPolicy` устарел и будет заменён CEL-политиками (`ValidatingPolicy`). Пока работает, но в проде я бы сразу писал на новом API.

![политики](screenshots/01_part1_policies.png)

### Пять нарушителей

В [`policies/bad/`](policies/bad) — пять подов, каждый нарушает **ровно одно** правило (остальное в них по правилам):

```bash
kubectl create ns shop && for f in policies/bad/*.yaml; do kubectl apply -f $f; done
```

![отказы](screenshots/02_part1_rejected.png)

Все пять отклонены, у каждого — своя политика и путь к полю, на котором сработало правило (`/spec/containers/0/image/`, `/metadata/labels/`, ...).

### Что понял

- **Когда проверяет Kyverno.** До etcd: admission стоит между аутентификацией и сохранением объекта. Не прошёл проверку — объекта в кластере просто нет. Уже работающие поды новая политика не трогает (не убивает) — они только попадают в фоновый отчёт (PolicyReport).
- **Зачем `exclude` для инфраструктуры.** Без него упавший под Grafana (образ не из `registry.lab.local`) не смог бы пересоздаться — Deployment есть, подов 0, мониторинг лежит. Поэтому системные namespace исключают или явно добавляют их реестры в доверенные.
- **Почему важен autogen (правила и для Deployment).** Если бы проверялись только поды: Deployment принят → контроллер ReplicaSet пытается создать поды → вебхук отказывает *контроллеру*, а не мне. `helm install --wait` просто висит до таймаута, подов 0, а причина видна только в events ReplicaSet (`FailedCreate ... denied the request`). С autogen отказ приходит сразу на `apply`/`helm install`.
