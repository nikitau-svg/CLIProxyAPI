#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "optparse"
require "socket"

options = {
  bind: "127.0.0.1",
  port: 18_991,
  status: 429,
  retry_after: "60",
  error_type: "rate_limit_error",
  error_code: "canary_fault_injected",
  message: "Controlled canary failure."
}

OptionParser.new do |parser|
  parser.banner = "Usage: bravo-fault-provider.rb [options]"
  parser.on("--bind ADDRESS", "Listen address") { |value| options[:bind] = value }
  parser.on("--port PORT", Integer, "Listen port") { |value| options[:port] = value }
  parser.on("--status STATUS", Integer, "HTTP failure status") { |value| options[:status] = value }
  parser.on("--retry-after VALUE", "Retry-After header") { |value| options[:retry_after] = value }
  parser.on("--error-type TYPE", "Provider error type") { |value| options[:error_type] = value }
  parser.on("--error-code CODE", "Provider error code; empty omits it") { |value| options[:error_code] = value }
  parser.on("--message TEXT", "Provider error message") { |value| options[:message] = value }
end.parse!

abort("status must be between 400 and 599") unless (400..599).cover?(options[:status])
abort("port must be between 1024 and 65535") unless (1024..65_535).cover?(options[:port])

error = {
  "type" => options[:error_type],
  "message" => options[:message]
}
error["code"] = options[:error_code] unless options[:error_code].to_s.empty?
body = JSON.generate("type" => "error", "error" => error)

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
puts "ready bind=#{options[:bind]} port=#{options[:port]} status=#{options[:status]}"

until stopping
  begin
    socket = server.accept
  rescue IOError, Errno::EBADF
    break if stopping

    raise
  end
  Thread.new(socket) do |client|
    begin
      request_line = client.gets
      next if request_line.nil?

      content_length = 0
      while (line = client.gets)
        break if line == "\r\n" || line == "\n"

        name, value = line.split(":", 2)
        content_length = value.to_i if name&.casecmp?("Content-Length")
      end
      client.read(content_length) if content_length.positive?

      client.write("HTTP/1.1 #{options[:status]} Controlled Canary Failure\r\n")
      client.write("Content-Type: application/json\r\n")
      client.write("Content-Length: #{body.bytesize}\r\n")
      unless options[:retry_after].to_s.empty?
        client.write("Retry-After: #{options[:retry_after]}\r\n")
      end
      client.write("Connection: close\r\n")
      client.write("\r\n")
      client.write(body)
    rescue IOError, SystemCallError
      nil
    ensure
      client.close
    end
  end
end
