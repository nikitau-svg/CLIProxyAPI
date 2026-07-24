# Bravo 0.5.0: шаблон production runbook

> Исторический runbook миграции production 0.5.0. Не используйте его для новой
> установки или обновления актуального stable-канала.
>
> Для новой установки на AWS без переноса данных используйте
[`AWS_INSTALL_RU.md`](AWS_INSTALL_RU.md).

Публичный шаблон использует:

```bash
BRAVO_ROOT=/srv/cliproxyapi-prod
BRAVO_ORIGIN=http://127.0.0.1:8317
```

Замените значения на свой deployment root и опубликованный origin.

## Где теперь production

- API: `$BRAVO_ORIGIN`
- OpenAI base URL: `$BRAVO_ORIGIN/v1`
- Админка: `$BRAVO_ORIGIN/management.html`
- Image: `cliproxyapi-local:v7.2.94-bravo-native0.5.0`
- Image digest:
  `sha256:605c9888b2f58c2d3db37575efecd2663b90e298f60b005315b073672b420b18`
- Страница Bravo: в админке открыть `Bravo` в левом меню.
- Production-каталог: `$BRAVO_ROOT`
- Compose: `$BRAVO_ROOT/docker-compose.yml`
- Приватный ledger/снимки квот:
  `$BRAVO_ROOT/bravo-data/bravo-state.json`
- OAuth-файлы: `$BRAVO_ROOT/auths`
- Management key: `$BRAVO_ROOT/secrets.env`

Значение management key намеренно не записано в документацию. На MacMini:

```bash
grep '^MANAGEMENT_KEY=' "$BRAVO_ROOT/secrets.env"
```

## Как создать ключ

1. Открыть админку и войти с `MANAGEMENT_KEY`.
2. В левом меню открыть `Bravo`.
3. Нажать `Создать проект`.
4. Дать понятное имя: один проект обычно равен одному человеку, CLI или
   продукту.
5. В `Разрешённый пул подписок` выбрать:
   - `Разрешить весь пул Bravo`, если проект может использовать любые текущие
     и будущие аккаунты;
   - либо отключить этот флаг и отметить только личные/рабочие подписки,
     доступные этому проекту.
6. При необходимости выбрать основные подписки внутри разрешённого пула. Bravo
   расходует их первыми и разрешает опустить до нуля. Одна подписка может быть
   основной только у одного активного проекта, но может входить в разрешённые
   secondary-пулы нескольких проектов.
7. Обычно оставить `Все модели Bravo`. Ограничение моделей раскрывается только
   при необходимости.
8. Нажать `Создать и выпустить ключ`.
9. Сохранить показанный `brv_...` ключ. Второй раз plaintext не показывается;
   в конфиге хранится только SHA-256.

Карточки проектов и пул подписок закрыты по умолчанию. Ключ можно отключить,
перевыпустить или удалить из карточки проекта. Разрешённый пул меняется через
`Редактировать`. Отдельный пул является жёсткой границей: primary, retry и
fallback не могут уйти в подписку вне него. Если сохранённая подписка исчезла,
проект fail-closed, а не возвращается к общему пулу.

## Claude Code

Модалка нового ключа уже показывает готовую команду. Эквивалентная ручная
настройка:

```bash
export ANTHROPIC_BASE_URL="$BRAVO_ORIGIN"
export ANTHROPIC_AUTH_TOKEN='<project Bravo key>'
export ANTHROPIC_DEFAULT_OPUS_MODEL=bravo/opus
export ANTHROPIC_DEFAULT_SONNET_MODEL=bravo/sonnet
export ANTHROPIC_DEFAULT_HAIKU_MODEL=bravo/haiku

claude --model opus --effort xhigh
```

Внутри Claude Code работают `/effort low|medium|high|xhigh|max` и обычный
`--effort`. Bravo переносит effort на каждый совместимый fallback. Если effort
не задан, используется настройка logical model.

Claude Code может передавать PNG/JPEG как в новом сообщении, так и внутри
старого `tool_result` в истории. В Anthropic Messages этот vision-контракт
проверен для Claude- и Codex-кандидатов, включая streaming и adaptive
`xhigh`. Поэтому сессия не ломается только из-за скриншотов в старой истории.
PDF/документы считаются отдельным `file_input`-контрактом и пока отклоняются,
а не удаляются из запроса.

## OpenAI-совместимый клиент

```text
Base URL: $BRAVO_ORIGIN/v1
API key:  <тот же project Bravo key>
Model:    bravo/opus, bravo/sonnet, bravo/haiku, bravo/fast и т. д.
```

Один smart key одинаково работает через OpenAI Chat Completions, OpenAI
Responses и Anthropic Messages.

## Как allocator расходует подписки

- Основные подписки проекта идут первыми и могут опускаться до `0%`.
- Остальные разрешённые проекту подписки образуют его вторичный пул. При
  включённом строгом пуле глобальные аккаунты вне списка не рассматриваются.
- Вторичная подписка допускается только при подтверждённых provider quota и
  когда одновременно сессионный и недельный остаток выше обоих порогов.
- Автоопределение учитывает провайдера: Claude personal/pro/plus получает
  `x1`, Claude team/business/enterprise/max — `x5`, OpenAI Codex Pro/Pro Lite
  — `x20`.
- Дефолтные резервы: `x1` — `50%` сессии и `50%` недели; `x5` — `30%` и
  `30%`; `x20` — `20%` и `20%` при reservation `0.05`. Каждый тариф
  настраивается отдельно.
