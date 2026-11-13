-- Auth attempt log, known devices and the notifications inbox.
--
-- auth_attempts backs the OTP throttles (30 s resend, 5 verifies per OTP,
-- 10-minute validity) and the password lockout (10 failures in 15 minutes
-- blocks for 15 minutes). It holds only sha256 hashes of identifiers and is
-- never readable by API clients: the service calls the SECURITY DEFINER
-- functions below as service_role.
--
-- known_devices lists the sessions a user has signed in from; device_hash is
-- secret (no client column grant). notifications is the top-bar bell inbox.

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

create table public.auth_attempts (
    id               uuid primary key default gen_random_uuid(),
    identifier_hash  varchar(64) not null
        constraint auth_attempts_identifier_hash_format check (identifier_hash ~ '^[0-9a-f]{64}$'),
    action           text not null
        constraint auth_attempts_action_check check (action in ('otp_send', 'otp_verify', 'password')),
    otp_ref          uuid,
    succeeded        boolean not null default false,
    created_at       timestamptz not null default now(),
    -- otp_send: otp_ref names the issued OTP; otp_verify: the OTP verified;
    -- password: no OTP.
    constraint auth_attempts_otp_ref_check check ((action = 'password') = (otp_ref is null))
);

create index auth_attempts_lookup_idx on public.auth_attempts (identifier_hash, action, created_at desc);
create index auth_attempts_otp_ref_idx on public.auth_attempts (otp_ref) where action = 'otp_verify';

create table public.known_devices (
    id            uuid primary key default gen_random_uuid(),
    user_id       uuid not null references public.profiles (id) on delete cascade,
    session_id    uuid not null,
    device_hash   varchar(64) not null
        constraint known_devices_device_hash_format check (device_hash ~ '^[0-9a-f]{64}$'),
    user_agent    varchar(300) not null default '',
    last_seen_at  timestamptz not null default now(),
    created_at    timestamptz not null default now()
);

create unique index known_devices_session_id_key on public.known_devices (session_id);
create index known_devices_user_device_idx on public.known_devices (user_id, device_hash);

create table public.notifications (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null references public.profiles (id) on delete cascade,
    kind        varchar(40) not null
        constraint notifications_kind_format check (kind ~ '^[a-z][a-z0-9_]*$'),
    payload     jsonb not null default '{}'::jsonb
        constraint notifications_payload_object check (jsonb_typeof(payload) = 'object'),
    read_at     timestamptz,
    created_at  timestamptz not null default now()
);

create index notifications_user_created_idx on public.notifications (user_id, created_at desc, id desc);
create index notifications_user_unread_idx on public.notifications (user_id) where read_at is null;

-- ---------------------------------------------------------------------------
-- Row level security and privileges
-- ---------------------------------------------------------------------------

alter table public.auth_attempts enable row level security;
alter table public.known_devices enable row level security;
alter table public.notifications enable row level security;

revoke all on public.auth_attempts, public.known_devices, public.notifications
  from anon, authenticated;

-- auth_attempts: no client access at all (no grants, no policies).

-- known_devices: own rows, every column except device_hash. Clients select
-- explicit columns; writes happen only through the functions below.
grant select (id, user_id, session_id, user_agent, last_seen_at, created_at)
  on public.known_devices to anon, authenticated;
create policy known_devices_select_own on public.known_devices for select to authenticated
  using (user_id = auth.uid());

-- notifications: own rows; the only client write is marking read.
grant select on public.notifications to anon, authenticated;
create policy notifications_select_own on public.notifications for select to authenticated
  using (user_id = auth.uid());
grant update (read_at) on public.notifications to authenticated;
create policy notifications_update_own on public.notifications for update to authenticated
  using (user_id = auth.uid()) with check (user_id = auth.uid());

-- ---------------------------------------------------------------------------
-- OTP throttles
-- ---------------------------------------------------------------------------

-- Logs an OTP send for an identifier and returns the new OTP's reference.
-- PT429 when the previous send for this identifier was less than 30 s ago.
create or replace function public.auth_otp_send(p_identifier_hash text)
returns uuid
language plpgsql security definer set search_path = ''
as $$
declare
  v_ref uuid := gen_random_uuid();
begin
  perform pg_advisory_xact_lock(hashtextextended('auth:' || p_identifier_hash, 0));
  if exists (
    select 1 from public.auth_attempts a
    where a.identifier_hash = p_identifier_hash
      and a.action = 'otp_send'
      and a.created_at > now() - interval '30 seconds'
  ) then
    raise exception 'Please wait before requesting another code.' using errcode = 'PT429';
  end if;
  insert into public.auth_attempts (identifier_hash, action, otp_ref, succeeded)
  values (p_identifier_hash, 'otp_send', v_ref, true);
  return v_ref;
end;
$$;

