<p align="center">
  <a href="https://hotcfg.run">
    <img src=".github/hotcfg-logo-card.svg" alt="hotcfg.run" width="300">
  </a>
</p>

<h1 align="center">hotcfg CLI</h1>

<p align="center">
  Remote JSON config for your apps, from the command line and CI.<br>
  Push, check, diff, promote and roll back <a href="https://hotcfg.run">hotcfg.run</a> config.
</p>

<p align="center">
  <a href="https://github.com/hotcfg/cli/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/hotcfg/cli?color=FF4F12"></a>
  <a href="https://github.com/hotcfg/cli/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/hotcfg/cli/actions/workflows/ci.yml/badge.svg"></a>
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
  <a href="https://hotcfg.run/docs/cli"><img alt="Docs" src="https://img.shields.io/badge/docs-hotcfg.run-18181B"></a>
</p>

---

The command-line tool for [hotcfg.run](https://hotcfg.run): remote JSON
configuration for apps, served from the edge. Push config from CI, read it back,
diff versions, promote between environments and roll back.

## Install

```sh
brew install --cask hotcfg/tap/hotcfg                 # macOS and Linux
go install github.com/hotcfg/cli/cmd/hotcfg@latest    # with Go 1.27+
```

Or download a binary for Linux, macOS or Windows (amd64, arm64) from the
[releases page](https://github.com/hotcfg/cli/releases) and check it against
`checksums.txt`. It is a single static binary, built with the Go standard
library plus the [jsonschema](https://github.com/santhosh-tekuri/jsonschema)
validator that `hotcfg validate --bin` uses to check files locally (and
[golang.org/x/text](https://pkg.go.dev/golang.org/x/text), which it uses for its
messages).

## Usage

```sh
hotcfg login                                   # saves a secret key
hotcfg validate flags.json --bin mobile-flags  # check against the bin's schema, change nothing
hotcfg types --bin mobile-flags --lang swift -o MobileFlags.swift  # types from the schema
hotcfg schema infer flags.json -o schema.json                    # a schema to start from
hotcfg push flags.json --bin mobile-flags --env staging
hotcfg get mobile-flags --env staging --query '.features'
hotcfg get acme/mobile-flags                   # without a key: name the organization
hotcfg diff mobile-flags --from v3 --to latest
hotcfg promote mobile-flags --from staging --to production
hotcfg promote mobile-flags --from staging --to production --percent 10  # roll out to 10% first
hotcfg rollout mobile-flags --percent 50                                 # then more, then --complete
hotcfg promote mobile-flags --from staging --to production \
  --at 2026-11-21T18:00:00Z --revert-at 2026-11-24T06:00:00Z            # schedule it, and put it back later
hotcfg schedules list mobile-flags
hotcfg rollback mobile-flags --env production --to 3
```

| Command | What it does |
| --- | --- |
| `login` | Validate a secret API key and save it locally |
| `push` | Upload a JSON file as the next version of a bin |
| `validate` | Check a JSON file, and with `--bin` against the bin's schema, without pushing |
| `get` | Print a bin's payload |
| `diff` | Show a colored diff between two versions |
| `rollback` | Make an earlier version current again |
| `promote` | Copy the latest version of one environment to another, to everyone or with `--percent` to some readers first, or with `--at` at a set time |
| `schedules list` / `schedules cancel` | List a bin's scheduled promotes, or cancel one |
| `rollout` | Change the share of a progressive rollout, complete it or abort it |
| `history` | List a bin's versions in one environment |
| `unpublish` | Stop serving an environment, keeping its history |
| `bins list` / `bins create` | List or create your organization's bins |
| `env delete` | Delete an environment of a bin with its whole history |
| `usage` | Show your organization's plan, usage and limits |
| `export` | Download every bin and its history as one JSON file |
| `types` | Generate typed code (TypeScript, Swift, Kotlin, Java, C#, Python, PHP, Ruby, Go, GDScript) for a bin's config from its JSON Schema |
| `schema infer` | Write a JSON Schema that accepts the given JSON files, to start a bin's schema from (no key needed) |

Run `hotcfg help` or `hotcfg <command> --help` for flags.

### Keys and configuration

Each setting is taken from the first place that has it:

1. `--key` and `--api-url` flags
2. `HOTCFG_API_KEY` and `HOTCFG_API_URL` environment variables
3. The credentials saved by `hotcfg login`, in
   `$XDG_CONFIG_HOME/hotcfg/credentials` (default `~/.config/hotcfg/credentials`).
   The file is written with mode `0600`.

In CI, set `HOTCFG_API_KEY` from your secret store. Commands that write need a
secret key (`sk_live_…`). Publishable keys (`pk_…`) are read-only: `get` and
`diff` with one read from `https://private-api.hotcfg.run`, where private bins
are served for apps, and need the bin as `org/bin` (e.g. `acme/flags`). Against
another API, set `HOTCFG_PRIVATE_API_URL` to its private host. Set `NO_COLOR`
to turn off colored output.

Exit codes: `0` success, `1` the command failed (API error, invalid file, ...),
`2` bad command line.

### CI

`hotcfg validate flags.json` checks that the file is valid JSON, offline and
without a key. With `--bin`, it also downloads the bin's JSON Schema (2020-12)
and checks the file against it locally; this needs a secret key with access to
the bin. `push` is still the final check.

`push`, `promote` and `rollback` accept `--json` to print the API's response
(new version, hash, ...) instead of the summary.

On GitHub Actions (`GITHUB_ACTIONS=true`), errors are also printed as
[workflow commands](https://docs.github.com/actions/reference/workflow-commands-for-github-actions),
so schema and syntax errors show on the line of the file in the pull request.
Pass `--no-annotations` to turn this off. For a ready-made workflow step, see the
[hotcfg GitHub Action](https://github.com/hotcfg/action).

## Development

```sh
go vet ./... && go test -race ./...
go build -ldflags "-X main.version=1.2.3" ./cmd/hotcfg
```

Releases: tag a version (`git tag v0.1.0 && git push origin v0.1.0`) and the
release workflow runs [GoReleaser](https://goreleaser.com) to build the binaries,
publish the GitHub release and update the cask in
[`hotcfg/homebrew-tap`](https://github.com/hotcfg/homebrew-tap). Build locally
without publishing with `goreleaser release --snapshot --clean`.

The tests here cover what the CLI does without a server: argument parsing,
configuration, local JSON and schema-error formatting, and diffs. End-to-end
tests against the real API run in the hotcfg server's CI.

## License

[Apache License 2.0](LICENSE)
