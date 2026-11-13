-- Chat threads API: chat list, open-or-create DM, groups, adding people,
-- per-participant settings (mute, alerts, clear, delete chat, exit,
-- disappearing), thread info and call logging. All SECURITY DEFINER,
-- callable by authenticated users; every function checks the caller itself.
-- Errors: PT400 bad input, PT403 not allowed, PT404 unknown.

-- Whether two users are both joined to a live community.
create or replace function public._joined_member(p_community_id uuid, p_user_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (
    select 1 from public.memberships m
    join public.communities c on c.id = m.community_id and c.deleted_at is null
    where m.community_id = p_community_id and m.user_id = p_user_id and m.status = 'joined')
$$;

-- Chat list for one community: the caller's active, not-deleted threads,
-- most recent first. p_filter all|unread|groups|calls; p_q matches title,
-- preview, subtitle and the community label (case-insensitive).
create or replace function public.list_threads(p_community_id uuid, p_filter text default 'all', p_q text default null)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_filter text := coalesce(nullif(lower(btrim(p_filter)), ''), 'all');
  v_q text := nullif(btrim(coalesce(p_q, '')), '');
begin
  if v_filter not in ('all', 'unread', 'groups', 'calls') then
    raise exception 'Filter must be all, unread, groups or calls.' using errcode = 'PT400';
  end if;
  if not public._joined_member(p_community_id, v_me) then
    raise exception 'Members only.' using errcode = 'PT403';
  end if;

  return coalesce((
    select jsonb_agg(x.item order by x.sort_at desc, x.id)
    from (
      select t.id, coalesce(t.last_message_at, t.created_at) as sort_at,
        to_jsonb(t) || jsonb_build_object(
          'title', s.title,
          'subtitle', s.subtitle,
          'avatar_url', s.avatar_url,
          'community_label', c.name,
          'unread_count', tp.unread_count,
          'muted_until', tp.muted_until,
          'muted', coalesce(tp.muted_until > now(), false),
          'member_count', s.member_count,
          'peer', s.peer,
          'last_call', lc.call) as item
      from public.thread_participants tp
      join public.threads t on t.id = tp.thread_id
      join public.communities c on c.id = t.community_id
      left join lateral (
        select p.id, p.full_name, p.avatar_url, p.headline, p.company, p.activity_status
        from public.thread_participants op
        join public.profiles p on p.id = op.user_id
        where t.kind = 'dm' and op.thread_id = t.id and op.user_id <> v_me
        limit 1
      ) peer on true
      cross join lateral (
        select
          case when t.kind = 'group' then t.name else peer.full_name end as title,
          case when t.kind = 'group' then t.description
               else nullif(concat_ws(' · ', peer.headline, peer.company), '') end as subtitle,
          case when t.kind = 'dm' then peer.avatar_url end as avatar_url,
          (select count(*)::int from public.thread_participants ap
            where ap.thread_id = t.id and ap.left_at is null) as member_count,
          case when t.kind = 'dm' then jsonb_build_object(
            'id', peer.id, 'full_name', peer.full_name, 'avatar_url', peer.avatar_url,
            'headline', peer.headline, 'company', peer.company,
            'activity_status', peer.activity_status) end as peer
      ) s
      left join lateral (
        select to_jsonb(cl) as call, cl.started_at
        from public.call_logs cl where cl.thread_id = t.id
        order by cl.started_at desc limit 1
      ) lc on true
      where tp.user_id = v_me
        and tp.left_at is null
        and tp.hidden_at is null
        and t.community_id = p_community_id
        -- a DM with someone blocked either way is not listed
        and (t.kind = 'group' or peer.id is null or not public.blocked_between(v_me, peer.id))
        and (v_filter <> 'unread' or tp.unread_count > 0)
        and (v_filter <> 'groups' or t.kind = 'group')
        and (v_filter <> 'calls' or lc.call is not null)
        and (v_q is null
             or strpos(lower(coalesce(s.title, '')), lower(v_q)) > 0
             or strpos(lower(coalesce(t.last_message_preview, '')), lower(v_q)) > 0
             or strpos(lower(coalesce(s.subtitle, '')), lower(v_q)) > 0
             or strpos(lower(c.name), lower(v_q)) > 0)
    ) x
  ), '[]'::jsonb);
end
$$;

-- Opens the caller's DM with p_user_id in this community, creating it once
-- (dm_key is unique, so concurrent calls converge on one thread).
create or replace function public.open_dm(p_community_id uuid, p_user_id uuid)
returns public.threads
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_key text;
  v_row public.threads;
begin
  if p_user_id is null or p_user_id = v_me then
    raise exception 'Choose someone else to message.' using errcode = 'PT400';
  end if;
  if not public._joined_member(p_community_id, v_me) then
    raise exception 'Members only.' using errcode = 'PT403';
  end if;
  if not public._joined_member(p_community_id, p_user_id) then
    raise exception 'You can only message members of this community.' using errcode = 'PT403';
  end if;
  if public.blocked_between(v_me, p_user_id) then
    raise exception 'You can''t message this person.' using errcode = 'PT403';
  end if;

  v_key := public._dm_key(p_community_id, v_me, p_user_id);
  insert into public.threads (community_id, kind, dm_key, created_by)
  values (p_community_id, 'dm', v_key, v_me)
  on conflict (dm_key) do nothing;
  select * into v_row from public.threads where dm_key = v_key;

  insert into public.thread_participants (thread_id, user_id)
  values (v_row.id, v_me), (v_row.id, p_user_id)
  on conflict (thread_id, user_id) do nothing;
  -- reopening a deleted chat brings it back for the caller
  update public.thread_participants set hidden_at = null
   where thread_id = v_row.id and user_id = v_me and hidden_at is not null;
  return v_row;
end
$$;

-- Validates group members: at least one other joined, unblocked member.
create or replace function public._check_group_members(p_community_id uuid, p_user_ids uuid[])
returns uuid[]
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_ids uuid[];
  v_id uuid;
begin
  select coalesce(array_agg(distinct u), '{}') into v_ids
  from unnest(coalesce(p_user_ids, '{}'::uuid[])) u where u is not null and u <> v_me;
  if cardinality(v_ids) = 0 then
    raise exception 'Select at least one member.' using errcode = 'PT400';
  end if;
  if cardinality(v_ids) > 255 then
    raise exception 'A group can have at most 256 members.' using errcode = 'PT400';
  end if;
  foreach v_id in array v_ids loop
    if not public._joined_member(p_community_id, v_id) then
      raise exception 'Only members of this community can be added.' using errcode = 'PT403';
    end if;
    if public.blocked_between(v_me, v_id) then
      raise exception 'You can''t add this person.' using errcode = 'PT403';
    end if;
  end loop;
  return v_ids;
end
$$;

-- Creates a group with the caller and the selected members.
create or replace function public.create_group(p_community_id uuid, p_name text, p_description text, p_user_ids uuid[])
returns public.threads
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_ids uuid[];
  v_row public.threads;
begin
  if p_name is null or btrim(p_name) = '' then
    raise exception 'Enter a group name.' using errcode = 'PT400';
  end if;
  if not public._joined_member(p_community_id, v_me) then
    raise exception 'Members only.' using errcode = 'PT403';
  end if;
  v_ids := public._check_group_members(p_community_id, p_user_ids);

  insert into public.threads (community_id, kind, name, description, created_by)
  values (p_community_id, 'group', btrim(p_name), nullif(btrim(coalesce(p_description, '')), ''), v_me)
  returning * into v_row;
  insert into public.thread_participants (thread_id, user_id)
  select v_row.id, u from unnest(array_prepend(v_me, v_ids)) u;
  return v_row;
end
$$;

-- Adds community members to a group the caller is active in. Someone who
-- exited earlier is re-added (joined_at reset, history before it cleared).
create or replace function public.add_participants(p_thread_id uuid, p_user_ids uuid[])
returns setof public.thread_participants
language plpgsql security definer set search_path = ''
as $$
declare
  v_thread public.threads;
  v_ids uuid[];
begin
  select * into v_thread from public.threads where id = p_thread_id;
  if not found or not public.is_active_participant(p_thread_id, auth.uid()) then
    raise exception 'Not a participant of this chat.' using errcode = 'PT403';
  end if;
  if v_thread.kind <> 'group' then
    raise exception 'People can only be added to groups.' using errcode = 'PT400';
  end if;
  v_ids := public._check_group_members(v_thread.community_id, p_user_ids);

  insert into public.thread_participants (thread_id, user_id)
  select p_thread_id, u from unnest(v_ids) u
  on conflict (thread_id, user_id) do update
    set left_at = null, joined_at = now(), cleared_at = now(), hidden_at = null, unread_count = 0
    where public.thread_participants.left_at is not null;

  return query select * from public.thread_participants
    where thread_id = p_thread_id and user_id = any (v_ids) order by joined_at, id;
end
$$;

-- Updates the caller's own settings in a thread. p_settings keys (all
-- optional): muted_until (timestamptz or null), mute (off|1h|8h|1w|always),
-- alert_mentions, alert_replies, alert_reactions, sound (booleans),
-- action (clear|delete|exit). p_set_disappearing applies p_disappearing
-- (null/0 = off) to the thread, for later messages only.
create or replace function public.update_thread_settings(
  p_thread_id uuid, p_settings jsonb, p_set_disappearing boolean default false, p_disappearing integer default null)
