# Codex exec client

This package is an in-tree fork of `github.com/godeps/codex-sdk-go` v0.1.2
(`a46504d80f69a8c15fe759f2af3da690f7dc858e`). It runs `codex exec
--experimental-json` and decodes its JSONL event stream.

The upstream SDK moved its primary API to app-server after the v0.1 line.
This fork keeps the exec transport independently maintainable by this project.

When updating it, compare changes with the recorded upstream revision, preserve
the MIT license, and extend the local tests for every JSONL protocol change.
