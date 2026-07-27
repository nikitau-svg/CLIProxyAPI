#!/usr/bin/env ruby
# frozen_string_literal: true

# Real Claude Code regression gate for the auto-mode Edit classifier. It runs
# only against the loopback canary, exposes Read/Edit without pre-authorizing
# either tool, and proves that Edit actually changed one disposable fixture.

require "json"
require "open3"
require "optparse"
require "tmpdir"
require "uri"

ClaudeAutoEditFailure = Class.new(StandardError)

options = {
  base_url: ENV.fetch("BRAVO_BASE_URL", "http://127.0.0.1:18319"),
  smart_key_file: ENV.fetch("BRAVO_SMART_KEY_FILE", "bravo-smart-key.txt"),
  claude_bin: ENV.fetch("CLAUDE_BIN", "/Users/juloaipc/.local/bin/claude"),
  model: ENV.fetch("BRAVO_CLAUDE_MODEL", "opus"),
  timeout: Integer(ENV.fetch("BRAVO_CLAUDE_TIMEOUT", "180"), 10)
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-claude-auto-edit-smoke.rb [options]"
  parser.on("--base-url URL", "Bravo canary origin") { |value| options[:base_url] = value }
  parser.on("--smart-key-file PATH", "Mode-0600 Bravo smart-key file") do |value|
    options[:smart_key_file] = value
  end
  parser.on("--claude-bin PATH", "Claude Code executable") { |value| options[:claude_bin] = value }
  parser.on("--model MODEL", "Claude Code model alias: opus or sonnet") { |value| options[:model] = value }
  parser.on("--timeout SECONDS", Integer, "Claude Code process timeout") do |value|
    options[:timeout] = value
  end
end.parse!

abort("unexpected positional arguments") unless ARGV.empty?

def read_secret(path)
  expanded = File.expand_path(path)
  stat = File.stat(expanded)
  raise ClaudeAutoEditFailure, "smart-key path is not a regular file" unless stat.file?
  if (stat.mode & 0o077) != 0
    raise ClaudeAutoEditFailure, "smart-key file must be mode 0600"
  end

  value = File.read(expanded, mode: "r:BOM|UTF-8").strip
  raise ClaudeAutoEditFailure, "smart-key file is empty" if value.empty?

  value
rescue Errno::ENOENT, Errno::EACCES => error
  raise ClaudeAutoEditFailure, "smart-key file cannot be read: #{error.class}"
end

def sanitize(value, secrets)
  text = value.to_s.dup
  secrets.each { |secret| text.gsub!(secret, "[REDACTED]") unless secret.empty? }
  text.gsub!(/\bbrv_[A-Za-z0-9_-]{16,}\b/, "brv_[REDACTED]")
  text.gsub!(/\bsk-[A-Za-z0-9_-]{16,}\b/, "sk-[REDACTED]")
  text.gsub(/\s+/, " ").strip.byteslice(0, 800)
end

def contains_edit_tool_use?(value)
  case value
  when Hash
    return true if value["type"] == "tool_use" && value["name"] == "Edit"

    value.values.any? { |nested| contains_edit_tool_use?(nested) }
  when Array
    value.any? { |nested| contains_edit_tool_use?(nested) }
  else
    false
  end
end

begin
  uri = URI(options.fetch(:base_url))
  unless uri.scheme == "http" &&
         %w[127.0.0.1 localhost ::1].include?(uri.host) &&
         uri.port == 18_319 &&
         ["", "/"].include?(uri.path.to_s) &&
         !uri.user &&
         !uri.password &&
         !uri.query &&
         !uri.fragment
    raise ClaudeAutoEditFailure, "real auto/Edit gate only permits http loopback:18319"
  end
  unless (30..300).cover?(options.fetch(:timeout))
    raise ClaudeAutoEditFailure, "timeout must be between 30 and 300 seconds"
  end
  unless %w[opus sonnet].include?(options.fetch(:model))
    raise ClaudeAutoEditFailure, "model must be opus or sonnet"
  end

  key = read_secret(options.fetch(:smart_key_file))
  secrets = [key]
  before = "BRAVO_AUTO_EDIT_BEFORE\n"
  after = "BRAVO_AUTO_EDIT_AFTER\n"
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
    "stream-json",
    "--verbose",
    "--permission-mode",
    "auto",
    "--tools",
    "Read,Edit",
    "--model",
    options.fetch(:model),
    "--effort",
    "low",
    "--no-session-persistence",
    "--safe-mode",
    "Read fixture.txt, then use the Edit tool, not Write or Bash, exactly once " \
      "to replace BRAVO_AUTO_EDIT_BEFORE with BRAVO_AUTO_EDIT_AFTER. Read the " \
      "file again to verify it. Do not finish until the edit succeeds."
  ]

  Dir.mktmpdir("bravo-auto-edit-") do |directory|
    fixture = File.join(directory, "fixture.txt")
    File.write(fixture, before, mode: "w:UTF-8")
    stdout = +""
    stderr = +""
    status = nil

    Open3.popen3(environment, *command, chdir: directory) do |stdin, out, err, wait_thread|
      stdin.close
      out_reader = Thread.new { out.read }
      err_reader = Thread.new { err.read }
      unless wait_thread.join(options.fetch(:timeout))
        Process.kill("TERM", wait_thread.pid)
        wait_thread.join(5)
        Process.kill("KILL", wait_thread.pid) if wait_thread.alive?
        raise ClaudeAutoEditFailure, "Claude Code timed out"
      end
      stdout = out_reader.value
      stderr = err_reader.value
      status = wait_thread.value
    end

    unless status.success?
      detail = [stderr, stdout].reject(&:empty?).join(" ")
      raise ClaudeAutoEditFailure,
            "Claude Code exited #{status.exitstatus}: #{sanitize(detail, secrets)}"
    end

    events = stdout.lines.each_with_object([]) do |line, parsed|
      next if line.strip.empty?

      parsed << JSON.parse(line)
    rescue JSON::ParserError
      raise ClaudeAutoEditFailure, "Claude Code returned invalid stream-json"
    end
    unless events.any? { |event| contains_edit_tool_use?(event) }
      raise ClaudeAutoEditFailure, "Claude Code output contains no Edit tool_use"
    end
    unless File.read(fixture, mode: "r:BOM|UTF-8") == after
      raise ClaudeAutoEditFailure, "Edit did not produce the exact expected fixture"
    end
  end

  puts "PASS  real Claude Code auto-mode Edit through Bravo canary"
  puts "claude_auto_edit=verified"
  puts "claude_auto_edit_model=#{options.fetch(:model)}"
rescue ClaudeAutoEditFailure => error
  warn "FAIL  #{sanitize(error.message, defined?(secrets) ? secrets : [])}"
  exit 1
ensure
  key = nil if defined?(key)
  secrets = [] if defined?(secrets)
end
