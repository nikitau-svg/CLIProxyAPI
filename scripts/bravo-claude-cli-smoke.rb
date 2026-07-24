#!/usr/bin/env ruby
# frozen_string_literal: true

# Runs a real, non-interactive Claude Code request through Bravo without
# accepting or printing the smart key. The key must live in a mode-0600 file.

require "json"
require "open3"
require "optparse"
require "uri"

SmokeFailure = Class.new(StandardError)

options = {
  base_url: ENV.fetch("BRAVO_BASE_URL", "http://127.0.0.1:18319"),
  smart_key_file: ENV.fetch("BRAVO_SMART_KEY_FILE", "bravo-smart-key.txt"),
  claude_bin: ENV.fetch("CLAUDE_BIN", "claude"),
  effort: ENV.fetch("BRAVO_CLAUDE_EFFORT", "low"),
  timeout: Integer(ENV.fetch("BRAVO_CLAUDE_TIMEOUT", "120"), 10)
}

OptionParser.new do |opts|
  opts.banner = "Usage: ruby scripts/bravo-claude-cli-smoke.rb [options]"
  opts.on("--base-url URL", "Bravo canary base URL") { |value| options[:base_url] = value }
  opts.on("--smart-key-file PATH", "Mode-0600 Bravo smart-key file") do |value|
    options[:smart_key_file] = value
  end
  opts.on("--claude-bin PATH", "Claude Code executable") { |value| options[:claude_bin] = value }
  opts.on("--effort LEVEL", "low, medium, high, xhigh, or max") { |value| options[:effort] = value }
  opts.on("--timeout SECONDS", Integer, "Claude Code process timeout") do |value|
    options[:timeout] = value
  end
end.parse!

abort("unexpected positional arguments") unless ARGV.empty?

def read_secret(path)
  expanded = File.expand_path(path)
  stat = File.stat(expanded)
  raise SmokeFailure, "smart-key path is not a regular file" unless stat.file?
  if (stat.mode & 0o077) != 0
    raise SmokeFailure, "smart-key file must not be group/world accessible (use chmod 600)"
  end

  value = File.read(expanded, mode: "r:BOM|UTF-8").strip
  raise SmokeFailure, "smart-key file is empty" if value.empty?

  value
rescue Errno::ENOENT, Errno::EACCES => error
  raise SmokeFailure, "smart-key file cannot be read: #{error.class}"
end

def sanitize(value, secrets)
  text = value.to_s.dup
  secrets.each { |secret| text.gsub!(secret, "[REDACTED]") unless secret.empty? }
  text.gsub!(/\bbrv_[A-Za-z0-9_-]{16,}\b/, "brv_[REDACTED]")
  text.gsub!(/\bsk-[A-Za-z0-9_-]{16,}\b/, "sk-[REDACTED]")
  text.gsub(/\s+/, " ").strip.byteslice(0, 500)
end

begin
  uri = URI(options.fetch(:base_url))
  unless %w[http https].include?(uri.scheme) && uri.host && ["", "/"].include?(uri.path.to_s)
    raise SmokeFailure, "base URL must contain only an http(s) origin"
  end
  if uri.user || uri.password || uri.query || uri.fragment
    raise SmokeFailure, "base URL must not contain credentials, query, or fragment"
  end

  effort = options.fetch(:effort).strip.downcase
  unless %w[low medium high xhigh max].include?(effort)
    raise SmokeFailure, "unsupported effort #{effort.inspect}"
  end

  key = read_secret(options.fetch(:smart_key_file))
  secrets = [key]
  marker = "BRAVO_CLAUDE_CLI_OK"
  environment = {
    "ANTHROPIC_API_KEY" => nil,
    "CLAUDE_CODE_OAUTH_TOKEN" => nil,
    "ANTHROPIC_BASE_URL" => uri.to_s.sub(%r{/$}, ""),
    "ANTHROPIC_AUTH_TOKEN" => key,
    "ANTHROPIC_DEFAULT_OPUS_MODEL" => "bravo/opus",
    "ANTHROPIC_DEFAULT_SONNET_MODEL" => "bravo/sonnet",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL" => "bravo/haiku",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" => "1"
  }
  command = [
    options.fetch(:claude_bin),
    "--print",
    "--output-format",
    "json",
    "--model",
    "opus",
    "--effort",
    effort,
    "What is the exact stdout of this Python program? Reply with stdout only.\n\n" \
      "print(\"#{marker}\")"
  ]

  stdout = +""
  stderr = +""
  status = nil
  Open3.popen3(environment, *command) do |stdin, out, err, wait_thread|
    stdin.close
    out_reader = Thread.new { out.read }
    err_reader = Thread.new { err.read }
    unless wait_thread.join(options.fetch(:timeout))
      Process.kill("TERM", wait_thread.pid)
      wait_thread.join(5)
      Process.kill("KILL", wait_thread.pid) if wait_thread.alive?
      raise SmokeFailure, "Claude Code timed out"
    end
    stdout = out_reader.value
    stderr = err_reader.value
    status = wait_thread.value
  end

  unless status.success?
    parsed_error = begin
      JSON.parse(stdout)
    rescue JSON::ParserError
      nil
    end
    structured_detail =
      if parsed_error.is_a?(Hash)
        [
          parsed_error["result"],
          parsed_error["error"],
          parsed_error["subtype"],
          parsed_error["stop_reason"]
        ].compact.map(&:to_s).join(" ")
      end
    detail = [stderr, structured_detail || stdout].reject(&:empty?).join(" ")
    raise SmokeFailure,
          "Claude Code exited #{status.exitstatus}: #{sanitize(detail, secrets)}"
  end

  root = JSON.parse(stdout)
  result = root["result"].to_s
  raise SmokeFailure, "Claude Code JSON did not contain the expected marker" unless result.include?(marker)

  puts "PASS  Claude Code --effort #{effort} through bravo/opus"
  puts "claude_cli_result=marker_present"
rescue JSON::ParserError => error
  warn "FAIL  Claude Code returned invalid JSON: #{error.message}"
  exit 1
rescue SmokeFailure => error
  warn "FAIL  #{sanitize(error.message, defined?(secrets) ? secrets : [])}"
  exit 1
ensure
  key = nil if defined?(key)
  secrets = [] if defined?(secrets)
end
