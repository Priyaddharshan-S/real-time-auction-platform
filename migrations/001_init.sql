CREATE TABLE profiles (
    user_id      uuid PRIMARY KEY,           -- = Supabase auth user id
    email        text NOT NULL UNIQUE,
    display_name text NOT NULL,
    role         text NOT NULL DEFAULT 'member' CHECK (role IN ('member','admin')),
    banned       boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE auctions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title         text NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    description   text NOT NULL DEFAULT '',
    image_url     text NOT NULL DEFAULT '',
    start_price   bigint NOT NULL CHECK (start_price >= 0),
    current_price bigint NOT NULL CHECK (current_price >= 0),
    min_increment bigint NOT NULL DEFAULT 1 CHECK (min_increment > 0),
    leader_id     uuid REFERENCES profiles(user_id),
    winner_id     uuid REFERENCES profiles(user_id),
    bid_count     integer NOT NULL DEFAULT 0,
    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('scheduled','active','closed','cancelled')),
    starts_at     timestamptz NOT NULL DEFAULT now(),
    ends_at       timestamptz NOT NULL,
    closed_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);
CREATE INDEX idx_auctions_status_ends ON auctions (status, ends_at);

CREATE TABLE bids (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    auction_id      uuid NOT NULL REFERENCES auctions(id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES profiles(user_id),
    amount          bigint NOT NULL CHECK (amount > 0),
    idempotency_key text NOT NULL,
    voided          boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (auction_id, idempotency_key)     -- retries / double-taps never duplicate
);
CREATE INDEX idx_bids_auction_amount ON bids (auction_id, amount DESC) WHERE NOT voided;
CREATE INDEX idx_bids_auction_created ON bids (auction_id, created_at DESC);
CREATE INDEX idx_bids_user ON bids (user_id);

CREATE TABLE admin_actions (
    id         bigserial PRIMARY KEY,
    admin_id   uuid NOT NULL REFERENCES profiles(user_id),
    action     text NOT NULL,
    auction_id uuid REFERENCES auctions(id) ON DELETE SET NULL,
    details    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_admin_actions_created ON admin_actions (created_at DESC);
