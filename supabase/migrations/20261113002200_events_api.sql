-- Events API RPCs (listEvents, createEvent, getEvent, getEventLanding,
-- updateEvent, deleteEvent). All SECURITY DEFINER with explicit checks.
--
--   _event_json(e)        event row (join_url only for the creator or a
--                         confirmed seat) + item_kind 'event', my_rsvp,
--                         badge ('Created by you' | _rsvp_badge | null),
--                         going_count, seats_left, seats_left_label,
--                         duration_minutes, mine, post_id
--   list_events(cid, tab, q)  jsonb array; PT403 'Members only'.
--       tab upcoming (default/all: not ended) | event | raise (raise cards)
--           | my_rsvps (caller has an RSVP) | created | paid
--       q matches title, venue or a tag (case-insensitive substring)
--   create_event(...)     event + feed post (kind 'event') + target, one txn;
--                         PT400 with the composer's messages, PT403 not joined
--   get_event(id)         PT404 unknown/deleted, PT403 'Members only';
--                         _event_json + host + attendees (going, blocks hidden)
--   event_landing(id)     anon-callable preview; PT404 unknown/deleted,
--                         PT403 private community; no join/payment links
--   update_event(id, patch)  PT404 not visible, PT403 not the creator
--   delete_event(id)      soft delete (+ the feed post, pending reminders);
--                         notifies going attendees ('event_deleted');
--                         returns {id, title, community_id, notified[]}

create or replace function public._event_json(p public.events)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select (to_jsonb(p) - 'join_url') || jsonb_build_object(
    'item_kind', 'event',
    'join_url', case when p.creator_id = auth.uid() or coalesce(r.seat_confirmed, false) then p.join_url end,
    'my_rsvp', case when r.id is not null then to_jsonb(r) end,
    'badge', case
       when p.creator_id = auth.uid() then 'Created by you'
       when r.id is not null then public._rsvp_badge(r.status, r.payment_status, r.seat_confirmed) end,
    'going_count', (select count(*) from public.rsvps g where g.event_id = p.id and g.status = 'going'),
    'seats_left', case when p.seat_cap is null then null else greatest(p.seat_cap - p.confirmed_count, 0) end,
    'seats_left_label', case
       when p.seat_cap is null then null
       when p.confirmed_count >= p.seat_cap then 'Event full'
       when p.seat_cap - p.confirmed_count = 1 then '1 seat left'
       else (p.seat_cap - p.confirmed_count)::text || ' seats left' end,
    'duration_minutes', (extract(epoch from p.ends_at - p.starts_at) / 60)::integer,
    'mine', p.creator_id = auth.uid(),
    'post_id', (select po.id from public.posts po
                 where po.event_id = p.id and po.deleted_at is null
                 order by po.created_at limit 1))
  from (select 1) one
  left join public.rsvps r on r.event_id = p.id and r.user_id = auth.uid()
$$;

create or replace function public.list_events(p_community_id uuid, p_tab text default null, p_q text default null)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_tab text := lower(coalesce(nullif(btrim(p_tab), ''), 'upcoming'));
  v_q text := nullif(btrim(coalesce(p_q, '')), '');
  v_pat text;
  v_out jsonb;
