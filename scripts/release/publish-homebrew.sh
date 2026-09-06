#!/usr/bin/env bash
# Publish one canonical formula without replacing a newer release in the tap.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec ruby - "$SCRIPT_DIR" "$@" <<'RUBY'
require "base64"
require "digest"
require "json"
require "open3"
require "optparse"
require "tmpdir"

class PublicationError < StandardError; end
class APIError < PublicationError
  attr_reader :conflict

  def initialize(message, conflict = false)
    super(message)
    @conflict = conflict
  end
end

TAP_PATH = "Formula/baseten-switch.rb"
TAP_API = "repos/basetenlabs/homebrew-baseten/contents/#{TAP_PATH}"
MAX_WRITES = 3

def require_condition(condition, message)
  raise PublicationError, message unless condition
end

def version(tag)
  require_condition(tag.match?(/\Av[0-9]+\.[0-9]+\.[0-9]+\z/), "invalid release tag")
  tag.delete_prefix("v").split(".").map { |part| Integer(part, 10) }
end

def field(content, name)
  values = content.scan(/^  #{Regexp.escape(name)} "([^"\r\n]+)"$/).flatten
  require_condition(values.length == 1, "formula must contain exactly one #{name} field")
  values.first
end

def formula_metadata(content)
  require_condition(content.bytesize <= 65_536 && content.valid_encoding?, "invalid formula content")
  url = field(content, "url")
  tag = url[%r{\Ahttps://github\.com/basetenlabs/baseten-switch/releases/download/(v[0-9]+\.[0-9]+\.[0-9]+)/}, 1]
  require_condition(!tag.nil?, "formula release URL is not canonical")
  expected_url = "https://github.com/basetenlabs/baseten-switch/releases/download/#{tag}/baseten-switch_#{tag.delete_prefix('v')}_darwin_universal.zip"
  require_condition(url == expected_url, "formula release URL is not canonical")
  checksum = field(content, "sha256")
  require_condition(checksum.match?(/\A[0-9a-f]{64}\z/), "invalid formula SHA256")
  [tag, version(tag), checksum]
end

def validate_candidate(script_dir, tag, content)
  candidate_tag, candidate_version, checksum = formula_metadata(content)
  require_condition(candidate_tag == tag, "formula does not match the selected release tag")
  license = field(content, "license")
  Dir.mktmpdir("baseten-switch-formula-") do |directory|
    expected = File.join(directory, "baseten-switch.rb")
    _stdout, _stderr, status = Open3.capture3(
      File.join(script_dir, "render-formula.sh"),
      "--tag", tag, "--sha256", checksum, "--approved-license-spdx", license,
      "--output", expected, "--patch-output", File.join(directory, "formula.patch")
    )
    require_condition(status.success? && File.binread(expected) == content,
                      "candidate does not match the canonical formula renderer")
  end
  candidate_version
end

def api(method, endpoint, payload = nil)
  arguments = ["gh", "api", "--hostname", "github.com", "--method", method,
               "--header", "Accept: application/vnd.github+json",
               "--header", "X-GitHub-Api-Version: 2022-11-28", endpoint]
  arguments += ["--input", "-"] if payload
  stdout, stderr, status = Open3.capture3(
    { "GH_DEBUG" => nil, "GH_HOST" => nil }, *arguments,
    stdin_data: payload ? JSON.generate(payload) : ""
  )
  unless status.success?
    code = stderr[/\(HTTP ([0-9]{3})\)/, 1]
    detail = code ? "HTTP #{code}" : "request failed"
    raise APIError.new("tap #{method} #{detail}", ["409", "422"].include?(code))
  end
  result = JSON.parse(stdout)
  require_condition(result.is_a?(Hash), "tap API returned an invalid response")
  result
rescue JSON::ParserError
  raise APIError, "tap #{method} returned invalid JSON"
end

def read_current
  current = api("GET", "#{TAP_API}?ref=main")
  require_condition(current["type"] == "file" && current["path"] == TAP_PATH &&
                    current["encoding"] == "base64" && current["sha"].is_a?(String) &&
                    current["sha"].match?(/\A[0-9a-f]{40}\z/) && current["content"].is_a?(String),
                    "tap API did not return the existing formula")
  content = Base64.strict_decode64(current["content"].delete("\r\n"))
  require_condition(Digest::SHA1.hexdigest("blob #{content.bytesize}\0#{content}") == current["sha"],
                    "tap formula does not match its file SHA")
  [current["sha"], content]
rescue ArgumentError
  raise PublicationError, "tap formula is not valid base64"
end

def check_version(current, candidate, candidate_version)
  _tag, current_version, _checksum = formula_metadata(current)
  comparison = candidate_version <=> current_version
  require_condition(comparison >= 0, "refusing to downgrade the Homebrew formula")
  require_condition(comparison != 0 || current == candidate,
                    "refusing to replace different formula content for the same version")
  comparison.zero?
end

begin
  script_dir = ARGV.shift
  options = {}
  parser = OptionParser.new do |arguments|
    arguments.banner = "Usage: publish-homebrew.sh --tag v<major>.<minor>.<patch> --formula PATH"
    ["tag", "formula"].each do |name|
      arguments.on("--#{name} VALUE") do |value|
        require_condition(!options.key?(name), "duplicate --#{name} argument")
        options[name] = value
      end
    end
    arguments.on("-h", "--help") { puts arguments; exit }
  end
  parser.parse!
  require_condition(ARGV.empty? && options.keys.sort == ["formula", "tag"], "--tag and --formula are required")
  version(options["tag"])
  candidate = File.binread(options["formula"], 65_537)
  candidate_version = validate_candidate(script_dir, options["tag"], candidate)
  require_condition(!ENV.fetch("GH_TOKEN", "").empty?, "GH_TOKEN is required")

  previous_error = nil
  writes = 0
  loop do
    sha, current = read_current
    if check_version(current, candidate, candidate_version)
      puts "Homebrew formula already matches #{options['tag']}."
      break
    end
    raise previous_error if previous_error && (!previous_error.conflict || writes >= MAX_WRITES)

    writes += 1
    begin
      api("PUT", TAP_API,
          "message" => "Update baseten-switch to #{options['tag']}", "branch" => "main",
          "sha" => sha, "content" => Base64.strict_encode64(candidate))
      _observed_sha, observed = read_current
      if observed == candidate
        puts "Published Homebrew formula for #{options['tag']}."
        break
      end
      check_version(observed, candidate, candidate_version)
      raise APIError, "tap update was not observed after publication"
    rescue APIError => error
      # Re-read before retrying or reporting an ambiguous write failure. A
      # completed write is a no-op; a newer release can never be overwritten.
      previous_error = error
    end
  end
rescue PublicationError => error
  warn "publish-homebrew: #{error.message}"
  exit 1
rescue OptionParser::ParseError, SystemCallError, ArgumentError
  warn "publish-homebrew: invalid arguments, unreadable input, or unavailable command"
  exit 1
end
RUBY
