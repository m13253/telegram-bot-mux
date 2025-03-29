package main

import "encoding/json"

func JSONQuote(s string) string {
	buf, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(buf)
}
