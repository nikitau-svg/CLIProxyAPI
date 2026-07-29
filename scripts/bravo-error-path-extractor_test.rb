#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "minitest/autorun"
require "tmpdir"

require_relative "bravo-error-path-extractor"

class BravoErrorPathExtractorTest < Minitest::Test
  Extractor = BravoErrorPathExtractor
  FIXTURE_DIRECTORY = File.expand_path("testdata/bravo-error-path", __dir__)

  def test_zero_attempt_planning_failure_is_not_reported_as_provider_execution
    report = Extractor::Parser.new(
      fixture("c914-zero-attempt.log"),
      filename: "error-v1-messages-2026-07-30T002424-c9145b66.log",
      project: "CLI_Nikita_Uskov"
    ).parse

    assert_equal(
      {
        "name" => "CLI_Nikita_Uskov",
        "source" => "operator_assertion",
        "confidence" => "asserted"
      },
      report.fetch("project")
    )
    assert_equal "c9145b66", report.dig("correlation", "request_id", "value")
    assert_equal "filename_suffix", report.dig("correlation", "request_id", "source")
    assert_equal "session-safe-123", report.dig("correlation", "session_id", "value")
    assert_equal "2026-07-30T00:24:24.294878752+08:00", report.dig("request", "received_at")
    assert_equal "anthropic_messages", report.dig("request", "protocol")
    assert_equal "bravo/gpt-5.6-sol", report.dig("request", "logical_model")
    assert_equal true, report.dig("request", "stream")
    assert_equal "max", report.dig("request", "effort")
    assert_equal 2, report.dig("request", "body_metrics", "message_count")
    assert_equal 1, report.dig("request", "body_metrics", "tool_count")
    assert_empty report.fetch("attempts")

    rejections = report.fetch("planning_rejections")
    assert_equal 2, rejections.length
    assert_equal(
      {
        "provider" => "codex",
        "model" => "gpt-5.6-sol",
        "eligibility_code" => "bravo_no_eligible_account",
        "credential_count" => 1,
        "reasons" => { "cooldown" => 1 },
        "executed" => false
      },
      rejections.first
    )
    assert_equal({ "cooldown" => 2, "quota_exhausted" => 1 }, rejections.last.fetch("reasons"))
    assert_equal 503, report.dig("gateway_response", "status")
    assert_equal "30", report.dig("gateway_response", "retry_after")
    assert_equal "bravo_no_eligible_account", report.dig("gateway_response", "json", "error", "code")
    assert_includes report.fetch("missing_fields"), "attempts"
    assert_includes report.fetch("missing_fields"), "attempt_provider_responses"

    serialized = JSON.generate(report)
    refute_includes serialized, "never print this prompt"
    refute_includes serialized, "brv_live_secret"

    readable = Extractor::TextRenderer.new.render([report])
    assert_includes readable, "status=503 | Retry-After=30"
  end

  def test_two_numbered_requests_are_ordered_and_missing_responses_are_explicit
    report = Extractor::Parser.new(
      fixture("two-attempts-no-responses.log"),
      filename: "error-two-abcdef12.log"
    ).parse
    attempts = report.fetch("attempts")

    assert_equal 2, attempts.length
    assert_equal [1, 2], attempts.map { |attempt| attempt.fetch("sequence") }
    assert_equal %w[first_request retry], attempts.map { |attempt| attempt.fetch("role") }
    assert_equal %w[codex claude], attempts.map { |attempt| attempt.fetch("provider") }
    assert_equal ["gpt-5.6-sol", "claude-fable-5"], attempts.map { |attempt| attempt.fetch("model") }
    assert_equal ["Codex primary", "Claude fallback"], attempts.map { |attempt| attempt.fetch("auth_label") }
    assert attempts.all? { |attempt| attempt.fetch("executed") }
    assert attempts.all? { |attempt| attempt.dig("provider_response", "present") == false }
    assert attempts.all? do |attempt|
      attempt.dig("provider_response", "missing_reason") == "api_response_section_not_found"
    end
    assert_equal(
      ["attempts[0].provider_response", "attempts[1].provider_response"],
      report.fetch("missing_fields").grep(/attempts\[/)
    )
  end

  def test_distinct_attempt_response_keeps_sanitized_full_error_json
    report = Extractor::Parser.new(
      fixture("redaction-provider-response.log"),
      filename: "error-redaction-00112233.log"
    ).parse
    response = report.dig("attempts", 0, "provider_response")

    assert_equal true, response.fetch("present")
    assert_equal 429, response.fetch("status")
    assert_equal "rate_limit_error", response.dig("json", "error", "type")
    assert_equal "[REDACTED]", response.dig("json", "error", "access_token")
    assert_equal "[REDACTED]", response.dig("json", "authorization")
    assert_includes response.dig("json", "error", "message"), "[REDACTED]"

    serialized = JSON.generate(report)
    refute_includes serialized, "prompt-body-must-never-appear"
    refute_includes serialized, "oauth-super-secret"
    refute_includes serialized, "sk-proj-supersecret"
    refute_includes serialized, "brv_supersecret"
    refute_includes serialized, "plainsecretvalue"

    readable = Extractor::TextRenderer.new.render([report])
    assert_includes readable, "Первый запрос: claude / claude-fable-5 / Личный аккаунт"
    assert_includes readable, "Ошибка провайдера (полный безопасно очищенный JSON):"
    refute_includes readable, "oauth-super-secret"
  end

  def test_finder_uses_latest_mtime_and_parser_survives_malformed_input
    Dir.mktmpdir("bravo-error-path") do |directory|
      old_path = File.join(directory, "error-old-00000001.log")
      tied_a = File.join(directory, "error-a-00000002.log")
      tied_b = File.join(directory, "error-b-00000003.log")
      ignored = File.join(directory, "request-not-an-error.log")
      File.write(old_path, "broken")
      File.write(tied_a, "=== REQUEST INFO ===\nTimestamp: not-a-time\n")
      File.write(tied_b, "=== REQUEST BODY ===\n{malformed-json\n")
      File.write(ignored, "ignored")
      File.utime(Time.at(100), Time.at(100), old_path)
      File.utime(Time.at(200), Time.at(200), tied_a)
      File.utime(Time.at(200), Time.at(200), tied_b)

      selected = Extractor::Finder.new([directory], latest: 2).paths
      assert_equal [tied_a, tied_b], selected

      report = Extractor::Parser.new(File.binread(tied_b), filename: File.basename(tied_b)).parse
      assert_equal false, report.dig("request", "body_metrics", "json_valid")
      assert_includes report.fetch("warnings"), "request_body_invalid_json"
      assert_includes report.fetch("missing_fields"), "request.logical_model"
      assert_includes report.fetch("missing_fields"), "gateway_response"
    end
  end

  private

  def fixture(name)
    File.binread(File.join(FIXTURE_DIRECTORY, name))
  end
end
