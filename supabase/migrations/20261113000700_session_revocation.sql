-- Session revocation for sign out, "sign out of all other devices" and
-- password change/reset.
--
-- Supabase access tokens are stateless JWTs that stay valid until they
-- expire even after Auth revokes the session's refresh token. The API
-- therefore keeps its own revocation list and rejects (401) any bearer whose
-- session was signed out:
--   revoked_sessions      — one row per session signed out with scope=local;
--   session_revocations   — per user: every token issued before
--                           revoked_before, except the kept session, is
--                           rejected (scope=others, password change).
-- Neither table is visible to clients; the service calls the functions below
-- as service_role.

create table public.revoked_sessions (
    session_id  uuid primary key,
    user_id     uuid not null references public.profiles (id) on delete cascade,
    revoked_at  timestamptz not null default now()
);

create index revoked_sessions_revoked_at_idx on public.revoked_sessions (revoked_at);

create table public.session_revocations (
    user_id          uuid primary key references public.profiles (id) on delete cascade,
    revoked_before   timestamptz not null,
    keep_session_id  uuid
);

alter table public.revoked_sessions enable row level security;
alter table public.session_revocations enable row level security;
revoke all on public.revoked_sessions, public.session_revocations from anon, authenticated;

-- Signs out one session (scope=local) and forgets its device.
create or replace function public.revoke_session(p_user_id uuid, p_session_id uuid)
returns void
language plpgsql security definer set search_path = ''
as $$
begin
  if not exists (select 1 from public.profiles p where p.id = p_user_id) then
    return; -- no profile, nothing recorded for it; Auth revocation still applies
  end if;
  insert into public.revoked_sessions (session_id, user_id)
  values (p_session_id, p_user_id)
  on conflict (session_id) do nothing;
  delete from public.known_devices d where d.session_id = p_session_id and d.user_id = p_user_id;
end;
$$;

-- Signs out every session of the user except p_keep_session_id: tokens
-- issued before now are rejected (iat has whole seconds, so a token issued in
-- the same second also is) unless they belong to the
-- kept session. Forgets the other devices and returns their session ids.
create or replace function public.revoke_other_sessions(p_user_id uuid, p_keep_session_id uuid)
returns setof uuid
language plpgsql security definer set search_path = ''
as $$
begin
  if not exists (select 1 from public.profiles p where p.id = p_user_id) then
    return;
  end if;
  insert into public.session_revocations (user_id, revoked_before, keep_session_id)
  values (p_user_id, now(), p_keep_session_id)
  on conflict (user_id) do update
    set revoked_before = excluded.revoked_before, keep_session_id = excluded.keep_session_id;
  return query select public.forget_other_devices(p_user_id, p_keep_session_id);
end;
$$;

-- True when a token (user, session id, issued-at) has been signed out.
-- The kept session of an earlier "others" revocation stays valid until it
-- is itself revoked.
create or replace function public.session_revoked(p_user_id uuid, p_session_id uuid, p_issued_at timestamptz)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (select 1 from public.revoked_sessions r where r.session_id = p_session_id)
      or exists (
        select 1 from public.session_revocations s
        where s.user_id = p_user_id
          and coalesce(p_issued_at, '-infinity') < s.revoked_before
          and s.keep_session_id is distinct from p_session_id)
$$;

-- Access tokens live at most an hour; older revocation rows are noise.
create or replace function public.revoked_sessions_purge(p_older_than interval default interval '1 day')
returns integer
language plpgsql security definer set search_path = ''
as $$
declare
  v_count integer;
begin
  delete from public.revoked_sessions where revoked_at < now() - p_older_than;
  get diagnostics v_count = row_count;
  return v_count;
end;
$$;

revoke all on function
  public.revoke_session(uuid, uuid), public.revoke_other_sessions(uuid, uuid),
  public.session_revoked(uuid, uuid, timestamptz), public.revoked_sessions_purge(interval)
  from public, anon, authenticated;
grant execute on function
  public.revoke_session(uuid, uuid), public.revoke_other_sessions(uuid, uuid),
  public.session_revoked(uuid, uuid, timestamptz), public.revoked_sessions_purge(interval)
  to service_role;

-- Password sign-in needs the second-factor phone and alert setting of the
-- account it just authenticated (service_role only).
create or replace function public.sign_in_settings(p_user_id uuid)
returns table (two_factor_enabled boolean, phone text, login_alerts boolean)
language sql stable security definer set search_path = ''
as $$
  select p.two_factor_enabled, p.phone::text, p.login_alerts
  from public.profiles p where p.id = p_user_id
$$;

revoke all on function public.sign_in_settings(uuid) from public, anon, authenticated;
grant execute on function public.sign_in_settings(uuid) to service_role;
