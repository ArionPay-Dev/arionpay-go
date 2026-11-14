package arionpay

// Runs against a real ArionPay server. Skipped unless ARIONPAY_IT_BASE_URL is set:
//
//	ARIONPAY_IT_BASE_URL=http://127.0.0.1:18090 ARIONPAY_IT_API_KEY=pk_... ARIONPAY_IT_API_SECRET=sk_... \
//	ARIONPAY_IT_WEBHOOK_SECRET=whsec_... go test -run Integration -v
//
// The store's webhook address must be http://127.0.0.1:<ARIONPAY_IT_HOOK_PORT, default 18099>/hook (a dev server
// that allows private webhook addresses).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIntegrationFullSandboxPayment(t *testing.T) {
	base := os.Getenv("ARIONPAY_IT_BASE_URL")
	if base == "" {
		t.Skip("set ARIONPAY_IT_BASE_URL to run against a real server")
	}
	port := os.Getenv("ARIONPAY_IT_HOOK_PORT")
	if port == "" {
		port = "18099"
	}
	c, err := New(Config{APIKey: os.Getenv("ARIONPAY_IT_API_KEY"), APISecret: os.Getenv("ARIONPAY_IT_API_SECRET"), WebhookSecret: os.Getenv("ARIONPAY_IT_WEBHOOK_SECRET"), BaseURL: base, AppInfo: "go-integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var mu sync.Mutex
	var received []*Event
	var refused []string
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		ev, err := c.VerifyWebhook(raw, r.Header.Get("ArionPay-Signature"))
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			refused = append(refused, err.Error())
			w.WriteHeader(401)
			return
		}
		received = append(received, ev)
		w.WriteHeader(200)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	waitFor := func(what string, pred func(*Event) bool) *Event {
		t.Helper()
		end := time.Now().Add(60 * time.Second)
		for time.Now().Before(end) {
			mu.Lock()
			for _, e := range received {
				if pred(e) {
					mu.Unlock()
					return e
				}
			}
			mu.Unlock()
			time.Sleep(300 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		types := []string{}
		for _, e := range received {
			types = append(types, e.Type)
		}
		t.Fatalf("%s not received; got %v, refused %v", what, types, refused)
		return nil
	}

	cur, err := c.Currencies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hasUSDT := false
	for _, x := range cur {
		hasUSDT = hasUSDT || x["symbol"] == "USDT_TRC20"
	}
	if !hasUSDT {
		t.Fatalf("USDT_TRC20 missing from %v", cur)
	}
	if r, err := c.Rates(ctx, "EUR"); err != nil || len(r) == 0 {
		t.Fatalf("rates: %v %v", err, r)
	}

	orderID := fmt.Sprintf("go-it-%d", time.Now().UnixNano())
	inv, err := c.CreateInvoice(ctx, InvoiceParams{Amount: "7.25", Currency: "USD", OrderID: orderID, Asset: "USDT_TRC20", Test: Bool(true), Metadata: map[string]any{"cart": "go", "n": 3}, CallbackURL: "http://127.0.0.1:" + port + "/hook"})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{24}$`).MatchString(inv.ID) || !strings.HasSuffix(inv.InvoiceURL, "/pay/"+inv.ID) || inv.OrderID != orderID {
		t.Fatalf("created: %+v", inv)
	}

	key := fmt.Sprintf("go-key-%d", time.Now().UnixNano())
	a, err1 := c.CreateInvoice(ctx, InvoiceParams{Amount: "5", OrderID: orderID + "-b", IdempotencyKey: key, Test: Bool(true)})
	b, err2 := c.CreateInvoice(ctx, InvoiceParams{Amount: "5", OrderID: orderID + "-b", IdempotencyKey: key, Test: Bool(true)})
	if err1 != nil || err2 != nil || a.ID != b.ID {
		t.Fatalf("idempotency: %v %v", err1, err2)
	}
	var ae *APIError
	if _, err := c.CreateInvoice(ctx, InvoiceParams{Amount: "6", OrderID: orderID + "-b", IdempotencyKey: key, Test: Bool(true)}); !errors.As(err, &ae) || ae.Code != "idempotency_key_reused" {
		t.Fatalf("reused key: %v", err)
	}

	read, err := c.GetInvoice(ctx, inv.ID)
	if err != nil || read.Status != "new" || read.Metadata["cart"] != "go" || !read.Test || read.OrderID != orderID {
		t.Fatalf("read before paying: %v %+v", err, read)
	}

	if _, err := c.SimulatePayment(ctx, inv.ID, "detected"); err != nil {
		t.Fatal(err)
	}
	pending := waitFor("invoice.pending", func(e *Event) bool { return e.Type == "invoice.pending" && e.Data.Invoice.ID == inv.ID })
	if pending.Data.Invoice.Status != "pending" {
		t.Fatalf("pending status %q", pending.Data.Invoice.Status)
	}
	if _, err := c.SimulatePayment(ctx, inv.ID, "confirm"); err != nil {
		t.Fatal(err)
	}
	paid := waitFor("invoice.paid", func(e *Event) bool { return e.Type == "invoice.paid" && e.Data.Invoice.ID == inv.ID })
	pi := paid.Data.Invoice
	if pi.OrderID != orderID || !pi.Test || IsTestEvent(paid) || !pi.Matches("7.25", "USD") || pi.Asset != "USDT_TRC20" || pi.PaidCrypto == "" || pi.Metadata["cart"] != "go" {
		t.Fatalf("paid event: %+v", pi)
	}

	read, err = c.GetInvoice(ctx, inv.ID)
	if err != nil || read.Status != "paid" || !read.Matches("7.25", "USD") || read.Asset != "USDT_TRC20" || read.PaidCrypto == "0" {
		t.Fatalf("read after paying: %v %+v", err, read)
	}

	// Payment tolerance: 1% under is still paid, flagged.
	under, err := c.CreateInvoice(ctx, InvoiceParams{Amount: "20", OrderID: orderID + "-u", Asset: "USDT_TRC20", Test: Bool(true), CallbackURL: "http://127.0.0.1:" + port + "/hook"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SimulatePayment(ctx, under.ID, "underpaid_1pct"); err != nil {
		t.Fatal(err)
	}
	up := waitFor("tolerance invoice.paid", func(e *Event) bool { return e.Type == "invoice.paid" && e.Data.Invoice.ID == under.ID })
	if !up.Data.Invoice.UnderpaidWithinTolerance || !up.Data.Invoice.Matches("20", "USD") {
		t.Fatalf("tolerance: %+v", up.Data.Invoice)
	}

	tw, err := c.SendTestWebhook(ctx, "invoice.expired")
	if err != nil || tw["delivered"] != true {
		t.Fatalf("test webhook: %v %v", err, tw)
	}
	te := waitFor("test event", func(e *Event) bool { return e.ID == tw["eventId"] })
	if !IsTestEvent(te) {
		t.Fatalf("test event not flagged: %+v", te)
	}

	if _, err := c.GetInvoice(ctx, strings.Repeat("0", 24)); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("unknown invoice: %v", err)
	}
	wrong, _ := New(Config{APIKey: os.Getenv("ARIONPAY_IT_API_KEY"), APISecret: "sk_wrong", BaseURL: base})
	if _, err := wrong.GetInvoice(ctx, inv.ID); !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("wrong secret: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(refused) != 0 {
		t.Fatalf("refused webhooks: %v", refused)
	}
	t.Logf("invoice %s paid, %d signed webhooks verified", inv.ID, len(received))
}
