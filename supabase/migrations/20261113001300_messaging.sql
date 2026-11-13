-- Messaging: send, page/catch-up + mark read, edit, delete, react/star/pin
-- (OP-050..054). SECURITY DEFINER RPCs called as the user; the triggers in
-- ..._chat.sql do the thread/unread bookkeeping and tombstoning.
-- Errors: PT400 bad input, PT403 not allowed, PT404 unknown/not visible,
-- PT409 message deleted, PT413 attachment too large.

-- The message as the caller sees it (participant, not expired, not cleared,
-- not hidden for me); PT404 otherwise. Tombstones are returned.
create or replace function public._my_message(p_message_id uuid)
returns public.messages
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_row public.messages;
begin
  select m.* into v_row from public.messages m where m.id = p_message_id;
  if v_row.id is null
     or not public.is_thread_participant(v_row.thread_id)
     or (v_row.expires_at is not null and v_row.expires_at <= now())
     or v_row.created_at <= public.thread_cleared_at(v_row.thread_id)
     or exists (select 1 from public.message_marks mm
                where mm.message_id = v_row.id and mm.user_id = auth.uid() and mm.kind = 'hidden_for_me') then
    raise exception 'Message not found.' using errcode = 'PT404';
  end if;
  return v_row;
end
$$;

-- The JSON a client renders for one message: the row plus delivery state
-- (own messages only), reactions, my star, and whether I may edit it.
create or replace function public._message_json(p_m public.messages)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select to_jsonb(p_m) || jsonb_build_object(
    'mine', p_m.sender_id = auth.uid(),
    'status', case when p_m.sender_id = auth.uid() then public.message_status(p_m.id) end,
    'can_edit', p_m.sender_id = auth.uid() and p_m.deleted_at is null
                and p_m.kind in ('text', 'image', 'video', 'document'),
    'starred', exists (select 1 from public.message_marks mm
                       where mm.message_id = p_m.id and mm.user_id = auth.uid() and mm.kind = 'star'),
    'reactions', coalesce((
      select jsonb_agg(jsonb_build_object('user_id', mm.user_id, 'emoji', mm.emoji) order by mm.created_at)
      from public.message_marks mm where mm.message_id = p_m.id and mm.kind = 'reaction'), '[]'::jsonb))
$$;

-- Send. Checks, in order: caller is an active participant joined to the
-- thread's community (PT403), the client's active scope is that community
-- (PT403), DM peer still joined and no block either way (PT403), content
-- (PT400) and attachment size (PT413). p_forward_of copies a message the
-- caller can see and marks the copy forwarded.
create or replace function public.send_message(
  p_thread_id uuid, p_scope_community_id uuid, p_kind text, p_body text,
  p_attachment_url text default null, p_attachment_bytes bigint default null,
  p_reply_to_id uuid default null, p_forwarded boolean default false,
  p_forward_of uuid default null)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_thread public.threads;
  v_peer uuid;
  v_src public.messages;
  v_kind text := coalesce(nullif(lower(btrim(p_kind)), ''), 'text');
  v_body text := p_body;
  v_url text := nullif(btrim(p_attachment_url), '');
  v_bytes bigint := p_attachment_bytes;
  v_fwd boolean := coalesce(p_forwarded, false);
  v_row public.messages;