- Пороги редактируются в `Пул подписок → Резервы тарифов`.
- Между допустимыми secondary Bravo выбирает не первый файл, а наименее
  напряжённую подписку: учитывает минимальный headroom над порогами,
  тариф-нормализованные недельные токены, активные/pending reservations и
  стабильный rendezvous tie-break.
- Неизвестная quota не считается равной `100%` и по умолчанию блокирует
  вторичное использование.

Retry/fallback происходит внутри одного клиентского запроса, пока downstream
ещё не получил первый payload: Claude Code обычно видит только чуть большую
задержку. После начала stream безопасно заменить провайдера уже нельзя —
текущий запрос завершается ошибкой, проблемная подписка охлаждается, и
следующий запрос уходит в следующий допустимый fallback.

Anthropic возвращает `reset=null`, пока новая 5-часовая сессия ещё не началась.
UI показывает это как `Сессия не началась`; остаток подтверждён как `100%`, а
таймер появится после первого использования. Если план Codex вообще не имеет
сессионного окна, UI явно показывает `У провайдера нет такого окна лимита`;
недельное окно при этом остаётся рабочим.

## Учёт по каждому ключу

Да: smart key связан со стабильным project ID. В карточке проекта раскрывается
`Аналитика потребления`: 24 часа, 7/30/90 дней или свой UTC-период, сравнение с
предыдущим периодом, запросы, ошибки, latency, input/output/reasoning/cache и
total tokens, график/таблица и CSV. Ниже показываются точная обслужившая
подписка, logical route и физическая модель.

Исторические project totals до обновления сохранены. Достоверная детализация
`проект × подписка × модель` собирается с установки 0.5.0; UI явно показывает
границу покрытия и не рисует старую неизвестную детализацию нулями. Hourly
buckets хранятся 31 день, daily — 400 дней. В API уходят только стабильные
redacted `sub_*`, сырые auth identity и plaintext ключи не выдаются.

Данные возвращаются authenticated Management API:

```text
GET  /v0/management/bravo/projects
GET  /v0/management/bravo/subscriptions
GET  /v0/management/bravo/analytics
GET  /v0/management/bravo/routes
PUT  /v0/management/bravo/routes
POST /v0/management/bravo/routes/reset
POST /v0/management/bravo/quotas/refresh
```

Маршруты меняются на горячую в раскрытии `Маршруты логических моделей`.
`PUT` с `preview: true` валидирует provider/model/effort и порядок без
сохранения; обычный `PUT` применяет маршрут, reset возвращает встроенный
default. Capabilities видны, но не редактируются вручную: неподтверждённый
контракт остаётся fail-closed. Редактор сейчас глобальный; per-project route
overrides остаются будущим улучшением.

## Проверка и эксплуатация

```bash
cd "$BRAVO_ROOT"

docker compose ps
ruby bravo-smoke.rb --base-url "$BRAVO_ORIGIN"
ruby bravo-vision-smoke.rb \
  --base-url "$BRAVO_ORIGIN" \
  --config config.yaml \
  --smart-key-file bravo-smart-key.txt \
  --key-mode smart \
  --models bravo/opus,bravo/sol \
  --placements tool_result \
  --effort xhigh
ruby bravo-quota-allocator-smoke.rb \
  --base-url "$BRAVO_ORIGIN" \
  --allow-other-target \
  --allow-production-quota-refresh \
  --management-env-file secrets.env \
  --management-env-variable MANAGEMENT_KEY
```

Контейнер `CLIProxyAPI-Prod` имеет встроенный liveness healthcheck. Служебный
Bravo state вынесен из auth-dir в отдельный volume, поэтому он не появляется
как ложная пятая подписка.

## Откат

Backup непосредственно перед переходом на 0.5.0:

```text
$BRAVO_ROOT/backups/pre-bravo-0.5.0
```

0.5.0 мигрирует `bravo-state.json` со schema v1 на v2. Поэтому для отката к
0.4.1 сначала обязательно остановить 0.5.0, затем вернуть compose, config и
state из одного backup:

```bash
cd "$BRAVO_ROOT"
/usr/local/bin/docker compose stop
cp -p backups/pre-bravo-0.5.0/docker-compose.yml docker-compose.yml
cp -p backups/pre-bravo-0.5.0/config.yaml config.yaml
cp -p backups/pre-bravo-0.5.0/bravo-state.json \
  bravo-data/bravo-state.json
docker compose up -d --force-recreate
```

При необходимости вернуть сохранённый canary:

```bash
docker start CLIProxyAPI-Canary
```

Не удаляйте предыдущий image и backup до завершения периода наблюдения.

## Что всё ещё fail-closed

Пока не подтверждены полные двусторонние контракты, Bravo синхронно отклоняет,
а не молча теряет: image-generation streaming, vision через OpenAI
Chat/Responses, PDF/document/file input, произвольные structured-output/schema
и background contracts, web-search domain filters, manual reasoning
budgets/summaries и неизвестные provider-specific built-in tools. Обычные
function tools, tool results, named effort, streaming text, token count, web
search без domain filters и vision через Anthropic Messages проверены.

## Следующие улучшения

- графики истории session/week quota и burn velocity;
- операторские бюджеты/rate caps по project key;
- fairness debt/credit между тяжёлыми и редкими проектами;
- уведомления до пересечения reserve floor;
- подтверждение OpenAI vision и оставшихся schema/background/tool контрактов;
- импорт/экспорт policy templates для десятков проектов.
