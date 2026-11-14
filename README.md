# ArionPay for Go

Official Go client for [ArionPay](https://arionpay.com). Create crypto invoices, send customers to the payment page, and check webhooks. The money goes straight to your own wallet: ArionPay never holds it.

- No dependencies (standard library only). Go 1.21 or newer.
- Works with `net/http`, Gin, Echo, Chi, Fiber or anything that gives you the raw request body.
- Safe retries: every invoice request carries an idempotency key, so a network error can never create two invoices.
- Every call takes a `context.Context`; a cancelled context also stops the waits between retries.

```bash
go get github.com/ArionPay-Dev/arionpay-go
```

## Keys

In your ArionPay dashboard, open your store:

| Value | Where | Starts with |
| --- | --- | --- |
| API key | Store page, **API key** | `pk_` |
| API secret | Store page, **API secret** (asks for your authenticator code) | `sk_` |
| Webhook secret | **Webhooks** page, **signing secret** | `whsec_` |

Keep them on your server, for example in environment variables.

## Take a payment

```go
import arionpay "github.com/ArionPay-Dev/arionpay-go"

client, err := arionpay.New(arionpay.Config{
	APIKey:        os.Getenv("ARIONPAY_API_KEY"),
	APISecret:     os.Getenv("ARIONPAY_API_SECRET"),
	WebhookSecret: os.Getenv("ARIONPAY_WEBHOOK_SECRET"),
})
if err != nil {
	log.Fatal(err)
}

inv, err := client.CreateInvoice(ctx, arionpay.InvoiceParams{
	Amount:     "49.90",
	Currency:   "USD",
	OrderID:    "1001",
	SuccessURL: "https://shop.example.com/thanks/1001",
	CancelURL:  "https://shop.example.com/cart",
	Metadata:   map[string]any{"user_id": 42},
})
if err != nil {
	return err
}
http.Redirect(w, r, inv.InvoiceURL, http.StatusSeeOther)
```

The customer coming back to `SuccessURL` does **not** prove they paid. Deliver the order when the `invoice.paid` webhook arrives (or when `GetInvoice` says `paid`).

A webhook's `ev.Data.Invoice` and the result of `GetInvoice` are the same `arionpay.Invoice` type (`ID`, `Status`, `OrderID`, `AmountFiat`, `Currency`, `Asset`, `PaidCrypto`, `Test`, `Metadata`, ...), so one function can handle both. Amounts are `arionpay.Amount`: text exactly as ArionPay sent it, never rounded.

## Webhooks

Set your webhook address on the **Webhooks** page of the dashboard (a public `https://` address), or per invoice with `CallbackURL`. Check the signature against the **raw** body, before you parse it.

```go
http.HandleFunc("/arionpay/webhook", func(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	ev, err := client.VerifyWebhook(raw, r.Header.Get("ArionPay-Signature"))
	if err != nil { // *arionpay.SignatureError
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if !arionpay.IsTestEvent(ev) { // "Send test webhook": not a real order
		handleEvent(ev)
	}
	w.WriteHeader(http.StatusOK)
})
```

With Gin: `raw, _ := c.GetRawData()`. With Echo: `raw, _ := io.ReadAll(c.Request().Body)`. Do not bind the body to a struct first.

No client at hand? `arionpay.VerifyWebhook(raw, header, webhookSecret)` does the same.

### Handling an event

```go
func handleEvent(ev *arionpay.Event) {
	if alreadyProcessed(ev.ID) { // the same event can arrive more than once
		return
	}
	inv := ev.Data.Invoice
	order := findOrder(inv.OrderID)
	if ev.Type == "invoice.paid" && order != nil && !order.Paid &&
		inv.Matches(order.Total, order.Currency) && inv.Test == order.IsTest {
		markOrderPaid(order, inv) // deliver here
	}
	rememberProcessed(ev.ID)
}
```

`inv.Matches("49.90", "USD")` is true when the invoice price is that total, to the cent, in that currency.

| Event | Meaning | What to do |
| --- | --- | --- |
| `invoice.pending` | A payment was seen on the network and waits for confirmations. | Show "payment on its way". Do not deliver yet. |
| `invoice.partial` | Less than the full amount arrived. The invoice stays open for the rest. | Do not deliver. |
| `invoice.paid` | Fully paid and confirmed. Also sent when you accept a payment by hand in the dashboard. | Deliver the order. |
| `invoice.expired` | Time ran out without full payment. A late payment can still make it `paid`. | Keep the order unpaid. |
| `invoice.needs_review` | Something needs a person, for example more money arrived after the invoice was paid. | Tell a human. Do not change the order. |

ArionPay sends again when your server does not answer `2xx` (for about 3 days).

### Rules for a safe integration

1. Check the signature on the raw body. Never trust a webhook you could not verify.
2. Deliver only on `invoice.paid` (or when `GetInvoice` returns `Status == "paid"`).
3. Match the invoice to your order: your `OrderID` or `Metadata`, and the same amount and currency as the order's current total (`inv.Matches`).
4. Ignore test events (`arionpay.IsTestEvent(ev)`), and in your live shop ignore invoices with `Test: true`.
5. Handle each `ev.ID` once, and answer `2xx` quickly.
6. Do not compare crypto amounts yourself. ArionPay decides when an invoice is paid, including payments inside your store's payment tolerance and payments you accept in Payment Radar.

## Test without real money

```go
inv, _ := client.CreateInvoice(ctx, arionpay.InvoiceParams{Amount: "10", OrderID: "test-1", Asset: "USDT_TRC20", Test: arionpay.Bool(true)})
client.SimulatePayment(ctx, inv.ID, "exact") // your webhook gets invoice.paid, like a real payment
```

Scenarios: `exact`, `detected`, `confirm`, `underpaid_1pct`, `underpaid_5pct`, `overpaid`, `partial_half`, `late`.

`client.SendTestWebhook(ctx, "invoice.paid")` sends a signed sample event to your store's webhook address. It describes a made-up invoice (`inv_test_...`) and must never change an order.

A complete small shop (payment link + webhook) is in [`examples/webhook`](examples/webhook/main.go).

## Reference

| Method | Returns |
| --- | --- |
| `CreateInvoice(ctx, InvoiceParams)` | `*CreatedInvoice`: `ID`, `InvoiceURL`, `Status`, `AmountFiat`, `Currency`, `Asset`, `Address`, `AmountCrypto`, `ExpiresAt`, `OrderID`, `Raw` |
| `GetInvoice(ctx, id)` | `*Invoice`, with the webhook field names. `Raw` has every field ArionPay sent. |
| `SimulatePayment(ctx, id, scenario)` | Sandbox only (test invoices). |
| `SendTestWebhook(ctx, eventType)` | `map` with `delivered`, `httpStatus`, `eventId` |
| `Currencies(ctx)` | Coins you can pass as `Asset` / `AllowedAssets`. |
| `Rates(ctx, currency)` | Current coin prices. |
| `VerifyWebhook(rawBody, signatureHeader)` | `*Event`, or a `*SignatureError`. Also a package function taking the secret as third argument. |

`InvoiceParams` fields: `Amount` (required), `Currency` (default `USD`), `OrderID`, `Description`, `CustomerEmail`, `SuccessURL`, `CancelURL`, `CallbackURL`, `Asset`, `AllowedAssets`, `ExpiresMin` (5 to 1440), `Metadata`, `IdempotencyKey`, `Test`. Empty fields are not sent.

Errors from ArionPay are `*arionpay.APIError` with `Status` (HTTP status, `0` when ArionPay could not be reached), `Code` (for example `idempotency_key_reused`, `rate_limited`, `unknown_asset`) and `Message`. Messages never contain your secret.

```go
var apiErr *arionpay.APIError
if errors.As(err, &apiErr) && apiErr.Code == "idempotency_key_reused" { ... }
```

`Config` fields: `APIKey`, `APISecret`, `WebhookSecret`, `BaseURL` (default `https://merchant.arionpay.com`), `HTTPClient` (default: 20 s timeout, no redirects), `MaxAttempts` (default 3), `AppInfo` (added to the User-Agent). A `Client` is safe to share between goroutines.

## Tests

```bash
go test ./...                       # unit tests
ARIONPAY_IT_BASE_URL=... ARIONPAY_IT_API_KEY=pk_... ARIONPAY_IT_API_SECRET=sk_... \
ARIONPAY_IT_WEBHOOK_SECRET=whsec_... go test -run Integration -v   # against a real ArionPay server
```

## License

MIT
