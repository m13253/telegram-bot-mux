# Telegram-bot-mux

Telegram-bot-mux acts as a reverse proxy / bouncer / multiplexer, allowing you to put different modules of a Telegram bot into separate processes.

## Usage

```bash
$ git clone https://github.com/m13253/telegram-bot-mux.git
$ cd telegram-bot-mux
$ go build -v  # Make take a while
$ cp tbmux.conf.example tbmux.conf
```

Then, edit `tbmux.conf`:
```toml
# The database file path
db = "tbmux.db"

[upstream]
# Specify the URL prefix of the upstream Telegram Bot API, or a Local Bot API Server.
# Refer to https://core.telegram.org/bots/api#using-a-local-bot-api-server for information about Local Bot API Servers.
api_url = "https://api.telegram.org/bot"

# Specify the URL prefix of the upstream Telegram Bot File Server, or a Local Bot API Server.
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

# Specify an HTTP path for telegram-bot-mux to serve a downstream Telegram Bot API
api_path = "/bot"

# Specify an HTTP path for telegram-bot-mux to serve a downstream Telegram Bot File Server
# Any requests to this path will be reverse-proxied to the upstream Bot File Server
file_path = "/file/bot"

# Specify any authentication token for your downstream clients as you wish
auth_token = "123456:AnotherToken"
```

After that, run `telegram-bot-mux`:
```bash
$ ./telegram-bot-mux --conf tbmux.conf
```

Alternatively, you can create a systemd unit to run telegram-bot-mux automatically at login:
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
$ loginctl enable-linger "$USER"
$ systemctl enable --now --user telegram-bot-mux.service
```

## Connecting to telegram-bot-mux

You can develop each of your modules as a separate Telegram bot, but specifying their Telegram Bot API to telegram-bot-mux.

1. Telegram-bot-mux does not delete confirmed updates after a successful `getUpdates`, allowing all clients to retrieve updates at their own paces. If `offset` is omitted or set to 0 or 1, instead of returning a list of unconfirmed updates, it returns an empty update to help the client calculate its next `offset`.
2. Telegram-bot-mux ignores the `filter_update_types` parameter from downstream `getUpdates`. Please specify it through the configuration file.
3. Telegram-bot-mux will echo all sent messages back to the next `getUpdates`, allowing different clients to see messages sent by each other. If your bot needs to respond to all incoming messages, please filter out messages send by the bot itself.
4. However, messages sent by `copyMessage` and `copyMessages` will not be echoed due to missing information from upstream.

## Rate limiting

Telegram-bot-mux implements a queuing system to limit the total message sending rate to the upstream.

According to <https://core.telegram.org/bots/faq#my-bot-is-hitting-limits-how-do-i-avoid-this>, we limit the global sending pace to 1/30 sec, private chat to 1 sec, and non-private chat to 3 sec. Private/non-private information is passively collected from prior updates, and defaults to non-private for unknown `chat_id`.

Rate limiting is automatically applied to all API calls with a `chat_id` parameter. It supports URL query string, `application/x-www-form-urlencoded`, `application/json`, but `multipart/form-data` bypasses rate limiting due to memory usage concerns.

While in queue, the client can cancel the pending API call by canceling the HTTP request.

## Web console

Telegram-bot-mux provides a simple web console at `http://<listen_addr>/<api_path><auth_token>/.tbmuxConsole`.

By visiting this web console using a web browser, you can check the list of previously received text messages, and send out text messages as your bot.

However, this web console only supports text messages right now. No images, stickers, or attachments can be displayed or sent yet.
