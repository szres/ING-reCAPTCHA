package bot

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// update_poller_test.go exercises the bot-owned getUpdates replacement.
//
// The reasons we own this loop instead of going through go-telegram-bot-api/v5:
//   - v5.5.1's ChatJoinRequest lacks both query_id (Guard Bots) and
//     user_chat_id (Bot API 6.5 proactive-DM window). v5 silently drops
//     unknown fields on unmarshal, so going through it strips the only
//     fields the Mini App flow needs.
//   - We need synchronous dispatch of chat_join_request BEFORE advancing
//     the local offset, so a crash during handler execution leaves the
//     update redeliverable on restart.
//
// These tests verify the JSON shapes the loop accepts AND its dispatch
// contract: chat_join_request → handler synchronously, every other
// update → channel.

// TestRawChatJoinRequest_UnmarshalAllFields locks in JSON-parsing for the
// fields v5 doesn't model. Regressions here silently break the Mini App
// flow — the bot would happily run while every join request quietly
// missed query_id and/or user_chat_id.
func TestRawChatJoinRequest_UnmarshalAllFields(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantQueryID    string
		wantUserChatID int64
		wantBio        string
	}{
		{
			name: "modern guard mode (query_id + user_chat_id)",
			body: `{
				"chat":{"id":-1003354414656,"type":"supergroup","title":"test"},
				"from":{"id":42,"is_bot":false,"first_name":"u"},
				"user_chat_id":99,
				"date":1700000000,
				"query_id":"qid-modern"
			}`,
			wantQueryID:    "qid-modern",
			wantUserChatID: 99,
		},
		{
			name: "DM-fallback (user_chat_id only, no query_id)",
			body: `{
				"chat":{"id":-1001,"type":"supergroup","title":"x"},
				"from":{"id":42,"is_bot":false,"first_name":"u"},
				"user_chat_id":99,
				"date":1700000000
			}`,
			wantQueryID:    "",
			wantUserChatID: 99,
		},
		{
			name: "legacy (neither field present)",
			body: `{
				"chat":{"id":-1001,"type":"supergroup","title":"x"},
				"from":{"id":42,"is_bot":false,"first_name":"u"},
				"date":1700000000
			}`,
			wantQueryID:    "",
			wantUserChatID: 0,
		},
		{
			name: "with bio (optional field)",
			body: `{
				"chat":{"id":-1001,"type":"supergroup","title":"x"},
				"from":{"id":42,"is_bot":false,"first_name":"u"},
				"user_chat_id":99,
				"date":1700000000,
				"bio":"hello world",
				"query_id":"qid"
			}`,
			wantQueryID:    "qid",
			wantUserChatID: 99,
			wantBio:        "hello world",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got rawChatJoinRequest
			require.NoError(t, json.Unmarshal([]byte(tc.body), &got))
			assert.Equal(t, tc.wantQueryID, got.QueryID)
			assert.Equal(t, tc.wantUserChatID, got.UserChatID)
			assert.Equal(t, tc.wantBio, got.Bio)
			assert.Equal(t, int64(42), got.From.ID)
		})
	}
}

// TestRawUpdate_UnmarshalDispatchableFields covers the outer envelope.
// Each non-nil pointer field must round-trip from real-shape JSON; nil
// fields when absent prevent the dispatcher from misclassifying updates.
func TestRawUpdate_UnmarshalDispatchableFields(t *testing.T) {
	body := `{
		"update_id":42,
		"chat_join_request":{
			"chat":{"id":-1,"type":"supergroup"},
			"from":{"id":7,"is_bot":false,"first_name":"x"},
			"user_chat_id":9,
			"date":1,
			"query_id":"q"
		}
	}`
	var u rawUpdate
	require.NoError(t, json.Unmarshal([]byte(body), &u))
	assert.Equal(t, 42, u.UpdateID)
	require.NotNil(t, u.ChatJoinRequest)
	assert.Equal(t, "q", u.ChatJoinRequest.QueryID)
	assert.Equal(t, int64(9), u.ChatJoinRequest.UserChatID)
	assert.Nil(t, u.Message, "absent fields must unmarshal to nil so dispatcher doesn't double-classify")
	assert.Nil(t, u.CallbackQuery)
	assert.Nil(t, u.ChatMember)
}

