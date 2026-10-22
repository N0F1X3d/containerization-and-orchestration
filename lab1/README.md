# Лаба 1 — свой Docker

Работаю на маке, а namespaces, cgroups и seccomp есть только в ядре Linux. Поэтому всё гоняю в виртуалке через Lima (Ubuntu 26.04, ядро 7.0, cgroup v2). Папка с репой примонтирована в VM, так что код правлю в GoLand, а запускаю в VM.

## Часть 0 — сервис

Сервис на **Go**, без зависимостей: [`api/main.go`](api/main.go).

- `/health` — отдаёт `ok`
- `/eat?mb=N` — съедает N МБ и держит их (пишу в каждую страницу памяти, иначе ядро реально ничего не выделит)
- `/burn` — крутит бесконечный цикл, грузит одно ядро на 100%

Собираю на маке сразу под линукс, статический бинарник:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o api .
```

## Часть 1 — запускаю как обычный процесс

Просто `./api &` в VM. В браузере на маке открывается, Lima сама пробрасывает порт:

![браузер](screenshots/01-part1-browser.png)

На хосте это обычный процесс от моего юзера (uid 502):

![ps на хосте](screenshots/02-part1-ps.png)

Что у него сейчас (это точка отсчёта, дальше буду с ней сравнивать):

```
$ ls -l /proc/$P/ns
cgroup -> 'cgroup:[4026531835]'
ipc -> 'ipc:[4026531839]'
mnt -> 'mnt:[4026531832]'
net -> 'net:[4026531833]'
pid -> 'pid:[4026531836]'
time -> 'time:[4026531834]'
user -> 'user:[4026531837]'
uts -> 'uts:[4026531838]'

$ cat /proc/$P/cgroup
0::/user.slice/user-502.slice/session-6.scope

