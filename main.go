// sbm-sync keeps the sbm bookmark file of a user the same on each device.
//
// A device sends its copy of the file and the name of the version that it
// got last time. The server merges the changes of the device into its own
// copy, keeps the result and sends it back. Accounts are an email address
// and a password. Billing through Stripe is off unless STRIPE_SECRET_KEY is
// set.
package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxFile = 4 << 20 // bytes in a bookmark file

//go:embed pages.html
var pagesHTML string

var pages = template.Must(template.New("").Parse(pagesHTML))

type server struct {
	store   *Store
	stripe  *Stripe // nil when billing is off
	site    string  // public address, such as https://sbm.example.org
	origin  string  // scheme and host of site
	secure  bool    // site is https
	trial   time.Duration
	since   time.Time // start of billing: no trial ends before since + trial
	contact string
	prices  map[string]string // plan ("month", "year"): Stripe price ID
	labels  map[string]string // plan: price as text
	limit   limiter
}

func main() {
	site := strings.TrimRight(env("SBM_URL", "http://localhost:8750"), "/")
	u, err := url.Parse(site)
	if err != nil || u.Host == "" {
		log.Fatalf("SBM_URL: not an address: %q", site)
	}
	days, err := strconv.Atoi(env("SBM_TRIAL_DAYS", "30"))
	if err != nil {
		log.Fatalf("SBM_TRIAL_DAYS: %v", err)
	}
	var since time.Time
	if v := os.Getenv("SBM_BILLING_START"); v != "" {
		if since, err = time.Parse("2006-01-02", v); err != nil {
			log.Fatalf("SBM_BILLING_START: %v", err)
		}
	}
	store, err := openStore(env("SBM_DATA", "data"))
	if err != nil {
		log.Fatal(err)
	}
	s := &server{
		store:   store,
		site:    site,
		origin:  u.Scheme + "://" + u.Host,
		secure:  u.Scheme == "https",
		trial:   time.Duration(days) * 24 * time.Hour,
		since:   since,
		contact: os.Getenv("SBM_CONTACT"),
		prices:  map[string]string{"month": os.Getenv("STRIPE_PRICE_MONTH"), "year": os.Getenv("STRIPE_PRICE_YEAR")},
		labels:  map[string]string{"month": "monthly", "year": "yearly"},
	}
	if key := os.Getenv("STRIPE_SECRET_KEY"); key != "" {
		for _, v := range []string{"STRIPE_WEBHOOK_SECRET", "STRIPE_PRICE_MONTH", "STRIPE_PRICE_YEAR"} {
			if os.Getenv(v) == "" {
				log.Fatalf("STRIPE_SECRET_KEY is set, so %s must be set too", v)
			}
		}
		s.stripe = &Stripe{Key: key, Webhook: os.Getenv("STRIPE_WEBHOOK_SECRET"),
			API: "https://api.stripe.com", Client: &http.Client{Timeout: 20 * time.Second}}
		for plan, price := range s.prices {
			if l, err := s.stripe.label(price); err != nil {
				log.Printf("price of the %s plan: %v", plan, err)
			} else {
				s.labels[plan] = l
			}
		}
	}
	addr := env("SBM_ADDR", "127.0.0.1:8750")
	log.Printf("sbm-sync on %s for %s, billing %v", addr, site, s.stripe != nil)
	hs := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	log.Fatal(hs.ListenAndServe())
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func (s *server) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", s.home)
	m.HandleFunc("GET /privacy", s.privacy)
	m.HandleFunc("GET /signup", s.form("signup"))
	m.HandleFunc("POST /signup", s.signup)
	m.HandleFunc("GET /login", s.form("login"))
	m.HandleFunc("POST /login", s.login)
	m.HandleFunc("POST /logout", s.logout)
	m.HandleFunc("GET /account", s.account)
	m.HandleFunc("POST /account/checkout", s.checkout)
	m.HandleFunc("POST /account/portal", s.portal)
	m.HandleFunc("POST /account/delete", s.remove)
	m.HandleFunc("POST /stripe", s.webhook)
	m.HandleFunc("POST /api/login", s.apiLogin)
	m.HandleFunc("POST /api/logout", s.apiLogout)
	m.HandleFunc("POST /api/sync", s.sync)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; "+
			"form-action 'self' https://checkout.stripe.com https://billing.stripe.com; "+
			"frame-ancestors 'none'; base-uri 'none'")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		if s.secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		m.ServeHTTP(w, r)
	})
}