// installFakeAPI hijacks the rawPoller's HTTP client so it routes
// getUpdates calls to a local handler. Returns a cleanup func.
//
// We can't override apiBaseURL (it's a const), so we intercept at the
// transport layer with a roundTripper that recognises the bot-token path
// and serves canned JSON envelopes.
func installFakeAPI(p *rawPoller, handler func(req *http.Request) (status int, body string)) {
	p.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			status, body := handler(req)
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}),
	}
}

// TestRawPoller_DispatchesJoinRequestBeforeAdvancingOffset verifies the
// load-bearing invariant: a chat_join_request handler runs synchronously
// AND finishes before the next getUpdates call goes out. Otherwise a
// crash mid-handler would acknowledge the update away.
func TestRawPoller_DispatchesJoinRequestBeforeAdvancingOffset(t *testing.T) {
	var (
		mu         sync.Mutex
		dispatched []*rawChatJoinRequest
		// fetchedOffsets is the sequence of `offset` query params the
		// poller has sent us. Used to assert ordering between dispatch
		// and offset advance.
		fetchedOffsets []string
		// dispatchedBeforeSecondCall confirms the handler ran before
		// the next getUpdates query came in with offset advanced.
		handlerRanBeforeOffsetAdvance int32
	)

	envelope := `{
		"ok":true,
		"result":[{
			"update_id":1001,
			"chat_join_request":{
				"chat":{"id":-100,"type":"supergroup","title":"t"},
				"from":{"id":42,"is_bot":false,"first_name":"u"},
				"user_chat_id":42,
				"date":1700000000,
				"query_id":"the-qid"
			}
		}]
	}`

	p := newRawPoller("TOKEN", []string{"chat_join_request"}, func(cjr *rawChatJoinRequest) {
		mu.Lock()
		dispatched = append(dispatched, cjr)
		// Mark that the handler ran while the poller has not yet
		// issued the next getUpdates call. We check at the moment we
		// see the second offset hit the HTTP layer.
		atomic.StoreInt32(&handlerRanBeforeOffsetAdvance, 1)
		mu.Unlock()
	})
	t.Cleanup(p.Stop)

	installFakeAPI(p, func(req *http.Request) (int, string) {
		q, _ := url.ParseQuery(req.URL.RawQuery)
		off := q.Get("offset")
		mu.Lock()
		fetchedOffsets = append(fetchedOffsets, off)
		isSecondCall := len(fetchedOffsets) >= 2
		mu.Unlock()
		if isSecondCall {
			// By the time the SECOND getUpdates call arrives, the
			// handler must already have run (synchronous dispatch).
			require.Equal(t, int32(1), atomic.LoadInt32(&handlerRanBeforeOffsetAdvance),
				"handler must run before offset advances")
			// Return an empty result so the loop keeps spinning until
			// the test ends.
			return 200, `{"ok":true,"result":[]}`
		}
		return 200, envelope
	})
	// Drop the long-poll wait so the loop runs fast in tests.
	p.pollTimeout = 0

	ch := p.Updates()
	// chat_join_request does NOT go through the channel; everything
	// else does. So we don't expect anything from ch — just wait for
	// the handler to be called.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(dispatched)
		offs := append([]string(nil), fetchedOffsets...)
		mu.Unlock()
		if n >= 1 && len(offs) >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for dispatch; dispatched=%d offsets=%v", n, offs)
		case <-time.After(20 * time.Millisecond):
		}
		// Drain any incidental sends so we don't deadlock the producer.
		select {
		case <-ch:
		default:
		}
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, dispatched, 1)
	assert.Equal(t, "the-qid", dispatched[0].QueryID)
	assert.Equal(t, int64(42), dispatched[0].UserChatID)
	require.GreaterOrEqual(t, len(fetchedOffsets), 2)
	assert.Equal(t, "0", fetchedOffsets[0], "first call uses offset=0")
	assert.Equal(t, "1002", fetchedOffsets[1], "second call advances offset past the dispatched update_id")
}

