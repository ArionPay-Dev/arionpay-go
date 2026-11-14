// Package arionpay is the Go client for ArionPay: create crypto invoices, read them, and verify webhooks.
// Payments go straight to the merchant's own wallet.
//
// Use it on your server only: it holds your API secret.
//
//	client, err := arionpay.New(arionpay.Config{APIKey: os.Getenv("ARIONPAY_API_KEY"), APISecret: os.Getenv("ARIONPAY_API_SECRET")})
//	inv, err := client.CreateInvoice(ctx, arionpay.InvoiceParams{Amount: "49.90", Currency: "USD", OrderID: "1001"})
//	// send the customer to inv.InvoiceURL; deliver when the invoice.paid webhook arrives
package arionpay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Version of this library.
const Version = "1.0.0"

// DefaultBaseURL is ArionPay's merchant API.
const DefaultBaseURL = "https://merchant.arionpay.com"

// Scenarios for SimulatePayment (test invoices only).
var Scenarios = []string{"detected", "confirm", "exact", "underpaid_1pct", "underpaid_5pct", "overpaid", "partial_half", "late"}

var (
	pkPattern = regexp.MustCompile(`^pk_[A-Za-z0-9_-]{8,64}$`)
	idPattern = regexp.MustCompile(`^[0-9a-fA-F]{24}$`)
)

// Config of a Client. APIKey and APISecret are required.
type Config struct {
	APIKey        string       // pk_... (ArionPay dashboard > your store)
	APISecret     string       // sk_...
	WebhookSecret string       // whsec_... (only needed for Client.VerifyWebhook)
	BaseURL       string       // default DefaultBaseURL
	HTTPClient    *http.Client // default: 20 s timeout, no redirects
	MaxAttempts   int          // tries for safe requests on network errors, 502/503/504 and short 429 (default 3, max 5)
	AppInfo       string       // added to the User-Agent, e.g. "myshop/2.1"
}

