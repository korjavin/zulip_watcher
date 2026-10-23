package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const botEmail = "watcher-bot@example.com"

// fakeZulip mimics the subset of the Zulip API the watcher uses.
type fakeZulip struct {
	t        *testing.T
	topic    string   // expected topic operand in narrow ("" = none)
	events   []string // scripted /events bodies; "BAD" = expired queue, "BLOCK" = hang until client gives up
	mu       sync.Mutex
	regs     int
	lastIDs  []string
	deleted  []string
	eventIdx int
}

func (f *fakeZulip) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := f.t
	if u, p, ok := r.BasicAuth(); !ok || u != botEmail || p != "secret" {
		t.Errorf("bad basic auth %q %q", u, p)
	}
	f.mu.Lock()
	switch {
	// https://zulip.com/api/register-queue — POST form: event_types, narrow,
	// apply_markdown, all_public_streams; returns queue_id, last_event_id.
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/register":
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("register content-type %q", ct)
		}
		r.ParseForm()
		want := `[["stream","ops"]]`
		if f.topic != "" {
			want = `[["stream","ops"],["topic","` + f.topic + `"]]`
		}
		for k, v := range map[string]string{"event_types": `["message"]`, "narrow": want, "apply_markdown": "false", "all_public_streams": "true"} {
			if got := r.PostForm.Get(k); got != v {
				t.Errorf("register %s = %q, want %q", k, got, v)
			}
		}
		f.regs++
		qid := "q" + strconv.Itoa(f.regs)
		f.mu.Unlock()
		w.Write([]byte(`{"result":"success","msg":"","queue_id":"` + qid + `","last_event_id":-1}`))
	// https://zulip.com/api/get-events — GET queue_id, last_event_id;
	// returns events[] with id, type ("heartbeat", "message" + message{...}).
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/events":
		f.lastIDs = append(f.lastIDs, r.URL.Query().Get("queue_id")+":"+r.URL.Query().Get("last_event_id"))
		body := "BLOCK"
		if f.eventIdx < len(f.events) {
			body = f.events[f.eventIdx]
			f.eventIdx++
		}
		f.mu.Unlock()
		switch body {
		case "BLOCK":
			<-r.Context().Done()
		case "BAD": // expired queue, per get-events error docs
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"result":"error","code":"BAD_EVENT_QUEUE_ID","msg":"Bad event queue ID: q1","queue_id":"q1"}`))
		default:
			w.Write([]byte(body))
		}
	// https://zulip.com/api/get-stream-id — GET stream; channel "ops" has id 7.
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/get_stream_id":
		f.mu.Unlock()
		if r.URL.Query().Get("stream") != "ops" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"result":"error","code":"BAD_REQUEST","msg":"Invalid channel name"}`))
			return
		}
		w.Write([]byte(`{"result":"success","msg":"","stream_id":7}`))
	// https://zulip.com/api/get-stream-by-id
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/streams/"):
		f.mu.Unlock()
		if r.URL.Path != "/api/v1/streams/7" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"result":"error","code":"BAD_REQUEST","msg":"Invalid channel ID"}`))
			return
		}
		w.Write([]byte(`{"result":"success","msg":"","stream":{"stream_id":7,"name":"ops"}}`))
	// https://zulip.com/api/get-stream-topics
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users/me/7/topics":
		f.mu.Unlock()
		w.Write([]byte(`{"result":"success","msg":"","topics":[{"name":"Deploy","max_id":100}]}`))
	// https://zulip.com/api/delete-queue — DELETE queue_id.
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/events":
		b, _ := io.ReadAll(r.Body) // net/http's ParseForm ignores DELETE bodies
		form, _ := url.ParseQuery(string(b))
		f.deleted = append(f.deleted, form.Get("queue_id"))
		f.mu.Unlock()
		w.Write([]byte(`{"result":"success","msg":""}`))
	default:
		f.mu.Unlock()
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

const (
	heartbeat = `{"result":"success","events":[{"id":0,"type":"heartbeat"}]}`
	fromBot   = `{"result":"success","events":[{"id":1,"type":"message","message":{"id":100,"sender_email":"Watcher-Bot@example.com","sender_full_name":"Bot","type":"stream","display_recipient":"ops","subject":"deploy","timestamp":1700000000,"content":"mine"}}]}`
	fromHuman = `{"result":"success","events":[{"id":2,"type":"message","message":{"id":101,"sender_email":"alice@example.com","sender_full_name":"Alice","type":"stream","display_recipient":"ops","subject":"deploy","timestamp":1700000001,"content":"ship it\n<b>now</b>"}}]}`
)

func start(t *testing.T, f *fakeZulip) func(string) string {
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	env := map[string]string{"ZULIP_URL": srv.URL + "/", "ZULIP_EMAIL": botEmail, "ZULIP_API_KEY": "secret"}
	return func(k string) string { return env[k] }
}

func TestHappyText(t *testing.T) {
	f := &fakeZulip{topic: "deploy", events: []string{heartbeat, fromBot, fromHuman}}
	var out bytes.Buffer
	if code := run([]string{"-stream", "ops", "-topic", "deploy"}, start(t, f), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "alice@example.com | ops > deploy | id=101\nship it\n<b>now</b>\n"
	if out.String() != want {
		t.Errorf("stdout %q, want %q", out.String(), want)
	}
	if got := strings.Join(f.lastIDs, ","); got != "q1:-1,q1:0,q1:1" {
		t.Errorf("last_event_id sequence %s", got)
	}
	if strings.Join(f.deleted, ",") != "q1" {
		t.Errorf("deleted %v", f.deleted)
	}
}

func TestHappyJSON(t *testing.T) {
	f := &fakeZulip{events: []string{fromBot, fromHuman}}
	var out bytes.Buffer
	if code := run([]string{"-stream", "ops", "-json"}, start(t, f), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Errorf("want one line, got %q", out.String())
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": 101.0, "sender_email": "alice@example.com", "sender_full_name": "Alice",
		"stream": "ops", "topic": "deploy", "timestamp": 1700000001.0, "content": "ship it\n<b>now</b>"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
}

func TestChannelByID(t *testing.T) {
	f := &fakeZulip{events: []string{fromHuman}}
	if code := run([]string{"-stream", "7"}, start(t, f), &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code) // register asserts narrow uses the name "ops"
	}
}

func TestUnknownChannel(t *testing.T) {
	for _, s := range []string{"nope", "25"} {
		f := &fakeZulip{}
		if code := run([]string{"-stream", s}, start(t, f), &bytes.Buffer{}); code != 1 {
			t.Errorf("%s: exit %d, want 1", s, code)
		}
		if f.regs != 0 {
			t.Errorf("%s: registered %d queues, want 0", s, f.regs)
		}
	}
}

func TestNewTopic(t *testing.T) { // unknown topic only warns
	f := &fakeZulip{topic: "fresh", events: []string{fromHuman}}
	if code := run([]string{"-stream", "ops", "-topic", "fresh"}, start(t, f), &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestQueueExpiry(t *testing.T) {
	f := &fakeZulip{events: []string{"BAD", fromHuman}}
	var out bytes.Buffer
	if code := run([]string{"-stream", "ops"}, start(t, f), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if f.regs != 2 {
		t.Errorf("registers %d, want 2", f.regs)
	}
	if got := strings.Join(f.lastIDs, ","); got != "q1:-1,q2:-1" {
		t.Errorf("polls %s", got)
	}
}

func TestTimeout(t *testing.T) {
	f := &fakeZulip{events: []string{heartbeat}} // then blocks
	var out bytes.Buffer
	if code := run([]string{"-stream", "ops", "-timeout", "50ms"}, start(t, f), &out); code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout %q", out.String())
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"result":"error","code":"UNAUTHORIZED","msg":"Invalid API key"}`))
	}))
	defer srv.Close()
	env := map[string]string{"ZULIP_URL": srv.URL, "ZULIP_EMAIL": botEmail, "ZULIP_API_KEY": "x"}
	if code := run([]string{"-stream", "ops"}, func(k string) string { return env[k] }, &bytes.Buffer{}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestUsage(t *testing.T) {
	full := map[string]string{"ZULIP_URL": "http://x", "ZULIP_EMAIL": "a", "ZULIP_API_KEY": "k"}
	for _, tc := range []struct {
		name string
		args []string
		drop string
	}{
		{"no stream", nil, ""},
		{"bad flag", []string{"-stream", "ops", "-nope"}, ""},
		{"no url", []string{"-stream", "ops"}, "ZULIP_URL"},
		{"no email", []string{"-stream", "ops"}, "ZULIP_EMAIL"},
		{"no key", []string{"-stream", "ops"}, "ZULIP_API_KEY"},
	} {
		getenv := func(k string) string {
			if k == tc.drop {
				return ""
			}
			return full[k]
		}
		if code := run(tc.args, getenv, &bytes.Buffer{}); code != 2 {
			t.Errorf("%s: exit %d, want 2", tc.name, code)
		}
	}
}
