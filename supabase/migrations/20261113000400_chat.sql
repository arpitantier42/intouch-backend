-- Chat: community-scoped threads (DM or group), participants with per-user
-- settings, messages, per-user message marks (star, reaction, hidden_for_me)
-- and call logs, plus the Supabase Realtime publication for these tables.
--
-- Scope: a thread belongs to exactly one community. A DM's dm_key is
-- `<community_id>:<least user>:<greatest user>` (unique), so the same two
-- people have one DM per community and two DMs across two communities.
-- Access needs an active participant row (left_at is null) AND a joined
-- membership in the thread's community, so leaving or being removed from a
-- community cuts chat access at once.
--
-- Writes: clients may only update their own participant settings
-- (read/delivered marks, mute, alerts, sound, clear, delete chat). Threads,
-- participants, messages, marks and call logs are inserted by the chat RPCs
-- (SECURITY DEFINER, later tickets). Triggers keep the denormalised state:
--   * a new message stamps expires_at from the thread's disappearing setting,
--     moves the thread to the top (last_message_at / preview), bumps the other
--     participants' unread_count, un-hides the chat for them, and resets the
--     sender's unread_count and last_read_at;
--   * advancing last_read_at recomputes unread_count (0 once fully read);
--   * delete-for-everyone (setting deleted_at) clears body, attachment,
--     reply, pin, and drops stars and reactions (hidden_for_me marks stay).
--
-- Visibility (RLS on messages): participants see a message unless it expired,
-- is older than their cleared_at, or they hid it (hidden_for_me). Stars and
-- hidden_for_me marks are visible to their owner only; reactions to every
-- participant.

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

create table public.threads (
    id                    uuid primary key default gen_random_uuid(),
    community_id          uuid not null references public.communities (id) on delete cascade,
    kind                  text not null
        constraint threads_kind_check check (kind in ('dm', 'group')),
    dm_key                varchar(110),
    name                  varchar(80),
    description           varchar(300),
    disappearing_seconds  integer
        constraint threads_disappearing_check check (disappearing_seconds in (86400, 604800, 7776000)),
    last_message_at       timestamptz,
    last_message_preview  varchar(140),
    created_by            uuid not null references public.profiles (id) on delete cascade,
    created_at            timestamptz not null default now(),
    constraint threads_dm_shape check (
        (kind = 'dm' and dm_key is not null and name is null)
        or (kind = 'group' and dm_key is null and name is not null and btrim(name) <> '')
    )
);

create unique index threads_dm_key_key on public.threads (dm_key);
-- chat list: a community's threads, most recent first
create index threads_community_recent_idx
  on public.threads (community_id, (coalesce(last_message_at, created_at)) desc);

create table public.thread_participants (
    id                 uuid primary key default gen_random_uuid(),
    thread_id          uuid not null references public.threads (id) on delete cascade,
    user_id            uuid not null references public.profiles (id) on delete cascade,
    unread_count       integer not null default 0
        constraint thread_participants_unread_check check (unread_count >= 0),
    last_delivered_at  timestamptz,
    last_read_at       timestamptz,
    muted_until        timestamptz,
    alert_mentions     boolean not null default true,
    alert_replies      boolean not null default true,
    alert_reactions    boolean not null default true,
    sound              boolean not null default true,
    cleared_at         timestamptz,
    hidden_at          timestamptz,
    left_at            timestamptz,
    joined_at          timestamptz not null default now(),
    constraint thread_participants_pair_key unique (thread_id, user_id)
);

create index thread_participants_user_idx on public.thread_participants (user_id, thread_id);

create table public.messages (
    id                uuid primary key default gen_random_uuid(),
    thread_id         uuid not null references public.threads (id) on delete cascade,
    sender_id         uuid not null references public.profiles (id) on delete cascade,
    kind              text not null default 'text'
        constraint messages_kind_check
        check (kind in ('text', 'image', 'video', 'document', 'location', 'contact', 'voice', 'gif')),
    body              varchar(4000)
        -- whitespace-only text is never stored
        constraint messages_body_not_blank check (body is null or body ~ '\S'),
    attachment_url    text,
    attachment_bytes  bigint
        constraint messages_attachment_bytes_check check (attachment_bytes between 0 and 26214400),
    reply_to_id       uuid references public.messages (id) on delete set null,
    forwarded         boolean not null default false,
    pinned_at         timestamptz,
    edited_at         timestamptz,
    deleted_at        timestamptz,
    expires_at        timestamptz,
    created_at        timestamptz not null default now(),
    -- live text needs a body; live media kinds need an attachment
    constraint messages_content_check check (
        deleted_at is not null
        or (kind = 'text' and body is not null)
        or (kind in ('image', 'video', 'document', 'voice', 'gif') and attachment_url is not null)
        or (kind in ('location', 'contact') and body is not null)
    ),
    -- delete-for-everyone tombstone keeps nothing
    constraint messages_tombstone_check check (
        deleted_at is null
        or (body is null and attachment_url is null and attachment_bytes is null
            and reply_to_id is null and pinned_at is null)
    )
);

