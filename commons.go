package main

import (
	"encoding/json"
	"strings"
)

const (
	httpBodyLimit = 32 << 20 // http.defaultMaxMemory
	httpUserAgent = "Mozilla/5.0 Telegram-bot-muxer/1.0 (+https://github.com/m13253/telegram-bot-muxer)"
)

func quoteJSON(s string) string {
	var buf strings.Builder
	err := json.NewEncoder(&buf).Encode(s)
	if err != nil {
		panic(err)
	}
	return buf.String()
}
