#!/usr/bin/env ruby
# frozen_string_literal: true

# Destructive only inside the dedicated synthetic credits/context canary.
# Replays the production Codex quota shape where the session window is not
# applicable, the weekly window is confirmed, and Bravo carries substantial
# pending debt. No real provider credential is used.

require "fileutils"
require "json"
require "net/http"
require "optparse"
require "time"
require "uri"

CanaryFailure = Class.new(StandardError)

options = {
  base_url: "http://127.0.0.1:18319",
  provider_url: "http://127.0.0.1:18993",
  management_env_file: "secrets.env",
  state_file: nil,
  docker_path: "/usr/local/bin/docker",
  container_name: nil,
  confirmed: false
}

OptionParser.new do |parser|
  parser.on("--confirm-canary-mutations") { options[:confirmed] = true }
  parser.on("--base-url URL") { |value| options[:base_url] = value }
  parser.on("--provider-url URL") { |value| options[:provider_url] = value }
  parser.on("--management-env-file PATH") { |value| options[:management_env_file] = value }
  parser.on("--state-file PATH") { |value| options[:state_file] = value }
  parser.on("--docker PATH") { |value| options[:docker_path] = value }
  parser.on("--container NAME") { |value| options[:container_name] = value }
end.parse!

abort("pass --confirm-canary-mutations") unless options[:confirmed]
abort("pass --state-file and --container") if options.values_at(:state_file, :container_name).any? { |value| value.to_s.empty? }
abort("unexpected positional arguments") unless ARGV.empty?

base = URI(options.fetch(:base_url))
provider = URI(options.fetch(:provider_url))
state_file = File.expand_path(options.fetch(:state_file))
env_file = File.expand_path(options.fetch(:management_env_file))
docker = options.fetch(:docker_path)
container = options.fetch(:container_name)
root = File.dirname(File.dirname(state_file))

unless base.scheme == "http" && base.host == "127.0.0.1" && base.port == 18_319 &&
       provider.scheme == "http" && provider.host == "127.0.0.1" && provider.port == 18_993
  abort("only the loopback canary ports 18319/18993 are accepted")
end
unless File.basename(root).match?(/\Acliproxyapi-credits-context-canary-v\d+\.[A-Za-z0-9]+\z/) &&
       File.dirname(state_file) == File.join(root, "bravo-data") &&
       File.basename(state_file) == "bravo-state.json" &&
       container.start_with?("CLIProxyAPI-Bravo-") && container != "CLIProxyAPI-Prod"
  abort("refusing a non-canary state/container target")
end

management_key = File.read(env_file)[/^MANAGEMENT_KEY=(.+)$/, 1].to_s.strip
abort("management key is missing") if management_key.empty?

def request(uri, method:, key: nil, bearer: nil, body: nil, control: nil)
  klass = {
    get: Net::HTTP::Get,
    post: Net::HTTP::Post,
    patch: Net::HTTP::Patch,
    delete: Net::HTTP::Delete
  }.fetch(method)
  req = klass.new(uri)
  req["X-Management-Key"] = key if key
  req["Authorization"] = "Bearer #{bearer}" if bearer
  req["X-Canary-Control"] = control if control
  if body
    req["Content-Type"] = "application/json"
    req.body = JSON.generate(body)
  end
  response = Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: 60) { |http| http.request(req) }
  parsed = response.body.to_s.empty? ? {} : JSON.parse(response.body)
  [response.code.to_i, parsed]
rescue JSON::ParserError
  raise CanaryFailure, "non-JSON response from #{uri.path}"
end

def endpoint(base, path)
  URI.join(base.to_s.end_with?("/") ? base.to_s : "#{base}/", path.sub(%r{\A/}, ""))
end

def docker_action(docker, action, container)
  ok = system(docker, action, container, out: File::NULL)
  raise CanaryFailure, "canary container #{action} failed" unless ok
end

def wait_management(base, key)
  deadline = Time.now + 75
  loop do
    status, = request(endpoint(base, "/v0/management/bravo/subscriptions"), method: :get, key: key)
    return if status == 200
  rescue StandardError
    raise CanaryFailure, "canary did not recover" if Time.now >= deadline
    sleep 1
  end
end

