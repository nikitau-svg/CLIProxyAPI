#!/usr/bin/env ruby
# frozen_string_literal: true

# Credential-free upstream used by the Bravo credits/context release canary.
# It reproduces the exact reviewed Anthropic credits_required payload, including
# deliberately sensitive fields that must never escape CLIProxyAPI. Request
# bodies and authorization headers are never retained by the control endpoint.

require "json"
require "optparse"
require "socket"
require "time"

CanaryProviderFailure = Class.new(StandardError)

options = {
  bind: "127.0.0.1",
  port: 18_993,
  retry_after: "600",
  control_token: "bravo-credits-context-canary",
  fallback_marker: "BRAVO_CREDITS_FALLBACK_OK",
  sibling_marker: "BRAVO_CLAUDE_SIBLING_OK",
  context_marker: "BRAVO_CONTEXT_OVERFLOW",
  reverse_fallback_marker: "BRAVO_CODEX_SERVER_ERROR"
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-credits-context-provider.rb [options]"
  parser.on("--bind ADDRESS", "Listen address") { |value| options[:bind] = value }
  parser.on("--port PORT", Integer, "Listen port") { |value| options[:port] = value }
  parser.on("--retry-after VALUE", "Retry-After for the HTTP failure") { |value| options[:retry_after] = value }
  parser.on("--control-token VALUE", "Token for /events and /reset") { |value| options[:control_token] = value }
end.parse!

abort("unexpected positional arguments") unless ARGV.empty?
abort("port must be between 1024 and 65535") unless (1024..65_535).cover?(options[:port])
abort("retry-after must be numeric") unless options[:retry_after].match?(/\A\d+\z/)
abort("control-token must not be empty") if options[:control_token].strip.empty?

CREDITS_REQUIRED = {
  "type" => "error",
  "error" => {
    "type" => "rate_limit_error",
    "message" => "Usage credits are required for this model.",
    "details" => {
      "error_code" => "credits_required",
      "notice" => {
        "title" => "You've hit your monthly spend limit",
        "text" => "Ask your admin to raise your spend limit, or switch models to continue this chat.",
        "cta" => {
          "copy" => "Switch models",
          "intent" => "switch_model",
          "redirect_hint" => nil
        },
        "is_dismissible" => true
      },
      "model_display_name" => "Fable 5",
      "can_user_purchase_credits" => false,
      "model" => "claude-fable-5",
      "has_chargeable_saved_payment_method" => true,
      "disabled_reason" => "org_level_disabled_until",
      "exhausted_included_allowance" => false
    }
  },
  "request_id" => "req_bravo_credits_private"
}.freeze

CREDITS_STREAM_DELAY_SECONDS = 0.1

CONTEXT_ERROR = {
  "error" => {
    "type" => "invalid_request_error",
    "code" => "context_length_exceeded",
    "message" => "Your input exceeds the context window of this model. Please adjust your input and try again."
  }
}.freeze

class CanaryEventStore
  def initialize
    @events = []
    @mutex = Mutex.new
    @sequence = 0
  end

  def record(type, model:, stream:)
    @mutex.synchronize do
      @sequence += 1
      @events << {
        "sequence" => @sequence,
        "type" => type,
        "model" => model,
        "stream" => stream,
        "at" => Time.now.utc.iso8601(6)
      }
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
  bytes = request_line.bytesize
  while (line = client.gets)
    bytes += line.bytesize
    raise CanaryProviderFailure, "headers too large" if bytes > 64 * 1024
    break if line == "\r\n" || line == "\n"

    name, value = line.split(":", 2)
    raise CanaryProviderFailure, "invalid header" unless name && value

    headers[name.strip.downcase] = value.strip
  end
  content_length = Integer(headers.fetch("content-length", "0"), 10)
  raise CanaryProviderFailure, "request body too large" if content_length > 4 * 1024 * 1024

  body = content_length.positive? ? client.read(content_length) : ""
  raise CanaryProviderFailure, "truncated request body" unless body&.bytesize == content_length

  [method, target.split("?", 2).first, headers, body]
rescue ArgumentError
  raise CanaryProviderFailure, "invalid content length"
end

def parse_payload(body)
  parsed = JSON.parse(body)
  raise CanaryProviderFailure, "request body must be an object" unless parsed.is_a?(Hash)

  parsed
rescue JSON::ParserError
  raise CanaryProviderFailure, "invalid JSON body"
end

def write_response(client, status, content_type, body, headers = {})
  reason = {
    200 => "OK",
    400 => "Bad Request",
    401 => "Unauthorized",
    404 => "Not Found",
    429 => "Too Many Requests"
  }.fetch(status, "Canary Response")
  client.write("HTTP/1.1 #{status} #{reason}\r\n")
  client.write("Content-Type: #{content_type}\r\n")
  client.write("Content-Length: #{body.bytesize}\r\n")
  client.write("Cache-Control: no-store\r\n")
  headers.each { |name, value| client.write("#{name}: #{value}\r\n") }
  client.write("Connection: close\r\n\r\n")
  client.write(body)
end

def json_response(client, status, object, headers = {})
  write_response(client, status, "application/json", JSON.generate(object), headers)
end

def authorized_control?(headers, expected)
  supplied = headers["x-canary-control"].to_s
  !supplied.empty? && supplied.bytesize == expected.bytesize &&
    supplied.bytes.zip(expected.bytes).reduce(0) { |diff, (left, right)| diff | (left ^ right) }.zero?
end

def anthropic_message(model, text)
  {
    "id" => "msg_bravo_credits_context_canary",
    "type" => "message",
    "role" => "assistant",
    "model" => model,
    "content" => [{ "type" => "text", "text" => text }],
    "stop_reason" => "end_turn",
    "stop_sequence" => nil,
    "usage" => { "input_tokens" => 7, "output_tokens" => 3 }
  }
end

def anthropic_sse(model, text)
  message = anthropic_message(model, text)
  events = [
    ["message_start", { "type" => "message_start", "message" => message.merge("content" => [], "stop_reason" => nil) }],
    ["content_block_start", {
      "type" => "content_block_start",
      "index" => 0,
      "content_block" => { "type" => "text", "text" => "" }
    }],
    ["content_block_delta", {
      "type" => "content_block_delta",
      "index" => 0,
      "delta" => { "type" => "text_delta", "text" => text }
    }],
    ["content_block_stop", { "type" => "content_block_stop", "index" => 0 }],
    ["message_delta", {
      "type" => "message_delta",
      "delta" => { "stop_reason" => "end_turn", "stop_sequence" => nil },
      "usage" => { "output_tokens" => 3 }
    }],
    ["message_stop", { "type" => "message_stop" }]
  ]
  events.map { |name, data| "event: #{name}\ndata: #{JSON.generate(data)}\n\n" }.join
end

def credits_sse_frames
  prelude = {
    "type" => "message_start",
    "message" => {
      "id" => "msg_bravo_credits_prelude",
      "type" => "message",
      "role" => "assistant",
      "model" => "claude-fable-5",
      "content" => [],
      "stop_reason" => nil,
      "stop_sequence" => nil,
      "usage" => { "input_tokens" => 7, "output_tokens" => 0 }
    }
  }
  [
    "event: message_start\ndata: #{JSON.generate(prelude)}\n\n",
    "event: error\ndata: #{JSON.generate(CREDITS_REQUIRED)}\n\n"
  ]
end

def write_http_chunk(client, payload)
  client.write("#{payload.bytesize.to_s(16)}\r\n")
  client.write(payload)
  client.write("\r\n")
  client.flush
end

def write_credits_stream(client)
  client.write("HTTP/1.1 200 OK\r\n")
  client.write("Content-Type: text/event-stream\r\n")
  client.write("Transfer-Encoding: chunked\r\n")
  client.write("Cache-Control: no-store\r\n")
  client.write("Connection: close\r\n\r\n")
  client.flush

  prelude, provider_error = credits_sse_frames
  write_http_chunk(client, prelude)
  sleep(CREDITS_STREAM_DELAY_SECONDS)
  write_http_chunk(client, provider_error)
  client.write("0\r\n\r\n")
  client.flush
end

def codex_response(model, marker)
  {
    "id" => "resp_bravo_credits_context_canary",
    "object" => "response",
    "created_at" => Time.now.to_i,
    "status" => "completed",
    "model" => model,
    "output" => [{
      "id" => "msg_bravo_credits_context_canary",
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
end

def codex_sse(model, marker)
  response = codex_response(model, marker)
  created = {
    "type" => "response.created",
    "sequence_number" => 0,
    "response" => response.merge("status" => "in_progress", "output" => [])
  }
  delta = {
    "type" => "response.output_text.delta",
    "sequence_number" => 1,
    "item_id" => "msg_bravo_credits_context_canary",
    "output_index" => 0,
    "content_index" => 0,
    "delta" => marker
  }
  completed = {
    "type" => "response.completed",
    "sequence_number" => 2,
    "response" => response
  }
  [created, delta, completed].map do |event|
    "event: #{event.fetch("type")}\ndata: #{JSON.generate(event)}\n\n"
  end.join
end

def write_codex_server_error_stream(client, model)
  created = {
    "type" => "response.created",
    "sequence_number" => 0,
    "response" => {
      "id" => "resp_bravo_reverse_prelude",
      "object" => "response",
      "created_at" => Time.now.to_i,
      "status" => "in_progress",
      "model" => model,
      "output" => []
    }
  }
  nested = {
    "error" => {
      "type" => "server_error",
      "code" => "server_error",
      "message" => "An error occurred while processing your request."
    }
  }
  terminal = {
    "type" => "error",
    "sequence_number" => 1,
    "error" => {
      "type" => "api_error",
      "message" => "model_execution_failed: #{JSON.generate(nested)}"
    }
  }

  client.write("HTTP/1.1 200 OK\r\n")
  client.write("Content-Type: text/event-stream\r\n")
  client.write("Transfer-Encoding: chunked\r\n")
  client.write("Cache-Control: no-store\r\n")
  client.write("Connection: close\r\n\r\n")
  client.flush

  write_http_chunk(
    client,
    "event: response.created\ndata: #{JSON.generate(created)}\n\n"
  )
  sleep(CREDITS_STREAM_DELAY_SECONDS)
  write_http_chunk(
    client,
    "event: error\ndata: #{JSON.generate(terminal)}\n\n"
  )
  client.write("0\r\n\r\n")
  client.flush
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
        payload = parse_payload(body)
        model = payload["model"].to_s
        stream = payload["stream"] == true
        if method == "POST" && path.end_with?("/v1/messages")
          if model == "claude-fable-5"
            events.record(stream ? "claude_credits_stream" : "claude_credits_http", model: model, stream: stream)
            if stream
              write_credits_stream(client)
            else
              json_response(client, 429, CREDITS_REQUIRED, "Retry-After" => options[:retry_after])
            end
          elsif model == "claude-sonnet-5"
            reverse_fallback = body.include?(options[:reverse_fallback_marker])
            events.record(
              reverse_fallback ? "claude_reverse_fallback_success" : "claude_sibling_success",
              model: model,
              stream: stream
            )
            marker = reverse_fallback ? options[:fallback_marker] : options[:sibling_marker]
            if stream
              write_response(client, 200, "text/event-stream", anthropic_sse(model, marker))
            else
              json_response(client, 200, anthropic_message(model, marker))
            end
          else
            events.record("claude_unknown_model", model: model, stream: stream)
            json_response(client, 404, "error" => "unknown_model")
          end
        elsif method == "POST" && path.end_with?("/responses")
          if body.include?(options[:reverse_fallback_marker])
            events.record("codex_server_error_stream", model: model, stream: stream)
            if stream
              write_codex_server_error_stream(client, model)
            else
              json_response(
                client,
                502,
                "error" => {
                  "type" => "server_error",
                  "code" => "server_error",
                  "message" => "An error occurred while processing your request."
                }
              )
            end
          elsif body.include?(options[:context_marker])
            events.record("codex_context_error", model: model, stream: stream)
            json_response(client, 400, CONTEXT_ERROR)
          else
            events.record("codex_fallback_success", model: model, stream: stream)
            marker = options[:fallback_marker]
            if stream
              write_response(client, 200, "text/event-stream", codex_sse(model, marker))
            else
              json_response(client, 200, codex_response(model, marker))
            end
          end
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
