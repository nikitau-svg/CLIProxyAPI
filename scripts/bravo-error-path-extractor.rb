#!/usr/bin/env ruby
# frozen_string_literal: true

# Read-only diagnostic for CLIProxyAPI error request logs.
#
# The extractor deliberately emits only routing metadata and error responses. It
# never prints downstream/upstream request bodies, request headers, auth IDs, or
# credential values.

require "json"
require "optparse"
require "time"

module BravoErrorPathExtractor
  SCHEMA_VERSION = "bravo_error_path/v1"
  REDACTED = "[REDACTED]"

  module Sanitizer
    module_function

    SENSITIVE_KEY = /\A(?:authorization|proxy_authorization|api_?key|access_?token|refresh_?token|id_?token|token|secret|password|cookie|set_cookie|client_?secret)\z/i
    REQUEST_CONTENT_KEY = /\A(?:prompt|input|messages|system|tools|contents?|request|request_body|raw_body|body|payload)\z/i

    def value(value)
      case value
      when Hash
        value.each_with_object({}) do |(key, child), safe|
          normalized = key.to_s.tr("-", "_")
          safe[key.to_s] =
            if normalized.match?(SENSITIVE_KEY) || normalized.match?(REQUEST_CONTENT_KEY)
              REDACTED
            else
              value(child)
            end
        end
      when Array
        value.map { |child| value(child) }
      when String
        string(value)
      else
        value
      end
    end

    def string(value)
      safe = value.to_s.dup
      safe.gsub!(/(?:Bearer|Basic)\s+[A-Za-z0-9._~+\/=-]+/i) do |match|
        "#{match.split.first} #{REDACTED}"
      end
      safe.gsub!(/\b(?:sk|brv|xox[a-z]?)[-_][A-Za-z0-9._-]{6,}\b/i, REDACTED)
      safe.gsub!(/\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}(?:\.[A-Za-z0-9_-]{8,})?\b/, REDACTED)
      safe.gsub!(/([?&](?:api_?key|access_?token|token|secret)=)[^&#\s]+/i, "\\1#{REDACTED}")
      safe.gsub!(/(["']?(?:api_?key|access_?token|refresh_?token|client_?secret|password)["']?\s*[:=]\s*["']?)[^"',}\s]+/i) do
        "#{Regexp.last_match(1)}#{REDACTED}"
      end
      safe
    end
  end

  Section = Struct.new(:name, :index, :body, :position, keyword_init: true)

  module SectionScanner
    module_function

    def scan(content)
      headings = []
      offset = 0
      content.each_line do |line|
        if (match = line.match(/\A=== ([A-Z][A-Z0-9 ]*?)(?: (\d+))? ===\r?\n?\z/))
          headings << {
            name: match[1],
            index: match[2]&.to_i,
            heading_start: offset,
            body_start: offset + line.bytesize
          }
        end
        offset += line.bytesize
      end

      headings.each_with_index.map do |heading, position|
        next_heading = headings[position + 1]
        body_end = next_heading ? next_heading.fetch(:heading_start) : content.bytesize
        Section.new(
          name: heading.fetch(:name),
          index: heading.fetch(:index),
          body: content.byteslice(heading.fetch(:body_start)...body_end).to_s,
          position: position
        )
      end
    end
  end

  class Finder
    def initialize(inputs, latest:)
      @inputs = Array(inputs)
      @latest = Integer(latest)
      raise ArgumentError, "latest must be greater than zero" unless @latest.positive?
    end

    def paths
      candidates = @inputs.flat_map { |input| expand(input) }.uniq
      candidates.sort_by { |path| [-File.mtime(path).to_f, path] }.first(@latest)
    end

    private

    def expand(input)
      path = File.expand_path(input)
      if File.directory?(path)
        Dir.glob(File.join(path, "error-*.log")).select { |candidate| File.file?(candidate) }
      elsif File.file?(path) && File.basename(path).match?(/\Aerror-.*\.log\z/)
        [path]
      else
        []
      end
    end
  end

  class Parser
    def initialize(content, filename:, project: nil)
      @content = content.to_s.dup.force_encoding(Encoding::UTF_8).scrub
      @filename = File.basename(filename.to_s)
      @project = project.to_s.strip
      @sections = SectionScanner.scan(@content)
      @warnings = []
    end

    def parse
      request_info = first_section("REQUEST INFO")
      headers = first_section("HEADERS")
      request_body = first_section("REQUEST BODY")
      request_json, body_metrics = parse_request_body(request_body)
      gateway = parse_gateway_response
      attempts = parse_attempts
      request_id = explicit_request_id(headers) || filename_request_id
      session_id = safe_field(headers&.body, "X-Claude-Code-Session-Id")
      logical_model = safe_scalar(request_json&.fetch("model", nil))
      received_at = safe_field(request_info&.body, "Timestamp")
      protocol = protocol_for(safe_field(request_info&.body, "URL"))

      report = {
        "schema_version" => SCHEMA_VERSION,
        "log_file" => @filename,
        "project" => project_record,
        "correlation" => {
          "request_id" => request_id,
          "session_id" => evidence(session_id, "request_header", session_id ? "exact" : "missing")
        },
        "request" => {
          "received_at" => received_at,
          "protocol" => protocol,
          "logical_model" => logical_model,
          "stream" => boolean_or_nil(request_json&.fetch("stream", nil)),
          "effort" => extract_effort(request_json),
          "body_metrics" => body_metrics
        },
        "attempts" => attempts,
        "planning_rejections" => parse_planning_rejections(gateway),
        "gateway_response" => gateway,
        "missing_fields" => [],
        "warnings" => @warnings.uniq
      }
      report["missing_fields"] = missing_fields(report)
      report
    end

    private

    def first_section(name)
      @sections.find { |section| section.name == name && section.index.nil? }
    end

    def sections(name)
      @sections.select { |section| section.name == name }
    end

    def project_record
      if @project.empty?
        { "name" => nil, "source" => "missing", "confidence" => "none" }
      else
        {
          "name" => Sanitizer.string(@project),
          "source" => "operator_assertion",
          "confidence" => "asserted"
        }
      end
    end

    def explicit_request_id(headers)
      %w[X-CPA-TRACE-ID X-Request-ID Request-Id].each do |name|
        value = safe_field(headers&.body, name)
        return evidence(value, "request_header:#{name}", "exact") if value
      end
      nil
    end

    def filename_request_id
      match = @filename.match(/-([0-9a-f]{8,64})\.log\z/i)
      evidence(match&.[](1), "filename_suffix", match ? "hint" : "missing")
    end

    def evidence(value, source, confidence)
      {
        "value" => value,
        "source" => value ? source : "missing",
        "confidence" => value ? confidence : "none"
      }
    end

    def parse_request_body(section)
      raw = section&.body.to_s.strip
      content_length = integer_or_nil(safe_field(first_section("HEADERS")&.body, "Content-Length"))
      metrics = {
        "bytes" => raw.bytesize,
        "content_length" => content_length,
        "json_valid" => false,
        "message_count" => nil,
        "tool_count" => nil,
        "system_item_count" => nil
      }
      if raw.empty?
        @warnings << "request_body_missing"
        return [nil, metrics]
      end

      parsed = JSON.parse(raw)
      unless parsed.is_a?(Hash)
        @warnings << "request_body_json_not_object"
        return [nil, metrics]
      end

      metrics["json_valid"] = true
      metrics["message_count"] = collection_count(parsed["messages"])
      metrics["tool_count"] = collection_count(parsed["tools"])
      metrics["system_item_count"] = collection_count(parsed["system"])
      [parsed, metrics]
    rescue JSON::ParserError
      @warnings << "request_body_invalid_json"
      [nil, metrics]
    end

    def collection_count(value)
      case value
      when Array then value.length
      when nil then 0
      else 1
      end
    end

    def extract_effort(request_json)
      return nil unless request_json.is_a?(Hash)

      value =
        request_json.dig("output_config", "effort") ||
        request_json.dig("reasoning", "effort") ||
        request_json["reasoning_effort"] ||
        request_json["effort"]
      safe_scalar(value)
    end

    def protocol_for(url)
      path = url.to_s.split("?", 2).first.to_s
      case path
      when %r{/v1/messages\z} then "anthropic_messages"
      when %r{/v1/responses\z} then "openai_responses"
      when %r{/v1/chat/completions\z} then "openai_chat_completions"
      else
        path.empty? ? nil : "unknown"
      end
    end

    def parse_attempts
      response_queues = Hash.new { |hash, key| hash[key] = [] }
      sections("API RESPONSE").each do |section|
        response_queues[section.index] << section if section.index
      end

      sections("API REQUEST").select(&:index).each_with_index.map do |section, position|
        response_section = response_queues[section.index].shift
        body = body_after_marker(section.body)
        request_json = parse_json_without_warning(body)
        auth = safe_field(section.body, "Auth")
        provider = auth_value(auth, "provider") || infer_provider(safe_field(section.body, "Upstream URL"))
        model = safe_scalar(request_json&.fetch("model", nil))
        auth_label = auth_label(auth)
        missing = []
        missing << "provider" unless provider
        missing << "model" unless model
        missing << "auth_label" unless auth_label

        {
          "sequence" => position + 1,
          "section_index" => section.index,
          "role" => position.zero? ? "first_request" : "retry",
          "executed" => !section.body.lstrip.start_with?("<missing>"),
          "requested_at" => safe_field(section.body, "Timestamp"),
          "provider" => provider,
          "model" => model,
          "auth_label" => auth_label,
          "provider_response" => parse_attempt_response(response_section),
          "missing_fields" => missing
        }
      end
    end

    def parse_attempt_response(section)
      unless section
        return {
          "present" => false,
          "status" => nil,
          "json" => nil,
          "missing_reason" => "api_response_section_not_found"
        }
      end

      status = integer_or_nil(safe_field(section.body, "Status"))
      raw_body = body_after_marker(section.body)
      parsed = parse_json_without_warning(raw_body)
      error_line = safe_field(section.body, "Error")
      response = {
        "present" => true,
        "received_at" => safe_field(section.body, "Timestamp"),
        "status" => status,
        "json" => nil
      }
      if parsed && (status.to_i >= 400 || error_json?(parsed))
        response["json"] = Sanitizer.value(parsed)
      elsif error_line
        response["transport_error"] = Sanitizer.string(error_line)[0, 2_000]
        response["missing_reason"] = "provider_response_json_not_available"
      elsif parsed
        response["missing_reason"] = "non_error_provider_body_omitted"
      else
        response["missing_reason"] = "provider_response_json_not_available"
      end
      response
    end

    def parse_gateway_response
      response_section = first_section("RESPONSE")
      return nil unless response_section

      raw_body = body_after_metadata(response_section.body)
      parsed = parse_json_without_warning(raw_body)
      unnumbered_api_response = sections("API RESPONSE").find { |section| section.index.nil? }
      parsed ||= parse_json_without_warning(json_tail(unnumbered_api_response&.body))
      {
        "received_at" => safe_field(unnumbered_api_response&.body, "Timestamp"),
        "status" => integer_or_nil(safe_field(response_section.body, "Status")),
        "retry_after" => safe_field(response_section.body, "Retry-After"),
        "json" => parsed ? Sanitizer.value(parsed) : nil
      }
    end

    def parse_planning_rejections(gateway)
      message = gateway&.dig("json", "error", "message")
      return [] unless message.is_a?(String) && message.include?("bravo_no_eligible_account")

      pattern = /([A-Za-z0-9_-]+)\/([A-Za-z0-9._:-]+)\s+eligibility\(([^)]+)\):\s*(\d+)\s+[A-Za-z0-9_-]+\s+credential\(s\):\s*([^;)]*)/
      message.scan(pattern).map do |provider, model, code, count, reason_text|
        reasons = {}
        reason_text.scan(/([A-Za-z0-9_-]+)=(\d+)/).each do |name, value|
          reasons[name] = value.to_i
        end
        {
          "provider" => provider,
          "model" => model,
          "eligibility_code" => code,
          "credential_count" => count.to_i,
          "reasons" => reasons,
          "executed" => false
        }
      end
    end

    def missing_fields(report)
      missing = []
      missing << "project.name" unless report.dig("project", "name")
      missing << "correlation.request_id" unless report.dig("correlation", "request_id", "value")
      missing << "correlation.session_id" unless report.dig("correlation", "session_id", "value")
      missing << "request.received_at" unless report.dig("request", "received_at")
      missing << "request.protocol" unless report.dig("request", "protocol")
      missing << "request.logical_model" unless report.dig("request", "logical_model")
      if report.fetch("attempts").empty?
        missing << "attempts"
        missing << "attempt_provider_responses"
      else
        report.fetch("attempts").each_with_index do |attempt, index|
          unless attempt.dig("provider_response", "present")
            missing << "attempts[#{index}].provider_response"
          end
        end
      end
      missing << "gateway_response" unless report["gateway_response"]
      missing
    end

    def body_after_marker(body)
      match = body.to_s.match(/^Body:\s*\r?\n/)
      match ? body.to_s[match.end(0)..].to_s.strip : ""
    end

    def body_after_metadata(body)
      _metadata, payload = body.to_s.split(/\r?\n\r?\n/, 2)
      payload.to_s.strip
    end

    def json_tail(body)
      return "" unless body

      lines = body.lines
      position = lines.index { |line| line.lstrip.start_with?("{", "[") }
      position ? lines[position..].join.strip : ""
    end

    def parse_json_without_warning(raw)
      value = raw.to_s.strip
      return nil if value.empty?

      JSON.parse(value)
    rescue JSON::ParserError
      nil
    end

    def error_json?(parsed)
      parsed.is_a?(Hash) && (parsed.key?("error") || parsed["type"] == "error")
    end

    def safe_field(body, name)
      return nil unless body

      match = body.match(/^#{Regexp.escape(name)}:\s*(.*?)\s*\r?$/i)
      value = match&.[](1).to_s.strip
      value.empty? ? nil : Sanitizer.string(value)
    end

    def auth_value(auth, key)
      return nil unless auth

      match = auth.match(/(?:\A|,\s*)#{Regexp.escape(key)}=([^,]*?)(?=,\s*[A-Za-z_]+=|\z)/)
      safe_scalar(match&.[](1)&.strip)
    end

    def auth_label(auth)
      auth_value(auth, "label")
    end

    def infer_provider(url)
      value = url.to_s.downcase
      return "claude" if value.include?("anthropic.com")
      return "codex" if value.include?("chatgpt.com") || value.include?("/backend-api/codex")
      return "openai" if value.include?("api.openai.com")
      return "gemini" if value.include?("googleapis.com")

      nil
    end

    def safe_scalar(value)
      case value
      when String, Symbol, Numeric
        safe = Sanitizer.string(value.to_s.strip)
        safe.empty? ? nil : safe
      else
        nil
      end
    end

    def integer_or_nil(value)
      Integer(value, 10)
    rescue ArgumentError, TypeError
      nil
    end

    def boolean_or_nil(value)
      value == true || value == false ? value : nil
    end
  end

  class TextRenderer
    def render(reports)
      Array(reports).map { |report| render_report(report) }.join("\n\n")
    end

    private

    def render_report(report)
      project = report.dig("project", "name") || "<missing>"
      project_source = report.dig("project", "source")
      request = report.fetch("request")
      lines = [
        "Лог: #{report.fetch("log_file")}",
        "Проект: #{project} (источник: #{project_source})",
        "Входящий запрос: #{request["received_at"] || "<missing>"} | #{request["protocol"] || "<missing>"} | #{request["logical_model"] || "<missing>"} | effort=#{request["effort"] || "<missing>"} | stream=#{request["stream"].nil? ? "<missing>" : request["stream"]}"
      ]

      if report.fetch("attempts").empty?
        lines << "Запросы к провайдерам: не зафиксированы"
      else
        report.fetch("attempts").each do |attempt|
          title = attempt["role"] == "first_request" ? "Первый запрос" : "Ретрай #{attempt["sequence"] - 1}"
          lines << "#{title}: #{attempt["provider"] || "<missing>"} / #{attempt["model"] || "<missing>"} / #{attempt["auth_label"] || "<missing>"}"
          provider_response = attempt.fetch("provider_response")
          if provider_response["json"]
            lines << "  Ошибка провайдера (полный безопасно очищенный JSON):"
            lines.concat(indent_json(provider_response["json"], 4))
          elsif provider_response["present"]
            lines << "  Ошибка провайдера: JSON отсутствует (#{provider_response["missing_reason"]})"
          else
            lines << "  Ошибка провайдера: секция ответа отсутствует (#{provider_response["missing_reason"]})"
          end
        end
      end

      report.fetch("planning_rejections").each do |rejection|
        reasons = rejection.fetch("reasons").map { |key, value| "#{key}=#{value}" }.join(" ")
        lines << "Отказ планировщика (запрос не выполнялся): #{rejection["provider"]} / #{rejection["model"]} | credentials=#{rejection["credential_count"]} | #{reasons}"
      end

      gateway = report["gateway_response"]
      if gateway
        retry_after = gateway["retry_after"] ? " | Retry-After=#{gateway["retry_after"]}" : ""
        lines << "Финальный ответ шлюза: status=#{gateway["status"] || "<missing>"}#{retry_after}"
        if gateway["json"]
          lines << "  JSON шлюза:"
          lines.concat(indent_json(gateway["json"], 4))
        end
      else
        lines << "Финальный ответ шлюза: отсутствует"
      end
      missing = report.fetch("missing_fields")
      lines << "Отсутствующие поля: #{missing.empty? ? "нет" : missing.join(", ")}"
      warnings = report.fetch("warnings")
      lines << "Предупреждения: #{warnings.join(", ")}" unless warnings.empty?
      lines.join("\n")
    end

    def indent_json(value, spaces)
      prefix = " " * spaces
      JSON.pretty_generate(value).lines.map { |line| "#{prefix}#{line.chomp}" }
    end
  end

  class CLI
    def self.run(argv, stdout: $stdout, stderr: $stderr)
      options = {
        inputs: [],
        latest: 10,
        format: "text",
        project: nil
      }
      parser = OptionParser.new do |opts|
        opts.banner = "Использование: bravo-error-path-extractor.rb [параметры] [ФАЙЛ_ИЛИ_КАТАЛОГ ...]"
        opts.on("--dir DIRECTORY", "Добавить каталог с файлами error-*.log") do |value|
          options[:inputs] << value
        end
        opts.on("--latest N", Integer, "Взять последние N логов по времени изменения (по умолчанию: 10)") do |value|
          options[:latest] = value
        end
        opts.on("--format FORMAT", %w[text json], "Формат вывода: text или json (по умолчанию: text)") do |value|
          options[:format] = value
        end
        opts.on("--project NAME", "Указать проект как утверждение оператора") do |value|
          options[:project] = value
        end
        opts.on("-h", "--help", "Показать справку") do
          stdout.puts opts
          return 0
        end
      end
      remaining = parser.parse(argv)
      options[:inputs].concat(remaining)
      options[:inputs] << "." if options[:inputs].empty?
      paths = Finder.new(options[:inputs], latest: options[:latest]).paths
      if paths.empty?
        stderr.puts "Локальные файлы error-*.log не найдены."
        return 2
      end

      reports = paths.map do |path|
        Parser.new(
          File.binread(path),
          filename: File.basename(path),
          project: options[:project]
        ).parse
      rescue SystemCallError
        {
          "schema_version" => SCHEMA_VERSION,
          "log_file" => File.basename(path),
          "project" => {
            "name" => options[:project],
            "source" => options[:project] ? "operator_assertion" : "missing",
            "confidence" => options[:project] ? "asserted" : "none"
          },
          "correlation" => {},
          "request" => {},
          "attempts" => [],
          "planning_rejections" => [],
          "gateway_response" => nil,
          "missing_fields" => ["log_content"],
          "warnings" => ["log_read_failed"]
        }
      end

      if options[:format] == "json"
        stdout.puts JSON.pretty_generate(
          "schema_version" => SCHEMA_VERSION,
          "logs" => reports
        )
      else
        stdout.puts TextRenderer.new.render(reports)
      end
      0
    rescue OptionParser::ParseError, ArgumentError => error
      stderr.puts error.message
      2
    end
  end
end

exit BravoErrorPathExtractor::CLI.run(ARGV) if $PROGRAM_NAME == __FILE__
