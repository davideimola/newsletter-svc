# newsletter-svc

The subscribers of [davideimola.dev](https://davideimola.dev)'s newsletter, and the send of each
issue, without a tracking pixel and without tracked links.

It replaces Kit, which cannot send an email without a per-subscriber pixel. The Italian DPA's
guidelines of 17 April 2026 make such a pixel a matter of consent. The why, and the whole
migration, is
[davideimola/davideimola.dev#132](https://github.com/davideimola/davideimola.dev/issues/132).

```
davideimola.dev (Vercel) --API_KEY--> newsletter-svc --> Postgres (subscribers, issues)
                                            |
reader's mail client --/confirm----------->|
                     --/unsubscribe------->|
owner (Google) --------/admin------------->|
                                            v
                                         Kannon (tracking off) --> SMTP
```

The service owns the list. [Kannon](https://github.com/kannon-email/kannon) only sends: it
keeps no list, and every call from here states tracking `off` for opens and links, on top of
the Domain's own `off`.

## The rules

They live in `internal/newsletter`, and its tests are the specification.

- **Double opt-in.** The form creates a `pending` address and mails a confirmation link. The
  address is `confirmed` only when the link is clicked, and the click's time is the proof of
  consent.
- **The form reveals nothing.** A new, waiting, confirmed or unsubscribed address all get the
  same answer.
- **Cooldown.** One confirmation email per address every 10 minutes, so the form cannot be
  used to flood a mailbox.
- **Expiry.** A confirmation link works for 7 days. An address that never confirms is deleted
  after 30 days.
- **Tokens.** Every token is 32 random bytes, never derived from the address, and no URL
  carries the address. Only the confirmation token's hash is stored.
- **Unsubscribe in one click.** Every issue carries `List-Unsubscribe` and
  `List-Unsubscribe-Post` (RFC 8058). The POST unsubscribes with no further step. The link
  in the body opens a page with one button, because mail scanners open every link and a GET
  must not unsubscribe anyone.
- **Unsubscribed means never again,** unless the address subscribes, and confirms, again.
- **Deletion on request** is a button on the subscriber's admin page, or
  `newsletter-svc erase EMAIL`. Nothing about the address is left.
- **An issue goes out once.** The API can save an issue and test-send it to the owner, but
  only the admin page sends it to the list. The admin page asks for the issue's slug to be
  typed, and only after a test of its latest version.
- **A send whose outcome is unknown is not retried.** If Kannon refuses, nothing went out and
  the issue goes back to draft. If the connection drops, the issue stays `sending` with the
  error shown, because trying again could mail everybody twice.

## HTTP

| Route | Who | What |
|---|---|---|
| `POST /api/subscribers` `{"email"}` | website, `Authorization: Bearer API_KEY` | start the double opt-in; `202` whatever the address's state, `400` for an invalid address |
| `PUT /api/issues/{slug}` `{"subject","html"}` | website | save or replace a draft; the HTML must contain `{{ unsubscribe_url }}` |
| `POST /api/issues/{slug}/test` | website | send the draft to `TEST_RECIPIENT` |
| `GET /confirm?token=` | reader | confirm, then redirect to `SITE_URL/newsletter/confirmed` |
| `GET /unsubscribe?token=` | reader | page with an Unsubscribe button |
| `POST /unsubscribe?token=` | reader, mail client | unsubscribe; `List-Unsubscribe=One-Click` is the RFC 8058 form |
| `/admin` | owner, signed in with Google | counts, subscribers, issues, preview, test, send, delete |
| `GET /healthz` | cluster | `200` when the database answers |

The issue HTML is the website's to render, footer included. Where each reader's unsubscribe link
goes, it writes `{{ unsubscribe_url }}`, and Kannon fills it in per recipient.

## Configuration

Environment variables, all required except `LISTEN_ADDR`.

| Variable | Example |
|---|---|
| `LISTEN_ADDR` | `:8080` |
| `DATABASE_URL` | `postgres://...` |
| `PUBLIC_URL` | `https://news.davideimola.dev`, this service's origin |
| `SITE_URL` | `https://davideimola.dev` |
| `API_KEY` | what the website sends as a bearer token, at least 32 characters |
| `KANNON_URL` | `http://kannon.kannon.svc:50051` |
| `KANNON_DOMAIN` | `news.davideimola.dev` |
| `KANNON_API_KEY` | a Kannon API key for that Domain |
| `SENDER_EMAIL`, `SENDER_NAME` | `hello@davideimola.dev`, `Davide Imola` |
| `TEST_RECIPIENT` | where test sends go |
| `OWNER_EMAIL` | the one Google account allowed into `/admin` |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | an OAuth client whose redirect URI is `PUBLIC_URL/auth/callback` |
| `SESSION_KEY` | signs the admin session cookie, at least 32 characters |

## Commands

```sh
newsletter-svc              # serve
newsletter-svc migrate      # apply the database migrations, then exit
newsletter-svc erase EMAIL  # delete one address and everything stored about it
newsletter-svc import-kit < export.csv
                            # add Kit's active subscribers as confirmed, once, at cutover
```

## Development

```sh
docker compose up -d   # the Postgres the tests use, on 127.0.0.1:55432
go test ./...
```

Every push to `main` publishes `ghcr.io/davideimola/newsletter-svc:main-<sha>-<timestamp>`,
and the cluster rolls forward to it.
