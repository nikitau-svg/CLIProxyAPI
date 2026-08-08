#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "net/http"
require "optparse"
require "uri"

class ToolAliasCanaryFailure < StandardError; end

PAIRS = [
  %w[memory_get memory_search],
  %w[message sessions_list],
  %w[message sessions_spawn],
  %w[sessions_list subagents],
  %w[sessions_spawn subagents]
].freeze

options = {
  base_url: "http://127.0.0.1:18319",
  api_key: ENV.fetch("BRAVO_CANARY_API_KEY", "bravo-tool-alias-canary-key"),
  model: "claude-sonnet-5"
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-tool-alias-canary.rb [options]"
  parser.on("--base-url URL", "Loopback canary URL") { |value| options[:base_url] = value }
  parser.on("--api-key KEY", "Canary-only API key") { |value| options[:api_key] = value }
  parser.on("--model MODEL", "Claude OAuth physical model") { |value| options[:model] = value }
end.parse!

base = URI(options.fetch(:base_url))
unless base.scheme == "http" && ["127.0.0.1", "localhost"].include?(base.host) && base.port == 18_319
  abort("refusing non-loopback or non-canary target")
end

def tool(name)
  {
    "name" => name,
    "description" => "Canary tool",
    "input_schema" => { "type" => "object", "properties" => {}, "required" => [] }
  }
end

def request(base, api_key, payload, stream: false)
  uri = base + "/v1/messages"
  req = Net::HTTP::Post.new(uri)
  req["x-api-key"] = api_key
  req["anthropic-version"] = "2023-06-01"
  req["content-type"] = "application/json"
  request_payload = stream ? payload.merge("stream" => true) : payload
  req.body = JSON.generate(request_payload)

  response = nil
  chunks = +""
  Net::HTTP.start(uri.host, uri.port, read_timeout: 60) do |http|
    http.request(req) do |res|
      response = res
      res.read_body { |chunk| chunks << chunk }
    end
  end
  [response, chunks]
end

def error_summary(body)
  parsed = JSON.parse(body)
  error = parsed["error"] || parsed
  [error["type"], error["code"], error["message"]].compact.join(": ")[0, 400]
rescue JSON::ParserError
  body.to_s.gsub(/\s+/, " ")[0, 400]
end

def response_tool_names(body)
  parsed = JSON.parse(body)
  Array(parsed["content"]).each_with_object([]) do |block, names|
    names << block["name"] if block["type"] == "tool_use"
  end
end

def stream_tool_names(body)
  body.each_line.each_with_object([]) do |line, names|
    next unless line.start_with?("data:")

    data = line.delete_prefix("data:").strip
    next if data.empty? || data == "[DONE]"

    parsed = JSON.parse(data)
    block = parsed["content_block"]
    names << block["name"] if block.is_a?(Hash) && block["type"] == "tool_use"
  rescue JSON::ParserError
    next
  end
end

failures = []

PAIRS.each do |left, right|
  payload = {
    "model" => options.fetch(:model),
    "max_tokens" => 64,
    "temperature" => 1,
    "tools" => [tool(left), tool(right)],
    "messages" => [{ "role" => "user", "content" => "ok" }]
  }
  response, body = request(base, options.fetch(:api_key), payload)
  if response.code.to_i == 200
    puts "PASS pair #{left} + #{right}: HTTP 200"
  else
    failures << "#{left} + #{right}: HTTP #{response.code}: #{error_summary(body)}"
    puts "FAIL pair #{left} + #{right}: HTTP #{response.code}"
  end
end

roundtrip_name = "memory_get"
roundtrip_payload = {
  "model" => options.fetch(:model),
  "max_tokens" => 64,
  "tools" => [tool(roundtrip_name), tool("memory_search")],
  "tool_choice" => { "type" => "tool", "name" => roundtrip_name },
  "messages" => [{ "role" => "user", "content" => "Call the selected tool once." }]
}

response, body = request(base, options.fetch(:api_key), roundtrip_payload)
names = response.code.to_i == 200 ? response_tool_names(body) : []
if response.code.to_i == 200 && names.include?(roundtrip_name) && names.none? { |name| name.start_with?("mcp__bravo__tool_") }
  puts "PASS nonstream reverse mapping: #{roundtrip_name}"
else
  failures << "nonstream reverse mapping: HTTP #{response.code}: names=#{names.inspect}: #{error_summary(body)}"
  puts "FAIL nonstream reverse mapping"
end

response, body = request(base, options.fetch(:api_key), roundtrip_payload, stream: true)
names = response.code.to_i == 200 ? stream_tool_names(body) : []
if response.code.to_i == 200 && names.include?(roundtrip_name) && names.none? { |name| name.start_with?("mcp__bravo__tool_") }
  puts "PASS stream reverse mapping: #{roundtrip_name}"
else
  failures << "stream reverse mapping: HTTP #{response.code}: names=#{names.inspect}: #{error_summary(body)}"
  puts "FAIL stream reverse mapping"
end

if failures.empty?
  puts "Tool alias canary: 7 passed, 0 failed"
  exit 0
end

warn "Tool alias canary: #{7 - failures.length} passed, #{failures.length} failed"
failures.each { |failure| warn "- #{failure}" }
exit 1
