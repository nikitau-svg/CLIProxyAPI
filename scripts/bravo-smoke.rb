#!/usr/bin/env ruby
# frozen_string_literal: true

# Read-only end-to-end smoke tests for a Bravo canary.
#
# Secrets are loaded from files and are never accepted as command-line values
# or printed. The suite only sends inference and model-list requests; it does
# not call any configuration or management mutation endpoint.
#
# Example:
#   ruby scripts/bravo-smoke.rb \
#     --base-url http://127.0.0.1:18319 \
#     --config /path/to/config.yaml \
#     --smart-key-file /path/to/bravo-smart-key.txt

require "json"
require "net/http"
require "optparse"
require "time"
require "uri"
require "yaml"

Response = Struct.new(:status, :headers, :body, :first_chunk_ms)
CheckFailure = Class.new(StandardError)

class BravoSmoke
  DEFAULT_TEXT_MODEL = "bravo/haiku"
  DEFAULT_TOOL_MODEL = "bravo/haiku"
  DEFAULT_EXACT_MODEL = "claude-haiku-4-5-20251001"
  MARKER = "BRAVO_SMOKE_OK"
  TOOL_NAME = "lookup_weather"
  MAX_RESPONSE_BYTES = 8 * 1024 * 1024

  def initialize(options)
    @base_uri = URI(options.fetch(:base_url))
    @smart_key = read_secret(options.fetch(:smart_key_file), "Bravo smart key")
    config = YAML.load_file(options.fetch(:config))
    @ordinary_key = ordinary_key(config)
    @timeout = options.fetch(:timeout)
    @text_model = options.fetch(:text_model)
    @tool_model = options.fetch(:tool_model)
    @exact_model = options.fetch(:exact_model)
    @only = options[:only]
    @secrets = [@smart_key, @ordinary_key].compact.reject(&:empty?)
    @results = []
  end

  def run
    check("OpenAI Responses string-input regression") { check_openai_responses_string_input }
    check("model catalog advertises Bravo") { check_model_catalog }
    check("OpenAI Chat non-stream") { check_openai_chat }
    check("OpenAI Responses non-stream") { check_openai_responses }
    check("Anthropic Messages non-stream") { check_anthropic_messages }
    check("Anthropic count_tokens") { check_anthropic_count_tokens }
    check("smart key routes unprefixed exact model") { check_smart_unprefixed }
    check("ordinary key cannot use Bravo namespace") { check_ordinary_bravo_denied }
    check("ordinary key keeps native unprefixed routing") { check_ordinary_unprefixed }
    check("OpenAI function tool contract") { check_openai_tool }
    check("Anthropic function tool contract") { check_anthropic_tool }
    check("OpenAI Chat stream") { check_openai_chat_stream }
    check("OpenAI Responses stream") { check_openai_responses_stream }
    check("Anthropic Messages stream") { check_anthropic_messages_stream }

    passed = @results.count { |item| item[:status] == :pass }
    failed = @results.count { |item| item[:status] == :fail }
    skipped = @results.count { |item| item[:status] == :skip }
    puts
    puts "Bravo smoke: #{passed} passed, #{failed} failed, #{skipped} skipped"
    failed.zero? ? 0 : 1
  ensure
    # Reduce the lifetime of references held by this object. Ruby strings
    # cannot be reliably zeroed, so callers should run this as a short-lived
    # process and keep the secret files mode 0600.
    @smart_key = nil
    @ordinary_key = nil
    @secrets = []
  end

  private

  def check(name)
    if @only && !name.match?(@only)
      @results << { name: name, status: :skip }
      puts "SKIP  #{name}"
      return
    end
    started = monotonic
    yield
    elapsed = ((monotonic - started) * 1000).round
    @results << { name: name, status: :pass }
    puts format("PASS  %-48s %6d ms", name, elapsed)
  rescue CheckFailure => error
    @results << { name: name, status: :fail }
    puts "FAIL  #{name}: #{sanitize(error.message)}"
  rescue StandardError => error
    @results << { name: name, status: :fail }
    puts "FAIL  #{name}: #{sanitize("#{error.class}: #{error.message}")}"
  end

  def check_openai_responses_string_input
    payload = {
      "model" => @text_model,
      "input" => marker_prompt,
      "max_output_tokens" => 512
    }
    response = request(:post, "/v1/responses", @smart_key, payload, {}, false)
    root = success_json(response)
    assert(root["model"] == @text_model, "logical model was not preserved")
    assert(response_output_text(root).include?(MARKER), "assistant marker is missing")
  end

  def check_model_catalog
    response = request(:get, "/v1/models", @smart_key, nil, {}, false)
    root = success_json(response)
    ids = Array(root["data"]).map { |item| item["id"] if item.is_a?(Hash) }.compact
    assert(ids.include?(@text_model), "#{@text_model} is absent from /v1/models")
    assert(ids.any? { |id| id.start_with?("bravo/") }, "no Bravo models were advertised")
  end

  def check_openai_chat
    response = request(
      :post,
      "/v1/chat/completions",
      @smart_key,
      chat_payload(@text_model, false),
      {},
      false
    )
    root = success_json(response)
    assert(root["model"] == @text_model, "logical model was not preserved")
    text = root.dig("choices", 0, "message", "content").to_s
    assert(text.include?(MARKER), "assistant marker is missing")
  end

  def check_openai_responses
    response = request(
      :post,
      "/v1/responses",
      @smart_key,
      responses_payload(@text_model, false),
      {},
      false
    )
    root = success_json(response)
    assert(root["model"] == @text_model, "logical model was not preserved")
    assert(response_output_text(root).include?(MARKER), "assistant marker is missing")
  end

  def check_anthropic_messages
    response = request(
      :post,
      "/v1/messages",
      @smart_key,
      anthropic_payload(@text_model, false),
      anthropic_headers,
      false,
      :x_api_key
    )
    root = success_json(response)
    assert(root["model"] == @text_model, "logical model was not preserved")
    assert(anthropic_text(root).include?(MARKER), "assistant marker is missing")
  end

  def check_anthropic_count_tokens
    payload = {
      "model" => @text_model,
      "messages" => [{ "role" => "user", "content" => "Count this Bravo smoke input." }]
    }
    response = request(
      :post,
      "/v1/messages/count_tokens",
      @smart_key,
      payload,
      anthropic_headers,
      false,
      :x_api_key
    )
    root = success_json(response)
    count = root["input_tokens"]
    assert(count.is_a?(Integer) && count.positive?, "input_tokens must be a positive integer")
  end

  def check_openai_chat_stream
    response = request(
      :post,
      "/v1/chat/completions",
      @smart_key,
      chat_payload(@text_model, true),
      {},
      true
    )
    events = success_sse(response)
    models = events.map { |event| event["model"] }.compact
    assert(models.empty? || models.all? { |model| model == @text_model }, "stream leaked a physical model")
    text = events.map { |event| event.dig("choices", 0, "delta", "content") }.compact.join
    assert(text.include?(MARKER), "streamed assistant marker is missing")
    assert(response.first_chunk_ms, "stream produced no payload")
  end

  def check_openai_responses_stream
    response = request(
      :post,
      "/v1/responses",
      @smart_key,
      responses_payload(@text_model, true),
      {},
      true
    )
    events = success_sse(response)
    assert(events.any? { |event| event["type"] == "response.completed" }, "response.completed is missing")
    models = events.map { |event| event.dig("response", "model") }.compact
    assert(models.empty? || models.all? { |model| model == @text_model }, "stream leaked a physical model")
    text = events.map do |event|
      event["delta"] if event["type"] == "response.output_text.delta"
    end.compact.join
    completed_text = events.map do |event|
      response_output_text(event["response"]) if event["type"] == "response.completed" && event["response"].is_a?(Hash)
    end.compact.join
    assert(
      (text + completed_text).include?(MARKER),
      "streamed assistant marker is missing " \
      "(event_types=#{events.map { |event| event["type"] }.compact.uniq.join(",")}; " \
      "delta_bytes=#{text.bytesize}; completed_bytes=#{completed_text.bytesize})"
    )
    assert(response.first_chunk_ms, "stream produced no payload")
  end

  def check_anthropic_messages_stream
    response = request(
      :post,
      "/v1/messages",
      @smart_key,
      anthropic_payload(@text_model, true),
      anthropic_headers,
      true,
      :x_api_key
    )
    events = success_sse(response)
    assert(events.any? { |event| event["type"] == "message_stop" }, "message_stop is missing")
    models = events.map { |event| event.dig("message", "model") }.compact
    assert(models.empty? || models.all? { |model| model == @text_model }, "stream leaked a physical model")
    text = events.map do |event|
      event.dig("delta", "text") if event["type"] == "content_block_delta"
    end.compact.join
    if text.strip.empty?
      event_types = events.map { |event| event["type"] }.compact.uniq.join(",")
      delta_types = events.map { |event| event.dig("delta", "type") }.compact.uniq.join(",")
      raise CheckFailure,
            "stream contained no assistant text " \
            "(event_types=#{event_types}; delta_types=#{delta_types}; text_bytes=#{text.bytesize})"
    end
    assert(response.first_chunk_ms, "stream produced no payload")
  end

  def check_openai_tool
    payload = {
      "model" => @tool_model,
      "messages" => [{
        "role" => "user",
        "content" => "Call #{TOOL_NAME} for Tbilisi. Do not answer in prose."
      }],
      "tools" => [openai_tool_definition],
      "tool_choice" => { "type" => "function", "function" => { "name" => TOOL_NAME } },
      "max_tokens" => 512
    }
    response = request(:post, "/v1/chat/completions", @smart_key, payload, {}, false)
    root = success_json(response)
    calls = Array(root.dig("choices", 0, "message", "tool_calls"))
    call = calls.find { |item| item.dig("function", "name") == TOOL_NAME }
    assert(call, "forced function call is missing")
    JSON.parse(call.dig("function", "arguments").to_s)
  end

  def check_anthropic_tool
    payload = {
      "model" => @tool_model,
      "messages" => [{
        "role" => "user",
        "content" => "Call #{TOOL_NAME} for Tbilisi. Do not answer in prose."
      }],
      "tools" => [anthropic_tool_definition],
      "tool_choice" => { "type" => "tool", "name" => TOOL_NAME },
      "max_tokens" => 512
    }
    response = request(
      :post,
      "/v1/messages",
      @smart_key,
      payload,
      anthropic_headers,
      false,
      :x_api_key
    )
    root = success_json(response)
    call = Array(root["content"]).find do |item|
      item.is_a?(Hash) && item["type"] == "tool_use" && item["name"] == TOOL_NAME
    end
    assert(call, "forced tool_use block is missing")
    assert(call["input"].is_a?(Hash), "tool input is not an object")
  end

  def check_smart_unprefixed
    response = request(
      :post,
      "/v1/chat/completions",
      @smart_key,
      chat_payload(@exact_model, false),
      {},
      false
    )
    root = success_json(response)
    assert(root["model"] == @exact_model, "unprefixed logical model was not preserved")
    assert(root.dig("choices", 0, "message", "content").to_s.include?(MARKER), "assistant marker is missing")
  end

  def check_ordinary_bravo_denied
    require_ordinary_key!
    response = request(
      :post,
      "/v1/chat/completions",
      @ordinary_key,
      chat_payload(@text_model, false),
      {},
      false
    )
    assert([401, 403].include?(response.status), "expected Bravo auth denial, got HTTP #{response.status}")
  end

  def check_ordinary_unprefixed
    require_ordinary_key!
    response = request(
      :post,
      "/v1/chat/completions",
      @ordinary_key,
      chat_payload(@exact_model, false),
      {},
      false
    )
    root = success_json(response)
    assert(root["model"] == @exact_model, "native route did not preserve the physical model")
    assert(root.dig("choices", 0, "message", "content").to_s.include?(MARKER), "assistant marker is missing")
  end

  def chat_payload(model, stream)
    {
      "model" => model,
      "messages" => [{ "role" => "user", "content" => marker_prompt }],
      "max_tokens" => 512,
      "stream" => stream
    }
  end

  def responses_payload(model, stream)
    {
      "model" => model,
      "input" => [{
        "role" => "user",
        "content" => [{ "type" => "input_text", "text" => marker_prompt }]
      }],
      "max_output_tokens" => 512,
      "stream" => stream
    }
  end

  def anthropic_payload(model, stream)
    {
      "model" => model,
      "messages" => [{ "role" => "user", "content" => marker_prompt }],
      "max_tokens" => 512,
      "stream" => stream
    }
  end

  def marker_prompt
    "What is the exact stdout of this Python program? Reply with stdout only.\n\nprint(#{MARKER.inspect})"
  end

  def openai_tool_definition
    {
      "type" => "function",
      "function" => {
        "name" => TOOL_NAME,
        "description" => "Look up weather for a city.",
        "parameters" => {
          "type" => "object",
          "properties" => { "city" => { "type" => "string" } },
          "required" => ["city"],
          "additionalProperties" => false
        }
      }
    }
  end

  def anthropic_tool_definition
    definition = openai_tool_definition.fetch("function")
    {
      "name" => definition.fetch("name"),
      "description" => definition.fetch("description"),
      "input_schema" => definition.fetch("parameters")
    }
  end

  def anthropic_headers
    { "anthropic-version" => "2023-06-01" }
  end

  def request(method, path, key, payload, extra_headers, streaming, auth_style = :bearer)
    uri = @base_uri.dup
    uri.path = path
    uri.query = nil
    klass = method == :get ? Net::HTTP::Get : Net::HTTP::Post
    request = klass.new(uri)
    request["Accept"] = streaming ? "text/event-stream" : "application/json"
    request["Content-Type"] = "application/json" if payload
    if auth_style == :x_api_key
      request["x-api-key"] = key
    else
      request["Authorization"] = "Bearer #{key}"
    end
    extra_headers.each { |name, value| request[name] = value }
    request.body = JSON.generate(payload) if payload

    body = +""
    first_chunk_at = nil
    started = monotonic
    status = nil
    headers = nil
    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = uri.scheme == "https"
    http.open_timeout = [@timeout, 10].min
    http.read_timeout = @timeout
    http.write_timeout = @timeout if http.respond_to?(:write_timeout=)
    http.start do |client|
      client.request(request) do |response|
        status = response.code.to_i
        headers = response.to_hash
        response.read_body do |chunk|
          first_chunk_at ||= monotonic
          body << chunk
          raise CheckFailure, "response exceeded #{MAX_RESPONSE_BYTES} bytes" if body.bytesize > MAX_RESPONSE_BYTES
        end
      end
    end
    first_chunk_ms = first_chunk_at && ((first_chunk_at - started) * 1000).round
    Response.new(status, headers, body, first_chunk_ms)
  end

  def success_json(response)
    assert(response.status.between?(200, 299), http_failure(response))
    JSON.parse(response.body)
  rescue JSON::ParserError => error
    raise CheckFailure, "invalid JSON response: #{error.message}"
  end

  def success_sse(response)
    assert(response.status.between?(200, 299), http_failure(response))
    events = response.body.each_line.map do |line|
      next unless line.start_with?("data:")

      data = line.delete_prefix("data:").strip
      next if data.empty? || data == "[DONE]"

      JSON.parse(data)
    rescue JSON::ParserError => error
      raise CheckFailure, "invalid SSE JSON: #{error.message}"
    end.compact
    assert(!events.empty?, "SSE response contained no JSON events")
    events
  end

  def response_output_text(root)
    Array(root["output"]).flat_map do |item|
      Array(item["content"]).map do |content|
        content["text"] if content.is_a?(Hash) && content["type"] == "output_text"
      end.compact
    end.join
  end

  def anthropic_text(root)
    Array(root["content"]).map do |item|
      item["text"] if item.is_a?(Hash) && item["type"] == "text"
    end.compact.join
  end

  def assert(condition, message)
    raise CheckFailure, message unless condition
  end

  def http_failure(response)
    detail = begin
      root = JSON.parse(response.body)
      error = root["error"]
      error.is_a?(Hash) ? [error["code"], error["message"]].compact.join(": ") : error.to_s
    rescue JSON::ParserError
      response.body.to_s
    end
    detail = detail.to_s.gsub(/\s+/, " ").strip
    detail = detail.byteslice(0, 300)
    "HTTP #{response.status}#{detail.empty? ? "" : ": #{detail}"}"
  end

  def require_ordinary_key!
    raise CheckFailure, "config contains no usable ordinary API key" if @ordinary_key.nil?
  end

  def ordinary_key(config)
    candidates = Array(config["api-keys"]).map do |item|
      case item
      when String
        item.strip
      when Hash
        %w[key api-key value].map { |name| item[name].to_s.strip }.find { |value| !value.empty? }
      end
    end.compact
    candidates.find do |value|
      !value.empty? && value != @smart_key && !value.start_with?("your-api-key-")
    end
  end

  def read_secret(path, label)
    value = File.read(File.expand_path(path), mode: "r:BOM|UTF-8").strip
    raise CheckFailure, "#{label} file is empty" if value.empty?

    value
  rescue Errno::ENOENT, Errno::EACCES => error
    raise CheckFailure, "#{label} cannot be read: #{error.class}"
  end

  def sanitize(message)
    text = message.to_s.dup
    @secrets.each { |secret| text.gsub!(secret, "[REDACTED]") unless secret.empty? }
    text.gsub!(/\bbrv_[A-Za-z0-9_-]{16,}\b/, "brv_[REDACTED]")
    text.gsub!(/\bsk-[A-Za-z0-9_-]{16,}\b/, "sk-[REDACTED]")
    text
  end

  def monotonic
    Process.clock_gettime(Process::CLOCK_MONOTONIC)
  end
