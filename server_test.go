package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStripe records the calls to the Stripe API and answers them.
type fakeStripe struct {
	mu    sync.Mutex
	calls []string // "METHOD /path form"
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+r.PostForm.Encode())
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/v1/checkout/sessions":
		io.WriteString(w, `{"url": "https://checkout.stripe.test/s1"}`)
	case r.URL.Path == "/v1/billing_portal/sessions":
		io.WriteString(w, `{"url": "https://billing.stripe.test/p1"}`)
	case strings.HasPrefix(r.URL.Path, "/v1/customers/"):
		io.WriteString(w, `{"deleted": true}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error": {"message": "no such thing"}}`)
	}
}

func (f *fakeStripe) called(prefix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return c
		}
	}
	return ""
}

type harness struct {
	t      *testing.T
	s      *server
	web    *httptest.Server
	stripe *fakeStripe
	mu     sync.Mutex
	mails  []string // "to\nsubject\nbody" of each email
}

func newEnv(t *testing.T, billing bool) *harness {
	store, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &server{store: store, trial: 30 * 24 * time.Hour,
		prices: map[string]string{"month": "price_m", "year": "price_y"},
		labels: map[string]string{"month": "$3 a month", "year": "$30 a year"}}
	e := &harness{t: t, s: s}
	if billing {
		e.stripe = &fakeStripe{}
		api := httptest.NewServer(e.stripe)
		t.Cleanup(api.Close)
		s.stripe = &Stripe{Key: "sk_test_x", Webhook: "whsec_test", API: api.URL, Client: api.Client()}
	}
	e.web = httptest.NewServer(s.routes())
	t.Cleanup(e.web.Close)
	s.site, s.origin = e.web.URL, e.web.URL
	return e
}

// browser gives a client that keeps cookies and does not follow redirects.
func (e *harness) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func (e *harness) post(c *http.Client, path string, form url.Values) *http.Response {
	req, _ := http.NewRequest("POST", e.web.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.web.URL)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *harness) signup(email string) *http.Client {
	c := e.browser()
	resp := e.post(c, "/signup", url.Values{"email": {email}, "password": {"password1"}})
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("signup: %s", resp.Status)
	}
	return c
}

func (e *harness) token(email string) string {
	resp := e.post(http.DefaultClient, "/api/login", url.Values{"email": {email}, "password": {"password1"}})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("api login: %s %s", resp.Status, b)
	}
	return strings.TrimSpace(string(b))
}

// sync sends a file as a device does and gives the answer.
func (e *harness) sync(token, base, text string) (int, string, string) {
	req, _ := http.NewRequest("POST", e.web.URL+"/api/sync?base="+base, strings.NewReader(text))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header.Get("Sbm-Version")
}

func (e *harness) body(c *http.Client, path string) string {
	resp, err := c.Get(e.web.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// withMail turns on the email check. The emails of the server go to
// e.mails.
func (e *harness) withMail() {
	e.s.mail = func(to, subject, body string) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.mails = append(e.mails, to+"\n"+subject+"\n"+body)
		return nil
	}
}

func (e *harness) sent() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.mails)
}

var checkLink = regexp.MustCompile(`http\S+/verify\?code=[0-9a-f]+`)

// link gives the link in the last email.
func (e *harness) link() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.mails) == 0 {
		e.t.Fatal("no email went out")
	}
	return checkLink.FindString(e.mails[len(e.mails)-1])
}

func (e *harness) open(link string) int {
	resp, err := http.Get(link)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestTwoDevicesSync(t *testing.T) {
	e := newEnv(t, false)
	e.signup("Me@Example.org")
	a, b := e.token("me@example.org"), e.token(" me@example.org")

	code, text, va := e.sync(a, "", "https://a.org\tA\t\n")
	if code != 200 || text != "https://a.org\tA\t\n" || len(va) != 64 {
		t.Fatalf("first sync: %d %q %q", code, text, va)
	}
	_, text, vb := e.sync(b, "", "https://b.org\tB\t\n")
	if text != "https://a.org\tA\t\nhttps://b.org\tB\t\n" {
		t.Fatalf("second device: %q", text)
	}
	// Device a removes its bookmark and adds another.
	_, text, _ = e.sync(a, va, "https://c.org\tC\t\n")
	if text != "https://b.org\tB\t\nhttps://c.org\tC\t\n" {
		t.Fatalf("remove and add: %q", text)
	}
	// Device b has no changes and gets the file of device a.
	_, text, _ = e.sync(b, vb, "https://a.org\tA\t\nhttps://b.org\tB\t\n")
	if text != "https://b.org\tB\t\nhttps://c.org\tC\t\n" {
		t.Fatalf("pull: %q", text)
	}
}

func TestSyncRefusals(t *testing.T) {
	e := newEnv(t, false)
	e.signup("me@example.org")
	tok := e.token("me@example.org")
	if code, _, _ := e.sync("wrong", "", "x\n"); code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", code)
	}
	if code, _, _ := e.sync(tok, "../../accounts.json", "x\n"); code != http.StatusBadRequest {
		t.Errorf("bad base: %d", code)
	}
	if code, _, _ := e.sync(tok, "", strings.Repeat("x", maxFile+1)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("large file: %d", code)
	}
	// An unknown base is no error: nothing is removed.
	if code, text, _ := e.sync(tok, strings.Repeat("0", 64), "a\n"); code != 200 || text != "a\n" {
		t.Errorf("unknown base: %d %q", code, text)
	}
}

