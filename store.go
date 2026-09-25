package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	maxTokens   = 20 // sign-ins kept per account; the oldest goes first
	maxVersions = 50 // versions of the bookmark file kept per account
)

var (
	errTaken    = errors.New("an account with this email exists already")
	errEmail    = errors.New("this is not an email address")
	errPassword = errors.New("the password must have 8 to 72 characters")
	errWrong    = errors.New("wrong email or password")
	errGone     = errors.New("the account does not exist")
	errCode     = errors.New("no account has this code")
	hashName    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Account is one user. The file of the user is in files/<ID>.
type Account struct {
	ID       string       `json:"id"`
	Email    string       `json:"email"`
	Hash     string       `json:"hash"` // bcrypt hash of the password
	Created  time.Time    `json:"created"`
	Tokens   []string     `json:"tokens"` // SHA-256 of each sign-in token
	Customer string       `json:"customer,omitempty"`
	Status   string       `json:"status,omitempty"`    // of the Stripe subscription
	StatusAt int64        `json:"status_at,omitempty"` // time of the Stripe event that set Status
	Verify   string       `json:"verify,omitempty"`    // SHA-256 of the code that confirms the email; empty when confirmed
	Feed     feedSettings `json:"feed,omitzero"`       // the feed choices, see feed.go
}

// Store keeps the accounts in one JSON file, accounts.json, and the versions
// of each bookmark file in files/<account ID>/<SHA-256 of the text>. The file
// HEAD beside the versions names the current one.
type Store struct {
	dir      string
	mu       sync.Mutex // for the maps, the accounts and accounts.json
	accounts map[string]*Account
	byEmail  map[string]*Account
	byToken  map[string]*Account
	locks    map[string]*sync.Mutex // one for the files of each account
}

func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		dir:      dir,
		accounts: map[string]*Account{},
		byEmail:  map[string]*Account{},
		byToken:  map[string]*Account{},
		locks:    map[string]*sync.Mutex{},
	}
	b, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	} else if err != nil {
		return nil, err
	}
	var list []*Account
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, a := range list {
		s.index(a)
	}
	return s, nil
}

func (s *Store) index(a *Account) {
	s.accounts[a.ID] = a
	s.byEmail[a.Email] = a
	for _, t := range a.Tokens {
		s.byToken[t] = a
	}
}

// save writes accounts.json. The caller holds s.mu.
func (s *Store) save() error {
	list := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	b, err := json.MarshalIndent(list, "", "\t")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(s.dir, "accounts.json"), b)
}

// writeFile replaces a file in one step: a crash leaves the old file or the
// new one, never a part of one.
func writeFile(name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, name)
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported systems
	}
	return hex.EncodeToString(b)
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// cleanEmail gives the email address in the form that the store keeps.
func cleanEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	a, err := mail.ParseAddress(email)
	if err != nil || a.Name != "" || a.Address != email || len(email) > 254 {
		return "", errEmail
	}
	return email, nil
}

// signup makes an account. With a code, the email of the account is not
// confirmed until someone opens the link with that code.
func (s *Store) signup(email, password, code string) (*Account, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return nil, err
	}
	if len(password) < 8 || len(password) > 72 {
		return nil, errPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byEmail[email] != nil {
		return nil, errTaken
	}
	a := &Account{ID: random(16), Email: email, Hash: string(hash), Created: time.Now().UTC()}
	if code != "" {
		a.Verify = digest(code)
	}
	s.index(a)
	if err := s.save(); err != nil {
		delete(s.accounts, a.ID)
		delete(s.byEmail, a.Email)
		return nil, err
	}
	return a, nil
}

// dummy is compared when the email is unknown, so that a wrong email takes
// as long as a wrong password.
var dummy, _ = bcrypt.GenerateFromPassword([]byte("sbm-sync dummy password"), bcrypt.DefaultCost)

func (s *Store) check(email, password string) (*Account, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	s.mu.Lock()
	a := s.byEmail[email]
	hash := dummy
	if a != nil {
		hash = []byte(a.Hash)
	}
	s.mu.Unlock()
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || a == nil {
		return nil, errWrong
	}
	return a, nil
}

// signIn gives a new token for the account. Only its hash is kept.
func (s *Store) signIn(a *Account) (string, error) {
	token := random(32)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts[a.ID] == nil {
		return "", errGone
	}
	a.Tokens = append(a.Tokens, digest(token))
	s.byToken[digest(token)] = a
	for len(a.Tokens) > maxTokens {
		delete(s.byToken, a.Tokens[0])
		a.Tokens = a.Tokens[1:]
	}
	return token, s.save()
}