// ---- web pages ----

type page struct {
	Title, Error, Email, State, URL, Contact string
	Month, Year                              string
	TrialDays                                int
	Billing, CanSubscribe                    bool
	Customer                                 string
}

func (s *server) page(title string) page {
	return page{Title: title, URL: s.site, Contact: s.contact, Billing: s.stripe != nil,
		Month: s.labels["month"], Year: s.labels["year"], TrialDays: int(s.trial.Hours() / 24)}
}

func (s *server) render(w http.ResponseWriter, status int, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pages.ExecuteTemplate(w, name, p); err != nil {
		log.Printf("page %s: %v", name, err)
	}
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	p := s.page("")
	if a := s.user(r); a != nil {
		p.Email = a.Email
	}
	s.render(w, http.StatusOK, "home", p)
}

func (s *server) privacy(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "privacy", s.page("Privacy"))
}

func (s *server) form(name string) http.HandlerFunc {
	title := map[string]string{"signup": "Create an account", "login": "Sign in"}[name]
	return func(w http.ResponseWriter, r *http.Request) {
		s.render(w, http.StatusOK, name, s.page(title))
	}
}

// user gives the account of the sign-in cookie, or nil.
func (s *server) user(r *http.Request) *Account {
	c, err := r.Cookie("sbm")
	if err != nil {
		return nil
	}
	return s.store.byTok(c.Value)
}

// sameSite refuses a form that a page of another site sent. The cookie is
// SameSite=Lax too; this is the second guard.
func (s *server) sameSite(w http.ResponseWriter, r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != s.origin {
		http.Error(w, "this form must come from "+s.origin, http.StatusForbidden)
		return false
	}
	return true
}