// TestRawPoller_StreamsNonJoinUpdatesThroughChannel verifies that
// message/callback_query updates go through the channel (so the existing
// v5-typed handlers process them), while chat_join_request goes only
// through the dispatchJoinRequest callback.
func TestRawPoller_StreamsNonJoinUpdatesThroughChannel(t *testing.T) {
	var dispatchedJoins int32
	p := newRawPoller("T", []string{"message", "chat_join_request"}, func(cjr *rawChatJoinRequest) {
		atomic.AddInt32(&dispatchedJoins, 1)
	})
	t.Cleanup(p.Stop)
	p.pollTimeout = 0

	envelope := `{
		"ok":true,
		"result":[
			{"update_id":1,"message":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"},"text":"hi"}},
			{"update_id":2,"chat_join_request":{
				"chat":{"id":-1,"type":"supergroup"},"from":{"id":7,"is_bot":false,"first_name":"u"},
				"user_chat_id":7,"date":1,"query_id":"q"}}
		]
	}`
	var calls int32
	installFakeAPI(p, func(req *http.Request) (int, string) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return 200, envelope
		}
		return 200, `{"ok":true,"result":[]}`
	})

	ch := p.Updates()

	// Drain the channel until we see the message update (chat_join_request
	// goes to the callback, not the channel).
	var got rawUpdate
	deadline := time.After(2 * time.Second)
	select {
	case got = <-ch:
	case <-deadline:
		t.Fatal("timed out waiting for message update on channel")
	}

	require.NotNil(t, got.Message, "message updates must reach the channel")
	assert.Equal(t, "hi", got.Message.Text)
	assert.Nil(t, got.ChatJoinRequest, "chat_join_request must NOT reach the channel")

	// Wait a moment for the join handler to also fire.
	for i := 0; i < 50; i++ {
		if atomic.LoadInt32(&dispatchedJoins) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&dispatchedJoins), "chat_join_request must reach the callback")
}

// TestRawPoller_BacksOffOnAPIError verifies that an ok:false envelope
// triggers a backoff (we don't spin at 100% CPU when Telegram is
// returning errors) and that Stop unblocks the backoff immediately.
func TestRawPoller_BacksOffOnAPIError(t *testing.T) {
	p := newRawPoller("T", []string{"message"}, nil)
	p.pollTimeout = 0

	var calls int32
	installFakeAPI(p, func(req *http.Request) (int, string) {
		atomic.AddInt32(&calls, 1)
		return 200, `{"ok":false,"error_code":401,"description":"unauthorized"}`
	})

	ch := p.Updates()

	// Give it enough time to make at least one call.
	time.Sleep(50 * time.Millisecond)
	require.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(1))

	// Stop must return promptly even while the loop is in its backoff sleep.
	doneStop := make(chan struct{})
	go func() {
		p.Stop()
		close(doneStop)
	}()
	select {
	case <-doneStop:
	case <-time.After(1 * time.Second):
		t.Fatal("Stop did not return promptly; backoff is not cancelable")
	}

	// And the Updates() channel closes once the goroutine exits.
	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel must be closed after Stop")
	case <-time.After(1 * time.Second):
		t.Fatal("Updates channel did not close after Stop")
	}
}

