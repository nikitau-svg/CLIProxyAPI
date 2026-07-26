# Чистая установка актуального стабильного CLIProxyAPI + Bravo на AWS

Эта инструкция создаёт новый независимый production: без переноса конфигурации,
OAuth-файлов, проектов, ключей и аналитики с MacMini.

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
sudo apt update
sudo apt install -y ca-certificates curl git openssl

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
sudo install -d -o ubuntu -g ubuntu /srv/bravo-build
cd /srv/bravo-build

git clone \
  --branch bravo/stable \
  --single-branch \
  https://github.com/nikitau-svg/CLIProxyAPI.git \
  CLIProxyAPI

./CLIProxyAPI/deploy/aws/fetch-management-ui.sh \
  /srv/bravo-build/Cli-Proxy-API-Management-Center
```

Скрипт читает frontend repository и точный commit из release manifest. Он не
берёт независимо обновившуюся `main`/`latest`, поэтому host, plugin и UI
остаются одной проверенной сборкой.

Проверьте выбранный комплект:

```bash
cd /srv/bravo-build/CLIProxyAPI
. ./deploy/aws/release.env

printf 'Bravo %s, image %s\n' "$BRAVO_VERSION" "$CLIPROXYAPI_IMAGE"
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
```

`bun run verify` должен завершить tests, lint, typecheck и production build без
ошибок. Официальный Bun image поддерживает Linux arm64:
[Bun installation](https://bun.com/docs/installation).

## 5. Собрать единый production image

```bash
cd /srv/bravo-build/CLIProxyAPI

. ./deploy/aws/release.env

sudo docker build \
  --platform "$RELEASE_PLATFORM" \
  --build-arg VERSION="$CLIPROXYAPI_VERSION" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
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
cd /srv/cliproxyapi-prod
sudo docker compose up -d
sudo docker compose ps
sudo docker compose logs --tail 100
```

В `docker compose ps` контейнер `CLIProxyAPI-Prod` должен стать `healthy`.

Проверить загрузку Bravo:

```bash
cd /srv/cliproxyapi-prod
. ./.env
. ./secrets.env

curl -fsS \
  -H "X-Management-Key: ${MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/bravo/status

unset MANAGEMENT_KEY ORDINARY_API_KEY
```

В ответе должны присутствовать версия из `BRAVO_VERSION` и состояние enabled.
Если healthcheck зелёный, но этот запрос не работает, deployment ещё не готов.

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
provider, физическую модель, effort, порядок и приоритет. Перед сохранением
маршрут проверяется; кнопка reset возвращает встроенный default. Изменения
сохраняются в `config.yaml`.

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

Для более строгой AWS-схемы вместо Caddy используйте ALB + ACM: EC2 должен
принимать `8317` только от Security Group балансировщика, а management paths
должны быть закрыты правилами балансировщика/WAF.

## 11. Backup после настройки

Хотя установка чистая, после OAuth и создания проектов уже появляются важные
данные:

- `.env` — закреплённые версии и локальный image tag;
- `docker-compose.yml` — runtime topology и mounts;
- `config.yaml` — bcrypt management secret, project key digests, routes, pools;
- `secrets.env` — operator copy management и ordinary break-glass keys;
- `auths/` — OAuth access/refresh tokens;
- `bravo-data/bravo-state.json` — analytics, quotas и allocator ledger.

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
sudo docker compose up -d
```

Не используйте `docker kill`: при штатной остановке Bravo успевает сбросить
ledger на диск.

## 12. Обновление через стабильный канал

Сейчас Bravo image не опубликован в GHCR/Docker Hub, поэтому на AWS он
собирается из согласованных исходников и используется с `pull_policy: never`.
Runtime-изменение публикуется в `bravo/stable` после зелёной изолированной
canary, затем именно из опубликованного commit собирается production image.
Результат production smoke фиксируется отдельным docs-only commit; уже
развёрнутый image остаётся привязан к отдельно записанному и протестированному
runtime source SHA и не требует пересборки из-за одних заметок. Файл
`deploy/aws/release.env` внутри stable является единственным manifest
совместимых версий; старые release tags сохраняются для точного отката.

До появления release image:

- не переключайте compose на `eceasy/cli-proxy-api:latest`;
- не запускайте `docker compose pull`;
- не меняйте backend и WebUI независимо от `release.env`;
- сначала обновляйте checkout `bravo/stable`, собирайте новый image, проверяйте
  его на canary, затем
  меняйте `CLIPROXYAPI_IMAGE`.

Следующее инфраструктурное улучшение — GitHub Actions, который собирает
multi-arch release и публикует его в GHCR. Тогда установка AWS сократится до
`docker pull` и `docker compose up -d`; текущая инструкция остаётся
воспроизводимым source-build fallback.
