-- Reservations, idempotency, and the reserve decision.
--
-- Lock order. Every write path takes its locks in the same global order, which
-- is what makes deadlock impossible rather than merely unlikely:
--   1. the idempotency key (unique index on reservations(user_id, idempotency_key))
--   2. the per-(show, user) advisory lock
--   3. seat rows, FOR UPDATE, in ascending seat_no
-- cancel_reservation (0003) takes 2 then 3, never 1, so it fits the same order.

CREATE TABLE reservations (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    show_id         BIGINT      NOT NULL REFERENCES shows (id),
    user_id         TEXT        NOT NULL,
    idempotency_key TEXT        NOT NULL,
    -- sha256 of the canonical request (show + sorted seats); a retry with the
    -- same key must match it, otherwise the key is being reused for a
    -- different request.
    fingerprint     TEXT        NOT NULL,
    seat_labels     TEXT[]      NOT NULL CHECK (cardinality(seat_labels) > 0),
    amount_paise    BIGINT      NOT NULL CHECK (amount_paise >= 0),
    -- 'held' past expires_at is reported as 'expired'; see effective_status.
    status          TEXT        NOT NULL CHECK (status IN ('held', 'confirmed', 'cancelled')),
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Exactly-once: one reservation per (user, key), ever. Concurrent inserts
    -- of the same pair serialise on this index.
    CONSTRAINT reservations_idempotency UNIQUE (user_id, idempotency_key)
);

ALTER TABLE seats
    ADD CONSTRAINT seats_reservation_fk FOREIGN KEY (reservation_id) REFERENCES reservations (id);

-- Per-user limit check, and cancel finding a reservation's seats.
CREATE INDEX seats_show_user ON seats (show_id, user_id) WHERE user_id IS NOT NULL;
CREATE INDEX seats_reservation ON seats (reservation_id) WHERE reservation_id IS NOT NULL;

-- Holds expire lazily: nothing has to run for a hold to lapse. Every read and
-- every decision compares held_until / expires_at with now().
CREATE FUNCTION effective_status(p_status TEXT, p_expires_at TIMESTAMPTZ) RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT CASE WHEN p_status = 'held' AND p_expires_at <= now() THEN 'expired' ELSE p_status END
$$;

-- The row shape both reserve and cancel return.
CREATE FUNCTION reservation_result(p_outcome TEXT, p_id UUID)
RETURNS TABLE (
    r_outcome TEXT, r_id UUID, r_show_id BIGINT, r_user_id TEXT, r_status TEXT,
    r_seats TEXT[], r_amount_paise BIGINT, r_expires_at TIMESTAMPTZ, r_created_at TIMESTAMPTZ, r_detail TEXT[]
)
LANGUAGE sql AS $$
    SELECT p_outcome, r.id, r.show_id, r.user_id, effective_status(r.status, r.expires_at),
           r.seat_labels, r.amount_paise, r.expires_at, r.created_at, NULL::TEXT[]
    FROM reservations r WHERE r.id = p_id
$$;

-- Hash of (show, user) for the per-user advisory lock. Shared with cancel.
CREATE FUNCTION user_lock_key(p_show_id BIGINT, p_user_id TEXT) RETURNS BIGINT
LANGUAGE sql IMMUTABLE AS $$
    SELECT hashtextextended('user-seats:' || p_show_id || ':' || p_user_id, 0)
$$;

-- reserve_seats holds p_labels for p_user_id, all or nothing, and returns one
-- row whose r_outcome is a booking.Outcome. It runs as a single statement, so
-- the whole decision is one transaction.
--
-- p_labels must be canonical (unique, upper-case); the caller validates.
CREATE FUNCTION reserve_seats(
    p_show_id BIGINT, p_user_id TEXT, p_key TEXT, p_fingerprint TEXT, p_labels TEXT[]
)
RETURNS TABLE (
    r_outcome TEXT, r_id UUID, r_show_id BIGINT, r_user_id TEXT, r_status TEXT,
    r_seats TEXT[], r_amount_paise BIGINT, r_expires_at TIMESTAMPTZ, r_created_at TIMESTAMPTZ, r_detail TEXT[]
)
LANGUAGE plpgsql AS $$
DECLARE
    v_show     shows%ROWTYPE;
    v_existing reservations%ROWTYPE;
    v_id       UUID;
    v_expires  TIMESTAMPTZ;
    v_n        INT := cardinality(p_labels);
    v_live     INT;
    v_seat     RECORD;
    v_found    TEXT[] := '{}';
    v_taken    TEXT[] := '{}';
