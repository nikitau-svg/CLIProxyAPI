package main

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

func clientExecutionFailureRU(failure executionFailure) executionFailure {
	failure = sanitizeExecutionFailure(failure)
	if containsCyrillic(failure.Message) {
		return failure
	}
	model := executionFailureDisplayModel(failure)
	switch strings.ToLower(strings.TrimSpace(failure.Code)) {
	case "bravo_disabled":
		failure.Message = "Режим Bravo отключён в конфигурации сервиса. Включите его или выберите модель вне Bravo."
	case "bravo_model_unknown":
		failure.Message = "Запрошенная логическая модель Bravo не найдена. Проверьте имя модели и настройки маршрута."
	case "bravo_smart_key_required":
		failure.Message = "Для этой модели нужен действующий ключ проекта Bravo. Проверьте ключ авторизации."
	case "bravo_model_forbidden":
		failure.Message = "Этот ключ проекта не разрешает использовать выбранную модель. Измените список моделей проекта или выберите разрешённую модель."
	case "bravo_no_eligible_account":
		failure.Message = "Для запроса не осталось доступной подписки: проверьте пул проекта, внутренние резервные пороги, cooldown и лимиты провайдеров."
	case "bravo_allocator_withheld":
		failure.Message = "Внутренний распределитель CLIProxyAPI не выпустил подписку. Проверьте включение подписки, состояние квоты и резервные пороги."
	case "bravo_allocator_reserve_floor":
		failure.Message = "Подписка доступна у провайдера, но временно удерживается внутренним резервным порогом CLIProxyAPI."
	case "bravo_compact_bypass_cooldown":
		if strings.TrimSpace(failure.Message) == "" {
			failure.Message = "Повторный доступ /compact к резерву Claude временно закрыт внутренним cooldown. Подождите время из Retry-After."
		}
	case "bravo_subscription_quota_exhausted", "rate_limit_error", "rate_limited":
		failure.Message = modelMessageRU(model, "достигла лимита провайдера. Дождитесь сброса лимита или разрешите другую подписку в проекте")
	case "bravo_subscription_model_credits_exhausted":
		failure.Message = modelMessageRU(model, "достигла отдельного лимита расходов у провайдера. Увеличьте лимит модели или выберите другой маршрут")
	case "bravo_subscription_auth_unavailable", "authentication_error":
		failure.Message = "Авторизация выбранной подписки недоступна. Переподключите аккаунт или разрешите другую подписку в проекте."
	case "bravo_subscription_access_denied", "permission_error":
		failure.Message = "Провайдер запретил доступ выбранной подписке. Проверьте права workspace и доступ к модели."
	case "bravo_subscription_model_unavailable":
		failure.Message = modelMessageRU(model, "недоступна на выбранной подписке. Проверьте тариф и доступ workspace либо измените маршрут")
	case "bravo_context_window_exceeded":
		if failure.Provider != nil && failure.Provider.RequiredTokens > 0 && failure.Provider.LimitTokens > 0 {
			failure.Message = fmt.Sprintf(
				"Контекст содержит %s токенов и превышает лимит выбранной модели %s токенов. Выполните /compact или начните новую сессию.",
				formatContextTokenCount(failure.Provider.RequiredTokens),
				formatContextTokenCount(failure.Provider.LimitTokens),
			)
		} else {
			failure.Message = modelMessageRU(model, "не может вместить весь контекст переписки. Выполните /compact или начните новую сессию")
		}
	case "bravo_route_temporarily_unavailable", "overloaded_error":
		failure.Message = "Все подходящие маршруты временно недоступны или перегружены. Подождите время из Retry-After и повторите запрос."
	case "bravo_contract_unavailable", "bravo_contract_rejected", "bravo_contract_unverified", "bravo_capability_conflict", "bravo_capability_undeclared", "bravo_effort_unavailable":
		failure.Message = "Ни один маршрут Bravo не может безопасно сохранить контракт этого запроса. Упростите параметры, инструменты или режим reasoning/effort."
	case "bravo_provider_invalid_request", "invalid_request_error", "invalid_tool_parameters", "invalid_function_parameters":
		if failure.Provider != nil && strings.TrimSpace(failure.Provider.Parameter) != "" {
			failure.Message = fmt.Sprintf(
				"Провайдер отклонил параметр %s. Проверьте JSON-схему инструмента и переданные аргументы.",
				failure.Provider.Parameter,
			)
		} else {
			failure.Message = "Провайдер отклонил параметры запроса или инструмента. Проверьте JSON-схему и аргументы tool-вызова."
		}
	case "bravo_request_invalid":
		failure.Message = "Bravo не смог разобрать или преобразовать запрос. Проверьте формат запроса и параметры модели."
	case "request_canceled":
		failure.Message = "Запрос отменён клиентом до завершения ответа."
	case "bravo_provider_stream_error":
		failure.Message = "Провайдер завершил поток структурированной ошибкой, которую Bravo пока не смог однозначно классифицировать. Повторите запрос; при повторении откройте путь ошибки в аналитике."
	case "bravo_host_call_failed", "host_call_failed", "bravo_host_response_invalid", "bravo_host_stream_invalid", "bravo_host_stream_missing", "bravo_host_stream_chunk_invalid":
		failure.Message = "Внутренний мост CLIProxyAPI не смог корректно выполнить запрос к провайдеру. Повторите запрос; при повторении проверьте путь ошибки в аналитике."
	case "billing_error":
		failure.Message = "Провайдер отклонил запрос из-за биллинга или лимита расходов подписки. Проверьте тариф и ограничения workspace."
	case "server_error":
		failure.Message = "Провайдер вернул внутреннюю ошибку. Подождите и повторите запрос; Bravo сохранит безопасный cooldown."
	default:
		failure.Message = genericStatusMessageRU(failure.Status, failure.Code)
	}
	return failure
}

