---
name: Ошибка Bravo
about: Сообщить о воспроизводимой ошибке project key, маршрута, provider fallback или API
title: "[Bravo] "
labels: bug
assignees: ''
---

## Версия

- Bravo version:
- Commit/image digest:
- Клиент и версия:
- Протокол: Anthropic Messages / OpenAI Chat / Responses / Images

## Запрос и маршрут

- Логическая модель `bravo/...`:
- Проект (без plaintext-ключа):
- Время с часовым поясом:
- HTTP status и стабильный `code`:
- Безопасный trace ID:

## Что произошло

Опишите фактическое и ожидаемое поведение. Если проблема плавающая, укажите
число успехов/ошибок и интервалы времени.

## Минимальное воспроизведение

Приложите минимальный запрос или команды, предварительно удалив секреты и
приватное содержимое.

## Безопасность данных

- [ ] В отчёте нет `brv_...`, management key, OAuth/provider credentials,
      cookies и содержимого `auths/`.
- [ ] Raw provider JSON и headers санитированы.
- [ ] Я указал, воспроизводится ли проблема на Bravo stable или только на
      preview-ветке.

Полные требования: [CONTRIBUTING.md](../../CONTRIBUTING.md).