BEGIN
    SELECT * INTO v_show FROM shows WHERE id = p_show_id;
    IF NOT FOUND THEN
        r_outcome := 'show_not_found';
        RETURN NEXT;
        RETURN;
    END IF;
    v_expires := now() + make_interval(secs => v_show.hold_ttl_seconds);

    -- 1. Claim the idempotency key. A concurrent request with the same
    --    (user, key) blocks here until we commit or roll back. If we commit, it
    --    sees our row and replays; if this attempt is declined, the row is
    --    deleted below before commit, so the other request just runs normally.
    INSERT INTO reservations (show_id, user_id, idempotency_key, fingerprint, seat_labels,
                              amount_paise, status, expires_at)
    VALUES (p_show_id, p_user_id, p_key, p_fingerprint, p_labels,
            v_show.price_paise * v_n, 'held', v_expires)
    ON CONFLICT (user_id, idempotency_key) DO NOTHING
    RETURNING id INTO v_id;

    IF v_id IS NULL THEN
        SELECT * INTO v_existing FROM reservations
         WHERE user_id = p_user_id AND idempotency_key = p_key;
        IF NOT FOUND THEN
            -- Can't happen: only committed rows cause a conflict, and committed
            -- reservations are never deleted. Ask the caller to retry rather than guess.
            RAISE EXCEPTION 'idempotency row disappeared' USING ERRCODE = 'serialization_failure';
        END IF;
        IF v_existing.fingerprint <> p_fingerprint THEN
            r_outcome := 'idempotency_conflict';
            RETURN NEXT;
            RETURN;
        END IF;
        RETURN QUERY SELECT * FROM reservation_result('replayed', v_existing.id);
        RETURN;
    END IF;

    -- 2. Serialise this user's reservations on this show. Under this lock the
    --    count below can't go stale: only this user's transactions add seats
    --    for this user, and they all queue on the same lock.
    PERFORM pg_advisory_xact_lock(user_lock_key(p_show_id, p_user_id));

    SELECT count(*) INTO v_live FROM seats s
     WHERE s.show_id = p_show_id AND s.user_id = p_user_id
       AND (s.status = 'confirmed' OR (s.status = 'held' AND s.held_until > now()));

    IF v_live + v_n > v_show.per_user_limit THEN
        DELETE FROM reservations WHERE id = v_id;
        r_outcome := 'per_user_limit';
        r_detail := ARRAY['held=' || v_live, 'requested=' || v_n, 'limit=' || v_show.per_user_limit];
        RETURN NEXT;
        RETURN;
    END IF;

    -- 3. Lock the requested seats in seat_no order and only then decide. The
    --    ORDER BY ... FOR UPDATE locks rows in that order, so two multi-seat
    --    requests that overlap always contend on the lowest shared seat first
    --    and can never each hold a seat the other is waiting for. After a wait,
    --    Postgres hands us the row as the previous holder committed it, so the
    --    status we test here is current, not a stale read.
    FOR v_seat IN
        SELECT s.label, s.status, s.held_until
          FROM seats s
         WHERE s.show_id = p_show_id AND s.label = ANY (p_labels)
         ORDER BY s.seat_no
           FOR UPDATE
    LOOP
        v_found := v_found || v_seat.label;
        IF NOT (v_seat.status = 'available'
                OR (v_seat.status = 'held' AND v_seat.held_until <= now())) THEN
            v_taken := v_taken || v_seat.label;
        END IF;
    END LOOP;

    IF cardinality(v_found) < v_n THEN
        DELETE FROM reservations WHERE id = v_id;
        r_outcome := 'seat_not_found';
        r_detail := ARRAY(SELECT l FROM unnest(p_labels) AS l WHERE l <> ALL (v_found) ORDER BY l);
        RETURN NEXT;
        RETURN;
    END IF;

    IF cardinality(v_taken) > 0 THEN
        DELETE FROM reservations WHERE id = v_id;
        r_outcome := 'seat_taken';
        r_detail := v_taken;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Every requested seat is locked by us and free: take them all.
    UPDATE seats
       SET status = 'held', user_id = p_user_id, reservation_id = v_id, held_until = v_expires
     WHERE show_id = p_show_id AND label = ANY (p_labels);

    RETURN QUERY SELECT * FROM reservation_result('reserved', v_id);
END
$$;
