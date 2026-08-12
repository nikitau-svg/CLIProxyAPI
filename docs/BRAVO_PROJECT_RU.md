# О проекте Bravo

## Идентичность проекта

**Bravo разрабатывается компанией Slowdive. Создатель — Никита Усков.**
Репозиторий является настоящим GitHub fork CLIProxyAPI, но продуктовая линия
Bravo, её проектные ключи, маршруты, диагностика, UI-контракты и правила
эксплуатации поддерживаются отдельно.

Это не официальный релиз CLIProxyAPI и не зеркало, которое автоматически
следует за upstream. Такое разделение намеренное: изменение внешнего ядра не
должно незаметно менять production-поведение Bravo.

## Почему это fork, а не внешний набор патчей

Ключевые гарантии Bravo проходят через несколько уровней CLIProxyAPI:

- входные OpenAI и Anthropic протоколы;
- project-key authentication и allowed pool;
- plugin plan, retry и fallback;
- host callbacks и закрепление физического аккаунта;
- request translation, streaming и provider error classification;
- prompt caching, quota snapshots, analytics и management UI.

Отдельный внешний proxy wrapper видел бы только часть этого пути и был бы
вынужден угадывать, дошёл ли запрос до provider, можно ли безопасно повторить
stream или какой capability был потерян при переводе. Fork позволяет менять и
тестировать весь путь атомарно, при этом точная upstream-база остаётся видимой
и проверяемой.

Практический результат: stable, документация, миграции и rollback относятся к
одному commit. Обновление upstream становится осознанной интеграцией, а не
случайным изменением работающего сервиса.

## Зафиксированная основа

| Компонент | Зафиксированная версия |
| --- | --- |
| Upstream repository | [`router-for-me/CLIProxyAPI`](https://github.com/router-for-me/CLIProxyAPI) |
| Upstream tag | [`v7.2.94`](https://github.com/router-for-me/CLIProxyAPI/tree/v7.2.94) |
| Upstream commit | [`36b45d57a3e804b9dfcee307e5d7b3e8cea5acfc`](https://github.com/router-for-me/CLIProxyAPI/commit/36b45d57a3e804b9dfcee307e5d7b3e8cea5acfc) |
| Bravo stable | `0.8.11` |
| Stable feature snapshot | [`4af9679916dbfa9fdfe756e1bd9370dd591466e9`](https://github.com/nikitau-svg/CLIProxyAPI/commit/4af9679916dbfa9fdfe756e1bd9370dd591466e9) |

Commit upstream выше является предком stable-линии Bravo. Поэтому ссылка ведёт
не на «примерно похожую» версию, а на точную основу текущего форка.

## Линии разработки

### `bravo/main`

Публичная точка входа и default branch. Здесь находится текущая стабильная
линия Bravo без незавершённого адаптивного лимитного роутинга. README и ссылки
на документацию должны быть достоверны именно для этой ветки.

### `codex/bravo-*`

Проверяемые кандидатные изменения. Такая ветка не считается релизом только
потому, что она опубликована или для неё открыт pull request.

### `bravo/stable`

Историческая замороженная линия экспериментального allocator 0.8.4. Она
сохранена для совместимости и аудита, но больше не является installation
channel. Её точная архивная копия доступна как
`archive/bravo-0.8.4-adaptive`; актуальный stable всегда берите из
`bravo/main`.

### `main`

Историческая upstream-линия форка. Она сохранена для сравнения с исходным
CLIProxyAPI, но пользователи Bravo не должны брать её как production source.

## Текущая матрица версий

| Версия | Назначение | Статус |
| --- | --- | --- |
| `0.8.10` | Безопасный fallback, vision, tool aliases, `json_object`, нормализация Claude sampling | previous stable |
| `0.8.11` | Возможности 0.8.10 плюс проектные endpoints лимитов и прозрачных маршрутов; без нового adaptive allocator | stable |

Наличие stable-кода в GitHub само по себе не меняет production. Сборка и
переключение контейнера выполняются только отдельной явной операцией после
проверки точного release commit.

## Как обновляется upstream

1. Выбирается конкретный upstream tag и commit.
2. Изменения сравниваются с текущей pinned-базой.
3. Отдельно проверяются интеграция плагина, host callbacks, протоколы OpenAI и
   Anthropic, streaming и сохранность state.
4. Обновляется эта таблица и ссылка в корневом README.
5. Только после тестов новая база входит в stable-ветку.

Плавающие ссылки вроде `upstream/main` не считаются спецификацией версии.

## Релизный контракт

Перед выпуском новой stable-версии обязательно:

1. поднять версию плагина и manifests;
2. пройти Go-тесты, race-проверки затронутого контура и сборку;
3. проверить русские клиентские ошибки и отсутствие секретов в trace/log;
4. проверить миграцию и откат состояния;
5. обновить README, версионную матрицу и операторские инструкции;
6. зафиксировать commit, из которого собран контейнер;
7. отдельно получить разрешение на GitHub release и production rollout.

Публикация ветки или draft PR сама по себе не меняет production.

## Где искать документацию

- [`../BRAVO_MODELS_AND_KEYS_RU.md`](../BRAVO_MODELS_AND_KEYS_RU.md) — модели,
  проектные ключи, allowlist и fallback;
- [`../AWS_INSTALL_RU.md`](../AWS_INSTALL_RU.md) — чистая установка;
- [`../CLAUDE_CODE_BRAVO_RU.md`](../CLAUDE_CODE_BRAVO_RU.md) — Claude Code,
  agent teams, project status и готовая инструкция для команды;
- [`../plugins/bravo/README.md`](../plugins/bravo/README.md) — технический
  контракт плагина;
- [`../CONTRIBUTING.md`](../CONTRIBUTING.md) — баги, логи и изменения;
- [`../BRAVO_PRODUCTION_RUNBOOK_RU.md`](../BRAVO_PRODUCTION_RUNBOOK_RU.md) —
  исторический runbook миграции 0.5.0, не инструкция для нового релиза.

## Атрибуция

Исходное ядро CLIProxyAPI и его авторские уведомления сохраняются по лицензии
MIT. Название Bravo относится к изменениям и продуктовой линии этого форка;
оно не заявляет авторство над исходным CLIProxyAPI.