func clientProjectFailureRU(failure projectFailure) projectFailure {
	if containsCyrillic(failure.Message) {
		return failure
	}
	switch strings.ToLower(strings.TrimSpace(failure.Code)) {
	case "bravo_project_body_required", "bravo_route_body_required", "bravo_allocator_body_required":
		failure.Message = "Нужно передать тело запроса в формате JSON."
	case "bravo_project_body_too_large", "bravo_route_body_too_large", "bravo_allocator_body_too_large":
		failure.Message = "Тело запроса слишком большое. Уменьшите объём данных и повторите запрос."
	case "bravo_project_body_invalid", "bravo_route_body_invalid", "bravo_allocator_body_invalid":
		failure.Message = "Тело запроса должно быть одним корректным JSON-объектом только с поддерживаемыми полями."
	case "bravo_project_name_exists":
		failure.Message = "Проект Bravo с таким названием уже существует. Выберите другое название."
	case "bravo_project_name_required":
		failure.Message = "Укажите название проекта Bravo."
	case "bravo_project_name_too_long":
		failure.Message = "Название проекта Bravo не должно быть длиннее 120 символов."
	case "bravo_project_name_invalid":
		failure.Message = "Название проекта Bravo содержит недопустимые управляющие или невидимые символы."
	case "bravo_project_model_unknown":
		failure.Message = "В проекте указана неизвестная логическая модель Bravo. Проверьте список моделей и маршруты."
	case "bravo_project_models_required":
		failure.Message = "Разрешите проекту хотя бы одну модель либо выберите все модели символом *."
	case "bravo_project_not_found":
		failure.Message = "Проект Bravo не найден. Обновите список проектов и проверьте его идентификатор."
	case "bravo_project_revoked":
		failure.Message = "Отозванный проект нельзя включить или изменить. Создайте новый проект либо поверните ключ активного проекта."
	case "bravo_project_state_conflict", "bravo_project_state_invalid", "bravo_project_status_invalid":
		failure.Message = "Состояние проекта противоречиво или недопустимо. Используйте active, disabled или revoked с согласованным флагом enabled."
	case "bravo_project_policy_invalid":
		failure.Message = "Политика проекта некорректна. Проверьте ограничения моделей, подписок и параметры кэширования."
	case "bravo_project_prompt_cache_invalid":
		failure.Message = "Настройка кэширования промптов некорректна. Для Anthropic используйте auto, 5m или 1h."
	case "bravo_project_id_generation_failed", "bravo_project_key_generation_failed":
		failure.Message = "Bravo не смог безопасно создать идентификатор или ключ проекта. Повторите операцию."
	case "bravo_project_persistence_failed":
		failure.Message = "CLIProxyAPI не смог сохранить проект Bravo. Повторите операцию; при повторении проверьте журнал сервера."
	case "bravo_project_runtime_install_failed":
		failure.Message = "Проект сохранён, но не установлен в работающую конфигурацию Bravo. Проверьте журнал сервера перед повторной попыткой."
	case "bravo_route_invalid":
		failure.Message = "Маршрут Bravo некорректен. Проверьте провайдеров, модели, приоритеты и контракт возможностей."
	case "bravo_subscription_auth_index_required":
		failure.Message = "Укажите auth_index подписки."
	case "bravo_subscription_not_found":
		failure.Message = "Подписка с таким auth_index не найдена или уже была удалена. Обновите список подписок."
	case "bravo_subscription_tariff_unknown", "bravo_tariff_not_found":
		failure.Message = "Указан неизвестный тариф подписки. Выберите существующий тариф или создайте его."
	case "bravo_tariff_invalid":
		failure.Message = "Настройки тарифа некорректны. Проверьте множитель, резервные пороги и лимиты."
	case "bravo_allowed_auth_validation_unavailable", "bravo_primary_auth_validation_unavailable":
		failure.Message = "CLIProxyAPI не смог проверить пул подписок проекта. Изменения не применены; повторите запрос позже."
	case "bravo_allowed_auth_not_found":
		failure.Message = "Одна из разрешённых подписок не существует. Пул проекта закрыт до исправления auth_index."
	case "bravo_primary_auth_not_found":
		failure.Message = "Основная подписка проекта не существует. Проверьте auth_index; Bravo не сопоставляет аккаунты по email."
	case "bravo_primary_auth_outside_allowed_pool":
		failure.Message = "Основная подписка проекта не входит в разрешённый пул. Добавьте её в allowed_auth_ids или выберите другую."
	case "bravo_primary_auth_conflict":
		failure.Message = "Эта основная подписка уже закреплена за другим активным проектом. Выберите другую подписку."
	default:
		failure.Message = genericStatusMessageRU(failure.Status, failure.Code)
	}
	return failure
}

