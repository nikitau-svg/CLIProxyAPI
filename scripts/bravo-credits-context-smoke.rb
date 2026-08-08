#!/usr/bin/env ruby
# frozen_string_literal: true

# Destructive only inside the dedicated credits/context canary. The script
# creates four disposable Bravo projects, exercises the exact production error,
# verifies sibling-model isolation and cross-provider fallback through all three
# streaming HTTP contracts, then removes the projects. It never prints keys or
# provider request bodies.

require "json"
require "net/http"
require "open3"
require "optparse"
require "securerandom"
require "time"
require "uri"

class CreditsContextSmokeFailure < StandardError; end

class BravoCreditsContextSmoke
  FALLBACK_MARKER = "BRAVO_CREDITS_FALLBACK_OK"
  REVERSE_FALLBACK_MARKER = "BRAVO_CODEX_SERVER_ERROR"

  PRIVATE_FORBIDDEN = [
    ["req_bravo_credits_private", "provider request id"],
    ["has_chargeable_saved_payment_method", "payment-method capability"],
    ["can_user_purchase_credits", "credit-purchase capability"],
    ["exhausted_included_allowance", "included-allowance flag"],
    ["redirect_hint", "provider redirect hint"],
    ["session_key", "session credential"],
    ["client_secret", "client credential"],
    ["password=", "password material"]
  ].freeze

  def initialize(options)
    @base = URI(options.fetch(:base_url))
    @provider = URI(options.fetch(:provider_url))
    @control_token = options.fetch(:control_token)
    @docker_path = options.fetch(:docker_path)
    @container_name = options.fetch(:container_name)
    @management_env_file = File.expand_path(options.fetch(:management_env_file))
    @state_file = File.expand_path(options.fetch(:state_file))
    @management_key = read_secret(
      @management_env_file,
      options.fetch(:management_env_variable)
    )
    @projects = []
    @project_keys = []
    @passed = 0
    @failed = 0
    validate_targets!
  end

  def run
    begin
      reset_canary_state
      reset_provider
      cleanup_stale_canary_projects
      claude_context, claude_messages, claude_chat, claude_responses, codex = isolated_subscriptions
      context_project = create_project("credits-context", claude_context, codex)
      context_project = rotate_project(context_project)

      check("HTTP credits_required keeps the Fable reason beside the terminal context error") do
        started = Time.now.utc - 1
        status, body = anthropic_request(
          context_project.fetch(:key),
          "bravo/fable",
          "BRAVO_CONTEXT_OVERFLOW",
          stream: false
        )
        raise CreditsContextSmokeFailure, "context scenario unexpectedly succeeded" if status.between?(200, 299)
        assert_includes(body, "Fable 5")
        assert_includes(body, "лимит расходов")
        assert_includes(body, "контекст")
        assert_includes(body, "bravo_context_window_exceeded")
        assert_redacted(body)

        attempts = recent_attempts(started)
        assert_attempt_chain(
          attempts,
          claude_context,
          codex,
          first_code: "bravo_subscription_model_credits_exhausted",
          second_code: "bravo_context_window_exceeded"
        )
        assert_allocator_pending(codex, expected: 0.0, label: "Codex context rejection")
        assert_safe_management_surfaces
      end

      check("streamed context rejection before content creates no Codex pending debt") do
        provider_cursor = current_provider_sequence
        status, body = anthropic_request(
          context_project.fetch(:key),
          "bravo/fable",
          "BRAVO_CONTEXT_OVERFLOW",
          stream: true
        )
        raise CreditsContextSmokeFailure, "streamed context scenario unexpectedly succeeded" if status.between?(200, 299)
        assert_includes(body, "bravo_context_window_exceeded")
        assert_redacted(body)
        assert_allocator_pending(codex, expected: 0.0, label: "streamed Codex context rejection")

        observed = provider_events.select { |event| event["sequence"].to_i > provider_cursor }
        unless observed.length == 1 && observed.first["type"] == "codex_context_stream_error" &&
               observed.first["model"] == "gpt-5.6-sol" && observed.first["stream"] == true
          raise CreditsContextSmokeFailure,
                "streamed context provider calls #{observed.map { |event| event["type"] }.inspect}"
        end
      end

      check("persisted effort-qualified Fable limit survives a Bravo restart") do
        provider_cursor = current_provider_sequence
        restart_canary
        assert_allocator_pending(codex, expected: 0.0, label: "Codex context rejection after restart")
        started = Time.now.utc - 1
        status, body = anthropic_request(
          context_project.fetch(:key),
          "bravo/fable",
          "Reply with the canary marker.",
          stream: false
        )
        raise CreditsContextSmokeFailure, "restart fallback returned HTTP #{status}" unless status == 200
        assert_includes(body, FALLBACK_MARKER)
        assert_redacted(body)

        attempts = recent_attempts(started)
        repeated_claude = attempts.find do |event|
          event["auth_id"] == claude_context.fetch("auth_id") &&
            event["model"] == "claude-fable-5"
        end
        raise CreditsContextSmokeFailure, "restart retried the exhausted Claude model" if repeated_claude

        codex_success = attempts.find do |event|
          event["auth_id"] == codex.fetch("auth_id") &&
            event["model"] == "gpt-5.6-sol" &&
            event["success"] == true
        end
        raise CreditsContextSmokeFailure, "restart did not preserve the Codex fallback" unless codex_success

        post_restart_provider_events = provider_events.select do |event|
          event["sequence"].to_i > provider_cursor
        end
        unless post_restart_provider_events.map { |event| event["type"] } == ["codex_fallback_success"]
          raise CreditsContextSmokeFailure,
                "restart provider order #{post_restart_provider_events.map { |event| event["type"] }.inspect}"
        end
      end

      check("a Fable model limit does not poison the sibling Sonnet model") do
        started = Time.now.utc - 1
        status, body = anthropic_request(
          context_project.fetch(:key),
          "bravo/claude-sonnet-5",
          "Reply with the canary marker.",
          stream: false
        )
        raise CreditsContextSmokeFailure, "sibling request returned HTTP #{status}" unless status == 200
        assert_includes(body, "BRAVO_CLAUDE_SIBLING_OK")
        assert_redacted(body)

        sibling = recent_attempts(started).find do |event|
          event["auth_id"] == claude_context.fetch("auth_id") &&
            event["model"] == "claude-sonnet-5" &&
            event["success"] == true
        end
        raise CreditsContextSmokeFailure, "healthy sibling did not use the same Claude subscription" unless sibling
      end

      check("subscription UI data is model-scoped and leaves the account ready") do
        response = management_json(:get, "/v0/management/bravo/subscriptions")
        subscription = Array(response["subscriptions"]).find do |item|
          item["auth_index"] == claude_context.fetch("auth_index")
        end
        raise CreditsContextSmokeFailure, "context subscription disappeared" unless subscription
        unless subscription["health"] == "ready"
          raise CreditsContextSmokeFailure, "model issue poisoned account health: #{subscription["health"].inspect}"
        end
        issue = Array(subscription["model_issues"]).find do |item|
          item["provider_error_code"] == "credits_required" &&
            item["provider_model"] == "claude-fable-5" &&
            item["scope"] == "model"
        end
        raise CreditsContextSmokeFailure, "safe model-scoped issue is missing" unless issue
        raise CreditsContextSmokeFailure, "wrong model display name" unless issue["provider_model_display_name"] == "Fable 5"
        assert_includes(issue["provider_notice_title"].to_s.downcase, "monthly spend")
        unless issue["provider_disabled_reason"] == "org_level_disabled_until"
          raise CreditsContextSmokeFailure, "safe provider disable reason is missing"
        end
        assert_redacted(JSON.generate(subscription))
      end

      release_project(context_project)
      messages_project = create_project("credits-stream-messages", claude_messages, codex)

      check("/v1/messages hides the failed prelude and emits one logical Claude stream") do
        provider_cursor = current_provider_sequence
        started = Time.now.utc - 1
        status, body, content_type = anthropic_request(
          messages_project.fetch(:key),
          "bravo/fable",
          "Reply with the canary marker.",
          stream: true
        )
        raise CreditsContextSmokeFailure, "messages stream fallback returned HTTP #{status}" unless status == 200
        assert_event_stream_content_type(content_type)
        assert_successful_fallback_stream(body, marker_count: 1)
        frames = parse_stream_frames(body)
        assert_stream_event_count(frames, "message_start", 1)
        assert_stream_event_count(frames, "message_stop", 1)

        attempts = recent_attempts(started)
        assert_attempt_chain(
          attempts,
          claude_messages,
          codex,
          first_code: "bravo_subscription_model_credits_exhausted",
          second_success: true
        )
        assert_stream_provider_pair(provider_cursor)
      end

      release_project(messages_project)
      chat_project = create_project("credits-stream-chat", claude_chat, codex)

      check("/v1/chat/completions suppresses role-only prelude before Codex fallback") do
        provider_cursor = current_provider_sequence
        started = Time.now.utc - 1
        status, body, content_type = openai_chat_request(
          chat_project.fetch(:key),
          "bravo/fable",
          "Reply with the canary marker."
        )
        raise CreditsContextSmokeFailure, "chat stream fallback returned HTTP #{status}" unless status == 200
        assert_event_stream_content_type(content_type)
        assert_successful_fallback_stream(body, marker_count: 1)
        frames = parse_stream_frames(body)
        assert_no_role_only_chat_prelude(frames)
        assert_chat_stream_shape(frames, body)

        attempts = recent_attempts(started)
        assert_attempt_chain(
          attempts,
          claude_chat,
          codex,
          first_code: "bravo_subscription_model_credits_exhausted",
          second_success: true
        )
        assert_stream_provider_pair(provider_cursor)
      end

      release_project(chat_project)
      responses_project = create_project("credits-stream-responses", claude_responses, codex)

      check("/v1/responses hides the failed prelude and emits one logical response.created") do
        provider_cursor = current_provider_sequence
        started = Time.now.utc - 1
        status, body, content_type = openai_responses_request(
          responses_project.fetch(:key),
          "bravo/fable",
          "Reply with the canary marker."
        )
        raise CreditsContextSmokeFailure, "responses stream fallback returned HTTP #{status}" unless status == 200
        assert_event_stream_content_type(content_type)
        assert_successful_fallback_stream(body)
        frames = parse_stream_frames(body)
        assert_stream_event_count(frames, "response.created", 1)
        assert_stream_event_count(frames, "response.completed", 1)
        assert_responses_stream_text(frames)

        attempts = recent_attempts(started)
        assert_attempt_chain(
          attempts,
          claude_responses,
          codex,
          first_code: "bravo_subscription_model_credits_exhausted",
          second_success: true
        )
        assert_stream_provider_pair(provider_cursor)
      end

      check("Codex server_error before content falls back to Claude without exposing its prelude") do
        provider_cursor = current_provider_sequence
        started = Time.now.utc - 1
        status, body, content_type = anthropic_request(
          responses_project.fetch(:key),
          "bravo/gpt-5.6-terra",
          REVERSE_FALLBACK_MARKER,
          stream: true
        )
        raise CreditsContextSmokeFailure, "reverse stream fallback returned HTTP #{status}" unless status == 200
        assert_event_stream_content_type(content_type)
        assert_successful_fallback_stream(
          body,
          marker_count: 1,
          logical_model: "bravo/gpt-5.6-terra"
        )
        frames = parse_stream_frames(body)
        assert_stream_event_count(frames, "message_start", 1)
        assert_stream_event_count(frames, "message_stop", 1)

        attempts = recent_attempts(started)
        assert_reverse_attempt_chain(attempts, codex, claude_responses)
        assert_reverse_stream_provider_pair(provider_cursor)
      end

      check("synthetic upstream observed the exact HTTP-matrix provider order") do
        types = provider_events.map { |event| event["type"] }
        expected = %w[
          claude_credits_http
          codex_context_stream_error
          codex_context_stream_error
          codex_fallback_success
          claude_sibling_success
          claude_credits_stream
          codex_fallback_success
          claude_credits_stream
          codex_fallback_success
          claude_credits_stream
          codex_fallback_success
          codex_server_error_stream
          claude_reverse_fallback_success
        ]
        unless types == expected
          raise CreditsContextSmokeFailure,
                "provider order #{types.inspect} does not match #{expected.inspect}"
        end
      end
    rescue StandardError => error
      record_failure("controlled canary setup", error.message)
    ensure
      cleanup_projects
      @management_key = nil
      @project_keys.fill(nil)
    end

    finish
  end

  private

  def check(name)
    yield
    @passed += 1
    puts "PASS  #{name}"
  rescue StandardError => error
    record_failure(name, error.message)
  end

  def finish
    puts
    puts "Bravo credits/context smoke: #{@passed} passed, #{@failed} failed"
    @failed.zero? ? 0 : 1
  end

  def record_failure(name, message)
    safe = message.to_s.dup
    ([@management_key] + @project_keys).compact.each do |secret|
      safe.gsub!(secret, "[REDACTED]") unless secret.empty?
    end
    safe.gsub!(/\bbrv_[A-Za-z0-9_-]{12,}\b/, "brv_[REDACTED]")
    PRIVATE_FORBIDDEN.each { |value, _label| safe.gsub!(value, "[REDACTED]") }
    @failed += 1
    puts "FAIL  #{name}: #{safe}"
  end

  def validate_targets!
    raise CreditsContextSmokeFailure, "production port 18317 is refused" if @base.port == 18_317
    raise CreditsContextSmokeFailure, "canary port must be 18319" unless @base.port == 18_319
    raise CreditsContextSmokeFailure, "provider port must be 18993" unless @provider.port == 18_993
    raise CreditsContextSmokeFailure, "only HTTP canary endpoints are accepted" unless [@base, @provider].all? do |uri|
      uri.scheme == "http"
    end
    unless @container_name.match?(/\ACLIProxyAPI-Bravo-[A-Za-z0-9_.-]+\z/) &&
        @container_name != "CLIProxyAPI-Prod"
      raise CreditsContextSmokeFailure, "restart target must be an explicit Bravo canary container"
    end
    unless @docker_path.start_with?("/") && File.file?(@docker_path) && File.executable?(@docker_path)
      raise CreditsContextSmokeFailure, "docker executable is unavailable"
    end
    state_parent = File.dirname(@state_file)
    canary_root = File.dirname(state_parent)
    unless File.basename(@state_file) == "bravo-state.json" &&
        File.basename(state_parent) == "bravo-data" &&
        File.basename(canary_root).match?(/\Acliproxyapi-credits-context-canary-v\d+\.[A-Za-z0-9]+\z/) &&
        File.directory?(state_parent) &&
        File.realpath(File.dirname(@management_env_file)) == File.realpath(canary_root)
      raise CreditsContextSmokeFailure, "state reset target must belong to the explicit credits/context canary"
    end
    raise CreditsContextSmokeFailure, "canary state file must not be a symlink" if File.symlink?(@state_file)
  end

  def reset_canary_state
    docker_container_action("stop")
    deletion_error = nil
    begin
      raise CreditsContextSmokeFailure, "canary state file disappeared before reset" unless File.file?(@state_file)

      File.delete(@state_file)
    rescue StandardError => error
      deletion_error = error
    ensure
      docker_container_action("start")
    end
    raise deletion_error if deletion_error

    wait_for_canary_management("state reset")
  end

  def restart_canary
    docker_container_action("restart")
    wait_for_canary_management("restart")
  end

  def docker_container_action(action)
    stdout, stderr, status = Open3.capture3(@docker_path, action, @container_name)
    unless status.success? && stdout.strip == @container_name
      detail = stderr.to_s.strip.slice(0, 300)
      raise CreditsContextSmokeFailure,
            "canary #{action} failed#{detail.empty? ? "" : ": #{detail}"}"
    end
  end

  def wait_for_canary_management(action)
    deadline = Time.now + 75
    loop do
      begin
        status_code, = request(
          :get,
          endpoint(@base, "/v0/management/bravo/projects"),
          key: @management_key,
          management: true
        )
        return if status_code == 200
      rescue StandardError
        # The listener and plugin initialize independently after container
        # restart. Retry only this isolated canary management endpoint.
      end
      break if Time.now >= deadline

      sleep 1
    end
    raise CreditsContextSmokeFailure, "canary management endpoint did not recover after #{action}"
  end

  def read_secret(path, variable)
    stat = File.stat(path)
    raise CreditsContextSmokeFailure, "management env file must be mode 0600" unless (stat.mode & 0o077).zero?

    matches = File.readlines(path, chomp: true).each_with_object([]) do |line, values|
      next if line.lstrip.start_with?("#")

      name, value = line.split("=", 2)
      next unless name&.strip == variable

      values << value.to_s.strip.sub(/\A(['"])(.*)\1\z/, '\2')
    end
    unless matches.length == 1 && !matches.first.empty?
      raise CreditsContextSmokeFailure, "management variable is missing, empty, or duplicated"
    end

    matches.first
  end

  def request(method, uri, key:, body: nil, management: false, expected: nil)
    request_class = {
      get: Net::HTTP::Get,
      post: Net::HTTP::Post,
      patch: Net::HTTP::Patch,
      put: Net::HTTP::Put,
      delete: Net::HTTP::Delete
    }.fetch(method)
    req = request_class.new(uri)
    if management
      req["X-Management-Key"] = key
    else
      req["Authorization"] = "Bearer #{key}"
    end
    if body
      req["Content-Type"] = "application/json"
      req.body = JSON.generate(body)
    end
    response = Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 60) do |http|
      http.request(req)
    end
    response_body = response.body.to_s.dup.force_encoding(Encoding::UTF_8)
    unless response_body.valid_encoding?
      raise CreditsContextSmokeFailure,
            "HTTP response for #{method.to_s.upcase} #{uri.path} is not valid UTF-8"
    end
    if expected && !Array(expected).map(&:to_i).include?(response.code.to_i)
      detail = safe_response_summary(response_body)
      suffix = detail.empty? ? "" : ": #{detail}"
      raise CreditsContextSmokeFailure,
            "HTTP #{response.code} for #{method.to_s.upcase} #{uri.path}#{suffix}"
    end
    [response.code.to_i, response_body, response["Content-Type"].to_s]
  end

  def safe_response_summary(value)
    safe = value.to_s.strip
    ([@management_key] + @project_keys).compact.each do |secret|
      safe.gsub!(secret, "[REDACTED]") unless secret.empty?
    end
    safe.gsub!(/\bbrv_[A-Za-z0-9_-]{12,}\b/, "brv_[REDACTED]")
    PRIVATE_FORBIDDEN.each { |marker, _label| safe.gsub!(marker, "[REDACTED]") }
    safe.slice(0, 600)
  end

  def endpoint(base, path)
    URI.join(base.to_s.end_with?("/") ? base.to_s : "#{base}/", path.sub(%r{\A/}, ""))
  end

  def management_raw(method, path, body = nil, expected: 200)
    _, response_body, = request(
      method,
      endpoint(@base, path),
      key: @management_key,
      body: body,
      management: true,
      expected: expected
    )
    response_body
  end

  def management_json(method, path, body = nil, expected: 200)
    raw = management_raw(method, path, body, expected: expected)
    raw.strip.empty? ? {} : JSON.parse(raw)
  rescue JSON::ParserError
    raise CreditsContextSmokeFailure, "non-JSON management response for #{path}"
  end

  def provider_events
    uri = endpoint(@provider, "/events")
    req = Net::HTTP::Get.new(uri)
    req["X-Canary-Control"] = @control_token
    response = Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 5) do |http|
      http.request(req)
    end
    raise CreditsContextSmokeFailure, "provider events returned HTTP #{response.code}" unless response.code.to_i == 200

    Array(JSON.parse(response.body)["events"])
  rescue JSON::ParserError
    raise CreditsContextSmokeFailure, "provider events returned invalid JSON"
  end

  def reset_provider
    uri = endpoint(@provider, "/reset")
    req = Net::HTTP::Post.new(uri)
    req["X-Canary-Control"] = @control_token
    response = Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 5) do |http|
      http.request(req)
    end
    raise CreditsContextSmokeFailure, "provider reset returned HTTP #{response.code}" unless response.code.to_i == 200
  end

  def isolated_subscriptions
    response = management_json(:get, "/v0/management/bravo/subscriptions")
    subscriptions = Array(response["subscriptions"])
    claude = subscriptions.select { |item| item["provider"].to_s.downcase == "claude" }
      .sort_by { |item| item["auth_index"].to_s }
    codex = subscriptions.select { |item| item["provider"].to_s.downcase == "codex" }
    unless claude.length == 4 && codex.length == 1 && subscriptions.length == 5
      raise CreditsContextSmokeFailure, "canary must contain exactly four Claude and one Codex subscriptions"
    end
    [claude[0], claude[1], claude[2], claude[3], codex[0]]
  end

  def assert_allocator_pending(subscription, expected:, label:)
    response = management_json(:get, "/v0/management/bravo/subscriptions")
    current = Array(response["subscriptions"]).find do |item|
      item["auth_index"] == subscription.fetch("auth_index")
    end
    raise CreditsContextSmokeFailure, "#{label}: subscription disappeared" unless current

    pending = Float(current.dig("allocator", "pending_percent") || 0)
    return if (pending - expected).abs <= 0.000_001

    raise CreditsContextSmokeFailure,
          "#{label}: pending #{pending.round(6)}%, expected #{expected.round(6)}%"
  rescue ArgumentError, TypeError
    raise CreditsContextSmokeFailure, "#{label}: pending percent is invalid"
  end

  def cleanup_stale_canary_projects
    response = management_json(:get, "/v0/management/bravo/projects")
    stale = Array(response["projects"])
    unexpected = stale.reject do |project|
      project["name"].to_s.start_with?("bravo-credits-context-", "bravo-credits-stream-")
    end
    unless unexpected.empty?
      raise CreditsContextSmokeFailure, "canary contains a non-smoke project; refusing cleanup"
    end
    stale.each do |project|
      id = project["id"].to_s
      next if id.empty?

      management_json(:delete, "/v0/management/bravo/projects", { "id" => id })
    end
  end

  def create_project(label, claude, codex)
    response = management_json(
      :post,
      "/v0/management/bravo/projects",
      {
        "name" => "bravo-#{label}-#{SecureRandom.hex(4)}",
        "enabled" => true,
        "models" => ["*"],
        "allowed_auth_ids" => [claude.fetch("auth_index"), codex.fetch("auth_index")],
        "primary_auth_ids" => [claude.fetch("auth_index"), codex.fetch("auth_index")]
      },
      expected: 201
    )
    project = response.fetch("project")
    key = response.fetch("plaintext_key")
    @projects << project.fetch("id")
    @project_keys << key
    request(
      :get,
      endpoint(@base, "/v1/models"),
      key: key,
      expected: 200
    )
    { id: project.fetch("id"), key: key }
  rescue KeyError
    raise CreditsContextSmokeFailure, "project creation response is incomplete"
  end

  def cleanup_projects
    @projects.reverse_each do |id|
      management_json(:delete, "/v0/management/bravo/projects", { "id" => id })
    rescue StandardError => error
      record_failure("temporary project cleanup", error.message)
    end
    @projects.clear
  end

  def release_project(project)
    id = project.fetch(:id)
    management_json(:delete, "/v0/management/bravo/projects", { "id" => id })
    @projects.delete(id)
    request(
      :get,
      endpoint(@base, "/v1/models"),
      key: project.fetch(:key),
      expected: 401
    )
  end

  def rotate_project(project)
    response = management_json(
      :post,
      "/v0/management/bravo/projects/rotate",
      { "id" => project.fetch(:id) }
    )
    key = response.fetch("plaintext_key")
    @project_keys << key
    request(
      :get,
      endpoint(@base, "/v1/models"),
      key: project.fetch(:key),
      expected: 401
    )
    request(
      :get,
      endpoint(@base, "/v1/models"),
      key: key,
      expected: 200
    )
    { id: project.fetch(:id), key: key }
  rescue KeyError
    raise CreditsContextSmokeFailure, "project rotation response is incomplete"
  end

  def anthropic_request(key, model, text, stream:)
    payload = {
      "model" => model,
      "messages" => [{ "role" => "user", "content" => text }],
      "max_tokens" => 64,
      "stream" => stream
    }
    request(:post, endpoint(@base, "/v1/messages"), key: key, body: payload)
  end

  def openai_chat_request(key, model, text)
    payload = {
      "model" => model,
      "messages" => [{ "role" => "user", "content" => text }],
      "max_tokens" => 64,
      "stream" => true
    }
    request(:post, endpoint(@base, "/v1/chat/completions"), key: key, body: payload)
  end

  def openai_responses_request(key, model, text)
    payload = {
      "model" => model,
      "input" => text,
      "max_output_tokens" => 64,
      "stream" => true
    }
    request(:post, endpoint(@base, "/v1/responses"), key: key, body: payload)
  end

  def recent_attempts(started)
    response = management_json(:get, "/v0/management/bravo/events")
    Array(response["events"]).select do |event|
      Time.parse(event["at"].to_s) >= started
    rescue ArgumentError
      false
    end
  end

  def assert_attempt_chain(attempts, claude, codex, first_code:, second_code: nil, second_success: false)
    ordered = attempts.sort_by do |event|
      Time.parse(event.fetch("at"))
    rescue ArgumentError, KeyError
      Time.at(0)
    end
    relevant = ordered.select do |event|
      (event["auth_id"] == claude.fetch("auth_id") && event["model"] == "claude-fable-5") ||
        (event["auth_id"] == codex.fetch("auth_id") && event["model"] == "gpt-5.6-sol")
    end
    first_index = relevant.index do |event|
      event["auth_id"] == claude.fetch("auth_id") &&
        event["model"] == "claude-fable-5" &&
        event["success"] == false
    end
    first = first_index && relevant[first_index]
    raise CreditsContextSmokeFailure, "Claude credits attempt is missing" unless first
    raise CreditsContextSmokeFailure, "wrong Claude classification #{first["error_code"].inspect}" unless \
      first["error_code"] == first_code
    raise CreditsContextSmokeFailure, "provider error is not model-scoped" unless \
      first["provider_error_code"] == "credits_required" && first["scope"] == "model"
    unless first["provider_disabled_reason"] == "org_level_disabled_until"
      raise CreditsContextSmokeFailure, "safe provider disable reason is missing from analytics"
    end

    chain = relevant.drop(first_index)
    second = chain[1]
    unless chain.length == 2 && second &&
           second["auth_id"] == codex.fetch("auth_id") &&
           second["model"] == "gpt-5.6-sol" &&
           second["success"] == second_success
      raise CreditsContextSmokeFailure, "expected an exact ordered Claude-to-Codex attempt chain"
    end
    if second_code && second["error_code"] != second_code
      raise CreditsContextSmokeFailure, "wrong Codex classification #{second["error_code"].inspect}"
    end
    unless Time.parse(first.fetch("at")) <= Time.parse(second.fetch("at"))
      raise CreditsContextSmokeFailure, "provider attempt order is invalid"
    end
    assert_redacted(JSON.generate([first, second]))
  end

  def current_provider_sequence
    provider_events.map { |event| Integer(event.fetch("sequence")) }.max || 0
  rescue ArgumentError, KeyError
    raise CreditsContextSmokeFailure, "provider event sequence is invalid"
  end

  def assert_stream_provider_pair(cursor)
    observed = provider_events.select do |event|
      Integer(event.fetch("sequence")) > cursor
    rescue ArgumentError, KeyError
      false
    end
    expected = [
      ["claude_credits_stream", "claude-fable-5"],
      ["codex_fallback_success", "gpt-5.6-sol"]
    ]
    actual = observed.map { |event| [event["type"], event["model"]] }
    unless actual == expected && observed.all? { |event| event["stream"] == true }
      raise CreditsContextSmokeFailure,
            "stream provider calls #{actual.inspect} do not match ordered Claude-to-Codex fallback"
    end
  end

  def assert_reverse_attempt_chain(attempts, codex, claude)
    ordered = attempts.sort_by do |event|
      Time.parse(event.fetch("at"))
    rescue ArgumentError, KeyError
      Time.at(0)
    end
    relevant = ordered.select do |event|
      (event["auth_id"] == codex.fetch("auth_id") && event["model"] == "gpt-5.6-terra") ||
        (event["auth_id"] == claude.fetch("auth_id") && event["model"] == "claude-sonnet-5")
    end
    unless relevant.length == 2 &&
           relevant[0]["auth_id"] == codex.fetch("auth_id") &&
           relevant[0]["model"] == "gpt-5.6-terra" &&
           relevant[0]["success"] == false &&
           relevant[0]["error_code"] == "api_error" &&
           relevant[1]["auth_id"] == claude.fetch("auth_id") &&
           relevant[1]["model"] == "claude-sonnet-5" &&
           relevant[1]["success"] == true
      raise CreditsContextSmokeFailure, "expected an exact ordered Codex-to-Claude attempt chain"
    end
    assert_redacted(JSON.generate(relevant))
  end

  def assert_reverse_stream_provider_pair(cursor)
    observed = provider_events.select do |event|
      Integer(event.fetch("sequence")) > cursor
    rescue ArgumentError, KeyError
      false
    end
    expected = [
      ["codex_server_error_stream", "gpt-5.6-terra"],
      ["claude_reverse_fallback_success", "claude-sonnet-5"]
    ]
    actual = observed.map { |event| [event["type"], event["model"]] }
    unless actual == expected && observed.all? { |event| event["stream"] == true }
      raise CreditsContextSmokeFailure,
            "stream provider calls #{actual.inspect} do not match ordered Codex-to-Claude fallback"
    end
  end

  def parse_stream_frames(body)
    frames = []
    event_name = nil
    data_lines = []
    flush = lambda do
      data = data_lines.join("\n").strip
      unless data.empty? || data == "[DONE]"
        begin
          frames << { "event" => event_name, "data" => JSON.parse(data) }
        rescue JSON::ParserError
          raise CreditsContextSmokeFailure, "stream contains a non-JSON data frame"
        end
      end
      event_name = nil
      data_lines = []
    end

    body.each_line do |raw_line|
      line = raw_line.delete_suffix("\n").delete_suffix("\r")
      if line.empty?
        flush.call
      elsif line.start_with?("event:")
        event_name = line.split(":", 2).last.to_s.strip
      elsif line.start_with?("data:")
        data_lines << line.split(":", 2).last.to_s.lstrip
      elsif line.lstrip.start_with?("{")
        flush.call
        begin
          frames << { "event" => nil, "data" => JSON.parse(line) }
        rescue JSON::ParserError
          raise CreditsContextSmokeFailure, "stream contains a non-JSON payload"
        end
      end
    end
    flush.call
    raise CreditsContextSmokeFailure, "stream did not contain any JSON frames" if frames.empty?

    frames
  end

  def stream_event_type(frame)
    frame["event"].to_s.empty? ? frame.fetch("data")["type"].to_s : frame["event"].to_s
  end

  def assert_stream_event_count(frames, event_type, expected)
    actual = frames.count { |frame| stream_event_type(frame) == event_type }
    return if actual == expected

    raise CreditsContextSmokeFailure,
          "stream emitted #{actual} #{event_type} events; expected #{expected}"
  end

  def assert_no_role_only_chat_prelude(frames)
    role_only = frames.any? do |frame|
      Array(frame.fetch("data")["choices"]).any? do |choice|
        delta = choice["delta"]
        next false unless delta.is_a?(Hash) && delta["role"] == "assistant"

        delta.all? do |name, value|
          name == "role" || value.nil? || value == "" || value == [] || value == {}
        end
      end
    end
    raise CreditsContextSmokeFailure, "chat stream emitted a role-only assistant prelude" if role_only
  end

  def assert_chat_stream_shape(frames, body)
    assistant_chunks = frames.count do |frame|
      Array(frame.fetch("data")["choices"]).any? do |choice|
        delta = choice["delta"]
        delta.is_a?(Hash) && delta["role"] == "assistant"
      end
    end
    unless assistant_chunks == 1
      raise CreditsContextSmokeFailure,
            "chat stream emitted #{assistant_chunks} assistant-start chunks; expected 1"
    end

    finish_chunks = frames.count do |frame|
      Array(frame.fetch("data")["choices"]).any? do |choice|
        !choice["finish_reason"].nil?
      end
    end
    unless finish_chunks == 1
      raise CreditsContextSmokeFailure,
            "chat stream emitted #{finish_chunks} finish chunks; expected 1"
    end

    done_count = body.each_line.count { |line| line.strip == "data: [DONE]" }
    return if done_count == 1

    raise CreditsContextSmokeFailure,
          "chat stream emitted #{done_count} [DONE] frames; expected 1"
  end

  def assert_responses_stream_text(frames)
    delta_text = frames.map do |frame|
      next unless stream_event_type(frame) == "response.output_text.delta"

      frame.fetch("data")["delta"].to_s
    end.compact.join
    unless delta_text == FALLBACK_MARKER
      raise CreditsContextSmokeFailure,
            "Responses delta text does not reconstruct exactly one fallback answer"
    end

    completed = frames.find { |frame| stream_event_type(frame) == "response.completed" }
    raise CreditsContextSmokeFailure, "Responses completed event is missing" unless completed

    completed_text = Array(completed.fetch("data").dig("response", "output")).flat_map do |item|
      Array(item["content"])
    end.map do |content|
      content["text"].to_s if content["type"] == "output_text"
    end.compact.join
    return if completed_text == delta_text

    raise CreditsContextSmokeFailure,
          "Responses completed aggregate does not match the streamed delta text"
  end

  def assert_successful_fallback_stream(body, marker_count: nil, logical_model: "bravo/fable")
    assert_includes(body, FALLBACK_MARKER)
    assert_redacted(body)
    raw_markers = [
      "credits_required",
      "usage credits are required",
      "monthly spend",
      "msg_bravo_credits_prelude",
      "claude-fable-5",
      "gpt-5.6-sol",
      "claude-sonnet-5",
      "resp_bravo_reverse_prelude",
      "model_execution_failed",
      "server_error",
      "request_id",
      "model_display_name",
      "disabled_reason"
    ]
    leaked = raw_markers.find { |marker| body.downcase.include?(marker.downcase) }
    leaked ||= '"type":"error"' if body.match?(/"type"\s*:\s*"error"/i)
    if leaked
      raise CreditsContextSmokeFailure, "pre-content provider failure leaked into client stream: #{leaked}"
    end
    logical_model_pattern = /"model"\s*:\s*#{Regexp.escape(JSON.generate(logical_model))}/
    unless body.match?(logical_model_pattern)
      raise CreditsContextSmokeFailure,
            "client stream did not restore the logical #{logical_model} model"
    end
    return if marker_count.nil?

    actual = body.scan(FALLBACK_MARKER).length
    return if actual == marker_count

    raise CreditsContextSmokeFailure,
          "stream emitted fallback marker #{actual} times; expected #{marker_count}"
  end

  def assert_event_stream_content_type(value)
    return if value.to_s.downcase.start_with?("text/event-stream")

    raise CreditsContextSmokeFailure,
          "stream Content-Type #{value.inspect} is not text/event-stream"
  end

  def assert_safe_management_surfaces
    assert_redacted(management_raw(:get, "/v0/management/bravo/events"))
    assert_redacted(management_raw(:get, "/v0/management/bravo/subscriptions"))
    status, body = request(
      :get,
      endpoint(@base, "/v0/management/auth-files"),
      key: @management_key,
      management: true
    )
    assert_redacted(body) if status == 200
  end

  def assert_includes(value, expected)
    return if value.include?(expected)

    raise CreditsContextSmokeFailure, "response is missing #{expected.inspect}"
  end

  def assert_redacted(value)
    PRIVATE_FORBIDDEN.each do |marker, label|
      next unless value.include?(marker)

      raise CreditsContextSmokeFailure, "sensitive provider marker escaped: #{label}"
    end
  end
