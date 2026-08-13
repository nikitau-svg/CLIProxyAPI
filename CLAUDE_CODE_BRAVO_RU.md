# Claude Code через Bravo

Эта инструкция предназначена для сотрудников и команд, которым выдан
отдельный проектный ключ Bravo. Никакой второй админки устанавливать не нужно:
проект создаётся владельцем в обычной встроенной админке CLIProxyAPI по адресу
`http://<server>:8317/management.html`.

## Требования

- Claude Code `2.1.223` или новее;
- URL сервера Bravo;
- отдельный ключ проекта `brv_...`;
- разрешённые проекту логические модели.

Версия `2.1.223+` нужна для корректной обработки неизвестного gateway model ID
и настройки его окна. Проверка и обновление:

```bash
claude --version
claude update
```

## Рекомендуемый профиль Slowdive

Откройте `~/.claude/settings.json`. Если файл уже существует, объедините
следующий `env` с существующим объектом: не удаляйте permissions, hooks,
plugins и другие пользовательские настройки.

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

Здесь используются официальные имена `ANTHROPIC_MODEL` и
`CLAUDE_CODE_EFFORT_LEVEL`. Похожие варианты `CLAUDE_CODE_MODEL` и
`CLAUDE_CODE_EFFORT` не являются объявленными переменными актуального Claude
Code.

Что делает профиль:

- включает экспериментальные agent teams;
- сообщает Claude Code о контексте 1M и запускает auto-compact около 700K;
- направляет aliases Fable/Opus/Sonnet/Haiku в одноимённые логические маршруты
  Bravo;
- фиксирует main session и subagents на `bravo/opus` с effort `max`;
- явно включает effort для gateway model IDs;
- сохраняет MCP tool search при стороннем `ANTHROPIC_BASE_URL`.

`bravo/opus` + `max` — профиль максимального качества, а не экономии. Для
повседневной параллельной работы можно заменить `ANTHROPIC_MODEL` и
`CLAUDE_CODE_SUBAGENT_MODEL` на `bravo/sonnet` или `bravo/terra`, а effort —
на `xhigh`/`high`.

После сохранения полностью перезапустите Claude Code и проверьте:

```text
/model
/effort
/status
```

## Лимиты прямо в Claude Code — без обращения к модели

Не оформляйте проверку лимитов как skill или пользовательскую slash-команду.
Skill остаётся модельным prompt: Claude Code добавляет его в текущую переписку
и отправляет весь актуальный контекст выбранной модели. Поэтому в уже большой
сессии `/bravo-limits` способен получить `bravo_context_window_exceeded` ещё
до показа результата `curl`.

Вместо этого используйте встроенный shell mode Claude Code. Введите строку,
начинающуюся с `!`: она выполняется локально и **не вызывает Claude, Codex или
любой другой модельный маршрут**:

```bash
!curl -sS "${ANTHROPIC_BASE_URL%/}/v1/bravo/limits?format=text" -H "Authorization: Bearer ${ANTHROPIC_AUTH_TOKEN}"
```

Это работает даже тогда, когда текущий модельный контекст уже не помещается.
Вывод будет добавлен в transcript, поэтому перед следующим обычным запросом
всё равно может потребоваться встроенная команда `/compact`.

Ту же проверку можно выполнить в обычном терминале вне Claude Code:

```bash
curl -sS "${ANTHROPIC_BASE_URL%/}/v1/bravo/limits?format=text" \
  -H "Authorization: Bearer ${ANTHROPIC_AUTH_TOKEN}"
```

JSON для автоматизации:

```bash
curl -sS "${ANTHROPIC_BASE_URL%/}/v1/bravo/limits?format=json" \
  -H "Authorization: Bearer ${ANTHROPIC_AUTH_TOKEN}"
```

Ответ содержит только текущий проект: доступность провайдеров, подтверждённые
окна и время сброса, а в JSON — usage за последние 30 дней с дневным рядом и
разбивкой по provider/model. Свежий результат разрешён один раз в час на
проект; повторный запрос получает `429` и время следующего обновления.

## Прозрачность маршрутов

Начиная с Bravo 0.8.11 текущую карту разрешённых проекту моделей можно получить
без management key:

```bash
curl -sS "${ANTHROPIC_BASE_URL%/}/v1/bravo/routes" \
  -H "Authorization: Bearer ${ANTHROPIC_AUTH_TOKEN}"
```

Результат показывает:

- доступные этому проекту `bravo/*` имена;
- preferred и fallback кандидатов по порядку;
- provider и физическую модель;
- effort и подтверждённые capabilities;
- политику allowed pool и момент, до которого допустим fallback.

Ручка read-only и не показывает OAuth identities, ключи или маршруты чужих
проектов.

## Как выдать доступ коллеге

Не передавайте свой личный проектный ключ. Для каждого сотрудника или продукта:

1. создайте отдельный проект во встроенной админке CLIProxyAPI;
2. задайте его allowed subscription pool и model allowlist;
3. безопасно передайте сотруднику base URL и показанный один раз `brv_...`;
4. отправьте ему этот документ и предложите проверить `/v1/bravo/routes`;
5. предложите проверить лимиты через прямой shell mode `!curl ...`, а не через
   model-based skill;
6. при увольнении или компрометации перевыпустите/отключите только его ключ.

Так сотрудник самостоятельно видит, на каких логических моделях строить CLI,
агентов и cron-задачи, но не получает management-доступ и не видит чужие пулы.

Тот же ключ подходит не только Claude Code. Его можно выдать небольшому
внутреннему проекту компании, сервису автоматизации или агентному framework,
который принимает OpenAI- либо Anthropic-совместимые base URL и key. Это
позволяет команде отлаживать проект на разрешённых подписках, не переписывая
его сразу под прямой платный API. Agent SDK при этом не обязателен и не
запрещён: Bravo заменяет транспорт и маршрутную policy, а не прикладную
агентную логику.

Ключ нельзя хранить в общем чате, issue, README проекта или shell history.
Используйте менеджер секретов либо защищённый канал передачи.

## Важные ограничения

- Контекст 1M в настройке описывает окно клиента, но не увеличивает окно
  физического fallback-кандидата.
- Agent teams экспериментальны и заметно повышают параллельный расход.
- `max` может расходовать квоту значительно быстрее `xhigh`/`high`.
- `/compact` остаётся полезным перед передачей очень длинной сессии модели с
  меньшим окном.
- `/v1/bravo/limits` и `/v1/bravo/routes` доступны начиная со stable 0.8.11.

Актуальные переменные Claude Code описаны в официальных разделах
[Environment variables](https://code.claude.com/docs/en/env-vars),
[Model configuration](https://code.claude.com/docs/en/model-config) и
[Subagents](https://code.claude.com/docs/en/sub-agents).