begin
  select * into v_thread from public.threads where id = p_thread_id;
  if v_thread.id is null or not public.is_active_participant(p_thread_id, v_me) then
    raise exception 'Not a participant of this chat.' using errcode = 'PT403';
  end if;
  if p_scope_community_id is distinct from v_thread.community_id then
    raise exception 'This chat belongs to another community. Switch to it to send.' using errcode = 'PT403';
  end if;
  if v_thread.kind = 'dm' then
    select tp.user_id into v_peer from public.thread_participants tp
     where tp.thread_id = p_thread_id and tp.user_id <> v_me limit 1;
    if v_peer is null or not public._joined_member(v_thread.community_id, v_peer) then
      raise exception 'This person is no longer a member of this community.' using errcode = 'PT403';
    end if;
    if public.blocked_between(v_me, v_peer) then
      raise exception 'You can''t send messages in this chat.' using errcode = 'PT403';
    end if;
  end if;

  if p_forward_of is not null then
    v_src := public._my_message(p_forward_of);
    if v_src.deleted_at is not null then
      raise exception 'This message was deleted.' using errcode = 'PT409';
    end if;
    v_kind := v_src.kind; v_body := v_src.body;
    v_url := v_src.attachment_url; v_bytes := v_src.attachment_bytes;
    v_fwd := true;
  end if;

  if v_kind not in ('text', 'image', 'video', 'document', 'location', 'contact', 'voice', 'gif') then
    raise exception 'Unknown message kind.' using errcode = 'PT400';
  end if;
  if v_body is not null and v_body !~ '\S' then
    v_body := null;
  end if;
  if v_kind in ('text', 'location', 'contact') and v_body is null then
    raise exception 'Type a message.' using errcode = 'PT400';
  end if;
  if v_kind in ('image', 'video', 'document', 'voice', 'gif') and v_url is null then
    raise exception 'Attach a file to send.' using errcode = 'PT400';
  end if;
  if char_length(v_body) > 4000 then
    raise exception 'Messages can be at most 4000 characters.' using errcode = 'PT400';
  end if;
  if v_bytes is not null and v_bytes < 0 then
    raise exception 'Attachment size is invalid.' using errcode = 'PT400';
  end if;
  if v_bytes > 26214400 then
    raise exception 'Attachments can be at most 25 MB.' using errcode = 'PT413';
  end if;
  if p_reply_to_id is not null and not exists (
    select 1 from public.messages m
    where m.id = p_reply_to_id and m.thread_id = p_thread_id and m.deleted_at is null) then
    raise exception 'The message you replied to is not in this chat.' using errcode = 'PT400';
  end if;

  insert into public.messages (thread_id, sender_id, kind, body, attachment_url, attachment_bytes,
                               reply_to_id, forwarded)
  values (p_thread_id, v_me, v_kind, v_body, v_url, v_bytes, p_reply_to_id, v_fwd)
  returning * into v_row;
  return public._message_json(v_row);
end
$$;

-- Page a thread. view: all (default, includes tombstones) | pinned |
-- starred | media | links | documents; q searches live text. p_after =
-- reconnect catch-up (ascending, everything newer); otherwise the newest
-- p_limit before p_before, returned oldest first. view=all marks the
-- thread read and delivered for the caller (unread → 0 via trigger).
create or replace function public.list_messages(
  p_thread_id uuid, p_view text default 'all', p_q text default null,
  p_after timestamptz default null, p_before timestamptz default null, p_limit integer default 50)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_view text := coalesce(nullif(lower(btrim(p_view)), ''), 'all');
  v_q text := nullif(btrim(p_q), '');
  v_limit integer := least(greatest(coalesce(p_limit, 50), 1), 200);
  v_out jsonb;
