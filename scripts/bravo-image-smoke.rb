#!/usr/bin/env ruby
# frozen_string_literal: true

# Read-only image generation/edit smoke test for a CLIProxyAPI canary.
# Secrets are loaded from files and image payloads are never written or printed.
#
# Direct physical-model probe:
#   ruby scripts/bravo-image-smoke.rb \
#     --base-url http://127.0.0.1:18319 \
#     --config /path/to/config.yaml \
#     --key-mode ordinary \
#     --model gpt-image-2
#
# Bravo probe:
#   ruby scripts/bravo-image-smoke.rb \
#     --base-url http://127.0.0.1:18319 \
#     --config /path/to/config.yaml \
#     --smart-key-file /path/to/bravo-smart-key.txt \
#     --key-mode smart \
#     --model bravo/image

require "json"
require "net/http"
require "optparse"
require "uri"
require "yaml"

class BravoImageSmoke
  def initialize(options)
    @base_uri = URI(options.fetch(:base_url))
    @timeout = options.fetch(:timeout)
    @model = options.fetch(:model)
    @config = YAML.load_file(options.fetch(:config))
    @smart_key = read_secret(options[:smart_key_file])
    @key_mode = options.fetch(:key_mode)
    @stream_only = options.fetch(:stream_only)
    @skip_stream = options.fetch(:skip_stream)
    @key = @key_mode == "smart" ? @smart_key : ordinary_key
    raise "no usable #{@key_mode} API key" if @key.nil? || @key.empty?
  end

  def run
    unless @stream_only
      generation, generation_elapsed = post_json(
        "/v1/images/generations",
        {
          "model" => @model,
          "prompt" => "A simple centered red circle on a plain white background",
          "size" => "1024x1024",
          "n" => 1,
          "response_format" => "b64_json"
        }
      )
      generated = report("generation", generation, generation_elapsed)
      return 2 unless success?(generation) && generated

      image_reference = generated.fetch(:reference)
      edit, edit_elapsed = post_json(
        "/v1/images/edits",
        {
          "model" => @model,
          "prompt" => "Change the circle from red to blue and keep the plain white background",
          "images" => [{ "image_url" => image_reference }],
          "size" => "1024x1024",
          "n" => 1,
          "response_format" => "b64_json"
        }
      )
      edited = report("edit", edit, edit_elapsed)
      return 3 unless success?(edit) && edited
      return 0 if @skip_stream
    end

    stream = post_stream(
      "/v1/images/generations",
      {
        "model" => @model,
        "prompt" => "A simple centered green triangle on a plain white background",
        "size" => "1024x1024",
        "n" => 1,
        "response_format" => "b64_json",
        "partial_images" => 1,
        "stream" => true
      }
    )
    puts JSON.generate(stream)
    stream["status"].between?(200, 299) && stream["completed"] ? 0 : 4
  ensure
    @key = nil
    @smart_key = nil
  end

  private

  def post_json(path, payload)
    uri = @base_uri.dup
    uri.path = join_paths(@base_uri.path, path)
    request = Net::HTTP::Post.new(uri)
    request["Authorization"] = "Bearer #{@key}"
    request["Content-Type"] = "application/json"
    request.body = JSON.generate(payload)
    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = uri.scheme == "https"
    http.open_timeout = [@timeout, 10].min
    http.read_timeout = @timeout
    started = monotonic
    response = http.request(request)
    [response, monotonic - started]
  end

  def report(phase, response, elapsed)
    root = JSON.parse(response.body)
    data = Array(root["data"])
    item = data.first.is_a?(Hash) ? data.first : {}
    b64 = item["b64_json"].to_s
    url = item["url"].to_s
    error = root["error"].is_a?(Hash) ? root["error"] : {}
    puts JSON.generate(
      "phase" => phase,
      "status" => response.code.to_i,
      "elapsed_s" => elapsed.round(2),
      "items" => data.length,
      "has_b64" => !b64.empty?,
      "has_url" => !url.empty?,
      "encoded_bytes" => b64.bytesize,
      "error_code" => error["code"].to_s,
      "error_message" => sanitize(error["message"].to_s)[0, 240]
    )
    reference = !b64.empty? ? "data:image/png;base64,#{b64}" : url
    reference.empty? ? nil : { reference: reference }
  rescue JSON::ParserError
    puts JSON.generate(
      "phase" => phase,
      "status" => response.code.to_i,
      "elapsed_s" => elapsed.round(2),
      "items" => 0,
      "error_code" => "invalid_json",
      "error_message" => "response was not valid JSON"
    )
    nil
  end

  def post_stream(path, payload)
    uri = @base_uri.dup
    uri.path = join_paths(@base_uri.path, path)
    request = Net::HTTP::Post.new(uri)
    request["Authorization"] = "Bearer #{@key}"
    request["Content-Type"] = "application/json"
    request["Accept"] = "text/event-stream"
    request.body = JSON.generate(payload)
    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = uri.scheme == "https"
    http.open_timeout = [@timeout, 10].min
    http.read_timeout = @timeout

    status = 0
    content_type = ""
    total_bytes = 0
    completed = false
    partial = false
    requested_model_seen = false
    physical_models_seen = {}
    event_names = []
    data_types = []
    data_root_keys = []
    tail = ""
    line_buffer = String.new
    patterns = ["image_generation.completed", "image_generation.partial_image", @model, "gpt-image-2", "gpt-image-1.5"]
    keep_bytes = patterns.map(&:bytesize).max + 8
    started = monotonic
    http.request(request) do |response|
      status = response.code.to_i
      content_type = response["Content-Type"].to_s
      response.read_body do |chunk|
        total_bytes += chunk.bytesize
        scan = tail + chunk
        completed ||= scan.include?("image_generation.completed")
        partial ||= scan.include?("image_generation.partial_image")
        requested_model_seen ||= scan.include?(@model)
        %w[gpt-image-2 gpt-image-1.5].each do |physical|
          physical_models_seen[physical] = true if scan.include?(physical)
        end
        tail = scan.byteslice(-keep_bytes, keep_bytes) || scan
        line_buffer << chunk
        while (newline = line_buffer.index("\n"))
          line = line_buffer.slice!(0, newline + 1)
          inspect_stream_line(line, event_names, data_types, data_root_keys)
        end
      end
    end
    inspect_stream_line(line_buffer, event_names, data_types, data_root_keys) unless line_buffer.empty?
    completed ||= (event_names + data_types).any? { |name| name.include?("completed") }
    partial ||= (event_names + data_types).any? { |name| name.include?("partial") }
    completed ||= data_root_keys.any? { |keys| keys.include?("data") }

    {
      "phase" => "stream",
      "status" => status,
      "elapsed_s" => (monotonic - started).round(2),
      "content_type" => content_type.split(";").first.to_s,
      "bytes" => total_bytes,
      "partial" => partial,
      "completed" => completed,
      "requested_model_seen" => requested_model_seen,
      "physical_models_seen" => physical_models_seen.keys.sort,
      "event_names" => event_names.uniq.sort,
      "data_types" => data_types.uniq.sort,
      "data_root_keys" => data_root_keys.uniq.sort
    }
  end

  def inspect_stream_line(line, event_names, data_types, data_root_keys)
    stripped = line.to_s.strip
    if stripped.start_with?("event:")
      event_names << stripped.sub(/\Aevent:\s*/, "")[0, 120]
      return
    end
    raw = if stripped.start_with?("data:")
            stripped.sub(/\Adata:\s*/, "")
          elsif stripped.start_with?("{")
            stripped
          else
            return
          end
    return if raw.empty? || raw == "[DONE]"

    value = JSON.parse(raw)
    return unless value.is_a?(Hash)

    data_types << value["type"].to_s[0, 120] unless value["type"].to_s.empty?
    data_root_keys << value.keys.map(&:to_s).sort.join(",")[0, 240]
  rescue JSON::ParserError
    data_types << "invalid_json"
  end

  def ordinary_key
    Array(@config["api-keys"]).map do |item|
      case item
      when String
        item.strip
      when Hash
        %w[key api-key value].map { |name| item[name].to_s.strip }.find { |value| !value.empty? }
      end
    end.compact.find do |value|
      !value.empty? && value != @smart_key && !value.start_with?("your-api-key-")
    end
  end

  def read_secret(path)
    return nil if path.nil?

    value = File.read(File.expand_path(path), mode: "r:BOM|UTF-8").strip
    value.empty? ? nil : value
  end

  def sanitize(message)
    text = message.to_s.dup
    [@key, @smart_key].compact.each do |secret|
      text.gsub!(secret, "[REDACTED]") unless secret.empty?
    end
    text.gsub!(/\bbrv_[A-Za-z0-9_-]{16,}\b/, "brv_[REDACTED]")
    text.gsub!(/\bsk-[A-Za-z0-9_-]{16,}\b/, "sk-[REDACTED]")
    text
  end

  def success?(response)
    response.code.to_i.between?(200, 299)
  end

  def join_paths(base, path)
    "#{base.to_s.sub(%r{/$}, "")}/#{path.to_s.sub(%r{^/}, "")}"
  end

  def monotonic
    Process.clock_gettime(Process::CLOCK_MONOTONIC)
  end
end

options = {
  base_url: "http://127.0.0.1:18319",
  config: "config.yaml",
  key_mode: "smart",
  model: "bravo/image",
  timeout: 240,
  stream_only: false,
  skip_stream: false
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-image-smoke.rb [options]"
  parser.on("--base-url URL") { |value| options[:base_url] = value }
  parser.on("--config PATH") { |value| options[:config] = value }
  parser.on("--smart-key-file PATH") { |value| options[:smart_key_file] = value }
  parser.on("--key-mode MODE", %w[smart ordinary]) { |value| options[:key_mode] = value }
  parser.on("--model MODEL") { |value| options[:model] = value }
  parser.on("--timeout SECONDS", Integer) { |value| options[:timeout] = value }
  parser.on("--stream-only") { options[:stream_only] = true }
  parser.on("--skip-stream") { options[:skip_stream] = true }
end.parse!

begin
  exit BravoImageSmoke.new(options).run
rescue StandardError => error
  warn JSON.generate(
    "phase" => "setup",
    "status" => 0,
    "error_code" => error.class.name,
    "error_message" => error.message.to_s[0, 240]
  )
  exit 1
end
