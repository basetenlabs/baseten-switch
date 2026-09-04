package config

import _ "embed"

// InitTemplate is the default gateway.yaml written by 'baseten-switch
// config init': the single-port door topology (front door on
// 127.0.0.1:45271 forwarding to the shared router listener on
// 127.0.0.1:45272, one global routing gate, Claude Code defaulting
// every model family to GLM-5.2, an enabled Claude Code model picker, and a
// native fallback).
//
// The embedded file is a byte-for-byte copy of the repo-level
// config/gateway.example.yaml. go:embed cannot reference files outside
// the module (the example lives one directory above the gateway
// module), so the copy below is embedded instead, and
// TestInitTemplateMatchesExampleConfig pins the two files to byte
// equality. Edit them together; the check.sh gate fails on drift.
//
//go:embed gateway.example.yaml
var InitTemplate []byte
