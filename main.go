// zulip_watcher blocks until one new Zulip message arrives in a channel
// (optionally a topic), prints it to stdout and exits.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout))
}

type message struct {
	ID               int64  `json:"id"`
	SenderEmail      string `json:"sender_email"`
	SenderFullName   string `json:"sender_full_name"`
	DisplayRecipient any    `json:"display_recipient"` // string for stream messages, list for private
	Subject          string `json:"subject"`
	Timestamp        int64  `json:"timestamp"`
	Content          string `json:"content"`
}

type apiResp struct {
	Result      string `json:"result"`
	Msg         string `json:"msg"`
	Code        string `json:"code"`
	QueueID     string `json:"queue_id"`
	LastEventID int64  `json:"last_event_id"`
	StreamID    int64  `json:"stream_id"`
	Stream      struct {
		Name string `json:"name"`
	} `json:"stream"`
	Topics []struct {
		Name string `json:"name"`
	} `json:"topics"`
	Events []struct {
		ID      int64    `json:"id"`
		Type    string   `json:"type"`
		Message *message `json:"message"`
	} `json:"events"`
}

type apiErr struct{ code, msg string }

func (e *apiErr) Error() string { return fmt.Sprintf("zulip: %s (%s)", e.msg, e.code) }

type client struct{ base, email, key string }

// do calls the Zulip API; GET sends form as query, other methods as a urlencoded body.
func (c *client) do(ctx context.Context, method, path string, form url.Values) (*apiResp, error) {
	u := c.base + path
	var body io.Reader
	if method == http.MethodGet {
		u += "?" + form.Encode()
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.email, c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// No client timeout: /events long-polls (heartbeat every ~45s); ctx carries -timeout.
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var r apiResp
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("%s %s: HTTP %d: %w", method, path, res.StatusCode, err)
	}
	if res.StatusCode != http.StatusOK || r.Result != "success" {
		return nil, &apiErr{code: r.Code, msg: r.Msg}
	}
	return &r, nil
}

func run(args []string, getenv func(string) string, stdout io.Writer) int {
	fs := flag.NewFlagSet("zulip_watcher", flag.ContinueOnError)
	stream := fs.String("stream", "", "channel name or ID to watch (required)")
	topic := fs.String("topic", "", "topic to watch (optional)")
	timeout := fs.Duration("timeout", 0, "give up after this long, e.g. 30m (0 = wait forever)")
	asJSON := fs.Bool("json", false, "print the message as one JSON object")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: zulip_watcher -stream NAME|ID [-topic NAME] [-timeout DUR] [-json]")
		fmt.Fprintln(fs.Output(), "env: ZULIP_URL, ZULIP_EMAIL, ZULIP_API_KEY (required)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c := &client{strings.TrimRight(getenv("ZULIP_URL"), "/"), getenv("ZULIP_EMAIL"), getenv("ZULIP_API_KEY")}
	if *stream == "" || c.base == "" || c.email == "" || c.key == "" {
		fs.Usage()
		return 2
	}

	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	fail := func(err error) int {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "zulip_watcher: timeout, no message")
			return 3
		}
		fmt.Fprintln(os.Stderr, "zulip_watcher:", err)
		return 1
	}

	// Register silently accepts unknown channels and waits forever, so check first.
	// https://zulip.com/api/get-stream-id, /api/get-stream-by-id, /api/get-stream-topics
	r, err := c.do(ctx, http.MethodGet, "/api/v1/get_stream_id", url.Values{"stream": {*stream}})
	if err != nil {
		id, convErr := strconv.ParseInt(*stream, 10, 64)
		if convErr != nil {
			return fail(fmt.Errorf("unknown channel %q: %w", *stream, err))
		}
		if r, err = c.do(ctx, http.MethodGet, "/api/v1/streams/"+*stream, nil); err != nil {
			return fail(fmt.Errorf("unknown channel %q (no channel with that name or ID): %w", *stream, err))
		}
		fmt.Fprintf(os.Stderr, "zulip_watcher: resolved channel id %d to %q\n", id, r.Stream.Name)
		*stream = r.Stream.Name
		r.StreamID = id
	}
	if *topic != "" {
		t, err := c.do(ctx, http.MethodGet, "/api/v1/users/me/"+strconv.FormatInt(r.StreamID, 10)+"/topics", nil)
		if err != nil {
			return fail(fmt.Errorf("list topics: %w", err))
		}
		found := false
		for _, x := range t.Topics {
			found = found || strings.EqualFold(x.Name, *topic)
		}
		if !found { // not fatal: the agent may start watching before posting to a new topic
			fmt.Fprintf(os.Stderr, "zulip_watcher: warning: topic %q does not exist in channel %q yet; waiting anyway\n", *topic, *stream)
		}
	}

	narrow := [][]string{{"stream", *stream}}
	if *topic != "" {
		narrow = append(narrow, []string{"topic", *topic})
	}
	narrowJSON, _ := json.Marshal(narrow)
	reg := url.Values{
		"event_types":        {`["message"]`},
		"narrow":             {string(narrowJSON)},
		"apply_markdown":     {"false"},
		"all_public_streams": {"true"},
	}

	var queueID string
	defer func() { // best-effort queue cleanup; result ignored
		if queueID != "" {
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c.do(dctx, http.MethodDelete, "/api/v1/events", url.Values{"queue_id": {queueID}})
		}
	}()

	var lastEventID int64
	register := func() error {
		r, err := c.do(ctx, http.MethodPost, "/api/v1/register", reg)
		if err != nil {
			return err
		}
		queueID, lastEventID = r.QueueID, r.LastEventID
		return nil
	}
	if err := register(); err != nil {
		return fail(err)
	}

	for {
		r, err := c.do(ctx, http.MethodGet, "/api/v1/events", url.Values{
			"queue_id":      {queueID},
			"last_event_id": {strconv.FormatInt(lastEventID, 10)},
		})
		var ae *apiErr
		if errors.As(err, &ae) && ae.code == "BAD_EVENT_QUEUE_ID" {
			// ponytail: messages sent between queue expiry and re-register are lost;
			// a fresh queue is the only correct recovery.
			queueID = ""
			if err := register(); err != nil {
				return fail(err)
			}
			continue
		}
		if err != nil {
			return fail(err)
		}
		for _, ev := range r.Events {
			lastEventID = ev.ID
			m := ev.Message
			if ev.Type != "message" || m == nil || strings.EqualFold(m.SenderEmail, c.email) {
				continue
			}
			name, ok := m.DisplayRecipient.(string)
			if !ok {
				name = *stream
			}
			var err error
			if *asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetEscapeHTML(false)
				err = enc.Encode(map[string]any{
					"id": m.ID, "sender_email": m.SenderEmail, "sender_full_name": m.SenderFullName,
					"stream": name, "topic": m.Subject, "timestamp": m.Timestamp, "content": m.Content,
				})
			} else {
				_, err = fmt.Fprintf(stdout, "%s | %s > %s | id=%d\n%s\n", m.SenderEmail, name, m.Subject, m.ID, m.Content)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "zulip_watcher: write stdout:", err)
				return 1
			}
			return 0
		}
	}
}
