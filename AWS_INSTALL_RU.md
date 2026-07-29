# Чистая установка актуального стабильного CLIProxyAPI + Bravo на AWS

Эта инструкция создаёт новый независимый production: без переноса конфигурации,
OAuth-файлов, проектов, ключей и аналитики с MacMini.

После установки используйте отдельный операторский гайд
[`BRAVO_MODELS_AND_KEYS_RU.md`](BRAVO_MODELS_AND_KEYS_RU.md): там простым
языком описаны проектные ключи, allowlist моделей, маршруты и сценарий без
OpenAI/Codex-подписок.

## Короткий ответ: что именно устанавливать

Не нужно сначала ставить обычный CLIProxyAPI, а затем вручную копировать в него
Bravo.

На AWS собирается **один согласованный Docker image** из двух форков. Версии не
зашиты в эту инструкцию: стабильная backend-ветка содержит
`deploy/aws/release.env`, который фиксирует совместимый frontend commit, номер
Bravo, имя image, платформу и builder image.

1. [`nikitau-svg/CLIProxyAPI`](https://github.com/nikitau-svg/CLIProxyAPI),
   ветка `bravo/stable` — проверенный patched host, Bravo, healthcheck и
   AWS-installer.
2. [`nikitau-svg/Cli-Proxy-API-Management-Center`](https://github.com/nikitau-svg/Cli-Proxy-API-Management-Center),
   точный commit из `deploy/aws/release.env` — подходящая версия админки.

Результат сборки:

```text
CLIProxyAPI host + bravo.so + management.html
                       ↓
CLIPROXYAPI_IMAGE из deploy/aws/release.env
```

Bravo использует новые host callbacks и загружается как нативный Linux-плагин
внутрь процесса. Поэтому `upstream latest + отдельно bravo.so` — неподдерживаемая
комбинация: версии host ABI, plugin и UI могут разойтись.

Не используйте корневые `Dockerfile` и `docker-compose.yml` этого форка для
Bravo deployment: они сохраняются для upstream-совместимого режима. В этой
инструкции намеренно используются только `Dockerfile.canary` и
`deploy/aws/docker-compose.yml`. Файл `BRAVO_PRODUCTION_RUNBOOK_RU.md` —
исторический runbook миграции 0.5, а не инструкция актуальной чистой установки.



## 1. Создать EC2

Проверенный release target сейчас только `linux/arm64`, поэтому для первой
установки берите Graviton:

- AMI: Ubuntu Server 24.04 LTS, `64-bit (Arm)`;
- instance: `t4g.large` на время сборки; после сборки можно остановить instance
  и попробовать `t4g.medium`;
- диск: 30–40 GiB encrypted `gp3`;
- включить termination protection;
- создать EC2 key pair и сохранить скачанный `.pem` на Mac;
- желательно назначить Elastic IP, если позже будет публичный домен.

Размер instance — практическая стартовая рекомендация, а не требование AWS.
AWS публикует ARM64 AMI для Graviton, а Docker официально поддерживает Ubuntu
24.04 и arm64:

- [AWS Graviton resources](https://aws.amazon.com/ec2/graviton/resources/)
- [Docker Engine on Ubuntu](https://docs.docker.com/engine/install/ubuntu/)

### Security Group для первого запуска

Самый простой вариант:

- TCP `22` — только с вашего текущего public IP;
- не добавлять inbound rules для `8317`, `1455`, `54545` и `51121`;
- outbound оставить разрешённым, чтобы работали GitHub, OAuth и провайдеры.

AWS отдельно рекомендует не открывать SSH на `0.0.0.0/0`:
[EC2 security group rules](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/changing-security-group.html).

Более строгая альтернатива — AWS Systems Manager Session Manager: тогда inbound
порт `22` тоже не нужен. Сначала удобнее выполнить инструкцию через SSH, а SSM
подключить после первого успешного запуска.

## 2. Подключиться и установить Docker

```bash
chmod 400 /path/to/bravo-aws.pem
ssh -i /path/to/bravo-aws.pem ubuntu@<EC2_PUBLIC_IP>
```

На EC2:

```bash
set -euo pipefail

sudo apt update
sudo apt install -y ca-certificates curl git jq openssl

sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
  -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc

sudo tee /etc/apt/sources.list.d/docker.sources >/dev/null <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: $(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.asc
EOF

sudo apt update
sudo apt install -y \
  docker-ce \
  docker-ce-cli \
  containerd.io \
  docker-buildx-plugin \
  docker-compose-plugin

sudo systemctl enable --now docker
sudo docker version
sudo docker compose version
```

Это официальный apt-способ Docker. Convenience script `get.docker.com` для
production не используем.

## 3. Скачать текущий стабильный комплект

```bash
set -euo pipefail

sudo install -d -o ubuntu -g ubuntu /srv/bravo-build
cd /srv/bravo-build

git clone \
  --branch bravo/stable \
  --single-branch \
  https://github.com/nikitau-svg/CLIProxyAPI.git \
  CLIProxyAPI

SOURCE_SHA="$(git -C CLIProxyAPI rev-parse HEAD)"
. ./CLIProxyAPI/deploy/aws/release.env

./CLIProxyAPI/deploy/aws/fetch-management-ui.sh \
  /srv/bravo-build/Cli-Proxy-API-Management-Center
```

Переменная `SOURCE_SHA` действует только в текущем shell. Не выполняйте
`git pull` и не переключайте ветку между её записью и окончанием сборки.

Скрипт читает frontend repository и точный commit из release manifest. Он не
берёт независимо обновившуюся `main`/`latest`, поэтому host, plugin и UI
остаются одной проверенной сборкой.

Проверьте выбранный комплект:

```bash
set -euo pipefail

cd /srv/bravo-build/CLIProxyAPI
. ./deploy/aws/release.env

SOURCE_SHA="$(git rev-parse HEAD)"
printf 'Source %s, Bravo %s, image %s\n' \
  "$SOURCE_SHA" "$BRAVO_VERSION" "$CLIPROXYAPI_IMAGE"
test "$(git -C ../Cli-Proxy-API-Management-Center rev-parse HEAD)" = \
  "$WEBUI_COMMIT"
```

Обычная новая установка всегда начинает со свежего проверенного состояния
`bravo/stable` и **не требует release tag**. Опциональные immutable-теги вида
`bravo-vX.Y.Z` — только удобные точки воспроизведения и отката уже
опубликованных версий.

## 4. Собрать и проверить WebUI

Ставить Bun на EC2 не обязательно. Проект закреплён на Bun 1.3.14, поэтому
используем одноразовый официальный контейнер:

```bash
set -euo pipefail

cd /srv/bravo-build/Cli-Proxy-API-Management-Center

. ../CLIProxyAPI/deploy/aws/release.env

sudo docker run --rm \
  --user "$(id -u):$(id -g)" \
  --env HOME=/tmp \
  --volume "$PWD:/workspace" \
  --workdir /workspace \
  "$BUN_IMAGE" \
  bun install --frozen-lockfile

sudo docker run --rm \
  --user "$(id -u):$(id -g)" \
  --env HOME=/tmp \
  --volume "$PWD:/workspace" \
  --workdir /workspace \
  "$BUN_IMAGE" \
  bun run verify

install -d ../CLIProxyAPI/.canary-dist
install -m 0644 dist/index.html \
  ../CLIProxyAPI/.canary-dist/management.html

WEBUI_SHA256="$(
  sha256sum ../CLIProxyAPI/.canary-dist/management.html | awk '{print $1}'
)"
printf 'Management UI %s\n' "$WEBUI_SHA256"
```

`bun run verify` должен завершить tests, lint, typecheck и production build без
ошибок. Сохраните показанный SHA-256 вместе с release-записью: он позволяет
после запуска проверить, что контейнер обслуживает именно собранный UI.
Официальный Bun image поддерживает Linux arm64:
[Bun installation](https://bun.com/docs/installation).

## 5. Собрать единый production image

```bash
set -euo pipefail

cd /srv/bravo-build/CLIProxyAPI

. ./deploy/aws/release.env
SOURCE_SHA="$(git rev-parse HEAD)"

sudo docker build \
  --platform "$RELEASE_PLATFORM" \
  --build-arg VERSION="$CLIPROXYAPI_VERSION" \
  --build-arg COMMIT="$SOURCE_SHA" \
  --build-arg BUILD_DATE="$(date -u +%F)" \
  --file Dockerfile.canary \
  --tag "$CLIPROXYAPI_IMAGE" \
  .
```

Проверьте image:

```bash
sudo docker image inspect \
  --format '{{.Id}} {{.Os}}/{{.Architecture}}' \
  "$CLIPROXYAPI_IMAGE"
```

Ожидается платформа из `RELEASE_PLATFORM`. Image ID разных сборок может
отличаться из-за `BUILD_DATE` — это нормально. Frontend commit, Go/Bun builder
images и runtime base закреплены commit SHA и Docker digest. Release tag для
новой установки не нужен.

Запишите для отката как минимум `SOURCE_SHA`, `WEBUI_COMMIT`,
`WEBUI_SHA256`, `CLIPROXYAPI_IMAGE` и полученный image ID. Само имя image из
manifest не является криптографическим доказательством его содержимого.

## 6. Создать новый runtime и секреты

Deployment хранится отдельно от Git checkout:

```text
/srv/cliproxyapi-prod/
├── .env
├── docker-compose.yml
├── config.yaml
├── secrets.env
├── auths/
├── bravo-data/
└── logs/
```

Создайте его включённым скриптом:

```bash
set -euo pipefail

cd /srv/bravo-build/CLIProxyAPI
sudo install -d -o ubuntu -g ubuntu /srv/cliproxyapi-prod
./deploy/aws/prepare-runtime.sh /srv/cliproxyapi-prod
```

Скрипт:

- создаёт полностью новый пустой runtime;
- генерирует случайный management key;
- генерирует отдельный ordinary break-glass API key;
- включает Bravo и правильный `plugin-dist`;
- запрещает автообновлению затереть нашу админку;
- закрепляет выбранные release/image в runtime `.env`;
- создаёт `auths`, `bravo-data` и `logs`;
- отказывается инициализировать непустой runtime.

Операторская копия обоих сгенерированных ключей лежит в:

```text
/srv/cliproxyapi-prod/secrets.env
```

`secrets.env` и `config.yaml` имеют режим `0600`; оба файла секретные и не
должны попадать в Git, user-data, скриншоты или логи. `config.yaml` постоянно
содержит ordinary break-glass API key. На первом старте CLIProxyAPI заменит
plaintext `remote-management.secret-key` в `config.yaml` его bcrypt-хешем.
`secrets.env` не подключается к контейнеру и сохраняет management key для
оператора.

Посмотреть management key:

```bash
grep '^MANAGEMENT_KEY=' /srv/cliproxyapi-prod/secrets.env
```

## 7. Запустить

```bash
set -euo pipefail

cd /srv/cliproxyapi-prod
sudo docker compose config --quiet
sudo docker compose up -d --wait --wait-timeout 120
sudo docker compose ps
```

В `docker compose ps` контейнер `CLIProxyAPI-Prod` должен стать `healthy`.
Флаги `--wait` и `--wait-timeout` входят в актуальный
[`docker compose up`](https://docs.docker.com/reference/cli/docker/compose/up/)
и дают ограниченное ожидание healthcheck вместо произвольного `sleep`.
До подключения аккаунтов можно посмотреть стартовый хвост логов:

```bash
sudo docker compose logs --tail 100
```

Проверьте, что запущен именно собранный image и контейнер не перезапускался:

```bash
set -euo pipefail

cd /srv/cliproxyapi-prod
. ./.env

EXPECTED_IMAGE_ID="$(
  sudo docker image inspect --format '{{.Id}}' "$CLIPROXYAPI_IMAGE"
)"

test "$(
  sudo docker inspect --format '{{.Image}}' CLIProxyAPI-Prod
)" = "$EXPECTED_IMAGE_ID"
test "$(
  sudo docker inspect --format '{{.State.Health.Status}}' CLIProxyAPI-Prod
)" = healthy
test "$(
  sudo docker inspect --format '{{.RestartCount}}' CLIProxyAPI-Prod
)" = 0
test "$(
  sudo docker inspect --format '{{.State.OOMKilled}}' CLIProxyAPI-Prod
)" = false

curl -fsS http://127.0.0.1:8317/healthz | jq -e '.status == "ok"'
curl -fsS http://127.0.0.1:8317/management.html >/dev/null
```

Если вы сохранили SHA-256 из шага 4, проверьте и обслуживаемый UI:

```bash
set -euo pipefail

EXPECTED_WEBUI_SHA256='<SHA256_FROM_STEP_4>'
test "$(
  curl -fsS http://127.0.0.1:8317/management.html | sha256sum | awk '{print $1}'
)" = "$EXPECTED_WEBUI_SHA256"
```

Проверить загрузку Bravo:

```bash
set -euo pipefail

cd /srv/cliproxyapi-prod
. ./.env
. ./secrets.env

BRAVO_STATUS_JSON="$(
  curl -fsS \
    --config <(
      printf 'header = "X-Management-Key: %s"\n' "$MANAGEMENT_KEY"
    ) \
    http://127.0.0.1:8317/v0/management/bravo/status
)"

jq -e --arg version "$BRAVO_VERSION" \
  '.version == $version and .enabled == true and .degraded == false' \
  <<<"$BRAVO_STATUS_JSON"

unset BRAVO_STATUS_JSON MANAGEMENT_KEY ORDINARY_API_KEY
```

Команда должна вернуть `true`. Если healthcheck зелёный, но версия не
совпадает, Bravo выключен или `degraded == true`, deployment ещё не готов.
Критические блоки намеренно включают `set -euo pipefail`: любая невыполненная
проверка останавливает текущий shell до следующего шага.

## 8. Открыть админку с Mac и заново подключить аккаунты

На MacBook откройте отдельное окно Terminal и держите команду запущенной:

```bash
ssh -N \
  -i /path/to/bravo-aws.pem \
  -L 8317:127.0.0.1:8317 \
  -L 1455:127.0.0.1:1455 \
  -L 54545:127.0.0.1:54545 \
  -L 51121:127.0.0.1:51121 \
  ubuntu@<EC2_PUBLIC_IP>
```

Теперь на Mac откройте:

```text
http://127.0.0.1:8317/management.html
```

Введите `MANAGEMENT_KEY` из `/srv/cliproxyapi-prod/secrets.env`.

Дальше:

1. В обычном разделе аутентификации CLIProxyAPI заново подключите все Claude и
   OpenAI/Codex подписки.
2. Дождитесь, пока каждая подписка появится отдельной строкой. Для Claude
   personal/team с одинаковой почтой эта ветка учитывает workspace.
3. Откройте `Bravo`.
4. Обновите квоты и проверьте тарифы `x1`, `x5`, `x20`.
5. Создайте новый проект, выберите разрешённый пул и primary-подписку.
6. Сохраните показанный `brv_...` ключ: plaintext показывается один раз.

Пошаговое объяснение выбора моделей и сценарий без OpenAI/Codex находятся в
[`BRAVO_MODELS_AND_KEYS_RU.md`](BRAVO_MODELS_AND_KEYS_RU.md).

Порты `1455`, `54545` и `51121` используются только localhost OAuth callback и
не открыты Security Group. Если конкретный OAuth flow предлагает ручную вставку
полного callback URL, можно использовать её вместо дополнительного tunnel.

### Что будет с маршрутами на чистой установке

Пустой runtime автоматически получает встроенные маршруты Bravo: `frontier`,
`deep`, `balanced`, `fast`, `auto`, семейства `opus`, `sonnet`, `haiku`,
`fable`/`fabulus`, `sol`, `terra`, `luna`, точные aliases физических моделей и
image-маршруты. Поэтому перед первым запросом вручную заполнять YAML не нужно.

В `management.html` откройте **Bravo → Маршруты логических моделей**. Там можно
на горячую полностью изменить цепочку кандидатов существующего маршрута:
provider, физическую модель, effort и порядок; приоритет формируется из этого
порядка. Перед сохранением маршрут проверяется; кнопка reset возвращает
встроенный default. Изменения сохраняются в `config.yaml`.

Текущий редактор не создаёт совершенно новый logical ID вроде
`bravo/my-own-route`: он безопасно переопределяет уже зарегистрированные
маршруты и разрешает только проверенные provider/model/capability сочетания.
Добавление произвольных logical IDs остаётся отдельным будущим улучшением.

## 9. Проверить новый project key

Через пока ещё открытый SSH tunnel:

```bash
export ANTHROPIC_BASE_URL='http://127.0.0.1:8317'
export ANTHROPIC_AUTH_TOKEN='<NEW_BRAVO_PROJECT_KEY>'
export ANTHROPIC_DEFAULT_OPUS_MODEL='bravo/opus'
export ANTHROPIC_DEFAULT_SONNET_MODEL='bravo/sonnet'
export ANTHROPIC_DEFAULT_HAIKU_MODEL='bravo/haiku'

claude --model opus --effort xhigh
```

Для OpenAI-совместимого клиента:

```text
Base URL: http://127.0.0.1:8317/v1
API key:  тот же новый brv_... key
Model:    bravo/opus
```

После теста проверьте в Bravo UI:

- запрос появился в аналитике проекта;
- видна фактически использованная подписка;
- токены записались;
- выбранная подписка находится внутри разрешённого пула.

## 10. Как дать доступ не только через SSH tunnel

Не открывайте `8317` напрямую в Internet: management key и project keys
передаются в HTTP headers.

Для одного EC2 самый простой production-вариант:

1. назначить домен на Elastic IP;
2. открыть Security Group TCP `80` и `443`;
3. поставить Caddy на host;
4. оставить Docker на `127.0.0.1:8317`;
5. публично проксировать API, но блокировать весь интерфейс и служебные
   маршруты управления.

Пример `/etc/caddy/Caddyfile`:

```caddyfile
bravo.example.com {
	@private {
		path /management.html /v0 /v0/* /anthropic/callback /codex/callback /antigravity/callback
	}

	respond @private 404
	reverse_proxy 127.0.0.1:8317
}
```

Caddy автоматически получает и продлевает TLS-сертификат, когда DNS уже
указывает на host. Официальные инструкции:

- [Install Caddy on Ubuntu](https://caddyserver.com/docs/install)
- [Caddy reverse proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)

После этого:

```text
Anthropic base URL: https://bravo.example.com
OpenAI base URL:    https://bravo.example.com/v1
Админка:            только через SSH tunnel на http://127.0.0.1:8317
```

Для более строгой AWS-схемы вместо Caddy можно использовать ALB + ACM, но
текущий Compose намеренно слушает `8317` только на loopback, поэтому ALB до
него не достучится. Для ALB нужен отдельный проверенный compose override,
который публикует `8317` на private interface EC2; Security Group instance
должна разрешать этот порт только от Security Group балансировщика, а
management paths должны быть закрыты правилами ALB/WAF. Не меняйте loopback
binding только ради быстрого теста.

## 11. Backup после настройки

Хотя установка чистая, после OAuth и создания проектов уже появляются важные
данные:

- `.env` — закреплённые версии и локальный image tag;
- `docker-compose.yml` — runtime topology и mounts;
- `config.yaml` — bcrypt management secret, project key digests, routes, pools;
- `secrets.env` — operator copy management и ordinary break-glass keys;
- `auths/` — OAuth access/refresh tokens;
- `bravo-data/bravo-state.json` — analytics, quotas, allocator ledger и
  санитизированные активные model-scoped cooldowns.

Включите encrypted EBS snapshots или AWS Backup. EBS snapshots являются
point-in-time backup и после первого снимка сохраняют изменённые блоки
инкрементально:
[How EBS snapshots work](https://docs.aws.amazon.com/ebs/latest/userguide/how_snapshots_work.html).

Перед ручным консистентным snapshot:

```bash
cd /srv/cliproxyapi-prod
sudo docker compose stop
```

После завершения snapshot:

```bash
sudo docker compose up -d --wait --wait-timeout 120
```

Не используйте `docker kill`: при штатной остановке Bravo успевает сбросить
ledger на диск.

## 12. Обновление через стабильный канал

Сейчас Bravo image не опубликован в GHCR/Docker Hub, поэтому на AWS он
собирается из согласованных исходников и используется с `pull_policy: never`.
`bravo/stable` — движущийся проверенный канал, а не immutable tag. Поэтому для
каждой сборки обязательно записывайте backend SHA и локальный image ID.
Наличие отдельного release tag не предполагается.

Не обновляйте код внутри старого `/srv/bravo-build`: скрипт загрузки UI
специально отказывается перезаписывать существующую директорию. Каждое
обновление собирайте в новой versioned-директории:

```bash
set -euo pipefail

RELEASE_ID="$(date -u +%Y%m%dT%H%M%SZ)"
BUILD_ROOT="/srv/bravo-build-${RELEASE_ID}"

sudo install -d -o ubuntu -g ubuntu "$BUILD_ROOT"
git clone \
  --branch bravo/stable \
  --single-branch \
  https://github.com/nikitau-svg/CLIProxyAPI.git \
  "${BUILD_ROOT}/CLIProxyAPI"

SOURCE_SHA="$(git -C "${BUILD_ROOT}/CLIProxyAPI" rev-parse HEAD)"
. "${BUILD_ROOT}/CLIProxyAPI/deploy/aws/release.env"

"${BUILD_ROOT}/CLIProxyAPI/deploy/aws/fetch-management-ui.sh" \
  "${BUILD_ROOT}/Cli-Proxy-API-Management-Center"
```

До появления `/srv/bravo-candidate.env` эти блоки предполагают один SSH shell.
Если соединение оборвалось раньше, начните сборку заново с новым `RELEASE_ID`;
production на этой стадии ещё не затронут.

Соберите UI именно из нового checkout:

```bash
set -euo pipefail
: "${BUILD_ROOT:?run the update clone step first}"

. "${BUILD_ROOT}/CLIProxyAPI/deploy/aws/release.env"
cd "${BUILD_ROOT}/Cli-Proxy-API-Management-Center"

sudo docker run --rm \
  --user "$(id -u):$(id -g)" \
  --env HOME=/tmp \
  --volume "$PWD:/workspace" \
  --workdir /workspace \
  "$BUN_IMAGE" \
  bun install --frozen-lockfile

sudo docker run --rm \
  --user "$(id -u):$(id -g)" \
  --env HOME=/tmp \
  --volume "$PWD:/workspace" \
  --workdir /workspace \
  "$BUN_IMAGE" \
  bun run verify

install -d "${BUILD_ROOT}/CLIProxyAPI/.canary-dist"
install -m 0644 \
  "${BUILD_ROOT}/Cli-Proxy-API-Management-Center/dist/index.html" \
  "${BUILD_ROOT}/CLIProxyAPI/.canary-dist/management.html"

WEBUI_SHA256="$(
  sha256sum "${BUILD_ROOT}/CLIProxyAPI/.canary-dist/management.html" |
    awk '{print $1}'
)"
```

Соберите candidate с уникальным tag, чтобы не затереть rollback image:

```bash
set -euo pipefail
: "${RELEASE_ID:?run the update clone step first}"
: "${BUILD_ROOT:?run the update clone step first}"

cd "${BUILD_ROOT}/CLIProxyAPI"

. ./deploy/aws/release.env
SOURCE_SHA="$(git rev-parse HEAD)"
CANDIDATE_IMAGE="${CLIPROXYAPI_IMAGE}-${SOURCE_SHA:0:12}"
WEBUI_SHA256="$(
  sha256sum .canary-dist/management.html | awk '{print $1}'
)"

sudo docker build \
  --platform "$RELEASE_PLATFORM" \
  --build-arg VERSION="$CLIPROXYAPI_VERSION" \
  --build-arg COMMIT="$SOURCE_SHA" \
  --build-arg BUILD_DATE="$(date -u +%F)" \
  --file Dockerfile.canary \
  --tag "$CANDIDATE_IMAGE" \
  .

CANDIDATE_IMAGE_ID="$(
  sudo docker image inspect --format '{{.Id}}' "$CANDIDATE_IMAGE"
)"
CANDIDATE_BRAVO_VERSION="$BRAVO_VERSION"
CANDIDATE_HOST_VERSION="$CLIPROXYAPI_VERSION"

sudo tee /srv/bravo-candidate.env >/dev/null <<EOF
RELEASE_ID=${RELEASE_ID}
BUILD_ROOT=${BUILD_ROOT}
SOURCE_SHA=${SOURCE_SHA}
WEBUI_SHA256=${WEBUI_SHA256}
CANDIDATE_IMAGE=${CANDIDATE_IMAGE}
CANDIDATE_IMAGE_ID=${CANDIDATE_IMAGE_ID}
CANDIDATE_BRAVO_VERSION=${CANDIDATE_BRAVO_VERSION}
CANDIDATE_HOST_VERSION=${CANDIDATE_HOST_VERSION}
EOF
sudo chown ubuntu:ubuntu /srv/bravo-candidate.env
sudo chmod 0600 /srv/bravo-candidate.env
```

Если на instance недостаточно запаса CPU/RAM для production и canary
одновременно, используйте временный второй Graviton. Не урезайте ресурсы
работающего production ради проверки.

### 12.1. Изолированная canary на том же EC2

Canary создаётся как новый пустой runtime. Она не получает production config,
OAuth, проекты или state:

```bash
set -euo pipefail

. /srv/bravo-candidate.env
: "${RELEASE_ID:?invalid candidate record}"
: "${BUILD_ROOT:?invalid candidate record}"
: "${CANDIDATE_IMAGE:?invalid candidate record}"
: "${CANDIDATE_IMAGE_ID:?invalid candidate record}"
CANARY_ROOT="/srv/cliproxyapi-canary-${RELEASE_ID}"

test -z "$(sudo ss -ltnH 'sport = :18319')"
test -z "$(sudo ss -ltnH 'sport = :11455')"
test -z "$(sudo ss -ltnH 'sport = :15455')"
test -z "$(sudo ss -ltnH 'sport = :15121')"

sudo install -d -o ubuntu -g ubuntu "$CANARY_ROOT"
"${BUILD_ROOT}/CLIProxyAPI/deploy/aws/prepare-runtime.sh" "$CANARY_ROOT"

sed -i \
  -e "s/container_name: CLIProxyAPI-Prod/container_name: CLIProxyAPI-Canary-${RELEASE_ID}/" \
  -e 's/127.0.0.1:8317:8317/127.0.0.1:18319:8317/' \
  -e 's/127.0.0.1:1455:1455/127.0.0.1:11455:1455/' \
  -e 's/127.0.0.1:54545:54545/127.0.0.1:15455:54545/' \
  -e 's/127.0.0.1:51121:51121/127.0.0.1:15121:51121/' \
  "${CANARY_ROOT}/docker-compose.yml"

sed -i \
  -e "s|^BRAVO_VERSION=.*|BRAVO_VERSION=${CANDIDATE_BRAVO_VERSION}|" \
  -e "s|^CLIPROXYAPI_VERSION=.*|CLIPROXYAPI_VERSION=${CANDIDATE_HOST_VERSION}|" \
  -e "s|^CLIPROXYAPI_IMAGE=.*|CLIPROXYAPI_IMAGE=${CANDIDATE_IMAGE}|" \
  "${CANARY_ROOT}/.env"

cd "$CANARY_ROOT"
sudo docker compose config --quiet
sudo docker compose up -d --wait --wait-timeout 120
CANARY_CONTAINER="CLIProxyAPI-Canary-${RELEASE_ID}"

test "$(
  sudo docker inspect \
    --format '{{.Image}}' \
    "$CANARY_CONTAINER"
)" = "$CANDIDATE_IMAGE_ID"
test "$(
  sudo docker inspect --format '{{.State.Health.Status}}' "$CANARY_CONTAINER"
)" = healthy
test "$(
  sudo docker inspect --format '{{.RestartCount}}' "$CANARY_CONTAINER"
)" = 0
test "$(
  sudo docker inspect --format '{{.State.OOMKilled}}' "$CANARY_CONTAINER"
)" = false

curl -fsS http://127.0.0.1:18319/healthz | jq -e '.status == "ok"'
test "$(
  curl -fsS http://127.0.0.1:18319/management.html |
    sha256sum |
    awk '{print $1}'
)" = "$WEBUI_SHA256"

. ./.env
. ./secrets.env
CANARY_HEADERS="$(mktemp "${CANARY_ROOT}/headers.XXXXXX")"
chmod 0600 "$CANARY_HEADERS"
trap 'rm -f "$CANARY_HEADERS"' EXIT

CANARY_STATUS_JSON="$(
  curl -fsS \
    --dump-header "$CANARY_HEADERS" \
    --config <(
      printf 'header = "X-Management-Key: %s"\n' "$MANAGEMENT_KEY"
    ) \
    http://127.0.0.1:18319/v0/management/bravo/status
)"
jq -e --arg version "$CANDIDATE_BRAVO_VERSION" \
  '.version == $version and .enabled == true and .degraded == false' \
  <<<"$CANARY_STATUS_JSON"
CANARY_HOST_VERSION="$(
  awk '
    tolower($1) == "x-cpa-version:" {
      sub(/\r$/, "", $2)
      print $2
    }
  ' "$CANARY_HEADERS" |
    tail -n 1
)"
test "$CANARY_HOST_VERSION" = "$CANDIDATE_HOST_VERSION"
CANARY_SOURCE_SHA="$(
  awk '
    tolower($1) == "x-cpa-commit:" {
      sub(/\r$/, "", $2)
      print $2
    }
  ' "$CANARY_HEADERS" |
    tail -n 1
)"
test "$CANARY_SOURCE_SHA" = "$SOURCE_SHA"

rm -f "$CANARY_HEADERS"
trap - EXIT
unset \
  CANARY_HEADERS \
  CANARY_HOST_VERSION \
  CANARY_SOURCE_SHA \
  CANARY_STATUS_JSON \
  MANAGEMENT_KEY \
  ORDINARY_API_KEY
```

Для canary UI и OAuth откройте отдельный tunnel с Mac:

```bash
ssh -N \
  -i /path/to/bravo-aws.pem \
  -L 18319:127.0.0.1:18319 \
  -L 1455:127.0.0.1:11455 \
  -L 54545:127.0.0.1:15455 \
  -L 51121:127.0.0.1:15121 \
  ubuntu@<EC2_PUBLIC_IP>
```

Закройте production tunnel на тех же локальных callback-портах перед запуском
этой команды, иначе SSH не сможет занять `1455`, `54545` и `51121`.

Откройте `http://127.0.0.1:18319/management.html`, введите management key из
canary `secrets.env` и подключите отдельные canary-подписки. Проверьте OpenAI
Chat, OpenAI Responses и Anthropic Messages, streaming, fallback только до
первого видимого payload, project model gate, аналитику и использованную
подписку.

Для release, который меняет обработку ошибок или cooldown, canary обязана
отдельно доказать:

- структурированный `credits_required` для одной физической модели переводит
  запрос на другую разрешённую подписку или сопоставленный provider, но не
  блокирует sibling-модели того же аккаунта;
- context-window overflow остаётся ошибкой только текущего запроса и сохраняет
  упорядоченную причину предшествующего fallback;
- модельный cooldown переживает перезапуск только canary-контейнера;
- Management API, UI, state и логи не содержат raw provider JSON, request ID,
  payment/CTA fields или credential material.

Не продолжайте, если хотя бы одна canary-проверка не зелёная.

### 12.2. Короткое production-переключение

Загрузите сохранённые значения кандидата, затем зафиксируйте старый image:

```bash
set -euo pipefail

. /srv/bravo-candidate.env
: "${RELEASE_ID:?invalid candidate record}"
: "${CANDIDATE_IMAGE:?invalid candidate record}"
: "${CANDIDATE_BRAVO_VERSION:?invalid candidate record}"
: "${CANDIDATE_HOST_VERSION:?invalid candidate record}"

cd /srv/cliproxyapi-prod

test "$(
  sudo docker image inspect --format '{{.Id}}' "$CANDIDATE_IMAGE"
)" = "$CANDIDATE_IMAGE_ID"

. ./.env
OLD_IMAGE_REF="$CLIPROXYAPI_IMAGE"
OLD_IMAGE_ID="$(sudo docker image inspect --format '{{.Id}}' "$OLD_IMAGE_REF")"
test "$(
  sudo docker inspect --format '{{.Image}}' CLIProxyAPI-Prod
)" = "$OLD_IMAGE_ID"

BACKUP_ROOT="/srv/cliproxyapi-backups/${RELEASE_ID}"
test ! -e "$BACKUP_ROOT"
sudo install -d -m 0700 "$BACKUP_ROOT"

sudo tee /srv/bravo-rollback.env >/dev/null <<EOF
RELEASE_ID=${RELEASE_ID}
BACKUP_ROOT=${BACKUP_ROOT}
OLD_IMAGE_REF=${OLD_IMAGE_REF}
OLD_IMAGE_ID=${OLD_IMAGE_ID}
EOF
sudo chown ubuntu:ubuntu /srv/bravo-rollback.env
sudo chmod 0600 /srv/bravo-rollback.env

sudo docker compose stop
sudo cp -a .env docker-compose.yml config.yaml "$BACKUP_ROOT/"
sudo cp -a bravo-data "$BACKUP_ROOT/"

sed -i \
  -e "s|^BRAVO_VERSION=.*|BRAVO_VERSION=${CANDIDATE_BRAVO_VERSION}|" \
  -e "s|^CLIPROXYAPI_VERSION=.*|CLIPROXYAPI_VERSION=${CANDIDATE_HOST_VERSION}|" \
  -e "s|^CLIPROXYAPI_IMAGE=.*|CLIPROXYAPI_IMAGE=${CANDIDATE_IMAGE}|" \
  .env

sudo docker compose config --quiet
sudo docker compose up \
  -d \
  --no-deps \
  --wait \
  --wait-timeout 120 \
  cli-proxy-api
```

Остановка перед копированием нужна, чтобы Bravo штатно сбросил ledger, а
backup `config.yaml` и `bravo-data` описывали одно состояние. Сразу повторите
проверки из шага 7, protocol smoke обоих контрактов и сравните количество
проектов, подписок и маршрутов. Для release обработки ошибок также проверьте
model-specific issue, упорядоченные попытки fallback и отсутствие raw provider
полей в Management API, UI, state и логах. Не создавайте дополнительный
production restart только ради проверки уже доказанной в canary persistence.

### 12.3. Воспроизводимый rollback

Старый tag должен всё ещё указывать на записанный image ID:

```bash
set -euo pipefail

. /srv/bravo-rollback.env
: "${BACKUP_ROOT:?invalid rollback record}"
: "${OLD_IMAGE_REF:?invalid rollback record}"
: "${OLD_IMAGE_ID:?invalid rollback record}"

test "$(
  sudo docker image inspect --format '{{.Id}}' "$OLD_IMAGE_REF"
)" = "$OLD_IMAGE_ID"
```

Если release явно не меняет schema состояния, верните согласованный старый
`.env` и пересоздайте только контейнер:

```bash
set -euo pipefail

. /srv/bravo-rollback.env
: "${BACKUP_ROOT:?invalid rollback record}"

cd /srv/cliproxyapi-prod
test "$(
  sudo docker image inspect --format '{{.Id}}' "$OLD_IMAGE_REF"
)" = "$OLD_IMAGE_ID"
sudo install \
  -o ubuntu \
  -g ubuntu \
  -m 0600 \
  "${BACKUP_ROOT}/.env" \
  .env
sudo docker compose up \
  -d \
  --no-deps \
  --force-recreate \
  --wait \
  --wait-timeout 120 \
  cli-proxy-api
```

Bravo 0.7.9 добавляет в schema v2 опциональное поле активных cooldowns. Это
additive-изменение не требует миграции: существующие schema-v2 snapshots
продолжают загружаться, а старый runtime игнорирует неизвестное опциональное
поле. Поэтому обычный image rollback не требует восстановления state, если нет
отдельного доказательства его повреждения.

Для будущего несовместимого schema-changing release одного отката image
недостаточно. С остановленным контейнером переместите новый `config.yaml` и
`bravo-data` в отдельный failed-release каталог, восстановите оба объекта из
одного pre-cutover backup, затем верните старый `.env` и запустите старый
image.
Release без явно документированной стратегии миграции/rollback в production не
выпускается.

Не запускайте:

- `docker compose pull`;
- `docker system prune` или широкую очистку Docker;
- upstream image `eceasy/cli-proxy-api:latest`;
- `prepare-runtime.sh` поверх существующего production;
- независимое обновление backend и WebUI вне `release.env`.

После успешного soak остановите canary командой `sudo docker compose down`
строго из её `CANARY_ROOT`. Удаляйте только точно названные canary-runtime и
неудачные image этого release. Общий BuildKit cache не имеет надёжного release
ownership, поэтому его оставляйте; широкие `builder prune`/`system prune`
запрещены. OAuth `auths`, production config/state и rollback image очисткой не
затрагиваются.

Следующее инфраструктурное улучшение — GitHub Actions, который собирает
multi-arch release и публикует его в GHCR. Тогда установка AWS сократится до
`docker pull` и `docker compose up -d`; текущая инструкция остаётся
воспроизводимым source-build fallback.
