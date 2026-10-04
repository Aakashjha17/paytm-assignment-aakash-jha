-- Shows and their seats. Reservation/idempotency tables arrive with the
-- reserve function in 0002.

CREATE TABLE shows (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name             TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    total_seats      INTEGER     NOT NULL CHECK (total_seats > 0),
    price_paise      BIGINT      NOT NULL CHECK (price_paise >= 0),
    per_user_limit   INTEGER     NOT NULL CHECK (per_user_limit > 0),
    hold_ttl_seconds INTEGER     NOT NULL CHECK (hold_ttl_seconds > 0),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per physical seat, created up front with the show, so every seat
-- always has exactly one status and available + held + confirmed == total_seats
-- is a property of the table rather than something we have to maintain.
CREATE TABLE seats (
    show_id        BIGINT      NOT NULL REFERENCES shows (id),
    seat_no        INTEGER     NOT NULL CHECK (seat_no > 0),
    label          TEXT        NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'available',
    user_id        TEXT,
    reservation_id UUID,
    held_until     TIMESTAMPTZ,
    PRIMARY KEY (show_id, seat_no),
    UNIQUE (show_id, label),
    -- The seat state machine, enforced by the database: an available seat has
    -- no owner, a held seat has an owner and an expiry, a confirmed seat has an
    -- owner. No code path can write a half-transitioned row.
    CONSTRAINT seat_state CHECK (
        (status = 'available' AND user_id IS NULL     AND reservation_id IS NULL     AND held_until IS NULL)
     OR (status = 'held'      AND user_id IS NOT NULL AND reservation_id IS NOT NULL AND held_until IS NOT NULL)
     OR (status = 'confirmed' AND user_id IS NOT NULL AND reservation_id IS NOT NULL)
    )
);
