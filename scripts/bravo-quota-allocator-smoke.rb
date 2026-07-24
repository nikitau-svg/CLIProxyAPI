#!/usr/bin/env ruby
# frozen_string_literal: true

# Safe quota/allocator canary smoke for Bravo.
#
# The script makes exactly two management requests:
#   GET  /v0/management/bravo/subscriptions
#   POST /v0/management/bravo/quotas/refresh
#
# It never PATCHes subscription/tariff/project policy. The refresh POST is
# restricted to the four exact auth indexes validated by the initial GET.
# Secrets are accepted only from a regular, non-symlink mode-0600 key or dotenv
# file and are never printed.
#
# Example for the MacMini canary:
#
#   ruby scripts/bravo-quota-allocator-smoke.rb \
#     --base-url http://203.0.113.10:18319 \
#     --allow-other-target \
#     --management-env-file /path/to/secrets.env
#
# Port 18317 requires the separate --allow-production-quota-refresh opt-in.

require "json"
require "net/http"
require "optparse"
require "time"
require "uri"

module BravoQuotaAllocatorSmoke
  class Failure < StandardError
    attr_reader :code

    def initialize(code, message = nil)
      @code = code.to_s
      super(message || @code)
    end
  end

  HTTPResponse = Struct.new(:status, :body)

  class SecretReader
    MAX_SECRET_FILE_BYTES = 64 * 1024

    def self.read_key(path)
      contents = read_mode_0600_file(path, "management_key_file")
      value = contents.strip
      fail_with("management_key_empty") if value.empty?
      fail_with("management_key_invalid") if value.include?("\n") || value.include?("\r")

      value
    end

    def self.read_dotenv(path, variable)
      unless variable.to_s.match?(/\A[A-Za-z_][A-Za-z0-9_]*\z/)
        fail_with("management_env_variable_invalid")
      end

      contents = read_mode_0600_file(path, "management_env_file")
      matches = []
      contents.each_line do |line|
        stripped = line.strip
        next if stripped.empty? || stripped.start_with?("#")

        assignment = line.chomp.match(
          /\A\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)\z/
        )
        next unless assignment && assignment[1] == variable

        matches << parse_dotenv_value(assignment[2], variable)
      end
      fail_with("management_env_entry_missing") if matches.empty?
      fail_with("management_env_entry_duplicate") if matches.length != 1

      value = matches.first
      fail_with("management_key_empty") if value.empty?
      if value.match?(/\$(?:[A-Za-z_]|\{)/)
        fail_with("management_env_reference_refused")
      end
      value
    end

    def self.read_mode_0600_file(path, label)
      expanded = File.expand_path(path.to_s)
      lstat = File.lstat(expanded)
      fail_with("#{label}_symlink_refused") if lstat.symlink?
      fail_with("#{label}_not_regular") unless lstat.file?
      fail_with("#{label}_permissions") unless (lstat.mode & 0o777) == 0o600
      fail_with("#{label}_too_large") if lstat.size > MAX_SECRET_FILE_BYTES

      File.read(expanded, mode: "r:BOM|UTF-8")
    rescue Errno::ENOENT, Errno::EACCES
      fail_with("#{label}_unreadable")
    end

    def self.parse_dotenv_value(raw, variable)
      value = raw.to_s.strip
      return "" if value.empty?

      case value[0]
      when "'"
        match = value.match(/\A'([^']*)'\s*(?:#.*)?\z/)
        fail_with("management_env_value_invalid") unless match
        match[1]
      when '"'
        match = value.match(/\A"((?:\\.|[^"\\])*)"\s*(?:#.*)?\z/)
        fail_with("management_env_value_invalid") unless match
        match[1].gsub(/\\(["\\])/, '\1')
      else
        value.sub(/\s+#.*\z/, "").strip
      end
    rescue ArgumentError
      fail_with("management_env_value_invalid")
    end

    def self.fail_with(code)
      raise Failure.new(code)
    end

    private_class_method :read_mode_0600_file, :parse_dotenv_value, :fail_with
  end

  class TargetPolicy
    DEFAULT_CANARY_PORT = 18_319
    PRODUCTION_PORT = 18_317
    LOOPBACK_HOSTS = %w[127.0.0.1 ::1 localhost].freeze

    def self.validate!(base_url, allow_other_target, allow_production)
      uri = URI(base_url.to_s)
      unless %w[http https].include?(uri.scheme) && !uri.host.to_s.empty?
        raise Failure.new("base_url_invalid")
      end
      if uri.user || uri.password || uri.query || uri.fragment
        raise Failure.new("base_url_components_refused")
      end
      unless ["", "/"].include?(uri.path.to_s)
        raise Failure.new("base_url_path_refused")
      end

      production = uri.port == PRODUCTION_PORT
      if production && !allow_production
        raise Failure.new("production_port_requires_explicit_opt_in")
      end

      loopback = LOOPBACK_HOSTS.include?(uri.host.to_s.downcase)
      default_canary = loopback && uri.port == DEFAULT_CANARY_PORT
      explicitly_allowed_production = production && allow_production
      unless default_canary || allow_other_target || explicitly_allowed_production
        raise Failure.new("target_requires_explicit_opt_in")
      end
      uri
    rescue URI::InvalidURIError
      raise Failure.new("base_url_invalid")
    end
  end

  class Validator
    EXPECTED_SUBSCRIPTIONS = 4
    PAST_RESET_GRACE_SECONDS = 10 * 60
    MAX_SESSION_RESET_SECONDS = 2 * 24 * 60 * 60
    MAX_LONG_RESET_SECONDS = 40 * 24 * 60 * 60
    MAX_CONFIRMED_OBSERVATION_AGE_SECONDS = 15 * 60
    FUTURE_OBSERVATION_GRACE_SECONDS = 5 * 60

    ALLOWED_CONFIDENCE = %w[confirmed unknown error].freeze
    ALLOWED_RESET_MODES = %w[scheduled inactive not_applicable].freeze
    USAGE_TOKEN_FIELDS = %w[
      input_tokens
      output_tokens
      reasoning_tokens
      cached_tokens
      cache_read_tokens
      cache_creation_tokens
      total_tokens
    ].freeze
    FORBIDDEN_EXACT_FIELDS = %w[
      token
      access_token
      refresh_token
      id_token
      oauth_token
      api_key
      key
      key_hash
      key_prefix
      secret
      password
      authorization
      cookie
      path
      file_path
      auth_path
      raw
      raw_json
      auth_json
      provider_json
      provider_payload
      provider_response
      storage_json
      metadata
      attributes
      credential
      credentials
      headers
      body
    ].freeze

    def initialize(now_proc, known_secrets)
      @now_proc = now_proc
      @known_secrets = Array(known_secrets).map(&:to_s).reject(&:empty?)
    end

    def validate_subscription_root!(root)
      require_hash!(root, "subscriptions_root_invalid")
      assert_no_leaks!(root)
      subscriptions = root["subscriptions"]
      unless subscriptions.is_a?(Array)
        fail_with("subscriptions_missing")
      end
      unless subscriptions.length == EXPECTED_SUBSCRIPTIONS
        fail_with("subscription_count_invalid")
      end

      indexes = subscriptions.map.with_index do |subscription, index|
        validate_subscription!(subscription, index)
        subscription["auth_index"].to_s.strip
      end
      if indexes.any?(&:empty?) || indexes.uniq.length != EXPECTED_SUBSCRIPTIONS
        fail_with("auth_indexes_not_unique")
      end
      validate_same_email_workspaces!(subscriptions)
      [subscriptions, indexes]
    end

    def validate_refresh_root!(root, expected_indexes)
      require_hash!(root, "refresh_root_invalid")
      assert_no_leaks!(root)
      subscriptions = root["subscriptions"]
      refreshed = root["refreshed_auth_indexes"]
      unless subscriptions.is_a?(Array) && refreshed.is_a?(Array)
        fail_with("refresh_shape_invalid")
      end

      refreshed_indexes = refreshed.map { |value| value.to_s.strip }
      if refreshed_indexes.any?(&:empty?) ||
         refreshed_indexes.uniq.length != EXPECTED_SUBSCRIPTIONS ||
         refreshed_indexes.sort != expected_indexes.sort
        fail_with("refreshed_auth_indexes_invalid")
      end

      validated_subscriptions, indexes = validate_subscription_root!(root)
      unless indexes.sort == expected_indexes.sort
        fail_with("refresh_subscription_set_changed")
      end
      validated_subscriptions
    end

    def assert_no_leaks!(value)
      inspect_for_leaks!(value, nil)
      true
    end

    private

    def validate_subscription!(subscription, index)
      require_hash!(subscription, "subscription_invalid")
      fail_with("auth_index_missing") if subscription["auth_index"].to_s.strip.empty?

      validate_code_value!(subscription["provider"], "provider_invalid")
      validate_code_value!(
        first_nonempty(subscription["effective_tariff"], subscription["tariff"]),
        "tariff_invalid"
      )

      quota = subscription["quota"]
      require_hash!(quota, "quota_invalid")
      confidence = quota["confidence"].to_s.strip.downcase
      fail_with("quota_confidence_invalid") unless ALLOWED_CONFIDENCE.include?(confidence)

      session = quota["session"]
      weekly = quota["weekly"]
      require_hash!(session, "session_window_missing")
      require_hash!(weekly, "weekly_window_missing")
      validate_window!(session, confidence, :session, index)
      validate_window!(weekly, confidence, :weekly, index)

      model_weekly = quota["model_weekly"]
      unless model_weekly.nil? || model_weekly.is_a?(Array)
        fail_with("model_weekly_invalid")
      end
      Array(model_weekly).each do |window|
        require_hash!(window, "model_weekly_window_invalid")
        validate_window!(window, confidence, :model_weekly, index)
      end

      validate_observed_at!(quota["observed_at"]) if confidence == "confirmed"
    end

    def validate_window!(window, confidence, kind, _index)
      unless window.key?("used_percent") && window.key?("remaining_percent")
        fail_with("quota_percentage_fields_missing")
      end
      used = window["used_percent"]
      remaining = window["remaining_percent"]
      if confidence != "confirmed"
        unless used.nil? && remaining.nil?
          fail_with("unknown_quota_percentage_not_null")
        end
        unless window["reset_at"].nil?
          fail_with("unknown_quota_reset_not_null")
        end
        return
      end

      validate_percent!(used)
      validate_percent!(remaining)
      unless ((used.to_f + remaining.to_f) - 100.0).abs <= 0.05
        fail_with("quota_percentages_inconsistent")
      end
      reset_mode = window["reset_mode"].to_s.strip.downcase
      unless ALLOWED_RESET_MODES.include?(reset_mode)
        fail_with("quota_reset_mode_invalid")
      end
      case reset_mode
      when "scheduled"
        validate_reset!(window["reset_at"], kind)
      when "inactive", "not_applicable"
        unless used.to_f == 0.0 &&
               remaining.to_f == 100.0 &&
               window["reset_at"].nil?
          fail_with("quota_resetless_window_invalid")
        end
      end
    end

    def validate_percent!(value)
      unless value.is_a?(Numeric) &&
             (!value.respond_to?(:finite?) || value.finite?) &&
             value.to_f >= 0.0 &&
             value.to_f <= 100.0
        fail_with("quota_percentage_invalid")
      end
    end

    def validate_reset!(value, kind)
      reset_at = parse_time(value, "quota_reset_invalid")
      now = @now_proc.call.utc
      if reset_at < now - PAST_RESET_GRACE_SECONDS
        fail_with("quota_reset_stale")
      end
      max_seconds =
        kind == :session ? MAX_SESSION_RESET_SECONDS : MAX_LONG_RESET_SECONDS
      if reset_at > now + max_seconds
        fail_with("quota_reset_implausible")
      end
    end

    def validate_observed_at!(value)
      observed_at = parse_time(value, "quota_observed_at_invalid")
      now = @now_proc.call.utc
      if observed_at < now - MAX_CONFIRMED_OBSERVATION_AGE_SECONDS
        fail_with("quota_observation_stale")
      end
      if observed_at > now + FUTURE_OBSERVATION_GRACE_SECONDS
        fail_with("quota_observation_from_future")
      end
    end

    def validate_same_email_workspaces!(subscriptions)
      groups = {}
      subscriptions.each do |subscription|
        email = subscription["email"].to_s.strip.downcase
        next if email.empty?

        groups[email] ||= []
        groups[email] << subscription
      end
      groups.each_value do |same_email|
        next unless same_email.length > 1

        workspaces = same_email.map { |item| item["workspace"].to_s.strip }
                               .reject(&:empty?)
        next unless workspaces.length > 1

        if workspaces.uniq.length != workspaces.length
          fail_with("same_email_workspace_collision")
        end
      end
    end

    def inspect_for_leaks!(value, parent_key)
      case value
      when Hash
        value.each do |key, child|
          normalized = key.to_s.strip.downcase
          fail_with("forbidden_response_field") if forbidden_field?(normalized)
          inspect_for_leaks!(child, normalized)
        end
      when Array
        value.each { |child| inspect_for_leaks!(child, parent_key) }
      when String
        @known_secrets.each do |secret|
          fail_with("known_secret_leaked") if value.include?(secret)
        end
        if value.match?(/\bBearer\s+\S+/i) ||
           value.match?(/\b(?:sk-|brv_)[A-Za-z0-9_-]{12,}\b/) ||
           value.match?(/\AeyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\z/) ||
           value.match?(%r{\A/(?:Users|home|private|var|etc|tmp)/}) ||
           value.match?(/\A[A-Za-z]:\\/)
          fail_with("secret_or_path_value_leaked")
        end
        stripped = value.strip
        if (stripped.start_with?("{") && stripped.end_with?("}")) ||
           (stripped.start_with?("[") && stripped.end_with?("]"))
          fail_with("raw_json_value_leaked")
        end
      end
    end

    def forbidden_field?(key)
      return false if USAGE_TOKEN_FIELDS.include?(key)
      return true if FORBIDDEN_EXACT_FIELDS.include?(key)
      return true if key.end_with?("_path") || key.start_with?("path_")
      return true if key.include?("secret") || key.include?("password")
      return true if key.include?("api_key") || key.include?("oauth_token")
      return true if key.include?("access_token") || key.include?("refresh_token")
      return true if key.include?("provider_json") || key.include?("raw_json")
      return true if key.include?("token") && !key.end_with?("_tokens")

      false
    end

    def parse_time(value, code)
      fail_with(code) if value.nil? || value.to_s.strip.empty?
      Time.iso8601(value.to_s).utc
    rescue ArgumentError
      fail_with(code)
    end

    def validate_code_value!(value, code)
      text = value.to_s.strip.downcase
      unless text.match?(/\A[a-z0-9][a-z0-9_.-]{0,47}\z/)
        fail_with(code)
      end
      text
    end

    def require_hash!(value, code)
      fail_with(code) unless value.is_a?(Hash)
      value
    end

    def first_nonempty(*values)
      values.each do |value|
        text = value.to_s.strip
        return text unless text.empty?
      end
      ""
    end

    def fail_with(code)
      raise Failure.new(code)
    end
  end

  class Runner
    SUBSCRIPTIONS_PATH = "/v0/management/bravo/subscriptions"
    QUOTA_REFRESH_PATH = "/v0/management/bravo/quotas/refresh"
    MAX_RESPONSE_BYTES = 2 * 1024 * 1024
    ALLOWED_REQUESTS = [
      [:get, SUBSCRIPTIONS_PATH],
      [:post, QUOTA_REFRESH_PATH]
    ].freeze

    def initialize(options, transport = nil, now_proc = nil)
      @base_uri = TargetPolicy.validate!(
        options.fetch(:base_url),
        options.fetch(:allow_other_target),
        options.fetch(:allow_production_quota_refresh)
      )
      @timeout = options.fetch(:timeout)
      unless @timeout.is_a?(Integer) && @timeout.positive? && @timeout <= 300
        raise Failure.new("timeout_invalid")
      end
      @management_key = read_management_key(options)
      @transport = transport
      @now_proc = now_proc || lambda { Time.now }
      @validator = Validator.new(@now_proc, [@management_key])
    end

    def run
      initial_root = success_json(
        management_request(:get, SUBSCRIPTIONS_PATH),
        "subscriptions_get_failed"
      )
      _initial_subscriptions, indexes =
        @validator.validate_subscription_root!(initial_root)

      refreshed_root = success_json(
        management_request(
          :post,
          QUOTA_REFRESH_PATH,
          { "auth_indexes" => indexes }
        ),
        "quota_refresh_failed"
      )
      subscriptions =
        @validator.validate_refresh_root!(refreshed_root, indexes)
      print_sanitized_summary(subscriptions)
      0
    ensure
      @management_key = nil
    end

    private

    def read_management_key(options)
      key_file = options[:management_key_file].to_s.strip
      unless key_file.empty?
        return SecretReader.read_key(key_file)
      end
      SecretReader.read_dotenv(
        options.fetch(:management_env_file),
        options.fetch(:management_env_variable).to_s.strip
      )
    end

    def management_request(method, path, payload = nil)
      unless ALLOWED_REQUESTS.include?([method, path])
        raise Failure.new("request_not_allowlisted")
      end
      headers = {
        "Accept" => "application/json",
        "X-Management-Key" => @management_key
      }
      if @transport
        return @transport.call(method, path, payload, headers)
      end

      uri = @base_uri.dup
      uri.path = path
      uri.query = nil
      request_class = method == :get ? Net::HTTP::Get : Net::HTTP::Post
      request = request_class.new(uri)
      headers.each { |name, value| request[name] = value }
      if payload
        request["Content-Type"] = "application/json"
        request.body = JSON.generate(payload)
      end

      body = +""
      status = nil
      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = uri.scheme == "https"
      http.open_timeout = [@timeout, 10].min
      http.read_timeout = @timeout
      http.write_timeout = @timeout if http.respond_to?(:write_timeout=)
      http.start do |client|
        client.request(request) do |response|
          status = response.code.to_i
          response.read_body do |chunk|
            body << chunk
            if body.bytesize > MAX_RESPONSE_BYTES
              raise Failure.new("response_too_large")
            end
          end
        end
      end
      HTTPResponse.new(status, body)
    rescue Failure
      raise
    rescue StandardError
      raise Failure.new("management_request_failed")
    end

    def success_json(response, failure_code)
      unless response.respond_to?(:status) && response.status.to_i.between?(200, 299)
        status = response.respond_to?(:status) ? response.status.to_i : 0
        code = status.positive? ? "http_#{status}" : failure_code
        raise Failure.new(code)
      end
      if response.body.to_s.include?(@management_key)
        raise Failure.new("management_key_leaked")
      end
      root = JSON.parse(response.body.to_s)
      unless root.is_a?(Hash)
        raise Failure.new("response_root_invalid")
      end
      root
    rescue JSON::ParserError
      raise Failure.new("response_json_invalid")
    end

    def print_sanitized_summary(subscriptions)
      subscriptions.each_with_index do |subscription, index|
        quota = subscription.fetch("quota")
        confidence = safe_code(quota["confidence"])
        provider = safe_code(subscription["provider"])
        tariff = safe_code(
          first_nonempty(
            subscription["effective_tariff"],
            subscription["tariff"]
          )
        )
        session = quota.fetch("session")
        weekly = quota.fetch("weekly")
        error_code =
          if !quota["error"].to_s.strip.empty? || confidence == "error"
            "quota_error"
          elsif confidence == "unknown"
            "quota_unknown"
          else
            "none"
          end
        puts [
          "subscription=#{index + 1}",
          "provider=#{provider}",
          "tariff=#{tariff}",
          "confidence=#{confidence}",
          "session_remaining=#{format_percent(session["remaining_percent"])}",
          "session_reset_mode=#{format_reset_mode(session, confidence)}",
          "session_reset=#{format_reset(session, confidence)}",
          "weekly_remaining=#{format_percent(weekly["remaining_percent"])}",
          "weekly_reset_mode=#{format_reset_mode(weekly, confidence)}",
          "weekly_reset=#{format_reset(weekly, confidence)}",
          "error_code=#{error_code}"
        ].join(" ")
      end
      puts(
        "quota_allocator_smoke=ok " \
        "subscriptions=#{subscriptions.length} refreshed=#{subscriptions.length}"
      )
    end

    def safe_code(value)
      text = value.to_s.strip.downcase
      unless text.match?(/\A[a-z0-9][a-z0-9_.-]{0,47}\z/)
        raise Failure.new("sanitized_output_value_invalid")
      end
      text
    end

    def format_percent(value)
      return "null" if value.nil?

      format("%.2f", value.to_f)
    end

    def format_reset_mode(window, confidence)
      return "unknown" unless confidence == "confirmed"

      safe_code(window["reset_mode"])
    end

    def format_reset(window, confidence)
      return "null" unless confidence == "confirmed"
      return "null" unless window["reset_mode"].to_s == "scheduled"

      Time.iso8601(window["reset_at"].to_s).utc.iso8601
    rescue ArgumentError
      raise Failure.new("sanitized_output_reset_invalid")
    end

    def first_nonempty(*values)
      values.each do |value|
        text = value.to_s.strip
        return text unless text.empty?
      end
      ""
    end
  end

  class CLI
    def self.run(argv, environment)
      options = {
        base_url: environment.fetch(
          "BRAVO_BASE_URL",
          "http://127.0.0.1:18319"
        ),
        management_key_file: environment["BRAVO_MANAGEMENT_KEY_FILE"],
        management_env_file: environment.fetch(
          "BRAVO_MANAGEMENT_ENV_FILE",
          "secrets.env"
        ),
        management_env_variable: environment.fetch(
          "BRAVO_MANAGEMENT_ENV_VARIABLE",
          "MANAGEMENT_PASSWORD"
        ),
        timeout: Integer(
          environment.fetch("BRAVO_QUOTA_SMOKE_TIMEOUT", "45"),
          10
        ),
        allow_other_target: false,
        allow_production_quota_refresh: false
      }

      parser = OptionParser.new do |opts|
        opts.banner = "Usage: ruby scripts/bravo-quota-allocator-smoke.rb [options]"
        opts.on("--base-url URL", "Bravo base URL") do |value|
          options[:base_url] = value
        end
        opts.on(
          "--management-key-file PATH",
          "Exact mode-0600 file containing only the management key"
        ) do |value|
          options[:management_key_file] = value
        end
        opts.on(
          "--management-env-file PATH",
          "Exact mode-0600 dotenv file containing the management key"
        ) do |value|
          options[:management_key_file] = nil
          options[:management_env_file] = value
        end
        opts.on(
          "--management-env-variable NAME",
          "Exact dotenv variable containing the management key"
        ) do |value|
          options[:management_env_variable] = value
        end
        opts.on("--timeout SECONDS", Integer, "Per-request timeout (max 300)") do |value|
          options[:timeout] = value
        end
        opts.on(
          "--allow-other-target",
          "Allow a verified target other than loopback:18319"
        ) do
          options[:allow_other_target] = true
        end
        opts.on(
          "--allow-production-quota-refresh",
          "Explicitly allow port 18317; only the quota refresh POST remains enabled"
        ) do
          options[:allow_production_quota_refresh] = true
        end
        opts.on("-h", "--help", "Show this help") do
          puts opts
          return 0
        end
      end
      parser.parse!(argv)
      raise Failure.new("unexpected_arguments") unless argv.empty?

      Runner.new(options).run
    rescue Failure => error
      warn "quota_allocator_smoke=fail error_code=#{safe_error_code(error.code)}"
      1
    rescue OptionParser::ParseError, ArgumentError
      warn "quota_allocator_smoke=fail error_code=cli_invalid"
      2
    end

    def self.safe_error_code(value)
      text = value.to_s.strip.downcase
      return "smoke_failed" unless text.match?(/\A[a-z0-9][a-z0-9_]{0,63}\z/)

      text
    end

    private_class_method :safe_error_code
  end
end

if $PROGRAM_NAME == __FILE__
  exit BravoQuotaAllocatorSmoke::CLI.run(ARGV, ENV)
end
