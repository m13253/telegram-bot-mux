package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

type Client struct {
	conf                *Config
	db                  *Database
	updateTypeIsMessage map[string]struct{}
	echoUpdateType      map[string]string
	nextRetryInterval   time.Duration
	globalCooldown      *CooldownQueue
	chatCooldown        map[int64]*CooldownQueue
	chatCooldownMtx     sync.Mutex
}

func NewClient(conf *Config, db *Database) *Client {
	c := &Client{
		conf: conf,
		db:   db,
		updateTypeIsMessage: map[string]struct{}{
			"message":                 {},
			"edited_message":          {},
			"channel_post":            {},
			"edited_channel_post":     {},
			"business_message":        {},
			"edited_business_message": {},
		},
		nextRetryInterval: time.Second,
		globalCooldown:    NewCooldownQueue(),
		chatCooldown:      make(map[int64]*CooldownQueue),
		chatCooldownMtx:   sync.Mutex{},
	}
	c.echoUpdateType = map[string]string{
		"sendMessage":             "message",
		"forwardMessage":          "message",
		"sendPhoto":               "message",
		"sendAudio":               "message",
		"sendDocument":            "message",
		"sendVideo":               "message",
		"sendAnimation":           "message",
		"sendVoice":               "message",
		"sendVideoNote":           "message",
		"sendPaidMedia":           "message",
		"sendMediaGroup":          "message",
		"sendLocation":            "message",
		"sendVenue":               "message",
		"sendContact":             "message",
		"sendPoll":                "message",
		"sendDice":                "message",
		"editMessageText":         "edited_message",
		"editMessageCaption":      "edited_message",
		"editMessageMedia":        "edited_message",
		"editMessageLiveLocation": "edited_message",
		"stopMessageLiveLocation": "edited_message",
		"editMessageReplyMarkup":  "edited_message",
	}
	return c
}

