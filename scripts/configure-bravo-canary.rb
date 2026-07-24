#!/usr/bin/env ruby
# frozen_string_literal: true

require "digest"
require "fileutils"
require "securerandom"
require "yaml"

config_path = File.expand_path(ARGV.fetch(0))
key_path = File.expand_path(ARGV.fetch(1))
project_name = ARGV.fetch(2, "default-project").strip
abort("project name is required") if project_name.empty?
abort("config not found: #{config_path}") unless File.file?(config_path)

backup_path = "#{config_path}.pre-bravo-native-0.1.0"
FileUtils.cp(config_path, backup_path, preserve: true) unless File.exist?(backup_path)

smart_key =
  if File.file?(key_path)
    File.read(key_path, mode: "r:BOM|UTF-8").strip
  else
    generated = "brv_#{SecureRandom.urlsafe_base64(36, false)}"
    FileUtils.mkdir_p(File.dirname(key_path), mode: 0o700)
    File.open(key_path, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
      file.write("#{generated}\n")
    end
    generated
  end
abort("stored smart key is empty") if smart_key.empty?

config = YAML.load_file(config_path)
abort("config root must be a mapping") unless config.is_a?(Hash)

plugins = config["plugins"] = {} unless config["plugins"].is_a?(Hash)
plugins ||= config["plugins"]
plugins["enabled"] = true
plugins["dir"] = "plugin-dist"
configs = plugins["configs"] = {} unless plugins["configs"].is_a?(Hash)
configs ||= plugins["configs"]
bravo = configs["bravo"] = {} unless configs["bravo"].is_a?(Hash)
bravo ||= configs["bravo"]
bravo["enabled"] = true
bravo["prefix"] = "bravo/"
bravo["require_smart_key"] = true
bravo["max_attempts"] = 0
bravo["cooldown_seconds"] = 30
smart_key_digest = Digest::SHA256.hexdigest(smart_key)
bravo["smart_keys"] = [{
  "id" => "legacy_#{smart_key_digest[0, 16]}",
  "name" => project_name,
  "sha256" => smart_key_digest,
  "enabled" => true,
  "status" => "active",
  "models" => ["*"],
  "primary_auth_ids" => [],
  "policy" => {}
}]

temporary_path = "#{config_path}.bravo-tmp-#{Process.pid}"
begin
  File.open(temporary_path, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
    file.write(YAML.dump(config))
    file.flush
    file.fsync
  end
  File.rename(temporary_path, config_path)
  File.chmod(0o600, config_path)
ensure
  FileUtils.rm_f(temporary_path)
end

puts "Bravo configured for #{project_name}; plaintext key stored at #{key_path}"