returns public.thread_participants
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_s jsonb := coalesce(p_settings, '{}'::jsonb);
  v_row public.thread_participants;
  v_kind text;
  v_mute text;
  v_action text;
begin
  if jsonb_typeof(v_s) <> 'object' then
    raise exception 'Settings must be a JSON object.' using errcode = 'PT400';
  end if;
  select * into v_row from public.thread_participants
   where thread_id = p_thread_id and user_id = v_me for update;
  if not found or not public.is_active_participant(p_thread_id, v_me) then
    raise exception 'Not a participant of this chat.' using errcode = 'PT403';
  end if;
  select kind into v_kind from public.threads where id = p_thread_id;

  if v_s ? 'mute' then
    v_mute := lower(btrim(v_s->>'mute'));
    v_row.muted_until := case v_mute
      when 'off' then null
      when '1h' then now() + interval '1 hour'
      when '8h' then now() + interval '8 hours'
      when '1w' then now() + interval '7 days'
      when 'always' then '9999-12-31T00:00:00Z'::timestamptz
      else null end;
    if v_mute is null or v_mute not in ('off', '1h', '8h', '1w', 'always') then
      raise exception 'Mute must be off, 1h, 8h, 1w or always.' using errcode = 'PT400';
    end if;
  elsif v_s ? 'muted_until' then
    v_row.muted_until := (v_s->>'muted_until')::timestamptz;
  end if;
  if v_s ? 'alert_mentions' then v_row.alert_mentions := (v_s->>'alert_mentions')::boolean; end if;
  if v_s ? 'alert_replies' then v_row.alert_replies := (v_s->>'alert_replies')::boolean; end if;
  if v_s ? 'alert_reactions' then v_row.alert_reactions := (v_s->>'alert_reactions')::boolean; end if;
  if v_s ? 'sound' then v_row.sound := (v_s->>'sound')::boolean; end if;

  v_action := nullif(lower(btrim(coalesce(v_s->>'action', ''))), '');
  if v_action is not null and v_action not in ('clear', 'delete', 'exit') then
    raise exception 'Action must be clear, delete or exit.' using errcode = 'PT400';
  end if;
  if v_action = 'exit' and v_kind <> 'group' then
    raise exception 'Only groups can be exited.' using errcode = 'PT400';
  end if;
  if v_action in ('clear', 'delete') then
    -- my copy only: the other participants keep their history
    v_row.cleared_at := now();
    v_row.unread_count := 0;
  end if;
  if v_action = 'delete' then
    v_row.hidden_at := now();
  end if;
  if v_action = 'exit' then
    v_row.left_at := now();
    v_row.unread_count := 0;
  end if;

  if p_set_disappearing then
    if p_disappearing is not null and p_disappearing <> 0
       and p_disappearing not in (86400, 604800, 7776000) then
      raise exception 'Disappearing messages must be off, 24 hours, 7 days or 90 days.' using errcode = 'PT400';
    end if;
    -- expires_at is stamped at insert, so earlier messages are unaffected
    update public.threads set disappearing_seconds = nullif(p_disappearing, 0) where id = p_thread_id;
  end if;

  update public.thread_participants
     set muted_until = v_row.muted_until, alert_mentions = v_row.alert_mentions,
         alert_replies = v_row.alert_replies, alert_reactions = v_row.alert_reactions,
         sound = v_row.sound, cleared_at = v_row.cleared_at, hidden_at = v_row.hidden_at,
         left_at = v_row.left_at, unread_count = v_row.unread_count
   where id = v_row.id
  returning * into v_row;
  return v_row;
