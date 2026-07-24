#!/usr/bin/env ruby
# frozen_string_literal: true

# Mutating canary-only smoke for Bravo 0.5 management contracts.
# It never prints management/project keys or raw auth identities.

require "json"
require "net/http"
require "optparse"
require "securerandom"
require "time"
require "uri"

class SmokeFailure < StandardError; end

class Bravo05CanarySmoke
  def initialize(options)
    @base = URI(options.fetch(:base_url))
    @management_key = read_secret(
      options.fetch(:management_env_file),
      options.fetch(:management_env_variable)
    )
    @project_id = nil
    @project_key = nil
    @route_id = nil
    @route_original = nil
    @route_was_overridden = false
    @passed = 0
    @failed = 0
    validate_target!
  end

  def run
    check("Bravo reports version 0.5.0") { verify_version }
    subscriptions = check("subscription catalog exposes safe analytics IDs") do
      verify_subscriptions
    end
    return finish unless subscriptions

    selected = subscriptions.find { |item| item["provider"].to_s.downcase == "codex" }
    secondary = subscriptions.find { |item| item["auth_index"] != selected&.dig("auth_index") }
    check("provider-aware Codex Pro tariff resolves to x20") do
      verify_codex_x20(selected)
    end
    check("route preview is non-persistent") { verify_route_preview }
    check("route override applies hot and resets exactly") { verify_route_mutation }

    if selected && secondary
      check("strict project subscription pool persists") do
        create_strict_project(selected)
      end
      check("primary outside the allowed pool fails closed") do
        reject_primary_outside_pool(selected, secondary)
      end
      check("strict-pool project executes through its allowed account") do
        execute_project_request
      end
      check("project analytics attributes the exact subscription") do
        verify_project_analytics(selected)
      end
    else
      record_failure("strict project pool", "canary needs a Codex account and one second account")
    end

    finish
  ensure
    cleanup_project
    restore_route
    @management_key = nil
    @project_key = nil
  end

  private

  def check(name)
    value = yield
    @passed += 1
    puts "PASS  #{name}"
    value
  rescue StandardError => error
    record_failure(name, error.message)
    nil
  end

  def finish
    puts
    puts "Bravo 0.5 canary smoke: #{@passed} passed, #{@failed} failed"
    @failed.zero? ? 0 : 1
  end

  def record_failure(name, message)
    @failed += 1
    safe = message.to_s
    safe = safe.gsub(@management_key, "[REDACTED]") if @management_key
    safe = safe.gsub(@project_key, "[REDACTED]") if @project_key
    safe = safe.gsub(/\bbrv_[A-Za-z0-9_-]{16,}\b/, "brv_[REDACTED]")
    puts "FAIL  #{name}: #{safe}"
  end

  def validate_target!
    raise SmokeFailure, "only http is supported for this local canary smoke" unless @base.scheme == "http"
    raise SmokeFailure, "production port 18317 is refused" if @base.port == 18_317
    raise SmokeFailure, "canary port must be 18319" unless @base.port == 18_319
    raise SmokeFailure, "base URL must not contain credentials" if @base.user || @base.password
  end

  def read_secret(path, variable)
    stat = File.stat(path)
    raise SmokeFailure, "management env file must be regular" unless stat.file?
    raise SmokeFailure, "management env file must be mode 0600" unless (stat.mode & 0o077).zero?

    matches = File.readlines(path, chomp: true).each_with_object([]) do |line, values|
      next if line.lstrip.start_with?("#")

      name, value = line.split("=", 2)
      next unless name&.strip == variable

      values << value.to_s.strip.sub(/\A(['"])(.*)\1\z/, '\2')
    end
    raise SmokeFailure, "management variable is missing or duplicated" unless matches.length == 1
    raise SmokeFailure, "management variable is empty" if matches.first.empty?

    matches.first
  end

  def management_request(method, path, body = nil, expected: 200)
    request(method, path, body, key: @management_key, expected: expected)
  end

  def project_request(method, path, body = nil, expected: 200)
    request(method, path, body, key: @project_key, expected: expected)
  end

  def request(method, path, body, key:, expected:)
    uri = URI.join(@base.to_s.end_with?("/") ? @base.to_s : "#{@base}/", path.sub(%r{\A/}, ""))
    request_class = {
      get: Net::HTTP::Get,
      post: Net::HTTP::Post,
      patch: Net::HTTP::Patch,
      put: Net::HTTP::Put,
      delete: Net::HTTP::Delete
    }.fetch(method)
    req = request_class.new(uri)
    req["X-Management-Key"] = key if key == @management_key
    req["Authorization"] = "Bearer #{key}" if key == @project_key
    if body
      req["Content-Type"] = "application/json"
      req.body = JSON.generate(body)
    end
    response = Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 120) do |http|
      http.request(req)
    end
    unless Array(expected).map(&:to_i).include?(response.code.to_i)
      raise SmokeFailure, "HTTP #{response.code} for #{method.to_s.upcase} #{path}"
    end
    return {} if response.body.to_s.strip.empty?

    JSON.parse(response.body)
  rescue JSON::ParserError
    raise SmokeFailure, "non-JSON response for #{method.to_s.upcase} #{path}"
  end

  def verify_version
    status = management_request(:get, "/v0/management/bravo/status")
    raise SmokeFailure, "unexpected plugin version" unless status["version"] == "0.5.0"
    raise SmokeFailure, "plugin is not enabled" unless status["enabled"] == true
  end

  def verify_subscriptions
    response = management_request(:get, "/v0/management/bravo/subscriptions")
    subscriptions = Array(response["subscriptions"])
    raise SmokeFailure, "fewer than two canary subscriptions" if subscriptions.length < 2

    subscriptions.each do |item|
      analytics_id = item["analytics_id"].to_s
      raise SmokeFailure, "unsafe or missing analytics_id" unless analytics_id.match?(/\Asub_[a-f0-9]+\z/)
    end
    subscriptions
  end

  def verify_codex_x20(subscription)
    raise SmokeFailure, "Codex subscription is missing" unless subscription

    plan = subscription["plan"].to_s.downcase
    raise SmokeFailure, "Codex plan is not recognized as Pro" unless plan.include?("pro")
    raise SmokeFailure, "Codex Pro did not resolve to x20" unless subscription["effective_tariff"] == "x20"
  end

  def verify_route_preview
    routes = management_request(:get, "/v0/management/bravo/routes")
    route = Array(routes["routes"]).find { |item| item["id"] == "opus" }
    raise SmokeFailure, "opus route is missing" unless route

    @route_id = route["id"]
    @route_original = Array(route["candidates"]).map do |item|
      {
        "provider" => item["provider"],
        "model" => item["model"],
        "effort" => item["effort"].to_s
      }.reject { |key, value| key == "effort" && value.empty? }
    end
    @route_was_overridden = route["overridden"] == true
    raise SmokeFailure, "opus route needs two candidates" unless @route_original.length == 2

    preview = management_request(
      :put,
      "/v0/management/bravo/routes",
      { "id" => @route_id, "candidates" => @route_original.reverse, "preview" => true }
    )
    raise SmokeFailure, "preview flag was not returned" unless preview["preview"] == true

    current = management_request(:get, "/v0/management/bravo/routes")
    effective = Array(current["routes"]).find { |item| item["id"] == @route_id }
    providers = Array(effective&.dig("candidates")).map { |item| item["provider"] }
    expected = @route_original.map { |item| item["provider"] }
    raise SmokeFailure, "preview persisted unexpectedly" unless providers == expected
  end

  def verify_route_mutation
    raise SmokeFailure, "route preview did not initialize" unless @route_id && @route_original

    management_request(
      :put,
      "/v0/management/bravo/routes",
      { "id" => @route_id, "candidates" => @route_original.reverse }
    )
    changed = management_request(:get, "/v0/management/bravo/routes")
    route = Array(changed["routes"]).find { |item| item["id"] == @route_id }
    providers = Array(route&.dig("candidates")).map { |item| item["provider"] }
    expected = @route_original.reverse.map { |item| item["provider"] }
    raise SmokeFailure, "hot route order did not change" unless providers == expected

    management_request(:post, "/v0/management/bravo/routes/reset", { "id" => @route_id })
    reset = management_request(:get, "/v0/management/bravo/routes")
    route = Array(reset["routes"]).find { |item| item["id"] == @route_id }
    default_providers = Array(route&.dig("default_candidates")).map { |item| item["provider"] }
    current_providers = Array(route&.dig("candidates")).map { |item| item["provider"] }
    raise SmokeFailure, "route reset differs from defaults" unless current_providers == default_providers

    @route_id = nil
    @route_original = nil
  end

  def create_strict_project(subscription)
    auth_index = subscription.fetch("auth_index")
    response = management_request(
      :post,
      "/v0/management/bravo/projects",
      {
        "name" => "bravo-0.5-smoke-#{SecureRandom.hex(4)}",
        "enabled" => true,
        "models" => ["opus"],
        "allowed_auth_ids" => [auth_index],
        # A subscription may intentionally be the dedicated primary of another
        # active project. Strict-pool authorization is independent from that
        # ownership rule, so this smoke must not try to steal the primary.
        "primary_auth_ids" => []
      },
      expected: 201
    )
    project = response.fetch("project")
    @project_id = project.fetch("id")
    @project_key = response.fetch("plaintext_key")
    raise SmokeFailure, "project key was not issued" unless @project_key.start_with?("brv_")
    raise SmokeFailure, "allowed pool differs" unless project["allowed_auth_ids"] == [auth_index]
    raise SmokeFailure, "primary pool differs" unless Array(project["primary_auth_ids"]).empty?
  end

  def reject_primary_outside_pool(selected, secondary)
    response = management_request(
      :patch,
      "/v0/management/bravo/projects",
      {
        "id" => @project_id,
        "name" => "bravo-0.5-smoke",
        "enabled" => true,
        "models" => ["opus"],
        "allowed_auth_ids" => [selected.fetch("auth_index")],
        "primary_auth_ids" => [secondary.fetch("auth_index")]
      },
      expected: 409
    )
    code = response.dig("error", "code") || response["code"]
    raise SmokeFailure, "unexpected fail-closed error code" unless code.to_s.include?("primary")
  end

  def execute_project_request
    body = {
      "model" => "bravo/opus",
      "messages" => [{ "role" => "user", "content" => "Reply with exactly: pool-ok" }],
      "max_tokens" => 32
    }
    response = project_request(:post, "/v1/chat/completions", body)
    content = response.dig("choices", 0, "message", "content").to_s
    raise SmokeFailure, "project request returned no content" if content.empty?
  end

  def verify_project_analytics(subscription)
    from = (Time.now.utc - 3600).iso8601
    to = Time.now.utc.iso8601
    path = [
      "/v0/management/bravo/analytics",
      "?project_id=#{URI.encode_www_form_component(@project_id)}",
      "&from=#{URI.encode_www_form_component(from)}",
      "&to=#{URI.encode_www_form_component(to)}",
      "&interval=hour"
    ].join
    analytics = management_request(:get, path)
    raise SmokeFailure, "analytics schema is not v2" unless analytics["schema_version"] == 2
    raise SmokeFailure, "analytics did not count the request" unless analytics.dig("summary", "requests").to_i >= 1

    expected_id = subscription.fetch("analytics_id")
    rows = Array(analytics.dig("breakdown", "project_subscription_models"))
    raise SmokeFailure, "exact subscription attribution is missing" unless rows.any? do |row|
      row["subscription_id"] == expected_id && row["project_id"] == @project_id
    end
    serialized = JSON.generate(analytics)
    raw_identity = subscription.fetch("auth_index")
    raise SmokeFailure, "analytics leaked raw auth identity" if serialized.include?(raw_identity)
  end

  def cleanup_project
    return unless @project_id

    management_request(
      :delete,
      "/v0/management/bravo/projects",
      { "id" => @project_id },
      expected: 200
    )
  rescue StandardError => error
    record_failure("temporary project cleanup", error.message)
  ensure
    @project_id = nil
  end

  def restore_route
    return unless @route_id && @route_original

    if @route_was_overridden
      management_request(
        :put,
        "/v0/management/bravo/routes",
        { "id" => @route_id, "candidates" => @route_original }
      )
    else
      management_request(:post, "/v0/management/bravo/routes/reset", { "id" => @route_id })
    end
  rescue StandardError => error
    record_failure("route cleanup", error.message)
  ensure
    @route_id = nil
    @route_original = nil
  end
end

options = {
  base_url: "http://127.0.0.1:18319",
  management_env_file: "secrets.env",
  management_env_variable: "MANAGEMENT_KEY",
  confirmed: false
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-0.5-canary-smoke.rb [options]"
  parser.on("--confirm-canary-mutations") { options[:confirmed] = true }
  parser.on("--base-url URL") { |value| options[:base_url] = value }
  parser.on("--management-env-file PATH") { |value| options[:management_env_file] = value }
  parser.on("--management-env-variable NAME") { |value| options[:management_env_variable] = value }
end.parse!

abort("pass --confirm-canary-mutations") unless options[:confirmed]
abort("unexpected positional arguments") unless ARGV.empty?

exit Bravo05CanarySmoke.new(options).run