func (s *server) setCookie(w http.ResponseWriter, token string, age int) {
	http.SetCookie(w, &http.Cookie{Name: "sbm", Value: token, Path: "/", MaxAge: age,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
}

func (s *server) signup(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	email := r.PostFormValue("email")
	p := s.page("Create an account")
	p.Email = email
	if !s.limit.allow("signup "+clientIP(r), 10, time.Hour) {
		p.Error = "Too many new accounts from your address. Try again later."
		s.render(w, http.StatusTooManyRequests, "signup", p)
		return
	}
	a, err := s.store.signup(email, r.PostFormValue("password"))
	if errors.Is(err, errTaken) || errors.Is(err, errEmail) || errors.Is(err, errPassword) {
		p.Error = strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + "."
		s.render(w, http.StatusBadRequest, "signup", p)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	log.Printf("new account %s", a.ID)
	s.startSession(w, r, a)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	email := r.PostFormValue("email")
	p := s.page("Sign in")
	p.Email = email
	if !s.limit.allow("login "+clientIP(r), 20, 15*time.Minute) {
		p.Error = "Too many attempts. Try again later."
		s.render(w, http.StatusTooManyRequests, "login", p)
		return
	}
	a, err := s.store.check(email, r.PostFormValue("password"))
	if err != nil {
		p.Error = "Wrong email or password."
		s.render(w, http.StatusUnauthorized, "login", p)
		return
	}
	s.startSession(w, r, a)
}

func (s *server) startSession(w http.ResponseWriter, r *http.Request, a *Account) {
	token, err := s.store.signIn(a)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setCookie(w, token, 365*24*3600)
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	if c, err := r.Cookie("sbm"); err == nil {
		if err := s.store.signOut(c.Value); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.setCookie(w, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func paid(status string) bool {
	return status == "active" || status == "trialing" || status == "past_due"
}

// active tells if sync works for the account: always without billing, else
// during the free trial or with a subscription.
func (s *server) active(a Account) bool {
	return s.stripe == nil || paid(a.Status) || s.trialLeft(a) > 0
}

// trialLeft gives the time until the free trial ends. The trial starts at
// sign-up, or when billing starts, if that is later: accounts from before
// billing get the full trial too.
func (s *server) trialLeft(a Account) time.Duration {
	start := a.Created
	if s.since.After(start) {
		start = s.since
	}
	return s.trial - time.Since(start)
}

func (s *server) account(w http.ResponseWriter, r *http.Request) {
	u := s.user(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.showAccount(w, http.StatusOK, s.store.view(u), "")
}

func (s *server) showAccount(w http.ResponseWriter, status int, a Account, problem string) {
	p := s.page("Your account")
	p.Email, p.Error, p.Customer = a.Email, problem, a.Customer
	left := s.trialLeft(a)
	switch {
	case s.stripe == nil:
		p.State = "Sync is on."
	case a.Status == "past_due":
		p.State = "Your last payment failed. Update your card in Manage billing."
	case paid(a.Status):
		p.State = "Your subscription is active."
	case left > 0:
		days := int(left.Hours()/24) + 1
		p.State = "Free trial: " + strconv.Itoa(days) + " days left."
		if days == 1 {
			p.State = "Free trial: 1 day left."
		}
	default:
		p.State = "Your free trial has ended. Sync is paused until you subscribe."
	}
	p.CanSubscribe = s.stripe != nil && !paid(a.Status)
	s.render(w, status, "account", p)
}

func (s *server) checkout(w http.ResponseWriter, r *http.Request) {
	u := s.user(r)
	if !s.sameSite(w, r) {
		return
	}
	if u == nil || s.stripe == nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	price := s.prices[r.PostFormValue("plan")]
	if price == "" {
		http.Error(w, "no such plan", http.StatusBadRequest)
		return
	}
	to, err := s.stripe.checkout(s.store.view(u), price, s.site)
	if err != nil {
		log.Printf("checkout for %s: %v", u.ID, err)
		s.showAccount(w, http.StatusBadGateway, s.store.view(u), "Stripe did not answer. Try again later.")
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *server) portal(w http.ResponseWriter, r *http.Request) {
	u := s.user(r)
	if !s.sameSite(w, r) {
		return
	}
	if u == nil || s.stripe == nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	a := s.store.view(u)
	if a.Customer == "" {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	to, err := s.stripe.portal(a.Customer, s.site)
	if err != nil {
		log.Printf("portal for %s: %v", u.ID, err)
		s.showAccount(w, http.StatusBadGateway, a, "Stripe did not answer. Try again later.")
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	u := s.user(r)
	if !s.sameSite(w, r) {
		return
	}
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a := s.store.view(u)
	if _, err := s.store.check(a.Email, r.PostFormValue("password")); err != nil {
		s.showAccount(w, http.StatusUnauthorized, a, "Wrong password. The account is not deleted.")
		return
	}
	// End the subscription first: a deleted account must not pay.
	if a.Customer != "" && s.stripe != nil {
		if err := s.stripe.deleteCustomer(a.Customer); err != nil {
			log.Printf("delete customer of %s: %v", a.ID, err)
			s.showAccount(w, http.StatusBadGateway, a, "Stripe did not answer, so the account is not deleted. Try again later.")
			return
		}
	}
	if err := s.store.remove(u); err != nil {
		s.fail(w, err)
		return
	}
	log.Printf("deleted account %s", a.ID)
	s.setCookie(w, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// webhook takes the events of Stripe that change a subscription.
func (s *server) webhook(w http.ResponseWriter, r *http.Request) {
	if s.stripe == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "cannot read the event", http.StatusBadRequest)
		return
	}
	if err := verify(body, r.Header.Get("Stripe-Signature"), s.stripe.Webhook, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Created int64  `json:"created"`
		Data    struct {
			Object struct {
				Customer  string            `json:"customer"`
				Reference string            `json:"client_reference_id"`
				Status    string            `json:"status"`
				Metadata  map[string]string `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "cannot read the event", http.StatusBadRequest)
		return
	}
	o := ev.Data.Object
	switch ev.Type {
	case "checkout.session.completed":
		if a := s.store.byID(o.Reference); a != nil && o.Customer != "" {
			err = s.store.setCustomer(a, o.Customer)
		}
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		a := s.store.byCustomer(o.Customer)
		if a == nil {
			if a = s.store.byID(o.Metadata["account"]); a != nil {
				err = s.store.setCustomer(a, o.Customer)
			}
		}
		if a != nil && err == nil {
			status := o.Status
			if ev.Type == "customer.subscription.deleted" {
				status = "canceled"
			}
			err = s.store.setStatus(a, status, ev.Created)
			log.Printf("account %s: subscription %s", a.ID, status)
		}
	}
	if err != nil {
		s.fail(w, err) // Stripe sends the event again later
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- API for bm-sync, the app and the add-on ----

// apiUser gives the account of the "Authorization: Bearer <token>" header.
func (s *server) apiUser(r *http.Request) *Account {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil
	}
	return s.store.byTok(strings.TrimSpace(token))
}

// apiLogin takes the form fields email and password and gives a token.
func (s *server) apiLogin(w http.ResponseWriter, r *http.Request) {
	if !s.limit.allow("login "+clientIP(r), 20, 15*time.Minute) {
		http.Error(w, "too many attempts: try again later", http.StatusTooManyRequests)
		return
	}
	a, err := s.store.check(r.PostFormValue("email"), r.PostFormValue("password"))
	if err != nil {
		http.Error(w, "wrong email or password", http.StatusUnauthorized)
		return
	}
	token, err := s.store.signIn(a)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, token+"\n")
}

func (s *server) apiLogout(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err := s.store.signOut(strings.TrimSpace(token)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sync takes the bookmark file of a device as the body, and the version that
// the device got last time as ?base=. It gives the merged file, with the
// name of its version in the Sbm-Version header. The device writes the file
// and sends that name as base next time.
func (s *server) sync(w http.ResponseWriter, r *http.Request) {
	u := s.apiUser(r)
	if u == nil {
		http.Error(w, "not signed in: sign in again", http.StatusUnauthorized)
		return
	}
	if !s.active(s.store.view(u)) {
		http.Error(w, "sbm Sync is paused for this account: see "+s.site+"/account", http.StatusPaymentRequired)
		return
	}
	base := r.URL.Query().Get("base")
	if base != "" && !hashName.MatchString(base) {
		http.Error(w, "base is not a version name", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFile))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		http.Error(w, "the bookmark file is too large", http.StatusRequestEntityTooLarge)
		return
	} else if err != nil {
		http.Error(w, "cannot read the bookmark file", http.StatusBadRequest)
		return
	}
	text, version, err := s.store.sync(u, base, string(body))
	if errors.Is(err, errGone) {
		http.Error(w, "not signed in: sign in again", http.StatusUnauthorized)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Sbm-Version", version)
	w.Header().Set("Access-Control-Expose-Headers", "Sbm-Version")
	io.WriteString(w, text)
}

func (s *server) fail(w http.ResponseWriter, err error) {
	log.Print(err)
	http.Error(w, "server error: try again later", http.StatusInternalServerError)
}

// clientIP gives the address of the client. Behind a proxy on the same
// machine, the proxy gives it in X-Real-IP.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if real := r.Header.Get("X-Real-IP"); real != "" {
			return real
		}
	}
	return host
}

// limiter counts attempts for each key in a sliding window.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *limiter) allow(key string, n int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.hits == nil || len(l.hits) > 10000 {
		l.hits = map[string][]time.Time{}
	}
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < per {
			recent = append(recent, t)
		}
	}
	if len(recent) >= n {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}
