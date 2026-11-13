// miniapp-probe is a Phase 0 validation CLI for the new Telegram Bot API
// methods sendChatJoinRequestWebApp and answerChatJoinRequestQuery.
//
// Per Codex's review: validate the new join-request-query API path BEFORE
// committing to the Mini App architecture. If query_id is not delivered
// on chat_join_request updates for our bot/chat configuration, or if the
// 10-second window proves unworkable, the whole design changes.
//
// Run modes:
//
//	miniapp-probe poll
//	    Long-polls getUpdates with allowed_updates=["chat_join_request"]
//	    and prints every incoming join request, including query_id if
//	    present. Have a test account request to join your test group
//	    while this is running.
//
//	miniapp-probe send-webapp -query-id <id> -url <https-url>
//	    Calls sendChatJoinRequestWebApp(query_id, web_app_url). Use a
//	    query_id captured by `poll`. The 10-second window starts when
//	    Telegram delivered the update, so run this within seconds.
//
//	miniapp-probe answer -query-id <id> -result approve|decline|queue
//	    Calls answerChatJoinRequestQuery(query_id, result).
//
// Bot token is read from $TELEGRAM_BOT_TOKEN.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const apiBase = "https://api.telegram.org/bot"

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN env var is required")
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "poll":
		runPoll(token)
	case "send-webapp":
		runSendWebApp(token, os.Args[2:])
	case "answer":
		runAnswer(token, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: miniapp-probe poll | send-webapp -query-id ID -url URL | answer -query-id ID -result approve|decline|queue")
}

// runPoll long-polls getUpdates and prints every chat_join_request it sees,
// echoing the full raw JSON so we can confirm whether query_id is present
// for the bot/chat configuration we're targeting.
func runPoll(token string) {
	offset := 0
	httpc := &http.Client{Timeout: 90 * time.Second}
	log.Println("polling getUpdates with allowed_updates=[chat_join_request]; have someone request to join your test group…")
	for {
		params := url.Values{}
		params.Set("offset", fmt.Sprintf("%d", offset))
		params.Set("timeout", "60")
		params.Set("allowed_updates", `["chat_join_request"]`)
		endpoint := apiBase + token + "/getUpdates?" + params.Encode()

		resp, err := httpc.Get(endpoint)
		if err != nil {
			log.Printf("getUpdates error: %v; retrying in 2s", err)
			time.Sleep(2 * time.Second)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("read body error: %v", err)
			continue
		}

		var env struct {
			OK     bool              `json:"ok"`
			Desc   string            `json:"description"`
			Result []json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			log.Printf("unmarshal error: %v; body=%s", err, string(body))
			continue
		}
		if !env.OK {
			log.Printf("api not-ok: %s; body=%s", env.Desc, string(body))
			time.Sleep(2 * time.Second)
			continue
		}

		for _, raw := range env.Result {
			var head struct {
				UpdateID        int             `json:"update_id"`
				ChatJoinRequest json.RawMessage `json:"chat_join_request"`
			}
			if err := json.Unmarshal(raw, &head); err != nil {
				log.Printf("update parse error: %v", err)
				continue
			}
			if head.UpdateID >= offset {
				offset = head.UpdateID + 1
			}
			if len(head.ChatJoinRequest) == 0 {
				continue
			}
			// Pretty-print the raw join-request JSON so we can eyeball whether
			// query_id is present and what shape the rest of the payload has.
			var pretty json.RawMessage
			_ = json.Unmarshal(head.ChatJoinRequest, &pretty)
			fmt.Printf("update_id=%d chat_join_request=%s\n", head.UpdateID, string(head.ChatJoinRequest))

			// Extract query_id (if present) for quick reuse.
			var cjr struct {
				QueryID string `json:"query_id"`
				Chat    struct {
					ID    int64  `json:"id"`
					Title string `json:"title"`
				} `json:"chat"`
				From struct {
					ID       int64  `json:"id"`
					Username string `json:"username"`
				} `json:"from"`
			}
			_ = json.Unmarshal(head.ChatJoinRequest, &cjr)
			if cjr.QueryID == "" {
				fmt.Printf("  → query_id ABSENT — chat may not be in query mode, or bot needs join-request admin rights\n")
			} else {
				fmt.Printf("  → query_id=%s chat=%d (%q) user=%d (@%s) — 10s window starts NOW\n",
					cjr.QueryID, cjr.Chat.ID, cjr.Chat.Title, cjr.From.ID, cjr.From.Username)
			}
		}
	}
}

func runSendWebApp(token string, args []string) {
	fs := flag.NewFlagSet("send-webapp", flag.ExitOnError)
	queryID := fs.String("query-id", "", "chat_join_request_query_id from a captured update")
	webAppURL := fs.String("url", "", "https URL of the Mini App")
	_ = fs.Parse(args)
	if *queryID == "" || *webAppURL == "" {
		fmt.Fprintln(os.Stderr, "send-webapp requires -query-id and -url")
		os.Exit(2)
	}
	form := url.Values{}
	form.Set("chat_join_request_query_id", *queryID)
	form.Set("web_app_url", *webAppURL)
	post(token, "sendChatJoinRequestWebApp", form)
}

func runAnswer(token string, args []string) {
	fs := flag.NewFlagSet("answer", flag.ExitOnError)
	queryID := fs.String("query-id", "", "chat_join_request_query_id")
	result := fs.String("result", "approve", "approve | decline | queue")
	_ = fs.Parse(args)
	if *queryID == "" || (*result != "approve" && *result != "decline" && *result != "queue") {
		fmt.Fprintln(os.Stderr, "answer requires -query-id and -result=approve|decline|queue")
		os.Exit(2)
	}
	form := url.Values{}
	form.Set("chat_join_request_query_id", *queryID)
	form.Set("result", *result)
	post(token, "answerChatJoinRequestQuery", form)
}

// post sends a form-encoded request to a Bot API method and prints the
// response verbatim. Designed for human inspection during validation.
func post(token, method string, form url.Values) {
	endpoint := apiBase + token + "/" + method
	resp, err := http.Post(endpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		log.Fatalf("POST %s: %v", method, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("HTTP %d\n", resp.StatusCode)
	fmt.Println(string(body))
	if resp.StatusCode != 200 {
		os.Exit(1)
	}
}