end
$$;

-- Messages the caller can see in a thread (mirrors the messages RLS policy).
create or replace function public._visible_messages(p_thread_id uuid)
returns setof public.messages
language sql stable security definer set search_path = ''
as $$
  select m.* from public.messages m
  where m.thread_id = p_thread_id
    and m.deleted_at is null
    and (m.expires_at is null or m.expires_at > now())
    and m.created_at > public.thread_cleared_at(p_thread_id)
    and not exists (select 1 from public.message_marks mm
                    where mm.message_id = m.id and mm.user_id = auth.uid() and mm.kind = 'hidden_for_me')
$$;

-- Thread info: thread fields, active participants with profile basics, the
-- DM peer, member count, shared media/link/document counts and my settings.
-- Carries no encryption claim (end-to-end encryption is not implemented).
create or replace function public.get_thread(p_thread_id uuid)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_out jsonb;
begin
  if not public.is_active_participant(p_thread_id, v_me) then
    raise exception 'Not a participant of this chat.' using errcode = 'PT403';
  end if;
  select to_jsonb(t) || jsonb_build_object(
    'community_label', c.name,
    'participants', coalesce((
      select jsonb_agg(jsonb_build_object(
        'user_id', p.id, 'full_name', p.full_name, 'avatar_url', p.avatar_url,
        'headline', p.headline, 'company', p.company, 'joined_at', tp.joined_at,
        'is_me', p.id = v_me) order by tp.joined_at, p.id)
      from public.thread_participants tp join public.profiles p on p.id = tp.user_id
      where tp.thread_id = t.id and tp.left_at is null), '[]'::jsonb),
    'member_count', (select count(*)::int from public.thread_participants tp
                     where tp.thread_id = t.id and tp.left_at is null),
    'peer', case when t.kind = 'dm' then (
      select jsonb_build_object('id', p.id, 'full_name', p.full_name, 'avatar_url', p.avatar_url,
                                'headline', p.headline, 'company', p.company)
      from public.thread_participants tp join public.profiles p on p.id = tp.user_id
      where tp.thread_id = t.id and tp.user_id <> v_me limit 1) end,
    'shared_counts', (
      select jsonb_build_object(
        'media', count(*) filter (where m.kind in ('image', 'video', 'gif') and m.attachment_url is not null),
        'documents', count(*) filter (where m.kind = 'document' and m.attachment_url is not null),
        'links', count(*) filter (where m.body ~* 'https?://'))
      from public._visible_messages(t.id) m),
    'me', (select to_jsonb(tp) from public.thread_participants tp
           where tp.thread_id = t.id and tp.user_id = v_me))
  into v_out
  from public.threads t join public.communities c on c.id = t.community_id
  where t.id = p_thread_id;
  return v_out;
