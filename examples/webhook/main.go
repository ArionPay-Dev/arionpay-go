// A minimal shop: /pay/<order> sends the customer to ArionPay, /arionpay/webhook marks the order paid.
//
//	ARIONPAY_API_KEY=pk_... ARIONPAY_API_SECRET=sk_... ARIONPAY_WEBHOOK_SECRET=whsec_... \
//	PUBLIC_URL=https://shop.example.com go run ./examples/webhook
package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	arionpay "github.com/ArionPay-Dev/arionpay-go"
)

type order struct {
	Total, Currency string
	Paid            bool
}

var (
	mu        sync.Mutex
	orders    = map[string]*order{"1001": {Total: "49.90", Currency: "USD"}}
	processed = map[string]bool{} // event ids already handled (use your database)
)

func main() {
	client, err := arionpay.New(arionpay.Config{
		APIKey:        os.Getenv("ARIONPAY_API_KEY"),
		APISecret:     os.Getenv("ARIONPAY_API_SECRET"),
		WebhookSecret: os.Getenv("ARIONPAY_WEBHOOK_SECRET"),
		BaseURL:       os.Getenv("ARIONPAY_BASE_URL"), // empty: https://merchant.arionpay.com
		AppInfo:       "example-shop/1.0",
	})
	if err != nil {
		log.Fatal(err)
	}
	public := strings.TrimRight(os.Getenv("PUBLIC_URL"), "/")
	testMode := os.Getenv("ARIONPAY_LIVE") != "1"

	http.HandleFunc("/pay/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/pay/")
		mu.Lock()
		o := orders[id]
		mu.Unlock()
		if o == nil {
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		inv, err := client.CreateInvoice(ctx, arionpay.InvoiceParams{
			Amount:         o.Total,
			Currency:       o.Currency,
			OrderID:        id,
			SuccessURL:     public + "/thanks/" + id,
			CancelURL:      public + "/cart",
			CallbackURL:    public + "/arionpay/webhook",
			IdempotencyKey: "order-" + id + "-" + o.Total + o.Currency, // one invoice per order total
			Test:           arionpay.Bool(testMode),
		})
		if err != nil {
			log.Printf("ArionPay: %v", err)
			http.Error(w, "The crypto payment could not be started. Please try again in a minute.", http.StatusBadGateway)
			return
		}
		http.Redirect(w, r, inv.InvoiceURL, http.StatusSeeOther)
	})

	http.HandleFunc("/arionpay/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // the exact bytes: check before parsing
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		ev, err := client.VerifyWebhook(raw, r.Header.Get("ArionPay-Signature"))
		if err != nil { // a *arionpay.SignatureError: never trust this request
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if arionpay.IsTestEvent(ev) { // "Send test webhook": a made-up invoice
			w.WriteHeader(http.StatusOK)
			return
		}
		inv := ev.Data.Invoice
		mu.Lock()
		defer mu.Unlock()
		if processed[ev.ID] {
			w.WriteHeader(http.StatusOK)
			return
		}
		if inv.Test != testMode {
			http.Error(w, "test/live mismatch", http.StatusConflict)
			return
		}
		o := orders[inv.OrderID]
		if ev.Type == "invoice.paid" && o != nil && !o.Paid && inv.Matches(o.Total, o.Currency) {
			o.Paid = true // deliver here
			log.Printf("order %s paid with %s %s (invoice %s)", inv.OrderID, inv.PaidCrypto, inv.Asset, inv.ID)
		}
		processed[ev.ID] = true
		w.WriteHeader(http.StatusOK)
	})

	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	log.Println("listening on " + addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
