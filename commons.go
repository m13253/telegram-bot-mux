package main

import (
	"encoding/json"
)

const (
	httpBodyLimit = 32 << 20 // http.defaultMaxMemory
	httpUserAgent = "Mozilla/5.0 Telegram-bot-muxer/1.0 (+https://github.com/m13253/telegram-bot-muxer)"
)

func quoteJSON(s string) string {
	buf, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(buf)
}
