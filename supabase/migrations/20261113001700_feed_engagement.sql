-- Feed, engagement and reports: posts published to one or more communities
-- (post_targets), comments and replies, likes, poll votes, bookmarks,
-- per-viewer content filters (hide post / mute author) and abuse reports.
--
-- Access is keyed on the targets: a post is visible to joined members of any
-- live target community (can_see_post). Posts and targets are inserted only
-- by SECURITY DEFINER RPCs (create_post, create_event, create_raise); the
-- target trigger rejects a community the author has not joined (PT403).
-- Counters (like_count, comment_count) are kept by triggers.
--
-- Feed reads go through feed_page(): one is_joined() check, then a keyset
-- scan of post_targets_feed_idx (community, created_at, post_id), which is
-- why post_targets carries a copy of the post's created_at.

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

-- 2..10 non-blank strings.
create or replace function public._poll_options_ok(p_options jsonb)
returns boolean
language sql immutable set search_path = ''
as $$
  select p_options is not null and public._is_string_array(p_options)
    and jsonb_array_length(p_options) between 2 and 10
    and not exists (select 1 from jsonb_array_elements_text(p_options) o(v) where btrim(o.v) = '')
$$;

create table public.posts (
    id                 uuid primary key default gen_random_uuid(),
    author_id          uuid not null references public.profiles (id) on delete cascade,
    kind               text not null
        constraint posts_kind_check check (kind in
          ('text', 'image', 'video', 'poll', 'document', 'announcement', 'job', 'event', 'raise')),
    body               varchar(1200),
    media_url          text,
    media_kind         text
        constraint posts_media_kind_check check (media_kind in ('image', 'video', 'document')),
    is_urgent          boolean not null default false,
    is_announcement    boolean not null default false,
    job_tag            text not null default 'general'
        constraint posts_job_tag_check check (job_tag in ('general', 'available', 'need')),
    poll_options       jsonb,
    event_id           uuid references public.events (id) on delete cascade,
    raise_id           uuid references public.raises (id) on delete cascade,
    pinned_to_profile  boolean not null default false,
    like_count         integer not null default 0 constraint posts_like_count_check check (like_count >= 0),
    comment_count      integer not null default 0 constraint posts_comment_count_check check (comment_count >= 0),
    edited_at          timestamptz,
    deleted_at         timestamptz,
    created_at         timestamptz not null default now(),
    -- text 10–1200 (event/raise posts carry the item's title, any length)
    constraint posts_body_check check (
      body is null or kind in ('event', 'raise') or char_length(btrim(body)) >= 10),
    -- body may be null only when media (or the linked event/raise) is the content
    constraint posts_content_check check (
      body is not null or media_url is not null or kind in ('event', 'raise')),
    -- at most one media item: url and kind come together
    constraint posts_media_shape check ((media_url is null) = (media_kind is null)),
    constraint posts_media_matches_kind check (
      kind not in ('image', 'video', 'document') or media_kind = kind),
    constraint posts_poll_shape check (
      case when kind = 'poll'
        then public._poll_options_ok(poll_options)
        else poll_options is null end),
    constraint posts_event_shape check ((kind = 'event') = (event_id is not null)),
    constraint posts_raise_shape check ((kind = 'raise') = (raise_id is not null)),
    constraint posts_announcement_flag check (kind <> 'announcement' or is_announcement)
);

create index posts_author_idx on public.posts (author_id, created_at desc);
create index posts_event_idx on public.posts (event_id) where event_id is not null;
create index posts_raise_idx on public.posts (raise_id) where raise_id is not null;

create table public.post_targets (
    id            uuid primary key default gen_random_uuid(),
    post_id       uuid not null references public.posts (id) on delete cascade,
    community_id  uuid not null references public.communities (id) on delete cascade,
    -- copy of posts.created_at (set by trigger) so the feed is one index scan
    created_at    timestamptz not null default now(),
    constraint post_targets_post_community_key unique (post_id, community_id)
);

create index post_targets_feed_idx on public.post_targets (community_id, created_at desc, post_id desc);

create table public.comments (
    id          uuid primary key default gen_random_uuid(),
    post_id     uuid not null references public.posts (id) on delete cascade,
    author_id   uuid not null default auth.uid() references public.profiles (id) on delete cascade,
    parent_id   uuid references public.comments (id) on delete cascade,
    body        varchar(1000) not null constraint comments_body_check check (body ~ '\S'),
    like_count  integer not null default 0 constraint comments_like_count_check check (like_count >= 0),
    created_at  timestamptz not null default now(),
    constraint comments_not_own_parent check (parent_id is null or parent_id <> id)
);

create index comments_post_idx on public.comments (post_id, created_at);
create index comments_parent_idx on public.comments (parent_id) where parent_id is not null;

create table public.likes (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null default auth.uid() references public.profiles (id) on delete cascade,
    post_id     uuid references public.posts (id) on delete cascade,
    comment_id  uuid references public.comments (id) on delete cascade,
    created_at  timestamptz not null default now(),
    constraint likes_one_target check ((post_id is null) <> (comment_id is null)),
    constraint likes_user_post_key unique (user_id, post_id),
    constraint likes_user_comment_key unique (user_id, comment_id)
);

create index likes_post_idx on public.likes (post_id) where post_id is not null;
create index likes_comment_idx on public.likes (comment_id) where comment_id is not null;

create table public.poll_votes (
    id            uuid primary key default gen_random_uuid(),
    post_id       uuid not null references public.posts (id) on delete cascade,
    user_id       uuid not null default auth.uid() references public.profiles (id) on delete cascade,
    option_index  integer not null constraint poll_votes_option_check check (option_index >= 0),
    updated_at    timestamptz not null default now(),
    constraint poll_votes_post_user_key unique (post_id, user_id)
);

-- job_id gets its foreign key to jobs in the jobs migration.
create table public.bookmarks (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null default auth.uid() references public.profiles (id) on delete cascade,
    post_id     uuid references public.posts (id) on delete cascade,
    job_id      uuid,
    created_at  timestamptz not null default now(),
    constraint bookmarks_one_target check ((post_id is null) <> (job_id is null)),
    constraint bookmarks_user_post_key unique (user_id, post_id),
    constraint bookmarks_user_job_key unique (user_id, job_id)
);

create index bookmarks_user_idx on public.bookmarks (user_id, created_at desc);

create table public.content_filters (
    id             uuid primary key default gen_random_uuid(),
    user_id        uuid not null default auth.uid() references public.profiles (id) on delete cascade,
    kind           text not null constraint content_filters_kind_check check (kind in ('hide_post', 'mute_author')),
    post_id        uuid references public.posts (id) on delete cascade,
    muted_user_id  uuid references public.profiles (id) on delete cascade,
    created_at     timestamptz not null default now(),
    constraint content_filters_shape check (
      (kind = 'hide_post' and post_id is not null and muted_user_id is null)
      or (kind = 'mute_author' and muted_user_id is not null and post_id is null)),
    constraint content_filters_not_self check (muted_user_id is null or muted_user_id <> user_id),
    constraint content_filters_user_post_key unique (user_id, post_id),
    constraint content_filters_user_muted_key unique (user_id, muted_user_id)
);

create table public.reports (
    id           uuid primary key default gen_random_uuid(),
    reporter_id  uuid not null default auth.uid() references public.profiles (id) on delete cascade,
    target_kind  text not null
        constraint reports_target_kind_check check (target_kind in ('post', 'comment', 'thread', 'job', 'user')),
    target_id    uuid not null,
    reason       text not null
        constraint reports_reason_check check (reason in ('spam', 'inappropriate', 'fake_job', 'harassment', 'other')),
    details      varchar(1000),
    status       text not null default 'open'
        constraint reports_status_check check (status in ('open', 'actioned', 'dismissed')),
    created_at   timestamptz not null default now()
);

create index reports_status_idx on public.reports (status, created_at desc);
create index reports_target_idx on public.reports (target_kind, target_id);

-- ---------------------------------------------------------------------------
-- Access helpers
-- ---------------------------------------------------------------------------

-- A live post visible to the caller: joined to a live target community.
create or replace function public.can_see_post(p_post_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (
    select 1
    from public.posts p
    join public.post_targets t on t.post_id = p.id
    join public.communities c on c.id = t.community_id and c.deleted_at is null
    join public.memberships m on m.community_id = t.community_id
      and m.user_id = auth.uid() and m.status = 'joined'
    where p.id = p_post_id and p.deleted_at is null
  )
$$;

create or replace function public.is_post_author(p_post_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (select 1 from public.posts p where p.id = p_post_id and p.author_id = auth.uid())
$$;

create or replace function public._post_live(p_post_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (select 1 from public.posts p where p.id = p_post_id and p.deleted_at is null)
$$;

-- ---------------------------------------------------------------------------
-- Triggers
-- ---------------------------------------------------------------------------

-- Targets: the author must be joined to a live community; copy created_at.
create or replace function public.post_targets_before_insert()
returns trigger
language plpgsql security definer set search_path = ''
as $$
declare
  v_post public.posts;
begin
  select * into v_post from public.posts p where p.id = new.post_id;
  if not found or v_post.deleted_at is not null then
    raise exception 'Post not found' using errcode = 'PT404';
  end if;
  if not exists (
    select 1 from public.memberships m
    join public.communities c on c.id = m.community_id and c.deleted_at is null
    where m.community_id = new.community_id and m.user_id = v_post.author_id and m.status = 'joined'
  ) then
    raise exception 'You can only post to communities you have joined.' using errcode = 'PT403';
  end if;
  new.created_at := v_post.created_at;
  return new;
end
$$;

create trigger post_targets_before_insert before insert on public.post_targets
  for each row execute function public.post_targets_before_insert();

-- Posts: edits stamp edited_at; created_at is immutable (targets copy it).
create or replace function public.posts_before_update()
returns trigger
language plpgsql set search_path = ''
as $$
begin
  new.created_at := old.created_at;
  if new.body is distinct from old.body then
    new.edited_at := now();
  end if;
  return new;
end
$$;

create trigger posts_before_update before update on public.posts
  for each row execute function public.posts_before_update();

-- Comments: a reply's parent must be on the same post.
create or replace function public.comments_before_insert()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  if new.parent_id is not null and not exists (
    select 1 from public.comments c where c.id = new.parent_id and c.post_id = new.post_id
  ) then
    raise exception 'Reply to a comment on this post.' using errcode = 'PT400';
  end if;
  new.created_at := now();
  new.like_count := 0;
  return new;
end
$$;

create trigger comments_before_insert before insert on public.comments
  for each row execute function public.comments_before_insert();

create or replace function public.comments_count()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  if tg_op = 'INSERT' then
    update public.posts set comment_count = comment_count + 1 where id = new.post_id;
  else
    update public.posts set comment_count = greatest(comment_count - 1, 0) where id = old.post_id;
  end if;
  return null;
end
$$;

create trigger comments_count after insert or delete on public.comments
  for each row execute function public.comments_count();

create or replace function public.likes_count()
returns trigger
language plpgsql security definer set search_path = ''
as $$
declare
  v_delta integer := case when tg_op = 'INSERT' then 1 else -1 end;
  v_row public.likes := case when tg_op = 'INSERT' then new else old end;
begin
  if v_row.post_id is not null then
    update public.posts set like_count = greatest(like_count + v_delta, 0) where id = v_row.post_id;
  else
    update public.comments set like_count = greatest(like_count + v_delta, 0) where id = v_row.comment_id;
  end if;
  return null;
end
$$;

create trigger likes_count after insert or delete on public.likes
  for each row execute function public.likes_count();

-- Poll votes: the post must be a poll and the index one of its options.
create or replace function public.poll_votes_before_write()
returns trigger
language plpgsql security definer set search_path = ''
as $$
declare
  v_options jsonb;
begin
  select p.poll_options into v_options from public.posts p
  where p.id = new.post_id and p.kind = 'poll' and p.deleted_at is null;
  if v_options is null then
    raise exception 'This post is not a poll.' using errcode = 'PT400';
  end if;
  if new.option_index < 0 or new.option_index >= jsonb_array_length(v_options) then
    raise exception 'Choose one of the poll options.' using errcode = 'PT400';
  end if;
  if tg_op = 'UPDATE' then
    new.post_id := old.post_id;
    new.user_id := old.user_id;
  end if;
  new.updated_at := now();
  return new;
end
$$;

create trigger poll_votes_before_write before insert or update on public.poll_votes
  for each row execute function public.poll_votes_before_write();

-- ---------------------------------------------------------------------------
-- RLS and grants
-- ---------------------------------------------------------------------------

alter table public.posts enable row level security;
alter table public.post_targets enable row level security;
alter table public.comments enable row level security;
alter table public.likes enable row level security;
alter table public.poll_votes enable row level security;
alter table public.bookmarks enable row level security;
alter table public.content_filters enable row level security;
alter table public.reports enable row level security;

revoke all on public.posts, public.post_targets, public.comments, public.likes, public.poll_votes,
  public.bookmarks, public.content_filters, public.reports from anon, authenticated;

-- posts: members of a target read; the author edits text, tags and pin.
grant select on public.posts to anon, authenticated;
grant update (body, is_urgent, job_tag, pinned_to_profile) on public.posts to authenticated;
create policy posts_select_member on public.posts for select to authenticated
  using (public.can_see_post(id));
create policy posts_update_author on public.posts for update to authenticated
  using (author_id = auth.uid() and deleted_at is null)
  with check (author_id = auth.uid() and deleted_at is null);

-- post_targets: joined members of that community, and the author.
grant select on public.post_targets to anon, authenticated;
create policy post_targets_select on public.post_targets for select to authenticated
  using (public.is_joined(community_id) or public.is_post_author(post_id));

-- comments: anyone who can see the post reads and adds them.
grant select on public.comments to anon, authenticated;
grant insert (post_id, parent_id, body) on public.comments to authenticated;
create policy comments_select on public.comments for select to authenticated
  using (public.can_see_post(post_id));
create policy comments_insert on public.comments for insert to authenticated
  with check (author_id = auth.uid() and public.can_see_post(post_id));

-- likes: own rows; insert on visible targets; unlike = delete own.
grant select, delete on public.likes to anon, authenticated;
grant insert (post_id, comment_id) on public.likes to authenticated;
create policy likes_select_own on public.likes for select to authenticated
  using (user_id = auth.uid());
create policy likes_insert_own on public.likes for insert to authenticated
  with check (user_id = auth.uid() and public.can_see_post(coalesce(
    post_id, (select c.post_id from public.comments c where c.id = comment_id))));
create policy likes_delete_own on public.likes for delete to authenticated
  using (user_id = auth.uid());

-- poll_votes: own row, upserted (insert … on conflict (post_id, user_id) do update).
grant select on public.poll_votes to anon, authenticated;
grant insert (post_id, option_index), update (option_index) on public.poll_votes to authenticated;
create policy poll_votes_select_own on public.poll_votes for select to authenticated
  using (user_id = auth.uid());
create policy poll_votes_insert_own on public.poll_votes for insert to authenticated
  with check (user_id = auth.uid() and public.can_see_post(post_id));
create policy poll_votes_update_own on public.poll_votes for update to authenticated
  using (user_id = auth.uid()) with check (user_id = auth.uid() and public.can_see_post(post_id));

-- bookmarks: own rows; deleted posts drop out of Saved. Job bookmarks are
-- enabled by the jobs migration (it replaces bookmarks_insert_own).
grant select, delete on public.bookmarks to anon, authenticated;
grant insert (post_id, job_id) on public.bookmarks to authenticated;
create policy bookmarks_select_own on public.bookmarks for select to authenticated
  using (user_id = auth.uid() and (post_id is null or public._post_live(post_id)));
create policy bookmarks_insert_own on public.bookmarks for insert to authenticated
  with check (user_id = auth.uid() and post_id is not null and public.can_see_post(post_id));
create policy bookmarks_delete_own on public.bookmarks for delete to authenticated
  using (user_id = auth.uid());

-- content_filters: own rows; Undo = delete.
grant select, delete on public.content_filters to anon, authenticated;
grant insert (kind, post_id, muted_user_id) on public.content_filters to authenticated;
create policy content_filters_select_own on public.content_filters for select to authenticated
  using (user_id = auth.uid());
create policy content_filters_insert_own on public.content_filters for insert to authenticated
  with check (user_id = auth.uid() and (post_id is null or public.can_see_post(post_id)));
create policy content_filters_delete_own on public.content_filters for delete to authenticated
  using (user_id = auth.uid());

-- reports: anyone signed in files one; only operators read, and reporter_id
-- is never granted for select (not even to operators).
grant select (id, target_kind, target_id, reason, details, status, created_at) on public.reports to anon, authenticated;
grant insert (target_kind, target_id, reason, details) on public.reports to authenticated;
create policy reports_insert_own on public.reports for insert to authenticated
  with check (reporter_id = auth.uid() and status = 'open');
create policy reports_select_operator on public.reports for select to authenticated
  using (public.is_operator());

-- ---------------------------------------------------------------------------
-- RPCs
-- ---------------------------------------------------------------------------

-- One feed page for a community, newest (default) or oldest first, keyset
-- on (created_at, id), page size p_limit (1..50, default 20). p_newer_than
-- returns posts published after the newest one loaded (pull-to-refresh).
-- Excludes deleted posts, posts the caller hid, muted authors and blocks
-- either way. PT403 'Members only' unless the caller is joined.
create or replace function public.feed_page(
  p_community_id uuid,
  p_sort text default 'newest',
  p_cursor_at timestamptz default null,
  p_cursor_id uuid default null,
  p_newer_than timestamptz default null,
  p_since timestamptz default null,
  p_limit integer default 20
)
returns setof public.posts
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_limit integer := least(greatest(coalesce(p_limit, 20), 1), 50);
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

  if coalesce(p_sort, 'newest') = 'newest' then
    return query
      select p.* from public.post_targets t
      join public.posts p on p.id = t.post_id
      where t.community_id = p_community_id
        and p.deleted_at is null
        and (p_cursor_at is null or (t.created_at, t.post_id) < (p_cursor_at, coalesce(p_cursor_id, 'ffffffff-ffff-ffff-ffff-ffffffffffff'::uuid)))
        and (p_since is null or t.created_at >= p_since)
        and (p_newer_than is null or t.created_at > p_newer_than)
        and not exists (select 1 from public.blocks b
                        where (b.blocker_id = v_me and b.blocked_id = p.author_id)
                           or (b.blocker_id = p.author_id and b.blocked_id = v_me))
        and not exists (select 1 from public.content_filters f
                        where f.user_id = v_me
                          and (f.post_id = p.id or f.muted_user_id = p.author_id))
      order by t.created_at desc, t.post_id desc
      limit v_limit;
  else
    return query
      select p.* from public.post_targets t
      join public.posts p on p.id = t.post_id
      where t.community_id = p_community_id
        and p.deleted_at is null
        and (p_cursor_at is null or (t.created_at, t.post_id) > (p_cursor_at, coalesce(p_cursor_id, '00000000-0000-0000-0000-000000000000'::uuid)))
        and (p_since is null or t.created_at >= p_since)
        and (p_newer_than is null or t.created_at > p_newer_than)
        and not exists (select 1 from public.blocks b
                        where (b.blocker_id = v_me and b.blocked_id = p.author_id)
                           or (b.blocker_id = p.author_id and b.blocked_id = v_me))
        and not exists (select 1 from public.content_filters f
                        where f.user_id = v_me
                          and (f.post_id = p.id or f.muted_user_id = p.author_id))
      order by t.created_at asc, t.post_id asc
      limit v_limit;
  end if;
end
$$;

-- Vote counts per option for a visible poll: jsonb array of integers.
create or replace function public.poll_results(p_post_id uuid)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select case when public.can_see_post(p_post_id) then (
    select coalesce(jsonb_agg(
      (select count(*) from public.poll_votes v where v.post_id = p.id and v.option_index = o.i - 1)
      order by o.i), '[]'::jsonb)
    from public.posts p, jsonb_array_elements(p.poll_options) with ordinality o(v, i)
    where p.id = p_post_id and p.kind = 'poll'
  ) end
$$;

-- Files a report as the caller; the response never carries reporter_id.
-- PT400 'Choose a reason.' when the reason is missing or unknown.
create or replace function public.create_report(
  p_target_kind text, p_target_id uuid, p_reason text, p_details text default null
)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_row public.reports;
begin
  if auth.uid() is null then
    raise exception 'Sign in required.' using errcode = 'PT401';
  end if;
  if p_reason is null or p_reason not in ('spam', 'inappropriate', 'fake_job', 'harassment', 'other') then
    raise exception 'Choose a reason.' using errcode = 'PT400';
  end if;
  if p_target_kind is null or p_target_kind not in ('post', 'comment', 'thread', 'job', 'user') or p_target_id is null then
    raise exception 'Choose what to report.' using errcode = 'PT400';
  end if;
  insert into public.reports (reporter_id, target_kind, target_id, reason, details)
  values (auth.uid(), p_target_kind, p_target_id, p_reason, nullif(btrim(p_details), ''))
  returning * into v_row;
  return to_jsonb(v_row) - 'reporter_id';
end
$$;

-- Operator listing, newest first, optional status filter. PT403 otherwise.
create or replace function public.list_reports(p_status text default null)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
begin
  if not public.is_operator() then
    raise exception 'Operators only' using errcode = 'PT403';
  end if;
  if p_status is not null and p_status not in ('open', 'actioned', 'dismissed') then
    raise exception 'Unknown status.' using errcode = 'PT400';
  end if;
  return coalesce((
    select jsonb_agg(to_jsonb(r) - 'reporter_id' order by r.created_at desc, r.id desc)
    from public.reports r where p_status is null or r.status = p_status
  ), '[]'::jsonb);
end
$$;

-- ---------------------------------------------------------------------------
-- Realtime: live like/comment counts and new posts (RLS per subscriber).
-- ---------------------------------------------------------------------------

do $$
declare
  v_table text;
begin
  if not exists (select 1 from pg_publication where pubname = 'supabase_realtime') then
    create publication supabase_realtime;
  end if;
  foreach v_table in array array['posts', 'post_targets', 'comments'] loop
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
  public.can_see_post(uuid), public.is_post_author(uuid), public._post_live(uuid),
  public.post_targets_before_insert(), public.posts_before_update(),
  public.comments_before_insert(), public.comments_count(), public.likes_count(),
  public.poll_votes_before_write(),
  public.feed_page(uuid, text, timestamptz, uuid, timestamptz, timestamptz, integer),
  public.poll_results(uuid), public.create_report(text, uuid, text, text), public.list_reports(text)
  from public, anon, authenticated;

grant execute on function
  public.can_see_post(uuid), public.is_post_author(uuid), public._post_live(uuid),
  public.feed_page(uuid, text, timestamptz, uuid, timestamptz, timestamptz, integer),
  public.poll_results(uuid), public.create_report(text, uuid, text, text), public.list_reports(text)
  to authenticated, service_role;
