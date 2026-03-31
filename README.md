# cctop

A terminal UI for monitoring and managing your running [Claude Code](https://docs.anthropic.com/en/docs/claude-code) sessions.

## Features

- Lists all running Claude Code sessions with PID, state, working directory, and terminal app
- Session states: **working** (actively generating), **waiting** (needs your input), **idle** (at prompt)
- Kill sessions directly from the TUI
- Auto-refreshes every 2 seconds
- Detects parent terminal app (Ghostty, iTerm, tmux, etc.)

## Install

### Homebrew

```sh
brew install montanaflynn/tap/cctop
```

### Go

```sh
go install github.com/montanaflynn/cctop@latest
```

### From releases

Download the latest binary from [GitHub Releases](https://github.com/montanaflynn/cctop/releases).

## Usage

```sh
cctop
```

### Keybindings

| Key | Action |
|-----|--------|
| `↑/↓` | Navigate sessions |
| `k` | Kill selected session |
| `q` / `Esc` | Quit |

## How it works

cctop reads Claude Code's session files (`~/.claude/sessions/*.json`) and conversation logs to determine each session's state. It cross-references with running processes to detect the terminal app and working directory.

## License

MIT
