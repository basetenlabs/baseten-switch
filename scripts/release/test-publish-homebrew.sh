#!/usr/bin/env bash
# Exercise tap publication against a local GitHub API fixture. No network or
# credential store is used, and the fixture accepts only the expected endpoints.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ruby - "$SCRIPT_DIR/publish-homebrew.sh" "$SCRIPT_DIR/render-formula.sh" <<'RUBY'
require "base64"
require "digest"
require "fileutils"
require "json"
require "open3"
require "tmpdir"

publisher, renderer = ARGV
tap_path = "repos/basetenlabs/homebrew-baseten/contents/Formula/baseten-switch.rb"
checksum = "0123456789abcdef" * 4
other_checksum = "fedcba9876543210" * 4
tag = "v1.2.3"
cases = 0

def check(condition, message)
  abort "FAIL: #{message}" unless condition
end

Dir.mktmpdir("switch-homebrew-test.") do |root|
  bin = File.join(root, "bin")
  FileUtils.mkdir_p(bin)
  fake_gh = File.join(bin, "gh")
  File.write(fake_gh, <<~'FAKE')
    #!/usr/bin/env ruby
    require "base64"
    require "digest"
    require "json"

    state_path = ENV.fetch("HOMEBREW_TEST_STATE")
    log_path = ENV.fetch("HOMEBREW_TEST_LOG")
    args = ARGV.dup
    abort "fixture only supports gh api" unless args.shift == "api"
    method = "GET"
    endpoint = nil
    payload = nil
    hostname = nil
    until args.empty?
      argument = args.shift
      case argument
      when "--method", "-X"
        method = args.shift
      when "--header", "-H"
        args.shift
      when "--hostname"
        hostname = args.shift
      when "--input"
        input = args.shift
        payload = JSON.parse(input == "-" ? STDIN.read : File.read(input))
      else
        abort "unsupported fixture argument" if argument.start_with?("-") || endpoint
        endpoint = argument
      end
    end
    abort "publication did not pin the public API host" unless hostname == "github.com"
    File.open(log_path, "a") { |f| f.puts JSON.generate({"method" => method, "endpoint" => endpoint, "payload" => payload}) }
    state = JSON.parse(File.read(state_path))
    blob_sha = ->(text) { Digest::SHA1.hexdigest("blob #{text.bytesize}\0#{text}") }
    save = -> { File.write(state_path, JSON.generate(state)) }
    fail_http = ->(status) {
      puts JSON.generate({"message" => "Synthetic API failure"})
      warn "gh: Synthetic API failure (HTTP #{status})"
      exit 1
    }
    tap = "repos/basetenlabs/homebrew-baseten/contents/Formula/baseten-switch.rb"
    if method == "GET" && endpoint == "#{tap}?ref=main"
      fail_http.call(404) if state["fault"] == "missing_formula"
      if state["fault"] == "malformed_contents"
        puts '{"type":"file","encoding":"base64","content":"!not-base64!","sha":"invalid"}'
      else
        puts JSON.generate({"type" => "file", "path" => "Formula/baseten-switch.rb", "encoding" => "base64", "content" => state.fetch("formula"), "sha" => state.fetch("sha")})
      end
    elsif method == "PUT" && endpoint == tap
      abort "tap mutation was not limited to one file on main" unless payload && payload.keys.sort == %w[branch content message sha] && payload["branch"] == "main"
      abort "tap mutation did not include the current blob SHA" unless payload["sha"] == state["sha"]
      content = Base64.strict_decode64(payload.fetch("content"))
      abort "tap mutation contained unexpected formula bytes" unless content == Base64.strict_decode64(state.fetch("candidate"))
      state["attempts"] += 1
      save.call
      fail_http.call(500) if state["fault"] == "write_failure"
      if state["fault"] == "unapplied_write"
        puts JSON.generate({"content" => {"sha" => blob_sha.call(content)}, "commit" => {"sha" => "d" * 40}})
        exit 0
      end
      conflict = state["fault"] == "conflict_forever" || (state["fault"].start_with?("conflict_") && state["attempts"] == 1)
      if conflict
        state["formula"] = state.fetch("conflict_formula") if state["fault"] == "conflict_once"
        state["formula"] = state["candidate"] if state["fault"] == "conflict_same"
        state["formula"] = state.fetch("newer_formula") if state["fault"] == "conflict_newer"
        state["sha"] = blob_sha.call(Base64.strict_decode64(state.fetch("formula")))
        save.call
        fail_http.call(409)
      end
      state["formula"] = payload.fetch("content")
      state["sha"] = blob_sha.call(content)
      state["commits"] += 1
      save.call
      fail_http.call(500) if state["fault"] == "lost_write_response"
      puts JSON.generate({"content" => {"sha" => state["sha"], "path" => "Formula/baseten-switch.rb"}, "commit" => {"sha" => "d" * 40}})
    else
      abort "unexpected endpoint or method in publication fixture"
    end
  FAKE
  File.chmod(0755, fake_gh)

  formula_number = 0
  render = lambda do |version, digest|
    formula_number += 1
    output = File.join(root, "rendered-#{formula_number}.rb")
    stdout, stderr, status = Open3.capture3(renderer, "--tag", version, "--sha256", digest,
      "--approved-license-spdx", "MIT", "--output", output, "--patch-output", "#{output}.patch")
    check(status.success?, "fixture renderer failed: #{stderr}#{stdout}")
    File.read(output)
  end
  candidate = render.call(tag, checksum)
  old = render.call("v1.2.1", other_checksum)
  conflict_formula = render.call("v1.2.2", other_checksum)
  newer = render.call("v2.0.0", other_checksum)
  same_version_other_checksum = render.call(tag, other_checksum)
  formula_path = File.join(root, "candidate.rb")
  state_path = File.join(root, "state.json")
  log_path = File.join(root, "calls.jsonl")
  environment = {
    "PATH" => "#{bin}:#{ENV.fetch('PATH')}",
    "GH_TOKEN" => "synthetic-test-token",
    "HOMEBREW_TEST_STATE" => state_path,
    "HOMEBREW_TEST_LOG" => log_path,
  }
  base_state = {
    "formula" => Base64.strict_encode64(old), "candidate" => Base64.strict_encode64(candidate),
    "newer_formula" => Base64.strict_encode64(newer),
    "conflict_formula" => Base64.strict_encode64(conflict_formula),
    "sha" => Digest::SHA1.hexdigest("blob #{old.bytesize}\0#{old}"),
    "fault" => "", "attempts" => 0, "commits" => 0,
  }
  setup = lambda do |state = base_state, formula = candidate|
    state = state.dup
    existing = Base64.strict_decode64(state.fetch("formula"))
    state["sha"] = Digest::SHA1.hexdigest("blob #{existing.bytesize}\0#{existing}")
    File.write(state_path, JSON.generate(state))
    File.write(formula_path, formula)
    File.write(log_path, "")
  end
  current = -> { JSON.parse(File.read(state_path)) }
  calls = -> { File.readlines(log_path).map { |line| JSON.parse(line) } }
  invoke = lambda do |arguments = ["--tag", tag, "--formula", formula_path], env = environment|
    Open3.capture3(env, publisher, *arguments)
  end
  expect_success = lambda do |name|
    stdout, stderr, status = invoke.call
    check(status.success?, "#{name}: #{stderr}#{stdout}")
    cases += 1
  end
  expect_rejected = lambda do |name, arguments = ["--tag", tag, "--formula", formula_path], env = environment|
    before = current.call
    stdout, stderr, status = invoke.call(arguments, env)
    check(!status.success?, "#{name}: unexpectedly succeeded: #{stdout}#{stderr}")
    after = current.call
    check(after["formula"] == before["formula"] && after["commits"] == before["commits"], "#{name}: changed the tap")
    cases += 1
  end

  setup.call
  expect_success.call("updates an existing formula")
  check(current.call["formula"] == Base64.strict_encode64(candidate), "updated formula does not match release")
  check(current.call["commits"] == 1 && current.call["attempts"] == 1, "update did not make exactly one commit")
  writes = calls.call.select { |call| call["method"] != "GET" }
  check(writes.length == 1 && writes.first["endpoint"] == tap_path, "update mutated more than the formula")
  expect_success.call("rerun of completed update")
  check(current.call["commits"] == 1 && current.call["attempts"] == 1, "identical rerun wrote another commit")

  {
    "downgrade" => newer,
    "same version with changed checksum" => same_version_other_checksum,
    "same version with changed formula" => candidate.sub('license "MIT"', 'license "Apache-2.0"'),
    "unrecognized existing formula" => "class Unexpected < Formula\nend\n",
  }.each do |name, existing|
    setup.call(base_state.merge("formula" => Base64.strict_encode64(existing)))
    expect_rejected.call(name)
    check(current.call["attempts"] == 0, "#{name}: attempted a write")
  end

  {
    "noncanonical URL" => candidate.sub("https://github.com/", "https://example.invalid/"),
    "changed asset name" => candidate.sub("darwin_universal.zip", "darwin_arm64.zip"),
    "invalid checksum" => candidate.sub(checksum, "not-a-checksum"),
    "injected Ruby" => candidate + "system('false')\n",
    "duplicate checksum" => candidate.sub("  sha256", "  sha256 \"#{checksum}\"\n  sha256"),
  }.each do |name, formula|
    setup.call(base_state, formula)
    expect_rejected.call(name)
    check(current.call["attempts"] == 0, "#{name}: attempted a write")
  end

  [
    ["missing tag", ["--formula", formula_path]],
    ["invalid tag", ["--tag", "v1.2.3/other", "--formula", formula_path]],
    ["missing formula", ["--tag", tag]],
    ["missing formula file", ["--tag", tag, "--formula", File.join(root, "absent.rb")]],
    ["duplicate option", ["--tag", tag, "--tag", tag, "--formula", formula_path]],
    ["unknown option", ["--tag", tag, "--formula", formula_path, "--branch", "other"]],
  ].each do |name, arguments|
    setup.call
    expect_rejected.call(name, arguments)
    check(current.call["attempts"] == 0, "#{name}: attempted a write")
  end
  setup.call
  expect_rejected.call("missing token", ["--tag", tag, "--formula", formula_path], environment.merge("GH_TOKEN" => nil))

  %w[missing_formula malformed_contents write_failure unapplied_write].each do |fault|
    setup.call(base_state.merge("fault" => fault))
    expect_rejected.call(fault)
  end

  setup.call(base_state.merge("fault" => "lost_write_response"))
  expect_success.call("verifies a successful update after its response is lost")
  check(current.call["commits"] == 1 && current.call["attempts"] == 1, "lost response caused a duplicate write")

  setup.call(base_state.merge("fault" => "conflict_once"))
  expect_success.call("retries a stale SHA after refetching")
  check(current.call["commits"] == 1 && current.call["attempts"] == 2, "SHA conflict did not produce one successful update")
  shas = calls.call.select { |call| call["method"] == "PUT" }.map { |call| call["payload"]["sha"] }
  check(shas.uniq.length == 2, "SHA conflict reused stale content SHA")

  setup.call(base_state.merge("fault" => "conflict_same"))
  expect_success.call("concurrent publisher already wrote identical formula")
  check(current.call["attempts"] == 1 && current.call["commits"] == 0, "converged update was written again")

  setup.call(base_state.merge("fault" => "conflict_newer"))
  _, _, status = invoke.call
  check(!status.success?, "concurrent newer release was not rejected")
  check(current.call["formula"] == Base64.strict_encode64(newer) && current.call["attempts"] == 1 && current.call["commits"] == 0,
    "retry replaced a concurrent newer release")
  cases += 1

  setup.call(base_state.merge("fault" => "conflict_forever"))
  expect_rejected.call("bounded repeated SHA conflicts")
  check(current.call["attempts"].between?(2, 3), "SHA retries are not bounded")
  state = current.call.merge("fault" => "")
  File.write(state_path, JSON.generate(state))
  expect_success.call("safe rerun after conflicts stop")
  check(current.call["commits"] == 1, "rerun did not complete exactly one update")
end

puts "PASS: Homebrew publication contract (#{cases} local fixture cases)"
RUBY