// TestRawPoller_StopWhileLongPolling exercises the OTHER cancellation
// path: when the HTTP request itself is in flight, Stop must abort it
// via context cancellation (the loop's http.NewRequestWithContext binds
// p.ctx as the request context). Without that, Stop would block up to
// 90s on the next getUpdates response.
func TestRawPoller_StopWhileLongPolling(t *testing.T) {
	p := newRawPoller("T", []string{"message"}, nil)
	p.pollTimeout = 0

	requestArrived := make(chan struct{}, 1)
	releaseRequest := make(chan struct{})
	installFakeAPI(p, func(req *http.Request) (int, string) {
		select {
		case requestArrived <- struct{}{}:
		default:
		}
		// Block until the context is cancelled so we simulate a real
		// long-poll. The httpClient propagates ctx.Done into the
		// roundTripper via Request.Context.
		<-req.Context().Done()
		return 0, ""
	})
	// Replace the transport with one that blocks honoring context cancel:
	p.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			select {
			case requestArrived <- struct{}{}:
			default:
			}
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-releaseRequest:
				return &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":[]}`)),
					Header:     make(http.Header),
				}, nil
			}
		}),
	}

	ch := p.Updates()

	select {
	case <-requestArrived:
	case <-time.After(1 * time.Second):
		t.Fatal("poller did not initiate the first getUpdates call")
	}

	doneStop := make(chan struct{})
	go func() {
		p.Stop()
		close(doneStop)
	}()
	select {
	case <-doneStop:
	case <-time.After(1 * time.Second):
		t.Fatal("Stop did not abort the in-flight long-poll request")
	}

	// Drain — channel should close.
	select {
	case _, ok := <-ch:
		assert.False(t, ok)
	case <-time.After(1 * time.Second):
		t.Fatal("channel did not close after Stop")
	}

	// Avoid leaking the blocked roundTripper if any retry slipped through.
	close(releaseRequest)
}

// TestRawPoller_AdvancesOffsetForNonJoinUpdates verifies the offset
// advances past every update the channel receives. The contract
// differs slightly from the chat_join_request branch (which advances
// AFTER dispatch); for channel-bound updates the offset advances
// BEFORE the send so a slow consumer cannot deadlock the poller.
func TestRawPoller_AdvancesOffsetForNonJoinUpdates(t *testing.T) {
	p := newRawPoller("T", []string{"message"}, nil)
	p.pollTimeout = 0

	var (
		mu   sync.Mutex
		offs []string
	)
	installFakeAPI(p, func(req *http.Request) (int, string) {
		q, _ := url.ParseQuery(req.URL.RawQuery)
		mu.Lock()
		offs = append(offs, q.Get("offset"))
		n := len(offs)
		mu.Unlock()
		if n == 1 {
			return 200, `{"ok":true,"result":[
				{"update_id":5,"message":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"},"text":"a"}},
				{"update_id":7,"message":{"message_id":2,"date":1,"chat":{"id":1,"type":"private"},"text":"b"}}
			]}`
		}
		return 200, `{"ok":true,"result":[]}`
	})

	ch := p.Updates()
	t.Cleanup(p.Stop)

	gotA := <-ch
	gotB := <-ch
	assert.Equal(t, "a", gotA.Message.Text)
	assert.Equal(t, "b", gotB.Message.Text)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		o := append([]string(nil), offs...)
		mu.Unlock()
		if len(o) >= 2 {
			assert.Equal(t, "0", o[0])
			assert.Equal(t, "8", o[1], "offset must advance past max update_id + 1")
			return
		}
		select {
		case <-deadline:
			t.Fatalf("offsets did not advance: %v", offs)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestRawPoller_RecoversFromMalformedBody confirms a non-JSON response
// is logged and retried instead of crashing the goroutine.
func TestRawPoller_RecoversFromMalformedBody(t *testing.T) {
	p := newRawPoller("T", []string{"message"}, nil)
	p.pollTimeout = 0

	var calls int32
	installFakeAPI(p, func(req *http.Request) (int, string) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return 200, "<<not-json>>"
		}
		return 200, `{"ok":true,"result":[
			{"update_id":1,"message":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"},"text":"ok"}}
		]}`
	})

	ch := p.Updates()
	t.Cleanup(p.Stop)

	select {
	case got := <-ch:
		assert.Equal(t, "ok", got.Message.Text, "poller must continue after a parse error")
	case <-time.After(5 * time.Second):
		t.Fatalf("never recovered from malformed body; calls=%d", atomic.LoadInt32(&calls))
	}
}

