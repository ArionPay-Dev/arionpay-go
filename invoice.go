package arionpay

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Amount is a decimal amount kept as text (crypto amounts must not be rounded by float64). It accepts JSON strings
// and numbers.
type Amount string

func (a *Amount) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*a = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = Amount(s)
		return nil
	}
	*a = Amount(string(b))
	return nil
}

// Float returns the amount as a float64 (for display or comparisons of fiat prices; 0 when empty or invalid).
func (a Amount) Float() float64 {
	f, _ := strconv.ParseFloat(string(a), 64)
	return f
}

// Fiat formats a fiat amount with two decimals ("49.90"), the form to compare with your order total.
func (a Amount) Fiat() string {
	return strconv.FormatFloat(a.Float(), 'f', 2, 64)
}

// Invoice uses the field names of the webhook's data.invoice, for webhooks and signed reads alike.
type Invoice struct {
	ID                       string         `json:"id"`
	Status                   string         `json:"status"` // new, pending, paid, expired, ...
	OrderID                  string         `json:"orderId"`
	AmountFiat               Amount         `json:"amountFiat"` // the invoice price in Currency
	Currency                 string         `json:"currency"`
	Asset                    string         `json:"asset"` // e.g. USDT_TRC20
	Network                  string         `json:"network"`
	AmountCrypto             Amount         `json:"amountCrypto"`
	PaidCrypto               Amount         `json:"paidCrypto"`
	ConfirmedCrypto          Amount         `json:"confirmedCrypto"`
	Overpaid                 bool           `json:"overpaid"`
	UnderpaidWithinTolerance bool           `json:"underpaidWithinTolerance"`
	Late                     bool           `json:"late"`
	NeedsReview              bool           `json:"needsReview"`
	ReviewReason             string         `json:"reviewReason,omitempty"`
	Manual                   bool           `json:"manual"`
	Test                     bool           `json:"test"`
	Transactions             []any          `json:"transactions"`
	Metadata                 map[string]any `json:"metadata"` // your data from CreateInvoice (numbers are json.Number)
	PaidAt                   string         `json:"paidAt,omitempty"`
	TxHash                   string         `json:"txHash,omitempty"`
	Raw                      map[string]any `json:"-"` // every field as ArionPay sent it (also customerEmail, createdAt, expiresAt, ...)
}

// UnmarshalJSON reads a webhook's data.invoice leniently: a field of an unexpected type becomes empty instead of
// making a valid, signed webhook unreadable.
func (inv *Invoice) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var r map[string]any
	if err := dec.Decode(&r); err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	m, _ := r["metadata"].(map[string]any)
	t, _ := r["transactions"].([]any)
	if m == nil {
		m = map[string]any{}
	}
	if t == nil {
		t = []any{}
	}
	*inv = Invoice{
		ID: str(r["id"]), Status: str(r["status"]), OrderID: str(r["orderId"]), AmountFiat: Amount(str(r["amountFiat"])),
		Currency: str(r["currency"]), Asset: str(r["asset"]), Network: str(r["network"]),
		AmountCrypto: Amount(str(r["amountCrypto"])), PaidCrypto: Amount(str(r["paidCrypto"])),
		ConfirmedCrypto: Amount(str(r["confirmedCrypto"])), Overpaid: flag(r["overpaid"]),
		UnderpaidWithinTolerance: flag(r["underpaidWithinTolerance"]), Late: flag(r["late"]),
		NeedsReview: flag(r["needsReview"]), ReviewReason: str(r["reviewReason"]), Manual: flag(r["manual"]),
		Test: flag(r["test"]), Transactions: t, Metadata: m, PaidAt: str(r["paidAt"]), TxHash: str(r["txHash"]), Raw: r,
	}
	return nil
}

// Matches is true when the invoice price is exactly this order total and currency: the check to make before you
// deliver. Crypto amounts never need comparing: ArionPay decides whether an invoice is paid.
func (inv *Invoice) Matches(orderTotal string, currency string) bool {
	return inv.AmountFiat.Fiat() == Amount(orderTotal).Fiat() && strings.EqualFold(inv.Currency, currency)
}

// CreatedInvoice is the answer of CreateInvoice. Send the customer to InvoiceURL.
type CreatedInvoice struct {
	ID           string
	InvoiceURL   string
	Status       string
	AmountFiat   Amount
	Currency     string
	Asset        string
	Address      string
	AmountCrypto Amount
	ExpiresAt    string
	OrderID      string
	Raw          map[string]any
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func flag(v any) bool { b, _ := v.(bool); return b }

func num(v any) float64 {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// normalizeRead turns the signed read (GET /api/v1/invoices/<id>) into webhook field names.
func normalizeRead(r map[string]any) *Invoice {
	id := str(r["invoiceId"])
	if id == "" {
		id = str(r["id"])
	}
	paid := str(r["amountReceived"])
	if paid == "" {
		paid = "0"
	}
	confirmed := str(r["amountConfirmed"])
	if confirmed == "" {
		confirmed = "0"
	}
	inv := &Invoice{
		ID:                       id,
		Status:                   strings.ToLower(str(r["status"])),
		OrderID:                  str(r["orderId"]),
		AmountFiat:               Amount(str(r["amountFiat"])),
		Currency:                 str(r["currency"]),
		Asset:                    str(r["settledCurrency"]),
		Network:                  str(r["network"]),
		AmountCrypto:             Amount(str(r["amountExpected"])),
		PaidCrypto:               Amount(paid),
		ConfirmedCrypto:          Amount(confirmed),
		Overpaid:                 flag(r["overpaid"]),
		UnderpaidWithinTolerance: flag(r["underpaidWithinTolerance"]),
		Late:                     flag(r["late"]),
		NeedsReview:              flag(r["needsReview"]),
		ReviewReason:             str(r["reviewReason"]),
		Manual:                   flag(r["manual"]),
		Test:                     flag(r["test"]),
		PaidAt:                   str(r["settledAt"]),
		TxHash:                   str(r["txHash"]),
		Raw:                      r,
	}
	if t, ok := r["transactions"].([]any); ok {
		inv.Transactions = t
	} else {
		inv.Transactions = []any{}
	}
	if m, ok := r["metadata"].(map[string]any); ok {
		inv.Metadata = m
	} else {
		inv.Metadata = map[string]any{}
	}
	return inv
}
