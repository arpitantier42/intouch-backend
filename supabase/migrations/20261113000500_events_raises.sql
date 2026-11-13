-- Events, RSVPs, reminders, raises and contributions.
--
-- Events: community-scoped; visible to joined members while not deleted.
-- Inserts go through the create-event RPC (a later ticket; it also posts to
-- the feed). The creator may update the client-writable columns. CHECKs carry
-- the publish rules: non-blank title, ends after it starts, a venue for
-- in-person and an http(s) join link for online, seat_cap > 0, and a paid
-- event needs price_minor > 0 and an http(s) payment_url (a free one has
-- neither). join_url is not column-granted to clients: it is read through
-- event_join_url(id), which returns it to the creator and to confirmed seats.
--
-- RSVPs: one per (event, user); written only by the RSVP RPCs (service or
-- SECURITY DEFINER). A trigger locks the event row, derives seat_confirmed
-- (going and free, or going and payment verified), keeps
-- events.confirmed_count in step and raises PT409 'Event full' when the seat
-- cap is reached; the CHECK confirmed_count <= seat_cap is the backstop. So of
-- two RSVPs racing for the last seat exactly one is confirmed.
-- A going RSVP schedules reminders: push 1 day and 1 hour before start when
-- remind_push, email on the morning of the event (08:00 IST, or 1 hour before
-- start if that is earlier) when remind_email. Turning a preference off or
-- leaving 'going' removes the pending ones. The confirmation email is sent by
-- the service at RSVP time, not scheduled here.
--
-- Raises: no client access to the base table (restrictive deny policy and no
-- grants). Clients read public.raises_masked, which returns the raises of
-- communities they are joined to and nulls upi_id, account_number, ifsc and
-- qr_url unless the caller is the organiser or a community owner/admin;
-- account_last4 (trigger-derived) is the only payout detail members see.
-- Contributions: unique gateway_payment_id; capture_contribution() applies a
-- captured gateway payment once, bumping raised_minor/donor_count once.

-- ---------------------------------------------------------------------------
-- Helpers used by CHECKs
-- ---------------------------------------------------------------------------

create or replace function public._is_http_url(p_url text)
returns boolean
language sql immutable set search_path = ''
as $$
  select p_url ~* '^https?://[^\s/?#]+[^\s]*$'
$$;

create or replace function public._is_string_array(p_value jsonb)
returns boolean
language sql immutable set search_path = ''
as $$
  select jsonb_typeof(p_value) = 'array'
     and not exists (select 1 from jsonb_array_elements(p_value) e where jsonb_typeof(e) <> 'string')
$$;

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

create table public.events (
    id               uuid primary key default gen_random_uuid(),
    community_id     uuid not null references public.communities (id) on delete cascade,
    creator_id       uuid not null references public.profiles (id) on delete cascade,
    title            varchar(120) not null
        constraint events_title_not_blank check (btrim(title) <> ''),
    description      varchar(4000),
    cover_url        text,
    starts_at        timestamptz not null,
    ends_at          timestamptz not null,
    format           text not null
        constraint events_format_check check (format in ('in_person', 'online')),
    venue            varchar(200),
    join_url         text
        constraint events_join_url_http check (join_url is null or public._is_http_url(join_url)),
    tags             jsonb not null default '[]'::jsonb
        constraint events_tags_array check (public._is_string_array(tags)),
    speaker          varchar(120),
    seat_cap         integer
        constraint events_seat_cap_positive check (seat_cap is null or seat_cap > 0),
    confirmed_count  integer not null default 0,
    is_paid          boolean not null default false,
    price_minor      bigint,
    payment_url      text
        constraint events_payment_url_http check (payment_url is null or public._is_http_url(payment_url)),
    live_state       text not null default 'scheduled'
        constraint events_live_state_check check (live_state in ('scheduled', 'live', 'ended')),
    deleted_at       timestamptz,
    created_at       timestamptz not null default now(),
    constraint events_ends_after_start check (ends_at > starts_at),
    constraint events_location_check check (
        (format = 'in_person' and venue is not null and btrim(venue) <> '')
        or (format = 'online' and join_url is not null)
    ),
    -- the seat CHECK
    constraint events_seats_check check (
        confirmed_count >= 0 and (seat_cap is null or confirmed_count <= seat_cap)
    ),
    -- paid: price and an http(s) payment link (external payment until a gateway)
    constraint events_paid_check check (
        (is_paid and price_minor is not null and price_minor > 0 and payment_url is not null)
        or (not is_paid and price_minor is null and payment_url is null)
    )
);