end

class DirectCodexContractSmoke < BravoSmoke
  SYNTHETIC_CALL_ID = "call_smoke_weather"

  def initialize(options)
    super
    @codex_model = options.fetch(:codex_model)
  end

  def run
    require_ordinary_key!
    check("Codex OpenAI Chat text") { check_codex_chat_text }
    check("Codex OpenAI Responses text") { check_codex_responses_text }
    check("Codex Anthropic Messages text") { check_codex_anthropic_text }
    check("Codex OpenAI Chat stream") { check_codex_chat_stream }
    check("Codex OpenAI Responses stream") { check_codex_responses_stream }
    check("Codex Anthropic Messages stream") { check_codex_anthropic_stream }
    check("Codex OpenAI Chat function call") { check_codex_chat_tool }
    check("Codex OpenAI Responses function call") { check_codex_responses_tool }
    check("Codex Anthropic Messages function call") { check_codex_anthropic_tool }
    check("Codex OpenAI Chat tool_result") { check_codex_chat_tool_result }
    check("Codex OpenAI Responses tool_result") { check_codex_responses_tool_result }
    check("Codex Anthropic Messages tool_result") { check_codex_anthropic_tool_result }

    passed = @results.count { |item| item[:status] == :pass }
    failed = @results.count { |item| item[:status] == :fail }
    skipped = @results.count { |item| item[:status] == :skip }
    puts
    puts "Direct Codex contract smoke: #{passed} passed, #{failed} failed, #{skipped} skipped"
    failed.zero? ? 0 : 1
  ensure
    @smart_key = nil
    @ordinary_key = nil
    @secrets = []
  end

  private

  def check_codex_chat_text
    root = success_json(request(
      :post,
      "/v1/chat/completions",
      @ordinary_key,
      codex_chat_text_payload(false),
      {},
      false
    ))
    assert(root["model"] == @codex_model, "model was not preserved")
    assert(root.dig("choices", 0, "message", "content").to_s.include?(MARKER), "assistant marker is missing")
  end

  def check_codex_responses_text
    root = success_json(request(
      :post,
      "/v1/responses",
      @ordinary_key,
      codex_responses_text_payload(false),
      {},
      false
    ))
    assert(root["model"] == @codex_model, "model was not preserved")
    assert(response_output_text(root).include?(MARKER), "assistant marker is missing")
  end

  def check_codex_anthropic_text
    root = success_json(request(
      :post,
      "/v1/messages",
      @ordinary_key,
      codex_anthropic_text_payload(false),
      anthropic_headers,
      false,
      :x_api_key
    ))
    assert(root["model"] == @codex_model, "model was not preserved")
    assert(anthropic_text(root).include?(MARKER), "assistant marker is missing")
  end

  def check_codex_chat_stream
    response = request(
      :post,
      "/v1/chat/completions",
      @ordinary_key,
      codex_chat_text_payload(true),
      {},
      true
    )
    events = success_sse(response)
    assert(events.map { |event| event["model"] }.compact.all? { |model| model == @codex_model }, "model changed in stream")
    text = events.map { |event| event.dig("choices", 0, "delta", "content") }.compact.join
    assert(text.include?(MARKER), "streamed assistant marker is missing")
  end

  def check_codex_responses_stream
    response = request(
      :post,
      "/v1/responses",
      @ordinary_key,
      codex_responses_text_payload(true),
      {},
      true
    )
    events = success_sse(response)
    assert(events.any? { |event| event["type"] == "response.completed" }, "response.completed is missing")
    models = events.map { |event| event.dig("response", "model") }.compact
    assert(models.all? { |model| model == @codex_model }, "model changed in stream")
    text = events.map do |event|
      event["delta"] if event["type"] == "response.output_text.delta"
    end.compact.join
    completed_text = events.map do |event|
      response_output_text(event["response"]) if event["type"] == "response.completed" && event["response"].is_a?(Hash)
    end.compact.join
    assert(
      (text + completed_text).include?(MARKER),
      "streamed assistant marker is missing " \
      "(event_types=#{events.map { |event| event["type"] }.compact.uniq.join(",")}; " \
      "delta_bytes=#{text.bytesize}; completed_bytes=#{completed_text.bytesize})"
    )
  end

  def check_codex_anthropic_stream
    response = request(
      :post,
      "/v1/messages",
      @ordinary_key,
      codex_anthropic_text_payload(true),
      anthropic_headers,
      true,
      :x_api_key
    )
    events = success_sse(response)
    assert(events.any? { |event| event["type"] == "message_stop" }, "message_stop is missing")
    models = events.map { |event| event.dig("message", "model") }.compact
    assert(models.all? { |model| model == @codex_model }, "model changed in stream")
    text = events.map do |event|
      event.dig("delta", "text") if event["type"] == "content_block_delta"
    end.compact.join
    assert(text.include?(MARKER), "streamed assistant marker is missing")
  end

  def check_codex_chat_tool
    payload = {
      "model" => @codex_model,
      "messages" => [{ "role" => "user", "content" => tool_request_text }],
      "tools" => [openai_tool_definition],
      "tool_choice" => { "type" => "function", "function" => { "name" => TOOL_NAME } },
      "max_tokens" => 512
    }
    root = success_json(request(:post, "/v1/chat/completions", @ordinary_key, payload, {}, false))
    calls = Array(root.dig("choices", 0, "message", "tool_calls"))
    call = calls.find { |item| item.dig("function", "name") == TOOL_NAME }
    assert(call, "forced function call is missing")
    JSON.parse(call.dig("function", "arguments").to_s)
  end

  def check_codex_responses_tool
    payload = {
      "model" => @codex_model,
      "input" => [{
        "role" => "user",
        "content" => [{ "type" => "input_text", "text" => tool_request_text }]
      }],
      "tools" => [responses_tool_definition],
      "tool_choice" => { "type" => "function", "name" => TOOL_NAME },
      "max_output_tokens" => 512
    }
    root = success_json(request(:post, "/v1/responses", @ordinary_key, payload, {}, false))
    call = Array(root["output"]).find do |item|
      item.is_a?(Hash) && item["type"] == "function_call" && item["name"] == TOOL_NAME
    end
    assert(call, "forced function_call item is missing")
    JSON.parse(call["arguments"].to_s)
  end

  def check_codex_anthropic_tool
    payload = {
      "model" => @codex_model,
      "messages" => [{ "role" => "user", "content" => tool_request_text }],
      "tools" => [anthropic_tool_definition],
      "tool_choice" => { "type" => "tool", "name" => TOOL_NAME },
      "max_tokens" => 512
    }
    root = success_json(request(
      :post,
      "/v1/messages",
      @ordinary_key,
      payload,
      anthropic_headers,
      false,
      :x_api_key
    ))
    call = Array(root["content"]).find do |item|
      item.is_a?(Hash) && item["type"] == "tool_use" && item["name"] == TOOL_NAME
    end
    assert(call, "forced tool_use block is missing")
    assert(call["input"].is_a?(Hash), "tool input is not an object")
  end

  def check_codex_chat_tool_result
    payload = {
      "model" => @codex_model,
      "messages" => [
        { "role" => "system", "content" => final_after_tool_instruction },
        { "role" => "user", "content" => tool_request_text },
        {
          "role" => "assistant",
          "content" => nil,
          "tool_calls" => [{
            "id" => SYNTHETIC_CALL_ID,
            "type" => "function",
            "function" => { "name" => TOOL_NAME, "arguments" => JSON.generate("city" => "Tbilisi") }
          }]
        },
        {
          "role" => "tool",
          "tool_call_id" => SYNTHETIC_CALL_ID,
          "content" => JSON.generate("temperature_c" => 18, "condition" => "clear")
        }
      ],
      "tools" => [openai_tool_definition],
      "tool_choice" => "none",
      "max_tokens" => 512
    }
    root = success_json(request(:post, "/v1/chat/completions", @ordinary_key, payload, {}, false))
    text = root.dig("choices", 0, "message", "content").to_s
    finish = root.dig("choices", 0, "finish_reason")
    calls = Array(root.dig("choices", 0, "message", "tool_calls")).length
    unless !text.strip.empty? && calls.zero?
      raise CheckFailure,
            "tool result did not produce a final assistant message " \
            "(finish_reason=#{finish}; text_bytes=#{text.bytesize}; tool_calls=#{calls})"
    end
  end

  def check_codex_responses_tool_result
    payload = {
      "model" => @codex_model,
      "instructions" => final_after_tool_instruction,
      "input" => [
        {
          "role" => "user",
          "content" => [{ "type" => "input_text", "text" => tool_request_text }]
        },
        {
          "type" => "function_call",
          "id" => "fc_smoke_weather",
          "call_id" => SYNTHETIC_CALL_ID,
          "name" => TOOL_NAME,
          "arguments" => JSON.generate("city" => "Tbilisi")
        },
        {
          "type" => "function_call_output",
          "call_id" => SYNTHETIC_CALL_ID,
          "output" => JSON.generate("temperature_c" => 18, "condition" => "clear")
        }
      ],
      "tools" => [responses_tool_definition],
      "tool_choice" => "none",
      "max_output_tokens" => 512
    }
    root = success_json(request(:post, "/v1/responses", @ordinary_key, payload, {}, false))
    text = response_output_text(root)
    output_types = Array(root["output"]).map { |item| item["type"] if item.is_a?(Hash) }.compact
    unless !text.strip.empty? && !output_types.include?("function_call")
      types = output_types.uniq.join(",")
      raise CheckFailure,
            "tool result did not produce a final assistant message " \
            "(status=#{root["status"]}; output_types=#{types}; text_bytes=#{text.bytesize})"
    end
  end

  def check_codex_anthropic_tool_result
    payload = {
      "model" => @codex_model,
      "system" => final_after_tool_instruction,
      "messages" => [
        { "role" => "user", "content" => tool_request_text },
        {
          "role" => "assistant",
          "content" => [{
            "type" => "tool_use",
            "id" => SYNTHETIC_CALL_ID,
            "name" => TOOL_NAME,
            "input" => { "city" => "Tbilisi" }
          }]
        },
        {
          "role" => "user",
          "content" => [{
            "type" => "tool_result",
            "tool_use_id" => SYNTHETIC_CALL_ID,
            "content" => JSON.generate("temperature_c" => 18, "condition" => "clear")
          }]
        }
      ],
      "tools" => [anthropic_tool_definition],
      "tool_choice" => { "type" => "auto" },
      "max_tokens" => 512
    }
    root = success_json(request(
      :post,
      "/v1/messages",
      @ordinary_key,
      payload,
      anthropic_headers,
      false,
      :x_api_key
    ))
    text = anthropic_text(root)
    content_types = Array(root["content"]).map { |item| item["type"] if item.is_a?(Hash) }.compact
    unless !text.strip.empty? && !content_types.include?("tool_use")
      types = content_types.uniq.join(",")
      raise CheckFailure,
            "tool result did not produce a final assistant message " \
            "(stop_reason=#{root["stop_reason"]}; content_types=#{types}; text_bytes=#{text.bytesize})"
    end
  end

  def codex_chat_text_payload(stream)
    {
      "model" => @codex_model,
      "messages" => [{ "role" => "user", "content" => marker_prompt }],
      "max_tokens" => 512,
      "stream" => stream
    }
  end

  def codex_responses_text_payload(stream)
    {
      "model" => @codex_model,
      "input" => [{
        "role" => "user",
        "content" => [{ "type" => "input_text", "text" => marker_prompt }]
      }],
      "max_output_tokens" => 512,
      "stream" => stream
    }
  end

  def codex_anthropic_text_payload(stream)
    {
      "model" => @codex_model,
      "messages" => [{ "role" => "user", "content" => marker_prompt }],
      "max_tokens" => 512,
      "stream" => stream
    }
  end

  def responses_tool_definition
    definition = openai_tool_definition.fetch("function")
    {
      "type" => "function",
      "name" => definition.fetch("name"),
      "description" => definition.fetch("description"),
      "parameters" => definition.fetch("parameters"),
      "strict" => true
    }
  end

  def tool_request_text
    "Call #{TOOL_NAME} for Tbilisi. Do not answer in prose."
  end

  def final_after_tool_instruction
    "The tool result is already present. Do not call tools again. Reply with exactly #{MARKER}."
  end
