package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
)

type Client struct {
	conf                *Config
	db                  *Database
	typesNeedCaching    map[string]struct{}
	echoProcessor       map[string]func(int64, []byte)
	last_retry_interval time.Duration
}

func NewClient(conf *Config, db *Database) *Client {
	c := &Client{
		conf: conf,
		db:   db,
		typesNeedCaching: map[string]struct{}{
			"message":                 {},
			"edited_message":          {},
			"channel_post":            {},
			"edited_channel_post":     {},
			"business_message":        {},
			"edited_business_message": {},
		},
		last_retry_interval: time.Second,
	}
	c.echoProcessor = map[string]func(int64, []byte){
		"sendMessage":             c.processEchoMessage,
		"forwardMessage":          c.processEchoMessage,
		"copyMessage":             c.processEchoMessage,
		"sendPhoto":               c.processEchoMessage,
		"sendAudio":               c.processEchoMessage,
		"sendDocument":            c.processEchoMessage,
		"sendVideo":               c.processEchoMessage,
		"sendAnimation":           c.processEchoMessage,
		"sendVoice":               c.processEchoMessage,
		"sendVideoNote":           c.processEchoMessage,
		"sendPaidMedia":           c.processEchoMessage,
		"sendMediaGroup":          c.processEchoMessageArray,
		"sendLocation":            c.processEchoMessage,
		"sendVenue":               c.processEchoMessage,
		"sendContact":             c.processEchoMessage,
		"sendPoll":                c.processEchoMessage,
		"sendDice":                c.processEchoMessage,
		"editMessageText":         c.processEchoMessageEdit,
		"editMessageCaption":      c.processEchoMessageEdit,
		"editMessageMedia":        c.processEchoMessageEdit,
		"editMessageLiveLocation": c.processEchoMessageEdit,
		"stopMessageLiveLocation": c.processEchoMessageEdit,
		"editMessageReplyMarkup":  c.processEchoMessageEdit,
	}
	return c
}