-- Starts a verify of the latest OTP sent to an identifier and returns the
-- attempt id. Call auth_attempt_succeeded(id) once the code checks out.
-- PT401 no OTP was sent; PT410 the OTP is older than 10 minutes;
-- PT429 five verifies were already made on this OTP (the 6th is rejected).
create or replace function public.auth_otp_verify_begin(p_identifier_hash text)
returns uuid
language plpgsql security definer set search_path = ''
as $$
declare
  v_send public.auth_attempts;
  v_id uuid;
begin
  perform pg_advisory_xact_lock(hashtextextended('auth:' || p_identifier_hash, 0));
  select * into v_send from public.auth_attempts a
  where a.identifier_hash = p_identifier_hash and a.action = 'otp_send'
  order by a.created_at desc
  limit 1;
  if not found then
    raise exception 'Invalid code. Try again.' using errcode = 'PT401';
  end if;
  if v_send.created_at <= now() - interval '10 minutes' then
    raise exception 'This code has expired. Request a new one.' using errcode = 'PT410';
  end if;
  if (select count(*) from public.auth_attempts a
      where a.otp_ref = v_send.otp_ref and a.action = 'otp_verify') >= 5 then
    raise exception 'Too many attempts. Request a new code.' using errcode = 'PT429';
  end if;
  insert into public.auth_attempts (identifier_hash, action, otp_ref, succeeded)
  values (p_identifier_hash, 'otp_verify', v_send.otp_ref, false)
  returning id into v_id;
  return v_id;
end;
$$;

-- ---------------------------------------------------------------------------
-- Password lockout
-- ---------------------------------------------------------------------------

-- Consecutive password failures (since the last success) in the last
-- 15 minutes. A success resets it to zero.
create or replace function public.auth_password_failures(p_identifier_hash text)
returns integer
language sql stable security definer set search_path = ''
as $$
  select count(*)::integer from public.auth_attempts a
  where a.identifier_hash = p_identifier_hash
    and a.action = 'password'
    and not a.succeeded
    and a.created_at > now() - interval '15 minutes'
    and a.created_at > coalesce((
      select max(s.created_at) from public.auth_attempts s
      where s.identifier_hash = p_identifier_hash and s.action = 'password' and s.succeeded
    ), '-infinity')
$$;

-- When the identifier is locked, the time the lock ends; otherwise null.
-- Locked = 10 consecutive failures (no success in between) within
-- 15 minutes, with the 10th of them less than 15 minutes ago.
create or replace function public.auth_password_locked_until(p_identifier_hash text)
returns timestamptz
language sql stable security definer set search_path = ''
as $$
  with last_success as (
    select coalesce(max(s.created_at), '-infinity') as at from public.auth_attempts s
    where s.identifier_hash = p_identifier_hash and s.action = 'password' and s.succeeded
  ), failures as (
    select a.created_at,
           lag(a.created_at, 9) over (order by a.created_at, a.id) as ninth_before
    from public.auth_attempts a, last_success l
    where a.identifier_hash = p_identifier_hash
      and a.action = 'password'
      and not a.succeeded
      and a.created_at > l.at
      and a.created_at > now() - interval '30 minutes'
  )
  select max(f.created_at) + interval '15 minutes' from failures f
  where f.ninth_before is not null
    and f.created_at - f.ninth_before <= interval '15 minutes'
    and f.created_at > now() - interval '15 minutes'
$$;

-- Starts a password sign-in attempt (logged as a failure until
-- auth_attempt_succeeded marks it) and returns its id.
-- PT423 while the identifier is locked; blocked attempts are not logged, so
-- the lock ends 15 minutes after the 10th failure.
create or replace function public.auth_password_begin(p_identifier_hash text)
returns uuid
language plpgsql security definer set search_path = ''
as $$
declare
  v_id uuid;
begin
  perform pg_advisory_xact_lock(hashtextextended('auth:' || p_identifier_hash, 0));
  if public.auth_password_locked_until(p_identifier_hash) is not null then
    raise exception 'Too many failed attempts. Try again in 15 minutes.' using errcode = 'PT423';
  end if;
  insert into public.auth_attempts (identifier_hash, action, succeeded)
  values (p_identifier_hash, 'password', false)
  returning id into v_id;
  return v_id;
end;
$$;

-- Marks an OTP verify or password attempt as successful.
create or replace function public.auth_attempt_succeeded(p_attempt_id uuid)
returns void
language plpgsql security definer set search_path = ''
as $$
begin
  update public.auth_attempts set succeeded = true
  where id = p_attempt_id and action in ('otp_verify', 'password');
  if not found then
    raise exception 'Unknown attempt.' using errcode = 'PT404';
  end if;
end;
$$;

-- Deletes attempts older than the retention window (default one day; every
-- rule above looks back at most 30 minutes).
create or replace function public.auth_attempts_purge(p_older_than interval default interval '1 day')
returns integer
language plpgsql security definer set search_path = ''
as $$
declare
  v_count integer;
begin
  delete from public.auth_attempts where created_at < now() - p_older_than;
  get diagnostics v_count = row_count;
  return v_count;
end;
$$;

