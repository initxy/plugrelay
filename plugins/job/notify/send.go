package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Backoff is the wait before each retry of a failed send.
var Backoff = []time.Duration{time.Second, 3 * time.Second}

const sendTimeout = 15 * time.Second

// permanent marks a failure a retry cannot fix (a refused request, a bot
// answering with an error code).
type permanent struct{ error }

func (n *Notifier) send(ctx context.Context, ch Channel, m Message) Delivery {
	d := Delivery{ChannelID: ch.ID, ChannelName: ch.Name, ChannelType: ch.Type}
	var err error
	d.Attempts, err = retry(ctx, n.sleep, func(ctx context.Context) error { return n.sendOnce(ctx, ch, m) })
	d.At, d.OK = n.now(), err == nil
	if err != nil {
		d.Error = err.Error()
		n.log.Warn("notification not delivered", "channel", ch.Name, "type", ch.Type, "err", err)
	}
	return d
}

// retry runs fn until it works, fails permanently or Backoff runs out.
func retry(ctx context.Context, sleep func(context.Context, time.Duration) error, fn func(context.Context) error) (int, error) {
	for i := 0; ; i++ {
		actx, cancel := context.WithTimeout(ctx, sendTimeout)
		err := fn(actx)
		cancel()
		var p permanent
		if err == nil || errors.As(err, &p) || i >= len(Backoff) {
			return i + 1, err
		}
		if serr := sleep(ctx, Backoff[i]); serr != nil {
			return i + 1, err
		}
	}
}

func (n *Notifier) sendOnce(ctx context.Context, ch Channel, m Message) error {
	switch ch.Type {
	case TypeEmail:
		return SendMail(ctx, n.b.Dial, ch, m.Title, m.Text())
	case TypeFeishu:
		return sendFeishu(ctx, n.h.HTTPClient(), ch, m, n.now())
	case TypeDingTalk:
		return sendDingTalk(ctx, n.h.HTTPClient(), ch, m, n.now())
	case TypeWeCom:
		body := map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": m.Markdown()}}
		return botPost(ctx, n.h.HTTPClient(), ch.URL, body, "errcode", "errmsg")
	case TypeWebhook:
		body, err := json.Marshal(map[string]any{"type": "notification", "title": m.Title, "lines": m.Lines,
			"link": m.Link, "text": m.Text(), "at": n.now().UTC().Format(time.RFC3339)})
		if err != nil {
			return err
		}
		return PostWebhook(ctx, n.h.HTTPClient(), ch, "notification", body, n.now())
	}
	return permanent{fmt.Errorf("unknown channel type %q", ch.Type)}
}

// post sends a JSON body and returns the response body of a 2xx answer.
func post(ctx context.Context, c *http.Client, target string, body []byte, header http.Header) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, permanent{err}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		return out, nil
	}
	err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(out[:min(len(out), 200)])))
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, err
	}
	return nil, permanent{err}
}

// botPost posts to a chat bot that answers 200 with an error code field.
func botPost(ctx context.Context, c *http.Client, target string, body any, codeField, msgField string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	out, err := post(ctx, c, target, raw, nil)
	if err != nil {
		return err
	}
	var ans map[string]any
	if json.Unmarshal(out, &ans) != nil {
		return nil
	}
	if code, ok := ans[codeField].(float64); ok && code != 0 {
		return permanent{fmt.Errorf("bot refused (%v): %v", code, ans[msgField])}
	}
	return nil
}