create index events_community_starts_idx on public.events (community_id, starts_at) where deleted_at is null;
create index events_creator_idx on public.events (creator_id);

create table public.rsvps (
    id              uuid primary key default gen_random_uuid(),
    event_id        uuid not null references public.events (id) on delete cascade,
    user_id         uuid not null references public.profiles (id) on delete cascade,
    status          text not null
        constraint rsvps_status_check check (status in ('going', 'not_sure', 'declined')),
    payment_status  text not null default 'none'
        constraint rsvps_payment_status_check
        check (payment_status in ('none', 'pay_later', 'verification_pending', 'verified')),
    seat_confirmed  boolean not null default false,
    decline_reason  text
        constraint rsvps_decline_reason_check
        check (decline_reason in ('schedule', 'not_relevant', 'too_far', 'next_time', 'other')),
    note            varchar(500),
    remind_push     boolean not null default true,
    remind_email    boolean not null default true,
    updated_at      timestamptz not null default now(),
    constraint rsvps_event_user_key unique (event_id, user_id),
    constraint rsvps_decline_reason_only_declined check (decline_reason is null or status = 'declined'),
    constraint rsvps_seat_needs_going check (not seat_confirmed or status = 'going')
);

create index rsvps_user_idx on public.rsvps (user_id, event_id);

create table public.reminders (
    id        uuid primary key default gen_random_uuid(),
    user_id   uuid not null default auth.uid() references public.profiles (id) on delete cascade,
    event_id  uuid not null references public.events (id) on delete cascade,
    channel   text not null
        constraint reminders_channel_check check (channel in ('push', 'email')),
    send_at   timestamptz not null,
    sent_at   timestamptz,
    constraint reminders_slot_key unique (user_id, event_id, channel, send_at)
);

create index reminders_due_idx on public.reminders (send_at) where sent_at is null;
create index reminders_event_idx on public.reminders (event_id);

create table public.raises (
    id              uuid primary key default gen_random_uuid(),
    community_id    uuid not null references public.communities (id) on delete cascade,
    organiser_id    uuid not null references public.profiles (id) on delete cascade,
    title           varchar(120) not null
        constraint raises_title_not_blank check (btrim(title) <> ''),
    description     varchar(4000),
    goal_minor      bigint
        constraint raises_goal_not_negative check (goal_minor is null or goal_minor >= 0),
    raised_minor    bigint not null default 0
        constraint raises_raised_not_negative check (raised_minor >= 0),
    donor_count     integer not null default 0
        constraint raises_donor_count_not_negative check (donor_count >= 0),
    open_until      timestamptz,
    visibility      text not null default 'public'
        constraint raises_visibility_check check (visibility in ('public', 'private')),
    invite_enabled  boolean not null default false,
    receiver_name   varchar(120) not null
        constraint raises_receiver_not_blank check (btrim(receiver_name) <> ''),
    payout_mode     text not null
        constraint raises_payout_mode_check check (payout_mode in ('upi', 'bank')),
    upi_id          varchar(100)
        constraint raises_upi_id_format check (upi_id is null or upi_id ~ '^[A-Za-z0-9._-]+@[A-Za-z0-9.]{2,}$'),
    account_number  varchar(18)
        constraint raises_account_number_format check (account_number is null or account_number ~ '^[0-9]{6,18}$'),
    account_last4   varchar(4),
    ifsc            varchar(11)
        -- exactly 11 characters (normalised to upper case by trigger)
        constraint raises_ifsc_format check (ifsc is null or ifsc ~ '^[A-Z0-9]{11}$'),
    qr_url          text
        constraint raises_qr_url_http check (qr_url is null or public._is_http_url(qr_url)),
    closed_at       timestamptz,
    created_at      timestamptz not null default now(),
    constraint raises_payout_check check (
        (payout_mode = 'upi' and upi_id is not null and account_number is null and ifsc is null)
        or (payout_mode = 'bank' and account_number is not null and ifsc is not null and upi_id is null)
    )
);

