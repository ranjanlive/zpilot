# Deployment

## Desktop (Windows, macOS, Linux)

```bash
zpilot desktop      # daemon + app window + tray icon + notifications
zpilot stop         # stop everything
```

- **The window.** `zpilot desktop` starts the daemon if it is not already running. It then opens the dashboard as an app window, using Edge, Chrome, Chromium or Brave in `--app` mode with its own profile, or your default browser if none is found.
- **Windows tray icon.** The menu shows status and has toggles for notifications and **Start at login**. **Close tray** keeps your agents running; **Quit** stops ZPilot.
- **macOS and Linux.** Notifications use Notification Center and `notify-send`. Start at login is set up with a LaunchAgent on macOS and an XDG autostart entry on Linux.

## Headless server (systemd)

```bash
sudo useradd --system --create-home --home-dir /var/lib/zpilot zpilot
sudo install -m 0755 zpilot /usr/local/bin/zpilot
sudo -u zpilot zpilot --data /var/lib/zpilot/data admin password
```

`/etc/systemd/system/zpilot.service`:

```ini
[Unit]
Description=ZPilot
After=network-online.target
Wants=network-online.target

[Service]
User=zpilot
ExecStart=/usr/local/bin/zpilot --data /var/lib/zpilot/data serve --listen 127.0.0.1:7700
Restart=on-failure
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/var/lib/zpilot

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now zpilot
```

Put a TLS reverse proxy in front of it, for example Caddy:

```
agents.example.com {
    reverse_proxy 127.0.0.1:7700
}
```

Then sign in at `https://agents.example.com` with the admin password. Run CLI commands on the server as the `zpilot` user, for example `sudo -u zpilot zpilot --data /var/lib/zpilot/data status`.

Because the daemon runs as a dedicated, unprivileged user with `ProtectSystem=strict`, even `trusted` agents can only change what that user can change. This is the recommended setup for agents that run commands.

## Channels that need a public URL

WhatsApp (and any webhook called from the internet) must reach ZPilot over HTTPS. Telegram, Discord and Email do not, because ZPilot connects out to them.

- **On a VPS** behind Caddy, the WhatsApp callback is `https://agents.example.com/hooks/whatsapp/<channel id>`.
- **On a laptop,** use a tunnel (Cloudflare Tunnel, ngrok, and similar) that points at `127.0.0.1:7700`. Expose only `/hooks/*`, not the dashboard.

## Backups

Back up the whole data directory while the daemon is stopped, or copy `state.json`, `threads/`, `secrets.json`, `master.key`, `library/` and `agents/`.

On Windows, `master.key` is sealed to your user account, so restore it as the same user. Otherwise re-enter the model keys after restoring.
