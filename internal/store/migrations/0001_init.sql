-- One row per address that ever asked to subscribe. The address is the identity: it is
-- stored lower-cased, so the same mailbox typed twice is one row.
CREATE TABLE subscribers (
    id                 bigserial PRIMARY KEY,
    email              text        NOT NULL UNIQUE,
    status             text        NOT NULL CHECK (status IN ('pending', 'confirmed', 'unsubscribed')),
    -- SHA-256 of the token in the confirmation link. Only the hash is kept, so a copy of the
    -- database cannot confirm anybody. It stays after confirmation, so a second click on the
    -- same link still lands on the confirmed page instead of an error.
    confirm_token_hash bytea       UNIQUE,
    confirm_sent_at    timestamptz,
    -- The token in every unsubscribe link and List-Unsubscribe header. Kept in clear because
    -- every issue has to print it again; all it can do is unsubscribe its own address.
    unsubscribe_token  text        NOT NULL UNIQUE,
    -- Where the row came from: `form` for the site's form, `kit` for the import at cutover.
    source             text        NOT NULL DEFAULT 'form',
    created_at         timestamptz NOT NULL DEFAULT now(),
    -- The proof of consent: when the address clicked the confirmation link.
    confirmed_at       timestamptz,
    unsubscribed_at    timestamptz
);

CREATE INDEX subscribers_status ON subscribers (status);

-- One row per newsletter issue, keyed by the site's slug (`2026-11`). The HTML is the issue as
-- the site rendered it, with `{{ unsubscribe_url }}` where each recipient's link goes.
CREATE TABLE issues (
    slug              text        PRIMARY KEY,
    subject           text        NOT NULL,
    html              text        NOT NULL,
    status            text        NOT NULL CHECK (status IN ('draft', 'sending', 'sent')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    -- The last test send. The real send is refused unless it is newer than the last change.
    test_sent_at      timestamptz,
    sent_at           timestamptz,
    recipients        integer,
    accepted          integer,
    rejected          integer,
    kannon_message_id text,
    last_error        text
);