func hmacB64(key, msg string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// sendFeishu posts a text message to a Feishu or Lark custom bot. With a
// signing secret the body carries timestamp and sign: base64 of an
// HMAC-SHA256 keyed with "timestamp\nsecret" over nothing.
func sendFeishu(ctx context.Context, c *http.Client, ch Channel, m Message, now time.Time) error {
	body := map[string]any{"msg_type": "text", "content": map[string]string{"text": m.Text()}}
	if ch.Secret != "" {
		ts := strconv.FormatInt(now.Unix(), 10)
		body["timestamp"], body["sign"] = ts, hmacB64(ts+"\n"+ch.Secret, "")
	}
	return botPost(ctx, c, ch.URL, body, "code", "msg")
}

// sendDingTalk posts a Markdown message to a DingTalk custom bot. With a
// signing secret the URL carries timestamp (ms) and sign: base64 of an
// HMAC-SHA256 keyed with the secret over "timestamp\nsecret".
func sendDingTalk(ctx context.Context, c *http.Client, ch Channel, m Message, now time.Time) error {
	target := ch.URL
	if ch.Secret != "" {
		ts := strconv.FormatInt(now.UnixMilli(), 10)
		u, err := url.Parse(target)
		if err != nil {
			return permanent{err}
		}
		q := u.Query()
		q.Set("timestamp", ts)
		q.Set("sign", hmacB64(ch.Secret, ts+"\n"+ch.Secret))
		u.RawQuery = q.Encode()
		target = u.String()
	}
	body := map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": m.Title, "text": m.Markdown()}}
	return botPost(ctx, c, target, body, "errcode", "errmsg")
}

// Signature is the X-Plugrelay-Signature value of a webhook body: hex
// HMAC-SHA256 under the channel's secret over "<timestamp>.<body>".
func Signature(secret, timestamp string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(timestamp + "."))
	h.Write(body)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

// PostWebhook posts one body to a generic webhook, signed when the channel
// has a secret. kind is "notification" or "records".
func PostWebhook(ctx context.Context, c *http.Client, ch Channel, kind string, body []byte, now time.Time) error {
	ts := strconv.FormatInt(now.Unix(), 10)
	h := http.Header{"X-Plugrelay-Event": {kind}, "X-Plugrelay-Timestamp": {ts}}
	if ch.Secret != "" {
		h.Set("X-Plugrelay-Signature", Signature(ch.Secret, ts, body))
	}
	_, err := post(ctx, c, ch.URL, body, h)
	return err
}

// Retry is retry for other plugins of the feature (the record webhook).
func Retry(ctx context.Context, fn func(context.Context) error) (int, error) {
	return retry(ctx, sleepCtx, fn)
}

// SendMail sends a plain-text mail over SMTP through dial. TLS "tls"
// starts TLS at once (port 465), "starttls" upgrades (port 587), "none"
// stays in the clear (a password is then refused unless the server is
// local, as net/smtp does).
func SendMail(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), ch Channel, subject, text string) error {
	if dial == nil {
		return permanent{errors.New("no dialer")}
	}
	port := ch.SMTPPort
	if port == 0 {
		port = map[string]int{"tls": 465, "none": 25}[ch.TLS]
		if port == 0 {
			port = 587
		}
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(ch.SMTPHost, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cfg := &tls.Config{ServerName: ch.SMTPHost, MinVersion: tls.VersionTLS12}
	if ch.TLS == "tls" {
		conn = tls.Client(conn, cfg)
	}
	c, err := smtp.NewClient(conn, ch.SMTPHost)
	if err != nil {
		return err
	}
	defer c.Close()
	if ch.TLS == "" || ch.TLS == "starttls" {
		if err := c.StartTLS(cfg); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if ch.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", ch.Username, ch.Password, ch.SMTPHost)); err != nil {
			return permanent{fmt.Errorf("sign in: %w", err)}
		}
	}
	if err := c.Mail(addrOf(ch.From)); err != nil {
		return permanent{err}
	}
	for _, to := range ch.To {
		if err := c.Rcpt(addrOf(to)); err != nil {
			return permanent{err}
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(mailBody(ch, subject, text)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// addrOf is the address part of "Name <addr>".
func addrOf(s string) string {
	if i, j := strings.LastIndex(s, "<"), strings.LastIndex(s, ">"); i >= 0 && j > i {
		return s[i+1 : j]
	}
	return strings.TrimSpace(s)
}

func mailBody(ch Channel, subject, text string) []byte {
	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", ch.From)
	header("To", strings.Join(ch.To, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("MIME-Version", "1.0")
	header("Content-Type", `text/plain; charset="utf-8"`)
	header("Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.Bytes()
}
