#!/usr/bin/env ruby
# frozen_string_literal: true

# Mutating end-to-end smoke test for the Bravo canary project API and named
# effort contract.
#
# The script never accepts secret values on the command line. It reads the
# management password from an explicitly named entry in a mode-0600 dotenv
# file (or from a dedicated mode-0600 key file), and reads the existing
# default Bravo key from another mode-0600 file. It keeps generated project
# keys in memory and redacts all known secret formats from failures. It creates
# one uniquely named temporary project and deletes only the ID returned by that
# create request.
#
# Run this on the canary host so the safe default target remains loopback:
#
#   ruby scripts/bravo-management-smoke.rb \
#     --confirm-canary-mutations \
#     --management-env-file /path/to/secrets.env \
#     --smart-key-file /path/to/bravo-smart-key.txt
#
# The known production port 18317 is refused even with --allow-other-target.

require "digest"
require "json"
require "net/http"
require "optparse"
require "securerandom"
require "time"
require "uri"

SmokeResponse = Struct.new(:status, :headers, :body)
SmokeFailure = Class.new(StandardError)

class BravoManagementSmoke
  DEFAULT_BASE_URL = "http://127.0.0.1:18319"
  DEFAULT_MODEL = "bravo/haiku"
  PRODUCTION_PORT = 18_317
  CANARY_PORT = 18_319
  MAX_RESPONSE_BYTES = 2 * 1024 * 1024
  MARKER = "BRAVO_MANAGEMENT_SMOKE_OK"
  VALID_EFFECTS = %w[low medium high xhigh max].freeze

  def initialize(options)
    @base_uri = URI(options.fetch(:base_url))
    @timeout = options.fetch(:timeout)
    @allow_other_target = options.fetch(:allow_other_target)
    @confirmed = options.fetch(:confirm_canary_mutations)
    @requested_model = options.fetch(:model)
    validate_target!

    @management_key = read_management_secret(options)
    @default_key = read_secret_file(
      options.fetch(:smart_key_file),
      "default Bravo key"
    )
    @secrets = [@management_key, @default_key]
    @results = []
    @project_id = nil
    @deleted_project_id = nil
    @project_key = nil
    @rotated_key = nil
    @logical_name = nil
    @logical_model = nil
    @initial_project_ids = []
    @default_preflight_ok = false
  end

  def run
    begin
      run_workflow
    rescue StandardError => error
      record_failure("unexpected smoke error", "#{error.class}: #{error.message}")
    ensure
      cleanup_temporary_project
    end

    if @default_preflight_ok
      check("existing default key still works") do
        assert_smart_key_active(@default_key)
      end
    end

    passed = @results.count { |item| item[:status] == :pass }
    failed = @results.count { |item| item[:status] == :fail }
    puts
    puts "Bravo management smoke: #{passed} passed, #{failed} failed"
    failed.zero? ? 0 : 1
  ensure
    @management_key = nil
    @default_key = nil
    @project_key = nil
    @rotated_key = nil
    @secrets = []
  end

  private

  def run_workflow
    ready = check("management auth and model catalog") { preflight }
    return unless ready

    @default_preflight_ok = check("existing default key works") do
      assert_smart_key_active(@default_key)
    end
    return unless @default_preflight_ok

    created = check("create isolated temporary project") { create_project }
    return unless created

    check("list redacts project key material") { list_created_project }
    check("unsupported project cache TTL fails closed") do
      assert_invalid_prompt_cache_ttl
    end

    disabled = check("patch project to disabled") { disable_project }
    if disabled
      check("disabled project key is denied") do
        assert_smart_key_denied(@project_key)
      end
    end

    enabled = check("patch project back to active") { enable_project }
    return unless enabled

    check("re-enabled project key works") do
      assert_smart_key_active(@project_key)
    end
    run_effort_checks

    rotated = check("rotate project key") { rotate_project }
    if rotated
      check("pre-rotation key is denied") do
        assert_smart_key_denied(@project_key)
      end
      check("rotated project key works") do
        assert_smart_key_active(@rotated_key)
      end
    end

    deleted = check("delete temporary project") { delete_project }
    return unless deleted

    check("deleted project key is denied") do
      assert_smart_key_denied(@rotated_key || @project_key)
    end
    check("deleted project is absent from list") { assert_project_absent }
  end

  def run_effort_checks
    check("OpenAI Chat explicit effort") do
      payload = {
        "model" => @logical_model,
        "messages" => [{
          "role" => "user",
          "content" => "Reply with exactly #{MARKER}."
        }],
        "reasoning_effort" => "low",
        "max_tokens" => 4096
      }
      assert_effort_request(
        "/v1/chat/completions",
        payload,
        @project_key,
        "low",
        :bearer
      ) do |root|
        assert(root["model"] == @logical_model, "logical model was not preserved")
        assert(Array(root["choices"]).any?, "OpenAI Chat returned no choices")
      end
    end

    check("OpenAI Responses explicit effort") do
      payload = {
        "model" => @logical_model,
        "input" => "Reply with exactly #{MARKER}.",
        "reasoning" => { "effort" => "low" },
        "max_output_tokens" => 4096
      }
      assert_effort_request(
        "/v1/responses",
        payload,
        @project_key,
        "low",
        :bearer
      ) do |root|
        assert(root["model"] == @logical_model, "logical model was not preserved")
        assert(Array(root["output"]).any?, "OpenAI Responses returned no output")
      end
    end

    check("Anthropic Messages explicit effort") do
      payload = {
        "model" => @logical_model,
        "messages" => [{
          "role" => "user",
          "content" => "Reply with exactly #{MARKER}."
        }],
        "thinking" => { "type" => "adaptive" },
        "output_config" => { "effort" => "low" },
        "max_tokens" => 4096
      }
      assert_effort_request(
        "/v1/messages",
        payload,
        @project_key,
        "low",
        :x_api_key
      ) do |root|
        assert(root["model"] == @logical_model, "logical model was not preserved")
        assert(Array(root["content"]).any?, "Anthropic Messages returned no content")
      end
    end

    check("OpenAI invalid effort fails closed") do
      payload = {
        "model" => @logical_model,
        "messages" => [{ "role" => "user", "content" => "Do not execute." }],
        "reasoning_effort" => "turbo",
        "max_tokens" => 16
      }
      response = inference_request(
        "/v1/chat/completions",
        payload,
        @project_key,
        :bearer
      )
      assert_contract_failure(response, "bravo_effort_invalid")
    end

    check("Anthropic manual budget fails closed") do
      payload = {
        "model" => @logical_model,
        "messages" => [{ "role" => "user", "content" => "Do not execute." }],
        "thinking" => { "type" => "enabled", "budget_tokens" => 1024 },
        "max_tokens" => 2048
      }
      response = inference_request(
        "/v1/messages",
        payload,
        @project_key,
        :x_api_key
      )
      assert_contract_failure(response, "bravo_contract_unverified")
    end
  end

  def preflight
    response = management_request(:get, "/v0/management/bravo/projects")
    root = success_json(response)
    projects = Array(root["projects"])
    models = Array(root["models"])
    assert_redacted_listing(response, [])

    @initial_project_ids = projects.map do |project|
      project["id"].to_s if project.is_a?(Hash)
    end.compact
    option = models.find do |item|
      next false unless item.is_a?(Hash)

      [item["id"], item["request_model"]].compact.any? do |value|
        value.to_s.casecmp?(@requested_model)
      end
    end
    assert(
      option,
      "requested model #{@requested_model.inspect} is absent from the Bravo project catalog"
    )
    @logical_name = option.fetch("id").to_s
    @logical_model = option.fetch("request_model").to_s
    assert(!@logical_name.empty?, "catalog returned an empty logical model ID")
    assert(!@logical_model.empty?, "catalog returned an empty request_model")
  end

  def create_project
    project_name = [
      "bravo-smoke",
      Time.now.utc.strftime("%Y%m%dT%H%M%SZ"),
      Process.pid,
      SecureRandom.hex(4)
    ].join("-")
    payload = {
      "name" => project_name,
      "models" => [@logical_name],
      "primary_auth_ids" => [],
      "prompt_cache" => { "anthropic_ttl" => "1h" },
      "policy" => {
        "smoke_test" => true,
        "created_by" => "scripts/bravo-management-smoke.rb"
      }
    }
    response = management_request(
      :post,
      "/v0/management/bravo/projects",
      payload
    )
    root = success_json(response, 201)
    project = require_object(root["project"], "create response project")
    @project_key = root["plaintext_key"].to_s
    track_secret(@project_key)
    @project_id = project["id"].to_s

    assert(@project_key.start_with?("brv_"), "create did not return a Bravo key")
    assert(@project_id.start_with?("prj_"), "create did not return a project ID")
    assert(
      !@initial_project_ids.include?(@project_id),
      "create reused an existing project ID"
    )
    assert(project["name"] == project_name, "created project name differs")
    assert(project["enabled"] == true, "created project is not enabled")
    assert(project["status"] == "active", "created project is not active")
    assert(Array(project["models"]).include?(@logical_name), "model allowlist differs")
    assert_prompt_cache(project, "1h")
    assert_redacted_project(project)
  end

  def list_created_project
    response = management_request(:get, "/v0/management/bravo/projects")
    root = success_json(response)
    assert_redacted_listing(response, [@project_key])
    project = Array(root["projects"]).find do |item|
      item.is_a?(Hash) && item["id"] == @project_id
    end
    assert(project, "created project is missing from list")
    assert_prompt_cache(project, "1h")
    assert_redacted_project(project)
  end

  def assert_invalid_prompt_cache_ttl
    response = management_request(
      :patch,
      "/v0/management/bravo/projects",
      {
        "id" => @project_id,
        "prompt_cache" => { "anthropic_ttl" => "24h" }
      }
    )
    assert(response.status == 400, http_failure(response, "expected HTTP 400"))
    error = require_object(parse_json(response)["error"], "invalid cache error")
    assert(
      error["code"] == "bravo_project_prompt_cache_invalid",
      "unsupported cache TTL returned #{error["code"].inspect}"
    )
  end

  def disable_project
    response = management_request(
      :patch,
      "/v0/management/bravo/projects",
      {
        "id" => @project_id,
        "name" => temporary_project_name("disabled"),
        "enabled" => false,
        "prompt_cache" => { "anthropic_ttl" => "5m" },
        "policy" => {
          "smoke_test" => true,
          "state" => "disabled"
        }
      }
    )
    project = require_object(success_json(response)["project"], "patched project")
    assert(project["id"] == @project_id, "patch changed the project ID")
    assert(project["enabled"] == false, "patch did not disable the project")
    assert(project["status"] == "disabled", "disabled status was not persisted")
    assert_prompt_cache(project, "5m")
    assert_redacted_project(project)
  end

  def enable_project
    response = management_request(
      :patch,
      "/v0/management/bravo/projects",
      {
        "id" => @project_id,
        "name" => temporary_project_name("active"),
        "enabled" => true,
        "prompt_cache" => { "anthropic_ttl" => "1h" },
        "policy" => {
          "smoke_test" => true,
          "state" => "active"
        }
      }
    )
    project = require_object(success_json(response)["project"], "patched project")
    assert(project["id"] == @project_id, "patch changed the project ID")
    assert(project["enabled"] == true, "patch did not enable the project")
    assert(project["status"] == "active", "active status was not persisted")
    assert_prompt_cache(project, "1h")
    assert_redacted_project(project)
  end

  def rotate_project
    response = management_request(
      :post,
      "/v0/management/bravo/projects/rotate",
      { "id" => @project_id }
    )
    root = success_json(response)
    project = require_object(root["project"], "rotated project")
    @rotated_key = root["plaintext_key"].to_s
    track_secret(@rotated_key)

    assert(project["id"] == @project_id, "rotation changed the project ID")
    assert(@rotated_key.start_with?("brv_"), "rotation did not return a Bravo key")
    assert(@rotated_key != @project_key, "rotation returned the old key")
    assert_prompt_cache(project, "1h")
    assert_redacted_project(project)

    listed = management_request(:get, "/v0/management/bravo/projects")
    success_json(listed)
    assert_redacted_listing(listed, [@project_key, @rotated_key])
  end

  def delete_project
    deleted_id = @project_id
    response = management_request(
      :delete,
      "/v0/management/bravo/projects",
      { "id" => deleted_id }
    )
    root = success_json(response)
    assert(root["deleted"] == true, "delete response did not confirm deletion")
    assert(root["id"] == deleted_id, "delete response returned another project ID")
    @deleted_project_id = deleted_id
    @project_id = nil
  end

  def assert_project_absent
    response = management_request(:get, "/v0/management/bravo/projects")
    root = success_json(response)
    assert_redacted_listing(response, [@project_key, @rotated_key].compact)
    found = Array(root["projects"]).any? do |item|
      item.is_a?(Hash) && item["id"] == @deleted_project_id
    end
    assert(!found, "deleted project is still listed")
  end

  def cleanup_temporary_project
    return if @project_id.nil? || @project_id.empty?

    cleanup_id = @project_id
    started = monotonic
    response = management_request(
      :delete,
      "/v0/management/bravo/projects",
      { "id" => cleanup_id }
    )
    root = success_json(response)
    assert(root["deleted"] == true, "cleanup did not confirm deletion")
    assert(root["id"] == cleanup_id, "cleanup deleted an unexpected project")
    @project_id = nil
    record_pass("emergency cleanup of temporary project", started)
  rescue StandardError => error
    record_failure(
      "emergency cleanup of temporary project",
      "manual cleanup may be required for temporary project #{cleanup_id}: " \
      "#{error.class}: #{error.message}"
    )
  end

  def assert_effort_request(path, payload, key, requested, auth_style)
    started_at = Time.now.utc - 2
    response = inference_request(path, payload, key, auth_style)
    root = success_json(response)
    yield root

    events_response = management_request(:get, "/v0/management/bravo/events")
    events = Array(success_json(events_response)["events"])
    event = events.find do |item|
      next false unless item.is_a?(Hash)
      next false unless item["logical_model"].to_s == @logical_name
      next false unless item["requested_effort"].to_s == requested
      next false unless item["success"] == true

      event_time = parse_time(item["at"])
      event_time && event_time >= started_at
    end
    assert(event, "no matching successful Bravo effort event was recorded")
    effective = event["effective_effort"].to_s
    assert(
      VALID_EFFECTS.include?(effective),
      "event returned an invalid effective effort #{effective.inspect}"
    )
    assert(event["effort"].to_s == effective, "event effort metadata is inconsistent")
  end

  def assert_contract_failure(response, expected_code)
    assert(
      response.status == 422,
      "expected fail-closed HTTP 422, got HTTP #{response.status}"
    )
    root = parse_json(response)
    serialized = JSON.generate(root)
    assert(
      serialized.include?(expected_code),
      "error response omitted #{expected_code}"
    )
  end

  def assert_smart_key_active(key)
    response = inference_request("/v1/models", nil, key, :bearer, :get)
    root = success_json(response)
    ids = Array(root["data"]).map do |item|
      item["id"].to_s if item.is_a?(Hash)
    end.compact
    assert(ids.include?(@logical_model), "#{@logical_model} is absent from /v1/models")
  end

  def assert_smart_key_denied(key)
    assert(key && !key.empty?, "project key is unavailable")
    response = inference_request("/v1/models", nil, key, :bearer, :get)
    assert(
      [401, 403].include?(response.status),
      "expected key denial, got HTTP #{response.status}"
    )
  end

  def temporary_project_name(state)
    suffix = @project_id.to_s.delete_prefix("prj_")[0, 12]
    "bravo-smoke-#{state}-#{suffix}"
  end

  def assert_redacted_listing(response, plaintext_keys)
    root = parse_json(response)
    serialized = JSON.generate(root)
    assert(!serialized.include?('"sha256"'), "project list exposed sha256")
    assert(
      !serialized.include?('"plaintext_key"'),
      "project list exposed a plaintext_key field"
    )
    plaintext_keys.each do |key|
      next if key.nil? || key.empty?

      assert(!serialized.include?(key), "project list exposed a plaintext key")
      digest = Digest::SHA256.hexdigest(key)
      assert(!serialized.include?(digest), "project list exposed a key digest")
    end
  end

  def assert_redacted_project(project)
    serialized = JSON.generate(project)
    %w[sha256 plaintext_key key secret token].each do |field|
      assert(!project.key?(field), "project object exposed #{field}")
    end
    [@project_key, @rotated_key].compact.each do |key|
      assert(!serialized.include?(key), "project object exposed key material")
      assert(
        !serialized.include?(Digest::SHA256.hexdigest(key)),
        "project object exposed a key digest"
      )
    end
  end

  def assert_prompt_cache(project, expected_ttl)
    cache = require_object(project["prompt_cache"], "project prompt_cache")
    assert(
      cache["anthropic_ttl"] == expected_ttl,
      "Claude cache TTL #{cache["anthropic_ttl"].inspect} differs from #{expected_ttl.inspect}"
    )
    assert(
      cache["openai_mode"] == "provider_managed",
      "OpenAI cache mode is not provider_managed"
    )
  end

  def management_request(method, path, payload = nil)
    request(
      method,
      path,
      payload,
      { "X-Management-Key" => @management_key }
    )
  end

  def inference_request(path, payload, key, auth_style, method = :post)
    headers =
      if auth_style == :x_api_key
        {
          "x-api-key" => key,
          "anthropic-version" => "2023-06-01"
        }
      else
        { "Authorization" => "Bearer #{key}" }
      end
    request(method, path, payload, headers)
  end

  def request(method, path, payload, headers)
    uri = @base_uri.dup
    uri.path = path
    uri.query = nil
    request_class = {
      get: Net::HTTP::Get,
      post: Net::HTTP::Post,
      patch: Net::HTTP::Patch,
      delete: Net::HTTP::Delete
    }.fetch(method)
    http_request = request_class.new(uri)
    http_request["Accept"] = "application/json"
    headers.each { |name, value| http_request[name] = value }
    if payload
      http_request["Content-Type"] = "application/json"
      http_request.body = JSON.generate(payload)
    end

    body = +""
    status = nil
    response_headers = nil
    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = uri.scheme == "https"
    http.open_timeout = [@timeout, 10].min
    http.read_timeout = @timeout
    http.write_timeout = @timeout if http.respond_to?(:write_timeout=)
    http.start do |client|
      client.request(http_request) do |response|
        status = response.code.to_i
        response_headers = response.to_hash
        response.read_body do |chunk|
          body << chunk
          if body.bytesize > MAX_RESPONSE_BYTES
            raise SmokeFailure, "response exceeded #{MAX_RESPONSE_BYTES} bytes"
          end
        end
      end
    end
    SmokeResponse.new(status, response_headers, body)
  end

  def success_json(response, expected_status = nil)
    if expected_status
      assert(
        response.status == expected_status,
        http_failure(response, "expected HTTP #{expected_status}")
      )
    else
      assert(response.status.between?(200, 299), http_failure(response))
    end
    parse_json(response)
  end

  def parse_json(response)
    JSON.parse(response.body)
  rescue JSON::ParserError => error
    raise SmokeFailure, "invalid JSON response: #{error.message}"
  end

  def require_object(value, label)
    assert(value.is_a?(Hash), "#{label} is not an object")
    value
  end

  def parse_time(value)
    Time.iso8601(value.to_s)
  rescue ArgumentError
    nil
  end

  def http_failure(response, prefix = nil)
    detail = begin
      root = JSON.parse(response.body)
      error = root["error"]
      error.is_a?(Hash) ? JSON.generate(error) : error.to_s
    rescue JSON::ParserError
      response.body.to_s
    end
    detail = detail.gsub(/\s+/, " ").strip.byteslice(0, 400)
    message = [prefix, "HTTP #{response.status}"].compact.join(": ")
    detail.empty? ? message : "#{message}: #{detail}"
  end

  def read_secret_file(path, label)
    expanded = File.expand_path(path)
    stat = File.stat(expanded)
    raise SmokeFailure, "#{label} path is not a regular file" unless stat.file?
    if (stat.mode & 0o077) != 0
      raise SmokeFailure, "#{label} file must not be group/world accessible (use chmod 600)"
    end

    value = File.read(expanded, mode: "r:BOM|UTF-8").strip
    raise SmokeFailure, "#{label} file is empty" if value.empty?

    value
  rescue Errno::ENOENT, Errno::EACCES => error
    raise SmokeFailure, "#{label} file cannot be read: #{error.class}"
  end

  def read_management_secret(options)
    key_file = options[:management_key_file].to_s.strip
    unless key_file.empty?
      return read_secret_file(key_file, "management key")
    end

    env_file = options.fetch(:management_env_file)
    variable = options.fetch(:management_env_variable).to_s.strip
    unless variable.match?(/\A[A-Za-z_][A-Za-z0-9_]*\z/)
      raise SmokeFailure, "management env variable name is invalid"
    end
    read_dotenv_secret(env_file, variable)
  end

  def read_dotenv_secret(path, variable)
    expanded = File.expand_path(path)
    stat = File.stat(expanded)
    raise SmokeFailure, "management env path is not a regular file" unless stat.file?
    if (stat.mode & 0o077) != 0
      raise SmokeFailure,
            "management env file must not be group/world accessible (use chmod 600)"
    end

    matches = []
    File.read(expanded, mode: "r:BOM|UTF-8").each_line do |line|
      stripped = line.strip
      next if stripped.empty? || stripped.start_with?("#")

      assignment = line.chomp.match(
        /\A\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)\z/
      )
      next unless assignment && assignment[1] == variable

      matches << parse_dotenv_value(assignment[2], variable)
    end
    if matches.empty?
      raise SmokeFailure, "management env file has no #{variable} entry"
    end
    if matches.length > 1
      raise SmokeFailure, "management env file contains duplicate #{variable} entries"
    end

    value = matches.first
    raise SmokeFailure, "#{variable} in management env file is empty" if value.empty?
    if value.match?(/\$(?:[A-Za-z_]|\{)/)
      raise SmokeFailure,
            "#{variable} must be a literal value, not a shell/dotenv reference"
    end

    value
  rescue Errno::ENOENT, Errno::EACCES => error
    raise SmokeFailure, "management env file cannot be read: #{error.class}"
  end

  def parse_dotenv_value(raw, variable)
    value = raw.to_s.strip
    return "" if value.empty?

    case value[0]
    when "'"
      match = value.match(/\A'([^']*)'\s*(?:#.*)?\z/)
      unless match
        raise SmokeFailure, "#{variable} has an invalid single-quoted dotenv value"
      end
      match[1]
    when '"'
      match = value.match(/\A"((?:\\.|[^"\\])*)"\s*(?:#.*)?\z/)
      unless match
        raise SmokeFailure, "#{variable} has an invalid double-quoted dotenv value"
      end
      match[1].gsub(/\\(["\\])/, '\1')
    else
      value.sub(/\s+#.*\z/, "").strip
    end
  end

  def validate_target!
    raise SmokeFailure, "--confirm-canary-mutations is required" unless @confirmed
    unless %w[http https].include?(@base_uri.scheme)
      raise SmokeFailure, "base URL scheme must be http or https"
    end
    if @base_uri.user || @base_uri.password || @base_uri.query || @base_uri.fragment
      raise SmokeFailure, "base URL must not contain credentials, query, or fragment"
    end
    unless ["", "/"].include?(@base_uri.path.to_s)
      raise SmokeFailure, "base URL must not contain a path"
    end
    if @base_uri.port == PRODUCTION_PORT
      raise SmokeFailure, "refusing known production port #{PRODUCTION_PORT}"
    end
    raise SmokeFailure, "timeout must be a positive integer" unless @timeout.positive?

    safe_host = %w[127.0.0.1 ::1 localhost].include?(@base_uri.host.to_s.downcase)
    safe_default = safe_host && @base_uri.port == CANARY_PORT
    unless safe_default || @allow_other_target
      raise SmokeFailure,
            "default safety policy only permits loopback:#{CANARY_PORT}; " \
            "use --allow-other-target for another verified canary"
    end
  end

  def check(name)
    started = monotonic
    yield
    record_pass(name, started)
    true
  rescue SmokeFailure => error
    record_failure(name, error.message)
    false
  rescue StandardError => error
    record_failure(name, "#{error.class}: #{error.message}")
    false
  end

  def record_pass(name, started)
    elapsed = ((monotonic - started) * 1000).round
    @results << { name: name, status: :pass }
    puts format("PASS  %-50s %6d ms", name, elapsed)
  end

  def record_failure(name, message)
    @results << { name: name, status: :fail }
    puts "FAIL  #{name}: #{sanitize(message)}"
  end

  def assert(condition, message)
    raise SmokeFailure, message unless condition
  end

  def track_secret(value)
    @secrets << value unless value.nil? || value.empty?
  end

  def sanitize(message)
    text = message.to_s.dup
    @secrets.compact.each do |secret|
      text.gsub!(secret, "[REDACTED]") unless secret.empty?
    end
    text.gsub!(/\bbrv_[A-Za-z0-9_-]{16,}\b/, "brv_[REDACTED]")
    text.gsub!(/\bsk-[A-Za-z0-9_-]{16,}\b/, "sk-[REDACTED]")
    text
  end

  def monotonic
    Process.clock_gettime(Process::CLOCK_MONOTONIC)
  end
end

options = {
  base_url: ENV.fetch("BRAVO_BASE_URL", BravoManagementSmoke::DEFAULT_BASE_URL),
  management_key_file: ENV["BRAVO_MANAGEMENT_KEY_FILE"],
  management_env_file: ENV.fetch("BRAVO_MANAGEMENT_ENV_FILE", "secrets.env"),
  management_env_variable: ENV.fetch(
    "BRAVO_MANAGEMENT_ENV_VARIABLE",
    "MANAGEMENT_PASSWORD"
  ),
  smart_key_file: ENV.fetch("BRAVO_SMART_KEY_FILE", "bravo-smart-key.txt"),
  timeout: Integer(ENV.fetch("BRAVO_MANAGEMENT_SMOKE_TIMEOUT", "120"), 10),
  model: ENV.fetch("BRAVO_TEXT_MODEL", BravoManagementSmoke::DEFAULT_MODEL),
  confirm_canary_mutations: false,
  allow_other_target: false
}

parser = OptionParser.new do |opts|
  opts.banner = "Usage: ruby scripts/bravo-management-smoke.rb [options]"
  opts.on(
    "--confirm-canary-mutations",
    "Required acknowledgement that this creates, rotates, and deletes a canary project"
  ) { options[:confirm_canary_mutations] = true }
  opts.on(
    "--base-url URL",
    "Canary base URL (default: #{options[:base_url]})"
  ) { |value| options[:base_url] = value }
  opts.on(
    "--management-key-file PATH",
    "Mode-0600 file containing only the management password"
  ) do |value|
    options[:management_key_file] = value
  end
  opts.on(
    "--management-env-file PATH",
    "Mode-0600 dotenv file containing the management password"
  ) do |value|
    options[:management_key_file] = nil
    options[:management_env_file] = value
  end
  opts.on(
    "--management-env-variable NAME",
    "Exact dotenv variable containing the management password"
  ) do |value|
    options[:management_env_variable] = value
  end
  opts.on(
    "--smart-key-file PATH",
    "Mode-0600 file containing the existing default Bravo key"
  ) { |value| options[:smart_key_file] = value }
  opts.on(
    "--model ID",
    "Bravo logical model or request_model (default: #{options[:model]})"
  ) { |value| options[:model] = value }
  opts.on("--timeout SECONDS", Integer, "Per-request timeout") do |value|
    options[:timeout] = value
  end
  opts.on(
    "--allow-other-target",
    "Allow a verified non-default canary target (port 18317 remains refused)"
  ) { options[:allow_other_target] = true }
  opts.on("-h", "--help", "Show this help") do
    puts opts
    exit 0
  end
end

begin
  parser.parse!
  abort("unexpected positional arguments") unless ARGV.empty?
  exit BravoManagementSmoke.new(options).run
rescue OptionParser::ParseError, URI::InvalidURIError, ArgumentError, SmokeFailure => error
  warn "Bravo management smoke setup failed: #{error.message}"
  exit 2
end