create index messages_thread_created_idx on public.messages (thread_id, created_at, id);
create index messages_pinned_idx on public.messages (thread_id, pinned_at) where pinned_at is not null;
create index messages_expires_idx on public.messages (expires_at) where expires_at is not null;

create table public.message_marks (
    id          uuid primary key default gen_random_uuid(),
    message_id  uuid not null references public.messages (id) on delete cascade,
    user_id     uuid not null references public.profiles (id) on delete cascade,
    kind        text not null
        constraint message_marks_kind_check check (kind in ('star', 'reaction', 'hidden_for_me')),
    emoji       varchar(16),
    created_at  timestamptz not null default now(),
    constraint message_marks_emoji_check check (
        (kind = 'reaction' and emoji is not null and btrim(emoji) <> '')
        or (kind <> 'reaction' and emoji is null)
    ),
    -- one star, one reaction (replaced on re-react) and one hide per user
    constraint message_marks_one_per_kind unique (message_id, user_id, kind)
);

create index message_marks_user_kind_idx on public.message_marks (user_id, kind, created_at desc);

create table public.call_logs (
    id                uuid primary key default gen_random_uuid(),
    thread_id         uuid not null references public.threads (id) on delete cascade,
    caller_id         uuid not null references public.profiles (id) on delete cascade,
    media             text not null
        constraint call_logs_media_check check (media in ('audio', 'video')),
    outcome           text not null
        constraint call_logs_outcome_check check (outcome in ('completed', 'missed', 'declined', 'cancelled')),
    duration_seconds  integer,
    started_at        timestamptz not null default now(),
    constraint call_logs_duration_check check (
        (outcome = 'completed' and (duration_seconds is null or duration_seconds >= 0))
        or (outcome <> 'completed' and duration_seconds is null)
    )
);

create index call_logs_thread_started_idx on public.call_logs (thread_id, started_at desc);

-- ---------------------------------------------------------------------------
-- Helpers
-- ---------------------------------------------------------------------------

-- `<community>:<least user>:<greatest user>`, 110 chars.
create or replace function public._dm_key(p_community_id uuid, p_a uuid, p_b uuid)
returns text
language sql immutable set search_path = ''
as $$
  select p_community_id::text || ':' || public._pair_key(p_a, p_b)
$$;