end

class DirectClaudeContractSmoke < DirectCodexContractSmoke
  def initialize(options)
    super
    @codex_model = options.fetch(:claude_model)
  end

  def run
    require_ordinary_key!
    check("Claude OpenAI Chat function call") { check_codex_chat_tool }
    check("Claude OpenAI Responses function call") { check_codex_responses_tool }
    check("Claude Anthropic Messages function call") { check_codex_anthropic_tool }
    check("Claude OpenAI Chat tool_result") { check_codex_chat_tool_result }
    check("Claude OpenAI Responses tool_result") { check_codex_responses_tool_result }
    check("Claude Anthropic Messages tool_result") { check_codex_anthropic_tool_result }

    passed = @results.count { |item| item[:status] == :pass }
    failed = @results.count { |item| item[:status] == :fail }
    skipped = @results.count { |item| item[:status] == :skip }
    puts
    puts "Direct Claude contract smoke: #{passed} passed, #{failed} failed, #{skipped} skipped"
    failed.zero? ? 0 : 1
  ensure
    @smart_key = nil
    @ordinary_key = nil
    @secrets = []
  end
end

class BravoContractSmoke < DirectCodexContractSmoke
  def initialize(options)
    super
    @ordinary_key = @smart_key
    @codex_model = options.fetch(:text_model)
  end
