# deploy/

- Packaging and systemd assets for shipping `rick-server` and the `rick-agent` desktop app to a Linux machine.

## Files
- `rick-agent.desktop` — XDG desktop entry installed by the .deb to `/usr/share/applications/`. `Exec=/usr/bin/rick-agent`, `Icon=rick-agent`, `StartupWMClass=rick-agent`.
- `rick-agent.svg` — app icon installed by the .deb to `/usr/share/icons/hicolor/scalable/apps/rick-agent.svg`.
- `rick-server.service` — system-wide systemd unit (`User=rick`, `ExecStart=/usr/bin/rick serve --db /var/lib/rick/rick.db --addr :8077`, `WantedBy=multi-user.target`). Reference for full system-wide deployments — NOT what dev uses.
- `rick-agent_<version>_amd64.deb` — built artifacts (gitignored). Currently `0.1.0` through `0.8.0` present locally.
- `systemd-user/` → see `systemd-user/CLAUDE.md` for the user-level rick-server unit that dev actually runs.

## Workflow
- `make package` — builds the .deb (binary + desktop entry + icon + control file) into this directory.
- `make install-agent` — installs the .deb. The ONLY supported way to get `rick-agent` into `/usr/bin`.
- `make deploy` — builds + restarts the user `rick-server` service.
- `make restart` — restarts `rick-server` plus the companion poller/reporter/planner services.

## Important conventions (from project memory)
- `rick-agent` binary lives ONLY at `/usr/bin/rick-agent` (from .deb), NEVER at `~/.local/bin/`. See memory `feedback_deb_only_rick_agent.md`.
- `rick-server` binary lives at `~/.local/bin/rick` and is run by the systemd **user** service. Always restart the service after deploying a new binary or changes don't take effect — see memory `feedback_restart_after_deploy.md`.
- Always deploy with `go build -o ~/.local/bin/rick`, not `go install` — see memory `feedback_deploy_binary_path.md`.

## Two service files (intentional duplication)
- `deploy/rick-server.service` — **system-wide** variant: `User=rick`, `/usr/bin/rick`, `/var/lib/rick/rick.db`, `WantedBy=multi-user.target`. Reference only.
- `deploy/systemd-user/rick-server.service` — **user-level** variant: `%h/.local/bin/rick`, `%h/.local/share/rick/rick.db`, `--grpc-addr :9077`, `EnvironmentFile=-%h/.config/rick/env`, `WantedBy=default.target`. This is the one the Makefile actually copies and dev runs.

## Related
- `../Makefile` — `package`, `install-agent`, `deploy`, `restart` targets.
- `systemd-user/CLAUDE.md` — user-unit install/restart steps and env file contract.
- `../agent/` — Wails v2 + Svelte 5 source for the `rick-agent` binary the .deb wraps.
