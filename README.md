# Bravo

### Проектный AI-шлюз поверх Claude, Codex и других CLI-подписок

**Проект Никиты Ускова.** Bravo — самостоятельно поддерживаемый форк
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), а не официальный
репозиторий CLIProxyAPI. Он добавляет проектные ключи, логические модели,
контролируемый fallback между провайдерами, русские диагностические ошибки,
аналитику и управление подписками из одной админки.

> База зафиксирована: [CLIProxyAPI v7.2.94](https://github.com/router-for-me/CLIProxyAPI/tree/v7.2.94),
> точный upstream commit
> [`36b45d57`](https://github.com/router-for-me/CLIProxyAPI/commit/36b45d57a3e804b9dfcee307e5d7b3e8cea5acfc).
> Upstream не подтягивается в Bravo автоматически.

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
- управление проектами, маршрутами, квотами и аналитикой через Management
  Center.

## Быстрый старт

### 1. Установите сервер

Для чистой установки используйте пошаговую инструкцию
[`AWS_INSTALL_RU.md`](AWS_INSTALL_RU.md). Если инстанс уже развёрнут, откройте:

```text
http://<server>:8317/management.html
```

В админке выберите **Bravo → Создать проект**, задайте разрешённые модели и
подписки, затем сохраните показанный один раз ключ `brv_...`.

### 2. Подключите Claude Code

```bash
export ANTHROPIC_BASE_URL='http://<server>:8317'
export ANTHROPIC_AUTH_TOKEN='<project-key-brv_...>'
export ANTHROPIC_DEFAULT_OPUS_MODEL='bravo/opus'
export ANTHROPIC_DEFAULT_SONNET_MODEL='bravo/sonnet'
export ANTHROPIC_DEFAULT_HAIKU_MODEL='bravo/haiku'

claude --model sonnet
```

### 3. Подключите OpenAI-совместимый клиент

```text
Base URL: http://<server>:8317/v1
API key:  <тот же project-key-brv_...>
Model:    bravo/terra
```

Один и тот же проектный ключ работает через Anthropic Messages, OpenAI Chat
Completions и OpenAI Responses. Полное объяснение ключей, пулов и встроенных
маршрутов: [`BRAVO_MODELS_AND_KEYS_RU.md`](BRAVO_MODELS_AND_KEYS_RU.md).

## Документация

| Задача | Документ |
| --- | --- |
| Понять, чей это проект, на чём он основан и какие ветки использовать | [`docs/BRAVO_PROJECT_RU.md`](docs/BRAVO_PROJECT_RU.md) |
| Установить чистый сервер | [`AWS_INSTALL_RU.md`](AWS_INSTALL_RU.md) |
| Создать проектный ключ и настроить модели/fallback | [`BRAVO_MODELS_AND_KEYS_RU.md`](BRAVO_MODELS_AND_KEYS_RU.md) |
| Понять устройство плагина Bravo | [`plugins/bravo/README.md`](plugins/bravo/README.md) |
| Внести изменение или оформить баг | [`CONTRIBUTING.md`](CONTRIBUTING.md) |
| Посмотреть оригинальный проект именно той версии, от которой сделан форк | [CLIProxyAPI v7.2.94](https://github.com/router-for-me/CLIProxyAPI/tree/v7.2.94) |

Интерфейс управления развивается отдельно в
[Cli-Proxy-API-Management-Center](https://github.com/nikitau-svg/Cli-Proxy-API-Management-Center/tree/feat/bravo-quota-polling-ui).

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
