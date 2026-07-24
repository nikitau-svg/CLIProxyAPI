#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "minitest/autorun"
require "stringio"
require "tmpdir"

require_relative "bravo-quota-allocator-smoke"

class BravoQuotaAllocatorSmokeTest < Minitest::Test
  Smoke = BravoQuotaAllocatorSmoke

  def setup
    @now = Time.utc(2026, 7, 23, 12, 0, 0)
    @validator = Smoke::Validator.new(lambda { @now }, ["management-secret-value"])
  end

  def test_accepts_four_unique_subscriptions_and_distinct_same_email_workspaces
    subscriptions, indexes =
      @validator.validate_subscription_root!(subscription_root)

    assert_equal 4, subscriptions.length
    assert_equal %w[index-1 index-2 index-3 index-4], indexes
  end

  def test_rejects_duplicate_or_empty_auth_indexes
    root = deep_copy(subscription_root)
    root["subscriptions"][3]["auth_index"] = "index-1"

    error = assert_raises(Smoke::Failure) do
      @validator.validate_subscription_root!(root)
    end
    assert_equal "auth_indexes_not_unique", error.code

    root = deep_copy(subscription_root)
    root["subscriptions"][0]["auth_index"] = " "
    error = assert_raises(Smoke::Failure) do
      @validator.validate_subscription_root!(root)
    end
    assert_equal "auth_index_missing", error.code
  end

  def test_unknown_percentages_must_be_explicit_nulls
    root = deep_copy(subscription_root)
    quota = root["subscriptions"][0]["quota"]
    quota["confidence"] = "unknown"
    quota["observed_at"] = nil
    quota["session"]["used_percent"] = nil
    quota["session"]["remaining_percent"] = nil
    quota["session"].delete("reset_at")
    quota["weekly"]["used_percent"] = nil
    quota["weekly"]["remaining_percent"] = nil
    quota["weekly"].delete("reset_at")

    @validator.validate_subscription_root!(root)

    root["subscriptions"][0]["quota"]["session"]["used_percent"] = 0
    error = assert_raises(Smoke::Failure) do
      @validator.validate_subscription_root!(root)
    end
    assert_equal "unknown_quota_percentage_not_null", error.code
  end

  def test_confirmed_inactive_and_not_applicable_windows_require_zero_and_null_reset
    root = deep_copy(subscription_root)
    session = root["subscriptions"][0]["quota"]["session"]
    session["used_percent"] = 0
    session["remaining_percent"] = 100
    session["reset_at"] = nil
    session["reset_mode"] = "inactive"
    weekly = root["subscriptions"][2]["quota"]["weekly"]
    weekly["used_percent"] = 0
    weekly["remaining_percent"] = 100
    weekly["reset_at"] = nil
    weekly["reset_mode"] = "not_applicable"

    @validator.validate_subscription_root!(root)

    session["remaining_percent"] = 99
    error = assert_raises(Smoke::Failure) do
      @validator.validate_subscription_root!(root)
    end
    assert_equal "quota_percentages_inconsistent", error.code

    session["remaining_percent"] = 100
    session["reset_at"] = (@now + 60).iso8601
    error = assert_raises(Smoke::Failure) do
      @validator.validate_subscription_root!(root)
    end
    assert_equal "quota_resetless_window_invalid", error.code
  end

  def test_rejects_credential_path_and_raw_provider_fields
    {
      "access_token" => "oauth-secret",
      "path" => "/Users/example/.credentials/account.json",
      "provider_json" => { "anything" => "value" }
    }.each do |field, value|
      root = deep_copy(subscription_root)
      root["subscriptions"][0][field] = value
      error = assert_raises(Smoke::Failure) do
        @validator.validate_subscription_root!(root)
      end
      assert_equal "forbidden_response_field", error.code
    end
  end

  def test_allows_usage_token_counters_but_rejects_raw_json_strings
    @validator.validate_subscription_root!(subscription_root)

    root = deep_copy(subscription_root)
    root["subscriptions"][0]["quota"]["error"] = '{"provider":"raw"}'
    error = assert_raises(Smoke::Failure) do
      @validator.validate_subscription_root!(root)
    end
    assert_equal "raw_json_value_leaked", error.code
  end

  def test_rejects_non_numeric_percentages_and_implausible_resets
    root = deep_copy(subscription_root)
    root["subscriptions"][0]["quota"]["session"]["used_percent"] = "25"
    error = assert_raises(Smoke::Failure) do
      @validator.validate_subscription_root!(root)
    end
    assert_equal "quota_percentage_invalid", error.code

    root = deep_copy(subscription_root)
    root["subscriptions"][0]["quota"]["weekly"]["reset_at"] =
      (@now + 50 * 24 * 60 * 60).iso8601
    error = assert_raises(Smoke::Failure) do
      @validator.validate_subscription_root!(root)
    end
    assert_equal "quota_reset_implausible", error.code
  end

  def test_rejects_same_email_workspace_collision
    root = deep_copy(subscription_root)
    root["subscriptions"][1]["workspace"] =
      root["subscriptions"][0]["workspace"]

    error = assert_raises(Smoke::Failure) do
      @validator.validate_subscription_root!(root)
    end
    assert_equal "same_email_workspace_collision", error.code
  end

  def test_production_port_requires_separate_explicit_opt_in
    error = assert_raises(Smoke::Failure) do
      Smoke::TargetPolicy.validate!(
        "http://203.0.113.10:18317",
        true,
        false
      )
    end
    assert_equal "production_port_requires_explicit_opt_in", error.code

    uri = Smoke::TargetPolicy.validate!(
      "http://203.0.113.10:18317",
      false,
      true
    )
    assert_equal 18_317, uri.port
  end

  def test_management_secret_requires_exact_mode_0600
    Dir.mktmpdir do |dir|
      path = File.join(dir, "management.key")
      File.open(path, File::WRONLY | File::CREAT | File::TRUNC, 0o600) do |file|
        file.write("management-secret-value\n")
      end
      File.chmod(0o600, path)
      assert_equal(
        "management-secret-value",
        Smoke::SecretReader.read_key(path)
      )

      File.chmod(0o640, path)
      error = assert_raises(Smoke::Failure) do
        Smoke::SecretReader.read_key(path)
      end
      assert_equal "management_key_file_permissions", error.code
    end
  end

  def test_runner_uses_only_get_and_allowlisted_refresh_and_prints_no_identity
    Dir.mktmpdir do |dir|
      key_path = File.join(dir, "management.key")
      File.open(key_path, File::WRONLY | File::CREAT | File::TRUNC, 0o600) do |file|
        file.write("management-secret-value\n")
      end
      File.chmod(0o600, key_path)

      initial = subscription_root
      refreshed = deep_copy(subscription_root)
      refreshed["refreshed_auth_indexes"] =
        %w[index-1 index-2 index-3 index-4]
      calls = []
      transport = lambda do |method, path, payload, headers|
        calls << [method, path, payload, headers]
        root = method == :get ? initial : refreshed
        Smoke::HTTPResponse.new(200, JSON.generate(root))
      end
      runner = Smoke::Runner.new(
        {
          base_url: "http://127.0.0.1:18319",
          management_key_file: key_path,
          management_env_file: "unused",
          management_env_variable: "MANAGEMENT_PASSWORD",
          timeout: 10,
          allow_other_target: false,
          allow_production_quota_refresh: false
        },
        transport,
        lambda { @now }
      )

      output = capture_stdout { assert_equal 0, runner.run }
      assert_equal 2, calls.length
      assert_equal(
        [
          [:get, "/v0/management/bravo/subscriptions"],
          [:post, "/v0/management/bravo/quotas/refresh"]
        ],
        calls.map { |call| call[0, 2] }
      )
      assert_nil calls[0][2]
      assert_equal(
        { "auth_indexes" => %w[index-1 index-2 index-3 index-4] },
        calls[1][2]
      )
      calls.each do |call|
        assert_equal "management-secret-value", call[3]["X-Management-Key"]
      end

      %w[
        management-secret-value
        index-1
        index-2
        index-3
        index-4
        shared@example.com
        workspace-a
        workspace-b
      ].each do |secret_or_identity|
        refute_includes output.downcase, secret_or_identity.downcase
      end
      assert_includes output, "provider=claude"
      assert_includes output, "tariff=x5"
      assert_includes output, "confidence=confirmed"
      assert_includes output, "session_reset_mode=scheduled"
      assert_includes output, "weekly_reset_mode=scheduled"
      assert_includes output, "quota_allocator_smoke=ok subscriptions=4 refreshed=4"
    end
  end

  private

  def subscription_root
    {
      "subscriptions" => [
        subscription("index-1", "claude", "shared@example.com", "workspace-a", "x5"),
        subscription("index-2", "claude", "shared@example.com", "workspace-b", "x5"),
        subscription("index-3", "codex", "third@example.com", "", "x1"),
        subscription("index-4", "codex", "fourth@example.com", "", "x1")
      ],
      "tariffs" => [
        {
          "id" => "x1",
          "session_floor_percent" => 50,
          "weekly_floor_percent" => 50
        },
        {
          "id" => "x5",
          "session_floor_percent" => 30,
          "weekly_floor_percent" => 30
        }
      ]
    }
  end

  def subscription(auth_index, provider, email, workspace, tariff)
    {
      "auth_index" => auth_index,
      "auth_id" => "#{provider}-account.json",
      "provider" => provider,
      "label" => email,
      "email" => email,
      "workspace" => workspace,
      "plan" => tariff == "x5" ? "team" : "pro",
      "tariff" => "auto",
      "effective_tariff" => tariff,
      "enabled" => true,
      "health" => "ready",
      "primary_project_ids" => [],
      "quota" => {
        "confidence" => "confirmed",
        "observed_at" => @now.iso8601,
        "error" => "",
        "session" => {
          "used_percent" => 20.0,
          "remaining_percent" => 80.0,
          "reset_at" => (@now + 4 * 60 * 60).iso8601,
          "reset_mode" => "scheduled",
          "eligible" => true,
          "reason" => "ready"
        },
        "weekly" => {
          "used_percent" => 35.0,
          "remaining_percent" => 65.0,
          "reset_at" => (@now + 6 * 24 * 60 * 60).iso8601,
          "reset_mode" => "scheduled",
          "eligible" => true,
          "reason" => "ready"
        },
        "model_weekly" => []
      },
      "usage" => {
        "total" => {
          "requests" => 3,
          "input_tokens" => 100,
          "output_tokens" => 25,
          "total_tokens" => 125
        },
        "session" => {
          "requests" => 1,
          "input_tokens" => 40,
          "output_tokens" => 10,
          "total_tokens" => 50
        },
        "weekly" => {
          "requests" => 3,
          "input_tokens" => 100,
          "output_tokens" => 25,
          "total_tokens" => 125
        },
        "average_latency_ms" => 250.0
      }
    }
  end

  def deep_copy(value)
    Marshal.load(Marshal.dump(value))
  end

  def capture_stdout
    previous = $stdout
    output = StringIO.new
    $stdout = output
    yield
    output.string
  ensure
    $stdout = previous
  end
end
