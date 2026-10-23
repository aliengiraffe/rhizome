# Contributing to Rhizome

Contributions to code, documentation, and reproducible bug reports are welcome.
Start with the [README](README.md) for capabilities and supported agent integrations.

## Discuss the change

Search [existing issues](https://github.com/aliengiraffe/rhizome/issues) before
opening a bug report or feature request using the issue chooser. For substantial
changes, discuss the problem and proposed behavior before implementing it. Small
fixes can go straight to a pull request.

Report suspected vulnerabilities privately using [SECURITY.md](SECURITY.md), not
public issues or pull requests. Never include credentials, tokens, private
transcripts, or other people's personal information in reports or examples.

## Set up a checkout

Install Git, Go 1.27.3+, Python 3, and [Task v3](https://taskfile.dev/docs/installation).
Fork the repository on GitHub if you do not have push access. In the clone command
below, replace `aliengiraffe` with your GitHub username to clone your fork; use the
upstream URL as written if you have push access:

```sh
git clone https://github.com/aliengiraffe/rhizome.git
cd rhizome
git switch -c describe-your-change
task build
./rhizome --version
```

Work on a feature branch, not `main`. Building and the synthetic tests do not
require installing Rhizome into your coding agents or starting a user service.
Use disposable state for testing; `rhizome setup` changes user configuration.

## Implement and validate

[AGENTS.md](AGENTS.md#commands-and-completion) is the authoritative engineering
and validation checklist. Read it before editing; it includes the required build,
vet/race tests, stdio smokes (local and LAN), service smoke, whitespace check, and
additional checks for particular areas. Run targeted package tests first for code
changes, and format Go files with `gofmt`.

Before running checks, isolate `HOME`, agent homes (`CODEX_HOME`,
`CLAUDE_CONFIG_DIR`), XDG directories, and Rhizome configuration/runtime inputs in
temporary directories. Clear inherited agent and `RHIZOME_*` overrides so setup
tests cannot rewrite your real registrations. See the isolation example in
[AGENTS.md](AGENTS.md#commands-and-completion).

The automated smokes use synthetic transcripts and isolated processes. They do
not prove live model or cross-device behavior. Live checks for
[Codex](docs/codex.md) and [OpenCode](docs/opencode.md) are explicitly opt-in,
require their documented prerequisites, and run real model turns. The
[verification guide](docs/verification.md) records tested scope and manual checks.
State clearly which checks you ran and which remain unverified.

Keep changes focused. Update README/docs when behavior changes, and update
AGENTS.md when its feature or engineering guidance changes. Check documentation
links and paths. Avoid adding dependencies or changing unrelated code.

## Open a pull request

Commit your changes using your existing Git identity, push your branch to your
fork (or the repository if authorized), and open a pull request targeting `main`.
Describe the problem, resulting behavior, related issue, validation, and material
limitations using the PR template. Use `Closes #123` only when the PR resolves that
issue. Do not add coding-agent attribution to commits or PR descriptions.

Maintainers review scope, behavior, tests, and documentation. Respond to feedback
and push follow-up changes to the same branch. Keep incomplete work in a draft PR
and call out any remaining prerequisites; review and merge timing is not guaranteed.