end

class DirectWebSearchContractSmoke < DirectCodexContractSmoke
  def initialize(options)
    super
    @codex_search_model = options.fetch(:codex_model)
    @claude_search_model = options.fetch(:claude_model)
  end

  def run
    require_ordinary_key!
    check("Codex OpenAI Responses web_search") { check_responses_web_search(@codex_search_model) }
    check("Claude OpenAI Responses web_search") { check_responses_web_search(@claude_search_model) }
    check("Codex Anthropic Messages web_search") { check_anthropic_web_search(@codex_search_model) }
    check("Claude Anthropic Messages web_search") { check_anthropic_web_search(@claude_search_model) }
    check("Codex OpenAI Chat web_search") { check_chat_web_search(@codex_search_model) }
    check("Claude OpenAI Chat web_search") { check_chat_web_search(@claude_search_model) }

    passed = @results.count { |item| item[:status] == :pass }
    failed = @results.count { |item| item[:status] == :fail }
    skipped = @results.count { |item| item[:status] == :skip }
    puts
    puts "Direct web-search contract smoke: #{passed} passed, #{failed} failed, #{skipped} skipped"
    failed.zero? ? 0 : 1
  ensure
    @smart_key = nil
    @ordinary_key = nil
    @secrets = []
  end

  private

  def check_responses_web_search(model)
    payload = {
      "model" => model,
      "input" => [{
        "role" => "user",
        "content" => [{ "type" => "input_text", "text" => web_search_prompt }]
      }],
      "tools" => [{ "type" => "web_search" }],
      "tool_choice" => "required",
      "max_output_tokens" => 1024
    }
    root = success_json(request(:post, "/v1/responses", search_key, payload, {}, false))
    assert(root["model"] == model, "logical model was not preserved")
    output = Array(root["output"])
    output_types = output.map { |item| item["type"] if item.is_a?(Hash) }.compact
    text = response_output_text(root)
    search_event = output_types.include?("web_search_call")
    grounded = grounded_payload?(text, output)
    assert(
      search_event || grounded,
      "no web_search_call or grounded final text " \
      "(status=#{root["status"]}; output_types=#{output_types.uniq.join(",")}; text_bytes=#{text.bytesize})"
    )
  end

  def check_anthropic_web_search(model)
    payload = {
      "model" => model,
      "messages" => [{ "role" => "user", "content" => web_search_prompt }],
      "tools" => [{
        "type" => "web_search_20250305",
        "name" => "web_search",
        "max_uses" => 1
      }],
      "tool_choice" => { "type" => "any" },
      "max_tokens" => 1024
    }
    root = success_json(request(
      :post,
      "/v1/messages",
      search_key,
      payload,
      anthropic_headers,
      false,
      :x_api_key
    ))
    assert(root["model"] == model, "logical model was not preserved")
    content = Array(root["content"])
    content_types = content.map { |item| item["type"] if item.is_a?(Hash) }.compact
    usage_count = root.dig("usage", "server_tool_use", "web_search_requests").to_i
    search_event = usage_count.positive? ||
                   content_types.include?("server_tool_use") ||
                   content_types.include?("web_search_tool_result")
    text = anthropic_text(root)
    grounded = grounded_payload?(text, content)
    assert(
      search_event || grounded,
      "no server web-search result or grounded final text " \
      "(content_types=#{content_types.uniq.join(",")}; web_search_requests=#{usage_count}; text_bytes=#{text.bytesize})"
    )
  end

  def check_chat_web_search(model)
    payload = {
      "model" => model,
      "messages" => [{ "role" => "user", "content" => web_search_prompt }],
      "tools" => [{ "type" => "web_search" }],
      "tool_choice" => "required",
      "max_tokens" => 1024
    }
    root = success_json(request(:post, "/v1/chat/completions", search_key, payload, {}, false))
    assert(root["model"] == model, "logical model was not preserved")
    message = root.dig("choices", 0, "message") || {}
    text = message["content"].to_s
    assert(
      grounded_payload?(text, message),
      "Chat response has no grounded final text " \
      "(finish_reason=#{root.dig("choices", 0, "finish_reason")}; text_bytes=#{text.bytesize})"
    )
  end

  def grounded_payload?(text, payload)
    !text.to_s.strip.empty? &&
      (text.to_s.match?(%r{https?://}i) || JSON.generate(payload).match?(%r{https?://}i))
  end

  def web_search_prompt
    "Use the built-in web_search tool to find the official OpenAI API documentation landing page. " \
      "Return a concise answer with at least one full https URL from the search result."
  end

  def search_key
    @ordinary_key
  end
end

class BravoWebSearchContractSmoke < DirectWebSearchContractSmoke
  def initialize(options)
    super
    @bravo_search_model = options.fetch(:text_model)
  end

  def run
    check("Bravo OpenAI Responses web_search") { check_responses_web_search(@bravo_search_model) }
    check("Bravo Anthropic Messages web_search") { check_anthropic_web_search(@bravo_search_model) }
    check("Bravo OpenAI Chat web_search") { check_chat_web_search(@bravo_search_model) }

    passed = @results.count { |item| item[:status] == :pass }
    failed = @results.count { |item| item[:status] == :fail }
    skipped = @results.count { |item| item[:status] == :skip }
    puts
    puts "Bravo web-search contract smoke: #{passed} passed, #{failed} failed, #{skipped} skipped"
    failed.zero? ? 0 : 1
  ensure
    @smart_key = nil
    @ordinary_key = nil
    @secrets = []
  end

  private

  def search_key
    @smart_key
  end
end

options = {
  suite: ENV.fetch("BRAVO_SMOKE_SUITE", "bravo"),
  base_url: ENV.fetch("BRAVO_BASE_URL", "http://127.0.0.1:18319"),
  config: ENV.fetch("BRAVO_CONFIG", "config.yaml"),
  smart_key_file: ENV.fetch("BRAVO_SMART_KEY_FILE", "bravo-smart-key.txt"),
  timeout: Integer(ENV.fetch("BRAVO_SMOKE_TIMEOUT", "120"), 10),
  text_model: ENV.fetch("BRAVO_TEXT_MODEL", BravoSmoke::DEFAULT_TEXT_MODEL),
  tool_model: ENV.fetch("BRAVO_TOOL_MODEL", BravoSmoke::DEFAULT_TOOL_MODEL),
  exact_model: ENV.fetch("BRAVO_EXACT_MODEL", BravoSmoke::DEFAULT_EXACT_MODEL),
  codex_model: ENV.fetch("BRAVO_CODEX_MODEL", "gpt-5.6-luna"),
  claude_model: ENV.fetch("BRAVO_CLAUDE_MODEL", BravoSmoke::DEFAULT_EXACT_MODEL)
}

parser = OptionParser.new do |opts|
  opts.banner = "Usage: ruby scripts/bravo-smoke.rb [options]"
  opts.on("--suite NAME", "bravo, bravo-contract, codex-contract, claude-contract, web-search-contract, or bravo-web-search") { |value| options[:suite] = value }
  opts.on("--base-url URL", "Canary base URL (default: #{options[:base_url]})") { |value| options[:base_url] = value }
  opts.on("--config PATH", "CLIProxyAPI YAML config path") { |value| options[:config] = value }
  opts.on("--smart-key-file PATH", "Bravo plaintext key file path") { |value| options[:smart_key_file] = value }
  opts.on("--timeout SECONDS", Integer, "Per-request timeout") { |value| options[:timeout] = value }
  opts.on("--text-model ID", "Bravo text model") { |value| options[:text_model] = value }
  opts.on("--tool-model ID", "Bravo model used for forced tools") { |value| options[:tool_model] = value }
  opts.on("--exact-model ID", "Unprefixed exact model") { |value| options[:exact_model] = value }
  opts.on("--codex-model ID", "Direct Codex model for contract evidence") { |value| options[:codex_model] = value }
  opts.on("--claude-model ID", "Direct Claude model for contract evidence") { |value| options[:claude_model] = value }
  opts.on("--only REGEX", "Run checks whose names match REGEX") { |value| options[:only] = Regexp.new(value, Regexp::IGNORECASE) }
  opts.on("-h", "--help", "Show this help") do
    puts opts
    exit 0
  end
end

begin
  parser.parse!
  abort("unexpected positional arguments") unless ARGV.empty?
  suite = options.fetch(:suite).strip.downcase
  runner =
    case suite
    when "bravo"
      BravoSmoke.new(options)
    when "codex-contract"
      DirectCodexContractSmoke.new(options)
    when "claude-contract"
      DirectClaudeContractSmoke.new(options)
    when "bravo-contract"
      BravoContractSmoke.new(options)
    when "web-search-contract"
      DirectWebSearchContractSmoke.new(options)
    when "bravo-web-search"
      BravoWebSearchContractSmoke.new(options)
    else
      raise OptionParser::InvalidArgument, "unknown suite #{suite.inspect}"
    end
  exit runner.run
rescue OptionParser::ParseError, URI::InvalidURIError, ArgumentError, CheckFailure => error
  warn "Bravo smoke setup failed: #{error.message}"
  exit 2
end
