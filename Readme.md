# Telegram-bot-mux

Telegram-bot-mux acts as a reverse proxy, allowing you to put different modules of a Telegram bot into separate processes.

## Usage

```bash
$ git clone https://github.com/m13253/telegram-bot-mux.git
$ cd telegram-bot-mux
$ go build
$ cp tbmux.conf.example tbmux.conf
```

Then, edit `tbmux.conf`:
```toml
# Database file path
db = "tbmux.db"

[upstream]
# Specify the URL prefix of Telegram Bot API, or a Local Bot API Server.
# Refer to https://core.telegram.org/bots/api#using-a-local-bot-api-server for information about Local Bot API Servers.
api_url = "https://api.telegram.org/bot"

# Specify the URL prefix of Telegram Bot File Server, or a Local Bot API Server.
file_url = "https://api.telegram.org/file/bot"

# Specify the authentication token
auth_token = "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"

# When polling from upstream using getUpdate, keep the request open for this number of seconds.
polling_timeout = 60

# When polling from upstream using getUpdate fails with a temporary error, telegram-bot-mux will retry after 1, 2, 4, 8, …, max_retry_interval seconds
max_retry_interval = 600

# When polling from upstream using getUpdate, ask the upstream server to only send these update types.
# The default value is "all updates except chat_member, message_reaction, and message_reaction_count"
# Refer to https://core.telegram.org/bots/api#getupdates for detailed description.
filter_update_types = []

[downstream]
# Specify a TCP address and port for telegram-bot-mux to listen on
listen_addr = "localhost:8080"

# Specify an HTTP path for telegram-bot-mux to serve Telegram Bot API
api_path = "/bot"

# Specify an HTTP path for telegram-bot-mux to serve Telegram Bot File Server
# Any requests to this path will be reverse-proxied to upstream Bot File Server
file_path = "/file/bot"

# Specify any authentication token for your downstream clients as you wish
auth_token = "123456:AnotherToken"
```

After that, run `telegram-bot-mux`:
```bash
$ ./telegram-bot-mux --conf tbmux.conf
```

Alternatively, you can create a systemd unit to run telegram-bot-mux:
```bash
$ cat >~/.config/systemd/user/telegram-bot-mux.service <<EOF
[Unit]
Description=telegram-bot-mux
After=network.target

[Service]
Type=simple
WorkingDirectory=%h/telegram-bot-mux/telegram-bot-mux
ExecStart=%h/telegram-bot-mux/telegram-bot-mux --conf tbmux.conf
Restart=always
RestartSec=1s
RestartMaxDelaySec=521s
RestartSteps=13

[Install]
WantedBy=default.target
EOF

$ systemctl daemon-reload --user
$ systemctl enable --now --user telegram-bot-mux.service
```

## Connecting to telegram-bot-mux

You can develop each of your modules as a separate Telegram bot, but specifying their Telegram Bot API to telegram-bot-mux.

1. Telegram-bot-mux does not clear processed updates after a successful `getUpdates`, allowing other clients to retrieve them. If `offset` is omitted, it defaults to `-1` instead of `0`.
2. Telegram-bot-mux ignores the `filter_update_types` parameter from downstream `getUpdates`. Specify it through the configuration file.
3. Telegram-bot-mux will echo all sent messages back to the next `getUpdates`, allowing different clients to see messages sent by each other.
4. However, messages sent by `copyMessage` and `copyMessages` will not be echoed due to missing information from upstream.