begin
  if v_tab = 'all' then
    v_tab := 'upcoming';
  end if;
  if v_tab not in ('upcoming', 'event', 'raise', 'my_rsvps', 'created', 'paid') then
    raise exception 'Tab must be upcoming, event, raise, my_rsvps, created or paid' using errcode = 'PT400';
  end if;
  if v_uid is null or not public.is_joined(p_community_id)
     or not exists (select 1 from public.communities c where c.id = p_community_id and c.deleted_at is null) then
    raise exception 'Members only' using errcode = 'PT403';
  end if;
  v_pat := '%' || replace(replace(replace(coalesce(v_q, ''), '\', '\\'), '%', '\%'), '_', '\_') || '%';

  if v_tab = 'raise' then
    select coalesce(jsonb_agg(x.j order by x.created_at desc, x.id desc), '[]'::jsonb) into v_out
    from (
      select r.id, r.created_at, jsonb_build_object(
        'item_kind', 'raise', 'id', r.id, 'community_id', r.community_id,
        'organiser_id', r.organiser_id, 'title', r.title, 'description', r.description,
        'goal_minor', r.goal_minor, 'raised_minor', r.raised_minor, 'donor_count', r.donor_count,
        'open_until', r.open_until, 'visibility', r.visibility, 'closed_at', r.closed_at,
        'created_at', r.created_at, 'mine', r.organiser_id = v_uid,
        'badge', case when r.organiser_id = v_uid then 'Created by you' end) as j
      from public.raises r
      where r.community_id = p_community_id
        and not public.blocked_between(v_uid, r.organiser_id)
        and (v_q is null or r.title ilike v_pat or coalesce(r.description, '') ilike v_pat)
    ) x;
    return v_out;
  end if;

  select coalesce(jsonb_agg(public._event_json(e) order by e.starts_at, e.id), '[]'::jsonb) into v_out
  from public.events e
  where e.community_id = p_community_id
    and e.deleted_at is null
    and (e.creator_id = v_uid or not public.blocked_between(v_uid, e.creator_id))
    and case v_tab
          when 'upcoming' then e.ends_at > now()
          when 'event' then true
          when 'my_rsvps' then exists (select 1 from public.rsvps r where r.event_id = e.id and r.user_id = v_uid)
          when 'created' then e.creator_id = v_uid
          when 'paid' then e.is_paid
        end
    and (v_q is null
         or e.title ilike v_pat
         or coalesce(e.venue, '') ilike v_pat
         or exists (select 1 from jsonb_array_elements_text(e.tags) t where t ilike v_pat));
  return v_out;
end;
$$;

create or replace function public.create_event(
  p_community_id uuid,
  p_title text,
  p_description text default null,
  p_cover_url text default null,
  p_starts_at timestamptz default null,
  p_ends_at timestamptz default null,
  p_format text default null,
  p_venue text default null,
  p_join_url text default null,
  p_tags jsonb default null,
  p_speaker text default null,
  p_seat_cap integer default null,
  p_is_paid boolean default false,
  p_price_minor bigint default null,
  p_payment_url text default null)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_title text := nullif(btrim(coalesce(p_title, '')), '');
  v_format text := lower(coalesce(nullif(btrim(p_format), ''), 'in_person'));
  v_venue text := nullif(btrim(coalesce(p_venue, '')), '');
  v_join text := nullif(btrim(coalesce(p_join_url, '')), '');
  v_pay text := nullif(btrim(coalesce(p_payment_url, '')), '');
  v_paid boolean := coalesce(p_is_paid, false);
  v_ends timestamptz := coalesce(p_ends_at, p_starts_at + interval '2 hours');
  v_event public.events;
  v_post uuid;
begin
  if v_uid is null then
    raise exception 'Sign in to create an event' using errcode = 'PT401';
  end if;
  if not public.is_joined(p_community_id)
     or not exists (select 1 from public.communities c where c.id = p_community_id and c.deleted_at is null) then
    raise exception 'Members only' using errcode = 'PT403';
  end if;
  if v_title is null then
    raise exception 'Add an event title' using errcode = 'PT400';
  end if;
  if v_format not in ('in_person', 'online') then
    raise exception 'Format must be in_person or online' using errcode = 'PT400';
  end if;
  if p_starts_at is null
     or (v_format = 'in_person' and v_venue is null)
     or (v_format = 'online' and v_join is null) then
    raise exception 'Fill date, time and location' using errcode = 'PT400';
  end if;
  if v_format = 'online' and not public._is_http_url(v_join) then
    raise exception 'Enter an http(s) event link' using errcode = 'PT400';
  end if;
  if v_ends <= p_starts_at then
    raise exception 'End must be after start' using errcode = 'PT400';
  end if;
  if p_seat_cap is not null and p_seat_cap <= 0 then
    raise exception 'Seats and price must be valid positive numbers' using errcode = 'PT400';
  end if;
  if v_paid and (p_price_minor is null or p_price_minor <= 0) then
    raise exception 'Seats and price must be valid positive numbers' using errcode = 'PT400';
  end if;
  if v_paid and (v_pay is null or not public._is_http_url(v_pay)) then
    raise exception 'Add an http(s) payment link' using errcode = 'PT400';
  end if;
  if p_tags is not null and not public._is_string_array(p_tags) then
    raise exception 'Tags must be a list of strings' using errcode = 'PT400';
  end if;

  insert into public.events (community_id, creator_id, title, description, cover_url, starts_at, ends_at,
                             format, venue, join_url, tags, speaker, seat_cap, is_paid, price_minor, payment_url)
  values (p_community_id, v_uid, v_title, nullif(btrim(coalesce(p_description, '')), ''),
          nullif(btrim(coalesce(p_cover_url, '')), ''), p_starts_at, v_ends, v_format,
          case when v_format = 'in_person' then v_venue end,
          case when v_format = 'online' then v_join end,
          public._clean_string_array(p_tags, 'Tags'),
          nullif(btrim(coalesce(p_speaker, '')), ''), p_seat_cap, v_paid,
          case when v_paid then p_price_minor end, case when v_paid then v_pay end)
  returning * into v_event;

  -- The feed post (REQ-130): kind event, body = the title.
  insert into public.posts (author_id, kind, body, event_id)
  values (v_uid, 'event', v_title, v_event.id)
  returning id into v_post;
  insert into public.post_targets (post_id, community_id) values (v_post, p_community_id);

  return public._event_json(v_event);
end;
$$;

create or replace function public.get_event(p_event_id uuid)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_event public.events;
begin
  select * into v_event from public.events e where e.id = p_event_id;
  if not found or v_event.deleted_at is not null
     or not exists (select 1 from public.communities c where c.id = v_event.community_id and c.deleted_at is null) then
    raise exception 'Event not found' using errcode = 'PT404';
  end if;
  if v_uid is null or not public.is_joined(v_event.community_id) then
    raise exception 'Members only' using errcode = 'PT403';
  end if;
  if v_event.creator_id <> v_uid and public.blocked_between(v_uid, v_event.creator_id) then
    raise exception 'Event not found' using errcode = 'PT404';
  end if;
  return public._event_json(v_event) || jsonb_build_object(
    'host', public._profile_card(v_event.creator_id),
    'attendees', coalesce((
      select jsonb_agg(public._profile_card(r.user_id) || jsonb_build_object('seat_confirmed', r.seat_confirmed)
                       order by r.updated_at, r.user_id)
      from public.rsvps r
      where r.event_id = v_event.id and r.status = 'going'
        and (r.user_id = v_uid or not public.blocked_between(v_uid, r.user_id))), '[]'::jsonb));
end;
$$;

create or replace function public.event_landing(p_event_id uuid)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_event public.events;
  v_comm public.communities;
begin
  select * into v_event from public.events e where e.id = p_event_id;
  if not found or v_event.deleted_at is not null then
    raise exception 'Event not found' using errcode = 'PT404';
  end if;
  select * into v_comm from public.communities c where c.id = v_event.community_id;
  if not found or v_comm.deleted_at is not null then
    raise exception 'Event not found' using errcode = 'PT404';
  end if;
  if v_comm.visibility <> 'public' then
    raise exception 'This event is in a private community' using errcode = 'PT403';
  end if;
  return jsonb_build_object(
    'id', v_event.id, 'community_id', v_event.community_id, 'title', v_event.title,
    'cover_url', v_event.cover_url, 'starts_at', v_event.starts_at, 'ends_at', v_event.ends_at,
    'duration_minutes', (extract(epoch from v_event.ends_at - v_event.starts_at) / 60)::integer,
    'format', v_event.format,
    'where', case when v_event.format = 'online' then 'Online' else v_event.venue end,
    'tags', v_event.tags, 'is_paid', v_event.is_paid, 'price_minor', v_event.price_minor,
    'live_state', v_event.live_state,
    'going_count', (select count(*) from public.rsvps g where g.event_id = v_event.id and g.status = 'going'),
    'host', (select jsonb_build_object('id', p.id, 'full_name', p.full_name, 'avatar_url', p.avatar_url)
               from public.profiles p where p.id = v_event.creator_id),
    'community', jsonb_build_object('id', v_comm.id, 'name', v_comm.name, 'handle', v_comm.handle,
                                    'emoji', v_comm.emoji, 'cover_gradient', v_comm.cover_gradient,
                                    'logo_url', v_comm.logo_url, 'visibility', v_comm.visibility,
                                    'member_count', v_comm.member_count),
    'joined', auth.uid() is not null and public.is_joined(v_comm.id));
end;
$$;

-- Patch keys: title, description, cover_url, starts_at, ends_at, format,
-- venue, join_url, tags, speaker, seat_cap, is_paid, price_minor,
-- payment_url, live_state. Others are ignored. CHECKs decide validity (23514).
create or replace function public.update_event(p_event_id uuid, p_patch jsonb)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_old public.events;
  v_new public.events;
  v_patch jsonb;
begin
  select * into v_old from public.events e where e.id = p_event_id for no key update;
  if not found or not public.can_see_event(p_event_id) then
    raise exception 'Event not found' using errcode = 'PT404';
  end if;
  if v_old.creator_id is distinct from auth.uid() then
    raise exception 'Only the creator can edit this event' using errcode = 'PT403';
  end if;
  select coalesce(jsonb_object_agg(k, v), '{}'::jsonb) into v_patch
    from jsonb_each(coalesce(p_patch, '{}'::jsonb)) as x(k, v)
   where k in ('title', 'description', 'cover_url', 'starts_at', 'ends_at', 'format', 'venue', 'join_url',
               'tags', 'speaker', 'seat_cap', 'is_paid', 'price_minor', 'payment_url', 'live_state');
  if v_patch ? 'title' and nullif(btrim(coalesce(v_patch ->> 'title', '')), '') is null then
    raise exception 'Add an event title' using errcode = 'PT400';
  end if;
  v_new := jsonb_populate_record(v_old, v_patch);
  if not v_new.is_paid then
    v_new.price_minor := null;
    v_new.payment_url := null;
  end if;

  update public.events e set
    title = btrim(v_new.title), description = v_new.description, cover_url = v_new.cover_url,
    starts_at = v_new.starts_at, ends_at = v_new.ends_at, format = v_new.format,
    venue = case when v_new.format = 'in_person' then v_new.venue end,
    join_url = case when v_new.format = 'online' then v_new.join_url end,
    tags = public._clean_string_array(v_new.tags, 'Tags'), speaker = v_new.speaker,
    seat_cap = v_new.seat_cap, is_paid = v_new.is_paid, price_minor = v_new.price_minor,
    payment_url = v_new.payment_url, live_state = v_new.live_state
  where e.id = p_event_id
  returning * into v_new;
  return public._event_json(v_new);
end;
$$;

create or replace function public.delete_event(p_event_id uuid)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_event public.events;
  v_users uuid[];
  v_user uuid;
begin
  select * into v_event from public.events e where e.id = p_event_id for no key update;
  if not found or not public.can_see_event(p_event_id) then
    raise exception 'Event not found' using errcode = 'PT404';
  end if;
  if v_event.creator_id is distinct from auth.uid() then
    raise exception 'Only the creator can delete this event' using errcode = 'PT403';
  end if;

  update public.events e set deleted_at = now() where e.id = p_event_id;
  update public.posts po set deleted_at = now() where po.event_id = p_event_id and po.deleted_at is null;
  delete from public.reminders r where r.event_id = p_event_id and r.sent_at is null;

  select coalesce(array_agg(r.user_id order by r.user_id), '{}') into v_users
    from public.rsvps r
   where r.event_id = p_event_id and r.status = 'going' and r.user_id <> v_event.creator_id;
  foreach v_user in array v_users loop
    perform public.notify(v_user, 'event_deleted',
      jsonb_build_object('event_id', p_event_id, 'community_id', v_event.community_id));
  end loop;

  return jsonb_build_object('id', v_event.id, 'title', v_event.title,
                            'community_id', v_event.community_id, 'notified', to_jsonb(v_users));
end;
$$;

revoke all on function
  public._event_json(public.events),
  public.list_events(uuid, text, text),
  public.create_event(uuid, text, text, text, timestamptz, timestamptz, text, text, text, jsonb, text,
                      integer, boolean, bigint, text),
  public.get_event(uuid),
  public.event_landing(uuid),
  public.update_event(uuid, jsonb),
  public.delete_event(uuid)
  from public, anon, authenticated;

grant execute on function public._event_json(public.events) to service_role;
grant execute on function
  public.list_events(uuid, text, text),
  public.create_event(uuid, text, text, text, timestamptz, timestamptz, text, text, text, jsonb, text,
                      integer, boolean, bigint, text),
  public.get_event(uuid),
  public.update_event(uuid, jsonb),
  public.delete_event(uuid)
  to authenticated, service_role;
grant execute on function public.event_landing(uuid) to anon, authenticated, service_role;