$ grep Cap /proc/$P/status
CapInh: 0000000000000000
CapPrm: 0000000000000000
CapEff: 0000000000000000
CapBnd: 000001ffffffffff
CapAmb: 0000000000000000
```

Что из этого видно:

- **namespaces** — числа это номера inode namespace'ов. Если у двух процессов номер одинаковый — они в одном namespace. Номера 40265318xx это начальные namespaces, которые ядро создаёт при загрузке, в них сидят все обычные процессы, включая systemd. То есть никакой изоляции.
- **cgroup** — процесс сидит в обычной сессии моего юзера, лимитов там нет. Если дёрнуть `/eat` на 10 ГБ, он будет жрать память всей VM, пока не придёт глобальный OOM killer, и убьёт он не обязательно `api`.
- **capabilities** — Eff и Prm нулевые, потому что процесс запущен от обычного юзера, а не от root. Bnd полный — это потолок, выше которого процесс никогда не получит прав. В контейнере процесс обычно root, поэтому там как раз важно урезать Bnd (это часть 4).

## Часть 2 — namespaces

### Сначала один namespace для разминки — UTS

`sudo unshare --uts bash`, внутри `hostname test`. Внутри имя поменялось, а во втором терминале осталось `lima-lab`. Из всех ссылок в `/proc/$$/ns` поменялась только `uts`:

![uts](screenshots/03_part1_uts_demo.png)

Hostname хранится внутри UTS namespace. `unshare --uts` сделал новый namespace с копией имени, я поменял имя в копии — хост этого не видит. Остальные ссылки остались хостовыми, потому что отделял только UTS. Отсюда главное: **namespaces независимы по типам**, каждый отделяется своим флагом, а контейнер — это когда отделили все сразу.

Без `sudo` не работает: для создания namespace нужна `CAP_SYS_ADMIN`.

### Ubuntu не даёт делать user namespace

![userns запрещены](screenshots/04_part2_userns_denied.png)

User namespace — единственный, который можно создать без root, и уже изнутри него делать остальные (так работает rootless Docker/Podman). Но тут:

- `unshare --user id` → `nobody`: маппинга uid нет, ядро не знает, кто я внутри, и показывает «неотображённый» uid 65534;
- `unshare --user --map-root-user id` → `Operation not permitted`. В `dmesg` видно, что это AppArmor: переводит процесс в профиль `unprivileged_userns` и запрещает `sys_admin`.

Зачем Ubuntu так делает: внутри user namespace обычный юзер получает `CAP_SYS_ADMIN` и может дёргать куски ядра, которые раньше были только для root (монтирование, сетевые штуки, nf_tables). Штатно на хост это не действует, но если в этом коде ядра баг — через него можно получить настоящий root. То есть закрывают поверхность атаки на ядро.

Для лабы выключил: `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`.

### Root внутри, а снаружи я обычный юзер

![root внутри = 502 снаружи](screenshots/05_part2_root_inside_502_outside.png)

- внутри `id` говорит `uid=0(root)`, а снаружи `ps` показывает тот же процесс с uid 502;
- `uid_map` = `0 502 1`: uid 0 внутри = uid 502 снаружи, длина диапазона 1 (отображён один uid). В rootless Docker там обычно `0 100000 65536`, тогда uid 1000 внутри = 101000 снаружи;
- `hostname evil` → «you must be root». Я root, но только в своём user namespace, а UTS namespace остался хостовым и принадлежит хостовому user namespace. Если добавить `--uts`, новый UTS будет принадлежать моему user namespace и hostname поменяется.

### Остальные namespaces по одному

Всё без sudo: `unshare -Ur` (это `--user --map-root-user`) + нужный флаг.

![namespaces по одному](screenshots/06_part2_namespaces-one-to-one.png)

| Namespace | Что делал | Что он прячет |
|---|---|---|
| user | `unshare -Ur bash` → `id`, `uid_map`, `ps` снаружи | uid/gid и capabilities. Внутри root, снаружи обычный юзер 502 |
| pid | `unshare -Ur --pid --fork` с `--mount-proc` и без | номера процессов. Внутри свой PID 1, чужих процессов не видно |
| mount | `mount -t tmpfs none /mnt` внутри, `findmnt /mnt` снаружи | таблицу монтирований. Что смонтировал внутри — снаружи не видно |
| net | `ip -br link`, `ip route`, `curl` внутри | весь сетевой стек: интерфейсы, маршруты, порты. Внутри только выключенный `lo` |
| uts | `hostname box` внутри, `hostname` снаружи | hostname |
| ipc | `ipcmk -Q` на хосте, `ipcs -q` внутри | очереди сообщений и общую память (SysV IPC). Хостовую очередь внутри не видно |

Что словил по ходу:

- **pid без `--mount-proc`**: `$$` = 1, а `ps` видит 137 процессов хоста. Потому что `ps` просто читает папку `/proc`, а `/proc` — это файловая система, привязанная к PID namespace того, кто её смонтировал. Старый `/proc` хостовый, поэтому нужен свежий — это и делает `--mount-proc`.
- **mount не прячет файлы**: mount namespace копирует только таблицу монтирований, а файлы те же самые, хостовые. Чтобы у процесса была своя ФС, нужен отдельный корень и `pivot_root`/`chroot` — это то, что даёт образ (часть 6).
- **net пустой**: чтобы дать сеть, нужна veth-пара (один конец внутри, другой на хосте), IP и маршрут, а на хосте `ip_forward` и NAT — у контейнера приватный адрес, интернет про него не знает, NAT подменяет его на адрес хоста.

### Всё сразу, `api` как PID 1

```bash
unshare -Ur --pid --fork --mount-proc --net --uts --ipc bash -c 'hostname box; ip link set lo up; exec ./api'
```

Захожу внутрь через `nsenter -t $P -U -p -m -n -u -i --preserve-credentials bash` (так же работает `docker exec`):

![изнутри](screenshots/07_part2_inside.png)

Снаружи тот же процесс:

![снаружи](screenshots/08_part2_outside.png)

`exec` нужен, чтобы bash заменил себя на `api` — тогда `api` становится PID 1, а не ребёнком bash. `ip link set lo up` — иначе внутри не работает даже localhost.

Итог:

- **изнутри** `api` — PID 1, root, hostname `box`, из сети только `lo`, других процессов не видно;
- **снаружи** это обычный процесс с большим PID и uid 502, и все его namespaces новые (номера 40265324xx вместо хостовых 40265318xx);
- на хосте `curl localhost:8080` ответил старый хостовый `api`. Два процесса слушают 8080 одновременно без конфликта, потому что таблица портов у каждого net namespace своя. До `api` в namespace с хоста вообще не достучаться — нет veth;
- у bash, в который зашёл через `nsenter`, PPID 0: его родитель (`nsenter`) живёт в хостовом PID namespace, изнутри его не видно. `nsenter`/`docker exec` не создают namespace'ы, а подселяют в существующие;
- root внутри — не настоящий root. Права на файлы ядро проверяет по реальному uid 502. В свою папку писать могу (она моя, внутри файл будет `root`, на хосте `dev`), а `/etc/shadow` прочитать нет — он хостового root, хоть он и виден (mount namespace — копия).

## Часть 3 — cgroups

Namespaces решают, что процесс **видит**, а cgroups — сколько он **может взять**. В cgroup v2 это просто папка в `/sys/fs/cgroup`: создал каталог, записал лимиты в файлы, закинул PID в `cgroup.procs` — и процесс со всеми детьми под лимитами. `resources.limits` в Kubernetes в итоге превращаются ровно в эти файлы.

Делал через `sudo`, потому что корень `/sys/fs/cgroup` принадлежит root:

```bash
CG=/sys/fs/cgroup/lab1; sudo mkdir -p $CG
echo 100M | sudo tee $CG/memory.max            # память — 100 МБ
echo 0 | sudo tee $CG/memory.swap.max          # без свопа, чтобы не «спасался» в него
echo "50000 100000" | sudo tee $CG/cpu.max     # 50 мс из каждых 100 мс = пол-ядра
echo 20 | sudo tee $CG/pids.max                # не больше 20 процессов/потоков
```

Запуск `api` внутри cgroup — шелл кладёт **себя** в `cgroup.procs` и через `exec` превращается в `api` (дети наследуют cgroup родителя):

```bash
bash -c "echo \$\$ | sudo tee $CG/cgroup.procs >/dev/null; exec ./api"
```

### Память → OOM

`/eat?mb=50` прошёл, на `/eat?mb=80` процесс убили — в сумме 130 МБ больше лимита 100 МБ:

![OOM](screenshots/09_part3_oom.png)

- в `memory.events`: `oom_kill 1` — ядро убило один процесс, `max 172` — столько раз упирались в лимит (до убийства ядро пыталось отжать память);
- в `dmesg`: `Memory cgroup out of memory: Killed process ... (api) anon-rss:101920kB` — убил именно **cgroup OOM**, а не глобальный, и как раз на ~100 МБ;
- остальная VM вообще ничего не заметила. Это то же самое, что `OOMKilled` в Kubernetes.

Зачем `memory.swap.max = 0`: со свопом ядро не убило бы процесс сразу, а начало бы выгружать его память на диск. Процесс жив, но тормозит в разы, и всё равно упадёт, когда кончится лимит свопа. Убить сразу честнее и предсказуемее, поэтому Kubernetes по умолчанию тоже работает без свопа.

### CPU → throttling

`/burn` хочет целое ядро, а лимит пол-ядра:

![throttling](screenshots/10_part3_throttling.png)

В `cpu.stat`: из 59 периодов по 100 мс процесс был придушен в 50 (`nr_throttled 50`), суммарно простоял на паузе ~2.4 с (`throttled_usec 2387168`). Процесс не убивают, его просто останавливают до следующего периода — поэтому при throttling сервис не падает, а начинает тупить (растёт latency).

`%CPU` в `ps` показал 13.5, а не 50. Это не баг: `ps` делит всё CPU-время процесса на **всё время его жизни**, а не показывает «прямо сейчас». `api` стартовал в 20:34:36, `/burn` пришёл в 20:34:50, смотрел я ещё через 5 с. Жил ~19 с, из них 14 с спал и 5 с жёг по пол-ядра ≈ 2.5 с CPU → 2.5 / 19 ≈ 13%. Для лимитов честнее смотреть `cpu.stat` (или `top`, который считает за последние секунды).

### Форк-бомба → pids.max

![pids.max](screenshots/11_part3_pids.png)

`pids.peak 20` — больше 20 процессов так и не набралось, `max 7019` — столько раз ядро отказало в `fork()`. VM при этом жива. Без лимита форк-бомба забила бы таблицу процессов всей машины.

Подвох: `pids.max` считает не процессы, а **задачи ядра**, то есть и потоки тоже. Go-рантайм сам держит несколько потоков, JVM — десятки (GC, JIT, пулы). Поэтому слишком маленький `pids.max` может уронить обычный сервис, который вообще ничего не форкает — он просто не сможет создать поток.

## Часть 4 — права

Namespaces прячут, cgroups ограничивают объём, а тут ограничиваем, **что процесс вообще может сделать**. Два отдельных механизма.

### Capabilities

Обычный root время менять может, а тот же root, у которого из bounding set выкинут `CAP_SYS_TIME`, — уже нет:

![capabilities](screenshots/12_part4_caps.png)

```bash
sudo capsh --drop=cap_sys_time -- -c 'date -s "$(date)"; grep CapBnd /proc/self/status'
```

`CapBnd` был `000001ffffffffff`, стал `000001fffdffffff` — пропал ровно один бит (25-й, это `CAP_SYS_TIME`). Ядро проверяет не «uid == 0», а конкретный бит, поэтому root без бита — не может. Docker делает так же, только оставляет контейнеру ~14 capabilities из 41 и выкидывает всё опасное (`SYS_ADMIN`, `SYS_TIME`, `SYS_MODULE`, ...).

### Seccomp

Seccomp — фильтр системных вызовов: процесс зовёт функцию ядра, а фильтр отвечает «нельзя» ещё до того, как ядро начнёт её выполнять. Навесил через `systemd-run` (systemd сам умеет ставить seccomp) запрет на `unshare`, `setns`, `mount` — ровно то, чем я строил «контейнер» в части 2:

```bash
sudo systemd-run --quiet --wait --pipe --collect --uid=dev \
  -p SystemCallFilter="~unshare setns mount" -p SystemCallErrorNumber=EPERM \
  bash -c 'grep Seccomp /proc/self/status; unshare -Ur true; mkdir -p /tmp/x && echo "mkdir работает"'
