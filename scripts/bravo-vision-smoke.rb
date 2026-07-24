#!/usr/bin/env ruby
# frozen_string_literal: true

# Live vision-input conformance test for Anthropic Messages routes.
#
# The script sends a tiny synthetic PNG, loads credentials only from mode-0600
# files, and reports response metadata without printing keys or model output.

require "json"
require "net/http"
require "optparse"
require "uri"
require "yaml"
require "base64"
require "zlib"

class BravoVisionSmoke
  MARKER = "BRAVO_VISION_OK"

  def initialize(options)
    @base_uri = URI(options.fetch(:base_url))
    @models = options.fetch(:models)
    @placements = options.fetch(:placements)
    @effort = options.fetch(:effort)
    @max_tokens = options.fetch(:max_tokens)
    @timeout = options.fetch(:timeout)
    @smart_key = read_secret(options[:smart_key_file])
    @key = options.fetch(:key_mode) == "smart" ? @smart_key : ordinary_key(options.fetch(:config))
    raise "no usable API key" if @key.nil? || @key.empty?
  end

  def run
    results = @models.product(@placements, [false, true]).map do |model, placement, stream|
      response = post_message(model, placement, stream)
      result = inspect_response(model, placement, stream, response)
      puts JSON.generate(result)
      result
    end
    results.all? { |result| result["passed"] } ? 0 : 1
  ensure
    @key = nil
    @smart_key = nil
  end

  private

  def post_message(model, placement, stream)
    uri = @base_uri.dup
    uri.path = join_paths(@base_uri.path, "/v1/messages")
    request = Net::HTTP::Post.new(uri)
    request["x-api-key"] = @key
    request["anthropic-version"] = "2023-06-01"
    request["content-type"] = "application/json"
    request["accept"] = stream ? "text/event-stream" : "application/json"
    payload = {
      "model" => model,
      "max_tokens" => @max_tokens,
      "stream" => stream,
      "messages" => vision_messages(placement)
    }
    unless @effort.empty?
      payload["thinking"] = { "type" => "adaptive" }
      payload["output_config"] = { "effort" => @effort }
    end
    request.body = JSON.generate(payload)
    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = uri.scheme == "https"
    http.open_timeout = [@timeout, 10].min
    http.read_timeout = @timeout
    http.request(request)
  end

  def inspect_response(model, placement, stream, response)
    status = response.code.to_i
    result = {
      "model" => model,
      "placement" => placement,
      "effort" => @effort,
      "stream" => stream,
      "status" => status,
      "content_type" => response["content-type"].to_s.split(";").first.to_s
    }
    if stream
      if !status.between?(200, 299) && response.body.to_s.lstrip.start_with?("{")
        root = JSON.parse(response.body)
        result["error_code"] = root.dig("error", "code").to_s
        result["error_type"] = root.dig("error", "type").to_s
        result["error_message"] = sanitized_error(root.dig("error", "message"))
        result["passed"] = false
        return result
      end
      events = response.body.to_s.each_line.map do |line|
        raw = line.strip.sub(/\Adata:\s*/, "")
        next if raw.empty? || raw == "[DONE]" || !raw.start_with?("{")

        JSON.parse(raw)
      rescue JSON::ParserError
        nil
      end.compact
      text = events.map do |event|
        event.dig("delta", "text") if event["type"] == "content_block_delta"
      end.compact.join
      result["message_stop"] = events.any? { |event| event["type"] == "message_stop" }
      result["text_bytes"] = text.bytesize
      result["marker"] = text.include?(MARKER)
      error = events.find { |event| event["type"] == "error" }
      result["error_code"] = error&.dig("error", "code").to_s
      result["passed"] = status.between?(200, 299) && result["message_stop"] && result["text_bytes"].positive?
    else
      root = JSON.parse(response.body)
      content = Array(root["content"])
      text = content.map do |item|
        item["text"] if item.is_a?(Hash) && item["type"] == "text"
      end.compact.join
      result["content_types"] = content.map { |item| item["type"].to_s if item.is_a?(Hash) }.compact.uniq.sort
      result["text_bytes"] = text.bytesize
      result["stop_reason"] = root["stop_reason"].to_s
      result["marker"] = text.include?(MARKER)
      result["error_code"] = root.dig("error", "code").to_s
      result["error_type"] = root.dig("error", "type").to_s
      result["error_message"] = sanitized_error(root.dig("error", "message"))
      result["passed"] = status.between?(200, 299) && result["text_bytes"].positive?
    end
    result
  rescue JSON::ParserError
    result["error_code"] = "invalid_json"
    result["passed"] = false
    result
  end

  def ordinary_key(config_path)
    config = YAML.load_file(config_path)
    Array(config["api-keys"]).map do |item|
      case item
      when String
        item.strip
      when Hash
        %w[key api-key value].map { |name| item[name].to_s.strip }.find { |value| !value.empty? }
      end
    end.compact.find { |value| !value.empty? && value != @smart_key && !value.start_with?("your-api-key-") }
  end

  def read_secret(path)
    return nil if path.nil?

    expanded = File.expand_path(path)
    mode = File.stat(expanded).mode & 0o777
    raise "secret file must have mode 0600" unless mode == 0o600

    value = File.read(expanded, mode: "r:BOM|UTF-8").strip
    value.empty? ? nil : value
  end

  def synthetic_png_base64
    width = 64
    height = 64
    row = [0].pack("C") + ([255, 255, 255] * width).pack("C*")
    raw = row * height
    png = "\x89PNG\r\n\x1A\n".b +
          png_chunk("IHDR", [width, height, 8, 2, 0, 0, 0].pack("NNCCCCC")) +
          png_chunk("IDAT", Zlib::Deflate.deflate(raw)) +
          png_chunk("IEND", "".b)
    Base64.strict_encode64(png)
  end

  def vision_messages(placement)
    image = {
      "type" => "image",
      "source" => {
        "type" => "base64",
        "media_type" => "image/png",
        "data" => synthetic_png_base64
      }
    }
    prompt = {
      "type" => "text",
      "text" => "Synthetic vision conformance check. Reply exactly #{MARKER}."
    }
    return [{ "role" => "user", "content" => [image, prompt] }] if placement == "user"

    [{
      "role" => "assistant",
      "content" => [{
        "type" => "tool_use",
        "id" => "toolu_bravo_vision_smoke",
        "name" => "inspect_screenshot",
        "input" => {}
      }]
    }, {
      "role" => "user",
      "content" => [{
        "type" => "tool_result",
        "tool_use_id" => "toolu_bravo_vision_smoke",
        "content" => [
          image,
          { "type" => "text", "text" => "Synthetic screenshot result." }
        ]
      }, prompt]
    }]
  end

  def png_chunk(type, data)
    payload = type.b + data
    [data.bytesize].pack("N") + payload + [Zlib.crc32(payload)].pack("N")
  end

  def sanitized_error(message)
    text = message.to_s
    text = text.gsub(@key.to_s, "[REDACTED]") unless @key.to_s.empty?
    text[0, 240]
  end

  def join_paths(base, path)
    "#{base.to_s.sub(%r{/$}, "")}/#{path.to_s.sub(%r{^/}, "")}"
  end
