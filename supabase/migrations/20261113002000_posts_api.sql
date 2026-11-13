-- Feed and post CRUD API (listFeed, createPost, getPost, updatePost,
-- deletePost). All RPCs are SECURITY DEFINER, run as `authenticated`, and
-- decide access themselves; the handlers are thin.
--
--   list_feed(cid, chip, type, since, sort, cursor_at, cursor_id, newer_than, limit)
--     → jsonb array of _post_json (feed_page's keyset + chip/type filters)
--   create_post(community_ids[] | null = all joined, kind, body, media_url,
--     media_kind, is_urgent, is_announcement, job_tag, poll_options) → _post_json
--   get_post(id) → _post_json (PT404 'Post not found')
--   update_post(id, patch jsonb) → _post_json (author only)
--   delete_post(id) → soft delete (author, or moderator+ of a target)

-- Chip and post-type predicates of the Home filters.
--   chip: all | urgent (urgent or announcement) | announcements
--   type: all | jobs (job-tagged or job kind) | mentorship (mentions
--         mentor/mentorship) | announcements | events
create or replace function public._feed_filter(p public.posts, p_chip text, p_type text)
returns boolean
language sql immutable set search_path = ''
as $$
  select (case coalesce(p_chip, 'all')
            when 'urgent' then p.is_urgent or p.is_announcement
            when 'announcements' then p.is_announcement
            else true end)
     and (case coalesce(p_type, 'all')
            when 'jobs' then p.job_tag in ('available', 'need') or p.kind = 'job'
            when 'mentorship' then coalesce(p.body, '') ~* '\mmentor'
            when 'announcements' then p.is_announcement
            when 'events' then p.kind = 'event'
            else true end)
$$;

-- Post JSON for the caller: row + author card, target_count (live target
-- communities: 'Posted to all N communities'), liked, bookmarked, my_vote,
-- poll_results, mine, can_delete.
create or replace function public._post_json(p public.posts)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select to_jsonb(p) || jsonb_build_object(
    'author', public._profile_card(p.author_id),
    'target_count', (select count(*) from public.post_targets t
                     join public.communities c on c.id = t.community_id and c.deleted_at is null
                     where t.post_id = p.id),
    'liked', exists (select 1 from public.likes l where l.post_id = p.id and l.user_id = auth.uid()),
    'bookmarked', exists (select 1 from public.bookmarks b where b.post_id = p.id and b.user_id = auth.uid()),
    'my_vote', (select v.option_index from public.poll_votes v where v.post_id = p.id and v.user_id = auth.uid()),
    'poll_results', case when p.kind = 'poll' then (
        select coalesce(jsonb_agg(
          (select count(*) from public.poll_votes v where v.post_id = p.id and v.option_index = o.i - 1)
          order by o.i), '[]'::jsonb)
        from jsonb_array_elements(p.poll_options) with ordinality o(v, i)) end,
    'mine', p.author_id = auth.uid(),
    'can_delete', p.author_id = auth.uid() or exists (
        select 1 from public.post_targets t where t.post_id = p.id
          and public.has_community_role(t.community_id, array['owner', 'admin', 'moderator']))
  )
$$;

-- One feed page as jsonb. Same rules as feed_page (PT403 'Members only',
-- keyset on (created_at, post_id), hidden/muted/blocked excluded) plus the
-- chip and type filters. Page size 1..50, default 20.
create or replace function public.list_feed(
  p_community_id uuid,
  p_chip text default 'all',
  p_type text default 'all',
  p_since timestamptz default null,
  p_sort text default 'newest',
  p_cursor_at timestamptz default null,
  p_cursor_id uuid default null,
  p_newer_than timestamptz default null,
  p_limit integer default 20
)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_limit integer := least(greatest(coalesce(p_limit, 20), 1), 50);
  v_out jsonb;
begin
  if not exists (
    select 1 from public.memberships m
    join public.communities c on c.id = m.community_id and c.deleted_at is null
    where m.community_id = p_community_id and m.user_id = v_me and m.status = 'joined'
  ) then
    raise exception 'Members only' using errcode = 'PT403';
  end if;
  if coalesce(p_sort, 'newest') not in ('newest', 'oldest') then
    raise exception 'Unknown sort.' using errcode = 'PT400';
  end if;
  if coalesce(p_chip, 'all') not in ('all', 'urgent', 'announcements') then
    raise exception 'Unknown filter chip.' using errcode = 'PT400';
  end if;
  if coalesce(p_type, 'all') not in ('all', 'jobs', 'mentorship', 'announcements', 'events') then
    raise exception 'Unknown post type.' using errcode = 'PT400';
  end if;

  if coalesce(p_sort, 'newest') = 'newest' then
    select coalesce(jsonb_agg(public._post_json(x) order by x.created_at desc, x.id desc), '[]'::jsonb)
      into v_out
    from (
      select p.* from public.post_targets t
      join public.posts p on p.id = t.post_id
      where t.community_id = p_community_id
        and p.deleted_at is null
        and (p_cursor_at is null or (t.created_at, t.post_id) < (p_cursor_at, coalesce(p_cursor_id, 'ffffffff-ffff-ffff-ffff-ffffffffffff'::uuid)))
        and (p_since is null or t.created_at >= p_since)
        and (p_newer_than is null or t.created_at > p_newer_than)
        and public._feed_filter(p, p_chip, p_type)
        and not exists (select 1 from public.blocks b
                        where (b.blocker_id = v_me and b.blocked_id = p.author_id)
                           or (b.blocker_id = p.author_id and b.blocked_id = v_me))
        and not exists (select 1 from public.content_filters f
                        where f.user_id = v_me
                          and (f.post_id = p.id or f.muted_user_id = p.author_id))
      order by t.created_at desc, t.post_id desc
      limit v_limit
    ) x;
  else
    select coalesce(jsonb_agg(public._post_json(x) order by x.created_at, x.id), '[]'::jsonb)
      into v_out
    from (
      select p.* from public.post_targets t
      join public.posts p on p.id = t.post_id
      where t.community_id = p_community_id
        and p.deleted_at is null
        and (p_cursor_at is null or (t.created_at, t.post_id) > (p_cursor_at, coalesce(p_cursor_id, '00000000-0000-0000-0000-000000000000'::uuid)))
        and (p_since is null or t.created_at >= p_since)
        and (p_newer_than is null or t.created_at > p_newer_than)
        and public._feed_filter(p, p_chip, p_type)
        and not exists (select 1 from public.blocks b
                        where (b.blocker_id = v_me and b.blocked_id = p.author_id)
                           or (b.blocker_id = p.author_id and b.blocked_id = v_me))
        and not exists (select 1 from public.content_filters f
                        where f.user_id = v_me
                          and (f.post_id = p.id or f.muted_user_id = p.author_id))
      order by t.created_at asc, t.post_id asc
      limit v_limit
    ) x;
  end if;
  return v_out;
end
$$;

-- Publish one post to one or more joined communities (null = all joined,
-- live communities). PT400 no target / event|raise kind (their own RPCs) /
-- content; PT403 a target not joined, or announcement without moderator+
-- in every target. Body/poll/media CHECKs raise 23514 (→ 400).
create or replace function public.create_post(
  p_community_ids uuid[],
  p_kind text default null,
  p_body text default null,
  p_media_url text default null,
  p_media_kind text default null,
  p_is_urgent boolean default false,
  p_is_announcement boolean default false,
  p_job_tag text default 'general',
  p_poll_options jsonb default null
)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_targets uuid[];
  v_kind text;
  v_cid uuid;
  v_post public.posts;
begin
  if v_me is null then
    raise exception 'Sign in required.' using errcode = 'PT401';
  end if;
  if p_community_ids is null then
    select coalesce(array_agg(m.community_id order by m.created_at), '{}') into v_targets
    from public.memberships m
    join public.communities c on c.id = m.community_id and c.deleted_at is null
    where m.user_id = v_me and m.status = 'joined';
  else
    select coalesce(array_agg(distinct x), '{}') into v_targets
    from unnest(p_community_ids) x where x is not null;
  end if;
  if cardinality(v_targets) = 0 then
    raise exception 'Choose at least one community.' using errcode = 'PT400';
  end if;

  foreach v_cid in array v_targets loop
    if not exists (
      select 1 from public.memberships m
      join public.communities c on c.id = m.community_id and c.deleted_at is null
      where m.community_id = v_cid and m.user_id = v_me and m.status = 'joined'
    ) then
      raise exception 'You can only post to communities you have joined.' using errcode = 'PT403';
    end if;
    if coalesce(p_is_announcement, false) or p_kind = 'announcement' then
      if not public.has_community_role(v_cid, array['owner', 'admin', 'moderator']) then
        raise exception 'Only moderators and admins can post announcements.' using errcode = 'PT403';
      end if;
    end if;
  end loop;

  v_kind := coalesce(nullif(btrim(p_kind), ''),
    case when p_poll_options is not null then 'poll'
         when p_media_kind is not null then p_media_kind
         when coalesce(p_is_announcement, false) then 'announcement'
         else 'text' end);
  if v_kind in ('event', 'raise') then
    raise exception 'Events and raises are published from their own composers.' using errcode = 'PT400';
  end if;
  if v_kind = 'job' and coalesce(p_job_tag, 'general') = 'general' then
    raise exception 'Choose Available job or Need job.' using errcode = 'PT400';
  end if;

  insert into public.posts (author_id, kind, body, media_url, media_kind, is_urgent,
                            is_announcement, job_tag, poll_options)
  values (v_me, v_kind, nullif(btrim(p_body), ''), p_media_url, p_media_kind,
          coalesce(p_is_urgent, false),
          coalesce(p_is_announcement, false) or v_kind = 'announcement',
          coalesce(p_job_tag, 'general'), p_poll_options)
  returning * into v_post;

  insert into public.post_targets (post_id, community_id)
  select v_post.id, x from unnest(v_targets) x;

  return public._post_json(v_post);
end
$$;

-- One visible post. PT404 'Post not found' when unknown, deleted, not in a
-- joined live community, or the author is blocked either way.
create or replace function public.get_post(p_post_id uuid)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_post public.posts;
begin
  select * into v_post from public.posts p where p.id = p_post_id;
  if not found or not public.can_see_post(p_post_id)
     or public.blocked_between(auth.uid(), v_post.author_id) then
    raise exception 'Post not found' using errcode = 'PT404';
  end if;
  return public._post_json(v_post);
end
$$;

-- Author edit: keys body, is_urgent, job_tag, pinned_to_profile (others
-- ignored). PT404 not visible to the caller (and not theirs) or deleted;
-- PT403 not the author. job_tag 'general' drops it from Jobs (jobs_feed).
create or replace function public.update_post(p_post_id uuid, p_patch jsonb)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_post public.posts;
begin
  select * into v_post from public.posts p where p.id = p_post_id for update;
  if not found or v_post.deleted_at is not null
     or (v_post.author_id <> auth.uid() and not public.can_see_post(p_post_id)) then
    raise exception 'Post not found' using errcode = 'PT404';
  end if;
  if v_post.author_id <> auth.uid() then
    raise exception 'Only the author can edit this post.' using errcode = 'PT403';
  end if;
  if jsonb_typeof(coalesce(p_patch, '{}'::jsonb)) <> 'object' then
    raise exception 'Send a JSON object.' using errcode = 'PT400';
  end if;

  update public.posts p set
    body = case when p_patch ? 'body' then nullif(btrim(p_patch ->> 'body'), '') else p.body end,
    is_urgent = case when p_patch ? 'is_urgent' then coalesce((p_patch ->> 'is_urgent')::boolean, false) else p.is_urgent end,
    job_tag = case when p_patch ? 'job_tag' then coalesce(p_patch ->> 'job_tag', 'general') else p.job_tag end,
    pinned_to_profile = case when p_patch ? 'pinned_to_profile'
                          then coalesce((p_patch ->> 'pinned_to_profile')::boolean, false) else p.pinned_to_profile end
  where p.id = p_post_id
  returning * into v_post;
  return public._post_json(v_post);
end
$$;

-- Soft delete: the author, or an owner/admin/moderator of any target
-- community. PT404 unknown/deleted/not visible; PT403 otherwise. Deleted
-- posts leave the feed, Saved (bookmarks policy) and Jobs (jobs_feed).
create or replace function public.delete_post(p_post_id uuid)
returns void
language plpgsql security definer set search_path = ''
as $$
declare
  v_post public.posts;
begin
  select * into v_post from public.posts p where p.id = p_post_id for update;
  if not found or v_post.deleted_at is not null
     or (v_post.author_id <> auth.uid() and not public.can_see_post(p_post_id)) then
    raise exception 'Post not found' using errcode = 'PT404';
  end if;
  if v_post.author_id <> auth.uid() and not exists (
    select 1 from public.post_targets t where t.post_id = p_post_id
      and public.has_community_role(t.community_id, array['owner', 'admin', 'moderator'])
  ) then
    raise exception 'Only the author or a moderator can delete this post.' using errcode = 'PT403';
  end if;
  update public.posts set deleted_at = now() where id = p_post_id;
end
$$;

revoke all on function
  public._feed_filter(public.posts, text, text), public._post_json(public.posts),
  public.list_feed(uuid, text, text, timestamptz, text, timestamptz, uuid, timestamptz, integer),
  public.create_post(uuid[], text, text, text, text, boolean, boolean, text, jsonb),
  public.get_post(uuid), public.update_post(uuid, jsonb), public.delete_post(uuid)
  from public, anon, authenticated;

grant execute on function
  public.list_feed(uuid, text, text, timestamptz, text, timestamptz, uuid, timestamptz, integer),
  public.create_post(uuid[], text, text, text, text, boolean, boolean, text, jsonb),
  public.get_post(uuid), public.update_post(uuid, jsonb), public.delete_post(uuid)
  to authenticated, service_role;
grant execute on function public._feed_filter(public.posts, text, text), public._post_json(public.posts)
  to service_role;