func executionFailureDisplayModel(failure executionFailure) string {
	if failure.Provider != nil {
		for _, value := range []string{failure.Provider.ModelDisplayName, failure.Provider.Model} {
			if strings.TrimSpace(value) != "" {
				return friendlyModelName(value)
			}
		}
	}
	if before, _, found := strings.Cut(strings.TrimSpace(failure.Message), " — "); found {
		return friendlyModelName(before)
	}
	return ""
}

func friendlyModelName(model string) string {
	model = strings.TrimSpace(model)
	switch strings.ToLower(model) {
	case "claude-fable-5":
		return "Fable 5"
	case "claude-opus-5":
		return "Opus 5"
	case "claude-sonnet-5":
		return "Sonnet 5"
	case "gpt-5.6-sol":
		return "Sol (gpt-5.6-sol)"
	case "gpt-5.6-terra":
		return "Terra (gpt-5.6-terra)"
	case "gpt-5.6-luna":
		return "Luna (gpt-5.6-luna)"
	default:
		return model
	}
}

func modelMessageRU(model, suffix string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "Выбранная модель " + suffix + "."
	}
	return fmt.Sprintf("Модель %s %s.", model, strings.TrimSuffix(strings.TrimSpace(suffix), "."))
}

func genericStatusMessageRU(status int, code string) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "Запрос отклонён как несовместимый или некорректный. Проверьте параметры и контракт модели."
	case http.StatusUnauthorized:
		return "Авторизация не прошла. Проверьте ключ проекта и подключение подписки."
	case http.StatusForbidden:
		return "Доступ к запросу запрещён настройками проекта или провайдера."
	case http.StatusTooManyRequests:
		return "Достигнут лимит или действует cooldown. Дождитесь времени из Retry-After либо выберите другой доступный маршрут."
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return "Маршрут временно недоступен. Повторите запрос позже; при повторении проверьте путь ошибки в аналитике."
	default:
		if strings.TrimSpace(code) == "" {
			return "Bravo не смог выполнить запрос. Проверьте путь ошибки в аналитике."
		}
		return fmt.Sprintf("Bravo не смог выполнить запрос. Диагностический код: %s.", strings.TrimSpace(code))
	}
}

func containsCyrillic(value string) bool {
	for _, char := range value {
		if unicode.In(char, unicode.Cyrillic) {
			return true
		}
	}
	return false
}
