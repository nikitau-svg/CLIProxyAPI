#!/usr/bin/env ruby
# frozen_string_literal: true

# Creates an isolated, synthetic-credential runtime for the credits/context
# canary. The generated management key is written once to a mode-0600 env file
# and is never printed.

require "fileutils"
require "optparse"
require "securerandom"
require "yaml"

CanarySetupFailure = Class.new(StandardError)

options = {
  root: nil,
  image: nil,
  bind_address: "127.0.0.1",
  port: 18_319,
  provider_url: "http://host.docker.internal:18993",
  container_name: "CLIProxyAPI-Bravo-0.7.9-Credits-Canary"
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-credits-context-canary-setup.rb --root PATH --image IMAGE"
  parser.on("--root PATH", "Empty canary runtime directory") { |value| options[:root] = value }
  parser.on("--image IMAGE", "Already-built canary image") { |value| options[:image] = value }
  parser.on("--bind-address ADDRESS", "Host address for port 18319") { |value| options[:bind_address] = value }
  parser.on("--port PORT", Integer, "Canary host port") { |value| options[:port] = value }
  parser.on("--provider-url URL", "Synthetic provider URL visible inside Docker") do |value|
    options[:provider_url] = value
  end
  parser.on("--container-name NAME", "Unique canary container name") { |value| options[:container_name] = value }
end.parse!

abort("unexpected positional arguments") unless ARGV.empty?

def validate_options!(options)
  root = File.expand_path(options[:root].to_s)
  raise CanarySetupFailure, "--root is required" if options[:root].to_s.strip.empty?
  raise CanarySetupFailure, "refusing broad root" if ["/", Dir.home, File.dirname(Dir.home)].include?(root)
  unless File.basename(root).start_with?("cliproxyapi-credits-context-canary-")
    raise CanarySetupFailure, "root basename must start with cliproxyapi-credits-context-canary-"
  end
  unless options[:image].to_s.match?(/\A[a-zA-Z0-9][a-zA-Z0-9._\/-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*\z/)
    raise CanarySetupFailure, "--image must be an explicit repository:tag"
  end
  raise CanarySetupFailure, "production port 18317 is refused" if options[:port] == 18_317
  raise CanarySetupFailure, "canary port must be 18319" unless options[:port] == 18_319
  unless options[:provider_url].start_with?("http://host.docker.internal:18993")
    raise CanarySetupFailure, "provider URL must use host.docker.internal:18993"
  end
  unless options[:container_name].match?(/\A[A-Za-z0-9][A-Za-z0-9_.-]+\z/)
    raise CanarySetupFailure, "invalid container name"
  end

  options.merge(root: root)
end

def write_private(path, contents)
  File.open(path, File::WRONLY | File::CREAT | File::EXCL, 0o600) { |file| file.write(contents) }
end

options = validate_options!(options)
root = options.fetch(:root)
if File.exist?(root) && !Dir.empty?(root)
  raise CanarySetupFailure, "runtime root must be absent or empty"
end

FileUtils.mkdir_p(root, mode: 0o700)
%w[auths bravo-data logs].each { |name| FileUtils.mkdir_p(File.join(root, name), mode: 0o700) }

management_key = SecureRandom.urlsafe_base64(36)
claude_keys = 4.times.map { "sk-ant-canary-#{SecureRandom.hex(24)}" }
codex_key = "sk-canary-#{SecureRandom.hex(24)}"

config = {
  "host" => "0.0.0.0",
  "port" => 8317,
  "remote-management" => {
    "allow-remote" => true,
    "secret-key" => management_key,
    "disable-control-panel" => false,
    "disable-auto-update-panel" => true
  },
  "auth-dir" => "/root/.cli-proxy-api",
  "api-keys" => [],
  "debug" => false,
  "logging-to-file" => true,
  "logs-max-total-size-mb" => 32,
  "error-logs-max-files" => 10,
  "usage-statistics-enabled" => true,
  "request-retry" => 0,
  "max-retry-credentials" => 1,
  "max-retry-interval" => 1,
  "disable-cooling" => false,
  "routing" => { "strategy" => "fill-first", "session-affinity" => false },
  "plugins" => {
    "enabled" => true,
    "dir" => "plugin-dist",
    "configs" => {
      "bravo" => {
        "enabled" => true,
        "prefix" => "bravo/",
        "require_smart_key" => true,
        "max_attempts" => 0,
        "cooldown_seconds" => 600,
        "state_path" => "/CLIProxyAPI/bravo-data/bravo-state.json",
        "smart_keys" => []
      }
    }
  },
  "claude-api-key" => claude_keys.map.with_index do |key, index|
    {
      "api-key" => key,
      "priority" => 10_000 - index,
      "base-url" => options.fetch(:provider_url),
      "models" => [
        {
          "name" => "claude-fable-5",
          "alias" => "claude-fable-5",
          "display-name" => "Fable 5",
          "force-mapping" => true
        },
        {
          "name" => "claude-sonnet-5",
          "alias" => "claude-sonnet-5",
          "display-name" => "Claude Sonnet 5",
          "force-mapping" => true
        }
      ]
    }
  end,
  "codex-api-key" => [{
    "api-key" => codex_key,
    "priority" => 10_000,
    "base-url" => options.fetch(:provider_url),
    "models" => [
      {
        "name" => "gpt-5.6-sol",
        "alias" => "gpt-5.6-sol",
        "display-name" => "GPT-5.6 Sol",
        "force-mapping" => true
      },
      {
        "name" => "gpt-5.6-terra",
        "alias" => "gpt-5.6-terra",
        "display-name" => "GPT-5.6 Terra",
        "force-mapping" => true
      }
    ]
  }]
}

compose = {
  "services" => {
    "credits-context-canary" => {
      "image" => options.fetch(:image),
      "pull_policy" => "never",
      "container_name" => options.fetch(:container_name),
      "init" => true,
      "restart" => "no",
      "ports" => ["#{options.fetch(:bind_address)}:#{options.fetch(:port)}:8317"],
      "volumes" => [
        "./config.yaml:/CLIProxyAPI/config.yaml",
        "./auths:/root/.cli-proxy-api",
        "./bravo-data:/CLIProxyAPI/bravo-data",
        "./logs:/CLIProxyAPI/logs"
      ],
      "healthcheck" => {
        "test" => ["CMD", "/CLIProxyAPI/cliproxy-healthcheck"],
        "interval" => "5s",
        "timeout" => "4s",
        "retries" => 12,
        "start_period" => "10s"
      },
      "cap_drop" => ["ALL"],
      "security_opt" => ["no-new-privileges:true"],
      "pids_limit" => 256,
      "mem_limit" => "1g",
      "cpus" => 2.0
    }
  }
}

write_private(File.join(root, "secrets.env"), "MANAGEMENT_KEY=#{management_key}\n")
write_private(File.join(root, "config.yaml"), YAML.dump(config))
File.write(File.join(root, "docker-compose.yml"), YAML.dump(compose), mode: "wx", perm: 0o644)

management_key = nil
claude_keys.fill(nil)
codex_key = nil

puts "created isolated canary runtime at #{root}"
puts "container=#{options.fetch(:container_name)} port=#{options.fetch(:port)} credentials=synthetic"