create index raises_community_idx on public.raises (community_id, created_at desc);

create table public.contributions (
    id                  uuid primary key default gen_random_uuid(),
    raise_id            uuid not null references public.raises (id) on delete cascade,
    donor_id            uuid not null references public.profiles (id) on delete cascade,
    amount_minor        bigint not null
        constraint contributions_amount_positive check (amount_minor > 0),
    gateway_payment_id  varchar(64) not null
        constraint contributions_gateway_payment_id_not_blank check (btrim(gateway_payment_id) <> ''),
    status              text not null default 'created'
        constraint contributions_status_check check (status in ('created', 'captured', 'failed')),
    receipt_sent_at     timestamptz,
    created_at          timestamptz not null default now()
);

create unique index contributions_gateway_payment_id_key on public.contributions (gateway_payment_id);
create index contributions_raise_idx on public.contributions (raise_id, created_at desc);
create index contributions_donor_idx on public.contributions (donor_id, created_at desc);

-- ---------------------------------------------------------------------------
-- Access helpers
-- ---------------------------------------------------------------------------

-- Whether the caller can see an event: joined to its community, not deleted.
create or replace function public.can_see_event(p_event_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select coalesce((
    select e.deleted_at is null and public.is_joined(e.community_id)
    from public.events e where e.id = p_event_id
  ), false)
$$;

create or replace function public._event_creator(p_event_id uuid)
returns uuid
language sql stable security definer set search_path = ''
as $$
  select e.creator_id from public.events e where e.id = p_event_id
$$;

-- "N seats left" for a visible event; null when unlimited or not visible.
create or replace function public.seats_left(p_event_id uuid)
returns integer
language sql stable security definer set search_path = ''
as $$
  select greatest(e.seat_cap - e.confirmed_count, 0)
  from public.events e
  where e.id = p_event_id and public.can_see_event(e.id)
$$;

-- The join link, for the creator and for members with a confirmed seat.
create or replace function public.event_join_url(p_event_id uuid)
returns text
language sql stable security definer set search_path = ''
as $$
  select e.join_url
  from public.events e
  where e.id = p_event_id
    and public.can_see_event(e.id)
    and (e.creator_id = auth.uid()
         or exists (select 1 from public.rsvps r
                    where r.event_id = e.id and r.user_id = auth.uid() and r.seat_confirmed))
$$;

-- Whether the caller may see a community's raises (joined). Callable by anon
-- (always false), so raises_masked works for every client role.
create or replace function public.can_see_raises(p_community_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select auth.uid() is not null and public.is_joined(p_community_id)
$$;

-- Whether the caller may see a raise's payout details: its organiser, or a
-- joined owner/admin of its community.
create or replace function public.can_view_payout(p_raise_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select coalesce((
    select r.organiser_id = auth.uid() or public.has_community_role(r.community_id, array['owner', 'admin'])
    from public.raises r where r.id = p_raise_id
  ), false)
$$;

-- ---------------------------------------------------------------------------
-- Reminder scheduling
-- ---------------------------------------------------------------------------

-- The standard reminder slots of a going RSVP for an event starting at p_starts_at.
create or replace function public._rsvp_reminder_slots(p_starts_at timestamptz)
returns table (channel text, send_at timestamptz)
language sql immutable set search_path = ''
as $$
  values
    ('push', p_starts_at - interval '1 day'),
    ('push', p_starts_at - interval '1 hour'),
    ('email', least(
       (date_trunc('day', p_starts_at at time zone 'Asia/Kolkata') + interval '8 hours')
         at time zone 'Asia/Kolkata',
       p_starts_at - interval '1 hour'))
$$;

-- Makes a user's pending standard reminders for an event match their RSVP
-- preferences. Past slots are never scheduled; sent reminders are kept.
create or replace function public._sync_rsvp_reminders(p_event_id uuid, p_user_id uuid, p_push boolean, p_email boolean)
returns void
language plpgsql security definer set search_path = ''
as $$
declare
  v_starts timestamptz;
  v_deleted timestamptz;
  v_slot record;
begin
  select e.starts_at, e.deleted_at into v_starts, v_deleted from public.events e where e.id = p_event_id;
  if v_starts is null then
    return;
  end if;
  for v_slot in select * from public._rsvp_reminder_slots(v_starts) loop
    if v_deleted is null
       and v_slot.send_at > now()
       and ((v_slot.channel = 'push' and p_push) or (v_slot.channel = 'email' and p_email)) then
      insert into public.reminders (user_id, event_id, channel, send_at)
      values (p_user_id, p_event_id, v_slot.channel, v_slot.send_at)
      on conflict on constraint reminders_slot_key do nothing;
    else
      delete from public.reminders r
       where r.user_id = p_user_id and r.event_id = p_event_id
         and r.channel = v_slot.channel and r.send_at = v_slot.send_at
         and r.sent_at is null;
    end if;
  end loop;
end;
$$;

-- For the scheduler (service_role): marks up to p_limit due reminders of live
-- events as sent and returns them for delivery.
create or replace function public.claim_due_reminders(p_limit integer default 100)
returns setof public.reminders
language sql security definer set search_path = ''
as $$
  with due as (
    select r.id
    from public.reminders r
    join public.events e on e.id = r.event_id and e.deleted_at is null
    where r.sent_at is null and r.send_at <= now()
    order by r.send_at
    limit p_limit
    for update of r skip locked
  )
  update public.reminders r set sent_at = now()
  from due where r.id = due.id
  returning r.*
$$;

-- ---------------------------------------------------------------------------
-- Triggers: events
-- ---------------------------------------------------------------------------

create or replace function public._events_before_write()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  if tg_op = 'INSERT' then
    new.confirmed_count := 0;
    new.deleted_at := null;
    new.created_at := now();
  else
    new.community_id := old.community_id;
    new.creator_id := old.creator_id;
    new.created_at := old.created_at;
  end if;
  return new;
end;
$$;

create trigger events_before_write before insert or update on public.events
  for each row execute function public._events_before_write();

-- A new start time moves the pending reminders: the standard RSVP slots are
-- recomputed, other ("Remind me") reminders shift by the same amount.
create or replace function public._events_after_reschedule()
returns trigger
language plpgsql security definer set search_path = ''
as $$
declare
  v_rsvp record;
begin
  delete from public.reminders r
   where r.event_id = new.id and r.sent_at is null
     and (r.channel, r.send_at) in (select s.channel, s.send_at from public._rsvp_reminder_slots(old.starts_at) s)
     and exists (select 1 from public.rsvps x where x.event_id = new.id and x.user_id = r.user_id and x.status = 'going');
  update public.reminders r
     set send_at = r.send_at + (new.starts_at - old.starts_at)
   where r.event_id = new.id and r.sent_at is null;
  for v_rsvp in select * from public.rsvps x where x.event_id = new.id and x.status = 'going' loop
    perform public._sync_rsvp_reminders(new.id, v_rsvp.user_id, v_rsvp.remind_push, v_rsvp.remind_email);
  end loop;
  return new;
end;
$$;

create trigger events_after_reschedule after update of starts_at on public.events
  for each row when (old.starts_at is distinct from new.starts_at)
  execute function public._events_after_reschedule();

-- ---------------------------------------------------------------------------
-- Triggers: rsvps (seat lock and reminders)
-- ---------------------------------------------------------------------------

create or replace function public._rsvps_before_write()
returns trigger
language plpgsql security definer set search_path = ''
as $$
declare
  v_event record;
begin
  if tg_op = 'UPDATE' then
    new.event_id := old.event_id;
    new.user_id := old.user_id;
  end if;

  -- Derivation only: a BEFORE INSERT trigger also fires for an
  -- `insert .. on conflict do update` that ends up updating, so the seat
  -- count is kept by the AFTER trigger, which fires for what really happened.
  select e.is_paid, e.deleted_at
    into v_event
    from public.events e where e.id = new.event_id;
  if not found or v_event.deleted_at is not null then
    raise exception 'Event not found' using errcode = 'PT404';
  end if;

  if new.status <> 'declined' then
    new.decline_reason := null;
  end if;
  if not v_event.is_paid then
    new.payment_status := 'none';
  elsif new.status = 'going' and new.payment_status = 'none' then
    new.payment_status := 'pay_later';
  end if;

  new.seat_confirmed := new.status = 'going'
                        and (not v_event.is_paid or new.payment_status = 'verified');
  new.updated_at := now();
  return new;
end;
$$;

create trigger rsvps_before_write before insert or update on public.rsvps
  for each row execute function public._rsvps_before_write();

create or replace function public._rsvps_after_write()
returns trigger
language plpgsql security definer set search_path = ''
as $$
declare
  v_delta integer;
  v_event record;
begin
  if tg_op = 'DELETE' then
    if old.seat_confirmed then
      update public.events e set confirmed_count = greatest(e.confirmed_count - 1, 0) where e.id = old.event_id;
    end if;
    perform public._sync_rsvp_reminders(old.event_id, old.user_id, false, false);
    return old;
  end if;

  v_delta := new.seat_confirmed::int
             - case when tg_op = 'UPDATE' then old.seat_confirmed::int else 0 end;
  if v_delta <> 0 then
    -- Lock the event row: concurrent RSVPs for the same event serialise
    -- here, and the later one reads the count the earlier one committed.
    -- NO KEY UPDATE (not UPDATE): it does not conflict with the KEY SHARE
    -- lock the rsvps foreign key already holds, so racers cannot deadlock.
    select e.seat_cap, e.confirmed_count into v_event
      from public.events e where e.id = new.event_id
      for no key update;
    if v_delta > 0 and v_event.seat_cap is not null and v_event.confirmed_count >= v_event.seat_cap then
      raise exception 'Event full' using errcode = 'PT409';
    end if;
    update public.events e set confirmed_count = e.confirmed_count + v_delta where e.id = new.event_id;
  end if;
  perform public._sync_rsvp_reminders(new.event_id, new.user_id,
                                      new.status = 'going' and new.remind_push,
                                      new.status = 'going' and new.remind_email);
  return new;
end;
$$;

create trigger rsvps_after_write after insert or update or delete on public.rsvps
  for each row execute function public._rsvps_after_write();

-- ---------------------------------------------------------------------------
-- Triggers: raises and contributions
-- ---------------------------------------------------------------------------

create or replace function public._raises_before_write()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  new.upi_id := nullif(btrim(new.upi_id), '');
  new.account_number := nullif(btrim(new.account_number), '');
  new.ifsc := nullif(upper(btrim(new.ifsc)), '');
  new.account_last4 := right(new.account_number, 4);

  if tg_op = 'INSERT' then
    new.raised_minor := 0;
    new.donor_count := 0;
    new.closed_at := null;
    new.created_at := now();
    if new.open_until is not null and new.open_until <= now() then
      raise exception 'Open until must be in the future' using errcode = '23514';
    end if;
  else
    new.community_id := old.community_id;
    new.organiser_id := old.organiser_id;
    new.created_at := old.created_at;
    if new.open_until is distinct from old.open_until
       and new.open_until is not null and new.open_until <= now() then
      raise exception 'Open until must be in the future' using errcode = '23514';
    end if;
  end if;
  return new;
end;
$$;

create trigger raises_before_write before insert or update on public.raises
  for each row execute function public._raises_before_write();

create or replace function public._contributions_before_write()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  if tg_op = 'INSERT' then
    new.created_at := now();
  else
    new.raise_id := old.raise_id;
    new.donor_id := old.donor_id;
    new.amount_minor := old.amount_minor;
    new.gateway_payment_id := old.gateway_payment_id;
    new.created_at := old.created_at;
    -- a captured payment stays captured; a receipt stays sent
    if old.status = 'captured' then
      new.status := 'captured';
    end if;
    new.receipt_sent_at := coalesce(old.receipt_sent_at, new.receipt_sent_at);
  end if;
  return new;
end;
$$;

create trigger contributions_before_write before insert or update on public.contributions
  for each row execute function public._contributions_before_write();

-- The raise's totals move exactly once per captured payment.
create or replace function public._contributions_after_capture()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  if new.status = 'captured' and (tg_op = 'INSERT' or old.status <> 'captured') then
    update public.raises r
       set raised_minor = r.raised_minor + new.amount_minor,
           donor_count = r.donor_count + case when exists (
             select 1 from public.contributions c
             where c.raise_id = new.raise_id and c.donor_id = new.donor_id
               and c.status = 'captured' and c.id <> new.id) then 0 else 1 end
     where r.id = new.raise_id;
  end if;
  return new;
end;
$$;

create trigger contributions_after_capture after insert or update of status on public.contributions
  for each row execute function public._contributions_after_capture();

-- For the webhook handler (service_role): records a captured gateway payment.
-- Idempotent on gateway_payment_id: `applied` is true only the first time the
-- payment becomes captured (send the receipt then), false for a duplicate.
create or replace function public.capture_contribution(
  p_raise_id uuid, p_donor_id uuid, p_amount_minor bigint, p_gateway_payment_id text)
returns table (contribution_id uuid, applied boolean)
language plpgsql security definer set search_path = ''
as $$
declare
  v_id uuid;
  v_status text;
begin
  insert into public.contributions (raise_id, donor_id, amount_minor, gateway_payment_id, status)
  values (p_raise_id, p_donor_id, p_amount_minor, p_gateway_payment_id, 'captured')
  on conflict (gateway_payment_id) do nothing
  returning id into v_id;
  if v_id is not null then
    return query select v_id, true;
    return;
  end if;

  select c.id, c.status into v_id, v_status
    from public.contributions c where c.gateway_payment_id = p_gateway_payment_id
    for update;
  if v_status <> 'captured' then
    update public.contributions c set status = 'captured' where c.id = v_id;
    return query select v_id, true;
  else
    return query select v_id, false;
  end if;
end;
$$;

-- For the service: marks the receipt sent; false if it already was.
create or replace function public.mark_receipt_sent(p_contribution_id uuid)
returns boolean
language sql security definer set search_path = ''
as $$
  with u as (
    update public.contributions c set receipt_sent_at = now()
    where c.id = p_contribution_id and c.receipt_sent_at is null and c.status = 'captured'
    returning 1
  )
  select exists (select 1 from u)
$$;

-- ---------------------------------------------------------------------------
-- The masked raise view (the only client read path for raises)
-- ---------------------------------------------------------------------------

-- Runs with its owner's rights (it reads the base table that clients cannot)
-- and filters by the caller itself: joined members of the raise's community
-- only. security_barrier keeps caller-supplied predicates from seeing rows
-- before that filter.
create view public.raises_masked with (security_barrier = true) as
select
  r.id, r.community_id, r.organiser_id, r.title, r.description, r.goal_minor,
  r.raised_minor, r.donor_count, r.open_until, r.visibility, r.invite_enabled,
  r.receiver_name, r.payout_mode,
  case when p.full then r.upi_id end         as upi_id,
  case when p.full then r.account_number end as account_number,
  r.account_last4,
  case when p.full then r.ifsc end           as ifsc,
  case when p.full then r.qr_url end         as qr_url,
  r.closed_at, r.created_at,
  p.full                                     as payout_visible
from public.raises r
cross join lateral (select public.can_view_payout(r.id) as full) p
where public.can_see_raises(r.community_id);

-- ---------------------------------------------------------------------------
-- RLS and grants
-- ---------------------------------------------------------------------------

alter table public.events enable row level security;
alter table public.rsvps enable row level security;
alter table public.reminders enable row level security;
alter table public.raises enable row level security;
alter table public.contributions enable row level security;

revoke all on public.events, public.rsvps, public.reminders, public.raises,
  public.contributions, public.raises_masked
  from anon, authenticated;

-- events: joined members see live events; join_url is not granted (see
-- event_join_url). The creator edits the client-writable columns.
grant select (id, community_id, creator_id, title, description, cover_url, starts_at, ends_at,
              format, venue, tags, speaker, seat_cap, confirmed_count, is_paid, price_minor,
              payment_url, live_state, deleted_at, created_at)
  on public.events to anon, authenticated;
create policy events_select_joined on public.events for select to authenticated
  using (deleted_at is null and public.is_joined(community_id));
grant update (title, description, cover_url, starts_at, ends_at, format, venue, join_url, tags,
              speaker, seat_cap, is_paid, price_minor, payment_url, live_state)
  on public.events to authenticated;
create policy events_update_creator on public.events for update to authenticated
  using (creator_id = auth.uid() and deleted_at is null and public.is_joined(community_id))
  with check (creator_id = auth.uid());

-- rsvps: written by the RSVP RPCs only. Visible to their owner, to the
-- event's creator (attendees, payment verification) and, for going rows, to
-- the event's members (attendee list).
grant select on public.rsvps to anon, authenticated;
create policy rsvps_select on public.rsvps for select to authenticated
  using (
    user_id = auth.uid()
    or (public.can_see_event(event_id)
        and (public._event_creator(event_id) = auth.uid() or status = 'going'))
  );

-- reminders: own rows. "Remind me" inserts one for a visible event;
-- user_id defaults to the caller; pending ones can be cancelled.
grant select on public.reminders to anon, authenticated;
grant insert (event_id, channel, send_at) on public.reminders to authenticated;
grant delete on public.reminders to authenticated;
create policy reminders_select_own on public.reminders for select to authenticated
  using (user_id = auth.uid());
create policy reminders_insert_own on public.reminders for insert to authenticated
  with check (user_id = auth.uid() and public.can_see_event(event_id));
create policy reminders_delete_own on public.reminders for delete to authenticated
  using (user_id = auth.uid() and sent_at is null);

-- raises: the base table is denied to clients outright; read raises_masked.
create policy raises_deny_clients on public.raises as restrictive for all to anon, authenticated
  using (false) with check (false);
grant select on public.raises_masked to anon, authenticated;

-- contributions: the donor's own, and the raise's organiser/admins.
grant select on public.contributions to anon, authenticated;
create policy contributions_select on public.contributions for select to authenticated
  using (donor_id = auth.uid() or public.can_view_payout(raise_id));

-- ---------------------------------------------------------------------------
-- Execute rights
-- ---------------------------------------------------------------------------

revoke all on function
  public._is_http_url(text), public._is_string_array(jsonb),
  public.can_see_event(uuid), public._event_creator(uuid), public.seats_left(uuid),
  public.event_join_url(uuid), public.can_view_payout(uuid),
  public._rsvp_reminder_slots(timestamptz),
  public._sync_rsvp_reminders(uuid, uuid, boolean, boolean),
  public.claim_due_reminders(integer),
  public._events_before_write(), public._events_after_reschedule(),
  public._rsvps_before_write(), public._rsvps_after_write(),
  public._raises_before_write(), public._contributions_before_write(),
  public._contributions_after_capture(),
  public.capture_contribution(uuid, uuid, bigint, text), public.mark_receipt_sent(uuid)
  from public, anon, authenticated;

grant execute on function
  public._is_http_url(text), public._is_string_array(jsonb),
  public.can_see_event(uuid), public._event_creator(uuid), public.seats_left(uuid),
  public.event_join_url(uuid), public.can_view_payout(uuid), public.can_see_raises(uuid)
  to anon, authenticated, service_role;

grant execute on function
  public._rsvp_reminder_slots(timestamptz),
  public._sync_rsvp_reminders(uuid, uuid, boolean, boolean),
  public.claim_due_reminders(integer),
  public.capture_contribution(uuid, uuid, bigint, text), public.mark_receipt_sent(uuid)
  to service_role;
