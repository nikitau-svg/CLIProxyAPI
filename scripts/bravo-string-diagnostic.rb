#!/usr/bin/env ruby
# frozen_string_literal: true

# Safe diagnostic for the OpenAI Responses string-input compatibility path.
# Secrets and assistant/user text are never printed.

require "json"
require "net/http"
require "optparse"
require "uri"

MARKER = "BRAVO_SMOKE_OK"
PROMPT = "What is the exact stdout of this Python program? Reply with stdout only.\n\nprint(#{MARKER.inspect})"

options = {
  base_url: ENV.fetch("BRAVO_BASE_URL", "http://127.0.0.1:18319"),
  smart_key_file: ENV.fetch("BRAVO_SMART_KEY_FILE", "bravo-smart-key.txt"),
  model: ENV.fetch("BRAVO_TEXT_MODEL", "bravo/haiku"),
  timeout: Integer(ENV.fetch("BRAVO_SMOKE_TIMEOUT", "120"), 10)
}

OptionParser.new do |opts|
  opts.on("--base-url URL") { |value| options[:base_url] = value }
  opts.on("--smart-key-file PATH") { |value| options[:smart_key_file] = value }
  opts.on("--model ID") { |value| options[:model] = value }
  opts.on("--timeout SECONDS", Integer) { |value| options[:timeout] = value }
end.parse!
abort("unexpected positional arguments") unless ARGV.empty?

key = File.read(File.expand_path(options.fetch(:smart_key_file)), mode: "r:BOM|UTF-8").strip
abort("smart key file is empty") if key.empty?

message = {
  "type" => "message",
  "role" => "user",
  "content" => [{ "type" => "input_text", "text" => PROMPT }]
}
variants = {
  "string_default" => { "input" => PROMPT },
  "string_stream_false" => { "input" => PROMPT, "stream" => false },
  "canonical_default" => { "input" => [message] },
  "canonical_stream_false" => { "input" => [message], "stream" => false }
}

uri = URI(options.fetch(:base_url))
uri.path = "/v1/responses"
uri.query = nil

variants.each do |name, fields|
  payload = {
    "model" => options.fetch(:model),
    "max_output_tokens" => 512
  }.merge(fields)
  request = Net::HTTP::Post.new(uri)
  request["Authorization"] = "Bearer #{key}"
  request["Accept"] = "application/json"
  request["Content-Type"] = "application/json"
  request.body = JSON.generate(payload)

  http = Net::HTTP.new(uri.host, uri.port)
  http.use_ssl = uri.scheme == "https"
  http.open_timeout = [options.fetch(:timeout), 10].min
  http.read_timeout = options.fetch(:timeout)
  response = http.request(request)
  summary = { "case" => name, "status" => response.code.to_i }

  begin
    root = JSON.parse(response.body)
    output = Array(root["output"])
    content = output.flat_map { |item| item.is_a?(Hash) ? Array(item["content"]) : [] }
    text = content.map do |item|
      item["text"].to_s if item.is_a?(Hash) && %w[output_text text].include?(item["type"])
    end.compact.join
    summary.merge!(
      "response_status" => root["status"],
      "logical_model" => root["model"] == options.fetch(:model),
      "output_types" => output.map { |item| item["type"] if item.is_a?(Hash) }.compact.uniq,
      "content_types" => content.map { |item| item["type"] if item.is_a?(Hash) }.compact.uniq,
      "content_keys" => content.map { |item| item.keys.sort if item.is_a?(Hash) }.compact.uniq,
      "text_bytes" => text.bytesize,
      "marker_in_parsed_text" => text.include?(MARKER),
      "marker_anywhere" => response.body.include?(MARKER),
      "error_type" => root.dig("error", "type"),
      "error_code" => root.dig("error", "code")
    )
  rescue JSON::ParserError => error
    summary["json_error"] = error.class.name
    summary["body_bytes"] = response.body.bytesize
  end
  puts JSON.generate(summary)
end

key = nil