end

options = {
  base_url: "http://127.0.0.1:18319",
  provider_url: "http://127.0.0.1:18993",
  control_token: "bravo-credits-context-canary",
  management_env_file: "secrets.env",
  management_env_variable: "MANAGEMENT_KEY",
  state_file: nil,
  docker_path: "/usr/local/bin/docker",
  container_name: nil,
  confirmed: false
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-credits-context-smoke.rb [options]"
  parser.on("--confirm-canary-mutations") { options[:confirmed] = true }
  parser.on("--base-url URL") { |value| options[:base_url] = value }
  parser.on("--provider-url URL") { |value| options[:provider_url] = value }
  parser.on("--control-token VALUE") { |value| options[:control_token] = value }
  parser.on("--management-env-file PATH") { |value| options[:management_env_file] = value }
  parser.on("--management-env-variable NAME") { |value| options[:management_env_variable] = value }
  parser.on("--reset-state-file PATH") { |value| options[:state_file] = value }
  parser.on("--docker PATH") { |value| options[:docker_path] = value }
  parser.on("--restart-container NAME") { |value| options[:container_name] = value }
end.parse!

abort("pass --confirm-canary-mutations") unless options[:confirmed]
abort("pass --restart-container with the explicit canary name") if options[:container_name].to_s.empty?
abort("pass --reset-state-file with the explicit canary bravo-state.json") if options[:state_file].to_s.empty?
abort("unexpected positional arguments") unless ARGV.empty?

exit BravoCreditsContextSmoke.new(options).run
