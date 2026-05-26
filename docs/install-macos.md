# Installing & running Rick on macOS

Rick has no first-party installer for macOS. The `Makefile`'s `deploy` and
`restart` targets assume Linux `systemctl --user` and will fail on Darwin.
On macOS the equivalent is a per-user `launchd` LaunchAgent. This page
documents the working setup.

## 1. Prerequisites

- Go 1.24+ (`go version`).
- `~/.local/bin` on `$PATH` (Rick binaries install there; create with
  `mkdir -p ~/.local/bin` and add to `~/.zshrc` if missing).
- An AI backend CLI on `$PATH`: `claude`, `gemini`, `codex`, or `agy` (Antigravity).
  Override with `RICK_CLAUDE_BIN` / `RICK_GEMINI_BIN` / `RICK_CODEX_BIN` /
  `RICK_ANTIGRAVITY_BIN` if the binary lives elsewhere.

## 2. Build

```sh
make build
```

Writes the binary to `$HOME/.local/bin/rick` (see `RICK_BIN` in `Makefile:4`).
The Makefile target is portable; only the `deploy` / `restart` targets
need a macOS-specific substitute.

## 3. Configure env vars

Rick reads `$HOME/.config/rick/env` at startup (loaded by the LaunchAgent
wrapper, see step 4). It is a plain shell file — one `KEY=value` per line.

```sh
mkdir -p ~/.config/rick
cat > ~/.config/rick/env <<'EOF'
# Required for any workflow that uses an isolated workspace.
RICK_REPOS_PATH=$HOME/work/rick-workspaces

# Integrations (set what you use).
JIRA_URL=https://your.atlassian.net
JIRA_EMAIL=you@example.com
JIRA_TOKEN=<api-token>
CONFLUENCE_URL=https://your.atlassian.net/wiki
GITHUB_TOKEN=<gh-pat>

# Backend overrides — only if the CLI binary is not on $PATH.
# RICK_CLAUDE_BIN=/opt/homebrew/bin/claude
# RICK_GEMINI_BIN=/opt/homebrew/bin/gemini
# RICK_CODEX_BIN=/opt/homebrew/bin/codex
# RICK_ANTIGRAVITY_BIN=/opt/homebrew/bin/agy

# macOS workstations without docker-compose stack virtualization want this:
# RICK_DISABLE_QUALITY_GATE=1
EOF
chmod 600 ~/.config/rick/env   # contains secrets
```

Full env-var reference: root `CLAUDE.md` (Environment variables table).

## 4. Install the LaunchAgent

LaunchAgents live in `~/Library/LaunchAgents/`. Drop the plist below at
`~/Library/LaunchAgents/com.marconn.rick.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.marconn.rick</string>

    <!--
        The wrapper script sources $HOME/.config/rick/env so credentials
        live in one place and don't have to be duplicated into the plist.
        `set -a` exports every variable defined in that file to the rick
        subprocess.
    -->
    <key>ProgramArguments</key>
    <array>
        <string>/bin/sh</string>
        <string>-c</string>
        <string>set -a; [ -f "$HOME/.config/rick/env" ] &amp;&amp; . "$HOME/.config/rick/env"; set +a; exec "$HOME/.local/bin/rick" serve --addr :58077 --grpc-addr :59077 --db "$HOME/.local/share/rick/rick.db" --backend claude</string>
    </array>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/Users/YOURUSER/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>

    <key>WorkingDirectory</key>
    <string>/Users/YOURUSER</string>

    <!-- Start at login, restart on crash. -->
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <!-- launchd refuses to relaunch faster than this; bumps to 10s
         so a crash loop won't peg the CPU. -->
    <key>ThrottleInterval</key>
    <integer>10</integer>

    <key>ProcessType</key>
    <string>Background</string>

    <key>StandardOutPath</key>
    <string>/Users/YOURUSER/Library/Logs/rick/out.log</string>
    <key>StandardErrorPath</key>
    <string>/Users/YOURUSER/Library/Logs/rick/err.log</string>
</dict>
</plist>
```

Replace every `YOURUSER` with the output of `whoami`. The PATH must include
the directory holding your backend CLI (Homebrew's `/opt/homebrew/bin` on
Apple Silicon, `/usr/local/bin` on Intel) — launchd does NOT inherit your
shell's PATH.

Create the log directory once: `mkdir -p ~/Library/Logs/rick`.

Then load:

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.marconn.rick.plist
```

(Use `bootout` to remove: `launchctl bootout gui/$(id -u)/com.marconn.rick`.)

## 5. Daily ops

| Task | Command |
|---|---|
| Status | `launchctl print gui/$(id -u)/com.marconn.rick \| grep -E 'state\|pid =\|last exit'` |
| Restart (after rebuild) | `launchctl kickstart -k gui/$(id -u)/com.marconn.rick` |
| Stop | `launchctl kill TERM gui/$(id -u)/com.marconn.rick` |
| Start (without reload) | `launchctl kickstart gui/$(id -u)/com.marconn.rick` |
| Tail logs | `tail -f ~/Library/Logs/rick/err.log` |
| Smoke test HTTP | `curl -sS http://localhost:58077/mcp -o /dev/null -w '%{http_code}\n'` (expect `200`) |

Typical rebuild + restart loop:

```sh
make build && launchctl kickstart -k gui/$(id -u)/com.marconn.rick
```

`kickstart -k` is the LaunchAgent analogue of `systemctl restart`: SIGTERMs
the running process, waits for exit, then immediately respawns it.
`KeepAlive=true` in the plist also handles unsupervised crashes.

## 6. Optional plugin services

The root Makefile's `build-plugins` and `deploy-plugins` targets live in a
sibling `../rick-plugins` repo and assume Linux systemd. On macOS, mirror
the plist above per plugin (different `Label`, different `ProgramArguments`
pointing at `$HOME/.local/bin/rick-jira-poller`, etc.). Each plugin
inherits the same env file convention via the `set -a; . "$HOME/.config/rick/env"`
wrapper.

## 7. Uninstall

```sh
launchctl bootout gui/$(id -u)/com.marconn.rick
rm ~/Library/LaunchAgents/com.marconn.rick.plist
rm -f ~/.local/bin/rick
rm -rf ~/.local/share/rick    # event store, blobs — destructive
rm -rf ~/Library/Logs/rick
```

`~/.config/rick/env` is preserved by default (re-uses credentials if you
reinstall later).

## 8. Why no Homebrew formula?

There isn't one yet. The blocker is that Rick depends on backend CLIs
(claude, gemini, codex, agy) that themselves don't have stable Homebrew
formulae, plus a workspace root (`RICK_REPOS_PATH`) that needs user input.
A `brew install rick && rick init` flow is plausible if/when those settle —
PRs welcome.
