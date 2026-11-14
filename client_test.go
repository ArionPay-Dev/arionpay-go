package arionpay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type call struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

// fake answers each request with the next answer. A nil answer closes the connection (a network error).
type fake struct {
	mu      sync.Mutex
	answers []answer
	calls   []call
	srv     *httptest.Server
}

type answer struct {
	status int
	body   string
	header map[string]string
	drop   bool
}

func newFake(t *testing.T, answers ...answer) *fake {
	f := &fake{answers: answers}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, call{r.Method, r.URL.RequestURI(), r.Header.Clone(), b})
		if len(f.answers) == 0 {
			f.mu.Unlock()
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		a := f.answers[0]
		f.answers = f.answers[1:]
		f.mu.Unlock()
		if a.drop {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		for k, v := range a.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) client(t *testing.T, mod func(*Config)) (*Client, *[]time.Duration) {
	cfg := Config{APIKey: "pk_unit_000001", APISecret: "sk_unit", BaseURL: f.srv.URL + "/"}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	waits := &[]time.Duration{}
	c.sleep = func(ctx context.Context, d time.Duration) error { *waits = append(*waits, d); return ctx.Err() }
	ts := int64(1700000000)
	c.now = func() time.Time { ts++; return time.Unix(ts, 0) } // a fresh timestamp per request
	return c, waits
}

var ctx = context.Background()

func TestConstructorChecks(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{APIKey: "nope", APISecret: "s"}, "pk_"},
		{Config{APIKey: "pk_abcdefgh"}, "APISecret"},
		{Config{APIKey: "pk_abcdefgh", APISecret: "s", BaseURL: "ftp://x"}, "http"},
		{Config{APIKey: "pk_abcdefgh", APISecret: "s", BaseURL: "https://"}, "http"},
	} {
		if _, err := New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: got %v, want error with %q", tc.cfg, err, tc.want)
		}
	}
	c, err := New(Config{APIKey: "pk_abcdefgh", APISecret: "s", AppInfo: "myshop/2.1"})
	if err != nil || c.origin != DefaultBaseURL || !strings.HasPrefix(c.userAgent, "arionpay-go/"+Version+" go") || !strings.HasSuffix(c.userAgent, " myshop/2.1") {
		t.Fatalf("defaults: %v %+v", err, c)
	}
}

