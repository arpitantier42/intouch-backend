-- RSVP and seat confirmation RPCs (setRsvp, verifyRsvpPayment).
--
-- Both lock the event row (FOR NO KEY UPDATE, the same mode the rsvps AFTER
-- trigger takes) before reading confirmed_count, so of two members racing
-- for the last seat exactly one is confirmed and the other gets PT409
-- 'Event full'. The rsvps triggers (events/raises migration) still derive
-- seat_confirmed, payment_status on free events, confirmed_count and the
-- reminder rows; these functions only decide what to write and who may.
--
-- Response shape (_rsvp_json): the rsvps row plus
--   badge            'Going' | 'Payment pending' | 'Verification pending' |
--                    'Not sure' | 'Declined'
--   seats_left       integer, null = unlimited
--   seats_left_label 'N seats left' | 'Event full' | null
--   join_url         the event link, only on a confirmed seat (else null)
--   event            {id, community_id, title, starts_at, ends_at, format,
--                     venue, is_paid, price_minor, payment_url, seat_cap,
--                     confirmed_count}
--   confirmation_email  true when this call should send the confirmation
--                       email (set_rsvp only; false otherwise)
--
-- Declines and "not sure" store an `rsvp_response` notification for the
-- host (in-app bell only, never a push: the host is informed quietly).

create or replace function public._rsvp_badge(p_status text, p_payment text, p_confirmed boolean)
returns text
language sql immutable set search_path = ''
as $$
  select case
    when p_status = 'going' and p_confirmed then 'Going'
    when p_status = 'going' and p_payment = 'verification_pending' then 'Verification pending'
    when p_status = 'going' then 'Payment pending'
    when p_status = 'not_sure' then 'Not sure'
    else 'Declined'
  end
$$;

create or replace function public._rsvp_json(p_rsvp public.rsvps, p_confirmation boolean default false)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select to_jsonb(p_rsvp) || jsonb_build_object(
    'badge', public._rsvp_badge(p_rsvp.status, p_rsvp.payment_status, p_rsvp.seat_confirmed),
    'seats_left', case when e.seat_cap is null then null else greatest(e.seat_cap - e.confirmed_count, 0) end,
    'seats_left_label', case
       when e.seat_cap is null then null
       when e.confirmed_count >= e.seat_cap then 'Event full'
       when e.seat_cap - e.confirmed_count = 1 then '1 seat left'
       else (e.seat_cap - e.confirmed_count)::text || ' seats left' end,
    'join_url', case when p_rsvp.seat_confirmed then e.join_url end,
    'event', jsonb_build_object(
       'id', e.id, 'community_id', e.community_id, 'title', e.title,
       'starts_at', e.starts_at, 'ends_at', e.ends_at, 'format', e.format,
       'venue', e.venue, 'is_paid', e.is_paid, 'price_minor', e.price_minor,
       'payment_url', e.payment_url, 'seat_cap', e.seat_cap,
       'confirmed_count', e.confirmed_count),
    'confirmation_email', coalesce(p_confirmation, false))
  from public.events e where e.id = p_rsvp.event_id
$$;

-- setRsvp. p_payment: null/'none' keeps the current choice; 'pay_later'
-- ("I'll pay later") or 'verification_pending' ("I've paid") on paid events;
-- 'verified' is host-only (PT400). Null reminder flags keep the current
-- value (default on). PT404 unknown/deleted event, PT403 not joined,
-- PT400 bad status/payment/reason, PT409 'Event full'.
create or replace function public.set_rsvp(
  p_event_id uuid,
  p_status text,
  p_payment text default null,
  p_decline_reason text default null,
  p_note text default null,
  p_remind_push boolean default null,
  p_remind_email boolean default null)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_event record;
  v_old public.rsvps;
  v_new public.rsvps;
  v_pay text;
  v_reason text := nullif(btrim(coalesce(p_decline_reason, '')), '');
  v_note text := nullif(btrim(coalesce(p_note, '')), '');
  v_confirm boolean;
