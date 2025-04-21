package main

import (
	_ "embed"
)

const (
	httpBodyLimit = 32 << 20 // http.defaultMaxMemory
	httpUserAgent = "Mozilla/5.0 Telegram-bot-muxer/1.0 (+https://github.com/m13253/telegram-bot-muxer)"
)

//go:embed webconsole/index.html
var webConsoleBody []byte