project_id = nil
begin
  status, subscriptions = request(endpoint(base, "/v0/management/bravo/subscriptions"), method: :get, key: management_key)
  raise CanaryFailure, "subscriptions returned HTTP #{status}" unless status == 200
  items = Array(subscriptions["subscriptions"])
  claude = items.find { |item| item["provider"] == "claude" }
  codex = items.find { |item| item["provider"] == "codex" }
  raise CanaryFailure, "synthetic Claude/Codex subscriptions are missing" unless claude && codex

  status, = request(
    endpoint(base, "/v0/management/bravo/tariffs"),
    method: :patch,
    key: management_key,
    body: {
      "id" => "x20",
      "session_floor_percent" => 10.0,
      "weekly_floor_percent" => 5.0,
      "reservation_percent" => 0.05
    }
  )
  raise CanaryFailure, "x20 canary tariff returned HTTP #{status}" unless status == 200
  status, = request(
    endpoint(base, "/v0/management/bravo/subscriptions"),
    method: :patch,
    key: management_key,
    body: { "auth_index" => codex.fetch("auth_index"), "tariff" => "x20" }
  )
  raise CanaryFailure, "Codex canary tariff assignment returned HTTP #{status}" unless status == 200

  status, created = request(
    endpoint(base, "/v0/management/bravo/projects"),
    method: :post,
    key: management_key,
    body: {
      "name" => "bravo-not-applicable-canary-#{Time.now.to_i}",
      "enabled" => true,
      "models" => ["*"],
      "allowed_auth_ids" => [claude.fetch("auth_index"), codex.fetch("auth_index")],
      "primary_auth_ids" => [claude.fetch("auth_index")]
    }
  )
  raise CanaryFailure, "project creation returned HTTP #{status}" unless status == 201
  project_id = created.dig("project", "id")
  project_key = created["plaintext_key"].to_s
  raise CanaryFailure, "project creation response is incomplete" if project_id.to_s.empty? || project_key.empty?

  docker_action(docker, "stop", container)
  state = JSON.parse(File.read(state_file))
  auth_index = codex.fetch("auth_index")
  now = Time.now.utc
  backup = "#{state_file}.pre-not-applicable-#{now.strftime("%Y%m%dT%H%M%S")}.json"
  FileUtils.cp(state_file, backup, preserve: true)
  wal = "#{state_file}.adaptive.wal"
  File.rename(wal, "#{wal}.pre-not-applicable-#{now.strftime("%Y%m%dT%H%M%S")}") if File.file?(wal)

  quota = state.fetch("quotas").fetch(auth_index)
  quota.update(
    "status" => "confirmed",
    "confidence" => "confirmed",
    "provider" => "codex",
    "session" => {
      "used_percent" => 0.0,
      "remaining_percent" => 100.0,
      "reset_at" => "0001-01-01T00:00:00Z",
      "reset_mode" => "not_applicable"
    },
    "weekly" => {
      "used_percent" => 26.0,
      "remaining_percent" => 74.0,
      "reset_at" => (now + 7 * 24 * 60 * 60).iso8601(9),
      "reset_mode" => "scheduled"
    },
    "refreshed_at" => now.iso8601(9),
    "confirmed_at" => now.iso8601(9),
    "error" => "",
    "dirty" => false,
    "usage_refresh" => {
      "attempt_count" => 1,
      "success_count" => 1,
      "last_attempt_at" => now.iso8601(9),
      "last_success_at" => now.iso8601(9),
      "next_attempt_at" => (now + 15 * 60).iso8601(9)
    }
  )
  state["adaptive_quota"] = {
    "pending" => { auth_index => { "percent" => 66.743, "updated_at" => now.iso8601(9) } },
    "revisions" => { auth_index => 1 }
  }
  state["updated_at"] = now.iso8601(9)

  temporary = "#{state_file}.not-applicable-#{Process.pid}.tmp"
  File.open(temporary, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
    file.write(JSON.pretty_generate(state))
    file.write("\n")
    file.flush
    file.fsync
  end
  File.rename(temporary, state_file)
  docker_action(docker, "start", container)
  wait_management(base, management_key)

  status, refreshed = request(endpoint(base, "/v0/management/bravo/subscriptions"), method: :get, key: management_key)
  raise CanaryFailure, "subscriptions after restart returned HTTP #{status}" unless status == 200
  current = Array(refreshed["subscriptions"]).find { |item| item["auth_index"] == auth_index }
  raise CanaryFailure, "Codex subscription disappeared" unless current
  allocator = current.fetch("allocator")
  unless current.dig("quota", "session", "reset_mode") == "not_applicable" &&
         allocator["session_admission_cutoff_percent"].to_f.zero? &&
         allocator["session_headroom_after_percent"].to_f == 100.0 &&
         allocator["weekly_headroom_after_percent"].to_f.positive?
    raise CanaryFailure, "not_applicable window still affects allocator/UI"
  end

  control = "bravo-credits-context-canary"
  _, before_events = request(endpoint(provider, "/events"), method: :get, control: control)
  cursor = Array(before_events["events"]).map { |event| event["sequence"].to_i }.max || 0
  status, response = request(
    endpoint(base, "/v1/messages"),
    method: :post,
    bearer: project_key,
    body: {
      "model" => "bravo/gpt-5.6-sol",
      "messages" => [{ "role" => "user", "content" => "Reply with the canary marker." }],
      "max_tokens" => 64,
      "stream" => false
    }
  )
  raise CanaryFailure, "secondary Codex request returned HTTP #{status}" unless status == 200
  raise CanaryFailure, "secondary Codex response marker is missing" unless JSON.generate(response).include?("BRAVO_CREDITS_FALLBACK_OK")

  _, after_events = request(endpoint(provider, "/events"), method: :get, control: control)
  observed = Array(after_events["events"]).select { |event| event["sequence"].to_i > cursor }
  unless observed.length == 1 && observed.first["type"] == "codex_fallback_success"
    raise CanaryFailure, "expected exactly one Codex provider call, got #{observed.map { |event| event["type"] }.inspect}"
  end
  puts "PASS  not_applicable session ignored; weekly-safe secondary Codex executed exactly once"
ensure
  if project_id
    request(endpoint(base, "/v0/management/bravo/projects"), method: :delete, key: management_key, body: { "id" => project_id })
  end
end
