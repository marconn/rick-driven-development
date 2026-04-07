# deploy/systemd-user

- systemd **user** unit files for running rick-server (and supporting daemons) under the current user account, no root required.

## Units
- `rick-server.service` — `Type=simple`, runs `%h/.local/bin/rick serve --db %h/.local/share/rick/rick.db --addr :58077 --grpc-addr :59077 --backend claude`
  - `EnvironmentFile=-%h/.config/rick/env` (optional, leading `-` ignores if missing)
  - `WorkingDirectory=%h/go/src/github.com/hulilabs`
  - `Restart=always`, `RestartSec=5`, `StartLimitBurst=5`, `StartLimitIntervalSec=60`
  - `WantedBy=default.target`
- Companion services restarted by the Makefile but **not** stored in this directory: `rick-github-reporter.service`, `rick-jira-poller.service`, `rick-jira-planner.service`, `rick-planning.service`. Their unit files live elsewhere (e.g. installed by hand into `~/.config/systemd/user/`).

## Install / restart
- `cp deploy/systemd-user/*.service ~/.config/systemd/user/`
- `systemctl --user daemon-reload`
- `systemctl --user enable --now rick-server.service` (first time only)
- `systemctl --user restart rick-server.service` after every binary deploy — see memory `feedback_restart_after_deploy.md`. Without a restart the new `~/.local/bin/rick` binary is not picked up.
- Status: `systemctl --user status rick-server.service`
- Logs: `journalctl --user -u rick-server.service -f`

## Env file
- `~/.config/rick/env` carries env vars consumed by the service: `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN`, `CONFLUENCE_URL`, `CONFLUENCE_EMAIL`, `CONFLUENCE_TOKEN`, `RICK_REPOS_PATH`, `RICK_DISABLE_QUALITY_GATE`, `RICK_CLAUDE_BIN`, `RICK_GEMINI_BIN`, `RICK_MODEL`, `RICK_LOG_LEVEL`, etc. See top-level `CLAUDE.md` for the full list.
- File is optional — the unit uses `EnvironmentFile=-` so a missing file is not fatal.

## Related
- `../../Makefile` — `make deploy` builds + restarts `rick-server`; `make restart` restarts rick-server plus all four companion services.
- `../rick-server.service` — root/system-wide variant (`User=rick`, `/usr/bin/rick`, `/var/lib/rick/rick.db`, `WantedBy=multi-user.target`). Duplicated on purpose: this user-unit copy is what is actually used in dev; the root copy is a reference for system-wide deployments.