func (c *Client) StartPolling(ctx context.Context) error {
	for {
		requestURL := c.conf.Upstream.ApiPrefix + "/deleteWebhook"
		requestBody := bytes.NewBufferString("drop_pending_updates=false")
		log.Printf("[ HTTP POST ] %s %s\n", requestURL, requestBody.String())

		req, err := http.NewRequestWithContext(ctx, "POST", requestURL, requestBody)
		if err != nil {
			debug.PrintStack()
			return fmt.Errorf("failed to send HTTP request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", httpUserAgent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// Assume this is not a fatal error
			log.Println("Upstream HTTP request error:", err)
			c.sleepUntilRetry()
			continue
		}
		resp.Body.Close()

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
		break
	}

	offset := uint64(0)
	for {
		requestURL := c.conf.Upstream.ApiPrefix + "/getUpdates"
		var requestBody bytes.Buffer
		if offset != 0 {
			fmt.Fprintf(&requestBody, "offset=%d&", offset)
		}
		fmt.Fprintf(&requestBody,
			"timeout=%d&allowed_updates=%s",
			c.conf.Upstream.PollingTimeout, c.conf.Upstream.FilterUpdateTypesStr,
		)
		log.Printf("[ HTTP POST ] %s %s\n", requestURL, requestBody.String())

		req, err := http.NewRequestWithContext(ctx, "POST", requestURL, &requestBody)
		if err != nil {
			debug.PrintStack()
			return fmt.Errorf("failed to send HTTP request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", httpUserAgent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// Assume this is not a fatal error
			log.Println("Upstream HTTP request error:", err)
			c.sleepUntilRetry()
			continue
		}

		requestSucceed := resp.StatusCode >= 200 && resp.StatusCode < 300
		failureIsFatal := resp.StatusCode >= 400 && resp.StatusCode < 500
		if !requestSucceed {
			log.Println("Upstream server returned error:", resp.Status)
		}
		if failureIsFatal {
			resp.Body.Close()
			return fmt.Errorf("HTTP error: %s", resp.Status)
		}
		if !requestSucceed {
			resp.Body.Close()
			c.sleepUntilRetry()
			continue
		}

		// Let's trust the server won't send us something ridiculously big
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			debug.PrintStack()
			log.Println("HTTP read error:", err)
			c.sleepUntilRetry()
			continue
		}

		bodyJson := gjson.ParseBytes(body)
		if bodyJson.Get("ok").Type != gjson.True {
			errorCode := bodyJson.Get("error_code").String()
			errorDesc := bodyJson.Get("description").String()
			debug.PrintStack()
			log.Println("Upstream error:", errorCode, errorDesc)
			c.sleepUntilRetry()
			continue
		}

		tx, err := c.db.BeginTx()
		if err != nil {
			debug.PrintStack()
			log.Println("Failed to store updates:", err)
			c.sleepUntilRetry()
			continue
		}
		bodyJson.Get("result").ForEach(func(_, update gjson.Result) bool {
			upstreamID := update.Get("update_id").Uint()
			offset = max(offset, upstreamID+1)
			update.ForEach(func(updateType, updateValue gjson.Result) bool {
				if updateType.Str == "update_id" {
					// Skip
					return true
				}
				err = tx.InsertUpdate(upstreamID, updateType.String(), updateValue.Raw)
				if err != nil {
					return false
				}
				if _, ok := c.updateTypeIsMessage[updateType.Str]; ok {
					err = tx.InsertMessage(&updateValue)
					if err != nil {
						return false
					}
				}
				return true
			})
			return err == nil
		})
		if err != nil {
			tx.Commit()
			debug.PrintStack()
			log.Println("Failed to store updates:", err)
			c.sleepUntilRetry()
			continue
		}
		err = tx.Commit()
		if err != nil {
			debug.PrintStack()
			log.Println("Failed to store updates:", err)
			c.sleepUntilRetry()
			continue
		}

		c.resetRetry()
	}
}

func (c *Client) ForwardRequest(ctx context.Context, s *Server, w http.ResponseWriter, r *http.Request, isFileRequest bool, urlSuffix string, bodyCopy io.ReadCloser) error {
	var urlPrefix string
	if isFileRequest {
		urlPrefix = c.conf.Upstream.FilePrefix
	} else {
		urlPrefix = c.conf.Upstream.ApiPrefix
	}
	var requestURL string
	if len(r.URL.RawQuery) == 0 {
		requestURL = fmt.Sprintf("%s/%s", urlPrefix, urlSuffix)
	} else {
		requestURL = fmt.Sprintf("%s/%s?%s", urlPrefix, urlSuffix, r.URL.RawQuery)
	}
	log.Printf("[HTTP %s] %s\n", r.Method, requestURL)

	if !isFileRequest {
		params := struct {
			ChatID int64 `json:"chat_id"`
		}{}
		_ = r.ParseForm()
		params.ChatID, _ = strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)

		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct == "application/json" {
			_ = json.NewDecoder(io.LimitReader(r.Body, httpBodyLimit)).Decode(&params)
		}

		err := c.waitForCooldown(ctx, params.ChatID)
		if err != nil {
			debug.PrintStack()
			return err
		}
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, requestURL, bodyCopy)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %v", err)
	}
	for k, v := range r.Header {
		if k != "Accept-Encoding" && k != "Content-Encoding" && k != "Connection" && k != "Host" && k != "Proxy-Connection" && k != "User-Agent" {
			req.Header[k] = v
		}
	}
	req.Header.Set("User-Agent", httpUserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upstream HTTP request error: %v", err)
	}

	respHeader := w.Header()
	for k, v := range resp.Header {
		if k != "Accept-Encoding" && k != "Content-Encoding" && k != "Connection" && k != "Proxy-Connection" {
			respHeader[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Too late to report error, so ignore errors from here

	var echoUpdateType string
	if !isFileRequest {
		echoUpdateType = c.echoUpdateType[urlSuffix]
	}
	if echoUpdateType == "" || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, err = io.Copy(w, resp.Body)
		resp.Body.Close()
		if err != nil {
			debug.PrintStack()
			log.Println("HTTP error:", err)
		}
		return nil
	}

	var respBodyCopy bytes.Buffer
	_, err = io.Copy(w, io.TeeReader(resp.Body, &respBodyCopy))
	resp.Body.Close()
	if err != nil {
		debug.PrintStack()
		log.Println("HTTP error:", err)
		return nil
	}
	c.processEchoMessage(echoUpdateType, respBodyCopy.Bytes())
	return nil
}

func (c *Client) sleepUntilRetry() {
	time.Sleep(c.nextRetryInterval)
	c.nextRetryInterval = min(c.nextRetryInterval*2, time.Duration(c.conf.Upstream.MaxRetryInterval)*time.Second)
}

func (c *Client) resetRetry() {
	c.nextRetryInterval = time.Second
}

func (c *Client) processEchoMessage(updateType string, body []byte) {
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
	result := bodyJson.Get("result")
	cb := func(_, message gjson.Result) bool {
		err := tx.InsertEchoUpdate(updateType, message.Raw)
		if err != nil {
			debug.PrintStack()
			log.Println("Failed to store updates:", err)
		}
		err = tx.InsertMessage(&message)
		if err != nil {
			debug.PrintStack()
			log.Println("Failed to store updates:", err)
		}
		return true
	}
	if result.IsArray() {
		result.ForEach(cb)
	} else {
		cb(gjson.Result{}, result)
	}
	err = tx.Commit()
	if err != nil {
		debug.PrintStack()
		log.Println("Failed to store updates:", err)
	}
}

func (c *Client) waitForCooldown(ctx context.Context, chatID int64) error {
	// https://core.telegram.org/bots/faq#my-bot-is-hitting-limits-how-do-i-avoid-this
	// Global cooldown is 1/30 sec
	// Private chat cooldown is 1 sec
	// Non-private chat cooldown is 3 sec

	const (
		global     = time.Second/30 + 1
		private    = time.Second
		nonPrivate = 3 * time.Second
	)

	if chatID == 0 {
		return nil
	}

	chatType, err := c.db.GetChatType(ctx, chatID)
	if err != nil {
		return fmt.Errorf("failed to retrieve chat information: %v", err)
	}

	var cd time.Duration
	if chatType == "private" {
		cd = private
	} else {
		cd = nonPrivate
	}

	c.chatCooldownMtx.Lock()
	queue, ok := c.chatCooldown[chatID]
	if !ok {
		queue = NewCooldownQueue()
		c.chatCooldown[chatID] = queue
	}
	notify, cancel := queue.Push(cd)
	c.chatCooldownMtx.Unlock()
	select {
	case <-notify:
	case <-ctx.Done():
		cancel()
		return nil
	}

	notify, cancel = c.globalCooldown.Push(global)
	select {
	case <-notify:
	case <-ctx.Done():
		cancel()
	}

	return nil
}