// Client talks to the ArionPay merchant API. It is safe for concurrent use.
type Client struct {
	cfg       Config
	origin    string
	basePath  string
	http      *http.Client
	userAgent string
	sleep     func(ctx context.Context, d time.Duration) error // waits between retries; stops when ctx ends
	now       func() time.Time
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// New checks the configuration and returns a Client.
func New(cfg Config) (*Client, error) {
	if !pkPattern.MatchString(cfg.APIKey) {
		return nil, errors.New("arionpay: APIKey must be your store API key (it starts with pk_)")
	}
	if cfg.APISecret == "" {
		return nil, errors.New("arionpay: APISecret is required (your store API secret, it starts with sk_)")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("arionpay: BaseURL must be an http(s) address")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.MaxAttempts > 5 {
		cfg.MaxAttempts = 5
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	ua := fmt.Sprintf("arionpay-go/%s %s", Version, runtime.Version())
	if cfg.AppInfo != "" {
		ua += " " + cfg.AppInfo
	}
	return &Client{
		cfg:       cfg,
		origin:    u.Scheme + "://" + u.Host,
		basePath:  strings.TrimRight(u.Path, "/"),
		http:      hc,
		userAgent: ua,
		sleep:     sleepCtx,
		now:       time.Now,
	}, nil
}

// InvoiceParams for CreateInvoice. Amount is required ("49.90"); empty fields are not sent.
type InvoiceParams struct {
	Amount         string
	Currency       string // default USD
	OrderID        string
	Description    string
	CustomerEmail  string
	SuccessURL     string // where the customer goes after paying (this does NOT prove payment)
	CancelURL      string
	CallbackURL    string // where ArionPay sends this invoice's webhooks (default: your store's webhook address)
	Asset          string // pre-select a coin, e.g. "USDT_TRC20"
	AllowedAssets  []string
	ExpiresMin     int
	Metadata       map[string]any // your data, sent back in every webhook of this invoice
	IdempotencyKey string         // the same key always returns the same invoice (a random one when empty)
	Test           *bool          // true for a test invoice (no real money)
}

// Bool returns a pointer to b, for InvoiceParams.Test.
func Bool(b bool) *bool { return &b }

// CreateInvoice creates an invoice. Send the customer to InvoiceURL. Safe to retry: every call carries an
// idempotency key, so a repeat after a network error returns the same invoice.
func (c *Client) CreateInvoice(ctx context.Context, p InvoiceParams) (*CreatedInvoice, error) {
	if strings.TrimSpace(p.Amount) == "" {
		return nil, errors.New("arionpay: Amount is required")
	}
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.IdempotencyKey == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		p.IdempotencyKey = "sdk_" + hex.EncodeToString(b)
	}
	body := map[string]any{"amount": p.Amount, "currency": p.Currency, "idempotencyKey": p.IdempotencyKey}
	for k, v := range map[string]string{"orderId": p.OrderID, "description": p.Description, "customerEmail": p.CustomerEmail, "successUrl": p.SuccessURL, "cancelUrl": p.CancelURL, "callbackUrl": p.CallbackURL, "asset": p.Asset} {
		if v != "" {
			body[k] = v
		}
	}
	if len(p.AllowedAssets) > 0 {
		body["allowedAssets"] = p.AllowedAssets
	}
	if p.ExpiresMin > 0 {
		body["expiresMin"] = p.ExpiresMin
	}
	if p.Metadata != nil {
		body["metadata"] = p.Metadata
	}
	if p.Test != nil {
		body["test"] = *p.Test
	}
	r, err := c.request(ctx, "POST", "/api/v1/invoices", body, true, true)
	if err != nil {
		return nil, err
	}
	d, ok := r["data"].(map[string]any)
	if !ok {
		d = r
	}
	status := str(d["status"])
	if status == "" {
		status = "new"
	}
	currency := str(d["currency"])
	if currency == "" {
		currency = p.Currency
	}
	orderID := str(d["order_id"])
	if orderID == "" {
		orderID = p.OrderID
	}
	return &CreatedInvoice{
		ID:           str(d["id"]),
		InvoiceURL:   str(d["invoice_url"]),
		Status:       status,
		AmountFiat:   Amount(str(d["amount_fiat"])),
		Currency:     currency,
		Asset:        str(d["asset"]),
		Address:      str(d["address"]),
		AmountCrypto: Amount(str(d["amount_crypto"])),
		ExpiresAt:    str(d["expires_at"]),
		OrderID:      orderID,
		Raw:          r,
	}, nil
}

// GetInvoice reads an invoice, signed, straight from ArionPay (webhook field names). Use it to double-check a
// payment, or to poll instead of webhooks.
func (c *Client) GetInvoice(ctx context.Context, id string) (*Invoice, error) {
	if !idPattern.MatchString(id) {
		return nil, errors.New("arionpay: invoice id must be the 24-character id ArionPay returned")
	}
	r, err := c.request(ctx, "GET", "/api/v1/invoices/"+id, nil, true, true)
	if err != nil {
		return nil, err
	}
	return normalizeRead(r), nil
}

// SimulatePayment moves a TEST invoice through a payment scenario (see Scenarios); webhooks then fire as for a real
// payment. Create the test invoice with Asset set (e.g. "USDT_TRC20") so a coin is chosen.
func (c *Client) SimulatePayment(ctx context.Context, id, scenario string) (map[string]any, error) {
	if !idPattern.MatchString(id) {
		return nil, errors.New("arionpay: invoice id must be the 24-character id ArionPay returned")
	}
	ok := false
	for _, s := range Scenarios {
		ok = ok || s == scenario
	}
	if !ok {
		return nil, fmt.Errorf("arionpay: scenario must be one of %s", strings.Join(Scenarios, ", "))
	}
	return c.request(ctx, "POST", "/api/v1/invoices/"+id+"/simulate", map[string]any{"scenario": scenario}, false, true)
}

// SendTestWebhook asks ArionPay to send a signed sample event to your store's webhook address. It never describes a
// real order.
func (c *Client) SendTestWebhook(ctx context.Context, eventType string) (map[string]any, error) {
	if eventType == "" {
		eventType = "invoice.paid"
	}
	return c.request(ctx, "POST", "/api/v1/webhooks/test", map[string]any{"eventType": eventType}, false, true)
}

// Currencies lists the coins you can use as Asset / AllowedAssets, with USD rates.
func (c *Client) Currencies(ctx context.Context) ([]map[string]any, error) {
	r, err := c.request(ctx, "GET", "/api/v1/currencies", nil, true, false)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	if list, ok := r["currencies"].([]any); ok {
		for _, x := range list {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// Rates returns current coin prices in one fiat currency.
func (c *Client) Rates(ctx context.Context, currency string) (map[string]any, error) {
	if currency == "" {
		currency = "USD"
	}
	return c.request(ctx, "GET", "/api/v1/rates?currency="+url.QueryEscape(currency), nil, true, false)
}

// VerifyWebhook checks a webhook with Config.WebhookSecret. See the package function VerifyWebhook.
func (c *Client) VerifyWebhook(rawBody []byte, signatureHeader string) (*Event, error) {
	return verifyWebhookAt(rawBody, signatureHeader, c.cfg.WebhookSecret, DefaultToleranceSec, c.now())
}

// ---------------------------------------------------------------------------------------------------- transport

func (c *Client) request(ctx context.Context, method, path string, body map[string]any, retrySafe, signed bool) (map[string]any, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	replayRetried := false
	for attempt := 1; ; attempt++ {
		r, err := c.send(ctx, method, path, raw, signed)
		if err == nil {
			return r, nil
		}
		var ae *APIError
		if !errors.As(err, &ae) {
			return nil, err
		}
		// Two identical signed requests in the same second have the same signature; only the first is accepted.
		if ae.Status == 401 && strings.Contains(strings.ToLower(ae.Message), "already used") && !replayRetried {
			replayRetried = true
			if c.sleep(ctx, 1100*time.Millisecond) != nil {
				return nil, err
			}
			continue
		}
		retryAfter := int(num(ae.Body["retryAfterSec"]))
		temporary := ae.Status == 0 || ae.Status == 502 || ae.Status == 503 || ae.Status == 504 || (ae.Status == 429 && retryAfter <= 5)
		if !retrySafe || !temporary || attempt >= c.cfg.MaxAttempts || ctx.Err() != nil {
			return nil, err
		}
		d := 500 * time.Millisecond
		for i := 1; i < attempt; i++ {
			d *= 3
		}
		if ae.Status == 429 {
			d = time.Duration(max(1, retryAfter)) * time.Second
		}
		if c.sleep(ctx, d) != nil {
			return nil, err
		}
	}
}

func (c *Client) send(ctx context.Context, method, path string, raw []byte, signed bool) (map[string]any, error) {
	pathOnly, query, _ := strings.Cut(path, "?")
	full := c.basePath + pathOnly
	if query != "" {
		full += "?" + query
	}
	var bodyReader io.Reader
	if raw != nil {
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+full, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if signed {
		ts := c.now().Unix()
		req.Header.Set("x-api-key", c.cfg.APIKey)
		req.Header.Set("x-timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("x-signature", SignRequest(c.cfg.APISecret, ts, method, full, raw))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &APIError{Status: 0, Code: "network_error", Message: "Could not reach ArionPay: " + err.Error(), Body: map[string]any{}, cause: err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	obj := map[string]any{}
	if len(data) > 0 {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber() // keep amounts exactly as ArionPay sent them
		var v any
		if dec.Decode(&v) == nil {
			if m, ok := v.(map[string]any); ok {
				obj = m
			}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := obj["error"].(string)
		if msg == "" {
			msg, _ = obj["message"].(string)
		}
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		code, _ := obj["code"].(string)
		if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 {
			obj["retryAfterSec"] = json.Number(strconv.Itoa(ra))
		}
		return nil, &APIError{Status: resp.StatusCode, Code: code, Message: msg, Body: obj}
	}
	return obj, nil
}
