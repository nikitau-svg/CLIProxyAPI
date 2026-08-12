# Bravo

### Проектный AI-шлюз поверх Claude, Codex и других CLI-подписок

**Bravo разрабатывается компанией Slowdive. Создатель — Никита Усков.** Это
самостоятельно поддерживаемый форк
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), а не официальный
репозиторий CLIProxyAPI. Bravo добавляет проектные ключи, логические модели,
контролируемый fallback между провайдерами, русские диагностические ошибки,
аналитику и управление подписками из одной админки.

> База зафиксирована: [CLIProxyAPI v7.2.94](https://github.com/router-for-me/CLIProxyAPI/tree/v7.2.94),
> точный upstream commit
> [`36b45d57`](https://github.com/router-for-me/CLIProxyAPI/commit/36b45d57a3e804b9dfcee307e5d7b3e8cea5acfc).
> Upstream не подтягивается в Bravo автоматически.

> **Один проектный ключ. Два стандартных API. Несколько подписок.**
> Claude Code, pet-проект, внутренний бот и production-сервис могут обращаться
> к одному и тому же Bravo через привычный Anthropic или OpenAI контракт.

## Текущий статус

| Линия | Статус | Что это |
| --- | --- | --- |
| **Bravo 0.8.10** | stable | Рабочая линия без нового адаптивного лимитного роутинга. Исходный снимок: [`59fc4f02`](https://github.com/nikitau-svg/CLIProxyAPI/commit/59fc4f0260831a192aac782cdea3950d91d0d4d6). |
| **Bravo 0.8.11** | preview | Новый проектный API лимитов/маршрутов и адаптивный allocator. Пока только [draft PR #29](https://github.com/nikitau-svg/CLIProxyAPI/pull/29), не production stable. |
| **CLIProxyAPI v7.2.94** | pinned upstream | Точная внешняя основа, от которой ведётся Bravo. |

Публичная стабильная ветка этого проекта — **`bravo/main`**. Ветка `main`
сохраняется как историческая копия upstream и не является актуальной точкой
входа в Bravo. Полная политика веток и обновлений описана в
[`docs/BRAVO_PROJECT_RU.md`](docs/BRAVO_PROJECT_RU.md).

## Что даёт Bravo

- один проектный ключ `brv_...` для Anthropic- и OpenAI-совместимых клиентов;
- логические модели `bravo/opus`, `bravo/sonnet`, `bravo/terra` и другие;
- явный пул подписок и allowlist моделей для каждого проекта;
- fallback между совместимыми Claude и Codex-моделями до первого видимого
  ответа клиенту;
- мультимодальный ввод PNG/JPEG, tool use и OpenAI JSON mode;
- безопасную нормализацию несовместимых параметров разных провайдеров;
- русские клиентские ошибки и route trace без раскрытия ключей и OAuth-данных;
- управление проектами, маршрутами, квотами и аналитикой в обычной встроенной
  админке CLIProxyAPI.

## Не только Claude Code

Bravo можно использовать для pet-проектов, прототипов, cron-задач, внутренних
ботов и агентных систем — практически для любого приложения, которое умеет
работать с OpenAI- или Anthropic-совместимым API.

Не обязательно принимать конкретный agent framework. Можно использовать
OpenAI Agents SDK, Anthropic SDK, LangChain, OpenClaw, собственный HTTP-клиент
или вообще небольшой скрипт: достаточно заменить `base_url`, выдать проектный
ключ и выбрать `bravo/*` модель. Bravo является транспортным и policy-слоем;
агентная логика, tools и память остаются в выбранном приложении.

OpenAI-совместимый проект:

```bash
export OPENAI_BASE_URL='http://<server>:8317/v1'
export OPENAI_API_KEY='<project-key-brv_...>'
```

Anthropic-совместимый проект:

```bash
export ANTHROPIC_BASE_URL='http://<server>:8317'
export ANTHROPIC_API_KEY='<project-key-brv_...>'
```

Это особенно удобно перед прямыми API-расходами: проект можно запускать и
отлаживать на уже разрешённых CLI-подписках, видеть его отдельную аналитику и
только затем решать, нужен ли ему платный API-канал. Подходящие проекты можно
оставлять на подписочном пуле и дальше. Это не обещание бесплатного или
безлимитного inference: действуют реальные квоты, rate limits и условия
подключённых провайдеров.

Если capability нельзя безопасно перенести между Claude и Codex, Bravo не
притворяется, что всё совместимо: маршрут завершается понятной fail-closed
ошибкой до искажения запроса. Благодаря этому прототип можно постепенно
доводить до production, не меняя внешний API-контракт вслепую.

## Кэширование промптов

Prompt caching настраивается отдельно для каждого проекта в той же встроенной
админке CLIProxyAPI:

- Claude: `auto`, `5m` или `1h`; Bravo переносит настройку в нативный запрос
  после выбора физического кандидата;
- OpenAI/Codex: provider-managed caching; поддерживаемая cache identity
  изолируется проектом;
- retries на той же паре provider/account могут повторно использовать точный
  подходящий prefix;
- переход на другой аккаунт или provider может закономерно дать cache miss;
- Bravo не хранит тексты промптов и ответы модели в своей аналитике.

Кэш снижает повторный ввод и задержку на длинных стабильных префиксах, но не
меняет ответ и не скрывает обычный запрос при cache miss.

## Почему полноценный fork — преимущество

Bravo нельзя было надёжно собрать как случайный набор runtime-патчей поверх
плавающего upstream. Маршрутизация затрагивает ядро, host callbacks, streaming,
перевод OpenAI ↔ Anthropic, tool use, vision, JSON mode, prompt caching и
сохранность ошибок до клиента. Полноценный fork даёт:

1. **Воспроизводимость.** Видны точные upstream tag/commit и commit каждой
   версии Bravo.
2. **Сквозные гарантии.** Изменение можно проверить от входного HTTP-запроса до
   конкретного provider/account, включая retry и первый stream payload.
3. **Безопасный stable.** Рабочая 0.8.10 не смешивается с preview allocator
   0.8.11.
4. **Нормальный откат.** Код, документация и миграции state зафиксированы
   вместе, а не разбросаны по локальным файлам сервера.
5. **Проверяемую связь с upstream.** Сохраняются история, MIT-атрибуция и
   возможность выборочно переносить изменения CLIProxyAPI без автоматического
   изменения production-поведения.
6. **Собственный продуктовый контракт Slowdive.** Проектные ключи, русские
   ошибки, изоляция пулов, аналитика и интерфейс развиваются как единая система.

## Быстрый старт

### 1. Установите сервер

Для чистой установки используйте пошаговую инструкцию
[`AWS_INSTALL_RU.md`](AWS_INSTALL_RU.md). Если инстанс уже развёрнут, откройте:

```text
http://<server>:8317/management.html
```

Это обычная встроенная админка CLIProxyAPI, а не отдельный сервис или второй
сайт. В ней выберите **Bravo → Создать проект**, задайте разрешённые модели и
подписки, затем сохраните показанный один раз ключ `brv_...`.

### 2. Подключите Claude Code

Для Claude Code рекомендуется версия `2.1.223` или новее и следующий профиль
в `~/.claude/settings.json`. Если файл уже существует, добавьте поля в его
текущий объект `env`, а не перезаписывайте остальные настройки:

```json
{
  "env": {
    "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1",
    "CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT": "1",
    "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "1000000",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "700000",
    "CLAUDE_CODE_ALWAYS_ENABLE_EFFORT": "1",
    "ENABLE_TOOL_SEARCH": "true",
    "ANTHROPIC_BASE_URL": "http://<server>:8317",
    "ANTHROPIC_AUTH_TOKEN": "<project-key-brv_...>",
    "ANTHROPIC_DEFAULT_FABLE_MODEL": "bravo/fable",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "bravo/opus",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "bravo/sonnet",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "bravo/haiku",
    "CLAUDE_CODE_SUBAGENT_MODEL": "bravo/opus",
    "ANTHROPIC_MODEL": "bravo/opus",
    "CLAUDE_CODE_EFFORT_LEVEL": "max"
  }
}
```

После сохранения перезапустите Claude Code и проверьте `claude --version`,
`/model`, `/effort` и `/status`. Профиль настроен на максимальное качество:
главная сессия и subagents используют `bravo/opus` с effort `max`, поэтому он
быстрее расходует лимиты. Для более экономной командной работы замените эти
два значения на `bravo/sonnet` или `bravo/terra`.

Окно auto-compact выставлено на 700K при заявленном окне 1M. Это защищает
длинные сессии, но не делает context window физической fallback-модели больше:
если конкретный кандидат меньше текущей истории, Bravo честно вернёт ошибку
контекста и предложит `/compact`.

Полная инструкция и готовый пакет для коллег:
[`CLAUDE_CODE_BRAVO_RU.md`](CLAUDE_CODE_BRAVO_RU.md).

### 3. Подключите OpenAI-совместимый клиент

```text
Base URL: http://<server>:8317/v1
API key:  <тот же project-key-brv_...>
Model:    bravo/terra
```

Один и тот же проектный ключ работает через Anthropic Messages, OpenAI Chat
Completions и OpenAI Responses. Полное объяснение ключей, пулов и встроенных
маршрутов: [`BRAVO_MODELS_AND_KEYS_RU.md`](BRAVO_MODELS_AND_KEYS_RU.md).

### 4. Лимиты и прозрачные маршруты проекта

Начиная с Bravo **0.8.11**, ключ проекта получает две read-only ручки:

```bash
curl -sS "${ANTHROPIC_BASE_URL%/}/v1/bravo/limits?format=text" \
  -H "Authorization: Bearer ${ANTHROPIC_AUTH_TOKEN}"

curl -sS "${ANTHROPIC_BASE_URL%/}/v1/bravo/routes" \
  -H "Authorization: Bearer ${ANTHROPIC_AUTH_TOKEN}"
```

`limits` показывает доступность Claude/Codex, окна и время сброса, а JSON-вид
дополнительно содержит usage проекта за 30 дней. Результат обновляется не чаще
одного раза в час на проект. `routes` объясняет, какие физические модели стоят
за разрешёнными этому ключу `bravo/*`, их порядок и capabilities.

Эти команды можно отдавать коллегам вместе с **их собственным** проектным
ключом: так каждый строит свой CLI и агентов по фактическим возможностям пула,
не получая доступ к чужим проектам или OAuth-аккаунтам. Сам `brv_...` остаётся
секретом — не вставляйте его прямо в общие инструкции или чаты.

Stable 0.8.10 этих двух project endpoints ещё не содержит; они находятся в
[preview 0.8.11](https://github.com/nikitau-svg/CLIProxyAPI/pull/29). Эта пометка
будет снята после отдельного выпуска 0.8.11.

## Документация

| Задача | Документ |
| --- | --- |
| Понять, что даёт fork, на чём он основан и какие ветки использовать | [`docs/BRAVO_PROJECT_RU.md`](docs/BRAVO_PROJECT_RU.md) |
| Установить чистый сервер | [`AWS_INSTALL_RU.md`](AWS_INSTALL_RU.md) |
| Настроить Claude Code, agent teams, `/bravo-limits` и командный onboarding | [`CLAUDE_CODE_BRAVO_RU.md`](CLAUDE_CODE_BRAVO_RU.md) |
| Создать проектный ключ и настроить модели/fallback | [`BRAVO_MODELS_AND_KEYS_RU.md`](BRAVO_MODELS_AND_KEYS_RU.md) |
| Понять устройство плагина Bravo | [`plugins/bravo/README.md`](plugins/bravo/README.md) |
| Внести изменение или оформить баг | [`CONTRIBUTING.md`](CONTRIBUTING.md) |
| Посмотреть оригинальный проект именно той версии, от которой сделан форк | [CLIProxyAPI v7.2.94](https://github.com/router-for-me/CLIProxyAPI/tree/v7.2.94) |

Исходники интерфейса развиваются в отдельном репозитории
[Cli-Proxy-API-Management-Center](https://github.com/nikitau-svg/Cli-Proxy-API-Management-Center/tree/feat/bravo-quota-polling-ui),
но пользователю не нужно открывать или устанавливать вторую админку: её сборка
встраивается в обычный `/management.html` этого сервера.

## Структура репозитория

- `plugins/bravo/` — маршрутизация, проектные ключи, квоты, диагностика и
  management API Bravo;
- `internal/`, `sdk/`, `cmd/` — зафиксированное ядро CLIProxyAPI и необходимые
  изменения интеграции;
- `deploy/`, `docker-compose*.yml` — контейнерное развёртывание;
- `scripts/`, `test/` и `*_TEST_PLAN.md` — проверки релизных контрактов;
- `BRAVO_*_RU.md` — операторская документация на русском языке.

## Происхождение и лицензия

Bravo распространяется по лицензии [MIT](LICENSE) и сохраняет атрибуцию
оригинального CLIProxyAPI. Связь GitHub fork с upstream также сохранена на
уровне самого репозитория.

Изменения Bravo поддерживаются в
[`nikitau-svg/CLIProxyAPI`](https://github.com/nikitau-svg/CLIProxyAPI).
Ошибки оригинального ядра, воспроизводимые на чистом upstream v7.2.94 без
Bravo, следует сверять с
[`router-for-me/CLIProxyAPI`](https://github.com/router-for-me/CLIProxyAPI).
