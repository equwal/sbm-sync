# sbm-sync

sbm-sync keeps your [sbm](https://github.com/equwal/sbm) bookmark file the
same on each of your devices: bm on your computers, the sbm app for Android
and the sbm add-on for Firefox and Chrome.

It is one small Go program. It keeps its data in plain files: no database.
The only dependency is `golang.org/x/crypto` for bcrypt.

The clients use **https://sbm.subread.space** unless you set another
server. That server is free for 30 days, then $3 a month or $30 a year.
Run your own server free of charge.

## How sync works

A device sends its whole bookmark file and the name of the version that it
got last time. The server merges the changes of the device into its own
copy, keeps the result and sends it back. The device writes the result to
its file and keeps the new version name.

The merge compares whole lines:

- A line that the device removed since its last version goes.
- A line that the device added comes in, at the end of the file, where bm
  adds bookmarks. When the server has another line for the same bookmark
  (the same URL, as bm compares URLs), the line of the device takes its
  place. So an edit on a device wins.
- Lines that other devices added stay.

When the server does not know the last version of a device (for its first
sync), nothing is removed: the result is the union of both files.

## Protocol

Plain HTTP, for any client with curl.

    POST /api/login        form fields email, password
                           200: a token, as text
    POST /api/sync?base=V  Authorization: Bearer <token>
                           body: the bookmark file
                           200: the merged file; header Sbm-Version: <new V>
    POST /api/logout       Authorization: Bearer <token>

Errors come as text: 401 (sign in again), 402 (the subscription or the
trial has ended), 413 (the file is larger than 4 MB), 429 (too many sign-in
attempts).

    curl -d email=me@example.org --data-urlencode password=... https://sbm.subread.space/api/login
    curl -H "Authorization: Bearer $token" --data-binary @bookmarks \
        "https://sbm.subread.space/api/sync?base=$version"

## Run your own server

    go build
    SBM_URL=https://sbm.example.org ./sbm-sync

Then put it behind a web server that does TLS. `contrib/` has a systemd unit
and an nginx site. With Go installed elsewhere:

    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build

Settings, all through the environment:

    SBM_URL         public address of the server (default http://localhost:8750)
    SBM_ADDR        address to listen on (default 127.0.0.1:8750)
    SBM_DATA        data directory (default ./data)
    SBM_CONTACT     email address on the privacy page (optional)
    SBM_TRIAL_DAYS  days of sync before payment, with billing on (default 30)
    SBM_BILLING_START  date when billing starts, as 2026-10-01: accounts
                    from before it get the full trial from that date

Accounts need no email check. To reset a password, the operator deletes the
account in `accounts.json` (with the server stopped), and the user signs up
again. The bookmark files stay on the devices.

## Billing

Billing is off unless `STRIPE_SECRET_KEY` is set. Without billing, sync
is free for all accounts. To turn it on:

1. In Stripe, make a product with two recurring prices, for example $3 a
   month and $30 a year.
2. Add a webhook endpoint `https://<your server>/stripe` for the events
   `checkout.session.completed`, `customer.subscription.created`,
   `customer.subscription.updated` and `customer.subscription.deleted`.
3. Turn on the customer portal in the Stripe settings, so that customers
   can cancel and change their card.
4. Set these variables, then restart the server:

        STRIPE_SECRET_KEY      sk_test_... or sk_live_...
        STRIPE_WEBHOOK_SECRET  whsec_... of the endpoint
        STRIPE_PRICE_MONTH     price_... of the monthly price
        STRIPE_PRICE_YEAR      price_... of the yearly price

Keep the keys out of git: put them in the environment file of the service.

## Data

    data/accounts.json            accounts: email, bcrypt hash, token hashes,
                                  Stripe customer and subscription state
    data/files/<account>/<sha256> the last 50 versions of each bookmark file
    data/files/<account>/HEAD     name of the current version

To back up, copy the directory.

## Test

    go vet ./... && go test ./...

The merge has property tests (with [rapid](https://github.com/flyingmutant/rapid)).

## License

AGPL-3.0. If you run a changed version for other people, give them its
source code.

If sbm is useful to you, you can support it on [Ko-fi](https://ko-fi.com/truex).
