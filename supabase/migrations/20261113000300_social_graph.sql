-- Social graph: blocks, pair-keyed connections, one-way follows and
-- mentorship requests.
--
-- Relationship states seen by a caller (connection_state):
--   idle       no row (also after withdraw, remove, block or unblock)
--   sent       the caller's request is pending (or was silently declined)
--   incoming   the other user's request awaits the caller (Accept / Ignore)
--   connected  mutual connection
--   blocked    the caller blocked the other user
-- Blocking deletes the connection and follows in both directions, so an
-- unblock leaves the pair Idle, never Connected.
--
-- Block filtering for feed, people and chat queries: blocked_user_ids()
-- (caller's view, either direction) and is_blocked_with(other); service and
-- other SECURITY DEFINER code uses blocked_between(a, b).

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

create table public.blocks (
    id          uuid primary key default gen_random_uuid(),
    blocker_id  uuid not null references public.profiles (id) on delete cascade,
    blocked_id  uuid not null references public.profiles (id) on delete cascade,
    created_at  timestamptz not null default now(),
    constraint blocks_not_self check (blocker_id <> blocked_id),
    constraint blocks_pair_key unique (blocker_id, blocked_id)
);

create index blocks_blocked_idx on public.blocks (blocked_id);

create table public.connections (
    id            uuid primary key default gen_random_uuid(),
    requester_id  uuid not null references public.profiles (id) on delete cascade,
    addressee_id  uuid not null references public.profiles (id) on delete cascade,
    -- sorted user ids, one row per pair whichever side asked
    pair_key      varchar(73) not null generated always as (
        least(requester_id, addressee_id)::text || ':' || greatest(requester_id, addressee_id)::text
    ) stored,
    state         text not null default 'pending'
        constraint connections_state_check check (state in ('pending', 'connected', 'declined')),
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now(),
    constraint connections_not_self check (requester_id <> addressee_id)
);

create unique index connections_pair_key_key on public.connections (pair_key);
create index connections_requester_idx on public.connections (requester_id, state);
create index connections_addressee_idx on public.connections (addressee_id, state);

create trigger connections_touch_updated_at before update on public.connections
  for each row execute function public.touch_updated_at();

create table public.follows (
    id           uuid primary key default gen_random_uuid(),
    follower_id  uuid not null references public.profiles (id) on delete cascade,
    followee_id  uuid not null references public.profiles (id) on delete cascade,
    created_at   timestamptz not null default now(),
    constraint follows_not_self check (follower_id <> followee_id),
    constraint follows_pair_key unique (follower_id, followee_id)
);

create index follows_followee_idx on public.follows (followee_id);

create table public.mentorship_requests (
    id            uuid primary key default gen_random_uuid(),
    requester_id  uuid not null references public.profiles (id) on delete cascade,
    recipient_id  uuid not null references public.profiles (id) on delete cascade,
    topic         text not null
        constraint mentorship_requests_topic_check
        check (topic in ('career', 'system_design', 'referral', 'resume', 'grad_school')),
    message       varchar(1000) not null default '',
    status        text not null default 'open'
        constraint mentorship_requests_status_check check (status in ('open', 'accepted', 'declined')),
    created_at    timestamptz not null default now(),
    constraint mentorship_requests_not_self check (requester_id <> recipient_id)
);

create index mentorship_requests_recipient_idx on public.mentorship_requests (recipient_id, created_at desc);
create index mentorship_requests_requester_idx on public.mentorship_requests (requester_id, created_at desc);

-- ---------------------------------------------------------------------------
-- Block helpers (reused by feed, people and chat queries)
-- ---------------------------------------------------------------------------

-- Whether either user has blocked the other. For service and SECURITY
-- DEFINER code only (it reveals blocks between arbitrary users).
create or replace function public.blocked_between(p_a uuid, p_b uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (
    select 1 from public.blocks b
    where (b.blocker_id = p_a and b.blocked_id = p_b)
       or (b.blocker_id = p_b and b.blocked_id = p_a)
  )
$$;

-- Whether the caller and another user are blocked either way.
create or replace function public.is_blocked_with(p_user_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select public.blocked_between(auth.uid(), p_user_id)
$$;

-- Users the caller has blocked or been blocked by: exclude these from feed,
-- people and chat results, e.g. `author_id not in (select public.blocked_user_ids())`.
create or replace function public.blocked_user_ids()
returns setof uuid
language sql stable security definer set search_path = ''
as $$
  select b.blocked_id from public.blocks b where b.blocker_id = auth.uid()
  union
  select b.blocker_id from public.blocks b where b.blocked_id = auth.uid()
$$;

create or replace function public._pair_key(p_a uuid, p_b uuid)
returns text
language sql immutable set search_path = ''
as $$
  select least(p_a, p_b)::text || ':' || greatest(p_a, p_b)::text
$$;

-- ---------------------------------------------------------------------------
-- Row level security and privileges
-- ---------------------------------------------------------------------------

alter table public.blocks enable row level security;
alter table public.connections enable row level security;
alter table public.follows enable row level security;
alter table public.mentorship_requests enable row level security;

revoke all on public.blocks, public.connections, public.follows, public.mentorship_requests
  from anon, authenticated;

grant select on public.blocks, public.connections, public.follows, public.mentorship_requests
  to anon, authenticated;

-- blocks: only the blocker sees the row (a blocked user is never told).
-- Insert through block_user(); unblock is a plain delete of one's own row.
create policy blocks_select_own on public.blocks for select to authenticated
  using (blocker_id = auth.uid());
grant delete on public.blocks to authenticated;
create policy blocks_delete_own on public.blocks for delete to authenticated
  using (blocker_id = auth.uid());

-- connections: both parties see the pair row; either may delete it
-- (withdraw, cancel, remove: back to Idle). Inserts and state changes go
-- through send_connection_request() and respond_to_connection().
create policy connections_select_party on public.connections for select to authenticated
  using (requester_id = auth.uid() or addressee_id = auth.uid());
grant delete on public.connections to authenticated;
create policy connections_delete_party on public.connections for delete to authenticated
  using (requester_id = auth.uid() or addressee_id = auth.uid());

-- follows: follower and followee see the row; the follower may unfollow.
-- Inserts go through follow_user().
create policy follows_select_party on public.follows for select to authenticated
  using (follower_id = auth.uid() or followee_id = auth.uid());
grant delete on public.follows to authenticated;
create policy follows_delete_own on public.follows for delete to authenticated
  using (follower_id = auth.uid());

-- mentorship_requests: both parties read; the recipient sets status.
-- Inserts go through request_mentorship() so the notification is stored too.
create policy mentorship_requests_select_party on public.mentorship_requests for select to authenticated
  using (requester_id = auth.uid() or recipient_id = auth.uid());
grant update (status) on public.mentorship_requests to authenticated;
create policy mentorship_requests_update_recipient on public.mentorship_requests for update to authenticated
  using (recipient_id = auth.uid()) with check (recipient_id = auth.uid());

-- ---------------------------------------------------------------------------
-- RPCs
-- ---------------------------------------------------------------------------

-- Blocks a user: stores the block (idempotent) and deletes the pair's
-- connection and follows in both directions. PT400 self; PT404 unknown user.
create or replace function public.block_user(p_user_id uuid)
returns public.blocks
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_row public.blocks;
begin
  if v_me is null then
    raise exception 'Sign in required.' using errcode = 'PT401';
  end if;
  if p_user_id is null or p_user_id = v_me then
    raise exception 'You cannot block yourself.' using errcode = 'PT400';
  end if;
  if not exists (select 1 from public.profiles p where p.id = p_user_id) then
    raise exception 'User not found.' using errcode = 'PT404';
  end if;

  insert into public.blocks (blocker_id, blocked_id) values (v_me, p_user_id)
  on conflict (blocker_id, blocked_id) do nothing;
  select * into v_row from public.blocks b where b.blocker_id = v_me and b.blocked_id = p_user_id;

  delete from public.connections c where c.pair_key = public._pair_key(v_me, p_user_id);
  delete from public.follows f
  where (f.follower_id = v_me and f.followee_id = p_user_id)
     or (f.follower_id = p_user_id and f.followee_id = v_me);
  return v_row;
end;
$$;

-- Unblocks a user. The relationship stays Idle (blocking already removed
-- any connection). Returns whether a block was removed.
create or replace function public.unblock_user(p_user_id uuid)
returns boolean
language sql security definer set search_path = ''
as $$
  with d as (
    delete from public.blocks b where b.blocker_id = auth.uid() and b.blocked_id = p_user_id
    returning 1
  )
  select exists (select 1 from d)
$$;

-- Sends a connection request to a user. PT400 self; PT404 unknown user;
-- PT403 a block exists either way; PT409 the pair is already pending or
-- connected (a silently declined request still looks pending to its sender).
-- If the caller had declined the other user's request, that row is replaced
-- by the caller's new request.
create or replace function public.send_connection_request(p_addressee_id uuid)
returns public.connections
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_existing public.connections;
  v_row public.connections;
begin
  if v_me is null then
    raise exception 'Sign in required.' using errcode = 'PT401';
  end if;
  if p_addressee_id is null or p_addressee_id = v_me then
    raise exception 'You cannot connect with yourself.' using errcode = 'PT400';
  end if;
  if not exists (select 1 from public.profiles p where p.id = p_addressee_id) then
    raise exception 'User not found.' using errcode = 'PT404';
  end if;
  if public.blocked_between(v_me, p_addressee_id) then
    raise exception 'You cannot connect with this user.' using errcode = 'PT403';
  end if;

  perform pg_advisory_xact_lock(hashtextextended('pair:' || public._pair_key(v_me, p_addressee_id), 0));
  select * into v_existing from public.connections c
  where c.pair_key = public._pair_key(v_me, p_addressee_id);
  if found then
    if v_existing.state = 'declined' and v_existing.addressee_id = v_me then
      delete from public.connections c where c.id = v_existing.id;
    else
      raise exception 'A request is already pending or you are connected.' using errcode = 'PT409';
    end if;
  end if;

  insert into public.connections (requester_id, addressee_id) values (v_me, p_addressee_id)
  returning * into v_row;
  return v_row;
end;
$$;

-- Accept, decline (Ignore) or undo a decline; only the addressee may.
-- accept: pending|declined -> connected; decline: pending -> declined;
-- undo: declined -> pending. PT400 unknown action; PT404 not a party;
-- PT403 caller is the requester; PT409 transition not allowed from the
-- current state; PT403 when a block exists.
create or replace function public.respond_to_connection(p_connection_id uuid, p_action text)
returns public.connections
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_row public.connections;
  v_to text;
begin
  if p_action is null or p_action not in ('accept', 'decline', 'undo') then
    raise exception 'Action must be accept, decline or undo.' using errcode = 'PT400';
  end if;
  select * into v_row from public.connections c where c.id = p_connection_id for update;
  if not found or v_me is null or v_me not in (v_row.requester_id, v_row.addressee_id) then
    raise exception 'Request not found.' using errcode = 'PT404';
  end if;
  if v_row.addressee_id <> v_me then
    raise exception 'Only the person who received the request can respond.' using errcode = 'PT403';
  end if;
  if public.blocked_between(v_row.requester_id, v_row.addressee_id) then
    raise exception 'You cannot connect with this user.' using errcode = 'PT403';
  end if;

  v_to := case
    when p_action = 'accept' and v_row.state in ('pending', 'declined') then 'connected'
    when p_action = 'decline' and v_row.state = 'pending' then 'declined'
    when p_action = 'undo' and v_row.state = 'declined' then 'pending'
  end;
  if v_to is null then
    raise exception 'This request cannot be changed that way.' using errcode = 'PT409';
  end if;

  update public.connections c set state = v_to where c.id = v_row.id returning * into v_row;
  return v_row;
end;
$$;

-- The caller's relationship with another user: idle|sent|incoming|connected|blocked.
-- Being blocked by the other user reads as idle (the block is not revealed).
create or replace function public.connection_state(p_user_id uuid)
returns text
language sql stable security definer set search_path = ''
as $$
  select case
    when exists (select 1 from public.blocks b
                 where b.blocker_id = auth.uid() and b.blocked_id = p_user_id) then 'blocked'
    when public.blocked_between(auth.uid(), p_user_id) then 'idle'
    else coalesce((
      select case
               when c.state = 'connected' then 'connected'
               when c.requester_id = auth.uid() then 'sent'      -- pending or silently declined
               when c.state = 'pending' then 'incoming'
               else 'idle'                                      -- the caller declined it
             end
      from public.connections c
      where c.pair_key = public._pair_key(auth.uid(), p_user_id)
    ), 'idle')
  end
$$;

-- Follows a public profile (idempotent: returns the existing row).
-- PT400 self; PT404 unknown user; PT403 the profile is connections-only or a
-- block exists either way.
create or replace function public.follow_user(p_user_id uuid)
returns public.follows
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_visibility text;
  v_row public.follows;
begin
  if v_me is null then
    raise exception 'Sign in required.' using errcode = 'PT401';
  end if;
  if p_user_id is null or p_user_id = v_me then
    raise exception 'You cannot follow yourself.' using errcode = 'PT400';
  end if;
  select p.profile_visibility into v_visibility from public.profiles p where p.id = p_user_id;
  if not found then
    raise exception 'User not found.' using errcode = 'PT404';
  end if;
  if v_visibility <> 'public' or public.blocked_between(v_me, p_user_id) then
    raise exception 'You cannot follow this profile.' using errcode = 'PT403';
  end if;

  insert into public.follows (follower_id, followee_id) values (v_me, p_user_id)
  on conflict (follower_id, followee_id) do nothing;
  select * into v_row from public.follows f where f.follower_id = v_me and f.followee_id = p_user_id;
  return v_row;
end;
$$;

-- Follower and following counts for a profile.
create or replace function public.follow_counts(p_user_id uuid)
returns table (followers integer, following integer)
language sql stable security definer set search_path = ''
as $$
  select (select count(*)::integer from public.follows f where f.followee_id = p_user_id),
         (select count(*)::integer from public.follows f where f.follower_id = p_user_id)
$$;

-- Sends a mentorship request and stores a 'mentorship' notification for the
-- recipient. PT400 no or unknown topic, self, or message over 1000 chars;
-- PT404 unknown recipient or a block exists either way.
create or replace function public.request_mentorship(p_recipient_id uuid, p_topic text, p_message text default '')
returns public.mentorship_requests
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_topic text := nullif(btrim(coalesce(p_topic, '')), '');
  v_row public.mentorship_requests;
begin
  if v_me is null then
    raise exception 'Sign in required.' using errcode = 'PT401';
  end if;
  if v_topic is null then
    raise exception 'Choose a topic.' using errcode = 'PT400';
  end if;
  if v_topic not in ('career', 'system_design', 'referral', 'resume', 'grad_school') then
    raise exception 'Unknown topic.' using errcode = 'PT400';
  end if;
  if char_length(coalesce(p_message, '')) > 1000 then
    raise exception 'Message is too long.' using errcode = 'PT400';
  end if;
  if p_recipient_id is null or p_recipient_id = v_me then
    raise exception 'You cannot request mentorship from yourself.' using errcode = 'PT400';
  end if;
  if not exists (select 1 from public.profiles p where p.id = p_recipient_id)
     or public.blocked_between(v_me, p_recipient_id) then
    raise exception 'User not found.' using errcode = 'PT404';
  end if;

  insert into public.mentorship_requests (requester_id, recipient_id, topic, message)
  values (v_me, p_recipient_id, v_topic, coalesce(p_message, ''))
  returning * into v_row;
  perform public.notify(p_recipient_id, 'mentorship',
    jsonb_build_object('mentorship_request_id', v_row.id, 'requester_id', v_me));
  return v_row;
end;
$$;

-- ---------------------------------------------------------------------------
-- Execute rights
-- ---------------------------------------------------------------------------

revoke all on function
  public.blocked_between(uuid, uuid), public.is_blocked_with(uuid), public.blocked_user_ids(),
  public._pair_key(uuid, uuid), public.block_user(uuid), public.unblock_user(uuid),
  public.send_connection_request(uuid), public.respond_to_connection(uuid, text),
  public.connection_state(uuid), public.follow_user(uuid), public.follow_counts(uuid),
  public.request_mentorship(uuid, text, text)
  from public, anon, authenticated;

grant execute on function public.blocked_between(uuid, uuid) to service_role;

grant execute on function
  public.is_blocked_with(uuid), public.blocked_user_ids(),
  public.block_user(uuid), public.unblock_user(uuid),
  public.send_connection_request(uuid), public.respond_to_connection(uuid, text),
  public.connection_state(uuid), public.follow_user(uuid), public.follow_counts(uuid),
  public.request_mentorship(uuid, text, text)
  to authenticated, service_role;
