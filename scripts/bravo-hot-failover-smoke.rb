#!/usr/bin/env ruby
# frozen_string_literal: true

# Canary-only controlled 429 smoke. A local fault endpoint must already be
# listening. The script installs two disposable Claude credentials, verifies
# invisible pre-payload streaming fallback to Codex, then verifies that a
# strict project pool cannot escape to Codex.

require "json"
require "net/http"
require "optparse"
require "securerandom"
require "time"
require "uri"

class FailoverSmokeFailure < StandardError; end

class BravoHotFailoverSmoke
  def initialize(options)
    @base = URI(options.fetch(:base_url))
    @fault_base_url = options.fetch(:fault_base_url)
    @management_key = read_secret(
      options.fetch(:management_env_file),
      options.fetch(:management_env_variable)
    )
    @project_id = nil
    @project_key = nil
    @installed_fault_keys = false
    @passed = 0
    @failed = 0
    @secrets = [@management_key]
    validate_target!
  end

  def run
    before = management_request(:get, "/v0/management/bravo/subscriptions")
    original_claude_keys = management_request(:get, "/v0/management/claude-api-key")
    unless Array(original_claude_keys["claude-api-key"]).empty?
      raise FailoverSmokeFailure, "canary already has claude-api-key entries; refusing to replace them"
    end

    codex = Array(before["subscriptions"]).find do |item|
      item["provider"].to_s.downcase == "codex" &&
        item["enabled"] == true &&
        Array(item["primary_project_ids"]).empty?
    end
    raise FailoverSmokeFailure, "an unowned enabled Codex subscription is required" unless codex

    fault_subscriptions = install_fault_credentials(Array(before["subscriptions"]))
    stream_fault, boundary_fault = fault_subscriptions.first(2)
    raise FailoverSmokeFailure, "fault credentials did not appear" unless stream_fault && boundary_fault

    check("streaming 429 falls back before the first client payload") do
      create_project(stream_fault, codex)
      started = Time.now.utc - 1
      status, body = project_stream_request
      raise FailoverSmokeFailure, "stream returned HTTP #{status}" unless status == 200
      raise FailoverSmokeFailure, "stream has no terminal marker" unless body.include?("[DONE]")
      if body.include?("canary_fault_injected") || body.include?("Controlled canary failure")
        raise FailoverSmokeFailure, "upstream 429 leaked into the client stream"
      end
      verify_attempt_pair(started, stream_fault, codex)
    end

    check("strict pool blocks cross-project subscription escape") do
      patch_project([boundary_fault], [boundary_fault])
      started = Time.now.utc - 1
      status, body = project_non_stream_request
      raise FailoverSmokeFailure, "fault-only project unexpectedly succeeded" if status.between?(200, 299)
      raise FailoverSmokeFailure, "fault-only response was empty" if body.empty?
      verify_boundary_attempts(started, boundary_fault, codex)
    end

    finish
  rescue StandardError => error
    record_failure("controlled failover setup", error.message)
    finish
  ensure
    cleanup_project
    remove_fault_credentials
    @management_key = nil
    @project_key = nil
    @secrets.clear
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
    puts "Bravo hot failover smoke: #{@passed} passed, #{@failed} failed"
    @failed.zero? ? 0 : 1
  end

  def record_failure(name, message)
    @failed += 1
    safe = message.to_s.dup
    @secrets.compact.each { |secret| safe.gsub!(secret, "[REDACTED]") unless secret.empty? }
    safe.gsub!(/\bbrv_[A-Za-z0-9_-]{16,}\b/, "brv_[REDACTED]")
    puts "FAIL  #{name}: #{safe}"
  end

  def validate_target!
    raise FailoverSmokeFailure, "production port 18317 is refused" if @base.port == 18_317
    raise FailoverSmokeFailure, "canary port must be 18319" unless @base.port == 18_319
    raise FailoverSmokeFailure, "base URL must use http" unless @base.scheme == "http"
    fault = URI(@fault_base_url)
    raise FailoverSmokeFailure, "fault URL must use port 18991" unless fault.port == 18_991
    raise FailoverSmokeFailure, "fault URL must use http" unless fault.scheme == "http"
  end

  def read_secret(path, variable)
    stat = File.stat(path)
    raise FailoverSmokeFailure, "management env file must be mode 0600" unless (stat.mode & 0o077).zero?

    matches = File.readlines(path, chomp: true).each_with_object([]) do |line, values|
      next if line.lstrip.start_with?("#")

      name, value = line.split("=", 2)
      next unless name&.strip == variable

      values << value.to_s.strip.sub(/\A(['"])(.*)\1\z/, '\2')
    end
    raise FailoverSmokeFailure, "management variable is missing or duplicated" unless matches.length == 1
    raise FailoverSmokeFailure, "management variable is empty" if matches.first.empty?

    matches.first
  end

  def management_request(method, path, body = nil, expected: 200)
    request(method, path, body, @management_key, expected)
  end

  def request(method, path, body, key, expected)
    uri = URI.join(@base.to_s.end_with?("/") ? @base.to_s : "#{@base}/", path.sub(%r{\A/}, ""))
    request_class = {
      get: Net::HTTP::Get,
      post: Net::HTTP::Post,
      patch: Net::HTTP::Patch,
      put: Net::HTTP::Put,
      delete: Net::HTTP::Delete
    }.fetch(method)
    req = request_class.new(uri)
    if key == @management_key
      req["X-Management-Key"] = key
    else
      req["Authorization"] = "Bearer #{key}"
    end
    if body
      req["Content-Type"] = "application/json"
      req.body = JSON.generate(body)
    end
    response = Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 120) do |http|
      http.request(req)
    end
    unless Array(expected).map(&:to_i).include?(response.code.to_i)
      raise FailoverSmokeFailure, "HTTP #{response.code} for #{method.to_s.upcase} #{path}"
    end
    return {} if response.body.to_s.strip.empty?

    JSON.parse(response.body)
  rescue JSON::ParserError
    raise FailoverSmokeFailure, "non-JSON response for #{method.to_s.upcase} #{path}"
  end

  def install_fault_credentials(previous_subscriptions)
    suffix = SecureRandom.hex(8)
    keys = %w[stream boundary].map do |name|
      api_key = "canary-fault-#{name}-#{suffix}"
      @secrets << api_key
      {
        "api-key" => api_key,
        "priority" => 100_000,
        "base-url" => @fault_base_url,
        "models" => [
          {
            "name" => "claude-opus-4-8",
            "alias" => "claude-opus-4-8",
            "force-mapping" => true
          }
        ]
      }
    end
    management_request(:put, "/v0/management/claude-api-key", keys)
    @installed_fault_keys = true

    previous_indexes = previous_subscriptions.map { |item| item["auth_index"] }.to_h { |value| [value, true] }
    deadline = Time.now + 15
    loop do
      current = management_request(:get, "/v0/management/bravo/subscriptions")
      added = Array(current["subscriptions"]).select do |item|
        item["provider"].to_s.downcase == "claude" &&
          !previous_indexes.key?(item["auth_index"])
      end
      return added.sort_by { |item| item["auth_index"].to_s } if added.length >= 2
      raise FailoverSmokeFailure, "timed out waiting for fault credentials" if Time.now >= deadline

      sleep 0.25
    end
  end

  def create_project(fault_subscription, codex)
    response = management_request(
      :post,
      "/v0/management/bravo/projects",
      {
        "name" => "bravo-hot-failover-#{SecureRandom.hex(4)}",
        "enabled" => true,
        "models" => ["opus"],
        "allowed_auth_ids" => [fault_subscription.fetch("auth_index"), codex.fetch("auth_index")],
        "primary_auth_ids" => [fault_subscription.fetch("auth_index"), codex.fetch("auth_index")]
      },
      expected: 201
    )
    @project_id = response.fetch("project").fetch("id")
    @project_key = response.fetch("plaintext_key")
    @secrets << @project_key
  end

  def patch_project(allowed, primary)
    management_request(
      :patch,
      "/v0/management/bravo/projects",
      {
        "id" => @project_id,
        "name" => "bravo-hot-failover-boundary",
        "enabled" => true,
        "models" => ["opus"],
        "allowed_auth_ids" => allowed.map { |item| item.fetch("auth_index") },
        "primary_auth_ids" => primary.map { |item| item.fetch("auth_index") }
      }
    )
  end

  def request_payload(stream)
    {
      "model" => "bravo/opus",
      "messages" => [{ "role" => "user", "content" => "Reply with exactly: fallback-ok" }],
      "max_tokens" => 32,
      "stream" => stream
    }
  end

  def project_stream_request
    uri = URI.join(@base.to_s.end_with?("/") ? @base.to_s : "#{@base}/", "v1/chat/completions")
    req = Net::HTTP::Post.new(uri)
    req["Authorization"] = "Bearer #{@project_key}"
    req["Content-Type"] = "application/json"
    req["Accept"] = "text/event-stream"
    req.body = JSON.generate(request_payload(true))
    status = 0
    body = +""
    Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 120) do |http|
      http.request(req) do |response|
        status = response.code.to_i
        response.read_body { |chunk| body << chunk }
      end
    end
    [status, body]
  end

  def project_non_stream_request
    uri = URI.join(@base.to_s.end_with?("/") ? @base.to_s : "#{@base}/", "v1/chat/completions")
    req = Net::HTTP::Post.new(uri)
    req["Authorization"] = "Bearer #{@project_key}"
    req["Content-Type"] = "application/json"
    req.body = JSON.generate(request_payload(false))
    response = Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 120) do |http|
      http.request(req)
    end
    [response.code.to_i, response.body.to_s]
  end

  def recent_attempts(started)
    events = management_request(:get, "/v0/management/bravo/events")
    Array(events["events"]).select do |event|
      at = Time.parse(event["at"].to_s)
      at >= started && event["logical_model"] == "opus"
    rescue ArgumentError
      false
    end
  end

  def verify_attempt_pair(started, fault_subscription, codex)
    attempts = recent_attempts(started)
    fault_id = fault_subscription.fetch("auth_id")
    codex_id = codex.fetch("auth_id")
    failure = attempts.find { |event| event["auth_id"] == fault_id && event["success"] == false }
    success = attempts.find { |event| event["auth_id"] == codex_id && event["success"] == true }
    raise FailoverSmokeFailure, "controlled Claude failure event is missing" unless failure
    raise FailoverSmokeFailure, "controlled failure was not HTTP 429" unless failure["status"].to_i == 429
    raise FailoverSmokeFailure, "Codex fallback success event is missing" unless success
    raise FailoverSmokeFailure, "fallback event order is invalid" unless Time.parse(failure["at"]) <= Time.parse(success["at"])
  end

  def verify_boundary_attempts(started, boundary_fault, codex)
    attempts = recent_attempts(started)
    boundary_id = boundary_fault.fetch("auth_id")
    codex_id = codex.fetch("auth_id")
    raise FailoverSmokeFailure, "strict-pool Claude attempt is missing" unless attempts.any? do |event|
      event["auth_id"] == boundary_id && event["success"] == false
    end
    if attempts.any? { |event| event["auth_id"] == codex_id }
      raise FailoverSmokeFailure, "strict project pool escaped to Codex"
    end
  end

  def cleanup_project
    return unless @project_id

    management_request(:delete, "/v0/management/bravo/projects", { "id" => @project_id })
  rescue StandardError => error
    record_failure("temporary project cleanup", error.message)
  ensure
    @project_id = nil
  end

  def remove_fault_credentials
    return unless @installed_fault_keys

    management_request(:put, "/v0/management/claude-api-key", [])
  rescue StandardError => error
    record_failure("fault credential cleanup", error.message)
  ensure
    @installed_fault_keys = false
  end
end

options = {
  base_url: "http://127.0.0.1:18319",
  fault_base_url: "http://127.0.0.1:18991",
  management_env_file: "secrets.env",
  management_env_variable: "MANAGEMENT_KEY",
  confirmed: false
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-hot-failover-smoke.rb [options]"
  parser.on("--confirm-canary-mutations") { options[:confirmed] = true }
  parser.on("--base-url URL") { |value| options[:base_url] = value }
  parser.on("--fault-base-url URL") { |value| options[:fault_base_url] = value }
  parser.on("--management-env-file PATH") { |value| options[:management_env_file] = value }
  parser.on("--management-env-variable NAME") { |value| options[:management_env_variable] = value }
end.parse!

abort("pass --confirm-canary-mutations") unless options[:confirmed]
abort("unexpected positional arguments") unless ARGV.empty?

exit BravoHotFailoverSmoke.new(options).run
