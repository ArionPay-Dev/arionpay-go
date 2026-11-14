package arionpay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultToleranceSec: webhooks signed more than this many seconds ago (or ahead) are refused.
// ArionPay signs again on every retry.
const DefaultToleranceSec = 300

// EventTypes are the webhook event types ArionPay sends.
var EventTypes = []string{"invoice.pending", "invoice.partial", "invoice.paid", "invoice.expired", "invoice.needs_review"}

var (
	hex64  = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	digits = regexp.MustCompile(`^[0-9]{1,12}$`)
)

// Event is a verified ArionPay webhook. Data.Invoice has the same field names as a signed read (GetInvoice).
type Event struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Created  int64  `json:"created"`
	Livemode bool   `json:"livemode"`
	Test     bool   `json:"test"`
	Data     struct {
		Invoice Invoice `json:"invoice"`
	} `json:"data"`
	Raw []byte `json:"-"` // the verified body
}

func hmacHex(secret string, data []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

// SignRequest returns the x-signature of one API request:
// POST: hex HMAC-SHA256(secret, "<ts>.<raw body>"), GET: hex HMAC-SHA256(secret, "<ts>.GET <path and query>").
func SignRequest(secretKey string, timestamp int64, method, pathAndQuery string, rawBody []byte) string {
	if strings.EqualFold(method, "GET") {
		return hmacHex(secretKey, []byte(strconv.FormatInt(timestamp, 10)+".GET "+pathAndQuery))
	}
	return hmacHex(secretKey, append([]byte(strconv.FormatInt(timestamp, 10)+"."), rawBody...))
}

// ParseSignatureHeader reads "t=<unix seconds>,v1=<hex>". ok is false when it is malformed.
func ParseSignatureHeader(header string) (t int64, v1 string, ok bool) {
	values := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		if k, v, found := strings.Cut(part, "="); found {
			values[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if !digits.MatchString(values["t"]) || !hex64.MatchString(values["v1"]) {
		return 0, "", false
	}
	t, _ = strconv.ParseInt(values["t"], 10, 64)
	return t, strings.ToLower(values["v1"]), true
}

// IsValidSignature is true when header is a valid, fresh signature of exactly rawBody. It never panics.
func IsValidSignature(rawBody []byte, header, webhookSecret string, toleranceSec int64, now time.Time) bool {
	if webhookSecret == "" {
		return false
	}
	t, v1, ok := ParseSignatureHeader(header)
	if !ok {
		return false
	}
	if d := now.Unix() - t; d > toleranceSec || d < -toleranceSec {
		return false
	}
	expected := hmacHex(webhookSecret, append([]byte(strconv.FormatInt(t, 10)+"."), rawBody...))
	return hmac.Equal([]byte(expected), []byte(v1))
}

// VerifyWebhook checks a webhook and returns the event, or a *SignatureError when it must not be trusted.
//
// rawBody must be the exact bytes ArionPay sent (io.ReadAll(r.Body)), not a decoded and re-encoded value.
func VerifyWebhook(rawBody []byte, header, webhookSecret string) (*Event, error) {
	return verifyWebhookAt(rawBody, header, webhookSecret, DefaultToleranceSec, time.Now())
}

func verifyWebhookAt(rawBody []byte, header, webhookSecret string, toleranceSec int64, now time.Time) (*Event, error) {
	if webhookSecret == "" {
		return nil, &SignatureError{"The webhook secret is empty. Copy it from the Webhooks page of your ArionPay dashboard (it starts with whsec_)."}
	}
	if _, _, ok := ParseSignatureHeader(header); !ok {
		return nil, &SignatureError{"Missing or malformed ArionPay-Signature header."}
	}
	if !IsValidSignature(rawBody, header, webhookSecret, toleranceSec, now) {
		return nil, &SignatureError{"Invalid webhook signature or stale timestamp. Check the webhook secret and your server clock."}
	}
	var probe struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Invoice json.RawMessage `json:"invoice"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rawBody, &probe); err != nil {
		return nil, &SignatureError{"The webhook body is not JSON."}
	}
	inv := strings.TrimSpace(string(probe.Data.Invoice))
	if probe.ID == "" || probe.Type == "" || !strings.HasPrefix(inv, "{") {
		return nil, &SignatureError{"The webhook body is not an ArionPay event."}
	}
	var ev Event
	if err := json.Unmarshal(rawBody, &ev); err != nil {
		return nil, &SignatureError{"The webhook body is not an ArionPay event."}
	}
	ev.Raw = rawBody
	return &ev, nil
}

// IsTestEvent is true for events made by "Send test webhook": a made-up invoice that must never change an order.
func IsTestEvent(ev *Event) bool {
	return ev.Test || strings.HasPrefix(ev.ID, "evt_test_") || strings.HasPrefix(ev.Data.Invoice.ID, "inv_test_")
}