func TestSignupAndLogin(t *testing.T) {
	e := newEnv(t, false)
	c := e.signup("me@example.org")
	if !strings.Contains(e.body(c, "/account"), "Signed in as me@example.org") {
		t.Error("the account page does not show the account")
	}
	if resp := e.post(e.browser(), "/signup", url.Values{"email": {"ME@example.org"}, "password": {"password2"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("second signup with the same email: %s", resp.Status)
	}
	for _, bad := range []url.Values{
		{"email": {"not an address"}, "password": {"password1"}},
		{"email": {"Me <x@example.org>"}, "password": {"password1"}},
		{"email": {"y@example.org"}, "password": {"short"}},
		{"email": {"y@example.org"}, "password": {strings.Repeat("p", 73)}},
	} {
		if resp := e.post(e.browser(), "/signup", bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("signup %v: %s", bad, resp.Status)
		}
	}
	if resp := e.post(e.browser(), "/login", url.Values{"email": {"me@example.org"}, "password": {"wrong-password"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password: %s", resp.Status)
	}
	e.post(c, "/logout", nil)
	if strings.Contains(e.body(c, "/"), "Your account") {
		t.Error("still signed in after sign out")
	}
}

func TestOtherSiteCannotPostForms(t *testing.T) {
	e := newEnv(t, false)
	c := e.signup("me@example.org")
	req, _ := http.NewRequest("POST", e.web.URL+"/account/delete", strings.NewReader("password=password1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("post from another site: %s", resp.Status)
	}
	if e.s.store.byTok(e.token("me@example.org")) == nil {
		t.Error("the account is gone")
	}
}

func TestLoginLimit(t *testing.T) {
	e := newEnv(t, false)
	e.signup("me@example.org")
	var last int
	for range 21 {
		resp := e.post(http.DefaultClient, "/api/login", url.Values{"email": {"me@example.org"}, "password": {"guess"}})
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("attempt 21: %d", last)
	}
}

func TestDeleteAccount(t *testing.T) {
	e := newEnv(t, true)
	c := e.signup("me@example.org")
	tok := e.token("me@example.org")
	e.sync(tok, "", "a\n")
	a := e.s.store.byTok(tok)
	e.s.store.setCustomer(a, "cus_1")

	if resp := e.post(c, "/account/delete", url.Values{"password": {"wrong-password"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("delete with a wrong password: %s", resp.Status)
	}
	if resp := e.post(c, "/account/delete", url.Values{"password": {"password1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %s", resp.Status)
	}
	if e.stripe.called("DELETE /v1/customers/cus_1") == "" {
		t.Error("the Stripe customer is not deleted")
	}
	if code, _, _ := e.sync(tok, "", "a\n"); code != http.StatusUnauthorized {
		t.Errorf("sync after delete: %d", code)
	}
	if _, err := os.Stat(filepath.Join(e.s.store.dir, "files", a.ID)); !os.IsNotExist(err) {
		t.Errorf("the files of the account are still there: %v", err)
	}
}

func TestTrialAndSubscription(t *testing.T) {
	e := newEnv(t, true)
	c := e.signup("me@example.org")
	tok := e.token("me@example.org")
	if code, _, _ := e.sync(tok, "", "a\n"); code != 200 {
		t.Fatalf("sync in the trial: %d", code)
	}
	if !strings.Contains(e.body(c, "/account"), "Free trial: 30 days left.") {
		t.Error("the account page does not show the trial")
	}
	a := e.s.store.byTok(tok)
	a.Created = time.Now().Add(-31 * 24 * time.Hour)
	if code, text, _ := e.sync(tok, "", "a\n"); code != http.StatusPaymentRequired {
		t.Fatalf("sync after the trial: %d %q", code, text)
	}

	resp := e.post(c, "/account/checkout", url.Values{"plan": {"year"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://checkout.stripe.test/s1" {
		t.Fatalf("checkout: %s to %q", resp.Status, resp.Header.Get("Location"))
	}
	call := e.stripe.called("POST /v1/checkout/sessions")
	for _, want := range []string{"client_reference_id=" + a.ID, "price%5D=price_y", "customer_email=me%40example.org", "mode=subscription"} {
		if !strings.Contains(call, want) {
			t.Errorf("checkout call %q lacks %q", call, want)
		}
	}

	now := time.Now().Unix()
	e.event(now, "checkout.session.completed", fmt.Sprintf(`{"customer":"cus_9","client_reference_id":%q}`, a.ID))
	e.event(now+2, "customer.subscription.updated", `{"customer":"cus_9","status":"active"}`)
	e.event(now+1, "customer.subscription.created", `{"customer":"cus_9","status":"incomplete"}`) // late
	if code, _, _ := e.sync(tok, "", "a\n"); code != 200 {
		t.Fatalf("sync with a subscription: %d", code)
	}
	if page := e.body(c, "/account"); !strings.Contains(page, "Your subscription is active.") || strings.Contains(page, "Subscribe:") {
		t.Error("the account page does not show the subscription")
	}
	if resp := e.post(c, "/account/portal", nil); resp.Header.Get("Location") != "https://billing.stripe.test/p1" {
		t.Errorf("portal: %s", resp.Status)
	}
	e.event(now+3, "customer.subscription.deleted", `{"customer":"cus_9","status":"canceled"}`)
	if code, _, _ := e.sync(tok, "", "a\n"); code != http.StatusPaymentRequired {
		t.Errorf("sync after the end of the subscription: %d", code)
	}
}

func TestTrialStartsWithBilling(t *testing.T) {
	e := newEnv(t, true)
	e.signup("me@example.org")
	tok := e.token("me@example.org")
	a := e.s.store.byTok(tok)
	a.Created = time.Now().Add(-100 * 24 * time.Hour) // from before billing
	e.s.since = time.Now().Add(-10 * 24 * time.Hour)
	if code, _, _ := e.sync(tok, "", "a\n"); code != 200 {
		t.Errorf("sync 10 days after billing started: %d", code)
	}
	e.s.since = time.Now().Add(-31 * 24 * time.Hour)
	if code, _, _ := e.sync(tok, "", "a\n"); code != http.StatusPaymentRequired {
		t.Errorf("sync 31 days after billing started: %d", code)
	}
}

func TestSubscriptionFoundByMetadata(t *testing.T) {
	e := newEnv(t, true)
	e.signup("me@example.org")
	a := e.s.store.byTok(e.token("me@example.org"))
	e.event(time.Now().Unix(), "customer.subscription.created",
		fmt.Sprintf(`{"customer":"cus_5","status":"active","metadata":{"account":%q}}`, a.ID))
	if v := e.s.store.view(a); v.Customer != "cus_5" || v.Status != "active" {
		t.Errorf("account: customer %q, status %q", v.Customer, v.Status)
	}
}

// event sends a signed webhook event as Stripe does.
func (e *harness) event(created int64, typ, object string) {
	body := fmt.Sprintf(`{"type":%q,"created":%d,"data":{"object":%s}}`, typ, created, object)
	req, _ := http.NewRequest("POST", e.web.URL+"/stripe", strings.NewReader(body))
	req.Header.Set("Stripe-Signature", sign([]byte(body), "whsec_test", time.Now().Unix()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		e.t.Fatalf("event %s: %s", typ, resp.Status)
	}
}

func sign(body []byte, secret string, t int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", t, body)
	return fmt.Sprintf("t=%d,v1=%s", t, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerify(t *testing.T) {
	body := []byte(`{"type":"x"}`)
	now := time.Unix(1_800_000_000, 0)
	good := sign(body, "whsec_a", now.Unix())
	if err := verify(body, good, "whsec_a", now); err != nil {
		t.Errorf("good signature: %v", err)
	}
	if err := verify(body, "t=1,v1=00,"+strings.TrimPrefix(good, "t=1800000000,"), "whsec_a", now); err == nil {
		t.Error("a signature for another time passes")
	}
	if err := verify(body, "v1=00,"+good, "whsec_a", now); err != nil {
		t.Errorf("one good signature among more: %v", err)
	}
	for name, c := range map[string]struct {
		body   string
		header string
		secret string
		now    time.Time
	}{
		"changed body":  {`{"type":"y"}`, good, "whsec_a", now},
		"other secret":  {string(body), good, "whsec_b", now},
		"old":           {string(body), good, "whsec_a", now.Add(6 * time.Minute)},
		"no header":     {string(body), "", "whsec_a", now},
		"no secret set": {string(body), good, "", now},
	} {
		if err := verify([]byte(c.body), c.header, c.secret, c.now); err == nil {
			t.Errorf("%s: passes", name)
		}
	}
	e := newEnv(t, true)
	req, _ := http.NewRequest("POST", e.web.URL+"/stripe", strings.NewReader(`{}`))
	req.Header.Set("Stripe-Signature", "t=1,v1=00")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("webhook with a bad signature: %s", resp.Status)
	}
}

func TestMoney(t *testing.T) {
	for cents, want := range map[int64]string{300: "$3", 3000: "$30", 299: "$2.99", 5: "$0.05"} {
		if got := money(cents, "usd"); got != want {
			t.Errorf("money(%d) = %q, want %q", cents, got, want)
		}
	}
	if got := money(500, "eur"); got != "5 EUR" {
		t.Errorf("euro: %q", got)
	}
}

func TestStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := openStore(dir)
	a, err := s.signup("me@example.org", "password1", "")
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := s.signIn(a)
	s.sync(a, "", "a\n")
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := s2.byTok(tok)
	if b == nil || b.Email != "me@example.org" {
		t.Fatal("the account or its token is lost")
	}
	if text, _, _ := s2.sync(b, "", ""); text != "a\n" {
		t.Errorf("the file is lost: %q", text)
	}
}

func TestOldVersionsGo(t *testing.T) {
	s, _ := openStore(t.TempDir())
	a, _ := s.signup("me@example.org", "password1", "")
	base := ""
	for i := range maxVersions + 10 {
		_, v, err := s.sync(a, base, fmt.Sprintf("line %d\n", i))
		if err != nil {
			t.Fatal(err)
		}
		base = v
		time.Sleep(2 * time.Millisecond) // distinct times of change
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir, "files", a.ID))
	n := 0
	for _, e := range entries {
		if hashName.MatchString(e.Name()) {
			n++
		}
	}
	if n != maxVersions {
		t.Errorf("%d versions kept, want %d", n, maxVersions)
	}
}

func TestEmailCheck(t *testing.T) {
	e := newEnv(t, false)
	e.withMail()
	c := e.signup("Me@Example.org")
	tok := e.token("me@example.org")
	if e.sent() != 1 || !strings.HasPrefix(e.mails[0], "me@example.org\nConfirm your email address") {
		t.Fatalf("emails: %q", e.mails)
	}
	first := e.link()
	if code, text, _ := e.sync(tok, "", "a\n"); code != http.StatusForbidden || !strings.Contains(text, "confirm your email address") {
		t.Fatalf("sync before the check: %d %q", code, text)
	}
	if !strings.Contains(e.body(c, "/account"), "Confirm your email address.") {
		t.Error("the account page does not ask for the check")
	}
	// A new email replaces the link of the first one.
	if resp := e.post(c, "/account/verify", nil); resp.StatusCode != http.StatusSeeOther || e.sent() != 2 {
		t.Fatalf("send again: %s, %d emails", resp.Status, e.sent())
	}
	if code := e.open(first); code != http.StatusBadRequest {
		t.Errorf("the replaced link: %d", code)
	}
	if code := e.open(e.link()); code != http.StatusOK {
		t.Fatalf("the new link: %d", code)
	}
	if code, _, _ := e.sync(tok, "", "a\n"); code != http.StatusOK {
		t.Errorf("sync after the check: %d", code)
	}
	if strings.Contains(e.body(c, "/account"), "Confirm your email address.") {
		t.Error("the account page still asks for the check")
	}
	if code := e.open(e.link()); code != http.StatusBadRequest {
		t.Errorf("the link works twice: %d", code)
	}
	e.post(c, "/account/verify", nil)
	if e.sent() != 2 {
		t.Error("an email went out for a confirmed address")
	}
}

func TestEmailCheckLimit(t *testing.T) {
	e := newEnv(t, false)
	e.withMail()
	c := e.signup("me@example.org")
	for range 5 {
		e.post(c, "/account/verify", nil)
	}
	if resp := e.post(c, "/account/verify", nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the seventh email in an hour: %s", resp.Status)
	}
	if e.sent() != 6 {
		t.Errorf("%d emails went out, want 6", e.sent())
	}
}

func TestEmailThatDoesNotGoOut(t *testing.T) {
	e := newEnv(t, false)
	e.s.mail = func(string, string, string) error { return errors.New("no SMTP server") }
	c := e.signup("me@example.org")
	resp := e.post(c, "/account/verify", nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), "The email did not go out.") {
		t.Errorf("send again: %s", resp.Status)
	}
}
