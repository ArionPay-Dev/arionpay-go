package arionpay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const unitSecret = "whsec_unit_0001"

const unitEvent = `{"id":"evt_abc","type":"invoice.paid","created":1,"livemode":true,"data":{"invoice":{"id":"6af75d7ce83f10cb386f0981","status":"paid","orderId":"1001","amountFiat":49.9,"currency":"USD","asset":"USDT_TRC20","paidCrypto":"49.900001","test":false,"metadata":{"user":42},"transactions":[]}}}`

func h(key string, data []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

func signed(body []byte, t int64, secret string) string {
	return fmt.Sprintf("t=%d,v1=%s", t, h(secret, append([]byte(fmt.Sprintf("%d.", t)), body...)))
}

func TestRequestSignaturesMatchBackend(t *testing.T) {
	if got, want := SignRequest("sk_x", 1700000000, "POST", "/api/v1/invoices", []byte(`{"a":1}`)), h("sk_x", []byte(`1700000000.{"a":1}`)); got != want {
		t.Fatalf("POST signature %s, want %s", got, want)
	}
	if got, want := SignRequest("sk_x", 1700000000, "get", "/api/v1/invoices/abc?x=1", []byte("ignored")), h("sk_x", []byte("1700000000.GET /api/v1/invoices/abc?x=1")); got != want {
		t.Fatalf("GET signature %s, want %s", got, want)
	}
}

func TestHeaderParsing(t *testing.T) {
	v1 := strings.Repeat("a", 64)
	if ts, sig, ok := ParseSignatureHeader("t=123,v1=" + v1); !ok || ts != 123 || sig != v1 {
		t.Fatal("plain header not parsed")
	}
	if ts, sig, ok := ParseSignatureHeader(" v1=" + strings.ToUpper(v1) + " , t=5 "); !ok || ts != 5 || sig != v1 {
		t.Fatal("spaced/upper-case header not parsed")
	}
	for _, bad := range []string{"", "t=1", "v1=" + v1, "t=x,v1=" + v1, "t=1,v1=zz", "t=1,v1=" + v1 + "0", "t=-1,v1=" + v1} {
		if _, _, ok := ParseSignatureHeader(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestVerifyValid(t *testing.T) {
	body := []byte(unitEvent)
	ev, err := VerifyWebhook(body, signed(body, time.Now().Unix(), unitSecret), unitSecret)
	if err != nil {
		t.Fatal(err)
	}
	inv := ev.Data.Invoice
	if ev.ID != "evt_abc" || ev.Type != "invoice.paid" || !ev.Livemode || inv.ID != "6af75d7ce83f10cb386f0981" || inv.Status != "paid" {
		t.Fatalf("event fields: %+v", ev)
	}
	if inv.AmountFiat != "49.9" || inv.AmountFiat.Fiat() != "49.90" || inv.PaidCrypto != "49.900001" || inv.Metadata["user"] != json.Number("42") {
		t.Fatalf("invoice fields: %+v", inv)
	}
	if !inv.Matches("49.90", "usd") || inv.Matches("49.91", "USD") || inv.Matches("49.90", "EUR") {
		t.Fatal("Matches")
	}
	if string(ev.Raw) != unitEvent || IsTestEvent(ev) {
		t.Fatal("Raw / IsTestEvent")
	}
}

func TestVerifyNonASCII(t *testing.T) {
	body := []byte(strings.Replace(unitEvent, `"status":"paid"`, `"status":"paid","description":"Café ☕ – 東京"`, 1))
	if _, err := VerifyWebhook(body, signed(body, time.Now().Unix(), unitSecret), unitSecret); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyFailures(t *testing.T) {
	body := []byte(unitEvent)
	now := time.Now().Unix()
	hdr := signed(body, now, unitSecret)
	expect := func(name string, err error, contains string) {
		t.Helper()
		var se *SignatureError
		if !errors.As(err, &se) {
			t.Fatalf("%s: want SignatureError, got %v", name, err)
		}
		if contains != "" && !strings.Contains(se.Message, contains) {
			t.Fatalf("%s: message %q lacks %q", name, se.Message, contains)
		}
	}
	_, err := VerifyWebhook([]byte(strings.Replace(unitEvent, "paid", "PAID", 1)), hdr, unitSecret)
	expect("changed body", err, "Invalid")
	_, err = VerifyWebhook([]byte(" "+unitEvent), hdr, unitSecret)
	expect("re-encoded body", err, "Invalid")
	_, err = VerifyWebhook(body, hdr, "whsec_other")
	expect("other secret", err, "Invalid")
	_, err = VerifyWebhook(body, hdr, "")
	expect("empty secret", err, "empty")
	_, err = VerifyWebhook(body, signed(body, now-301, unitSecret), unitSecret)
	expect("old", err, "stale")
	_, err = VerifyWebhook(body, signed(body, now+301, unitSecret), unitSecret)
	expect("future", err, "stale")
	if !IsValidSignature(body, signed(body, now-301, unitSecret), unitSecret, 600, time.Now()) {
		t.Fatal("custom tolerance")
	}
	_, err = VerifyWebhook(body, "", unitSecret)
	expect("no header", err, "Missing or malformed")
	if IsValidSignature(body, hdr, "", 300, time.Now()) {
		t.Fatal("empty secret accepted")
	}
}

func TestSignedButNotEvent(t *testing.T) {
	for _, b := range []string{"not json", "{}", "[]", `{"id":"","type":"invoice.paid","data":{"invoice":{}}}`, `{"id":"evt_1","type":"invoice.paid"}`, `{"id":"evt_1","type":"invoice.paid","data":{"invoice":null}}`, "\xff\xfe"} {
		body := []byte(b)
		if _, err := VerifyWebhook(body, signed(body, time.Now().Unix(), unitSecret), unitSecret); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
}

func TestOddFieldTypesStillVerify(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"invoice.paid","data":{"invoice":{"id":"6af75d7ce83f10cb386f0981","status":"paid","amountFiat":"12.5","metadata":[1],"transactions":{"x":1},"late":"yes","paidAt":null,"orderId":77,"customerEmail":"a@b.c"}}}`)
	ev, err := VerifyWebhook(body, signed(body, time.Now().Unix(), unitSecret), unitSecret)
	if err != nil {
		t.Fatal(err)
	}
	inv := ev.Data.Invoice
	if inv.OrderID != "77" || inv.AmountFiat.Fiat() != "12.50" || len(inv.Metadata) != 0 || len(inv.Transactions) != 0 || inv.Late || inv.PaidAt != "" || inv.Raw["customerEmail"] != "a@b.c" {
		t.Fatalf("%+v", inv)
	}
}

func TestTestEvents(t *testing.T) {
	if !IsTestEvent(&Event{ID: "evt_1", Test: true}) || !IsTestEvent(&Event{ID: "evt_test_1"}) {
		t.Fatal("test flag / evt_test_")
	}
	ev := &Event{ID: "evt_1"}
	ev.Data.Invoice.ID = "inv_test_x"
	if !IsTestEvent(ev) {
		t.Fatal("inv_test_")
	}
	ev.Data.Invoice.ID = "6af75d7ce83f10cb386f0981"
	if IsTestEvent(ev) {
		t.Fatal("real event counted as test")
	}
}

func TestAmount(t *testing.T) {
	for in, want := range map[string]string{"49.9": "49.90", "10": "10.00", "0.005": "0.01", "": "0.00", "abc": "0.00", "1e2": "100.00"} {
		if got := Amount(in).Fiat(); got != want {
			t.Errorf("Amount(%q).Fiat() = %s, want %s", in, got, want)
		}
	}
}