-- Whether a user is an active participant of a thread AND joined to its
-- community. For service and SECURITY DEFINER code (send/open-DM RPCs).
create or replace function public.is_active_participant(p_thread_id uuid, p_user_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (
    select 1
    from public.thread_participants tp
    join public.threads t on t.id = tp.thread_id
    join public.memberships m
      on m.community_id = t.community_id and m.user_id = tp.user_id and m.status = 'joined'
    join public.communities c on c.id = t.community_id and c.deleted_at is null
    where tp.thread_id = p_thread_id
      and tp.user_id = p_user_id
      and tp.left_at is null
  )
$$;

-- The caller's access check used by every chat policy.
create or replace function public.is_thread_participant(p_thread_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select public.is_active_participant(p_thread_id, auth.uid())
$$;

-- The caller's cleared_at for a thread (-infinity when never cleared).
create or replace function public.thread_cleared_at(p_thread_id uuid)
returns timestamptz
language sql stable security definer set search_path = ''
as $$
  select coalesce(
    (select tp.cleared_at from public.thread_participants tp
     where tp.thread_id = p_thread_id and tp.user_id = auth.uid()),
    '-infinity'::timestamptz)
$$;

create or replace function public._message_thread_id(p_message_id uuid)
returns uuid
language sql stable security definer set search_path = ''
as $$
  select m.thread_id from public.messages m where m.id = p_message_id
$$;

-- Delivery state of a message as its sender sees it:
--   read       every other active participant's last_read_at >= created_at
--   delivered  every other active participant's last_delivered_at (or read) >= created_at
--   sent       otherwise
-- Null when the caller cannot see the message's thread.
create or replace function public.message_status(p_message_id uuid)
returns text
language sql stable security definer set search_path = ''
as $$
  select case
    when not public.is_thread_participant(m.thread_id) then null
    when not exists (
      select 1 from public.thread_participants tp
      where tp.thread_id = m.thread_id and tp.user_id <> m.sender_id and tp.left_at is null
        and (tp.last_read_at is null or tp.last_read_at < m.created_at)
    ) then 'read'
    when not exists (
      select 1 from public.thread_participants tp
      where tp.thread_id = m.thread_id and tp.user_id <> m.sender_id and tp.left_at is null
        and greatest(coalesce(tp.last_delivered_at, '-infinity'), coalesce(tp.last_read_at, '-infinity'))
            < m.created_at
    ) then 'delivered'
    else 'sent'
  end
  from public.messages m
  where m.id = p_message_id
$$;

-- Text shown in the chat list for a message.
create or replace function public._message_preview(p_kind text, p_body text, p_deleted boolean)
returns text
language sql immutable set search_path = ''
as $$
  select left(case
    when p_deleted then 'This message was deleted'
    when p_kind in ('text') then regexp_replace(btrim(p_body), '\s+', ' ', 'g')
    when p_body is not null and p_kind in ('image', 'video', 'document') then
      initcap(p_kind) || ': ' || regexp_replace(btrim(p_body), '\s+', ' ', 'g')
    when p_kind = 'voice' then 'Voice message'
    when p_kind = 'gif' then 'GIF'
    when p_kind = 'location' then 'Location'
    when p_kind = 'contact' then 'Contact'
    else initcap(p_kind)
  end, 140)
$$;

-- ---------------------------------------------------------------------------
-- Triggers
-- ---------------------------------------------------------------------------

create or replace function public._messages_before_insert()
returns trigger
language plpgsql security definer set search_path = ''
as $$
declare
  v_seconds integer;
begin
  new.created_at := coalesce(new.created_at, now());
  new.pinned_at := null;
  new.edited_at := null;
  new.deleted_at := null;
  select t.disappearing_seconds into v_seconds from public.threads t where t.id = new.thread_id;
  new.expires_at := case when v_seconds is null then null
                         else new.created_at + make_interval(secs => v_seconds) end;
  return new;
end;
$$;

create trigger messages_before_insert before insert on public.messages
  for each row execute function public._messages_before_insert();

-- New message: thread to the top, others' unread +1 (and a deleted chat
-- reappears), sender's unread reset and read up to the message.
create or replace function public._messages_after_insert()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  update public.threads t
     set last_message_at = new.created_at,
         last_message_preview = public._message_preview(new.kind, new.body, false)
   where t.id = new.thread_id
     and (t.last_message_at is null or t.last_message_at <= new.created_at);

  update public.thread_participants tp
     set unread_count = case when tp.user_id = new.sender_id then 0 else tp.unread_count + 1 end,
         last_read_at = case when tp.user_id = new.sender_id
                             then greatest(coalesce(tp.last_read_at, new.created_at), new.created_at)
                             else tp.last_read_at end,
         hidden_at = null
   where tp.thread_id = new.thread_id
     and tp.left_at is null;
  return new;
end;
$$;

create trigger messages_after_insert after insert on public.messages
  for each row execute function public._messages_after_insert();

-- Delete for everyone: tombstone the row whatever else the update says.
create or replace function public._messages_before_update()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  if old.deleted_at is not null then
    -- a tombstone stays a tombstone
    new.deleted_at := old.deleted_at;
  end if;
  if new.deleted_at is not null then
    new.body := null;
    new.attachment_url := null;
    new.attachment_bytes := null;
    new.reply_to_id := null;
    new.pinned_at := null;
  end if;
  new.thread_id := old.thread_id;
  new.sender_id := old.sender_id;
  new.created_at := old.created_at;
  return new;
end;
$$;

create trigger messages_before_update before update on public.messages
  for each row execute function public._messages_before_update();

create or replace function public._messages_after_delete_for_everyone()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  delete from public.message_marks mm
   where mm.message_id = new.id and mm.kind in ('star', 'reaction');
  update public.threads t
     set last_message_preview = public._message_preview(new.kind, null, true)
   where t.id = new.thread_id and t.last_message_at = new.created_at;
  return new;
end;
$$;

create trigger messages_after_delete_for_everyone after update of deleted_at on public.messages
  for each row when (old.deleted_at is null and new.deleted_at is not null)
  execute function public._messages_after_delete_for_everyone();

-- Opening a thread (advancing last_read_at) recomputes the unread count.
create or replace function public._thread_participants_before_update()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  if new.last_read_at is distinct from old.last_read_at then
    -- read marks only move forward
    if old.last_read_at is not null and (new.last_read_at is null or new.last_read_at < old.last_read_at) then
      new.last_read_at := old.last_read_at;
    end if;
    select count(*)::int into new.unread_count
      from public.messages m
     where m.thread_id = new.thread_id
       and m.sender_id <> new.user_id
       and m.deleted_at is null
       and m.created_at > coalesce(new.last_read_at, '-infinity');
  end if;
  if old.last_delivered_at is not null
     and (new.last_delivered_at is null or new.last_delivered_at < old.last_delivered_at) then
    new.last_delivered_at := old.last_delivered_at;
  end if;
  return new;
end;
$$;

create trigger thread_participants_before_update before update on public.thread_participants
  for each row execute function public._thread_participants_before_update();

-- ---------------------------------------------------------------------------
-- Row level security and privileges
-- ---------------------------------------------------------------------------

alter table public.threads enable row level security;
alter table public.thread_participants enable row level security;
alter table public.messages enable row level security;
alter table public.message_marks enable row level security;
alter table public.call_logs enable row level security;

revoke all on public.threads, public.thread_participants, public.messages,
  public.message_marks, public.call_logs
  from anon, authenticated;

grant select on public.threads, public.thread_participants, public.messages,
  public.message_marks, public.call_logs
  to anon, authenticated;

-- threads: active participants who are joined to the thread's community.
create policy threads_select_participant on public.threads for select to authenticated
  using (public.is_thread_participant(id));

-- thread_participants: everyone in the thread sees the roster (and the read
-- marks that drive delivery ticks); each user updates only their own
-- settings. unread_count and left_at are server-set.
create policy thread_participants_select on public.thread_participants for select to authenticated
  using (public.is_thread_participant(thread_id));
grant update (last_delivered_at, last_read_at, muted_until, alert_mentions, alert_replies,
              alert_reactions, sound, cleared_at, hidden_at)
  on public.thread_participants to authenticated;
create policy thread_participants_update_own on public.thread_participants for update to authenticated
  using (user_id = auth.uid() and public.is_thread_participant(thread_id))
  with check (user_id = auth.uid());

-- messages: not expired, not cleared by me, not hidden for me.
create policy messages_select_participant on public.messages for select to authenticated
  using (
    public.is_thread_participant(thread_id)
    and (expires_at is null or expires_at > now())
    and created_at > public.thread_cleared_at(thread_id)
    and not exists (
      select 1 from public.message_marks mm
      where mm.message_id = messages.id and mm.user_id = auth.uid() and mm.kind = 'hidden_for_me'
    )
  );

-- message_marks: my own marks; other people's reactions only.
create policy message_marks_select on public.message_marks for select to authenticated
  using (
    user_id = auth.uid()
    or (kind = 'reaction' and public.is_thread_participant(public._message_thread_id(message_id)))
  );

-- call_logs: participants (Calls tab).
create policy call_logs_select_participant on public.call_logs for select to authenticated
  using (public.is_thread_participant(thread_id));

-- ---------------------------------------------------------------------------
-- Realtime: broadcast committed chat rows; Realtime applies the RLS above
-- per subscriber, so only participants receive them.
-- ---------------------------------------------------------------------------

do $$
declare
  v_table text;
begin
  if not exists (select 1 from pg_publication where pubname = 'supabase_realtime') then
    create publication supabase_realtime;
  end if;
  foreach v_table in array array['threads', 'thread_participants', 'messages', 'message_marks', 'call_logs'] loop
    if not exists (
      select 1 from pg_publication_tables
      where pubname = 'supabase_realtime' and schemaname = 'public' and tablename = v_table
    ) then
      execute format('alter publication supabase_realtime add table public.%I', v_table);
    end if;
  end loop;
end
$$;

-- ---------------------------------------------------------------------------
-- Execute rights
-- ---------------------------------------------------------------------------

revoke all on function
  public._dm_key(uuid, uuid, uuid), public.is_active_participant(uuid, uuid),
  public.is_thread_participant(uuid), public.thread_cleared_at(uuid),
  public._message_thread_id(uuid), public.message_status(uuid),
  public._message_preview(text, text, boolean),
  public._messages_before_insert(), public._messages_after_insert(),
  public._messages_before_update(), public._messages_after_delete_for_everyone(),
  public._thread_participants_before_update()
  from public, anon, authenticated;

grant execute on function
  public._dm_key(uuid, uuid, uuid), public.is_active_participant(uuid, uuid),
  public._message_preview(text, text, boolean)
  to service_role;

grant execute on function
  public.is_thread_participant(uuid), public.thread_cleared_at(uuid),
  public._message_thread_id(uuid), public.message_status(uuid)
  to authenticated, service_role;
