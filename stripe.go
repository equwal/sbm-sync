package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Stripe calls the few parts of the Stripe API that billing needs. The API
// takes form fields and gives JSON, so the standard library is enough.
type Stripe struct {
	Key     string // secret API key
	Webhook string // signing secret of the webhook endpoint
	API     string // https://api.stripe.com; tests replace it
	Client  *http.Client
}

func (s *Stripe) call(method, path string, form url.Values) (map[string]any, error) {
	req, err := http.NewRequest(method, s.API+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(s.Key, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("stripe %s %s: %s", method, path, resp.Status)
	}
	if resp.StatusCode >= 300 {
		msg := resp.Status
		if e, ok := out["error"].(map[string]any); ok {
			msg, _ = e["message"].(string)
		}
		return nil, fmt.Errorf("stripe %s %s: %s", method, path, msg)
	}
	return out, nil
}

// checkout starts a Stripe Checkout session for a subscription to price and
// gives the address of its page.
func (s *Stripe) checkout(a Account, price, site string) (string, error) {
	form := url.Values{
		"mode":                                 {"subscription"},
		"line_items[0][price]":                 {price},
		"line_items[0][quantity]":              {"1"},
		"client_reference_id":                  {a.ID},
		"subscription_data[metadata][account]": {a.ID},
		"success_url":                          {site + "/account"},
		"cancel_url":                           {site + "/account"},
	}
	if a.Customer != "" {
		form.Set("customer", a.Customer)
	} else {
		form.Set("customer_email", a.Email)
	}
	out, err := s.call("POST", "/v1/checkout/sessions", form)
	if err != nil {
		return "", err
	}
	u, _ := out["url"].(string)
	if u == "" {
		return "", errors.New("stripe gave no checkout address")
	}
	return u, nil
}

// portal gives the address of a Stripe customer portal session, where the
// customer changes the card, sees invoices or cancels.
func (s *Stripe) portal(customer, site string) (string, error) {
	out, err := s.call("POST", "/v1/billing_portal/sessions",
		url.Values{"customer": {customer}, "return_url": {site + "/account"}})
	if err != nil {
		return "", err
	}
	u, _ := out["url"].(string)
	if u == "" {
		return "", errors.New("stripe gave no portal address")
	}
	return u, nil
}

// deleteCustomer deletes the customer in Stripe. That ends the
// subscriptions of the customer at once.
func (s *Stripe) deleteCustomer(customer string) error {
	_, err := s.call("DELETE", "/v1/customers/"+url.PathEscape(customer), nil)
	return err
}

// label gives a price as text, such as "$3 a month".
func (s *Stripe) label(price string) (string, error) {
	out, err := s.call("GET", "/v1/prices/"+url.PathEscape(price), nil)
	if err != nil {
		return "", err
	}
	amount, _ := out["unit_amount"].(float64)
	currency, _ := out["currency"].(string)
	interval := ""
	if r, ok := out["recurring"].(map[string]any); ok {
		interval, _ = r["interval"].(string)
	}
	return money(int64(amount), currency) + " a " + interval, nil
}

func money(cents int64, currency string) string {
	n := strconv.FormatInt(cents/100, 10)
	if cents%100 != 0 {
		n = fmt.Sprintf("%d.%02d", cents/100, cents%100)
	}
	if currency == "usd" {
		return "$" + n
	}
	return n + " " + strings.ToUpper(currency)
}

var errSignature = errors.New("bad Stripe signature")

// verify checks the Stripe-Signature header of a webhook request: an
// HMAC-SHA256 of "<time>.<body>" with the signing secret. A request older
// than five minutes is refused, so that a copy cannot be sent again later.
func verify(body []byte, header, secret string, now time.Time) error {
	var t string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			t = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	ts, err := strconv.ParseInt(t, 10, 64)
	if err != nil || secret == "" {
		return errSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return errSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, sig := range sigs {
		got, err := hex.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return errSignature
}