```

![seccomp](screenshots/13_part4_seccomp.png)

`Seccomp: 2` — включён режим фильтра. `unshare` получил `Operation not permitted`, а `mkdir`, который не запрещали, работает.

### Что закрывает каждый механизм

- **Capabilities** — проверяют, разрешено ли **привилегированное действие** (менять время, монтировать, грузить модули ядра). Режу именно **bounding set**: это потолок, его нельзя поднять обратно, даже запустив setuid-бинарник вроде `sudo`. Если убрать capability только из effective, она останется в permitted и процесс сможет включить её себе обратно.
- **Seccomp** — режет **сами системные вызовы**, ещё до их выполнения, причём любые, а не только привилегированные. Это важно, потому что дыры бывают и в коде вызовов, которым никакие права не нужны. Меньше доступных вызовов — меньше поверхность атаки на ядро.
- Зачем контейнеру запрещать `unshare`/`mount`, если он и так в своих namespaces: `unshare` (создать себе user namespace и получить в нём `CAP_SYS_ADMIN`) — классический первый шаг эксплойтов побега из контейнера (например CVE-2022-0185). Нет вызова — до уязвимого кода не добраться.

Нужны оба: capabilities не закроют непривилегированные вызовы, а seccomp не различает, *кто* зовёт — только *что*.

## Часть 5 — свой Docker (`mydocker.sh`) против `docker run`

Собрал всё из частей 2–4 в один скрипт: [`mydocker.sh`](mydocker.sh).

```bash
./mydocker.sh run box                 # запустить api в «контейнере»
./mydocker.sh exec box curl -s localhost:8080/health   # как docker exec (через nsenter)
```

Что он делает по порядку:

1. **cgroup** `/sys/fs/cgroup/mydocker-box` с лимитами памяти, CPU и pids (часть 3), при выходе удаляет её;
2. шелл кладёт себя в cgroup и делает `exec unshare --user --map-root-user --pid --fork --mount-proc --net --uts --ipc` — свои namespaces (часть 2);
3. внутри: hostname, поднять `lo`;
4. последним шагом `setpriv --bounding-set=-all --inh-caps=-all --no-new-privs --seccomp-filter seccomp.bpf ./api` — пустые capabilities, запрет повышать права и seccomp (часть 4).

Seccomp навешивается **последним**, после `unshare` и `mount` — иначе профиль запретил бы мой же `unshare`. Поэтому не через `systemd-run`, как в части 4, а через `setpriv`. `setpriv` понимает только скомпилированный BPF, а профиль у меня в формате Docker — [`seccomp.json`](seccomp.json) (запрещает `unshare`, `setns`, `mount`, `ptrace`, загрузку модулей, `bpf`, `keyctl` и т. п.). Чтобы один и тот же профиль работал и в скрипте, и в `docker run`, написал маленький конвертер [`seccompgen`](seccompgen/main.go): JSON → BPF.

![mydocker.sh](screenshots/14_part5_mydocker.png)

Внутри `api` — PID 1 и `/health` отвечает. Снаружи: uid 502, `CapBnd` из нулей, `NoNewPrivs: 1`, `Seccomp: 2`.

Тот же бинарник, те же лимиты и тот же seccomp-профиль через Docker:

```bash
docker run -d --name api-docker -v $PWD/api:/app:ro --memory 100m --cpus 0.5 --pids-limit 50 \
  --security-opt seccomp=$PWD/seccomp.json -p 8081:8080 busybox /app/api
