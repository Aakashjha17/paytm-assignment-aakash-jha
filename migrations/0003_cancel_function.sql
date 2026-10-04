-- cancel_reservation releases a held reservation's seats. Only the owner can
-- cancel; anyone else gets 'not_found', the same as a reservation that doesn't
-- exist. Cancelling twice is safe: the second call returns 'already_cancelled'.
--
-- Lock order matches reserve_seats (see 0002): per-user advisory lock, then the
-- reservation row, then its seats in ascending seat_no.
CREATE FUNCTION cancel_reservation(p_id UUID, p_user_id TEXT)
RETURNS TABLE (
    r_outcome TEXT, r_id UUID, r_show_id BIGINT, r_user_id TEXT, r_status TEXT,
    r_seats TEXT[], r_amount_paise BIGINT, r_expires_at TIMESTAMPTZ, r_created_at TIMESTAMPTZ, r_detail TEXT[]
)
LANGUAGE plpgsql AS $$
DECLARE
    v_res  reservations%ROWTYPE;
    v_seat RECORD;
BEGIN
    SELECT * INTO v_res FROM reservations WHERE id = p_id AND user_id = p_user_id;
    IF NOT FOUND THEN
        r_outcome := 'not_found';
        RETURN NEXT;
        RETURN;
    END IF;

    PERFORM pg_advisory_xact_lock(user_lock_key(v_res.show_id, p_user_id));

    -- Re-read under the lock: a concurrent cancel may have just finished.
    SELECT * INTO v_res FROM reservations WHERE id = p_id FOR UPDATE;

    IF v_res.status = 'cancelled' THEN
        RETURN QUERY SELECT * FROM reservation_result('already_cancelled', p_id);
        RETURN;
    END IF;
    IF v_res.status = 'confirmed' THEN
        RETURN QUERY SELECT * FROM reservation_result('not_cancellable', p_id);
        RETURN;
    END IF;
    IF v_res.expires_at <= now() THEN
        -- Its seats are already free (or already re-sold); there is nothing to
        -- release and touching them could clobber the new holder.
        RETURN QUERY SELECT * FROM reservation_result('expired', p_id);
        RETURN;
    END IF;

    -- Lock this reservation's seats in seat_no order before releasing them, the
    -- same order reserve_seats uses, so a reserve storming one of these seats
    -- can't deadlock with us.
    FOR v_seat IN
        SELECT s.seat_no FROM seats s
         WHERE s.show_id = v_res.show_id AND s.reservation_id = p_id
         ORDER BY s.seat_no
           FOR UPDATE
    LOOP
    END LOOP;

    UPDATE seats
       SET status = 'available', user_id = NULL, reservation_id = NULL, held_until = NULL
     WHERE show_id = v_res.show_id AND reservation_id = p_id AND status = 'held';

    UPDATE reservations SET status = 'cancelled', updated_at = now() WHERE id = p_id;

    RETURN QUERY SELECT * FROM reservation_result('cancelled', p_id);
END
$$;
