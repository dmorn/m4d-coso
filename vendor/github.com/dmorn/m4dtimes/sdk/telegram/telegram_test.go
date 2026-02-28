package telegram

import (
	"context"
	"encoding/json"
	"io"
	"github.com/dmorn/m4dtimes/sdk/agent"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = t.target.Host
	return t.base.RoundTrip(clone)
}

func newTestClient(ts *httptest.Server) *Client {
	u, _ := url.Parse(ts.URL)
	hc := ts.Client()
	c := New("test-token")
	c.httpClient = &http.Client{Transport: rewriteTransport{target: u, base: hc.Transport}}
	return c
}

func TestPoll_TextMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["timeout"].(float64) != 30 {
			t.Fatalf("expected timeout=30, got %v", body["timeout"])
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":1001,"message":{"message_id":22,"from":{"id":123,"first_name":"V"},"chat":{"id":456,"type":"private"},"text":"hello","date":1700}}]}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	updates, err := c.Poll(context.Background(), 10, 30)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}

	expected := []agent.Update{{UpdateID: 1001, UserID: 123, ChatID: 456, Text: "hello"}}
	if !reflect.DeepEqual(updates, expected) {
		t.Fatalf("updates mismatch\n got: %#v\nwant: %#v", updates, expected)
	}
}

func TestPoll_CallbackQuery(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":2002,"callback_query":{"id":"abc","from":{"id":777,"first_name":"U"},"message":{"message_id":9,"chat":{"id":888,"type":"private"},"date":1700},"data":"btn:yes"}}]}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	updates, err := c.Poll(context.Background(), 0, 5)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	expected := []agent.Update{{UpdateID: 2002, UserID: 777, ChatID: 888, Text: "btn:yes"}}
	if !reflect.DeepEqual(updates, expected) {
		t.Fatalf("updates mismatch\n got: %#v\nwant: %#v", updates, expected)
	}
}

func TestPoll_SkipsEmptyText(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":1,"first_name":"A"},"chat":{"id":10,"type":"private"},"date":1700}},{"update_id":2,"callback_query":{"id":"z","from":{"id":2,"first_name":"B"},"message":{"message_id":2,"chat":{"id":11,"type":"private"},"date":1700}}}]}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	updates, err := c.Poll(context.Background(), 0, 5)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("expected no updates, got %#v", updates)
	}
}

func TestSend(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		defer r.Body.Close()
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		if body["chat_id"].(float64) != 42 {
			t.Fatalf("chat_id mismatch: %v", body["chat_id"])
		}
		if body["text"] != "hi" {
			t.Fatalf("text mismatch: %v", body["text"])
		}
		if body["parse_mode"] != "HTML" {
			t.Fatalf("parse_mode mismatch: %v", body["parse_mode"])
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	if err := c.Send(context.Background(), 42, "hi"); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestSendWithButtons(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["parse_mode"] != "HTML" {
			t.Fatalf("parse_mode mismatch: %v", body["parse_mode"])
		}
		rm := body["reply_markup"].(map[string]any)
		ik := rm["inline_keyboard"].([]any)
		if len(ik) != 1 {
			t.Fatalf("expected one row, got %d", len(ik))
		}
		row := ik[0].([]any)
		if len(row) != 2 {
			t.Fatalf("expected two buttons, got %d", len(row))
		}
		btn := row[0].(map[string]any)
		if btn["text"] != "Yes" || btn["callback_data"] != "yes" {
			t.Fatalf("unexpected first button: %#v", btn)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":2}}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	err := c.SendWithButtons(context.Background(), 42, "choose", []Button{{Text: "Yes", CallbackData: "yes"}, {Text: "No", CallbackData: "no"}})
	if err != nil {
		t.Fatalf("send with buttons: %v", err)
	}
}

func TestAnswerCallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["callback_query_id"] != "cbq-1" {
			t.Fatalf("callback id mismatch: %v", body["callback_query_id"])
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	if err := c.AnswerCallback(context.Background(), "cbq-1", "ack"); err != nil {
		t.Fatalf("answer callback: %v", err)
	}
}

func TestApiError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request"}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	err := c.Send(context.Background(), 1, "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Bad Request") {
		t.Fatalf("unexpected error: %v", err)
	}
}