func (c *Client) StartPolling(ctx context.Context) error {
	offset := uint64(0)

	for {
		var requestURL string
		if offset == 0 {
			requestURL = fmt.Sprintf("%s/getUpdates?timeout=%d&allowed_updates=%s", c.conf.Upstream.ApiPrefix, c.conf.Upstream.PollingTimeout, c.conf.Upstream.FilterUpdateTypesStr)
		} else {
			requestURL = fmt.Sprintf("%s/getUpdates?offset=%d&timeout=%d&allowed_updates=%s", c.conf.Upstream.ApiPrefix, offset, c.conf.Upstream.PollingTimeout, c.conf.Upstream.FilterUpdateTypesStr)
		}
		log.Println("GET", requestURL)

		req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
		if err != nil {
			return fmt.Errorf("failed to send HTTP request: %v", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 Telegram-bot-muxer/1.0 (+https://github.com/m13253/telegram-bot-muxer)")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// Assume this is not a fatal errors
			log.Println("Upstream HTTP request error:", err)
			c.sleepUntilRetry()
			continue
		}
		defer resp.Body.Close()

		requestSucceed := resp.StatusCode >= 200 && resp.StatusCode < 300
		failureIsFatal := resp.StatusCode >= 400 && resp.StatusCode < 500
		if !requestSucceed {
			log.Println("Upstream server returned error:", resp.Status)
		}
		if failureIsFatal {
			return fmt.Errorf("HTTP error: %s", resp.Status)
		}
		if !requestSucceed {
			c.sleepUntilRetry()
			continue
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Println("HTTP read error:", err)
			c.sleepUntilRetry()
			continue
		}

		bodyJson := gjson.ParseBytes(body)
		if bodyJson.Get("ok").Type != gjson.True {
			errorCode := bodyJson.Get("error_code").String()
			errorDesc := bodyJson.Get("description").String()
			log.Println("Upstream error:", errorCode, errorDesc)
			c.sleepUntilRetry()
			continue
		}

		tx, err := c.db.BeginTx()
		if err != nil {
			log.Println("Failed to store updates:", err)
			c.sleepUntilRetry()
			continue
		}
		bodyJson.Get("result").ForEach(func(_, update gjson.Result) bool {
			upstreamID := update.Get("update_id").Uint()
			offset = max(offset, upstreamID+1)
			update.ForEach(func(updateType, updateValue gjson.Result) bool {
				if updateType.Str == "update_id" {
					return true
				}
				if _, ok := c.typesNeedCaching[updateType.Str]; ok {
					err = tx.InsertMessage(&updateValue)
					if err != nil {
						return false
					}
				}
				err = tx.InsertUpdate(upstreamID, updateType.String(), updateValue.Raw)
				return err == nil
			})
			return err == nil
		})
		if err != nil {
			tx.Commit()
			c.db.NotifyUpdates()
			log.Println("Failed to store updates:", err)
			c.sleepUntilRetry()
			continue
		}
		err = tx.Commit()
		c.db.NotifyUpdates()
		if err != nil {
			log.Println("Failed to store updates:", err)
			c.sleepUntilRetry()
			continue
		}

		c.resetRetry()
	}
}

func (c *Client) ForwardAPI(ctx context.Context, w http.ResponseWriter, r *http.Request, method string) error {
	var requestURL string
	if len(r.URL.RawQuery) == 0 {
		requestURL = fmt.Sprintf("%s/%s", c.conf.Upstream.ApiPrefix, method)
	} else {
		requestURL = fmt.Sprintf("%s/%s?%s", c.conf.Upstream.ApiPrefix, method, r.URL.RawQuery)
	}
	log.Println(r.Method, requestURL)

	chatID, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)

	req, err := http.NewRequestWithContext(ctx, r.Method, requestURL, r.Body)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %v", err)
	}
	for k, v := range r.Header {
		if k != "Accept-Encoding" && k != "Content-Encoding" && k != "Connection" && k != "Host" && k != "Proxy-Connection" && k != "User-Agent" {
			req.Header[k] = v
		}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 Telegram-bot-muxer/1.0 (+https://github.com/m13253/telegram-bot-muxer)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upstream HTTP request error: %v", err)
	}
	defer resp.Body.Close()

	respHeader := w.Header()
	for k, v := range resp.Header {
		if k != "Accept-Encoding" && k != "Content-Encoding" && k != "Connection" && k != "Proxy-Connection" {
			respHeader[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Too late to report error, so ignore errors from here

	echoProcessor := c.echoProcessor[method]
	if echoProcessor == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, err = io.Copy(w, resp.Body)
		if err != nil {
			debug.PrintStack()
			log.Println("HTTP error:", err)
		}
		return nil
	}

	var bodyCopy bytes.Buffer
	_, err = io.Copy(w, io.TeeReader(resp.Body, &bodyCopy))
	if err != nil {
		debug.PrintStack()
		log.Println("HTTP error:", err)
		return nil
	}

	echoProcessor(chatID, bodyCopy.Bytes())
	return nil
}

func (c *Client) ForwardFile(ctx context.Context, w http.ResponseWriter, r *http.Request, method string) error {
	var requestURL string
	if len(r.URL.RawQuery) == 0 {
		requestURL = fmt.Sprintf("%s/%s", c.conf.Upstream.FilePrefix, method)
	} else {
		requestURL = fmt.Sprintf("%s/%s?%s", c.conf.Upstream.FilePrefix, method, r.URL.RawQuery)
	}
	log.Println(r.Method, requestURL)

	req, err := http.NewRequestWithContext(ctx, r.Method, requestURL, r.Body)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %v", err)
	}
	for k, v := range r.Header {
		if k != "Accept-Encoding" && k != "Content-Encoding" && k != "Connection" && k != "Host" && k != "Proxy-Connection" && k != "User-Agent" {
			req.Header[k] = v
		}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 Telegram-bot-muxer/1.0 (+https://github.com/m13253/telegram-bot-muxer)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upstream HTTP request error: %v", err)
	}
	defer resp.Body.Close()

	respHeader := w.Header()
	for k, v := range resp.Header {
		if k != "Accept-Encoding" && k != "Content-Encoding" && k != "Connection" && k != "Proxy-Connection" {
			respHeader[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Too late to report error, so ignore errors from here

	_, err = io.Copy(w, resp.Body)
	if err != nil {
		debug.PrintStack()
		log.Println("HTTP error:", err)
	}
	return nil
}

func (c *Client) sleepUntilRetry() {
	time.Sleep(c.last_retry_interval)
	c.last_retry_interval = min(c.last_retry_interval*2, time.Duration(c.conf.Upstream.MaxRetryInterval)*time.Second)
}

func (c *Client) resetRetry() {
	c.last_retry_interval = time.Second
}

func (c *Client) processEchoMessage(_ int64, body []byte) {
	bodyJson := gjson.ParseBytes(body)
	if bodyJson.Get("ok").Type != gjson.True {
		errorCode := bodyJson.Get("error_code").String()
		errorDesc := bodyJson.Get("description").String()
		log.Println("Upstream error:", errorCode, errorDesc)
		return
	}

	message := bodyJson.Get("result")
	tx, err := c.db.BeginTx()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.InsertMessage(&message)
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.InsertLocalUpdate("message", message.Raw)
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.Commit()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	c.db.NotifyUpdates()
}

func (c *Client) processEchoMessageEdit(_ int64, body []byte) {
	bodyJson := gjson.ParseBytes(body)
	if bodyJson.Get("ok").Type != gjson.True {
		errorCode := bodyJson.Get("error_code").String()
		errorDesc := bodyJson.Get("description").String()
		log.Println("Upstream error:", errorCode, errorDesc)
		return
	}

	message := bodyJson.Get("result")
	if message.Type == gjson.True {
		return
	}
	tx, err := c.db.BeginTx()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.InsertMessage(&message)
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.InsertLocalUpdate("edited_message", message.Raw)
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.Commit()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	c.db.NotifyUpdates()
}

func (c *Client) processEchoMessageArray(_ int64, body []byte) {
	bodyJson := gjson.ParseBytes(body)
	if bodyJson.Get("ok").Type != gjson.True {
		errorCode := bodyJson.Get("error_code").String()
		errorDesc := bodyJson.Get("description").String()
		log.Println("Upstream error:", errorCode, errorDesc)
		return
	}

	tx, err := c.db.BeginTx()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	bodyJson.Get("result").ForEach(func(_, message gjson.Result) bool {
		err := tx.InsertMessage(&message)
		if err != nil {
			log.Println("Failed to store updates:", err)
		}
		err = tx.InsertLocalUpdate("message", message.Raw)
		if err != nil {
			log.Println("Failed to store updates:", err)
		}
		return true
	})
	err = tx.Commit()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	c.db.NotifyUpdates()
}