-- ---------------------------------------------------------------------------
-- Notifications and devices
-- ---------------------------------------------------------------------------

-- Inserts an in-app notification; the server-side writer for every kind.
create or replace function public.notify(p_user_id uuid, p_kind text, p_payload jsonb default '{}'::jsonb)
returns uuid
language sql security definer set search_path = ''
as $$
  insert into public.notifications (user_id, kind, payload)
  values (p_user_id, p_kind, coalesce(p_payload, '{}'::jsonb))
  returning id
$$;

-- Records the session a user just signed in with (after the password and,
-- with 2FA on, the SMS code). A device_hash this user has not signed in from
-- before is a new device; with login_alerts on (and p_alert true) a
-- login_alert notification is stored. Returns
-- {"device_id", "new_device", "alerted"}; when "alerted" the service also
-- sends the alert email.
create or replace function public.record_sign_in(
  p_user_id uuid, p_session_id uuid, p_device_hash text, p_user_agent text,
  p_alert boolean default true)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_new boolean;
  v_alerts boolean;
  v_device_id uuid;
  v_alerted boolean := false;
begin
  select p.login_alerts into v_alerts from public.profiles p where p.id = p_user_id for update;
  if not found then
    raise exception 'Unknown user.' using errcode = 'PT404';
  end if;
  v_new := not exists (
    select 1 from public.known_devices d
    where d.user_id = p_user_id and d.device_hash = p_device_hash);

  insert into public.known_devices (user_id, session_id, device_hash, user_agent)
  values (p_user_id, p_session_id, p_device_hash, left(coalesce(p_user_agent, ''), 300))
  on conflict (session_id) do update
    set last_seen_at = now(), user_agent = excluded.user_agent
    where public.known_devices.user_id = excluded.user_id
  returning id into v_device_id;
  if v_device_id is null then
    raise exception 'Session belongs to another user.' using errcode = 'PT409';
  end if;

  if v_new and v_alerts and p_alert then
    perform public.notify(p_user_id, 'login_alert',
      jsonb_build_object('known_device_id', v_device_id, 'session_id', p_session_id));
    v_alerted := true;
  end if;
  return jsonb_build_object('device_id', v_device_id, 'new_device', v_new, 'alerted', v_alerted);
end;
$$;

-- Removes every known device of the user except the given session and
-- returns the removed session ids. The service revokes those sessions in
-- Supabase Auth (sign out scope=others) in the same request.
create or replace function public.forget_other_devices(p_user_id uuid, p_keep_session_id uuid)
returns setof uuid
language sql security definer set search_path = ''
as $$
  delete from public.known_devices d
  where d.user_id = p_user_id and d.session_id is distinct from p_keep_session_id
  returning d.session_id
$$;

-- Removes the device of one session (sign out scope=local).
create or replace function public.forget_device(p_session_id uuid)
returns void
language sql security definer set search_path = ''
as $$
  delete from public.known_devices where session_id = p_session_id
$$;

-- ---------------------------------------------------------------------------
-- Scope switcher
-- ---------------------------------------------------------------------------

-- The communities the caller has joined, with their role: what the top-bar
-- switcher lists. Pending requests and deleted communities are excluded.
create or replace function public.switcher_communities()
returns table (id uuid, handle text, name text, emoji text, cover_gradient text,
               logo_url text, member_count integer, role text)
language sql stable security definer set search_path = ''
as $$
  select c.id, c.handle::text, c.name::text, c.emoji::text, c.cover_gradient::text,
         c.logo_url, c.member_count, m.role
  from public.memberships m
  join public.communities c on c.id = m.community_id
  where m.user_id = auth.uid()
    and m.status = 'joined'
    and c.deleted_at is null
  order by c.name, c.id
$$;

-- ---------------------------------------------------------------------------
-- Execute rights: auth and device functions for the service only.
-- ---------------------------------------------------------------------------

revoke all on function
  public.auth_otp_send(text), public.auth_otp_verify_begin(text),
  public.auth_password_failures(text), public.auth_password_locked_until(text),
  public.auth_password_begin(text), public.auth_attempt_succeeded(uuid),
  public.auth_attempts_purge(interval), public.notify(uuid, text, jsonb),
  public.record_sign_in(uuid, uuid, text, text, boolean),
  public.forget_other_devices(uuid, uuid), public.forget_device(uuid),
  public.switcher_communities()
  from public, anon, authenticated;

grant execute on function
  public.auth_otp_send(text), public.auth_otp_verify_begin(text),
  public.auth_password_failures(text), public.auth_password_locked_until(text),
  public.auth_password_begin(text), public.auth_attempt_succeeded(uuid),
  public.auth_attempts_purge(interval), public.notify(uuid, text, jsonb),
  public.record_sign_in(uuid, uuid, text, text, boolean),
  public.forget_other_devices(uuid, uuid), public.forget_device(uuid)
  to service_role;

grant execute on function public.switcher_communities() to authenticated, service_role;