end

options = {
  base_url: "http://127.0.0.1:18319",
  config: "config.yaml",
  key_mode: "ordinary",
  models: %w[claude-opus-4-8 gpt-5.6-sol],
  placements: %w[user tool_result],
  timeout: 180,
  max_tokens: 512,
  effort: ""
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-vision-smoke.rb [options]"
  parser.on("--base-url URL") { |value| options[:base_url] = value }
  parser.on("--config PATH") { |value| options[:config] = value }
  parser.on("--smart-key-file PATH") { |value| options[:smart_key_file] = value }
  parser.on("--key-mode MODE", %w[ordinary smart]) { |value| options[:key_mode] = value }
  parser.on("--models LIST") { |value| options[:models] = value.split(",").map(&:strip).reject(&:empty?) }
  parser.on("--placements LIST") { |value| options[:placements] = value.split(",").map(&:strip).reject(&:empty?) }
  parser.on("--timeout SECONDS", Integer) { |value| options[:timeout] = value }
  parser.on("--max-tokens TOKENS", Integer) { |value| options[:max_tokens] = value }
  parser.on("--effort LEVEL") { |value| options[:effort] = value.to_s.strip.downcase }
end.parse!

begin
  exit BravoVisionSmoke.new(options).run
rescue StandardError => error
  warn JSON.generate(
    "status" => 0,
    "error_code" => error.class.name,
    "error_message" => error.message.to_s[0, 200]
  )
  exit 1
end
