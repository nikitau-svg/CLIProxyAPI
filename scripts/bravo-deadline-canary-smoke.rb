#!/usr/bin/env ruby
# frozen_string_literal: true

# Mutating, canary-only proof for Bravo's streaming pre-commit hedge. It creates
# one disposable project, sends one request through fake slow Claude and fake
# fast Codex credentials, validates provider/Core/Bravo accounting, then removes
# the project. Secrets are accepted only from a mode-0600 file and never printed.

require "json"
require "net/http"
require "optparse"
require "securerandom"
require "time"
require "uri"

DeadlineCanaryFailure = Class.new(StandardError)

options = {
  base_url: "http://127.0.0.1:18319",
  provider_url: "http://127.0.0.1:18992",
  management_env_file: "secrets.env",
  management_env_variable: "MANAGEMENT_KEY",
  control_token: "bravo-deadline-canary",
  marker: "BRAVO_DEADLINE_FALLBACK_OK",
  maximum_seconds: 8,
  confirmed: false
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-deadline-canary-smoke.rb [options]"
  parser.on("--confirm-canary-mutations") { options[:confirmed] = true }
  parser.on("--base-url URL", "Canary base URL") { |value| options[:base_url] = value }
  parser.on("--provider-url URL", "Fake provider origin") { |value| options[:provider_url] = value }
  parser.on("--management-env-file PATH", "Mode-0600 management env file") do |value|
    options[:management_env_file] = value
  end
  parser.on("--management-env-variable NAME", "Management key variable") do |value|
    options[:management_env_variable] = value
  end
  parser.on("--control-token VALUE", "Fake-provider control token") do |value|
    options[:control_token] = value
  end
  parser.on("--marker VALUE", "Expected Codex marker") { |value| options[:marker] = value }
  parser.on("--maximum-seconds SECONDS", Integer, "Maximum end-to-end duration") do |value|
    options[:maximum_seconds] = value
  end
end.parse!

abort("pass --confirm-canary-mutations") unless options[:confirmed]
abort("unexpected positional arguments") unless ARGV.empty?

class BravoDeadlineCanarySmoke
  def initialize(options)
    @base = URI(options.fetch(:base_url))
    @provider = URI(options.fetch(:provider_url))
    @api_key_base_url = "http://host.docker.internal:#{@provider.port}"
    @control_token = options.fetch(:control_token)
    @marker = options.fetch(:marker)
    @maximum_seconds = options.fetch(:maximum_seconds)
    @management_key = read_secret(
      options.fetch(:management_env_file),
      options.fetch(:management_env_variable)
    )
    @project_id = nil
    @project_key = nil
    @secrets = [@management_key]
    @passed = 0
    @failed = 0
    validate_targets!
  end

  def run
    begin
      check("candidate reports Bravo 0.7.8 and starts without cooldown") do
      status = management_request(:get, "/v0/management/bravo/status")
      raise DeadlineCanaryFailure, "Bravo version is #{status["version"].inspect}" unless status["version"] == "0.7.8"
      raise DeadlineCanaryFailure, "Bravo is not enabled" unless status["enabled"] == true
      raise DeadlineCanaryFailure, "Bravo starts degraded" unless status["degraded"] == false
      raise DeadlineCanaryFailure, "Bravo prefix is not bravo/" unless status["prefix"] == "bravo/"
      raise DeadlineCanaryFailure, "canary starts with cooldowns" unless status["cooldowns"].to_i.zero?
      end

    check("candidate exposes the exact short hedge and Sonnet route") do
      config = management_request(:get, "/v0/management/bravo/config")
      unless config["fallback_hedge_delay_seconds"].to_i == 1 && config["max_attempts"].to_i == 2
        raise DeadlineCanaryFailure, "canary hedge config is not delay=1/max_attempts=2"
      end
      candidates = Array(config.dig("models", "sonnet", "candidates"))
      route = candidates.map { |item| [item["provider"], item["model"]] }
      expected = [["claude", "claude-sonnet-5"], ["codex", "gpt-5.6-terra"]]
      raise DeadlineCanaryFailure, "Sonnet route is #{route.inspect}" unless route == expected
    end

    subscriptions = management_request(:get, "/v0/management/bravo/subscriptions")
    claude = unique_provider_subscription(subscriptions, "claude")
    codex = unique_provider_subscription(subscriptions, "codex")

    check("fake Claude and Codex subscriptions are ready") do
      [claude, codex].each do |item|
        raise DeadlineCanaryFailure, "#{item["provider"]} auth_index is empty" if item["auth_index"].to_s.empty?
        raise DeadlineCanaryFailure, "#{item["provider"]} auth_id is empty" if item["auth_id"].to_s.empty?
        unless item["analytics_id"].to_s.match?(/\Asub_[0-9a-f]{16}\z/)
          raise DeadlineCanaryFailure, "#{item["provider"]} analytics_id is invalid"
        end
        raise DeadlineCanaryFailure, "#{item["provider"]} subscription is disabled" unless item["enabled"] == true
        unless item["health"].to_s == "ready"
          raise DeadlineCanaryFailure, "#{item["provider"]} subscription health is #{item["health"].inspect}"
        end
      end
      if claude["auth_index"] == codex["auth_index"] || claude["auth_id"] == codex["auth_id"]
        raise DeadlineCanaryFailure, "fake subscriptions do not have distinct identities"
      end
    end

    reset_provider_events
    create_project(claude, codex)
    core_before = api_key_snapshots(claude.fetch("provider"), codex.fetch("provider"))
    bravo_before = management_request(:get, "/v0/management/bravo/status")
    started = Time.now.utc

    check("slow Claude is invisibly hedged by fast Codex") do
      elapsed, status, content_type, body = project_stream_request
      raise DeadlineCanaryFailure, "stream returned HTTP #{status}" unless status == 200
      unless content_type.to_s.downcase.start_with?("text/event-stream")
        raise DeadlineCanaryFailure, "stream content type is #{content_type.inspect}"
      end
      raise DeadlineCanaryFailure, "stream exceeded #{@maximum_seconds}s (#{elapsed.round(3)}s)" if elapsed > @maximum_seconds
      done_count = body.lines.count { |line| line.strip == "data: [DONE]" }
      raise DeadlineCanaryFailure, "terminal marker count is #{done_count}, want 1" unless done_count == 1
      marker_count = body.scan(@marker).length
      raise DeadlineCanaryFailure, "Codex marker count is #{marker_count}, want 1" unless marker_count == 1
      if body.include?("canary_stall_expired") ||
         body.include?("deterministic Claude stall") ||
         body.include?("claude-sonnet-5") ||
         body.include?("gpt-5.6-terra")
        raise DeadlineCanaryFailure, "Claude stall response leaked into the client stream"
      end
      payloads = parse_sse_payloads(body)
      models = payloads.map { |item| item["model"].to_s }.reject(&:empty?).uniq
      unless !models.empty? && models == ["bravo/sonnet"]
        raise DeadlineCanaryFailure, "client-visible models are #{models.inspect}"
      end
    end

    check("provider sequence contains one canceled loser") do
      events = wait_for_provider_cancellation
      types = events.map { |event| event["type"] }
      %w[claude_started codex_started codex_completed claude_canceled].each do |type|
        count = types.count(type)
        raise DeadlineCanaryFailure, "#{type} count is #{count}, want 1" unless count == 1
      end
      claude_started = event_sequence(events, "claude_started")
      codex_started = event_sequence(events, "codex_started")
      codex_completed = event_sequence(events, "codex_completed")
      claude_canceled = event_sequence(events, "claude_canceled")
      unless claude_started < codex_started &&
             codex_started < codex_completed &&
             codex_started < claude_canceled
        raise DeadlineCanaryFailure, "provider event order is invalid: #{types.join(",")}"
      end
      expected_models = {
        "claude_started" => "claude-sonnet-5",
        "claude_canceled" => "claude-sonnet-5",
        "codex_started" => "gpt-5.6-terra",
        "codex_completed" => "gpt-5.6-terra"
      }
      events.each do |event|
        expected = expected_models[event["type"]]
        next unless expected
        raise DeadlineCanaryFailure, "#{event["type"]} model is #{event["model"].inspect}" unless event["model"] == expected
      end
    end

    check("Bravo records winner and neutral superseded attempt") do
      attempts = recent_attempts(started, claude["auth_id"], codex["auth_id"])
      claude_attempts = attempts.select { |event| event["auth_id"] == claude["auth_id"] }
      codex_attempts = attempts.select { |event| event["auth_id"] == codex["auth_id"] }
      unless claude_attempts.length == 1 &&
             claude_attempts.first["success"] == false &&
             claude_attempts.first["status"].to_i == 499 &&
             claude_attempts.first["error_code"] == "bravo_attempt_superseded"
        raise DeadlineCanaryFailure, "Claude attempt is not one neutral superseded 499"
      end
      unless codex_attempts.length == 1 &&
             codex_attempts.first["success"] == true &&
             codex_attempts.first["status"].to_i == 200
        raise DeadlineCanaryFailure, "Codex attempt is not one successful 200"
      end
    end

    check("Core commits only the Codex winner and stays quiet") do
      wait_for_core_winner(claude, codex, core_before)
      sleep 1
      core_after = api_key_snapshots(claude.fetch("provider"), codex.fetch("provider"))
      assert_core_accounting(
        claude,
        codex,
        core_before,
        core_after
      )
    end

    check("loser creates no cooldown and analytics count only the winner") do
      status = management_request(:get, "/v0/management/bravo/status")
      raise DeadlineCanaryFailure, "superseded attempt created a cooldown" unless status["cooldowns"].to_i.zero?
      raise DeadlineCanaryFailure, "Bravo became degraded after the hedge" unless status["degraded"] == false
      unless status["status_code"].to_s.empty?
        raise DeadlineCanaryFailure, "Bravo reports a status error after the hedge"
      end
      unless status["recent_success"].to_i == bravo_before["recent_success"].to_i + 1 &&
             status["recent_superseded"].to_i == bravo_before["recent_superseded"].to_i + 1 &&
             status["recent_failure"].to_i == bravo_before["recent_failure"].to_i
        raise DeadlineCanaryFailure, "recent status does not separate the neutral loser"
      end

      current = management_request(:get, "/v0/management/bravo/subscriptions")
      current_claude = subscription_by_auth_index(current, claude.fetch("auth_index"))
      current_codex = subscription_by_auth_index(current, codex.fetch("auth_index"))
      unless current_claude["health"] == "ready" && current_codex["health"] == "ready"
        raise DeadlineCanaryFailure, "provider health changed after the hedge"
      end

      analytics = wait_for_project_analytics
      summary = analytics.fetch("summary")
      raise DeadlineCanaryFailure, "analytics requests is not 1" unless summary["requests"].to_i == 1
      raise DeadlineCanaryFailure, "analytics failures is not 0" unless summary["failures"].to_i.zero?
      unless summary["input_tokens"].to_i == 7 &&
             summary["output_tokens"].to_i == 3 &&
             summary["total_tokens"].to_i == 10
        raise DeadlineCanaryFailure, "analytics token totals are not 7/3/10"
      end

      breakdown = Array(analytics.dig("breakdown", "subscriptions"))
      used = breakdown.select { |item| item.dig("usage", "requests").to_i.positive? }
      unless used.length == 1 && used.first["subscription_id"] == codex["analytics_id"]
        raise DeadlineCanaryFailure, "analytics attributed usage to a loser or multiple subscriptions"
      end
      providers = Array(analytics.dig("breakdown", "providers")).select do |item|
        item.dig("usage", "requests").to_i.positive?
      end
      unless providers.length == 1 && providers.first["provider"] == "codex"
        raise DeadlineCanaryFailure, "analytics provider breakdown is not Codex-only"
      end
      models = Array(analytics.dig("breakdown", "models")).select do |item|
        item.dig("usage", "requests").to_i.positive?
      end
      unless models.length == 1 &&
             models.first["model"] == "gpt-5.6-terra" &&
             models.first["logical_model"] == "bravo/sonnet"
        raise DeadlineCanaryFailure, "analytics model breakdown is not the Codex Sonnet winner"
      end
      serialized = JSON.generate(analytics)
      if serialized.include?(claude["auth_index"]) || serialized.include?(codex["auth_index"])
        raise DeadlineCanaryFailure, "analytics exposed a raw auth_index"
      end
    end
    rescue StandardError => error
      record_failure("deadline canary setup", error.message)
    ensure
      cleanup_project
    end
    finish
  ensure
    @management_key = nil
    @project_key = nil
    @secrets.clear
  end

  private

  def validate_targets!
    raise DeadlineCanaryFailure, "production port 18317 is refused" if @base.port == 18_317
    unless @base.scheme == "http" && %w[127.0.0.1 localhost ::1].include?(@base.host) && @base.port == 18_319
      raise DeadlineCanaryFailure, "canary must be http loopback:18319"
    end
    unless @provider.scheme == "http" && %w[127.0.0.1 localhost ::1].include?(@provider.host) &&
           @provider.port == 18_992
      raise DeadlineCanaryFailure, "fake provider must be http loopback:18992"
    end
    unless (2..30).cover?(@maximum_seconds)
      raise DeadlineCanaryFailure, "maximum-seconds must be between 2 and 30"
    end
  end

  def read_secret(path, variable)
    stat = File.stat(path)
    raise DeadlineCanaryFailure, "management env path is not a regular file" unless stat.file?
    if (stat.mode & 0o077) != 0
      raise DeadlineCanaryFailure, "management env file must be mode 0600"
    end

    matches = File.readlines(path, chomp: true).each_with_object([]) do |line, values|
      next if line.lstrip.start_with?("#")

      name, value = line.split("=", 2)
      next unless name&.strip == variable

      values << value.to_s.strip.sub(/\A(['"])(.*)\1\z/, '\2')
    end
    unless matches.length == 1 && !matches.first.empty?
      raise DeadlineCanaryFailure, "management variable is missing, empty, or duplicated"
    end
    matches.first
  rescue Errno::ENOENT, Errno::EACCES => error
    raise DeadlineCanaryFailure, "management env file cannot be read: #{error.class}"
  end

  def check(name)
    yield
    @passed += 1
    puts "PASS  #{name}"
  rescue StandardError => error
    record_failure(name, error.message)
  end

  def record_failure(name, message)
    @failed += 1
    safe = message.to_s.dup
    @secrets.compact.each { |secret| safe.gsub!(secret, "[REDACTED]") unless secret.empty? }
    safe.gsub!(/\bbrv_[A-Za-z0-9_-]{16,}\b/, "brv_[REDACTED]")
    safe.gsub!(/\b(?:sk|canary)-[A-Za-z0-9_-]{16,}\b/, "[REDACTED]")
    puts "FAIL  #{name}: #{safe}"
  end

  def finish
    puts
    puts "Bravo deadline canary smoke: #{@passed} passed, #{@failed} failed"
    @failed.zero? ? 0 : 1
  end

  def management_request(method, path, body = nil, expected: 200)
    request(method, path, body, @management_key, expected, management: true)
  end

  def request(method, path, body, key, expected, management: false)
    uri = join_uri(@base, path)
    request_class = {
      get: Net::HTTP::Get,
      post: Net::HTTP::Post,
      patch: Net::HTTP::Patch,
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
    response = Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 30) do |http|
      http.request(req)
    end
    unless Array(expected).map(&:to_i).include?(response.code.to_i)
      raise DeadlineCanaryFailure, "HTTP #{response.code} for #{method.to_s.upcase} #{uri.path}"
    end
    return {} if response.body.to_s.strip.empty?

    JSON.parse(response.body)
  rescue JSON::ParserError
    raise DeadlineCanaryFailure, "non-JSON response for #{method.to_s.upcase} #{uri.path}"
  end

  def join_uri(origin, path)
    URI.join(origin.to_s.end_with?("/") ? origin.to_s : "#{origin}/", path.sub(%r{\A/}, ""))
  end

  def provider_request(method, path)
    uri = join_uri(@provider, path)
    request_class = { get: Net::HTTP::Get, post: Net::HTTP::Post }.fetch(method)
    req = request_class.new(uri)
    req["X-Canary-Control"] = @control_token
    response = Net::HTTP.start(uri.host, uri.port, open_timeout: 3, read_timeout: 5) do |http|
      http.request(req)
    end
    raise DeadlineCanaryFailure, "fake provider returned HTTP #{response.code}" unless response.code.to_i == 200

    JSON.parse(response.body)
  rescue JSON::ParserError
    raise DeadlineCanaryFailure, "fake provider returned non-JSON control response"
  end

  def reset_provider_events
    provider_request(:post, "/reset")
  end

  def provider_events
    Array(provider_request(:get, "/events")["events"])
  end

  def wait_for_provider_cancellation
    deadline = Time.now + 5
    loop do
      events = provider_events
      return events if events.any? { |event| event["type"] == "claude_canceled" }
      raise DeadlineCanaryFailure, "Claude loser was not canceled" if Time.now >= deadline

      sleep 0.1
    end
  end

  def event_sequence(events, type)
    events.find { |event| event["type"] == type }.fetch("sequence").to_i
  end

  def unique_provider_subscription(payload, provider)
    items = Array(payload["subscriptions"]).select do |item|
      item["provider"].to_s.downcase == provider
    end
    raise DeadlineCanaryFailure, "expected one #{provider} subscription, found #{items.length}" unless items.length == 1

    items.first
  end

  def subscription_by_auth_index(payload, auth_index)
    Array(payload["subscriptions"]).find { |item| item["auth_index"] == auth_index } ||
      raise(DeadlineCanaryFailure, "subscription disappeared")
  end

  def create_project(claude, codex)
    response = management_request(
      :post,
      "/v0/management/bravo/projects",
      {
        "name" => "bravo-deadline-canary-#{SecureRandom.hex(4)}",
        "enabled" => true,
        "models" => ["sonnet"],
        "allowed_auth_ids" => [claude.fetch("auth_index"), codex.fetch("auth_index")],
        "primary_auth_ids" => [claude.fetch("auth_index"), codex.fetch("auth_index")]
      },
      expected: 201
    )
    project = response.fetch("project")
    @project_id = project.fetch("id")
    @project_key = response.fetch("plaintext_key")
    @secrets << @project_key
    unless project["status"] == "active" &&
           Array(project["models"]) == ["sonnet"] &&
           Array(project["allowed_auth_ids"]).sort == [claude["auth_index"], codex["auth_index"]].sort &&
           Array(project["primary_auth_ids"]).sort == [claude["auth_index"], codex["auth_index"]].sort
      raise DeadlineCanaryFailure, "created project does not match the requested pool"
    end
  end

  def project_stream_request
    uri = join_uri(@base, "/v1/chat/completions")
    req = Net::HTTP::Post.new(uri)
    req["Authorization"] = "Bearer #{@project_key}"
    req["Content-Type"] = "application/json"
    req["Accept"] = "text/event-stream"
    req.body = JSON.generate(
      "model" => "bravo/sonnet",
      "messages" => [{ "role" => "user", "content" => "Reply with the canary marker." }],
      "max_tokens" => 32,
      "stream" => true
    )
    status = 0
    content_type = ""
    body = +""
    started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 30) do |http|
      http.request(req) do |response|
        status = response.code.to_i
        content_type = response["Content-Type"].to_s
        response.read_body { |chunk| body << chunk }
      end
    end
    elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - started
    [elapsed, status, content_type, body]
  end

  def parse_sse_payloads(body)
    body.lines.each_with_object([]) do |line, payloads|
      line = line.strip
      next unless line.start_with?("data: ")
      raw = line.delete_prefix("data: ").strip
      next if raw == "[DONE]"

      payloads << JSON.parse(raw)
    rescue JSON::ParserError
      raise DeadlineCanaryFailure, "client stream contains invalid JSON data"
    end
  end

  def recent_attempts(started, *auth_ids)
    payload = management_request(:get, "/v0/management/bravo/events")
    Array(payload["events"]).select do |event|
      Time.parse(event["at"].to_s) >= started &&
        event["logical_model"] == "sonnet" &&
        auth_ids.include?(event["auth_id"])
    rescue ArgumentError
      false
    end
  end

  def api_key_snapshots(*providers)
    payload = management_request(:get, "/v0/management/api-key-usage")
    providers.each_with_object({}) do |provider, snapshots|
      entries = payload.fetch(provider.to_s, {})
      unless entries.is_a?(Hash)
        raise DeadlineCanaryFailure, "api-key-usage returned malformed #{provider} rows"
      end
      matches = entries.select { |composite, _usage| composite.start_with?("#{@api_key_base_url}|") }
      unless matches.length == 1
        raise DeadlineCanaryFailure, "api-key-usage returned #{matches.length} canary #{provider} rows"
      end
      usage = matches.values.first
      success = usage["success"]
      failed = usage["failed"]
      unless success.is_a?(Integer) && success >= 0 && failed.is_a?(Integer) && failed >= 0
        raise DeadlineCanaryFailure, "api-key-usage returned invalid #{provider} counters"
      end
      snapshots[provider] = {
        "success" => success,
        "failed" => failed
      }
    end
  end

  def wait_for_core_winner(claude, codex, before)
    deadline = Time.now + 5
    loop do
      after = api_key_snapshots(claude.fetch("provider"), codex.fetch("provider"))
      if after.fetch(codex.fetch("provider"))["success"] ==
         before.fetch(codex.fetch("provider"))["success"] + 1
        assert_core_accounting(claude, codex, before, after)
        return
      end
      raise DeadlineCanaryFailure, "Core winner accounting did not become visible" if Time.now >= deadline

      sleep 0.1
    end
  end

  def assert_core_accounting(claude, codex, before, after)
    claude_before = before.fetch(claude.fetch("provider"))
    codex_before = before.fetch(codex.fetch("provider"))
    claude_after = after.fetch(claude.fetch("provider"))
    codex_after = after.fetch(codex.fetch("provider"))
    unless claude_after == claude_before
      raise DeadlineCanaryFailure, "#{claude["provider"]} loser changed Core accounting or health"
    end
    expected_codex = codex_before.merge("success" => codex_before["success"] + 1)
    unless codex_after == expected_codex
      raise DeadlineCanaryFailure, "#{codex["provider"]} winner Core accounting is not exactly success +1"
    end
  end

  def wait_for_project_analytics
    deadline = Time.now + 5
    query = URI.encode_www_form("interval" => "hour", "project_id" => @project_id)
    loop do
      analytics = management_request(:get, "/v0/management/bravo/analytics?#{query}")
      return analytics if analytics.dig("summary", "requests").to_i.positive?
      raise DeadlineCanaryFailure, "project analytics did not become visible" if Time.now >= deadline

      sleep 0.1
    end
  end

  def cleanup_project
    return unless @project_id

    deleted = management_request(:delete, "/v0/management/bravo/projects", { "id" => @project_id })
    unless deleted["deleted"] == true && deleted["id"] == @project_id
      raise DeadlineCanaryFailure, "temporary project delete response is invalid"
    end
  rescue StandardError => error
    record_failure("temporary project cleanup", error.message)
  ensure
    @project_id = nil
  end
end

exit BravoDeadlineCanarySmoke.new(options).run