begin
  if v_uid is null then
    raise exception 'Sign in to RSVP' using errcode = 'PT401';
  end if;
  if p_status is null or p_status not in ('going', 'not_sure', 'declined') then
    raise exception 'Status must be going, not_sure or declined' using errcode = 'PT400';
  end if;
  if p_payment is not null and p_payment not in ('none', 'pay_later', 'verification_pending', 'verified') then
    raise exception 'Payment must be pay_later or verification_pending' using errcode = 'PT400';
  end if;
  if v_reason is not null and v_reason not in ('schedule', 'not_relevant', 'too_far', 'next_time', 'other') then
    raise exception 'Reason must be schedule, not_relevant, too_far, next_time or other' using errcode = 'PT400';
  end if;

  -- Seat lock: concurrent RSVPs for this event serialise here.
  select e.id, e.community_id, e.creator_id, e.is_paid, e.seat_cap, e.confirmed_count, e.deleted_at
    into v_event
    from public.events e where e.id = p_event_id
    for no key update;
  if not found or v_event.deleted_at is not null then
    raise exception 'Event not found' using errcode = 'PT404';
  end if;
  if not public.is_joined(v_event.community_id) then
    raise exception 'Members only' using errcode = 'PT403';
  end if;

  select * into v_old from public.rsvps r where r.event_id = p_event_id and r.user_id = v_uid;

  if p_payment = 'verified' and coalesce(v_old.payment_status, '') <> 'verified' then
    raise exception 'Only the host can verify a payment' using errcode = 'PT400';
  end if;
  v_pay := case
    when v_old.payment_status = 'verified' then 'verified'
    when p_payment in ('pay_later', 'verification_pending') then p_payment
    else coalesce(v_old.payment_status, 'none') end;
  if p_status <> 'going' and v_pay = 'pay_later' then
    v_pay := 'none';
  end if;

  -- Event full: a member who is not already going cannot take a place
  -- (free seat or held paid seat) once every seat is confirmed.
  if p_status = 'going'
     and coalesce(v_old.status, '') <> 'going'
     and v_event.seat_cap is not null
     and v_event.confirmed_count >= v_event.seat_cap then
    raise exception 'Event full' using errcode = 'PT409';
  end if;

  insert into public.rsvps as r (event_id, user_id, status, payment_status, decline_reason, note,
                                 remind_push, remind_email)
  values (p_event_id, v_uid, p_status, v_pay,
          case when p_status = 'declined' then v_reason end, v_note,
          coalesce(p_remind_push, true), coalesce(p_remind_email, true))
  on conflict on constraint rsvps_event_user_key do update
    set status = excluded.status,
        payment_status = excluded.payment_status,
        decline_reason = excluded.decline_reason,
        note = excluded.note,
        remind_push = coalesce(p_remind_push, r.remind_push),
        remind_email = coalesce(p_remind_email, r.remind_email)
  returning * into v_new;

  -- Confirmation email: opted in, and the caller just became going or just
  -- got a confirmed seat (repeating the same RSVP does not re-send).
  v_confirm := v_new.status = 'going' and v_new.remind_email
               and (v_old.id is null or v_old.status <> 'going'
                    or (v_new.seat_confirmed and not v_old.seat_confirmed));

  -- Quiet host notice (bell only) when a member says no or maybe.
  if v_new.status in ('declined', 'not_sure')
     and v_event.creator_id <> v_uid
     and (v_old.id is null or v_old.status is distinct from v_new.status) then
    perform public.notify(v_event.creator_id, 'rsvp_response',
      jsonb_build_object('event_id', p_event_id, 'user_id', v_uid, 'status', v_new.status));
  end if;

  return public._rsvp_json(v_new, v_confirm);
end;
$$;

-- verifyRsvpPayment: the event's creator (joined) moves a going RSVP on a
-- paid event to verified, which confirms the seat. Idempotent once
-- verified. PT404 unknown event or no RSVP, PT403 not the creator, PT400
-- free event, PT409 RSVP not going or 'Event full'.
create or replace function public.verify_rsvp_payment(p_event_id uuid, p_user_id uuid)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_event record;
  v_rsvp public.rsvps;
begin
  select e.id, e.community_id, e.creator_id, e.is_paid, e.seat_cap, e.confirmed_count, e.deleted_at
    into v_event
    from public.events e where e.id = p_event_id
    for no key update;
  if not found or v_event.deleted_at is not null or not public.is_joined(v_event.community_id) then
    raise exception 'Event not found' using errcode = 'PT404';
  end if;
  if v_event.creator_id is distinct from auth.uid() then
    raise exception 'Only the host can verify payments' using errcode = 'PT403';
  end if;
  if not v_event.is_paid then
    raise exception 'This event is free' using errcode = 'PT400';
  end if;

  select * into v_rsvp from public.rsvps r
   where r.event_id = p_event_id and r.user_id = p_user_id
   for update;
  if not found then
    raise exception 'RSVP not found' using errcode = 'PT404';
  end if;
  if v_rsvp.seat_confirmed then
    return public._rsvp_json(v_rsvp);
  end if;
  if v_rsvp.status <> 'going' then
    raise exception 'This member is not going' using errcode = 'PT409';
  end if;
  if v_event.seat_cap is not null and v_event.confirmed_count >= v_event.seat_cap then
    raise exception 'Event full' using errcode = 'PT409';
  end if;

  update public.rsvps r set payment_status = 'verified'
   where r.id = v_rsvp.id
  returning * into v_rsvp;
  return public._rsvp_json(v_rsvp);
end;
$$;

revoke all on function
  public._rsvp_badge(text, text, boolean),
  public._rsvp_json(public.rsvps, boolean),
  public.set_rsvp(uuid, text, text, text, text, boolean, boolean),
  public.verify_rsvp_payment(uuid, uuid)
  from public, anon, authenticated;

grant execute on function public._rsvp_badge(text, text, boolean) to authenticated, service_role;
grant execute on function public._rsvp_json(public.rsvps, boolean) to service_role;
grant execute on function
  public.set_rsvp(uuid, text, text, text, text, boolean, boolean),
  public.verify_rsvp_payment(uuid, uuid)
  to authenticated, service_role;