func (s *Store) signOut(token string) error {
	h := digest(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byToken[h]
	if a == nil {
		return nil
	}
	delete(s.byToken, h)
	for i, t := range a.Tokens {
		if t == h {
			a.Tokens = append(a.Tokens[:i], a.Tokens[i+1:]...)
			break
		}
	}
	return s.save()
}

func (s *Store) byTok(token string) *Account {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byToken[digest(token)]
}

func (s *Store) byID(id string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts[id]
}

func (s *Store) byCustomer(customer string) *Account {
	if customer == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.Customer == customer {
			return a
		}
	}
	return nil
}

// view gives a copy of the account, safe to read without the lock.
func (s *Store) view(a *Account) Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *a
	c.Tokens = nil
	return c
}

// newCode gives a new code that confirms the email of the account. It
// replaces the old code. Only its hash is kept.
func (s *Store) newCode(a *Account) (string, error) {
	code := random(16)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts[a.ID] == nil {
		return "", errGone
	}
	a.Verify = digest(code)
	return code, s.save()
}

// confirm marks the email of the account with the code as confirmed.
func (s *Store) confirm(code string) (*Account, error) {
	if code == "" {
		return nil, errCode
	}
	h := digest(code)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.Verify == h {
			a.Verify = ""
			return a, s.save()
		}
	}
	return nil, errCode
}

func (s *Store) setCustomer(a *Account, customer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.Customer = customer
	return s.save()
}

// setStatus records the state of the subscription, unless the store has a
// newer event already: Stripe does not send events in order.
func (s *Store) setStatus(a *Account, status string, at int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at < a.StatusAt {
		return nil
	}
	a.Status, a.StatusAt = status, at
	return s.save()
}

func (s *Store) lock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	return m
}

// remove deletes the account, all versions of its file and its feeds.
func (s *Store) remove(a *Account) error {
	m := s.lock(a.ID)
	m.Lock()
	defer m.Unlock()
	s.mu.Lock()
	delete(s.accounts, a.ID)
	delete(s.byEmail, a.Email)
	for _, t := range a.Tokens {
		delete(s.byToken, t)
	}
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.dir, "files", a.ID)); err != nil {
		return err
	}
	return os.RemoveAll(s.feedDir(a.ID))
}

// sync merges the file that a device sends into the current version, keeps
// the result as the new current version and gives it with its name. base
// names the version that the device got last time, or is "" for none.
func (s *Store) sync(a *Account, base, local string) (string, string, error) {
	m := s.lock(a.ID)
	m.Lock()
	defer m.Unlock()
	if s.byID(a.ID) == nil {
		return "", "", errGone
	}
	dir := filepath.Join(s.dir, "files", a.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	remote, _, err := head(dir)
	if err != nil {
		return "", "", err
	}
	old := ""
	if hashName.MatchString(base) {
		if b, err := os.ReadFile(filepath.Join(dir, base)); err == nil {
			old = string(b)
		}
	}
	merged := merge(old, local, remote)
	name := digest(merged)
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		now := time.Now()
		err = os.Chtimes(path, now, now) // the newest versions are kept
		if err != nil {
			return "", "", err
		}
	} else if err := writeFile(path, []byte(merged)); err != nil {
		return "", "", err
	}
	if err := writeFile(filepath.Join(dir, "HEAD"), []byte(name+"\n")); err != nil {
		return "", "", err
	}
	return merged, name, prune(dir, name)
}

// head gives the current version of the file in dir and its name. Without a
// version, both are "". The caller holds the lock of the account.
func head(dir string) (string, string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	} else if err != nil {
		return "", "", err
	}
	name := strings.TrimSpace(string(b))
	text, err := os.ReadFile(filepath.Join(dir, name))
	return string(text), name, err
}

// read gives the version of the file of the account with the name, or the
// current version when name is "", and the name of the version.
func (s *Store) read(a *Account, name string) (string, string, error) {
	m := s.lock(a.ID)
	m.Lock()
	defer m.Unlock()
	dir := filepath.Join(s.dir, "files", a.ID)
	if name == "" {
		return head(dir)
	}
	if !hashName.MatchString(name) {
		return "", "", fs.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	return string(b), name, err
}

// prune deletes all but the newest versions in dir, and never keep.
func prune(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type version struct {
		name string
		time time.Time
	}
	var vs []version
	for _, e := range entries {
		if !hashName.MatchString(e.Name()) || e.Name() == keep {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		vs = append(vs, version{e.Name(), info.ModTime()})
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].time.After(vs[j].time) })
	for i := maxVersions - 1; i < len(vs); i++ {
		if err := os.Remove(filepath.Join(dir, vs[i].name)); err != nil {
			return err
		}
	}
	return nil
}