```

![docker run](screenshots/15_part5_docker.png)

### Сравнение

| | `mydocker.sh` | `docker run` |
|---|---|---|
| pid, mount, net, uts, ipc namespaces | ✅ | ✅ |
| user namespace | ✅ root внутри = uid 502 снаружи | ❌ по умолчанию нет — **снаружи настоящий root (uid 0)** |
| cgroup namespace | ❌ процесс видит путь своей cgroup на хосте | ✅ внутри `/proc/1/cgroup` = `0::/` |
| лимиты cgroup | ✅ сам пишу в файлы | ✅ те же файлы, только в `system.slice/docker-<id>.scope` |
| capabilities | 0 из 41 | 14 из 41 (`chown`, `setuid`, `net_bind_service`, `kill`, ...) |
| no_new_privs | ✅ | ❌ (включается `--security-opt no-new-privileges`) |
| seccomp | ✅ мой профиль | ✅ мой профиль (без `--security-opt` — встроенный дефолтный) |
| своя файловая система | ❌ виден весь хост | ✅ корень из образа (`ls /` — это busybox) через overlayfs + `pivot_root` |
| сеть | ❌ только `lo`, снаружи не достучаться | ✅ veth `eth0` → мост `docker0`, IP 172.17.0.2, NAT наружу и `-p 8081:8080` внутрь |
| образы, логи, рестарты, `docker ps` | ❌ | ✅ |

### Выводы

- **Главное отличие — uid снаружи.** У Docker `api` на хосте — настоящий root. Если через баг ядра процесс сбежит из контейнера, он окажется root на хосте. Мой скрипт за счёт user namespace сбежит максимум как обычный юзер 502.
- **Почему Docker не включает user namespace по умолчанию:** ломаются тома. С маппингом `0 100000 65536` файл, созданный root в контейнере, на хосте принадлежит uid 100000, а хостовый файл юзера 1000 (которого нет в маппинге) внутри выглядит как `nobody` и не пишется. Поэтому user namespace включают явно — `userns-remap` или rootless Docker.
- **Почему у Docker 14 capabilities, а не 0:** обычным образам они нужны. Например, nginx/postgres стартуют от root и сами переключаются на непривилегированного юзера (`CAP_SETUID`/`CAP_SETGID`), меняют владельца своих файлов (`CAP_CHOWN`, `CAP_FOWNER`), nginx слушает порт 80 (`CAP_NET_BIND_SERVICE`). Опасное (`SYS_ADMIN`, `SYS_MODULE`, `SYS_TIME`) Docker выкидывает. Мне для `api` не нужно ничего — поэтому 0.
- **Сеть у Docker:** `eth0` в контейнере — конец veth-пары (`eth0@if6` = «моя пара — интерфейс №6 на хосте»), второй конец воткнут в мост `docker0` (172.17.0.1 — шлюз контейнера). Наружу — NAT (masquerade) подменяет 172.17.0.2 на адрес хоста. Внутрь — `-p 8081:8080` это DNAT-правило (плюс `docker-proxy` для localhost), которое превращает `localhost:8081` в `172.17.0.2:8080`. У моего скрипта этого нет — только `lo`.
- **Файловая система:** Docker даёт контейнеру корень из образа (overlayfs + `pivot_root`), а мой скрипт оставляет хостовую ФС — это и есть то, чего не хватает (часть 6).

## Часть 6 — образы

## Часть 7 — gVisor

## Часть 8 — мониторинг