func TestCreateSignsExactBody(t *testing.T) {
	f := newFake(t, answer{status: 201, body: `{"status":"success","id":"` + strings.Repeat("a", 24) + `","invoice_url":"https://x/pay/1","data":{"id":"` + strings.Repeat("a", 24) + `","invoice_url":"https://x/pay/1","amount_fiat":9.5,"currency":"EUR","status":"new","order_id":"7","amount_crypto":"10.123456789012345678"}}`})
	c, _ := f.client(t, nil)
	inv, err := c.CreateInvoice(ctx, InvoiceParams{Amount: "9.50", Currency: "EUR", OrderID: "7", Metadata: map[string]any{"name": "Zoë <b>"}, Test: Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvoiceURL != "https://x/pay/1" || inv.AmountFiat != "9.5" || inv.Currency != "EUR" || inv.OrderID != "7" || inv.AmountCrypto != "10.123456789012345678" {
		t.Fatalf("created: %+v", inv)
	}
	cl := f.calls[0]
	if cl.Method != "POST" || cl.Path != "/api/v1/invoices" || cl.Header.Get("x-api-key") != "pk_unit_000001" {
		t.Fatalf("request: %+v", cl)
	}
	var body map[string]any
	_ = json.Unmarshal(cl.Body, &body)
	if body["amount"] != "9.50" || body["test"] != true || body["metadata"].(map[string]any)["name"] != "Zoë <b>" || !strings.HasPrefix(body["idempotencyKey"].(string), "sdk_") {
		t.Fatalf("body: %s", cl.Body)
	}
	if _, has := body["description"]; has {
		t.Fatal("empty description sent")
	}
	if want := h("sk_unit", append([]byte(cl.Header.Get("x-timestamp")+"."), cl.Body...)); cl.Header.Get("x-signature") != want {
		t.Fatal("signature is not over the exact body")
	}
}

func TestGetSignedOverPathWithBasePath(t *testing.T) {
	id := strings.Repeat("b", 24)
	f := newFake(t, answer{status: 200, body: `{"invoiceId":"` + id + `","status":"PAID","amountFiat":1,"currency":"USD","settledCurrency":"USDT_TRC20","amountExpected":"1.000000","amountReceived":"1.000000","settledAt":"2026-11-14T00:00:00Z","test":true,"underpaidWithinTolerance":true,"metadata":{"k":"v"}}`})
	c, _ := f.client(t, func(cfg *Config) { cfg.BaseURL = f.srv.URL + "/gw/" })
	inv, err := c.GetInvoice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inv.ID != id || inv.Status != "paid" || !inv.Test || inv.Asset != "USDT_TRC20" || inv.PaidCrypto != "1.000000" || inv.ConfirmedCrypto != "0" || inv.PaidAt == "" || !inv.UnderpaidWithinTolerance || inv.Metadata["k"] != "v" || inv.Transactions == nil || !inv.Matches("1", "USD") {
		t.Fatalf("normalized: %+v", inv)
	}
	cl := f.calls[0]
	if cl.Path != "/gw/api/v1/invoices/"+id {
		t.Fatalf("path %s", cl.Path)
	}
	if want := h("sk_unit", []byte(cl.Header.Get("x-timestamp")+".GET /gw/api/v1/invoices/"+id)); cl.Header.Get("x-signature") != want {
		t.Fatal("GET signature")
	}
}

func TestReplayResignedOnce(t *testing.T) {
	id := strings.Repeat("c", 24)
	used := answer{status: 401, body: `{"error":"This signed request was already used. Sign each request with a fresh timestamp."}`}
	f := newFake(t, used, answer{status: 200, body: `{"invoiceId":"` + id + `","status":"NEW"}`})
	c, waits := f.client(t, nil)
	inv, err := c.GetInvoice(ctx, id)
	if err != nil || inv.Status != "new" || len(f.calls) != 2 || f.calls[0].Header.Get("x-timestamp") == f.calls[1].Header.Get("x-timestamp") || len(*waits) != 1 {
		t.Fatalf("%v %+v calls=%d", err, inv, len(f.calls))
	}
	// only once
	f2 := newFake(t, used, used)
	c2, _ := f2.client(t, nil)
	var ae *APIError
	if _, err := c2.GetInvoice(ctx, id); !errors.As(err, &ae) || ae.Status != 401 || len(f2.calls) != 2 {
		t.Fatalf("second replay: %v", err)
	}
}

func TestCreateRetryKeepsIdempotencyKey(t *testing.T) {
	f := newFake(t, answer{drop: true}, answer{status: 503, body: `{"error":"busy"}`}, answer{status: 201, body: `{"id":"` + strings.Repeat("d", 24) + `","invoice_url":"u","data":{}}`})
	c, waits := f.client(t, nil)
	inv, err := c.CreateInvoice(ctx, InvoiceParams{Amount: "1"})
	if err != nil || inv.Status != "new" || inv.Currency != "USD" {
		t.Fatalf("%v %+v", err, inv)
	}
	if len(f.calls) != 3 || string(f.calls[0].Body) != string(f.calls[2].Body) {
		t.Fatalf("calls=%d", len(f.calls))
	}
	if len(*waits) != 2 || (*waits)[0] != 500*time.Millisecond || (*waits)[1] != 1500*time.Millisecond {
		t.Fatalf("backoff %v", *waits)
	}
}

func TestRateLimit(t *testing.T) {
	id := strings.Repeat("e", 24)
	f := newFake(t, answer{status: 429, body: `{"error":"slow down","code":"rate_limited"}`, header: map[string]string{"Retry-After": "2"}}, answer{status: 200, body: `{"invoiceId":"` + id + `","status":"NEW"}`})
	c, waits := f.client(t, nil)
	if _, err := c.GetInvoice(ctx, id); err != nil || len(*waits) != 1 || (*waits)[0] != 2*time.Second {
		t.Fatalf("%v %v", err, *waits)
	}
	// a long wait is not done for you
	f2 := newFake(t, answer{status: 429, body: `{"error":"slow down","code":"rate_limited","retryAfterSec":60}`})
	c2, _ := f2.client(t, nil)
	var ae *APIError
	if _, err := c2.GetInvoice(ctx, id); !errors.As(err, &ae) || ae.Status != 429 || ae.Code != "rate_limited" || len(f2.calls) != 1 {
		t.Fatalf("long 429: %v", err)
	}
}

func TestNoRetryForSimulateAndCodes(t *testing.T) {
	f := newFake(t, answer{status: 503, body: `{"error":"busy"}`})
	c, _ := f.client(t, nil)
	var ae *APIError
	if _, err := c.SimulatePayment(ctx, strings.Repeat("e", 24), "exact"); !errors.As(err, &ae) || ae.Status != 503 || len(f.calls) != 1 {
		t.Fatalf("simulate retried: %v", err)
	}
	f2 := newFake(t, answer{status: 409, body: `{"error":"This Idempotency-Key was already used","code":"idempotency_key_reused"}`})
	c2, _ := f2.client(t, nil)
	if _, err := c2.CreateInvoice(ctx, InvoiceParams{Amount: "1", IdempotencyKey: "k"}); !errors.As(err, &ae) || ae.Status != 409 || ae.Code != "idempotency_key_reused" || len(f2.calls) != 1 {
		t.Fatalf("409: %v", err)
	}
	if !strings.Contains(ae.Error(), "(409)") || strings.Contains(ae.Error(), "sk_unit") {
		t.Fatalf("message %q", ae.Error())
	}
}

func TestUnreachable(t *testing.T) {
	f := newFake(t, answer{drop: true}, answer{drop: true})
	c, _ := f.client(t, func(cfg *Config) { cfg.MaxAttempts = 2 })
	var ae *APIError
	_, err := c.GetInvoice(ctx, strings.Repeat("f", 24))
	if !errors.As(err, &ae) || ae.Status != 0 || !strings.Contains(err.Error(), "Could not reach") || len(f.calls) != 2 {
		t.Fatalf("%v calls=%d", err, len(f.calls))
	}
}

func TestCancelledContext(t *testing.T) {
	f := newFake(t)
	c, _ := f.client(t, nil)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := c.GetInvoice(cctx, strings.Repeat("f", 24))
	if !errors.Is(err, context.Canceled) || len(f.calls) != 0 {
		t.Fatalf("%v", err)
	}
}

func TestBadInputNeverReachesNetwork(t *testing.T) {
	f := newFake(t)
	c, _ := f.client(t, nil)
	if _, err := c.GetInvoice(ctx, "../admin"); err == nil {
		t.Fatal("bad id")
	}
	if _, err := c.SimulatePayment(ctx, strings.Repeat("a", 24), "free_money"); err == nil {
		t.Fatal("bad scenario")
	}
	if _, err := c.CreateInvoice(ctx, InvoiceParams{Amount: " "}); err == nil {
		t.Fatal("no amount")
	}
	if len(f.calls) != 0 {
		t.Fatal("request sent")
	}
}

func TestReadsAndTestWebhook(t *testing.T) {
	f := newFake(t,
		answer{status: 200, body: `{"currencies":[{"symbol":"USDT_TRC20","usdRate":1},{"symbol":"BTC","usdRate":90000.12}]}`},
		answer{status: 200, body: `{"currency":"EUR","rates":{"BTC":"81000.5"}}`},
		answer{status: 200, body: `{"delivered":true,"httpStatus":200,"eventId":"evt_test_1"}`},
	)
	c, _ := f.client(t, nil)
	cur, err := c.Currencies(ctx)
	if err != nil || len(cur) != 2 || cur[0]["symbol"] != "USDT_TRC20" {
		t.Fatalf("%v %v", err, cur)
	}
	if f.calls[0].Header.Get("x-signature") != "" {
		t.Fatal("public read was signed")
	}
	r, err := c.Rates(ctx, "EUR")
	if err != nil || r["currency"] != "EUR" || f.calls[1].Path != "/api/v1/rates?currency=EUR" {
		t.Fatalf("%v %v", err, r)
	}
	w, err := c.SendTestWebhook(ctx, "")
	var sent map[string]any
	_ = json.Unmarshal(f.calls[2].Body, &sent)
	if err != nil || w["delivered"] != true || sent["eventType"] != "invoice.paid" {
		t.Fatalf("%v %v", err, w)
	}
}

func TestClientVerifyWebhook(t *testing.T) {
	c, err := New(Config{APIKey: "pk_abcdefgh", APISecret: "s", WebhookSecret: unitSecret})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(unitEvent)
	if ev, err := c.VerifyWebhook(body, signed(body, time.Now().Unix(), unitSecret)); err != nil || ev.Type != "invoice.paid" {
		t.Fatal(err)
	}
}