begin
  if not public.is_thread_participant(p_thread_id) then
    raise exception 'Not a participant of this chat.' using errcode = 'PT403';
  end if;
  if v_view not in ('all', 'pinned', 'starred', 'media', 'links', 'documents') then
    raise exception 'View must be all, pinned, starred, media, links or documents.' using errcode = 'PT400';
  end if;

  with visible as (
    select m.* from public.messages m
    where m.thread_id = p_thread_id
      and (m.expires_at is null or m.expires_at > now())
      and m.created_at > public.thread_cleared_at(p_thread_id)
      and not exists (select 1 from public.message_marks mm
                      where mm.message_id = m.id and mm.user_id = v_me and mm.kind = 'hidden_for_me')
      and (p_after is null or m.created_at > p_after)
      and (p_before is null or m.created_at < p_before)
      and (v_view = 'all' and v_q is null or m.deleted_at is null)
      and (v_q is null or m.body ilike '%' || replace(replace(replace(v_q, '\', '\\'), '%', '\%'), '_', '\_') || '%')
      and case v_view
            when 'pinned' then m.pinned_at is not null
            when 'starred' then exists (select 1 from public.message_marks mm
                                        where mm.message_id = m.id and mm.user_id = v_me and mm.kind = 'star')
            when 'media' then m.kind in ('image', 'video', 'gif') and m.attachment_url is not null
            when 'documents' then m.kind = 'document' and m.attachment_url is not null
            when 'links' then m.body ~* 'https?://'
            else true end
  ), page as (
    select * from visible v
    order by case when p_after is null then v.created_at end desc,
             case when p_after is not null then v.created_at end asc, v.id
    limit case when p_after is null then v_limit else 1000 end
  )
  select coalesce(jsonb_agg(public._message_json(p::public.messages)
                            order by case when v_view = 'pinned' then p.pinned_at end desc,
                                     p.created_at, p.id), '[]'::jsonb)
    into v_out from page p;

  if v_view = 'all' and v_q is null then
    update public.thread_participants tp
       set last_read_at = now(), last_delivered_at = now()
     where tp.thread_id = p_thread_id and tp.user_id = v_me;
  end if;
  return v_out;
end
$$;

-- Edit own text/caption. Blank text leaves the message unchanged (200).
create or replace function public.edit_message(p_message_id uuid, p_body text)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_row public.messages := public._my_message(p_message_id);
begin
  if v_row.sender_id <> auth.uid() then
    raise exception 'You can only edit your own messages.' using errcode = 'PT403';
  end if;
  if v_row.deleted_at is not null then
    raise exception 'This message was deleted.' using errcode = 'PT409';
  end if;
  if v_row.kind not in ('text', 'image', 'video', 'document') then
    raise exception 'This message can''t be edited.' using errcode = 'PT400';
  end if;
  if p_body is null or p_body !~ '\S' then
    return public._message_json(v_row);
  end if;
  if char_length(p_body) > 4000 then
    raise exception 'Messages can be at most 4000 characters.' using errcode = 'PT400';
  end if;
  update public.messages set body = p_body, edited_at = now()
   where id = p_message_id returning * into v_row;
  update public.threads t
     set last_message_preview = public._message_preview(v_row.kind, v_row.body, false)
   where t.id = v_row.thread_id and t.last_message_at = v_row.created_at;
  return public._message_json(v_row);
end
$$;

-- Delete for me (hidden_for_me mark; tombstones may be hidden too) or for
-- everyone (author only; the trigger clears body, attachment, reply, pin,
-- stars and reactions).
create or replace function public.delete_message(p_message_id uuid, p_scope text)
returns void
language plpgsql security definer set search_path = ''
as $$
declare
  v_scope text := lower(btrim(coalesce(p_scope, '')));
  v_row public.messages;
begin
  if v_scope not in ('me', 'everyone') then
    raise exception 'Scope must be me or everyone.' using errcode = 'PT400';
  end if;
  v_row := public._my_message(p_message_id);
  if v_scope = 'me' then
    insert into public.message_marks (message_id, user_id, kind)
    values (p_message_id, auth.uid(), 'hidden_for_me')
    on conflict (message_id, user_id, kind) do nothing;
    return;
  end if;
  if v_row.sender_id <> auth.uid() then
    raise exception 'You can only delete your own messages for everyone.' using errcode = 'PT403';
  end if;
  if v_row.deleted_at is not null then
    raise exception 'This message was already deleted.' using errcode = 'PT409';
  end if;
  update public.messages set deleted_at = now() where id = p_message_id;
end
$$;

-- React/unreact, star/unstar, pin/unpin. Returns a MessageMark-shaped
-- object: the mark (kind reaction|star) or, for pins, kind 'pin' with
-- created_at = pinned_at; `applied` is false when nothing changed.
create or replace function public.mark_message(p_message_id uuid, p_action text, p_emoji text default null)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_action text := lower(btrim(coalesce(p_action, '')));
  v_emoji text := nullif(btrim(p_emoji), '');
  v_row public.messages;
  v_mark public.message_marks;
  v_n integer;
begin
  if v_action not in ('react', 'unreact', 'star', 'unstar', 'pin', 'unpin') then
    raise exception 'Action must be react, unreact, star, unstar, pin or unpin.' using errcode = 'PT400';
  end if;
  v_row := public._my_message(p_message_id);
  if v_row.deleted_at is not null then
    raise exception 'This message was deleted.' using errcode = 'PT409';
  end if;

  if v_action = 'react' then
    if v_emoji is null or char_length(v_emoji) > 16 then
      raise exception 'Choose an emoji to react with.' using errcode = 'PT400';
    end if;
    insert into public.message_marks (message_id, user_id, kind, emoji)
    values (p_message_id, v_me, 'reaction', v_emoji)
    on conflict (message_id, user_id, kind) do update set emoji = excluded.emoji
    returning * into v_mark;
    return to_jsonb(v_mark) || jsonb_build_object('applied', true);
  elsif v_action = 'star' then
    insert into public.message_marks (message_id, user_id, kind)
    values (p_message_id, v_me, 'star')
    on conflict (message_id, user_id, kind) do nothing;
    get diagnostics v_n = row_count;
    select * into v_mark from public.message_marks
     where message_id = p_message_id and user_id = v_me and kind = 'star';
    return to_jsonb(v_mark) || jsonb_build_object('applied', v_n > 0);
  elsif v_action in ('unreact', 'unstar') then
    delete from public.message_marks
     where message_id = p_message_id and user_id = v_me
       and kind = case v_action when 'unreact' then 'reaction' else 'star' end
    returning * into v_mark;
    if v_mark.id is null then
      return jsonb_build_object('message_id', p_message_id, 'user_id', v_me,
        'kind', case v_action when 'unreact' then 'reaction' else 'star' end,
        'emoji', null, 'created_at', null, 'applied', false);
    end if;
    return to_jsonb(v_mark) || jsonb_build_object('applied', true);
  end if;

  -- pin / unpin: any participant; the banner lists pinned_at desc.
  update public.messages
     set pinned_at = case when v_action = 'pin' then coalesce(pinned_at, now()) end
   where id = p_message_id
     and (pinned_at is null) = (v_action = 'pin')
  returning * into v_row;
  get diagnostics v_n = row_count;
  if v_n = 0 then
    select * into v_row from public.messages where id = p_message_id;
  end if;
  return jsonb_build_object('id', null, 'message_id', p_message_id, 'user_id', v_me, 'kind', 'pin',
    'emoji', null, 'created_at', v_row.pinned_at, 'pinned', v_row.pinned_at is not null,
    'applied', v_n > 0);
end
$$;

-- The push alert for a message the caller just sent: recipients are the
-- other active participants (joined to the community), not muted right now
-- (an expired mute alerts again without any action) and not blocked either
-- way with the sender. Returns {user_ids, thread_id, message_id, title, body}.
create or replace function public.message_push(p_message_id uuid)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select jsonb_build_object(
    'user_ids', coalesce((
      select jsonb_agg(tp.user_id) from public.thread_participants tp
      where tp.thread_id = m.thread_id
        and tp.user_id <> m.sender_id
        and public.is_active_participant(m.thread_id, tp.user_id)
        and (tp.muted_until is null or tp.muted_until <= now())
        and not public.blocked_between(m.sender_id, tp.user_id)), '[]'::jsonb),
    'thread_id', m.thread_id,
    'message_id', m.id,
    'title', coalesce(case when t.kind = 'group' then t.name end, p.full_name),
    'body', case when t.kind = 'group' then p.full_name || ': ' else '' end
            || public._message_preview(m.kind, m.body, false))
  from public.messages m
  join public.threads t on t.id = m.thread_id
  join public.profiles p on p.id = m.sender_id
  where m.id = p_message_id and m.sender_id = auth.uid()
$$;

revoke all on function
  public._my_message(uuid), public._message_json(public.messages),
  public.send_message(uuid, uuid, text, text, text, bigint, uuid, boolean, uuid),
  public.list_messages(uuid, text, text, timestamptz, timestamptz, integer),
  public.edit_message(uuid, text), public.delete_message(uuid, text),
  public.mark_message(uuid, text, text), public.message_push(uuid)
  from public, anon, authenticated;
grant execute on function
  public.send_message(uuid, uuid, text, text, text, bigint, uuid, boolean, uuid),
  public.list_messages(uuid, text, text, timestamptz, timestamptz, integer),
  public.edit_message(uuid, text), public.delete_message(uuid, text),
  public.mark_message(uuid, text, text), public.message_push(uuid)
  to authenticated, service_role;