end
$$;

-- Records a call attempt by the caller in a thread they are active in.
create or replace function public.log_call(
  p_thread_id uuid, p_media text, p_outcome text, p_duration integer default null, p_started_at timestamptz default null)
returns public.call_logs
language plpgsql security definer set search_path = ''
as $$
declare
  v_row public.call_logs;
begin
  if not public.is_active_participant(p_thread_id, auth.uid()) then
    raise exception 'Not a participant of this chat.' using errcode = 'PT403';
  end if;
  if p_media is null or p_media not in ('audio', 'video') then
    raise exception 'Media must be audio or video.' using errcode = 'PT400';
  end if;
  if p_outcome is null or p_outcome not in ('completed', 'missed', 'declined', 'cancelled') then
    raise exception 'Outcome must be completed, missed, declined or cancelled.' using errcode = 'PT400';
  end if;
  if p_outcome <> 'completed' and p_duration is not null then
    raise exception 'Only completed calls have a duration.' using errcode = 'PT400';
  end if;
  insert into public.call_logs (thread_id, caller_id, media, outcome, duration_seconds, started_at)
  values (p_thread_id, auth.uid(), p_media, p_outcome, p_duration,
          least(coalesce(p_started_at, now()), now()))
  returning * into v_row;
  return v_row;
end
$$;

revoke all on function
  public._joined_member(uuid, uuid), public._check_group_members(uuid, uuid[]),
  public._visible_messages(uuid),
  public.list_threads(uuid, text, text), public.open_dm(uuid, uuid),
  public.create_group(uuid, text, text, uuid[]), public.add_participants(uuid, uuid[]),
  public.update_thread_settings(uuid, jsonb, boolean, integer), public.get_thread(uuid),
  public.log_call(uuid, text, text, integer, timestamptz)
  from public, anon, authenticated;
grant execute on function
  public.list_threads(uuid, text, text), public.open_dm(uuid, uuid),
  public.create_group(uuid, text, text, uuid[]), public.add_participants(uuid, uuid[]),
  public.update_thread_settings(uuid, jsonb, boolean, integer), public.get_thread(uuid),
  public.log_call(uuid, text, text, integer, timestamptz)
  to authenticated, service_role;
grant execute on function public._joined_member(uuid, uuid) to service_role;
