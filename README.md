# hotcfg CLI

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
`checksums.txt`. It is a single static binary built with only the Go standard
library.

## Usage

```sh
hotcfg login                                   # saves a secret key
hotcfg push flags.json --bin mobile-flags --env staging
hotcfg get mobile-flags --env staging --query '.features'
hotcfg get acme/mobile-flags                   # without a key: name the organization
hotcfg diff mobile-flags --from v3 --to latest
hotcfg promote mobile-flags --from staging --to production
hotcfg rollback mobile-flags --env production --to 3
```

| Command | What it does |
| --- | --- |
| `login` | Validate a secret API key and save it locally |
| `push` | Upload a JSON file as the next version of a bin |
| `get` | Print a bin's payload |
| `diff` | Show a colored diff between two versions |
| `rollback` | Make an earlier version current again |
| `promote` | Copy the latest version of one environment to another |
| `history` | List a bin's versions in one environment |
| `unpublish` | Stop serving an environment, keeping its history |
| `bins list` / `bins create` | List or create your organization's bins |
| `env delete` | Delete an environment of a bin with its whole history |
| `usage` | Show your organization's plan, usage and limits |

Run `hotcfg help` or `hotcfg <command> --help` for flags.

### Keys and configuration

Each setting is taken from the first place that has it:

1. `--key` and `--api-url` flags
2. `HOTCFG_API_KEY` and `HOTCFG_API_URL` environment variables
3. The credentials saved by `hotcfg login`, in
   `$XDG_CONFIG_HOME/hotcfg/credentials` (default `~/.config/hotcfg/credentials`).
   The file is written with mode `0600`.

In CI, set `HOTCFG_API_KEY` from your secret store. Commands that write need a
secret key (`sk_live_…`). Publishable keys (`pk_…`) are read-only. Set `NO_COLOR`
to turn off colored output.

Exit codes: `0` success, `1` the command failed (API error, invalid file, ...),
`2` bad command line.

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
