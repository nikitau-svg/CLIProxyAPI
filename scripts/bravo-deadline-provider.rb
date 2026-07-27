#!/usr/bin/env ruby
# frozen_string_literal: true

# Deterministic, credential-free upstream for the Bravo pre-commit hedge
# canary. Anthropic Messages requests are accepted but never receive response
# headers; Codex Responses requests immediately return one valid SSE response.
# The control endpoints expose only event names and timings, never request
# bodies or authorization headers.

require "json"
require "optparse"
require "socket"
require "time"

CanaryProviderFailure = Class.new(StandardError)

options = {
  bind: "127.0.0.1",
  port: 18_992,
  stall_seconds: 15,
  marker: "BRAVO_DEADLINE_FALLBACK_OK",
  control_token: "bravo-deadline-canary"
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-deadline-provider.rb [options]"
  parser.on("--bind ADDRESS", "Listen address") { |value| options[:bind] = value }
  parser.on("--port PORT", Integer, "Listen port") { |value| options[:port] = value }
  parser.on("--stall-seconds SECONDS", Integer, "Maximum Claude stall") do |value|
    options[:stall_seconds] = value
  end
  parser.on("--marker VALUE", "Codex response marker") { |value| options[:marker] = value }
  parser.on("--control-token VALUE", "Token for /events and /reset") do |value|
    options[:control_token] = value
  end
end.parse!

abort("unexpected positional arguments") unless ARGV.empty?
abort("port must be between 1024 and 65535") unless (1024..65_535).cover?(options[:port])
abort("stall-seconds must be between 2 and 120") unless (2..120).cover?(options[:stall_seconds])
abort("marker must not be empty") if options[:marker].to_s.strip.empty?
abort("control-token must not be empty") if options[:control_token].to_s.strip.empty?

class CanaryEventStore
  def initialize
    @events = []
    @mutex = Mutex.new
    @sequence = 0
  end

  def record(type, model: nil)
    @mutex.synchronize do
      @sequence += 1
      event = {
        "sequence" => @sequence,
        "type" => type,
        "at" => Time.now.utc.iso8601(6),
        "monotonic_seconds" => Process.clock_gettime(Process::CLOCK_MONOTONIC)
      }
      event["model"] = model unless model.to_s.empty?
      @events << event
      event
    end
  end

  def snapshot
    @mutex.synchronize { @events.map(&:dup) }
  end

  def reset
    @mutex.synchronize do
      @events.clear
      @sequence = 0
    end
  end
end

def read_request(client)
  request_line = client.gets
  raise CanaryProviderFailure, "empty request" if request_line.nil?

  method, target, version = request_line.strip.split(/\s+/, 3)
  raise CanaryProviderFailure, "invalid request line" unless method && target && version&.start_with?("HTTP/")

  headers = {}
  header_bytes = request_line.bytesize
  while (line = client.gets)
    header_bytes += line.bytesize
    raise CanaryProviderFailure, "headers too large" if header_bytes > 64 * 1024
    break if line == "\r\n" || line == "\n"

    name, value = line.split(":", 2)
    raise CanaryProviderFailure, "invalid header" unless name && value

    headers[name.strip.downcase] = value.strip
  end

  content_length = Integer(headers.fetch("content-length", "0"), 10)
  raise CanaryProviderFailure, "request body too large" if content_length > 4 * 1024 * 1024

  body = content_length.positive? ? client.read(content_length) : ""
  raise CanaryProviderFailure, "truncated request body" if body.nil? || body.bytesize != content_length

  [method, target.split("?", 2).first, headers, body]
rescue ArgumentError
  raise CanaryProviderFailure, "invalid content length"
end

def request_model(body)
  root = JSON.parse(body)
  root["model"].to_s
rescue JSON::ParserError
  ""
end

def write_response(client, status, content_type, body, extra_headers = {})
  reason = {
    200 => "OK",
    400 => "Bad Request",
    401 => "Unauthorized",
    404 => "Not Found",
    504 => "Gateway Timeout"
  }.fetch(status, "Canary Response")
  client.write("HTTP/1.1 #{status} #{reason}\r\n")
  client.write("Content-Type: #{content_type}\r\n")
  client.write("Content-Length: #{body.bytesize}\r\n")
  client.write("Cache-Control: no-store\r\n")
  extra_headers.each { |name, value| client.write("#{name}: #{value}\r\n") }
  client.write("Connection: close\r\n")
  client.write("\r\n")
  client.write(body)
end

def json_response(client, status, object)
  write_response(client, status, "application/json", JSON.generate(object))
end

def authorized_control?(headers, expected)
  supplied = headers["x-canary-control"].to_s
  !supplied.empty? && supplied.bytesize == expected.bytesize &&
    supplied.bytes.zip(expected.bytes).reduce(0) { |diff, (left, right)| diff | (left ^ right) }.zero?
end

def peer_closed?(client)
  ready = IO.select([client], nil, nil, 0.05)
  return false unless ready

  peeked = client.recv_nonblock(1, Socket::MSG_PEEK, exception: false)
  peeked.nil? || peeked == ""
rescue IOError, SystemCallError
  true
end

def codex_sse(marker)
  response_id = "resp_bravo_deadline_canary"
  created = {
    "type" => "response.created",
    "sequence_number" => 0,
    "response" => {
      "id" => response_id,
      "object" => "response",
      "created_at" => Time.now.to_i,
      "status" => "in_progress",
      "model" => "gpt-5.6-terra",
      "output" => []
    }
  }
  delta = {
    "type" => "response.output_text.delta",
    "sequence_number" => 1,
    "item_id" => "msg_bravo_deadline_canary",
    "output_index" => 0,
    "content_index" => 0,
    "delta" => marker
  }
  completed = {
    "type" => "response.completed",
    "sequence_number" => 2,
    "response" => {
      "id" => response_id,
      "object" => "response",
      "created_at" => Time.now.to_i,
      "status" => "completed",
      "model" => "gpt-5.6-terra",
      "output" => [{
        "id" => "msg_bravo_deadline_canary",
        "type" => "message",
        "role" => "assistant",
        "content" => [{ "type" => "output_text", "text" => marker }]
      }],
      "usage" => {
        "input_tokens" => 7,
        "output_tokens" => 3,
        "total_tokens" => 10,
        "input_tokens_details" => { "cached_tokens" => 0 },
        "output_tokens_details" => { "reasoning_tokens" => 0 }
      }
    }
  }
  [created, delta, completed].map do |event|
    "event: #{event.fetch("type")}\ndata: #{JSON.generate(event)}\n\n"
  end.join
end

events = CanaryEventStore.new
server = TCPServer.new(options[:bind], options[:port])
server.setsockopt(Socket::SOL_SOCKET, Socket::SO_REUSEADDR, true)

stopping = false
stop = proc do
  stopping = true
  server.close
end
trap("INT", &stop)
trap("TERM", &stop)

$stdout.sync = true
puts "ready bind=#{options[:bind]} port=#{options[:port]}"

until stopping
  begin
    socket = server.accept
  rescue IOError, Errno::EBADF
    break if stopping

    raise
  end

  Thread.new(socket) do |client|
    begin
      method, path, headers, body = read_request(client)
      case [method, path]
      when ["GET", "/health"]
        json_response(client, 200, "ok" => true)
      when ["GET", "/events"]
        unless authorized_control?(headers, options[:control_token])
          json_response(client, 401, "error" => "unauthorized")
          next
        end
        json_response(client, 200, "events" => events.snapshot)
      when ["POST", "/reset"]
        unless authorized_control?(headers, options[:control_token])
          json_response(client, 401, "error" => "unauthorized")
          next
        end
        events.reset
        json_response(client, 200, "ok" => true)
      else
        model = request_model(body)
        if method == "POST" && path.end_with?("/v1/messages")
          events.record("claude_started", model: model)
          deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + options[:stall_seconds]
          canceled = false
          until Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
            if peer_closed?(client)
              canceled = true
              break
            end
          end
          if canceled
            events.record("claude_canceled", model: model)
          else
            events.record("claude_stall_expired", model: model)
            json_response(
              client,
              504,
              "type" => "error",
              "error" => {
                "type" => "api_error",
                "code" => "canary_stall_expired",
                "message" => "The deterministic Claude stall was not canceled."
              }
            )
          end
        elsif method == "POST" && path.end_with?("/responses")
          events.record("codex_started", model: model)
          payload = codex_sse(options[:marker])
          write_response(client, 200, "text/event-stream", payload)
          events.record("codex_completed", model: model)
        else
          json_response(client, 404, "error" => "not_found")
        end
      end
    rescue CanaryProviderFailure => error
      json_response(client, 400, "error" => error.message)
    rescue IOError, SystemCallError
      nil
    ensure
      client.close
    end
  end
end
